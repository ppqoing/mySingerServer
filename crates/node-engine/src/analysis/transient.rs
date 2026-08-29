//! 当前扫描分析的瞬态输入、候选和结果发布边界。

use std::{collections::BTreeMap, path::Path};

use dedup_core::{AnalysisRunId, LocationKey, MediaKind, TaskId, Thresholds};
use dedup_media::ImageStage1;
use dedup_node_store::{CompleteStage1, NodeStore, ResolvedScanFile, classify_cache_completeness};
use dedup_protocol::{BASE_MISSING_PROBE, BASE_MISSING_STAGE1};
use dedup_windows::atomic_replace_file_from_handle;

use super::phase2::Stage2BatchItem;
use super::result_reader::LatestAnalysisReader;
use super::video::video_candidates;
use super::{
    AnalysisBlocked, AnalysisCandidateStatus, AnalysisGroupKind, AnalysisResultError,
    AnalysisResultGroupKind, AnalysisResultHeader, AnalysisResultMode, AnalysisResultRow,
    AnalysisResultWriter, LocalAnalysisReport,
};
use super::{image::image_candidates, model::LocalAnalysisRun, model::ScanAnalysisInput};

/// 从 actor 提供的最近完成扫描快照创建进程内运行对象。
pub(crate) fn prepare_current_scan_analysis(
    store: &NodeStore,
    current_scan_task_id: TaskId,
    current_scan_revision: u64,
    resolved_files: &[ResolvedScanFile],
    selected_tasks: &[TaskId],
    thresholds: Thresholds,
    created_at_ms: u64,
) -> Result<LocalAnalysisRun, AnalysisBlocked> {
    if selected_tasks.len() != 1 || selected_tasks[0] != current_scan_task_id {
        return Err(AnalysisBlocked::CurrentScanTaskMismatch {
            expected: current_scan_task_id,
            selected: selected_tasks.to_vec(),
        });
    }
    let actual_revision = store.library_revision()?;
    if actual_revision != current_scan_revision {
        return Err(AnalysisBlocked::LibraryRevisionChanged {
            expected: current_scan_revision,
            actual: actual_revision,
        });
    }

    let mut inputs = resolved_files
        .iter()
        .map(|resolved| ScanAnalysisInput {
            content: resolved.content,
            location: LocationKey::new(
                store.machine_id().clone(),
                resolved.scanned.normalized_path.clone(),
            ),
            display_path: resolved.scanned.display_path.clone(),
            media_kind: MediaKind::Other,
        })
        .collect::<Vec<_>>();
    inputs.sort_by(|left, right| {
        left.content
            .cmp(&right.content)
            .then_with(|| left.location.cmp(&right.location))
    });
    inputs.dedup_by(|right, left| right.content == left.content && right.location == left.location);

    let mut keys = inputs.iter().map(|input| input.content).collect::<Vec<_>>();
    keys.sort();
    keys.dedup();
    let cached_records = store.lookup_base_cache_by_keys(&keys)?;
    if cached_records.len() != keys.len() {
        return Err(AnalysisBlocked::InvalidState(
            "当前扫描基础缓存批量返回数量不匹配".into(),
        ));
    }
    let mut images = BTreeMap::<dedup_core::ContentKey, ImageStage1>::new();
    let mut videos = BTreeMap::<dedup_core::ContentKey, Box<[Option<ImageStage1>; 6]>>::new();
    let mut media_kinds = BTreeMap::new();
    let mut skipped_incomplete = 0_usize;
    for (key, record) in keys.into_iter().zip(cached_records) {
        let Some(record) = record else {
            skipped_incomplete += 1;
            continue;
        };
        media_kinds.insert(key, record.media_kind);
        let completeness = classify_cache_completeness(&record, true);
        if completeness.base_missing_parts != 0 {
            skipped_incomplete += 1;
            continue;
        }
        match (record.media_kind, record.stage1) {
            (MediaKind::Image, Some(CompleteStage1::Image(feature))) => {
                images.insert(key, feature);
            }
            (MediaKind::Video, Some(CompleteStage1::Video(feature))) => {
                videos.insert(key, feature);
            }
            (MediaKind::Other, None) => {}
            _ => skipped_incomplete += 1,
        }
    }
    for input in &mut inputs {
        if let Some(media_kind) = media_kinds.get(&input.content) {
            input.media_kind = *media_kind;
        }
    }
    let mut candidates = image_candidates(&images, &thresholds);
    candidates.extend(video_candidates(&videos, &thresholds));

    Ok(LocalAnalysisRun {
        run_id: AnalysisRunId::new(),
        library_revision: current_scan_revision,
        created_at_ms,
        thresholds,
        inputs,
        candidates,
        skipped_incomplete,
    })
}

/// 从当前瞬态运行收集结构上缺失的唯一二筛内容和冻结来源。
pub(crate) fn missing_stage2_items(
    store: &NodeStore,
    run: &LocalAnalysisRun,
) -> Result<Vec<Stage2BatchItem>, AnalysisBlocked> {
    let mut requested = BTreeMap::<dedup_core::ContentKey, Option<super::AnalysisPairKind>>::new();
    for candidate in run
        .candidates
        .iter()
        .filter(|candidate| candidate.status == AnalysisCandidateStatus::Stage1Passed)
    {
        for content in [candidate.left, candidate.right] {
            requested
                .entry(content)
                .and_modify(|kind| {
                    if kind.is_some_and(|current| current != candidate.kind) {
                        *kind = None;
                    }
                })
                .or_insert(Some(candidate.kind));
        }
    }
    let keys = requested.keys().copied().collect::<Vec<_>>();
    let records = store.lookup_base_cache_by_keys(&keys)?;
    if records.len() != keys.len() {
        return Err(AnalysisBlocked::InvalidState(
            "瞬态二筛基础缓存批量返回数量不匹配".into(),
        ));
    }

    let mut items = Vec::new();
    for ((content, expected_kind), record) in requested.into_iter().zip(records) {
        let Some(expected_kind) = expected_kind else {
            continue;
        };
        let Some(record) = record else {
            continue;
        };
        let Some(source) = run
            .inputs
            .iter()
            .filter(|input| input.content == content)
            .min_by(|left, right| left.location.cmp(&right.location))
        else {
            continue;
        };
        let completeness = classify_cache_completeness(&record, true);
        if completeness.base_missing_parts & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1) != 0 {
            continue;
        }
        let frame_slots = match (expected_kind, record.media_kind) {
            (super::AnalysisPairKind::Image, MediaKind::Image) => {
                if completeness.image_stage2_missing {
                    Vec::new()
                } else {
                    continue;
                }
            }
            (super::AnalysisPairKind::Video, MediaKind::Video) => {
                let Some(CompleteStage1::Video(stage1)) = record.stage1.as_ref() else {
                    continue;
                };
                stage1
                    .iter()
                    .enumerate()
                    .filter_map(|(slot, feature)| {
                        feature.and_then(|_| {
                            (completeness.video_stage2_missing_slots & (1_u8 << slot) != 0)
                                .then_some(slot as u8)
                        })
                    })
                    .collect::<Vec<_>>()
            }
            _ => continue,
        };
        if expected_kind == super::AnalysisPairKind::Video && frame_slots.is_empty() {
            continue;
        }
        items.push(Stage2BatchItem {
            content,
            source: source.location.clone(),
            frame_slots,
        });
    }
    Ok(items)
}

/// 在库版本仍匹配且候选已经是终态时，把当前运行原子发布为最近一次结果 TSV。
pub(crate) fn publish_local_analysis_result(
    store: &NodeStore,
    run: LocalAnalysisRun,
    results_root: &Path,
) -> Result<(super::PublishedAnalysisResult, LocalAnalysisReport), AnalysisBlocked> {
    let (published, report, _) =
        publish_local_analysis_result_with_reader(store, run, results_root)?;
    Ok((published, report))
}

/// 在替换结果路径前完成验真，并把替换后的同一文件身份交给 actor。
pub(crate) fn publish_local_analysis_result_with_reader(
    store: &NodeStore,
    run: LocalAnalysisRun,
    results_root: &Path,
) -> Result<
    (
        super::PublishedAnalysisResult,
        LocalAnalysisReport,
        LatestAnalysisReader,
    ),
    AnalysisBlocked,
> {
    let actual_revision = store.library_revision()?;
    if actual_revision != run.library_revision {
        return Err(AnalysisBlocked::LibraryRevisionChanged {
            expected: run.library_revision,
            actual: actual_revision,
        });
    }
    let unresolved = run
        .candidates
        .iter()
        .filter(|candidate| {
            matches!(
                candidate.status,
                AnalysisCandidateStatus::Stage1Passed | AnalysisCandidateStatus::Incomplete
            )
        })
        .count();
    if unresolved != 0 {
        return Err(AnalysisBlocked::Stage2Incomplete { unresolved });
    }

    let groups = super::group_analysis_results(&run.inputs, &run.candidates);
    let header = AnalysisResultHeader {
        format_version: 1,
        analysis_id: run.run_id,
        library_revision: run.library_revision,
        analysis_mode: AnalysisResultMode::Local,
        created_at_ms: run.created_at_ms,
        thresholds: run.thresholds,
    };
    let mut writer = AnalysisResultWriter::begin(results_root, &header)?;
    for group in &groups {
        let group_kind = match group.kind {
            AnalysisGroupKind::Exact => AnalysisResultGroupKind::Exact,
            AnalysisGroupKind::Image => AnalysisResultGroupKind::Image,
            AnalysisGroupKind::Video => AnalysisResultGroupKind::Video,
        };
        for member in &group.members {
            writer.write_member(&AnalysisResultRow {
                group_kind,
                group_id: group.group_id.clone(),
                representative: member.representative,
                representative_content: group.representative,
                location: member.location.clone(),
                display_path: member.display_path.as_path().to_string_lossy().into_owned(),
                content: member.content,
                stage1_score: member.stage1_score,
                phash_passed_parts: member.phash_passed_parts,
                stage2_score: member.stage2_score,
            })?;
        }
    }
    let (published, reader) = writer.publish_with_verifier_and_replacer(
        LatestAnalysisReader::open_verified,
        |source, destination, reader| {
            atomic_replace_file_from_handle(reader.source_file(), source, destination)
                .map_err(AnalysisResultError::Io)
        },
    )?;
    let report = LocalAnalysisReport {
        run_id: run.run_id,
        status: dedup_node_store::AnalysisStatus::Completed,
        exact_groups: groups
            .iter()
            .filter(|group| group.kind == AnalysisGroupKind::Exact)
            .count(),
        image_groups: groups
            .iter()
            .filter(|group| group.kind == AnalysisGroupKind::Image)
            .count(),
        video_groups: groups
            .iter()
            .filter(|group| group.kind == AnalysisGroupKind::Video)
            .count(),
        skipped_incomplete: run.skipped_incomplete,
        phase2_dispatched: 0,
        unresolved_candidates: 0,
    };
    Ok((published, report, reader))
}

#[cfg(test)]
mod tests {
    use std::fs;

    use dedup_core::{
        ContentKey, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath, TaskId,
        Thresholds,
    };
    use dedup_media::{ImageStage1, ImageStage2, PdqHash};
    use dedup_node_store::{
        AnalysisStatus, FeatureWrite, GroupKind, ImageStage1Fields, NewTaskItem, NodeStore,
        ResolvedScanFile, ScannedPath, VideoFrameStage1Fields, VideoFrameStage2Fields,
        VideoMetadataFields,
    };
    use rusqlite::Connection;
    use tempfile::tempdir;

    use super::super::model::LocalAnalysisRun;
    use super::super::result_file::verify_result_file;
    use super::super::result_reader::LocalResultWindowKind;
    use super::super::{
        AnalysisCandidate, AnalysisCandidateStatus, AnalysisPairKind, ScanAnalysisInput,
        Stage2BatchItem, missing_stage2_items, prepare_current_scan_analysis,
        publish_local_analysis_result_with_reader,
    };
    use super::*;

    /// 第一个瞬态分析 RED：当前基线还没有拥有型运行对象和当前扫描入口。
    #[test]
    fn current_scan_analysis_returns_owned_run_without_persisted_analysis() {
        let machine = MachineId::parse(&"11".repeat(32)).unwrap();
        let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let content = ContentKey::new([7; 16], 12);
        let path = ScannedPath::new(
            NormalizedPath::new(r"D:\current\one.bin").unwrap(),
            DisplayPath::new(r"D:\current\one.bin").unwrap(),
            12,
        );
        let resolved = ResolvedScanFile {
            scanned: path.clone(),
            content: store
                .upsert_content_and_location(&path, content.md5(), dedup_core::MediaKind::Other)
                .unwrap()
                .key,
        };
        let content_id = store
            .content_id_by_key(content)
            .unwrap()
            .expect("测试内容必须存在");
        store.mark_base_complete(content_id).unwrap();
        let scan_task = TaskId::new();

        let run = prepare_current_scan_analysis(
            &store,
            scan_task,
            0,
            &[resolved],
            &[scan_task],
            Thresholds::default(),
            1,
        )
        .unwrap();

        let _: LocalAnalysisRun = run;
    }

    /// 当前扫描的两个活动位置共享一个内容键时，只发布 exact 组而不落分析表。
    #[test]
    fn current_scan_analysis_publishes_tsv_without_sqlite_analysis_rows() {
        let directory = tempdir().unwrap();
        let database = directory.path().join("node.db");
        let results_root = directory.path().join("results");
        let machine = MachineId::parse(&"12".repeat(32)).unwrap();
        let mut store = NodeStore::open(&database, machine.clone()).unwrap();
        let content = ContentKey::new([8; 16], 42);
        let left = scanned(r"D:\Current\A.BIN", 42);
        let right = scanned(r"D:\Current\B.BIN", 42);
        let left_record = store
            .upsert_content_and_location(&left, content.md5(), dedup_core::MediaKind::Other)
            .unwrap();
        store
            .upsert_content_and_location(&right, content.md5(), dedup_core::MediaKind::Other)
            .unwrap();
        store.mark_base_complete(left_record.id).unwrap();
        let resolved = vec![
            ResolvedScanFile {
                scanned: left,
                content,
            },
            ResolvedScanFile {
                scanned: right,
                content,
            },
        ];
        let scan_task = TaskId::new();
        let run = prepare_current_scan_analysis(
            &store,
            scan_task,
            0,
            &resolved,
            &[scan_task],
            Thresholds::default(),
            10,
        )
        .unwrap();
        let (published, report, mut reader) =
            publish_local_analysis_result_with_reader(&store, run, &results_root).unwrap();
        assert_eq!(published.member_count, 2);
        assert_eq!(published.group_count, 1);
        assert_eq!(report.status, AnalysisStatus::Completed);
        assert_eq!(report.exact_groups, 1);
        assert_eq!(report.image_groups, 0);
        assert_eq!(report.video_groups, 0);
        assert_eq!(verify_result_file(&published.path).unwrap().member_count, 2);
        assert!(!results_root.join("latest-analysis.partial.tsv").exists());
        assert!(!results_root.join("latest-analysis.result.tsv.idx").exists());
        let groups = reader
            .read_window(LocalResultWindowKind::Groups(GroupKind::Exact), 0, 1)
            .unwrap();
        let group_id = groups.groups[0].group_id.clone();
        fs::remove_file(&published.path).unwrap();
        let members = reader
            .read_window(LocalResultWindowKind::Members { group_id }, 0, 2)
            .unwrap();
        assert_eq!(members.members.len(), 2);
        drop(store);

        let connection = Connection::open(&database).unwrap();
        for table in [
            "analysis_runs",
            "analysis_run_stages",
            "analysis_run_inputs",
            "candidate_pairs",
            "duplicate_groups",
            "group_members",
            "review_marks",
            "tasks",
        ] {
            let count: i64 = connection
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                    row.get(0)
                })
                .unwrap();
            assert_eq!(count, 0, "瞬态分析不应写入 {table}");
        }
    }

    /// 当前扫描只能接受唯一、相等的任务 ID；版本变化同样在创建运行前拒绝。
    #[test]
    fn current_scan_analysis_requires_exact_task_and_revision() {
        let directory = tempdir().unwrap();
        let database = directory.path().join("gate.db");
        let results_root = directory.path().join("results");
        let machine = MachineId::parse(&"13".repeat(32)).unwrap();
        let store = NodeStore::open(&database, machine).unwrap();
        let current = TaskId::new();
        let old = TaskId::new();
        for selected in [Vec::new(), vec![current, TaskId::new()], vec![old]] {
            let error = prepare_current_scan_analysis(
                &store,
                current,
                0,
                &[],
                &selected,
                Thresholds::default(),
                10,
            )
            .unwrap_err();
            assert!(matches!(
                error,
                AnalysisBlocked::CurrentScanTaskMismatch { .. }
            ));
            assert!(!results_root.exists());
        }

        let error = prepare_current_scan_analysis(
            &store,
            current,
            1,
            &[],
            &[current],
            Thresholds::default(),
            10,
        )
        .unwrap_err();
        assert!(matches!(
            error,
            AnalysisBlocked::LibraryRevisionChanged { .. }
        ));
        assert!(!results_root.exists());
    }

    /// 旧任务表中的 queued/running 行不应阻挡 actor 已确认的当前扫描快照。
    #[test]
    fn current_scan_analysis_ignores_legacy_task_rows_for_latest_scan_gate() {
        let directory = tempdir().unwrap();
        let database = directory.path().join("legacy-tasks.db");
        let machine = MachineId::parse(&"14".repeat(32)).unwrap();
        let mut store = NodeStore::open(&database, machine).unwrap();
        let queued_task = store
            .create_task("scan", &[NewTaskItem::detached("legacy-queued")], 1)
            .unwrap();
        let running_task = store
            .create_task("scan", &[NewTaskItem::detached("legacy-running")], 2)
            .unwrap();
        store.claim_next_item(running_task, 3).unwrap().unwrap();

        let current_scan_task = TaskId::new();
        let run = prepare_current_scan_analysis(
            &store,
            current_scan_task,
            0,
            &[],
            &[current_scan_task],
            Thresholds::default(),
            4,
        )
        .expect("旧任务行不应阻挡当前扫描快照");
        assert_eq!(run.library_revision, 0);
        drop(store);

        let connection = Connection::open(&database).unwrap();
        for (task_id, expected_status) in [(queued_task, "queued"), (running_task, "running")] {
            let status: String = connection
                .query_row(
                    "SELECT status FROM tasks WHERE task_id=?1",
                    [task_id.as_uuid().to_string()],
                    |row| row.get(0),
                )
                .unwrap();
            assert_eq!(status, expected_status);
        }
        for table in [
            "analysis_runs",
            "analysis_run_stages",
            "analysis_run_inputs",
            "candidate_pairs",
            "duplicate_groups",
            "group_members",
            "review_marks",
        ] {
            let count: i64 = connection
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                    row.get(0)
                })
                .unwrap();
            assert_eq!(count, 0, "旧任务门禁不应写入 {table}");
        }
    }

    /// 二筛批次只包含结构缺口的唯一内容，并且完全不创建旧任务表行。
    #[test]
    fn missing_stage2_items_are_unique_and_only_structural_gaps() {
        let directory = tempdir().unwrap();
        let database = directory.path().join("missing-stage2.db");
        let machine = MachineId::parse(&"15".repeat(32)).unwrap();
        let mut store = NodeStore::open(&database, machine.clone()).unwrap();

        let missing_image = seed_b2_image(&mut store, r"D:\z\missing.jpg", [21; 16], 121, false);
        // 该活动位置故意不放进 run.inputs，验证来源只来自冻结输入而不是活动位置查询。
        seed_b2_image(&mut store, r"D:\0\not-in-run.jpg", [21; 16], 121, false);
        let missing_image_input = ScanAnalysisInput {
            content: missing_image.0,
            location: missing_image.1.clone(),
            display_path: DisplayPath::new(r"D:\z\frozen-spelling.jpg").unwrap(),
            media_kind: MediaKind::Image,
        };
        let missing_image_alt =
            seed_b2_image(&mut store, r"D:\a\also-in-run.jpg", [21; 16], 121, false);
        let missing_image_alt_input = ScanAnalysisInput {
            content: missing_image_alt.0,
            location: missing_image_alt.1.clone(),
            display_path: DisplayPath::new(r"D:\a\frozen-spelling.jpg").unwrap(),
            media_kind: MediaKind::Image,
        };
        let complete_image = seed_b2_image(&mut store, r"D:\complete.jpg", [22; 16], 122, true);
        let partial_video = seed_b2_video(
            &mut store,
            r"D:\partial.mp4",
            [23; 16],
            123,
            &[0, 2, 5],
            &[0, 1, 2, 3, 4, 5],
        );
        let failed_slot_video = seed_b2_video(
            &mut store,
            r"D:\failed-slot.mp4",
            [24; 16],
            124,
            &[0, 1, 4, 5],
            &[0, 1, 2, 4, 5],
        );
        let complete_video = seed_b2_video(
            &mut store,
            r"D:\complete.mp4",
            [25; 16],
            125,
            &[0, 1, 2, 3, 4, 5],
            &[0, 1, 2, 3, 4, 5],
        );
        let malformed_image = seed_b2_image(&mut store, r"D:\malformed.jpg", [26; 16], 126, false);
        let ignored_status =
            seed_b2_image(&mut store, r"D:\ignored-status.jpg", [27; 16], 127, false);
        let mismatch_video = seed_b2_video(
            &mut store,
            r"D:\mismatch.mp4",
            [28; 16],
            128,
            &[0, 1, 2, 3],
            &[0, 1, 2, 3],
        );
        let no_source = seed_b2_image(&mut store, r"D:\no-source.jpg", [29; 16], 129, false);
        // 通过真实 SQLite 行构造损坏的一筛结构，确认结构缺口不会伪造二筛项。
        let malformed_id = store
            .content_id_by_key(malformed_image.0)
            .unwrap()
            .expect("损坏图片内容必须存在");
        let connection = Connection::open(&database).unwrap();
        connection
            .execute_batch("PRAGMA ignore_check_constraints=ON;")
            .unwrap();
        connection
            .execute(
                "UPDATE image_stage1 SET width=0 WHERE content_id=?1",
                [malformed_id.as_i64()],
            )
            .unwrap();

        let mut inputs = vec![
            missing_image_alt_input,
            missing_image_input,
            input_for(&complete_image),
            input_for(&partial_video),
            input_for(&failed_slot_video),
            input_for(&complete_video),
            input_for(&malformed_image),
            input_for(&ignored_status),
            input_for(&mismatch_video),
        ];
        inputs.sort_by(|left, right| {
            left.content
                .cmp(&right.content)
                .then_with(|| left.location.cmp(&right.location))
        });

        let candidates = vec![
            candidate(
                AnalysisPairKind::Image,
                missing_image.0,
                complete_image.0,
                AnalysisCandidateStatus::Stage1Passed,
            ),
            // 同一个缺失图片重复出现在另一对候选中，结果仍只能有一项。
            candidate(
                AnalysisPairKind::Image,
                missing_image.0,
                malformed_image.0,
                AnalysisCandidateStatus::Stage1Passed,
            ),
            candidate(
                AnalysisPairKind::Video,
                partial_video.0,
                complete_video.0,
                AnalysisCandidateStatus::Stage1Passed,
            ),
            candidate(
                AnalysisPairKind::Video,
                failed_slot_video.0,
                complete_video.0,
                AnalysisCandidateStatus::Stage1Passed,
            ),
            // 媒体类型不匹配时不能为视频伪造图片二筛项。
            candidate(
                AnalysisPairKind::Image,
                mismatch_video.0,
                complete_image.0,
                AnalysisCandidateStatus::Stage1Passed,
            ),
            // 非 Stage1Passed 候选不应触发二筛批次。
            candidate(
                AnalysisPairKind::Image,
                ignored_status.0,
                complete_image.0,
                AnalysisCandidateStatus::Passed,
            ),
            // 缺少冻结来源时不能回退到 store 的活动位置。
            candidate(
                AnalysisPairKind::Image,
                no_source.0,
                complete_image.0,
                AnalysisCandidateStatus::Stage1Passed,
            ),
        ];
        let run = LocalAnalysisRun {
            run_id: dedup_core::AnalysisRunId::new(),
            library_revision: 0,
            created_at_ms: 1,
            thresholds: Thresholds::default(),
            inputs,
            candidates,
            skipped_incomplete: 0,
        };

        let items = missing_stage2_items(&store, &run).unwrap();

        assert_eq!(
            items,
            vec![
                Stage2BatchItem {
                    content: missing_image.0,
                    source: missing_image_alt.1,
                    frame_slots: Vec::new(),
                },
                Stage2BatchItem {
                    content: partial_video.0,
                    source: partial_video.1,
                    frame_slots: vec![1, 3, 4],
                },
                Stage2BatchItem {
                    content: failed_slot_video.0,
                    source: failed_slot_video.1,
                    frame_slots: vec![2],
                },
            ]
        );

        drop(store);
        let connection = Connection::open(&database).unwrap();
        for table in ["tasks", "task_items", "task_stages"] {
            let count: i64 = connection
                .query_row(&format!("SELECT COUNT(*) FROM {table}"), [], |row| {
                    row.get(0)
                })
                .unwrap();
            assert_eq!(count, 0, "缺失二筛准备不能写入 {table}");
        }
    }

    /// 构造一个基础完整度可控的图片缓存记录。
    fn seed_b2_image(
        store: &mut NodeStore,
        path: &str,
        md5: [u8; 16],
        size: u64,
        with_stage2: bool,
    ) -> (ContentKey, LocationKey) {
        let scanned = scanned(path, size);
        let content = store
            .upsert_content_and_location(&scanned, md5, MediaKind::Image)
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(b2_stage1())),
            )
            .unwrap();
        if with_stage2 {
            store
                .commit_feature_result(content.id, None, FeatureWrite::ImageStage2(b2_stage2()))
                .unwrap();
        }
        store.mark_base_complete(content.id).unwrap();
        (
            content.key,
            LocationKey::new(store.machine_id().clone(), scanned.normalized_path),
        )
    }

    /// 构造一个带指定一筛成功槽位和二筛槽位的视频缓存记录。
    fn seed_b2_video(
        store: &mut NodeStore,
        path: &str,
        md5: [u8; 16],
        size: u64,
        stage2_slots: &[u8],
        stage1_slots: &[u8],
    ) -> (ContentKey, LocationKey) {
        let scanned = scanned(path, size);
        let content = store
            .upsert_content_and_location(&scanned, md5, MediaKind::Video)
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoMetadata(VideoMetadataFields {
                    duration_ms: Some(12_000),
                    width: Some(100),
                    height: Some(100),
                }),
            )
            .unwrap();
        for slot in 0..6 {
            let feature = b2_stage1();
            let decoded = stage1_slots.contains(&slot);
            store
                .commit_feature_result(
                    content.id,
                    None,
                    FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                        slot,
                        time_ms: u64::from(slot) * 2_000 + 1_000,
                        decoded,
                        width: decoded.then_some(feature.width),
                        height: decoded.then_some(feature.height),
                        pdq: decoded.then_some(feature.pdq),
                        quality: decoded.then_some(feature.quality),
                    }),
                )
                .unwrap();
            if stage2_slots.contains(&slot) {
                store
                    .commit_feature_result(
                        content.id,
                        None,
                        FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                            slot,
                            features: b2_stage2(),
                        }),
                    )
                    .unwrap();
            }
        }
        store.mark_base_complete(content.id).unwrap();
        (
            content.key,
            LocationKey::new(store.machine_id().clone(), scanned.normalized_path),
        )
    }

    /// 把测试内容转成默认显示路径的冻结分析输入。
    fn input_for((content, location): &(ContentKey, LocationKey)) -> ScanAnalysisInput {
        ScanAnalysisInput {
            content: *content,
            location: location.clone(),
            display_path: DisplayPath::new(location.normalized_path().as_str()).unwrap(),
            media_kind: MediaKind::Other,
        }
    }

    /// 生成只关注二筛状态的内容候选。
    fn candidate(
        kind: AnalysisPairKind,
        left: ContentKey,
        right: ContentKey,
        status: AnalysisCandidateStatus,
    ) -> AnalysisCandidate {
        AnalysisCandidate {
            kind,
            left,
            right,
            stage1_score: 1.0,
            phash_passed_parts: None,
            stage2_score: None,
            status,
        }
    }

    /// 生成结构有效的一筛图片特征。
    fn b2_stage1() -> ImageStage1 {
        ImageStage1 {
            width: 100,
            height: 100,
            pdq: PdqHash::from_bytes([0; 32]),
            quality: 100,
        }
    }

    /// 生成合法全零二筛特征；零值不是失败占位。
    fn b2_stage2() -> ImageStage2 {
        ImageStage2 {
            phash_parts: [0; 9],
            sobel: [0.0; 128],
        }
    }

    /// 构造保留原始显示路径的扫描项。
    fn scanned(path: &str, file_size: u64) -> ScannedPath {
        ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            file_size,
        )
    }
}

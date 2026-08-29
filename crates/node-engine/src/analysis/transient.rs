//! 当前扫描分析的瞬态输入、候选和结果发布边界。

use std::{collections::BTreeMap, path::Path};

use dedup_core::{AnalysisRunId, LocationKey, MediaKind, TaskId, Thresholds};
use dedup_media::ImageStage1;
use dedup_node_store::{CompleteStage1, NodeStore, ResolvedScanFile, classify_cache_completeness};

use super::video::video_candidates;
use super::{
    AnalysisBlocked, AnalysisCandidateStatus, AnalysisGroupKind, AnalysisResultGroupKind,
    AnalysisResultHeader, AnalysisResultMode, AnalysisResultRow, AnalysisResultWriter,
    LocalAnalysisReport,
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

/// 在库版本仍匹配且候选已经是终态时，把当前运行原子发布为最近一次结果 TSV。
pub(crate) fn publish_local_analysis_result(
    store: &NodeStore,
    run: LocalAnalysisRun,
    results_root: &Path,
) -> Result<(super::PublishedAnalysisResult, LocalAnalysisReport), AnalysisBlocked> {
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
    let published = writer.publish()?;
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
    Ok((published, report))
}

#[cfg(test)]
mod tests {
    use dedup_core::{ContentKey, DisplayPath, MachineId, NormalizedPath, TaskId, Thresholds};
    use dedup_node_store::{AnalysisStatus, NewTaskItem, NodeStore, ResolvedScanFile, ScannedPath};
    use rusqlite::Connection;
    use tempfile::tempdir;

    use super::super::model::LocalAnalysisRun;
    use super::super::result_file::verify_result_file;
    use super::super::{prepare_current_scan_analysis, publish_local_analysis_result};
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
        let (published, report) =
            publish_local_analysis_result(&store, run, &results_root).unwrap();
        assert_eq!(published.member_count, 2);
        assert_eq!(published.group_count, 1);
        assert_eq!(report.status, AnalysisStatus::Completed);
        assert_eq!(report.exact_groups, 1);
        assert_eq!(report.image_groups, 0);
        assert_eq!(report.video_groups, 0);
        assert_eq!(verify_result_file(&published.path).unwrap().member_count, 2);
        assert!(!results_root.join("latest-analysis.partial.tsv").exists());
        assert!(!results_root.join("latest-analysis.result.tsv.idx").exists());
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

    /// 构造保留原始显示路径的扫描项。
    fn scanned(path: &str, file_size: u64) -> ScannedPath {
        ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            file_size,
        )
    }
}

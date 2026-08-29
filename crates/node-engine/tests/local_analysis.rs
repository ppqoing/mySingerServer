use dedup_core::{DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath, Thresholds};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_engine::{
    analysis::{
        AnalysisBlocked, LocalAnalysisEngine, Stage2BatchItem, Stage2Processor, Stage2Request,
        dispatch_stage2_batch,
    },
    worker::{Stage2Frame, Stage2Output},
};
use dedup_node_store::{
    AnalysisStatus, FeatureWrite, GroupKind, ImageStage1Fields, NewTaskItem, NodeStore,
    ReviewDecision, ScannedPath, TaskItemCompletion, TaskStatus, VideoFrameStage1Fields,
    VideoFrameStage2Fields, VideoMetadataFields,
};
use rusqlite::Connection;

#[derive(Default)]
struct CountingStage2 {
    calls: usize,
    fail_next: bool,
    requested_slots: Vec<Vec<u8>>,
}

impl Stage2Processor for CountingStage2 {
    async fn process(&mut self, request: Stage2Request) -> Result<Stage2Output, String> {
        self.calls += 1;
        self.requested_slots.push(request.frame_slots.clone());
        if self.fail_next {
            self.fail_next = false;
            return Err("fixture worker failure".into());
        }
        let slots = if request.media_kind == MediaKind::Video {
            request.frame_slots
        } else {
            vec![0]
        };
        Ok(Stage2Output {
            frames: slots
                .into_iter()
                .map(|slot| Stage2Frame {
                    slot,
                    feature: Some(stage2()),
                    error: None,
                })
                .collect(),
            regenerated_contact_sheet_jpeg: None,
        })
    }
}

#[tokio::test]
async fn active_computation_blocks_start_and_file_failures_do_not() {
    let mut store = store();
    let first = seed_other(&mut store, r"D:\exact-a.bin", [1; 16], 10);
    let second = seed_other(&mut store, r"D:\exact-b.bin", [1; 16], 10);
    let selected = completed_task_with_file_failure(&mut store, &[first, second], 1);
    let blocking = store
        .create_task("scan", &[NewTaskItem::detached("queued")], 2)
        .unwrap();
    let mut processor = CountingStage2::default();

    let error = LocalAnalysisEngine::start(
        &mut store,
        &[selected],
        Thresholds::default(),
        &mut processor,
        3,
    )
    .await
    .unwrap_err();
    assert!(matches!(error, AnalysisBlocked::ComputationRunning));

    let item = store.claim_next_item(blocking, 4).unwrap().unwrap();
    store
        .complete_item(
            &item.item_id,
            TaskItemCompletion::Succeeded { content_id: None },
            5,
        )
        .unwrap();
    assert_eq!(
        store.task_snapshot(selected).unwrap().status,
        TaskStatus::Completed
    );
    let report = LocalAnalysisEngine::start(
        &mut store,
        &[selected],
        Thresholds::default(),
        &mut processor,
        6,
    )
    .await
    .unwrap();

    assert_eq!(report.status, AnalysisStatus::Completed);
    assert_eq!(report.exact_groups, 1);
    let page = store.page_groups(report.run_id, None, 10).unwrap();
    assert_eq!(page.items.len(), 1);
    assert_eq!(page.items[0].kind, GroupKind::Exact);
    assert_eq!(
        store
            .page_group_members(report.run_id, &page.items[0].group_id, None, 10)
            .unwrap()
            .items
            .len(),
        2
    );
}

#[tokio::test]
async fn failed_or_cancelled_selected_task_requires_retry_or_reselection() {
    let mut store = store();
    let failed = store
        .create_task("scan", &[NewTaskItem::detached("fixture")], 7)
        .unwrap();
    store.fail_task(failed, 8).unwrap();
    let cancelled = store
        .create_task("scan", &[NewTaskItem::detached("fixture")], 9)
        .unwrap();
    store.cancel_task(cancelled, 10).unwrap();

    for (task_id, expected) in [
        (failed, TaskStatus::Failed),
        (cancelled, TaskStatus::Cancelled),
    ] {
        let error = LocalAnalysisEngine::start(
            &mut store,
            &[task_id],
            Thresholds::default(),
            &mut CountingStage2::default(),
            11,
        )
        .await
        .unwrap_err();
        assert!(matches!(
            error,
            AnalysisBlocked::SelectedTaskNeedsAttention { status, .. } if status == expected
        ));
    }
}

#[tokio::test]
async fn complete_cached_stage_two_creates_image_group_without_worker_calls() {
    let mut store = store();
    let left = seed_image(&mut store, r"D:\left.jpg", [2; 16], true);
    let right = seed_image(&mut store, r"D:\right.jpg", [3; 16], true);
    let task = completed_task_with_file_failure(&mut store, &[left, right], 10);
    let mut processor = CountingStage2::default();

    let report = LocalAnalysisEngine::start(
        &mut store,
        &[task],
        Thresholds::default(),
        &mut processor,
        11,
    )
    .await
    .unwrap();

    assert_eq!(processor.calls, 0);
    assert_eq!(report.status, AnalysisStatus::Completed);
    assert_eq!(report.image_groups, 1);
    assert_eq!(report.skipped_incomplete, 0);
    let page = store.page_groups(report.run_id, None, 10).unwrap();
    assert!(
        page.items
            .iter()
            .any(|group| group.kind == GroupKind::Image)
    );
}

#[tokio::test]
async fn partial_retry_dispatches_only_the_still_missing_content() {
    let mut store = store();
    let left = seed_image(&mut store, r"D:\cached.jpg", [4; 16], true);
    let right = seed_image(&mut store, r"D:\missing.jpg", [5; 16], false);
    let right_id = right.2;
    let task = completed_task_with_file_failure(&mut store, &[left, right], 20);
    let mut processor = CountingStage2 {
        fail_next: true,
        ..Default::default()
    };

    let partial = LocalAnalysisEngine::start(
        &mut store,
        &[task],
        Thresholds::default(),
        &mut processor,
        21,
    )
    .await
    .unwrap();
    assert_eq!(processor.calls, 1, "已有二筛的一端不得再次派发");
    assert_eq!(partial.status, AnalysisStatus::Partial);
    assert_eq!(partial.unresolved_candidates, 1);

    let completed =
        LocalAnalysisEngine::retry_phase2(&mut store, partial.run_id, &mut processor, 22)
            .await
            .unwrap();
    assert_eq!(processor.calls, 2, "重试只补同一个缺失 ContentKey");
    assert_eq!(completed.status, AnalysisStatus::Completed);
    assert!(store.load_complete_stage2(right_id).unwrap().is_some());
}

#[tokio::test]
async fn complete_six_slot_videos_use_average_and_cached_stage_two() {
    let mut store = store();
    let left = seed_video(&mut store, r"D:\left.mp4", [6; 16]);
    let right = seed_video(&mut store, r"D:\right.mp4", [7; 16]);
    let task = completed_task_with_file_failure(&mut store, &[left, right], 30);
    let mut processor = CountingStage2::default();

    let report = LocalAnalysisEngine::start(
        &mut store,
        &[task],
        Thresholds::default(),
        &mut processor,
        31,
    )
    .await
    .unwrap();

    assert_eq!(processor.calls, 0);
    assert_eq!(report.video_groups, 1);
    let page = store.page_groups(report.run_id, None, 10).unwrap();
    assert!(
        page.items
            .iter()
            .any(|group| group.kind == GroupKind::Video)
    );
}

#[tokio::test]
async fn video_stage2_dispatches_only_missing_successful_slots() {
    let mut store = store();
    let left =
        seed_video_with_stage2_slots(&mut store, r"D:\partial-left.mp4", [11; 16], &[0, 1, 4, 5]);
    let right =
        seed_video_with_stage2_slots(&mut store, r"D:\partial-right.mp4", [12; 16], &[0, 1, 4, 5]);
    let task = completed_task_with_file_failure(&mut store, &[left, right], 35);
    let mut processor = CountingStage2::default();

    let report = LocalAnalysisEngine::start(
        &mut store,
        &[task],
        Thresholds::default(),
        &mut processor,
        36,
    )
    .await
    .unwrap();

    assert_eq!(processor.calls, 2);
    assert_eq!(processor.requested_slots, vec![vec![2, 3], vec![2, 3]]);
    assert_eq!(report.status, AnalysisStatus::Completed);
    assert_eq!(report.video_groups, 1);
}

#[tokio::test]
async fn explicit_stage2_batch_requests_only_missing_successful_video_slots() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("stage2-batch.db");
    let machine = MachineId::parse(&"77".repeat(32)).unwrap();
    let mut store = NodeStore::open(&database, machine).unwrap();
    let seeded = seed_video_with_stage2_slots(
        &mut store,
        r"D:\central-partial.mp4",
        [13; 16],
        &[0, 1, 4, 5],
    );
    let cached = store.load_base_cache_record(seeded.2).unwrap();
    let mut processor = CountingStage2::default();

    dispatch_stage2_batch(
        &mut store,
        &[Stage2BatchItem {
            content: cached.content_key,
            source: seeded.1,
            frame_slots: (0..6).collect(),
        }],
        &mut processor,
        37,
    )
    .await
    .unwrap();

    assert_eq!(processor.calls, 1);
    assert_eq!(processor.requested_slots, vec![vec![2, 3]]);
}

#[tokio::test]
async fn explicit_stage2_batch_republishes_requested_cached_slot_without_worker() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("stage2-existing-slot.db");
    let machine = MachineId::parse(&"79".repeat(32)).unwrap();
    let mut store = NodeStore::open(&database, machine).unwrap();
    let seeded = seed_video_with_stage2_slots(
        &mut store,
        r"D:\central-existing.mp4",
        [16; 16],
        &[0, 1, 4, 5],
    );
    let content_key = store.load_base_cache_record(seeded.2).unwrap().content_key;
    let before = store.outbox_high_seq().unwrap();
    let mut processor = CountingStage2::default();

    let _runtime_id = dispatch_stage2_batch(
        &mut store,
        &[Stage2BatchItem {
            content: content_key,
            source: seeded.1,
            frame_slots: vec![0],
        }],
        &mut processor,
        38,
    )
    .await
    .unwrap();

    assert_eq!(processor.calls, 0);
    assert_eq!(
        store
            .pull_changes(before, 100)
            .unwrap()
            .changes
            .into_iter()
            .filter(|change| change.entity_kind == "video_frame_stage2")
            .count(),
        1
    );
    assert!(
        store.page_tasks(None, 20).unwrap().items.is_empty(),
        "瞬态二筛批次不应创建旧任务表行"
    );
}

#[tokio::test]
async fn incomplete_media_is_counted_and_never_enters_candidates() {
    let mut store = store();
    let incomplete = seed_content(
        &mut store,
        r"D:\incomplete.jpg",
        [8; 16],
        100,
        MediaKind::Image,
    );
    let task = completed_task_with_file_failure(&mut store, &[incomplete], 40);
    let mut processor = CountingStage2::default();

    let report = LocalAnalysisEngine::start(
        &mut store,
        &[task],
        Thresholds::default(),
        &mut processor,
        41,
    )
    .await
    .unwrap();

    assert_eq!(report.skipped_incomplete, 1);
    assert_eq!(report.phase2_dispatched, 0);
    assert!(store.analysis_candidates(report.run_id).unwrap().is_empty());
}

#[tokio::test]
async fn completed_analysis_groups_members_and_review_survive_reopen() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("local-analysis.db");
    let machine = MachineId::parse(&"66".repeat(32)).unwrap();
    let report;
    let group_id;
    let reviewed;
    let mut store = NodeStore::open(&database, machine).unwrap();
    let left = seed_image(&mut store, r"D:\persist-left.jpg", [9; 16], true);
    let right = seed_image(&mut store, r"D:\persist-right.jpg", [10; 16], true);
    let task = completed_task_with_file_failure(&mut store, &[left, right], 50);
    report = LocalAnalysisEngine::start(
        &mut store,
        &[task],
        Thresholds::default(),
        &mut CountingStage2::default(),
        51,
    )
    .await
    .unwrap();
    let group = store.page_groups(report.run_id, None, 1).unwrap().items[0].clone();
    group_id = group.group_id;
    reviewed = store
        .page_group_members(report.run_id, &group_id, None, 1)
        .unwrap()
        .items[0]
        .location
        .clone();
    store
        .save_review_mark(report.run_id, &group_id, &reviewed, ReviewDecision::Keep)
        .unwrap();

    let store = store.reopen().unwrap();
    assert_eq!(
        store.analysis_run_snapshot(report.run_id).unwrap().status,
        AnalysisStatus::Completed
    );
    assert_eq!(
        store
            .page_groups(report.run_id, None, 10)
            .unwrap()
            .items
            .len(),
        1
    );
    assert_eq!(
        store
            .page_group_members(report.run_id, &group_id, None, 10)
            .unwrap()
            .items
            .len(),
        2
    );
    assert_eq!(
        store
            .review_mark(report.run_id, &group_id, &reviewed)
            .unwrap(),
        Some(ReviewDecision::Keep)
    );
}

#[tokio::test]
async fn malformed_stage1_is_skipped_without_blocking_valid_content() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("malformed-analysis.db");
    let machine = MachineId::parse(&"78".repeat(32)).unwrap();
    let mut store = NodeStore::open(&database, machine).unwrap();
    let bad = seed_image(&mut store, r"D:\malformed-analysis.jpg", [14; 16], true);
    let good = seed_image(&mut store, r"D:\valid-analysis.jpg", [15; 16], true);
    let bad_id = bad.2.as_i64();
    let task = completed_task_with_file_failure(&mut store, &[bad, good], 60);

    let connection = Connection::open(&database).unwrap();
    connection
        .execute_batch("PRAGMA ignore_check_constraints=ON;")
        .unwrap();
    connection
        .execute(
            "UPDATE image_stage1 SET width=0 WHERE content_id=?1",
            [bad_id],
        )
        .unwrap();
    drop(connection);

    let mut store = store.reopen().unwrap();
    let mut processor = CountingStage2::default();
    let report = LocalAnalysisEngine::start(
        &mut store,
        &[task],
        Thresholds::default(),
        &mut processor,
        61,
    )
    .await
    .unwrap();

    assert_eq!(report.skipped_incomplete, 1);
    assert_eq!(report.image_groups, 0);
    assert_eq!(processor.calls, 0);
}

type Seeded = (ScannedPath, LocationKey, dedup_node_store::ContentId);

fn store() -> NodeStore {
    NodeStore::open_in_memory(MachineId::parse(&"55".repeat(32)).unwrap()).unwrap()
}

fn seed_other(store: &mut NodeStore, path: &str, md5: [u8; 16], size: u64) -> Seeded {
    let seeded = seed_content(store, path, md5, size, MediaKind::Other);
    store.mark_base_complete(seeded.2).unwrap();
    seeded
}

fn seed_image(store: &mut NodeStore, path: &str, md5: [u8; 16], with_stage2: bool) -> Seeded {
    let seeded = seed_content(store, path, md5, 100, MediaKind::Image);
    store
        .commit_feature_result(
            seeded.2,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(stage1())),
        )
        .unwrap();
    if with_stage2 {
        store
            .commit_feature_result(seeded.2, None, FeatureWrite::ImageStage2(stage2()))
            .unwrap();
    }
    store.mark_base_complete(seeded.2).unwrap();
    seeded
}

fn seed_video(store: &mut NodeStore, path: &str, md5: [u8; 16]) -> Seeded {
    seed_video_with_stage2_slots(store, path, md5, &[0, 1, 2, 3, 4, 5])
}

fn seed_video_with_stage2_slots(
    store: &mut NodeStore,
    path: &str,
    md5: [u8; 16],
    stage2_slots: &[u8],
) -> Seeded {
    let seeded = seed_content(store, path, md5, 200, MediaKind::Video);
    store
        .commit_feature_result(
            seeded.2,
            None,
            FeatureWrite::VideoMetadata(VideoMetadataFields {
                duration_ms: Some(12_000),
                width: Some(100),
                height: Some(100),
            }),
        )
        .unwrap();
    for slot in 0..6 {
        let feature = stage1();
        store
            .commit_feature_result(
                seeded.2,
                None,
                FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                    slot,
                    time_ms: u64::from(slot) * 2_000 + 1_000,
                    decoded: true,
                    width: Some(feature.width),
                    height: Some(feature.height),
                    pdq: Some(feature.pdq),
                    quality: Some(feature.quality),
                }),
            )
            .unwrap();
        if stage2_slots.contains(&slot) {
            store
                .commit_feature_result(
                    seeded.2,
                    None,
                    FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                        slot,
                        features: stage2(),
                    }),
                )
                .unwrap();
        }
    }
    store.mark_base_complete(seeded.2).unwrap();
    seeded
}

fn seed_content(
    store: &mut NodeStore,
    path: &str,
    md5: [u8; 16],
    size: u64,
    kind: MediaKind,
) -> Seeded {
    let scanned = ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        size,
    );
    let content = store
        .upsert_content_and_location(&scanned, md5, kind)
        .unwrap();
    let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path.clone());
    (scanned, location, content.id)
}

fn completed_task_with_file_failure(
    store: &mut NodeStore,
    contents: &[Seeded],
    now: i64,
) -> dedup_core::TaskId {
    let mut items = contents
        .iter()
        .map(|(scanned, location, content_id)| {
            NewTaskItem::for_content(
                location.clone(),
                scanned.display_path.clone(),
                scanned.file_size,
                *content_id,
                "stage1",
            )
        })
        .collect::<Vec<_>>();
    items.push(NewTaskItem::detached("fixture_failed_file"));
    let task = store.create_task("scan", &items, now).unwrap();
    while let Some(item) = store.claim_next_item(task, now).unwrap() {
        let completion = if item.stage == "fixture_failed_file" {
            TaskItemCompletion::Failed("fixture".into())
        } else {
            TaskItemCompletion::Succeeded {
                content_id: item.content_id,
            }
        };
        store.complete_item(&item.item_id, completion, now).unwrap();
    }
    task
}

fn stage1() -> ImageStage1 {
    ImageStage1 {
        width: 100,
        height: 100,
        pdq: PdqHash::from_bytes([0; 32]),
        quality: 100,
    }
}

fn stage2() -> ImageStage2 {
    ImageStage2 {
        phash_parts: [0; 9],
        sobel: [0.0; 128],
    }
}

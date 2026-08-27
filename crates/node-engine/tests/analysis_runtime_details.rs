use dedup_core::{
    ContentKey, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath, TaskId, Thresholds,
};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_engine::{
    analysis::{LocalAnalysisEngine, Stage2Processor, Stage2Request, WorkerPoolStage2Processor},
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry},
    worker::{Stage2Frame, Stage2Output, WorkerPool},
};
use dedup_node_store::{
    AnalysisStatus, FeatureWrite, ImageStage1Fields, NewTaskItem, NodeStore, ScannedPath,
    TaskItemCompletion,
};
use dedup_protocol::proto::RuntimeStageState;

#[derive(Default)]
struct FailingStage2;

impl Stage2Processor for FailingStage2 {
    async fn process(&mut self, _request: Stage2Request) -> Result<Stage2Output, String> {
        Err("controlled stage2 failure".into())
    }
}

struct CompleteStage2;

impl Stage2Processor for CompleteStage2 {
    async fn process(&mut self, _request: Stage2Request) -> Result<Stage2Output, String> {
        Ok(Stage2Output {
            frames: vec![Stage2Frame {
                slot: 0,
                feature: Some(stage2()),
                error: None,
            }],
            regenerated_contact_sheet_jpeg: None,
        })
    }
}

#[tokio::test]
async fn local_analysis_reports_three_persistent_duplicate_list_stages() {
    let machine = MachineId::from_sha256([0x81; 32]);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::LocalAnalysis, machine.clone(), "本地分析")
        .await;
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let left = seed_image(&mut store, r"D:\runtime-left.jpg", [1; 16], true);
    let right = seed_image(&mut store, r"D:\runtime-right.jpg", [2; 16], true);
    let task = completed_task(&mut store, &[left, right]);
    let run_id = LocalAnalysisEngine::begin(&mut store, &[task], Thresholds::default(), 2).unwrap();

    let report = LocalAnalysisEngine::run_existing_with_runtime(
        &mut store,
        run_id,
        &mut CompleteStage2,
        &reporter,
        3,
    )
    .await
    .unwrap();

    assert_eq!(report.status, AnalysisStatus::Completed);
    let details = registry.details(reporter.id()).await.unwrap();
    let summary = details.summary.as_ref().unwrap();
    assert!(summary.overall_total_known);
    assert_eq!(summary.overall_total, 2);
    assert_eq!(summary.overall_completed, 2);
    assert_eq!(summary.task_kind, "duplicate_list");
    let expected = [
        ("build_candidates", "candidate_pairs"),
        ("dispatch_stage2", "files"),
        ("final_compare", "candidate_pairs"),
    ];
    assert_eq!(details.stages.len(), expected.len());
    for (id, unit) in expected {
        let stage = details
            .stages
            .iter()
            .find(|stage| stage.stage_id == id)
            .unwrap();
        assert_eq!(stage.unit, unit);
        assert_eq!(stage.state, RuntimeStageState::RuntimeStageCompleted as i32);
    }
    let persisted = store.analysis_stages(run_id).unwrap();
    assert_eq!(
        persisted
            .iter()
            .map(|stage| stage.stage_id.as_str())
            .collect::<Vec<_>>(),
        vec!["build_candidates", "dispatch_stage2", "final_compare"]
    );
    assert!(persisted.iter().all(|stage| stage.started_at_ms.is_some()));
    assert!(persisted.iter().all(|stage| stage.finished_at_ms.is_some()));
}

#[tokio::test]
async fn unresolved_stage2_marks_final_compare_failed() {
    let machine = MachineId::from_sha256([0x82; 32]);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::LocalAnalysis, machine.clone(), "本地分析")
        .await;
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    let left = seed_image(&mut store, r"D:\partial-left.jpg", [3; 16], true);
    let right = seed_image(&mut store, r"D:\partial-right.jpg", [4; 16], false);
    let task = completed_task(&mut store, &[left, right]);
    let run_id = LocalAnalysisEngine::begin(&mut store, &[task], Thresholds::default(), 4).unwrap();

    let report = LocalAnalysisEngine::run_existing_with_runtime(
        &mut store,
        run_id,
        &mut FailingStage2,
        &reporter,
        5,
    )
    .await
    .unwrap();

    assert_eq!(report.status, AnalysisStatus::Partial);
    let details = registry.details(reporter.id()).await.unwrap();
    assert!(details.summary.as_ref().unwrap().overall_failed > 0);
    let dispatch = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "dispatch_stage2")
        .unwrap();
    assert_eq!(
        dispatch.state,
        RuntimeStageState::RuntimeStageCompleted as i32
    );
    let compare = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "final_compare")
        .unwrap();
    assert_eq!(compare.state, RuntimeStageState::RuntimeStageFailed as i32);
    assert_eq!(compare.total, 1);
    assert_eq!(compare.completed, 0);
    assert_eq!(compare.failed, 1);
    let persisted = store.analysis_stages(run_id).unwrap();
    assert_eq!(persisted[1].state.as_str(), "completed");
    assert_eq!(persisted[2].state.as_str(), "failed");
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn local_analysis_real_pool_crash_is_persisted_before_next_dispatch_and_slot_recovers() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("analysis-crash.db");
    let machine = MachineId::from_sha256([0x84; 32]);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(RuntimeTaskKind::LocalAnalysis, machine.clone(), "本地分析")
        .await;
    let reporter_id = reporter.id().to_owned();
    let mut store = NodeStore::open(&database, machine.clone()).unwrap();
    let a = seed_image(&mut store, r"D:\crash-a.jpg", [8; 16], false);
    let b = seed_image(&mut store, r"D:\crash-b.jpg", [9; 16], false);
    let c = seed_image(&mut store, r"D:\crash-c.jpg", [10; 16], false);
    let task = completed_task(&mut store, &[a, b, c]);
    let run_id = LocalAnalysisEngine::begin(&mut store, &[task], Thresholds::default(), 6).unwrap();
    let (mut pool, mut started, control) = WorkerPool::controlled_batch_for_test(2);
    let controller_registry = registry.clone();
    let controller_database = database.clone();
    let controller_reporter_id = reporter_id.clone();
    let controller = tokio::spawn(async move {
        let (task_id, first_item) = started.recv().await.unwrap();
        let (second_task, second_item) = started.recv().await.unwrap();
        assert_eq!(second_task, task_id);
        assert_ne!(
            second_item, first_item,
            "两个 Worker 应在任何结果返回前同时占满"
        );
        control
            .crash(
                task_id.clone(),
                first_item,
                "controlled stage2 crash".into(),
            )
            .await;
        control
            .complete_stage2(
                task_id.clone(),
                second_item,
                Stage2Output {
                    frames: vec![Stage2Frame {
                        slot: 0,
                        feature: Some(stage2()),
                        error: None,
                    }],
                    regenerated_contact_sheet_jpeg: None,
                },
            )
            .await;

        let (_, third_item) = started.recv().await.unwrap();
        let reopened =
            NodeStore::open(&controller_database, MachineId::from_sha256([0x84; 32])).unwrap();
        let stage2_task = reopened
            .page_tasks(None, 20)
            .unwrap()
            .items
            .into_iter()
            .find(|task| task.task_id.as_uuid().to_string() == task_id)
            .unwrap();
        assert_eq!(stage2_task.failed, 0, "Worker 崩溃项不应再次进入失败重试");
        assert_eq!(stage2_task.cancelled, 1, "Worker 崩溃项必须立即标记跳过");
        assert_eq!(stage2_task.succeeded, 1, "并行完成项必须独立写入成功终态");
        let live = controller_registry
            .details(&controller_reporter_id)
            .await
            .unwrap();
        assert_eq!(
            live.failures.len(),
            1,
            "Store 终态后 runtime failure 才可见"
        );
        assert!(live.failures[0].message.contains("controlled stage2 crash"));
        control
            .complete_stage2(
                task_id,
                third_item,
                Stage2Output {
                    frames: vec![Stage2Frame {
                        slot: 0,
                        feature: Some(stage2()),
                        error: None,
                    }],
                    regenerated_contact_sheet_jpeg: None,
                },
            )
            .await;
        control
    });

    let run_registry = registry.clone();
    let run_reporter = reporter.clone();
    let run = tokio::spawn(async move {
        let mut processor = WorkerPoolStage2Processor::new(&mut pool)
            .with_runtime_reporter(run_reporter.clone(), machine);
        let report = LocalAnalysisEngine::run_existing_with_runtime(
            &mut store,
            run_id,
            &mut processor,
            &run_reporter,
            7,
        )
        .await;
        drop(processor);
        (report, store, pool, run_registry)
    });

    let control = tokio::time::timeout(std::time::Duration::from_secs(3), controller)
        .await
        .expect("crash 后必须补槽并继续 B/C")
        .unwrap();
    let (report, store, pool, registry) =
        tokio::time::timeout(std::time::Duration::from_secs(3), run)
            .await
            .expect("真实 LocalAnalysis/WorkerPool 链不得死锁")
            .unwrap();
    assert_eq!(report.unwrap().status, AnalysisStatus::Partial);
    assert_eq!(control.available_slots(), 2);
    assert_eq!(pool.busy_workers(), 0);
    let stage2_task = store
        .page_tasks(None, 20)
        .unwrap()
        .items
        .into_iter()
        .find(|task| task.kind == "stage2_compute")
        .unwrap();
    assert_eq!(stage2_task.failed, 0);
    assert_eq!(stage2_task.cancelled, 1);
    assert_eq!(stage2_task.succeeded, 2);
    let details = registry.details(&reporter_id).await.unwrap();
    assert_eq!(
        details
            .workers
            .iter()
            .map(|worker| worker.completed_files)
            .sum::<u64>(),
        2
    );
    assert_eq!(details.failures.len(), 1);
}

#[tokio::test]
async fn phase2_worker_reports_actual_started_slot_pid_path_disk_and_completion() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("phase2.jpg");
    std::fs::write(&path, b"phase2").unwrap();
    let machine = MachineId::from_sha256([0x83; 32]);
    let registry = RuntimeTaskRegistry::new();
    let reporter = registry
        .begin(
            RuntimeTaskKind::Stage2Compute,
            machine.clone(),
            "二次特征计算",
        )
        .await;
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let scanned = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        6,
    );
    let content = store
        .upsert_content_and_location(&scanned, [7; 16], MediaKind::Image)
        .unwrap();
    let (mut pool, mut started, control) = WorkerPool::controlled_batch_for_test(1);
    let controller = tokio::spawn(async move {
        for _ in 0..2 {
            let (task_id, item_id) = started.recv().await.unwrap();
            control
                .complete_stage2(
                    task_id,
                    item_id,
                    Stage2Output {
                        frames: vec![Stage2Frame {
                            slot: 0,
                            feature: Some(stage2()),
                            error: None,
                        }],
                        regenerated_contact_sheet_jpeg: None,
                    },
                )
                .await;
        }
    });
    let mut processor =
        WorkerPoolStage2Processor::new(&mut pool).with_runtime_reporter(reporter.clone(), machine);
    let task_id = TaskId::from_uuid(uuid::Uuid::new_v4());
    for index in 0..2 {
        processor
            .process(Stage2Request {
                task_id,
                item_id: format!("phase2-{index}"),
                content: ContentKey::new([7; 16], 6),
                content_id: content.id,
                display_path: scanned.display_path.clone(),
                media_kind: MediaKind::Image,
                frame_slots: Vec::new(),
                contact_sheet_path: None,
            })
            .await
            .unwrap();
    }
    controller.await.unwrap();

    let details = registry.details(reporter.id()).await.unwrap();
    assert_eq!(details.workers.len(), 1);
    let worker = &details.workers[0];
    assert_eq!(worker.slot, 0);
    assert_eq!(worker.process_id, Some(1));
    assert_eq!(worker.stage_id, "compute_stage2_features");
    assert_eq!(worker.display_path, path.to_string_lossy());
    assert!(!worker.physical_disk_id.is_empty());
    assert_eq!(worker.completed_files, 2);
    assert!(worker.speed_per_second > 0.0);
    assert_eq!(worker.current_step, "计算二次特征");
    assert_eq!(worker.cache_detail, "读取原图");
}

type Seeded = (ScannedPath, LocationKey, dedup_node_store::ContentId);

fn seed_image(store: &mut NodeStore, path: &str, md5: [u8; 16], with_stage2: bool) -> Seeded {
    let scanned = ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        100,
    );
    let content = store
        .upsert_content_and_location(&scanned, md5, MediaKind::Image)
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(stage1())),
        )
        .unwrap();
    if with_stage2 {
        store
            .commit_feature_result(content.id, None, FeatureWrite::ImageStage2(stage2()))
            .unwrap();
    }
    (
        scanned.clone(),
        LocationKey::new(store.machine_id().clone(), scanned.normalized_path),
        content.id,
    )
}

fn completed_task(store: &mut NodeStore, contents: &[Seeded]) -> dedup_core::TaskId {
    let items = contents
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
    let task = store.create_task("scan", &items, 1).unwrap();
    while let Some(item) = store.claim_next_item(task, 1).unwrap() {
        store
            .complete_item(
                &item.item_id,
                TaskItemCompletion::Succeeded {
                    content_id: item.content_id,
                },
                1,
            )
            .unwrap();
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

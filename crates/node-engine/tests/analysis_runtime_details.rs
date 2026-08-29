use std::{fs, path::Path, time::Duration};

use dedup_core::{
    ContentKey, DisplayPath, EnumeratorKind, MachineId, MediaKind, NormalizedPath, TaskId,
};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_media_ffmpeg::MediaProbe;
use dedup_node_engine::{
    actor::NodeEngine,
    analysis::{Stage2Processor, Stage2Request, WorkerPoolStage2Processor, verify_result_file},
    runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry},
    scan::md5_bytes,
    server::NodeRequestHandler,
    worker::{BaseComputeOutput, Stage1Frame, Stage2Frame, Stage2Output, WorkerPool},
};
use dedup_node_store::{NodeStore, ScannedPath};
use dedup_protocol::proto;
use rusqlite::Connection;

/// 等待当前进程运行任务进入指定终态，避免用固定 sleep 掩盖状态竞态。
async fn wait_for_runtime_state(
    registry: &RuntimeTaskRegistry,
    task_id: &str,
    state: &str,
) -> proto::RuntimeTaskDetails {
    tokio::time::timeout(Duration::from_secs(3), async {
        loop {
            if let Some(details) = registry.details(task_id).await
                && details
                    .summary
                    .as_ref()
                    .is_some_and(|summary| summary.state == state)
            {
                return details;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap_or_else(|_| panic!("运行任务 {task_id} 未进入 {state} 终态"))
}

/// 验证本地分析未新增已废弃的 SQLite 运行态记录。
fn assert_no_legacy_runtime_rows(database: &Path) {
    let connection = Connection::open(database).unwrap();
    for table in [
        "tasks",
        "task_items",
        "task_stages",
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
        assert_eq!(count, 0, "瞬态本地分析不应写入 {table}");
    }
}

/// 使用受控 Worker 完成一次瞬态基础扫描，并返回扫描任务 ID。
async fn complete_base_scan(
    handle: &dedup_node_engine::actor::NodeEngineHandle,
    registry: &RuntimeTaskRegistry,
    started: &mut tokio::sync::mpsc::Receiver<(String, String)>,
    controller: &dedup_node_engine::worker::ControlledWorkerPool,
    root: &Path,
    md5s: &[[u8; 16]],
    output: BaseComputeOutput,
    file_count: usize,
) -> String {
    assert_eq!(md5s.len(), file_count);
    let response = handle
        .handle(proto::Envelope {
            request_id: 1,
            payload: Some(proto::envelope::Payload::CreateScan(proto::CreateScan {
                roots: vec![root.to_string_lossy().into_owned()],
                force_recalculate: false,
                enumerator: "windows_walker".into(),
            })),
        })
        .await;
    let Some(proto::envelope::Payload::TaskAccepted(accepted)) = response.payload else {
        panic!("瞬态基础扫描必须返回任务 ID");
    };
    for md5 in md5s {
        let (task_id, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
            .await
            .expect("基础扫描必须进入受控 Worker")
            .expect("受控 Worker 不应提前关闭");
        assert_eq!(task_id, accepted.task_id);
        controller
            .base_source_read_complete(task_id.clone(), item_id.clone())
            .await;
        controller
            .complete_base(task_id, item_id, *md5, output.clone())
            .await;
    }
    wait_for_runtime_state(registry, &accepted.task_id, "completed").await;
    accepted.task_id
}

/// 当前扫描完成后，本地分析只发布运行内存摘要和最近结果 TSV，不再写旧分析阶段表。
#[tokio::test]
async fn local_analysis_reports_current_runtime_and_tsv_without_legacy_rows() {
    let directory = tempfile::tempdir().unwrap();
    let scan_root = directory.path().join("scan");
    fs::create_dir(&scan_root).unwrap();
    fs::write(scan_root.join("left.bin"), b"same content").unwrap();
    fs::write(scan_root.join("right.bin"), b"same content").unwrap();
    let database = directory.path().join("node.db");
    let cache_root = directory.path().join("cache");
    let runtime_root = directory.path().join("data/node/runtime");
    let results_root = directory.path().join("data/node/results");
    let machine = MachineId::from_sha256([0x81; 32]);
    let store = NodeStore::open(&database, machine).unwrap();
    let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
        store,
        pool,
        "127.0.0.1:39091".parse().unwrap(),
        &cache_root,
        &runtime_root,
        EnumeratorKind::WindowsWalker,
    );
    let registry = handle.runtime_tasks_for_test();
    let scan_task = complete_base_scan(
        &handle,
        &registry,
        &mut started,
        &controller,
        &scan_root,
        &[md5_bytes(b"same content"), md5_bytes(b"same content")],
        other_base_output(),
        2,
    )
    .await;
    let response = handle
        .handle(proto::Envelope {
            request_id: 2,
            payload: Some(proto::envelope::Payload::CreateLocalAnalysis(
                proto::CreateLocalAnalysis {
                    scan_task_ids: vec![scan_task],
                    group_kind: proto::GroupKind::GroupExact as i32,
                    thresholds: None,
                },
            )),
        })
        .await;
    let Some(proto::envelope::Payload::QueryAnalysisRun(accepted)) = response.payload else {
        panic!("当前扫描必须可以启动本地分析");
    };
    let details = wait_for_runtime_state(&registry, &accepted.analysis_run_id, "completed").await;
    let summary = details.summary.unwrap();
    assert_eq!(summary.overall_total, 2);
    assert_eq!(summary.overall_completed, 2);
    assert_eq!(summary.overall_failed, 0);
    let result = verify_result_file(&results_root.join("latest-analysis.result.tsv")).unwrap();
    assert_eq!(result.group_count, 1);
    assert_eq!(result.member_count, 2);

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
    assert_no_legacy_runtime_rows(&database);
}

/// 二筛失败时本地分析保持 partial/失败运行态，不把缺失特征伪装为已完成结果。
#[tokio::test]
async fn unresolved_stage2_stays_partial_without_publishing_result() {
    let directory = tempfile::tempdir().unwrap();
    let scan_root = directory.path().join("scan");
    fs::create_dir(&scan_root).unwrap();
    fs::write(scan_root.join("left.jpg"), b"left image").unwrap();
    fs::write(scan_root.join("right.jpg"), b"right image").unwrap();
    let database = directory.path().join("node.db");
    let cache_root = directory.path().join("cache");
    let runtime_root = directory.path().join("data/node/runtime");
    let results_root = directory.path().join("data/node/results");
    let machine = MachineId::from_sha256([0x82; 32]);
    let store = NodeStore::open(&database, machine).unwrap();
    let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(1);
    let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
        store,
        pool,
        "127.0.0.1:39091".parse().unwrap(),
        &cache_root,
        &runtime_root,
        EnumeratorKind::WindowsWalker,
    );
    let registry = handle.runtime_tasks_for_test();
    let scan_task = complete_base_scan(
        &handle,
        &registry,
        &mut started,
        &controller,
        &scan_root,
        &[md5_bytes(b"left image"), md5_bytes(b"right image")],
        image_base_output(),
        2,
    )
    .await;
    let response = handle
        .handle(proto::Envelope {
            request_id: 2,
            payload: Some(proto::envelope::Payload::CreateLocalAnalysis(
                proto::CreateLocalAnalysis {
                    scan_task_ids: vec![scan_task],
                    group_kind: proto::GroupKind::GroupSimilarImage as i32,
                    thresholds: None,
                },
            )),
        })
        .await;
    let Some(proto::envelope::Payload::QueryAnalysisRun(accepted)) = response.payload else {
        panic!("当前扫描必须可以启动本地分析");
    };
    let (task_id, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
        .await
        .expect("缺失二筛必须进入 Worker")
        .expect("受控 Worker 不应提前关闭");
    controller
        .crash(task_id, item_id, "controlled stage2 crash".into())
        .await;
    let (task_id, item_id) = tokio::time::timeout(Duration::from_secs(2), started.recv())
        .await
        .expect("首个二筛失败后仍需继续下一项")
        .expect("受控 Worker 不应提前关闭");
    controller
        .stage2_source_read_complete(task_id.clone(), item_id.clone())
        .await;
    controller
        .complete_stage2(task_id, item_id, stage2_output(2))
        .await;

    let details = wait_for_runtime_state(&registry, &accepted.analysis_run_id, "failed").await;
    let stage2 = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "compute_stage2_features")
        .expect("二筛失败必须保留阶段统计");
    assert_eq!(stage2.total, 2);
    assert_eq!(stage2.completed, 1);
    assert_eq!(stage2.failed, 1);
    assert_eq!(details.failures.len(), 1);
    let query = handle
        .handle(proto::Envelope {
            request_id: 3,
            payload: Some(proto::envelope::Payload::QueryAnalysisRun(
                proto::QueryAnalysisRun {
                    analysis_run_id: accepted.analysis_run_id,
                    ..Default::default()
                },
            )),
        })
        .await;
    assert!(matches!(
        query.payload,
        Some(proto::envelope::Payload::QueryAnalysisRun(proto::QueryAnalysisRun { state, .. }))
            if state == "partial"
    ));
    assert!(!results_root.join("latest-analysis.result.tsv").exists());
    assert_eq!(controller.available_slots(), 1);

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
    assert_no_legacy_runtime_rows(&database);
}

/// 多 Worker 二筛中崩溃只影响当前项，池补回槽位并完成其他项，运行态记录失败而不写旧任务表。
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn local_analysis_real_pool_crash_recovers_slot_without_legacy_task() {
    let directory = tempfile::tempdir().unwrap();
    let scan_root = directory.path().join("scan");
    fs::create_dir(&scan_root).unwrap();
    fs::write(scan_root.join("a.jpg"), b"a image bytes").unwrap();
    fs::write(scan_root.join("b.jpg"), b"b image bytes").unwrap();
    fs::write(scan_root.join("c.jpg"), b"c image bytes").unwrap();
    let database = directory.path().join("node.db");
    let cache_root = directory.path().join("cache");
    let runtime_root = directory.path().join("data/node/runtime");
    let machine = MachineId::from_sha256([0x84; 32]);
    let store = NodeStore::open(&database, machine).unwrap();
    let (pool, mut started, controller) = WorkerPool::controlled_batch_for_test(2);
    let (handle, actor) = NodeEngine::spawn_with_runtime_root_for_test(
        store,
        pool,
        "127.0.0.1:39091".parse().unwrap(),
        &cache_root,
        &runtime_root,
        EnumeratorKind::WindowsWalker,
    );
    let registry = handle.runtime_tasks_for_test();
    let scan_task = complete_base_scan(
        &handle,
        &registry,
        &mut started,
        &controller,
        &scan_root,
        &[
            md5_bytes(b"a image bytes"),
            md5_bytes(b"b image bytes"),
            md5_bytes(b"c image bytes"),
        ],
        image_base_output(),
        3,
    )
    .await;
    let response = handle
        .handle(proto::Envelope {
            request_id: 2,
            payload: Some(proto::envelope::Payload::CreateLocalAnalysis(
                proto::CreateLocalAnalysis {
                    scan_task_ids: vec![scan_task],
                    group_kind: proto::GroupKind::GroupSimilarImage as i32,
                    thresholds: None,
                },
            )),
        })
        .await;
    let Some(proto::envelope::Payload::QueryAnalysisRun(accepted)) = response.payload else {
        panic!("当前扫描必须可以启动本地分析");
    };
    let (task_id, first_item) = tokio::time::timeout(Duration::from_secs(2), started.recv())
        .await
        .expect("二筛必须进入首个 Worker")
        .expect("受控 Worker 不应提前关闭");
    let (second_task, second_item) = tokio::time::timeout(Duration::from_secs(2), started.recv())
        .await
        .expect("二筛必须占满第二个 Worker")
        .expect("受控 Worker 不应提前关闭");
    assert_eq!(second_task, task_id);
    assert_ne!(first_item, second_item);
    controller
        .crash(
            task_id.clone(),
            first_item,
            "controlled stage2 crash".into(),
        )
        .await;
    controller
        .stage2_source_read_complete(second_task.clone(), second_item.clone())
        .await;
    controller
        .complete_stage2(second_task, second_item, stage2_output(4))
        .await;
    let (third_task, third_item) = tokio::time::timeout(Duration::from_secs(2), started.recv())
        .await
        .expect("崩溃释放槽位后必须补派第三项")
        .expect("受控 Worker 不应提前关闭");
    assert_eq!(third_task, task_id);
    controller
        .stage2_source_read_complete(third_task.clone(), third_item.clone())
        .await;
    controller
        .complete_stage2(third_task, third_item, stage2_output(5))
        .await;

    let details = wait_for_runtime_state(&registry, &accepted.analysis_run_id, "failed").await;
    let stage2 = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "compute_stage2_features")
        .expect("二筛失败必须保留阶段统计");
    assert_eq!(stage2.total, 3);
    assert_eq!(stage2.completed, 2);
    assert_eq!(stage2.failed, 1);
    assert_eq!(details.failures.len(), 1);
    assert_eq!(controller.available_slots(), 2);

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
    assert_no_legacy_runtime_rows(&database);
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

/// 构造普通文件基础结果，让当前扫描快照能够形成精确重复组。
fn other_base_output() -> BaseComputeOutput {
    BaseComputeOutput {
        probe: Some(MediaProbe {
            media_kind: MediaKind::Other,
            width: 0,
            height: 0,
            duration_ms: None,
        }),
        stage1_frames: Some(Vec::new()),
        contact_sheet_jpeg: None,
    }
}

/// 构造完整的一筛图片结果，让当前扫描快照能够真实进入二筛。
fn image_base_output() -> BaseComputeOutput {
    BaseComputeOutput {
        probe: Some(MediaProbe {
            media_kind: MediaKind::Image,
            width: 2,
            height: 2,
            duration_ms: None,
        }),
        stage1_frames: Some(vec![Stage1Frame {
            slot: 0,
            feature: Some(ImageStage1 {
                width: 2,
                height: 2,
                pdq: PdqHash::from_bytes([0; 32]),
                quality: 100,
            }),
            error: None,
        }]),
        contact_sheet_jpeg: None,
    }
}

/// 构造受控 Worker 的完整二筛结果。
fn stage2_output(seed: u64) -> Stage2Output {
    Stage2Output {
        frames: vec![Stage2Frame {
            slot: 0,
            feature: Some(ImageStage2 {
                phash_parts: [seed; 9],
                sobel: [seed as f32; 128],
            }),
            error: None,
        }],
        regenerated_contact_sheet_jpeg: None,
    }
}

fn stage2() -> ImageStage2 {
    ImageStage2 {
        phash_parts: [0; 9],
        sobel: [0.0; 128],
    }
}

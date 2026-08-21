//! Node 进程内运行任务 registry 的单调时钟、并行阶段和终态契约。

use std::{
    sync::{Arc, atomic::{AtomicU64, Ordering}},
    time::Duration,
};

use dedup_core::MachineId;
use dedup_node_engine::runtime_tasks::{
    RuntimeFailureUpdate, RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate,
    RuntimeTaskClock, RuntimeTaskKind, RuntimeTaskRegistry, RuntimeTaskState,
    RuntimeWorkerUpdate,
};
use dedup_protocol::{MAX_RUNTIME_FAILURES, proto};

#[derive(Default)]
struct ManualClock(AtomicU64);

impl ManualClock {
    fn advance(&self, duration: Duration) {
        self.0.fetch_add(
            duration.as_millis().try_into().unwrap(),
            Ordering::SeqCst,
        );
    }
}

impl RuntimeTaskClock for ManualClock {
    fn now(&self) -> Duration {
        Duration::from_millis(self.0.load(Ordering::SeqCst))
    }
}

#[tokio::test]
async fn registry_tracks_parallel_stages_speed_workers_failures_and_one_terminal_event() {
    let clock = Arc::new(ManualClock::default());
    let registry = RuntimeTaskRegistry::with_clock(clock.clone());
    let mut events = registry.subscribe();
    let task = registry
        .begin(
            RuntimeTaskKind::Scan,
            MachineId::from_sha256([0x91; 32]),
            "扫描",
        )
        .await;
    task.update_overall(0, None, 0, 0).await.unwrap();
    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ReadMd5,
        RuntimeProgressUnit::Bytes,
        0,
        Some(100),
    ))
    .await
    .unwrap();
    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ProbeStage1,
        RuntimeProgressUnit::Files,
        0,
        None,
    ))
    .await
    .unwrap();

    let summary = &registry.list().await[0];
    assert_eq!(summary.machine_id.len(), 64);
    assert_eq!(summary.state, "running");
    assert_eq!(summary.stage_summary, "读取与 MD5 / 媒体探测与一筛并行");
    assert!(!summary.overall_total_known);

    clock.advance(Duration::from_secs(5));
    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ReadMd5,
        RuntimeProgressUnit::Bytes,
        50,
        Some(100),
    ))
    .await
    .unwrap();
    let stage = &registry.details(task.id()).await.unwrap().stages[0];
    assert_eq!(stage.speed_per_second, 10.0);
    assert_eq!(stage.eta_ms, Some(5_000));
    assert!(stage.total_known);

    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ReadMd5,
        RuntimeProgressUnit::Bytes,
        55,
        Some(100),
    ))
    .await
    .unwrap();
    assert!(registry.details(task.id()).await.unwrap().stages[0]
        .speed_per_second
        .is_finite());
    clock.advance(Duration::from_secs(11));
    task.update_stage(RuntimeStageUpdate::running(
        RuntimeStage::ReadMd5,
        RuntimeProgressUnit::Bytes,
        5,
        Some(100),
    ))
    .await
    .unwrap();
    let reset = &registry.details(task.id()).await.unwrap().stages[0];
    assert_eq!(reset.speed_per_second, 0.0, "counter 回退必须重置速度窗口");
    assert_eq!(reset.eta_ms, None);

    task.update_worker(RuntimeWorkerUpdate {
        slot: 3,
        process_id: Some(9001),
        stage: RuntimeStage::ProbeStage1,
        display_path: r"D:\Media\clip.mp4".into(),
        physical_disk_id: "PhysicalDisk7".into(),
        completed_files: 18,
        speed_per_second: 3.5,
    })
    .await
    .unwrap();
    for index in 0..25 {
        task.record_failure(RuntimeFailureUpdate {
            stage: RuntimeStage::ReadMd5,
            display_path: format!(r"D:\Media\broken-{index}.bin"),
            message: format!("failure-{index}"),
        })
        .await
        .unwrap();
    }
    let details = registry.details(task.id()).await.unwrap();
    assert_eq!(details.workers[0].slot, 3);
    assert_eq!(details.workers[0].process_id, Some(9001));
    assert_eq!(details.failures.len(), MAX_RUNTIME_FAILURES);
    assert!(details.failures[0].display_path.ends_with("broken-5.bin"));

    task.finish(RuntimeTaskState::Completed).await.unwrap();
    let event = events.recv().await.unwrap();
    assert_eq!(event.runtime_task_id, task.id());
    assert_eq!(event.state, "completed");
    assert!(task.finish(RuntimeTaskState::Completed).await.is_err());
    assert!(
        task.update_stage(RuntimeStageUpdate {
            stage: RuntimeStage::ReadMd5,
            state: proto::RuntimeStageState::RuntimeStageRunning,
            unit: RuntimeProgressUnit::Bytes,
            completed: 99,
            total: Some(100),
            failed: 0,
            skipped: 0,
        })
        .await
        .is_err(),
        "终态后不得倒退到 Running"
    );
    assert!(events.try_recv().is_err(), "终态只能广播一次");
}

#[tokio::test]
async fn recreated_registry_is_empty_and_never_restores_process_history() {
    let clock = Arc::new(ManualClock::default());
    let registry = RuntimeTaskRegistry::with_clock(clock.clone());
    registry
        .begin(
            RuntimeTaskKind::Delete,
            MachineId::from_sha256([0x92; 32]),
            "删除",
        )
        .await;
    assert_eq!(registry.list().await.len(), 1);
    drop(registry);

    let recreated = RuntimeTaskRegistry::with_clock(clock);
    assert!(recreated.list().await.is_empty());
}

//! Node 重启只为未完成持久任务创建全新的临时恢复详情。

use dedup_core::MachineId;
use dedup_node_engine::{
    actor::NodeEngine, runtime_tasks::RuntimeStage, server::NodeRequestHandler,
};
use dedup_node_store::{
    NewTaskItem, NodeStore, PersistentStageState, TaskItemCompletion, TaskStageWrite,
};
use dedup_protocol::proto;

/// 重启后的 registry 复用持久任务 ID，并恢复已经落库的阶段计数和独立计时。
#[tokio::test]
async fn restart_restores_base_and_stage2_runtime_stages_from_sqlite() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("node.db");
    let machine_id = MachineId::from_sha256([0xe4; 32]);
    let persistent_ids = {
        let mut store = NodeStore::open(&database, machine_id.clone()).unwrap();
        let queued = store
            .create_task("base_compute", &[NewTaskItem::detached("queued")], 1)
            .unwrap();
        store
            .save_task_stage(
                queued,
                persisted_stage(
                    RuntimeStage::EnumerateFiles,
                    PersistentStageState::Completed,
                    1,
                    Some(1),
                    Some(100),
                    Some(200),
                ),
            )
            .unwrap();
        store
            .save_task_stage(
                queued,
                persisted_stage(
                    RuntimeStage::LookupBaseCache,
                    PersistentStageState::Running,
                    0,
                    Some(1),
                    Some(300),
                    None,
                ),
            )
            .unwrap();
        let running = store
            .create_task("stage2_compute", &[NewTaskItem::detached("running")], 2)
            .unwrap();
        store.claim_next_item(running, 3).unwrap().unwrap();
        let completed = store
            .create_task("scan", &[NewTaskItem::detached("completed")], 4)
            .unwrap();
        let completed_item = store.claim_next_item(completed, 5).unwrap().unwrap();
        store
            .complete_item(
                &completed_item.item_id,
                TaskItemCompletion::Succeeded { content_id: None },
                6,
            )
            .unwrap();
        let failed = store
            .create_task("scan", &[NewTaskItem::detached("failed")], 7)
            .unwrap();
        store.fail_task(failed, 8).unwrap();
        [queued, running, completed, failed]
    };

    let store = NodeStore::open(&database, machine_id.clone()).unwrap();
    let (handle, actor) = NodeEngine::spawn_for_test(
        store,
        "127.0.0.1:39097".parse().unwrap(),
        &directory.path().join("cache"),
    );
    let response = handle
        .handle(proto::Envelope {
            request_id: 1,
            payload: Some(proto::envelope::Payload::NodeStatus(
                proto::NodeStatus::default(),
            )),
        })
        .await;
    assert!(matches!(
        response.payload,
        Some(proto::envelope::Payload::NodeStatus(_))
    ));

    let registry = handle.runtime_tasks_for_test();
    let summaries = registry.list().await;
    assert_eq!(summaries.len(), 2, "只允许 queued/running 任务进入恢复列表");
    let base_id = persistent_ids[0].as_uuid().to_string();
    let base = summaries
        .iter()
        .find(|summary| summary.runtime_task_id == base_id)
        .unwrap();
    assert_eq!(base.task_kind, "base_compute");
    assert_eq!(base.machine_id, machine_id.as_str());
    let details = registry.details(&base_id).await.unwrap();
    assert_eq!(details.workers.len(), 0);
    assert_eq!(details.failures.len(), 0);
    assert!(
        details.execution_config.is_none(),
        "恢复任务没有本进程实际执行配置，必须保持缺失"
    );
    assert!(
        details.pipeline_metrics.is_none(),
        "恢复任务没有本进程采集指标，必须保持缺失"
    );
    assert_eq!(details.stages.len(), 2);
    let enumerate = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == RuntimeStage::EnumerateFiles.id())
        .unwrap();
    assert_eq!(enumerate.completed, 1);
    assert_eq!(enumerate.elapsed_ms, 100);

    let stage2_id = persistent_ids[1].as_uuid().to_string();
    assert_eq!(
        summaries
            .iter()
            .find(|summary| summary.runtime_task_id == stage2_id)
            .unwrap()
            .task_kind,
        "stage2_compute"
    );

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

/// 构造恢复测试使用的持久阶段快照。
fn persisted_stage(
    stage: RuntimeStage,
    state: PersistentStageState,
    completed: u64,
    total: Option<u64>,
    started_at_ms: Option<u64>,
    finished_at_ms: Option<u64>,
) -> TaskStageWrite {
    TaskStageWrite {
        stage_id: stage.id().into(),
        state,
        completed,
        total,
        failed: 0,
        skipped: 0,
        started_at_ms,
        finished_at_ms,
        warning_text: None,
    }
}

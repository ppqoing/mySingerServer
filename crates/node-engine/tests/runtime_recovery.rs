//! Node 重启只为未完成持久任务创建全新的临时恢复详情。

use dedup_core::MachineId;
use dedup_node_engine::{
    actor::NodeEngine, runtime_tasks::RuntimeStage, server::NodeRequestHandler,
};
use dedup_node_store::{NewTaskItem, NodeStore, TaskItemCompletion};
use dedup_protocol::proto;

/// 重启后的 registry 不恢复旧 ID、阶段、Worker 或失败，只包装当前活动持久任务。
#[tokio::test]
async fn restart_exposes_only_fresh_runtime_recovery_tasks() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("node.db");
    let machine_id = MachineId::from_sha256([0xe4; 32]);
    let persistent_ids = {
        let mut store = NodeStore::open(&database, machine_id.clone()).unwrap();
        let queued = store
            .create_task("scan", &[NewTaskItem::detached("queued")], 1)
            .unwrap();
        let running = store
            .create_task("analysis_stage2", &[NewTaskItem::detached("running")], 2)
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
    for summary in summaries {
        assert_eq!(summary.task_kind, "recovery");
        assert_eq!(summary.machine_id, machine_id.as_str());
        assert!(
            persistent_ids
                .iter()
                .all(|task_id| summary.runtime_task_id != task_id.as_uuid().to_string()),
            "恢复运行 ID 不得复用 SQLite 任务 ID"
        );
        let details = registry.details(&summary.runtime_task_id).await.unwrap();
        assert_eq!(details.workers.len(), 0);
        assert_eq!(details.failures.len(), 0);
        assert_eq!(details.stages.len(), 1);
        assert_eq!(
            details.stages[0].stage_id,
            RuntimeStage::RecoveryValidate.id()
        );
    }

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

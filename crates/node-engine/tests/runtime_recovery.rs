//! Node 重启清除 transient SQLite 任务，绝不构造恢复运行详情。

use dedup_core::MachineId;
use dedup_node_engine::{actor::NodeEngine, server::NodeRequestHandler};
use dedup_node_store::{NewTaskItem, NodeStore};
use dedup_protocol::proto;

/// 已落库的 running 项在下一进程启动时清除，运行 registry 只表示当前进程事实。
#[tokio::test]
async fn restart_clears_persisted_running_rows_without_publishing_runtime_recovery() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("node.db");
    let machine_id = MachineId::from_sha256([0xe4; 32]);
    let task_id = {
        let mut store = NodeStore::open(&database, machine_id.clone()).unwrap();
        let task_id = store
            .create_task("stage2_compute", &[NewTaskItem::detached("running")], 2)
            .unwrap();
        store.claim_next_item(task_id, 3).unwrap().unwrap();
        task_id
    };

    let store = NodeStore::open(&database, machine_id).unwrap();
    assert!(
        store.page_tasks(None, 20).unwrap().items.is_empty(),
        "启动清理必须删除旧 transient task 行"
    );
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
    assert!(
        handle.runtime_tasks_for_test().list().await.is_empty(),
        "旧任务 {} 不能进入新的运行 registry",
        task_id.as_uuid()
    );

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

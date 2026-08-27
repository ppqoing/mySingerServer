//! PostgreSQL 清单阶段和二次派发的幂等持久化契约。

use dedup_central_store::{
    CentralAnalysisNode, CentralStore, PersistentStageState, Stage2DispatchWrite, TaskStageWrite,
};
use dedup_core::{ContentKey, MachineId, TaskId, Thresholds};

fn machine() -> MachineId {
    MachineId::parse("7474747474747474747474747474747474747474747474747474747474747474").unwrap()
}

#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn analysis_stage_and_stage2_dispatch_are_idempotent() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let mut store = CentralStore::connect(&url).await.unwrap();
    let machine_id = machine();
    let content = ContentKey::new([0x44; 16], 123);
    seed_content(&url, content).await;
    let run = store
        .create_analysis_run(
            &Thresholds::default(),
            &[CentralAnalysisNode {
                machine_id: machine_id.clone(),
                task_id: TaskId::new(),
                task_highwater: 0,
                sync_highwater: 0,
                task_status: "queued".into(),
            }],
        )
        .await
        .unwrap();
    store
        .save_analysis_stage(
            run,
            TaskStageWrite {
                stage_id: "dispatch_stage2".into(),
                state: PersistentStageState::Running,
                completed: 0,
                total: Some(1),
                failed: 0,
                skipped: 0,
                started_at_ms: Some(100),
                finished_at_ms: None,
                warning_text: None,
            },
        )
        .await
        .unwrap();

    store
        .upsert_stage2_dispatch(
            run,
            Stage2DispatchWrite {
                machine_id: machine_id.clone(),
                content,
                node_task_id: None,
                state: "queued".into(),
                updated_at_ms: 110,
            },
        )
        .await
        .unwrap();
    let node_task_id = TaskId::new();
    store
        .upsert_stage2_dispatch(
            run,
            Stage2DispatchWrite {
                machine_id,
                content,
                node_task_id: Some(node_task_id),
                state: "running".into(),
                updated_at_ms: 120,
            },
        )
        .await
        .unwrap();

    let stages = store.analysis_stages(run).await.unwrap();
    assert_eq!(stages.len(), 1);
    assert_eq!(stages[0].started_at_ms, Some(100));
    let dispatches = store.stage2_dispatches(run).await.unwrap();
    assert_eq!(dispatches.len(), 1);
    assert_eq!(dispatches[0].node_task_id, Some(node_task_id));
    assert_eq!(dispatches[0].state, "running");
}

/// 写入派发表外键依赖的内容夹具；真实链路由 Node 同步先完成同一写入。
async fn seed_content(url: &str, content: ContentKey) {
    let (client, connection) = tokio_postgres::connect(url, tokio_postgres::NoTls)
        .await
        .unwrap();
    let connection_task = tokio::spawn(async move {
        let _ = connection.await;
    });
    client
        .execute(
            "INSERT INTO contents(md5,file_size,media_kind,base_complete) VALUES($1,$2,'image',TRUE) \
             ON CONFLICT(md5,file_size) DO NOTHING",
            &[&content.md5().as_slice(), &(content.file_size() as i64)],
        )
        .await
        .unwrap();
    drop(client);
    connection_task.abort();
}

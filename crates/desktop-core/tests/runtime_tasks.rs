use dedup_core::{DesktopConfig, MachineId};
use dedup_desktop_core::{
    app::{DesktopApp, UiCommand},
    runtime_tasks::{
        CROSS_ANALYSIS_STAGES, DELETE_STAGES, DesktopRuntimeTaskKind, DesktopRuntimeTaskRegistry,
        DesktopRuntimeTaskState, RuntimeTaskKey, RuntimeTaskOwner, SYNC_STAGES,
    },
    view_state::DesktopPaths,
};
use dedup_protocol::proto;
use tempfile::TempDir;

#[test]
fn desktop_registry_is_stable_ephemeral_and_exposes_fixed_stage_sets() {
    let registry = DesktopRuntimeTaskRegistry::new();
    assert!(registry.snapshot().is_empty());
    let machine_a = MachineId::from_sha256([0xc1; 32]);
    let machine_b = MachineId::from_sha256([0xc2; 32]);
    let cross = registry.begin_cross_analysis(
        "cross-run",
        &[machine_b.clone(), machine_a.clone(), machine_a.clone()],
        "跨机器分析",
    );
    let delete = registry.begin_delete(
        "delete-run",
        &[machine_b.clone(), machine_a.clone()],
        "删除",
        3,
    );

    let first = registry.list();
    assert_eq!(first.len(), 2);
    assert!(first[0].key < first[1].key);
    assert_eq!(
        registry
            .details(cross.key())
            .unwrap()
            .stages
            .iter()
            .map(|stage| stage.stage_id.as_str())
            .collect::<Vec<_>>(),
        CROSS_ANALYSIS_STAGES
            .iter()
            .map(|stage| stage.id)
            .collect::<Vec<_>>()
    );
    assert_eq!(
        registry
            .details(delete.key())
            .unwrap()
            .stages
            .iter()
            .map(|stage| stage.stage_id.as_str())
            .collect::<Vec<_>>(),
        DELETE_STAGES
            .iter()
            .map(|stage| stage.id)
            .collect::<Vec<_>>()
    );
    assert_eq!(
        registry.details(cross.key()).unwrap().machine_ids,
        vec![machine_a.as_str().to_owned(), machine_b.as_str().to_owned()]
    );
    cross.finish(DesktopRuntimeTaskState::Completed).unwrap();
    delete.finish(DesktopRuntimeTaskState::Failed).unwrap();
    assert_eq!(registry.list()[0].state, DesktopRuntimeTaskState::Completed);
    assert!(DesktopRuntimeTaskRegistry::new().snapshot().is_empty());
}

#[test]
fn active_sync_triggers_merge_and_terminal_then_creates_a_new_row() {
    let registry = DesktopRuntimeTaskRegistry::new();
    let machine = MachineId::from_sha256([0xc3; 32]);
    let first = registry.begin_or_merge_sync(&machine, "同步");
    let merged = registry.begin_or_merge_sync(&machine, "手动同步");
    assert_eq!(first.key(), merged.key());
    assert_eq!(registry.list().len(), 1);
    assert_eq!(
        registry
            .details(first.key())
            .unwrap()
            .stages
            .iter()
            .map(|stage| stage.stage_id.as_str())
            .collect::<Vec<_>>(),
        SYNC_STAGES.iter().map(|stage| stage.id).collect::<Vec<_>>()
    );
    first.finish(DesktopRuntimeTaskState::Completed).unwrap();
    let next = registry.begin_or_merge_sync(&machine, "下一轮同步");
    assert_ne!(first.key(), next.key());
    assert_eq!(registry.list().len(), 2);
}

#[test]
fn node_summary_uses_handshake_machine_identity_and_unified_key() {
    let machine = MachineId::from_sha256([0xc4; 32]);
    let snapshot = DesktopRuntimeTaskRegistry::node_snapshot(
        7,
        &machine,
        proto::RuntimeTaskSummary {
            runtime_task_id: "node-runtime".into(),
            machine_id: "spoofed".into(),
            task_kind: "scan".into(),
            title: "扫描".into(),
            state: "running".into(),
            stage_summary: "读取".into(),
            overall_completed: 4,
            overall_total: 9,
            overall_total_known: true,
            overall_failed: 0,
            overall_skipped: 0,
        },
    );
    assert_eq!(
        snapshot.key,
        RuntimeTaskKey {
            owner: RuntimeTaskOwner::Node { node_index: 7 },
            id: "node-runtime".into(),
        }
    );
    assert_eq!(snapshot.machine_ids, vec![machine.as_str()]);
    assert_eq!(snapshot.kind, DesktopRuntimeTaskKind::Node);
    assert_eq!(snapshot.overall_completed, 4);
}

#[test]
fn node_compute_kinds_use_three_fixed_product_titles() {
    let machine = MachineId::from_sha256([0xc6; 32]);
    for (task_kind, expected_title) in [
        ("base_compute", "基础计算"),
        ("duplicate_list", "重复文件清单"),
        ("stage2_compute", "二次特征计算"),
    ] {
        let snapshot = DesktopRuntimeTaskRegistry::node_snapshot(
            0,
            &machine,
            proto::RuntimeTaskSummary {
                runtime_task_id: format!("runtime-{task_kind}"),
                machine_id: machine.as_str().into(),
                task_kind: task_kind.into(),
                title: "旧标题不得覆盖产品任务名".into(),
                state: "running".into(),
                ..Default::default()
            },
        );
        assert_eq!(snapshot.title, expected_title);
    }
}

#[tokio::test]
async fn desktop_app_owns_one_ephemeral_registry_per_process_start() {
    let first_temp = TempDir::new().unwrap();
    let mut config = DesktopConfig::default();
    config.nodes.clear();
    config.reconnect_interval_seconds = 60;
    let (first_app, _first_events) = DesktopApp::start(config.clone(), desktop_paths(&first_temp));
    let first_registry = first_app.runtime_tasks();
    let machine = MachineId::from_sha256([0xc5; 32]);
    first_registry.begin_or_merge_sync(&machine, "同步");
    assert_eq!(first_app.runtime_tasks().list().len(), 1);
    first_app.send(UiCommand::Shutdown).await.unwrap();

    let second_temp = TempDir::new().unwrap();
    let (second_app, _second_events) = DesktopApp::start(config, desktop_paths(&second_temp));
    assert!(second_app.runtime_tasks().snapshot().is_empty());
    second_app.send(UiCommand::Shutdown).await.unwrap();
}

/// 为控制器测试构造完全隔离的临时 Desktop 路径。
fn desktop_paths(temp: &TempDir) -> DesktopPaths {
    DesktopPaths {
        data: temp.path().to_path_buf(),
        logs: temp.path().join("logs"),
        cache: temp.path().join("cache"),
        config: temp.path().join("config.toml"),
    }
}

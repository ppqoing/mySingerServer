//! 管理端纯状态模型的节点编辑、计算门禁和设置校验契约。

use std::path::PathBuf;

use dedup_core::{DeleteMode, DesktopConfig, Thresholds};
use dedup_desktop_core::view_state::{
    DesktopPaths, DesktopViewState, NodeConnectionState, NodeRuntimeStats, PostgresHealth,
    TaskView, ViewTaskState,
};

#[test]
fn manual_nodes_can_be_added_edited_removed_and_receive_runtime_state() {
    let mut state = state();
    let index = state.add_node("192.168.1.20", 39092).unwrap();
    assert_eq!(index, 1);
    assert_eq!(state.nodes()[1].endpoint.to_string(), "192.168.1.20:39092");

    state.edit_node(index, "192.168.1.21", 39093).unwrap();
    state.set_node_connection(
        index,
        NodeConnectionState::Online,
        Some(NodeRuntimeStats {
            worker_count: 4,
            busy_workers: 2,
            queued_items: 3,
            running_items: 2,
            outbox_high_seq: 77,
            sync_high_seq: 70,
        }),
    );
    assert_eq!(
        state.nodes()[index].endpoint.to_string(),
        "192.168.1.21:39093"
    );
    assert_eq!(state.nodes()[index].connection, NodeConnectionState::Online);
    assert_eq!(state.nodes()[index].stats.as_ref().unwrap().busy_workers, 2);

    state.remove_node(index).unwrap();
    assert_eq!(state.nodes().len(), 1);
}

#[test]
fn queued_or_running_work_disables_filtering_with_progress_reason() {
    let mut state = state();
    state.upsert_task(TaskView {
        task_id: "scan-1".into(),
        node_index: 0,
        title: "媒体扫描".into(),
        stage: "图片一筛".into(),
        state: ViewTaskState::Running,
        completed_items: 40,
        total_items: 100,
        failed_items: 2,
        skipped_incomplete: 3,
    });
    let availability = state.filtering_availability();
    assert!(!availability.enabled);
    assert!(availability.reason.contains("等待所有节点计算完成"));
    assert_eq!(state.tasks()[0].progress_percent(), 40);

    state.tasks_mut()[0].state = ViewTaskState::Completed;
    assert!(state.filtering_availability().enabled);
}

#[test]
fn missing_postgres_schema_only_disables_central_mode() {
    let mut state = state();
    state.set_postgres_health(PostgresHealth::SchemaMissing);
    assert!(state.local_mode_enabled());
    assert!(!state.central_mode_enabled());
    assert!(state.postgres_message().contains("手动执行"));
}

#[test]
fn invalid_thresholds_are_rejected_and_default_delete_uses_recycle_bin() {
    let mut state = state();
    assert_eq!(state.config().delete_mode, DeleteMode::RecycleBin);
    let original = state.config().clone();
    let mut invalid = original.clone();
    invalid.thresholds = Thresholds {
        sobel_min: 1.5,
        ..Thresholds::default()
    };
    assert!(state.apply_settings(invalid).is_err());
    assert_eq!(state.config(), &original);
}

fn state() -> DesktopViewState {
    DesktopViewState::new(
        DesktopConfig::default(),
        DesktopPaths {
            data: PathBuf::from(r"C:\portable\data\desktop"),
            logs: PathBuf::from(r"C:\portable\data\desktop\logs"),
            cache: PathBuf::from(r"C:\portable\data\desktop\cache"),
            config: PathBuf::from(r"C:\portable\data\desktop\config.toml"),
        },
    )
}

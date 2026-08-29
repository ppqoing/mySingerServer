use std::{io::Cursor, path::PathBuf, sync::Arc};

use dedup_core::{DeleteMode, DesktopConfig, EnumeratorKind};
use dedup_desktop_core::{
    app::{PathEntryView, UiCommand, UiEvent},
    results::GroupKind,
    review::{QuickReviewRule, ReviewDecision},
    view_state::{DesktopPaths, DesktopViewState, TaskView, ViewTaskState},
};
use dedup_desktop_ui::{MainWindow, UiScanRootRow, apply_event, bind_commands};
use i_slint_backend_testing::ElementHandle;
use slint::{ComponentHandle, Model, ModelRc, VecModel};
use tokio::sync::mpsc;

fn next(receiver: &mut mpsc::Receiver<UiCommand>) -> UiCommand {
    receiver.try_recv().expect("回调应把命令发送到真实 channel")
}

fn accessible(window: &MainWindow, label: &str) -> ElementHandle {
    ElementHandle::find_by_accessible_label(window, label)
        .next()
        .unwrap_or_else(|| panic!("应找到可访问元素：{label}"))
}

#[test]
fn config_callbacks_draft_are_single_shot_and_switching_clears_remote_state() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, mut receiver) = mpsc::channel(16);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let mut state = DesktopViewState::new(
        DesktopConfig::default(),
        DesktopPaths {
            data: PathBuf::from(r"C:\fixture\desktop"),
            logs: PathBuf::from(r"C:\fixture\desktop\logs"),
            cache: PathBuf::from(r"C:\fixture\desktop\cache"),
            config: PathBuf::from(r"C:\fixture\desktop\config.toml"),
        },
    );
    state.set_node_identity(0, "machine-local");
    state.set_node_connection(
        0,
        dedup_desktop_core::view_state::NodeConnectionState::Online,
        None,
    );
    let offline = state.add_node("10.0.0.8", 39091).expect("离线节点 fixture");
    state.set_node_identity(offline, "machine-offline");
    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));
    let options = window.get_node_config_options();
    assert_eq!(
        slint::Model::row_data(&options, 0).as_deref(),
        Some("本机节点 · machine-local · 127.0.0.1:39091 · 在线"),
    );
    assert_eq!(
        slint::Model::row_data(&options, 1).as_deref(),
        Some("计算节点 2 · machine-offline · 10.0.0.8:39091 · 离线"),
    );
    window.invoke_navigate_to(6);
    accessible(&window, "节点服务").invoke_accessible_default_action();

    window.invoke_select_node_config(0);
    accessible(&window, "加载配置").invoke_accessible_default_action();
    match next(&mut receiver) {
        UiCommand::LoadNodeConfig { node_index } => assert_eq!(node_index, 0),
        command => panic!("加载远程配置命令错误：{command:?}"),
    }
    assert!(receiver.try_recv().is_err(), "一次加载动作只能发送一条命令");

    window.set_node_config_loaded(true);
    window.set_node_config_dirty(true);
    window.set_node_config_listen_ip("0.0.0.0".into());
    window.set_node_config_port(39100);
    window.set_node_config_enumerator_index(1);
    window.set_node_config_data_path("data\\node".into());
    window.set_node_config_config_path("data\\node\\config.toml".into());
    window.set_node_config_log_path("data\\node\\logs".into());
    window.set_node_config_cache_path("data\\node\\cache".into());
    window.set_node_config_hdd_threads(1);
    window.set_node_config_ssd_threads(2);
    window.set_node_config_unknown_threads(1);
    window.set_node_config_total_threads(4);
    window.set_node_config_block_size(4 * 1024 * 1024);
    window.set_node_config_timeout_seconds(3);
    window.set_node_config_retries(2);
    window.set_node_config_legacy_workers(4);
    window.set_node_config_worker_mode_index(0);
    window.set_node_config_reserved_cores(1);
    window.set_node_config_manual_workers(2);
    accessible(&window, "保存配置").invoke_accessible_default_action();
    match next(&mut receiver) {
        UiCommand::SaveNodeConfig { node_index, config } => {
            assert_eq!(node_index, 0);
            assert_eq!(config.listen_ip, "0.0.0.0");
            assert_eq!(config.port, 39100);
            assert_eq!(config.data_path, "data\\node");
            assert_eq!(config.block_timeout_seconds, 3);
            assert_eq!(config.block_retries, 2);
        }
        command => panic!("保存远程配置命令错误：{command:?}"),
    }
    assert!(receiver.try_recv().is_err(), "一次保存动作只能发送一条命令");

    window.set_scan_root("D:\\Media".into());
    window.set_node_config_loaded(true);
    window.set_node_config_dirty(true);
    window.set_node_config_saving(true);
    window.set_node_config_machine_id("stale-machine".into());
    window.set_node_config_version("stale-version".into());
    window.set_node_config_phase("stale-phase".into());
    window.set_node_config_error("stale-error".into());
    window.set_node_config_listen_ip("192.0.2.10".into());
    window.set_node_config_port(49999);
    window.set_node_config_enumerator_index(0);
    window.set_node_config_data_path("stale-data".into());
    window.set_node_config_config_path("stale-config".into());
    window.set_node_config_log_path("stale-log".into());
    window.set_node_config_cache_path("stale-cache".into());
    window.set_node_config_hdd_threads(63);
    window.set_node_config_ssd_threads(62);
    window.set_node_config_unknown_threads(61);
    window.set_node_config_total_threads(60);
    window.set_node_config_block_size(65536);
    window.set_node_config_timeout_seconds(59);
    window.set_node_config_retries(10);
    window.set_node_config_legacy_workers(59);
    window.set_node_config_worker_mode_index(1);
    window.set_node_config_reserved_cores(58);
    window.set_node_config_manual_workers(57);
    window.set_node_config_logical_cpus(56);
    window.set_node_config_effective_workers(55);
    // ComboBox 的双向 current-index 会先写根属性，再触发 selected 回调。
    window.set_node_config_selected_index(1);
    window.invoke_select_node_config(1);
    assert_eq!(window.get_node_config_selected_index(), 1);
    assert!(!window.get_node_config_loaded(), "切换节点必须清除远程快照");
    assert!(!window.get_node_config_dirty());
    assert!(!window.get_node_config_saving());
    assert_eq!(window.get_scan_root(), "", "切换节点必须清除旧扫描路径");
    assert_eq!(window.get_node_config_machine_id(), "");
    assert_eq!(window.get_node_config_version(), "");
    assert_eq!(window.get_node_config_phase(), "未加载");
    assert_eq!(window.get_node_config_error(), "");
    assert_eq!(window.get_node_config_listen_ip(), "");
    assert_eq!(window.get_node_config_port(), 39091);
    assert_eq!(window.get_node_config_enumerator_index(), 1);
    assert_eq!(window.get_node_config_data_path(), "");
    assert_eq!(window.get_node_config_config_path(), "");
    assert_eq!(window.get_node_config_log_path(), "");
    assert_eq!(window.get_node_config_cache_path(), "");
    assert_eq!(window.get_node_config_hdd_threads(), 1);
    assert_eq!(window.get_node_config_ssd_threads(), 2);
    assert_eq!(window.get_node_config_unknown_threads(), 1);
    assert_eq!(window.get_node_config_total_threads(), 4);
    assert_eq!(window.get_node_config_block_size(), 4 * 1024 * 1024);
    assert_eq!(window.get_node_config_timeout_seconds(), 3);
    assert_eq!(window.get_node_config_retries(), 2);
    assert_eq!(window.get_node_config_legacy_workers(), 1);
    assert_eq!(window.get_node_config_worker_mode_index(), 0);
    assert_eq!(window.get_node_config_reserved_cores(), 1);
    assert_eq!(window.get_node_config_manual_workers(), 1);
    assert_eq!(window.get_node_config_logical_cpus(), 0);
    assert_eq!(window.get_node_config_effective_workers(), 0);

    window.set_node_config_listen_ip("same-selection-must-survive".into());
    window.set_node_config_dirty(true);
    window.set_scan_root("E:\\Keep".into());
    window.invoke_select_node_config(1);
    assert_eq!(
        window.get_node_config_listen_ip(),
        "same-selection-must-survive"
    );
    assert!(
        window.get_node_config_dirty(),
        "同一节点重复回调不得再次清表单"
    );
    assert_eq!(
        window.get_scan_root(),
        "E:\\Keep",
        "同一节点不得再次清扫描根"
    );
    assert_eq!(
        accessible(&window, "加载配置").accessible_enabled(),
        Some(false),
        "离线节点必须禁用加载",
    );
    assert_eq!(
        accessible(&window, "保存配置").accessible_enabled(),
        Some(false),
        "离线节点必须禁用保存",
    );
    accessible(&window, "加载配置").invoke_accessible_default_action();
    accessible(&window, "保存配置").invoke_accessible_default_action();
    assert!(receiver.try_recv().is_err(), "离线动作不得发送命令");

    window.invoke_save_settings();
    assert!(matches!(next(&mut receiver), UiCommand::SaveSettings(_)));
    assert!(receiver.try_recv().is_err(), "旧保存设置不得冒充 Node 保存");
}

#[test]
fn config_periodic_and_phase_draft_preserves_dirty_edits() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, _receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let mut state = dedup_desktop_core::view_state::DesktopViewState::new(
        DesktopConfig::default(),
        dedup_desktop_core::view_state::DesktopPaths {
            data: std::path::PathBuf::from(r"C:\fixture\desktop"),
            logs: std::path::PathBuf::from(r"C:\fixture\desktop\logs"),
            cache: std::path::PathBuf::from(r"C:\fixture\desktop\cache"),
            config: std::path::PathBuf::from(r"C:\fixture\desktop\config.toml"),
        },
    );
    state.set_node_identity(0, "machine-local");
    state.set_node_connection(
        0,
        dedup_desktop_core::view_state::NodeConnectionState::Online,
        None,
    );
    apply_event(
        &window,
        &binding,
        UiEvent::ViewChanged(Box::new(state.clone())),
    );

    window.set_node_config_loaded(true);
    window.set_node_config_dirty(true);
    window.set_node_config_listen_ip("198.51.100.23".into());
    window.set_node_config_data_path("edited-data".into());
    window.set_node_config_retries(9);
    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));
    assert!(window.get_node_config_loaded());
    assert!(window.get_node_config_dirty());
    assert_eq!(window.get_node_config_listen_ip(), "198.51.100.23");
    assert_eq!(window.get_node_config_data_path(), "edited-data");
    assert_eq!(window.get_node_config_retries(), 9);

    apply_event(
        &window,
        &binding,
        UiEvent::NodeConfigChanged(
            dedup_desktop_core::view_state::NodeConfigControllerState::default(),
        ),
    );
    assert!(window.get_node_config_loaded());
    assert!(window.get_node_config_dirty());
    assert_eq!(window.get_node_config_listen_ip(), "198.51.100.23");
    assert_eq!(window.get_node_config_data_path(), "edited-data");
    assert_eq!(window.get_node_config_retries(), 9);
}

#[test]
fn scan_root_items_add_choose_delete_and_submit_one_multi_root_task() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1080, 700));
    window.invoke_navigate_to(2);
    window.set_scan_node_index(7);
    let (sender, mut receiver) = mpsc::channel(16);
    let binding = bind_commands(&window, sender, DesktopConfig::default());

    let roots = window.get_scan_roots();
    assert_eq!(roots.row_count(), 1);
    assert_eq!(
        roots.row_data(0).expect("默认扫描路径 Item").path,
        "D:\\Media"
    );

    accessible(&window, "添加扫描路径").invoke_accessible_default_action();
    let roots = window.get_scan_roots();
    assert_eq!(roots.row_count(), 2, "每次添加必须创建一个独立 Item");
    assert_eq!(roots.row_data(1).expect("新增扫描路径 Item").path, "");
    assert!(!window.get_scan_roots_valid(), "空 Item 必须阻止扫描");

    accessible(&window, "选择扫描路径：2").invoke_accessible_default_action();
    assert!(
        window.get_path_picker_open(),
        "Item 的选择路径动作必须立即打开节点路径选择器"
    );
    match next(&mut receiver) {
        UiCommand::BrowsePaths {
            node_index,
            parent_path,
            cursor,
        } => assert_eq!(
            (node_index, parent_path, cursor),
            (7, String::new(), String::new())
        ),
        command => panic!("首次路径浏览命令错误：{command:?}"),
    }

    apply_event(
        &window,
        &binding,
        UiEvent::PathsChanged {
            node_index: 7,
            parent_path: String::new(),
            entries: vec![
                PathEntryView {
                    display_path: "E:\\".into(),
                    is_directory: true,
                },
                PathEntryView {
                    display_path: "C:\\pagefile.sys".into(),
                    is_directory: false,
                },
            ],
            next_cursor: String::new(),
        },
    );
    accessible(&window, "进入目录：E:\\").invoke_accessible_default_action();
    match next(&mut receiver) {
        UiCommand::BrowsePaths {
            node_index,
            parent_path,
            cursor,
        } => assert_eq!(
            (node_index, parent_path, cursor),
            (7, "E:\\".into(), String::new())
        ),
        command => panic!("进入盘符命令错误：{command:?}"),
    }
    assert!(
        ElementHandle::find_by_accessible_label(&window, "进入目录：C:\\pagefile.sys")
            .next()
            .is_none(),
        "路径选择器只应显示可选择的目录",
    );

    apply_event(
        &window,
        &binding,
        UiEvent::PathsChanged {
            node_index: 7,
            parent_path: "E:\\".into(),
            entries: vec![PathEntryView {
                display_path: "E:\\Archive".into(),
                is_directory: true,
            }],
            next_cursor: String::new(),
        },
    );
    accessible(&window, "进入目录：E:\\Archive").invoke_accessible_default_action();
    match next(&mut receiver) {
        UiCommand::BrowsePaths {
            node_index,
            parent_path,
            cursor,
        } => assert_eq!(
            (node_index, parent_path, cursor),
            (7, "E:\\Archive".into(), String::new())
        ),
        command => panic!("进入目标目录命令错误：{command:?}"),
    }

    apply_event(
        &window,
        &binding,
        UiEvent::PathsChanged {
            node_index: 7,
            parent_path: "E:\\Archive".into(),
            entries: Vec::new(),
            next_cursor: String::new(),
        },
    );
    accessible(&window, "选择此文件夹").invoke_accessible_default_action();
    assert!(!window.get_path_picker_open());
    let roots = window.get_scan_roots();
    assert_eq!(roots.row_data(0).expect("第一项").path, "D:\\Media");
    assert_eq!(roots.row_data(1).expect("第二项").path, "E:\\Archive");
    assert!(window.get_scan_roots_valid());

    accessible(&window, "开始扫描").invoke_accessible_default_action();
    match next(&mut receiver) {
        UiCommand::CreateScan { roots, .. } => {
            assert_eq!(roots, ["D:\\Media", "E:\\Archive"])
        }
        command => panic!("多扫描根命令错误：{command:?}"),
    }

    accessible(&window, "选择扫描路径：1").invoke_accessible_default_action();
    let _ = next(&mut receiver);
    accessible(&window, "取消选择目录").invoke_accessible_default_action();
    assert!(!window.get_path_picker_open());
    assert_eq!(
        window
            .get_scan_roots()
            .row_data(0)
            .expect("取消后第一项")
            .path,
        "D:\\Media",
        "取消不得修改正在编辑的 Item"
    );

    accessible(&window, "删除扫描路径：1").invoke_accessible_default_action();
    let roots = window.get_scan_roots();
    assert_eq!(roots.row_count(), 1);
    assert_eq!(roots.row_data(0).expect("删除后剩余项").path, "E:\\Archive");

    accessible(&window, "删除扫描路径：1").invoke_accessible_default_action();
    assert_eq!(window.get_scan_roots().row_count(), 0);
    assert!(!window.get_scan_roots_valid());
    assert_eq!(
        accessible(&window, "开始扫描").accessible_enabled(),
        Some(false),
        "删除全部 Item 后必须禁用扫描"
    );
}

#[test]
fn switching_scan_node_clears_paths_and_the_open_remote_picker() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, mut receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let mut state = DesktopViewState::new(
        DesktopConfig::default(),
        DesktopPaths {
            data: PathBuf::from(r"C:\fixture\desktop"),
            logs: PathBuf::from(r"C:\fixture\desktop\logs"),
            cache: PathBuf::from(r"C:\fixture\desktop\cache"),
            config: PathBuf::from(r"C:\fixture\desktop\config.toml"),
        },
    );
    state.set_node_identity(0, "machine-local");
    let remote = state
        .add_node("10.0.0.8", 39092)
        .expect("远程节点 fixture 应有效");
    state.set_node_identity(remote, "machine-remote");
    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));
    let options = window.get_scan_node_options();
    assert_eq!(options.row_count(), 2);
    assert_eq!(
        options.row_data(0).as_deref(),
        Some("machine-local · 本机节点 · 127.0.0.1:39091")
    );
    assert_eq!(
        options.row_data(1).as_deref(),
        Some("machine-remote · 计算节点 2 · 10.0.0.8:39092")
    );
    window.set_scan_roots(ModelRc::new(VecModel::from(vec![
        UiScanRootRow {
            path: "D:\\Media".into(),
        },
        UiScanRootRow {
            path: "E:\\Archive".into(),
        },
    ])));
    window.set_scan_roots_valid(true);
    window.set_path_picker_open(true);
    window.set_path_picker_node_index(0);
    window.set_path_picker_target_index(1);
    window.set_path_picker_current_path("E:\\Archive".into());

    window.invoke_select_scan_node(1);

    assert_eq!(window.get_scan_node_index(), 1);
    assert_eq!(window.get_scan_roots().row_count(), 0);
    assert_eq!(window.get_scan_root(), "");
    assert!(!window.get_scan_roots_valid());
    assert!(!window.get_path_picker_open());
    assert_eq!(window.get_path_picker_target_index(), -1);
    assert_eq!(window.get_path_picker_current_path(), "");
    assert_eq!(window.get_path_picker_directories().row_count(), 0);
    assert_eq!(window.get_last_error(), "节点已切换，请重新添加扫描路径");
    assert!(
        receiver.try_recv().is_err(),
        "切换扫描节点只重置 UI 选择，不得误发后端命令",
    );
}

#[test]
fn root_callbacks_emit_their_ui_commands_and_reject_invalid_settings() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, mut receiver) = mpsc::channel(32);
    let _binding = bind_commands(&window, sender, DesktopConfig::default());

    window.invoke_start_scan(
        -9,
        ModelRc::new(VecModel::from(vec![UiScanRootRow {
            path: "D:\\Evidence".into(),
        }])),
        true,
        1,
    );
    match next(&mut receiver) {
        UiCommand::CreateScan {
            node_index,
            roots,
            force_recalculate,
            enumerator,
        } => {
            assert_eq!(node_index, 0);
            assert_eq!(roots, ["D:\\Evidence"]);
            assert!(force_recalculate);
            assert_eq!(enumerator, EnumeratorKind::Everything);
        }
        command => panic!("start-scan 参数顺序错误：{command:?}"),
    }
    window.invoke_start_scan(
        4,
        ModelRc::new(VecModel::from(vec![UiScanRootRow {
            path: "E:\\Archive".into(),
        }])),
        false,
        7,
    );
    match next(&mut receiver) {
        UiCommand::CreateScan {
            node_index,
            roots,
            force_recalculate,
            enumerator,
        } => {
            assert_eq!(node_index, 4);
            assert_eq!(roots, ["E:\\Archive"]);
            assert!(!force_recalculate);
            assert_eq!(enumerator, EnumeratorKind::WindowsWalker);
        }
        command => panic!("非 Everything 枚举器应回退 WindowsWalker：{command:?}"),
    }

    window.invoke_add_node("10.0.0.4".into(), 39099);
    match next(&mut receiver) {
        UiCommand::AddNode { ip, port } => assert_eq!((ip, port), ("10.0.0.4".into(), 39099)),
        command => panic!("add-node 命令错误：{command:?}"),
    }
    window.invoke_edit_node(-3, "10.0.0.5".into(), 39100);
    match next(&mut receiver) {
        UiCommand::EditNode { index, ip, port } => {
            assert_eq!((index, ip, port), (0, "10.0.0.5".into(), 39100));
        }
        command => panic!("edit-node 命令错误：{command:?}"),
    }
    window.invoke_remove_node(-2);
    assert!(matches!(
        next(&mut receiver),
        UiCommand::RemoveNode { index: 0 }
    ));
    window.invoke_connect_all();
    assert!(matches!(next(&mut receiver), UiCommand::ConnectAll));
    window.invoke_refresh();
    assert!(matches!(next(&mut receiver), UiCommand::Refresh));
    window.invoke_sync_node(-1);
    assert!(matches!(
        next(&mut receiver),
        UiCommand::SyncNow { index: 0 }
    ));
    window.invoke_browse_paths(-8, "D:\\Library".into());
    match next(&mut receiver) {
        UiCommand::BrowsePaths {
            node_index,
            parent_path,
            cursor,
        } => assert_eq!(
            (node_index, parent_path, cursor),
            (0, "D:\\Library".into(), String::new())
        ),
        command => panic!("browse-paths 命令错误：{command:?}"),
    }
    window.invoke_cancel_task(-4, "cancel-task".into());
    match next(&mut receiver) {
        UiCommand::CancelTask {
            node_index,
            task_id,
        } => assert_eq!((node_index, task_id), (0, "cancel-task".into())),
        command => panic!("cancel-task 命令错误：{command:?}"),
    }
    window.invoke_start_local_analysis(-5, "scan-a,scan-b".into(), 2);
    match next(&mut receiver) {
        UiCommand::StartLocalAnalysis {
            node_index,
            scan_task_ids,
            kind,
        } => assert_eq!(
            (node_index, scan_task_ids, kind),
            (0, "scan-a,scan-b".into(), GroupKind::SimilarVideo)
        ),
        command => panic!("start-local-analysis 命令错误：{command:?}"),
    }
    window.invoke_start_cross_analysis("0:scan-a,1:scan-b".into());
    match next(&mut receiver) {
        UiCommand::StartCrossAnalysis { selections } => assert_eq!(selections, "0:scan-a,1:scan-b"),
        command => panic!("start-cross-analysis 命令错误：{command:?}"),
    }
    window.invoke_poll_cross_analysis();
    assert!(matches!(next(&mut receiver), UiCommand::PollCrossAnalysis));
    window.invoke_retry_cross_analysis();
    assert!(matches!(next(&mut receiver), UiCommand::RetryCrossAnalysis));
    window.invoke_load_groups(true, -6, "run-1".into(), 2, "group-cursor".into());
    match next(&mut receiver) {
        UiCommand::LoadGroups {
            central,
            node_index,
            analysis_run_id,
            kind,
            cursor,
        } => assert_eq!(
            (central, node_index, analysis_run_id, kind, cursor),
            (
                true,
                0,
                "run-1".into(),
                GroupKind::SimilarVideo,
                "group-cursor".into()
            )
        ),
        command => panic!("load-groups 命令错误：{command:?}"),
    }
    window.invoke_load_members(
        false,
        -7,
        "run-2".into(),
        "group-1".into(),
        1,
        "member-cursor".into(),
    );
    match next(&mut receiver) {
        UiCommand::LoadMembers {
            central,
            node_index,
            analysis_run_id,
            group_id,
            kind,
            cursor,
        } => assert_eq!(
            (central, node_index, analysis_run_id, group_id, kind, cursor),
            (
                false,
                0,
                "run-2".into(),
                "group-1".into(),
                GroupKind::SimilarImage,
                "member-cursor".into()
            )
        ),
        command => panic!("load-members 命令错误：{command:?}"),
    }
    window.invoke_save_review("machine-a".into(), "D:\\Media\\keep.jpg".into(), 2);
    match next(&mut receiver) {
        UiCommand::SaveReview {
            machine_id,
            normalized_path,
            decision,
        } => assert_eq!(
            (machine_id, normalized_path, decision),
            (
                "machine-a".into(),
                "D:\\Media\\keep.jpg".into(),
                ReviewDecision::Delete
            )
        ),
        command => panic!("save-review 命令错误：{command:?}"),
    }
    window.invoke_quick_review(3, "archive".into());
    match next(&mut receiver) {
        UiCommand::ApplyQuickReview(QuickReviewRule::PathContains(value)) => {
            assert_eq!(value, "archive");
        }
        command => panic!("quick-review 命令错误：{command:?}"),
    }
    window.invoke_load_preview("machine-b".into(), "D:\\Media\\preview.jpg".into());
    match next(&mut receiver) {
        UiCommand::LoadPreview {
            machine_id,
            normalized_path,
        } => assert_eq!(
            (machine_id, normalized_path),
            ("machine-b".into(), "D:\\Media\\preview.jpg".into())
        ),
        command => panic!("load-preview 命令错误：{command:?}"),
    }
    window.invoke_prepare_delete();
    assert!(matches!(next(&mut receiver), UiCommand::PrepareDelete));
    window.invoke_confirm_delete();
    assert!(matches!(next(&mut receiver), UiCommand::ConfirmDelete));
    window.set_postgres_host("10.0.0.20".into());
    window.set_postgres_port(5433);
    window.set_postgres_database("media db".into());
    window.set_postgres_username("dedup user".into());
    window.set_postgres_password("p@ss:/word".into());
    window.set_reconnect_seconds(17);
    window.set_delete_mode_index(1);
    window.set_pdq_quality("61".into());
    window.set_aspect_tolerance("0.27".into());
    window.set_pdq_hamming("42".into());
    window.set_phash_hamming("13".into());
    window.set_phash_parts("6".into());
    window.set_sobel_min("0.73".into());
    window.set_video_valid("5".into());
    window.set_video_stage1("0.66".into());
    window.set_video_stage2("0.91".into());
    window.invoke_save_settings();
    match next(&mut receiver) {
        UiCommand::SaveSettings(config) => {
            assert_eq!(config.nodes.len(), 1);
            assert_eq!(config.nodes[0].ip.to_string(), "127.0.0.1");
            assert_eq!(config.nodes[0].port, 39091);
            assert_eq!(
                config.postgres_url.as_deref(),
                Some("postgresql://dedup%20user:p%40ss%3A%2Fword@10.0.0.20:5433/media%20db")
            );
            assert_eq!(config.reconnect_interval_seconds, 17);
            assert_eq!(config.delete_mode, DeleteMode::Permanent);
            assert_eq!(config.thresholds.pdq_quality_min, 61);
            assert_eq!(config.thresholds.aspect_tolerance, 0.27);
            assert_eq!(config.thresholds.pdq_hamming_max, 42);
            assert_eq!(config.thresholds.phash_part_hamming_max, 13);
            assert_eq!(config.thresholds.phash_min_passed_parts, 6);
            assert_eq!(config.thresholds.sobel_min, 0.73);
            assert_eq!(config.thresholds.video_min_valid_frames, 5);
            assert_eq!(config.thresholds.video_stage1_min, 0.66);
            assert_eq!(config.thresholds.video_stage2_min, 0.91);
        }
        command => panic!("save-settings 命令错误：{command:?}"),
    }

    window.set_pdq_quality("not-a-number".into());
    window.invoke_save_settings();
    assert!(receiver.try_recv().is_err());
    assert!(window.get_last_error().contains("PDQ Quality 不是有效数值"));
}

#[test]
fn postgres_connection_fields_load_from_the_existing_url() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, _receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let mut config = DesktopConfig::default();
    config.postgres_url =
        Some("postgresql://reader:s%40fe@192.168.1.9:15439/media%20archive".into());
    config.reconnect_interval_seconds = 19;
    let state = DesktopViewState::new(
        config,
        DesktopPaths {
            data: PathBuf::from(r"C:\fixture\desktop"),
            logs: PathBuf::from(r"C:\fixture\desktop\logs"),
            cache: PathBuf::from(r"C:\fixture\desktop\cache"),
            config: PathBuf::from(r"C:\fixture\desktop\config.toml"),
        },
    );

    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));

    assert_eq!(window.get_postgres_host(), "192.168.1.9");
    assert_eq!(window.get_postgres_port(), 15439);
    assert_eq!(window.get_postgres_database(), "media archive");
    assert_eq!(window.get_postgres_username(), "reader");
    assert_eq!(window.get_postgres_password(), "s@fe");
    assert_eq!(window.get_reconnect_seconds(), 19);
}

#[test]
fn database_test_uses_unsaved_fields_and_reports_schema_status() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, mut receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    window.set_postgres_host("db.internal".into());
    window.set_postgres_port(15432);
    window.set_postgres_database("media db".into());
    window.set_postgres_username("reader user".into());
    window.set_postgres_password("p@ss".into());

    window.invoke_test_database_connection();

    assert!(window.get_database_testing(), "发送后应立即进入检测中状态");
    assert!(matches!(
        next(&mut receiver),
        UiCommand::TestDatabaseConnection { ref url }
            if url == "postgresql://reader%20user:p%40ss@db.internal:15432/media%20db"
    ));

    apply_event(
        &window,
        &binding,
        UiEvent::DatabaseDiagnosticsChanged(Ok(())),
    );

    assert!(!window.get_database_testing());
    assert_eq!(
        window.get_database_test_status(),
        "连接成功 · Rust V2 schema 正常"
    );
    assert_eq!(window.get_database_test_error(), "");
}

#[test]
fn periodic_view_updates_preserve_unsaved_database_credentials() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, _receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let paths = DesktopPaths {
        data: PathBuf::from(r"C:\fixture\desktop"),
        logs: PathBuf::from(r"C:\fixture\desktop\logs"),
        cache: PathBuf::from(r"C:\fixture\desktop\cache"),
        config: PathBuf::from(r"C:\fixture\desktop\config.toml"),
    };

    // 首次快照负责初始化表单；随后用户输入尚未保存的数据库连接字段。
    apply_event(
        &window,
        &binding,
        UiEvent::ViewChanged(Box::new(DesktopViewState::new(
            DesktopConfig::default(),
            paths.clone(),
        ))),
    );
    window.set_postgres_host("db.internal".into());
    window.set_postgres_database("media".into());
    window.set_postgres_username("editor".into());
    window.set_postgres_password("secret".into());

    // 节点状态等周期刷新仍携带同一份已保存配置，不得覆盖正在编辑的连接字段。
    apply_event(
        &window,
        &binding,
        UiEvent::ViewChanged(Box::new(DesktopViewState::new(
            DesktopConfig::default(),
            paths,
        ))),
    );

    assert_eq!(window.get_postgres_host(), "db.internal");
    assert_eq!(window.get_postgres_database(), "media");
    assert_eq!(window.get_postgres_username(), "editor");
    assert_eq!(window.get_postgres_password(), "secret");
}

#[test]
fn preview_completions_preserve_identity_and_increment_sequence() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, _receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());

    let same_failure = UiEvent::PreviewFailed {
        machine_id: "machine-a".into(),
        normalized_path: "D:\\Media\\same-failure.jpg".into(),
        error: "节点拒绝读取".into(),
    };
    apply_event(&window, &binding, same_failure.clone());
    assert_eq!(window.get_preview_result_machine(), "machine-a");
    assert_eq!(
        window.get_preview_result_path(),
        "D:\\Media\\same-failure.jpg"
    );
    assert!(!window.get_preview_result_succeeded());
    assert_eq!(window.get_preview_result_sequence(), 1);
    assert_eq!(window.get_last_error(), "节点拒绝读取");

    apply_event(&window, &binding, same_failure);
    assert_eq!(
        window.get_preview_result_sequence(),
        2,
        "相同身份和相同错误连续完成也必须产生新 sequence",
    );

    apply_event(
        &window,
        &binding,
        UiEvent::PreviewReady {
            machine_id: "machine-b".into(),
            normalized_path: "D:\\Media\\decode-failure.jpg".into(),
            display_path: "D:\\Display\\decode-failure.jpg".into(),
            file_kind: "original".into(),
            bytes: Arc::from([0_u8, 1, 2, 3]),
        },
    );
    assert_eq!(window.get_preview_result_machine(), "machine-b");
    assert_eq!(
        window.get_preview_result_path(),
        "D:\\Media\\decode-failure.jpg"
    );
    assert!(!window.get_preview_result_succeeded());
    assert_eq!(window.get_preview_result_sequence(), 3);
    assert!(window.get_last_error().contains("预览格式无法解码"));

    let mut png = Cursor::new(Vec::new());
    image::DynamicImage::new_rgba8(1, 1)
        .write_to(&mut png, image::ImageFormat::Png)
        .expect("应能生成内存 PNG fixture");
    apply_event(
        &window,
        &binding,
        UiEvent::PreviewReady {
            machine_id: "machine-b".into(),
            normalized_path: "D:\\Media\\ready.jpg".into(),
            display_path: "D:\\Display\\ready.jpg".into(),
            file_kind: "original".into(),
            bytes: Arc::from(png.into_inner()),
        },
    );
    assert_eq!(window.get_preview_result_machine(), "machine-b");
    assert_eq!(window.get_preview_result_path(), "D:\\Media\\ready.jpg");
    assert!(window.get_preview_result_succeeded());
    assert_eq!(window.get_preview_result_sequence(), 4);
    assert_eq!(window.get_last_error(), "");
    assert!(window.get_preview_info().contains("D:\\Display\\ready.jpg"));
}

#[test]
fn remote_node_config_callbacks_map_identity_and_send_only_task6_commands() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let find = |label: &str| {
        i_slint_backend_testing::ElementHandle::find_by_accessible_label(&window, label)
            .next()
            .unwrap_or_else(|| panic!("应找到可访问元素：{label}"))
    };
    let (sender, mut receiver) = mpsc::channel(16);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let mut state = dedup_desktop_core::view_state::DesktopViewState::new(
        DesktopConfig::default(),
        dedup_desktop_core::view_state::DesktopPaths {
            data: std::path::PathBuf::from(r"C:\fixture\desktop"),
            logs: std::path::PathBuf::from(r"C:\fixture\desktop\logs"),
            cache: std::path::PathBuf::from(r"C:\fixture\desktop\cache"),
            config: std::path::PathBuf::from(r"C:\fixture\desktop\config.toml"),
        },
    );
    state.set_node_identity(0, "machine-local");
    state.set_node_connection(
        0,
        dedup_desktop_core::view_state::NodeConnectionState::Online,
        None,
    );
    let offline = state.add_node("10.0.0.8", 39091).expect("离线节点 fixture");
    state.set_node_identity(offline, "machine-offline");
    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));
    let options = window.get_node_config_options();
    assert_eq!(
        slint::Model::row_data(&options, 0).as_deref(),
        Some("本机节点 · machine-local · 127.0.0.1:39091 · 在线"),
    );
    assert_eq!(
        slint::Model::row_data(&options, 1).as_deref(),
        Some("计算节点 2 · machine-offline · 10.0.0.8:39091 · 离线"),
    );
    window.invoke_navigate_to(6);
    find("节点服务").invoke_accessible_default_action();

    window.invoke_select_node_config(0);
    find("加载配置").invoke_accessible_default_action();
    match next(&mut receiver) {
        UiCommand::LoadNodeConfig { node_index } => assert_eq!(node_index, 0),
        command => panic!("加载远程配置命令错误：{command:?}"),
    }
    assert!(receiver.try_recv().is_err(), "一次加载动作只能发送一条命令");

    window.set_node_config_loaded(true);
    window.set_node_config_dirty(true);
    window.set_node_config_listen_ip("0.0.0.0".into());
    window.set_node_config_port(39100);
    window.set_node_config_enumerator_index(1);
    window.set_node_config_data_path("data\\node".into());
    window.set_node_config_config_path("data\\node\\config.toml".into());
    window.set_node_config_log_path("data\\node\\logs".into());
    window.set_node_config_cache_path("data\\node\\cache".into());
    window.set_node_config_hdd_threads(1);
    window.set_node_config_ssd_threads(2);
    window.set_node_config_unknown_threads(1);
    window.set_node_config_total_threads(4);
    window.set_node_config_block_size(4 * 1024 * 1024);
    window.set_node_config_timeout_seconds(3);
    window.set_node_config_retries(2);
    window.set_node_config_legacy_workers(4);
    window.set_node_config_worker_mode_index(0);
    window.set_node_config_reserved_cores(1);
    window.set_node_config_manual_workers(2);
    window.set_node_config_postgres_enabled(true);
    window.set_node_config_postgres_host("10.0.0.30".into());
    window.set_node_config_postgres_port(15432);
    window.set_node_config_postgres_database("media_node".into());
    window.set_node_config_postgres_username("node_user".into());
    window.set_node_config_postgres_password("node_secret".into());
    window.set_node_config_postgres_timeout_seconds(8);
    find("保存配置").invoke_accessible_default_action();
    match next(&mut receiver) {
        UiCommand::SaveNodeConfig { node_index, config } => {
            assert_eq!(node_index, 0);
            assert_eq!(config.listen_ip, "0.0.0.0");
            assert_eq!(config.port, 39100);
            assert_eq!(config.data_path, "data\\node");
            assert_eq!(config.block_timeout_seconds, 3);
            assert_eq!(config.block_retries, 2);
            let postgres = config.postgres.expect("Node PostgreSQL 配置必须完整下发");
            assert!(postgres.enabled);
            assert_eq!(postgres.host, "10.0.0.30");
            assert_eq!(postgres.port, 15432);
            assert_eq!(postgres.database, "media_node");
            assert_eq!(postgres.username, "node_user");
            assert_eq!(postgres.password, "node_secret");
            assert_eq!(postgres.connect_timeout_seconds, 8);
        }
        command => panic!("保存远程配置命令错误：{command:?}"),
    }
    assert!(receiver.try_recv().is_err(), "一次保存动作只能发送一条命令");

    window.set_scan_root("D:\\Media".into());
    window.invoke_select_node_config(1);
    assert_eq!(window.get_node_config_selected_index(), 1);
    assert!(!window.get_node_config_loaded(), "切换节点必须清除远程快照");
    assert!(!window.get_node_config_dirty());
    assert_eq!(window.get_scan_root(), "", "切换节点必须清除旧扫描路径");
    assert_eq!(find("加载配置").accessible_enabled(), Some(false));
    assert_eq!(find("保存配置").accessible_enabled(), Some(false));
    find("加载配置").invoke_accessible_default_action();
    find("保存配置").invoke_accessible_default_action();
    assert!(receiver.try_recv().is_err(), "离线动作不得发送命令");

    window.invoke_save_settings();
    assert!(matches!(next(&mut receiver), UiCommand::SaveSettings(_)));
    assert!(receiver.try_recv().is_err(), "旧保存设置不得冒充 Node 保存");
}

#[test]
fn remote_node_config_combo_order_clears_once_and_resets_every_field() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, _receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let mut state = dedup_desktop_core::view_state::DesktopViewState::new(
        DesktopConfig::default(),
        dedup_desktop_core::view_state::DesktopPaths {
            data: std::path::PathBuf::from(r"C:\fixture\desktop"),
            logs: std::path::PathBuf::from(r"C:\fixture\desktop\logs"),
            cache: std::path::PathBuf::from(r"C:\fixture\desktop\cache"),
            config: std::path::PathBuf::from(r"C:\fixture\desktop\config.toml"),
        },
    );
    state.set_node_identity(0, "machine-local");
    let second = state.add_node("10.0.0.8", 39091).expect("第二节点 fixture");
    state.set_node_identity(second, "machine-second");
    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));

    window.set_scan_root("D:\\Media".into());
    window.set_node_config_loaded(true);
    window.set_node_config_dirty(true);
    window.set_node_config_saving(true);
    window.set_node_config_machine_id("stale-machine".into());
    window.set_node_config_version("stale-version".into());
    window.set_node_config_phase("stale-phase".into());
    window.set_node_config_error("stale-error".into());
    window.set_node_config_listen_ip("192.0.2.10".into());
    window.set_node_config_port(49999);
    window.set_node_config_enumerator_index(0);
    window.set_node_config_data_path("stale-data".into());
    window.set_node_config_config_path("stale-config".into());
    window.set_node_config_log_path("stale-log".into());
    window.set_node_config_cache_path("stale-cache".into());
    window.set_node_config_hdd_threads(63);
    window.set_node_config_ssd_threads(62);
    window.set_node_config_unknown_threads(61);
    window.set_node_config_total_threads(60);
    window.set_node_config_block_size(65536);
    window.set_node_config_timeout_seconds(59);
    window.set_node_config_retries(10);
    window.set_node_config_legacy_workers(59);
    window.set_node_config_worker_mode_index(1);
    window.set_node_config_reserved_cores(58);
    window.set_node_config_manual_workers(57);
    window.set_node_config_logical_cpus(56);
    window.set_node_config_effective_workers(55);
    window.set_node_config_postgres_enabled(true);
    window.set_node_config_postgres_host("stale-db".into());
    window.set_node_config_postgres_port(15433);
    window.set_node_config_postgres_database("stale-database".into());
    window.set_node_config_postgres_username("stale-user".into());
    window.set_node_config_postgres_password("stale-password".into());
    window.set_node_config_postgres_timeout_seconds(59);

    // 真实 ComboBox 顺序：双向绑定先把根属性写成第二项，再触发 selected 回调。
    window.set_node_config_selected_index(1);
    window.invoke_select_node_config(1);

    assert_eq!(window.get_node_config_selected_index(), 1);
    assert!(!window.get_node_config_loaded());
    assert!(!window.get_node_config_dirty());
    assert!(!window.get_node_config_saving());
    assert_eq!(window.get_scan_root(), "");
    assert_eq!(window.get_node_config_machine_id(), "");
    assert_eq!(window.get_node_config_version(), "");
    assert_eq!(window.get_node_config_phase(), "未加载");
    assert_eq!(window.get_node_config_error(), "");
    assert_eq!(window.get_node_config_listen_ip(), "");
    assert_eq!(window.get_node_config_port(), 39091);
    assert_eq!(window.get_node_config_enumerator_index(), 1);
    assert_eq!(window.get_node_config_data_path(), "");
    assert_eq!(window.get_node_config_config_path(), "");
    assert_eq!(window.get_node_config_log_path(), "");
    assert_eq!(window.get_node_config_cache_path(), "");
    assert_eq!(window.get_node_config_hdd_threads(), 1);
    assert_eq!(window.get_node_config_ssd_threads(), 2);
    assert_eq!(window.get_node_config_unknown_threads(), 1);
    assert_eq!(window.get_node_config_total_threads(), 4);
    assert_eq!(window.get_node_config_block_size(), 4 * 1024 * 1024);
    assert_eq!(window.get_node_config_timeout_seconds(), 3);
    assert_eq!(window.get_node_config_retries(), 2);
    assert_eq!(window.get_node_config_legacy_workers(), 1);
    assert_eq!(window.get_node_config_worker_mode_index(), 0);
    assert_eq!(window.get_node_config_reserved_cores(), 1);
    assert_eq!(window.get_node_config_manual_workers(), 1);
    assert_eq!(window.get_node_config_logical_cpus(), 0);
    assert_eq!(window.get_node_config_effective_workers(), 0);
    assert!(!window.get_node_config_postgres_enabled());
    assert_eq!(window.get_node_config_postgres_host(), "127.0.0.1");
    assert_eq!(window.get_node_config_postgres_port(), 5432);
    assert_eq!(window.get_node_config_postgres_database(), "media_dedup");
    assert_eq!(window.get_node_config_postgres_username(), "postgres");
    assert_eq!(window.get_node_config_postgres_password(), "");
    assert_eq!(window.get_node_config_postgres_timeout_seconds(), 3);

    window.set_node_config_listen_ip("same-selection-must-survive".into());
    window.set_node_config_dirty(true);
    window.set_scan_root("E:\\Keep".into());
    window.invoke_select_node_config(1);
    assert_eq!(
        window.get_node_config_listen_ip(),
        "same-selection-must-survive"
    );
    assert!(window.get_node_config_dirty());
    assert_eq!(window.get_scan_root(), "E:\\Keep");
}

#[test]
fn remote_node_config_view_and_phase_events_preserve_dirty_fields() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, _receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let mut state = dedup_desktop_core::view_state::DesktopViewState::new(
        DesktopConfig::default(),
        dedup_desktop_core::view_state::DesktopPaths {
            data: std::path::PathBuf::from(r"C:\fixture\desktop"),
            logs: std::path::PathBuf::from(r"C:\fixture\desktop\logs"),
            cache: std::path::PathBuf::from(r"C:\fixture\desktop\cache"),
            config: std::path::PathBuf::from(r"C:\fixture\desktop\config.toml"),
        },
    );
    state.set_node_identity(0, "machine-local");
    state.set_node_connection(
        0,
        dedup_desktop_core::view_state::NodeConnectionState::Online,
        None,
    );
    apply_event(
        &window,
        &binding,
        UiEvent::ViewChanged(Box::new(state.clone())),
    );

    window.set_node_config_loaded(true);
    window.set_node_config_dirty(true);
    window.set_node_config_listen_ip("198.51.100.23".into());
    window.set_node_config_data_path("edited-data".into());
    window.set_node_config_retries(9);
    window.set_node_config_postgres_username("edited-node-user".into());
    window.set_node_config_postgres_password("edited-node-secret".into());
    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));
    assert!(window.get_node_config_loaded());
    assert!(window.get_node_config_dirty());
    assert_eq!(window.get_node_config_listen_ip(), "198.51.100.23");
    assert_eq!(window.get_node_config_data_path(), "edited-data");
    assert_eq!(window.get_node_config_retries(), 9);
    assert_eq!(
        window.get_node_config_postgres_username(),
        "edited-node-user"
    );
    assert_eq!(
        window.get_node_config_postgres_password(),
        "edited-node-secret"
    );

    // 没有新快照的阶段事件也不得把用户编辑当成一次加载覆盖。
    apply_event(
        &window,
        &binding,
        UiEvent::NodeConfigChanged(
            dedup_desktop_core::view_state::NodeConfigControllerState::default(),
        ),
    );
    assert!(window.get_node_config_loaded());
    assert!(window.get_node_config_dirty());
    assert_eq!(window.get_node_config_listen_ip(), "198.51.100.23");
    assert_eq!(window.get_node_config_data_path(), "edited-data");
    assert_eq!(window.get_node_config_retries(), 9);
    assert_eq!(
        window.get_node_config_postgres_username(),
        "edited-node-user"
    );
    assert_eq!(
        window.get_node_config_postgres_password(),
        "edited-node-secret"
    );
}

#[test]
fn runtime_task_details_map_unknown_totals_workers_failures_and_selection_once() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, mut receiver) = mpsc::channel(8);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let node_key = dedup_desktop_core::runtime_tasks::RuntimeTaskKey {
        owner: dedup_desktop_core::runtime_tasks::RuntimeTaskOwner::Node { node_index: 3 },
        id: "runtime-node".into(),
    };
    let desktop_key = dedup_desktop_core::runtime_tasks::RuntimeTaskKey {
        owner: dedup_desktop_core::runtime_tasks::RuntimeTaskOwner::Desktop,
        id: "runtime-desktop".into(),
    };
    let summaries = vec![
        dedup_desktop_core::runtime_tasks::RuntimeTaskSnapshot {
            key: node_key.clone(),
            machine_ids: vec!["machine-unique-7".into()],
            kind: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskKind::Node,
            title: "节点扫描".into(),
            state: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskState::Running,
            overall_completed: 7,
            overall_total: None,
            overall_failed: 2,
            overall_skipped: 1,
            stages: Vec::new(),
            failures: Vec::new(),
        },
        dedup_desktop_core::runtime_tasks::RuntimeTaskSnapshot {
            key: desktop_key.clone(),
            machine_ids: vec!["machine-a".into(), "machine-b".into()],
            kind: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskKind::CrossAnalysis,
            title: "跨机器分析".into(),
            state: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskState::Completed,
            overall_completed: 12,
            overall_total: Some(12),
            overall_failed: 0,
            overall_skipped: 0,
            stages: Vec::new(),
            failures: Vec::new(),
        },
    ];
    let failures = (0..25)
        .map(|index| dedup_protocol::proto::RuntimeFailureDetails {
            stage_id: "read_md5".into(),
            display_path: format!(r"D:\Media\broken-{index}.mp4"),
            message: format!("读取失败 {index}"),
        })
        .collect();
    let details = dedup_desktop_core::view_state::RuntimeTaskDetailsView::Node {
        node_index: 3,
        machine_id: "machine-unique-7".into(),
        details: dedup_protocol::proto::RuntimeTaskDetails {
            summary: Some(dedup_protocol::proto::RuntimeTaskSummary {
                runtime_task_id: "runtime-node".into(),
                machine_id: "machine-unique-7".into(),
                task_kind: "scan".into(),
                title: "节点扫描".into(),
                state: "running".into(),
                stage_summary: "读取与 MD5".into(),
                overall_completed: 7,
                overall_total: 0,
                overall_total_known: false,
                overall_failed: 2,
                overall_skipped: 1,
                ..Default::default()
            }),
            stages: vec![dedup_protocol::proto::RuntimeStageDetails {
                stage_id: "read_md5".into(),
                display_name: "读取与 MD5".into(),
                state: dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning as i32,
                unit: "bytes".into(),
                completed: 7,
                total: 0,
                total_known: false,
                failed: 2,
                skipped: 1,
                speed_per_second: 2048.0,
                elapsed_ms: 2500,
                eta_ms: None,
            }],
            workers: vec![dedup_protocol::proto::RuntimeWorkerDetails {
                slot: 2,
                process_id: None,
                stage_id: "probe_stage1".into(),
                display_path: r"D:\Media\clip.mp4".into(),
                physical_disk_id: "PhysicalDisk7".into(),
                completed_files: 18,
                speed_per_second: 3.5,
                current_step: "生成缩略图".into(),
                cache_detail: "复用本地缩略图".into(),
                phase: Some(dedup_protocol::proto::RuntimeWorkerPhase::RuntimeWorkerFeature as i32),
                cpu_weight: Some(3),
                decoder_threads: Some(2),
            }],
            failures,
            execution_config: Some(dedup_protocol::proto::RuntimeExecutionConfig {
                hash_tasks: Some(4),
                path_cache_queue_capacity: Some(2),
                content_cache_queue_capacity: Some(3),
                decode_queue_capacity: Some(5),
                persist_queue_capacity: Some(2),
                worker_slots: Some(2),
                cpu_budget: Some(6),
                global_disk_permits: Some(4),
                hdd_per_disk_permits: Some(1),
                ssd_per_disk_permits: Some(2),
                unknown_per_disk_permits: Some(1),
            }),
            pipeline_metrics: Some(dedup_protocol::proto::RuntimePipelineMetrics {
                hash_queue: Some(dedup_protocol::proto::RuntimeQueueMetrics {
                    current: Some(0),
                    peak: Some(2),
                    capacity: Some(4),
                    ..Default::default()
                }),
                hash_bytes: Some(4_096),
                hash_waiting_permit: Some(dedup_protocol::proto::RuntimeOwnershipMetrics {
                    current: Some(1),
                    peak: Some(2),
                    capacity: Some(3),
                }),
                hash_reading: Some(dedup_protocol::proto::RuntimeOwnershipMetrics::default()),
                hash_completed_unjoined: Some(
                    dedup_protocol::proto::RuntimeOwnershipMetrics::default(),
                ),
                media_permit_waiting: Some(
                    dedup_protocol::proto::RuntimeOwnershipMetrics::default(),
                ),
                media_acquire_ready: Some(dedup_protocol::proto::RuntimeOwnershipMetrics::default()),
                media_permit_ready: Some(dedup_protocol::proto::RuntimeOwnershipMetrics::default()),
                worker_dispatching: Some(dedup_protocol::proto::RuntimeOwnershipMetrics::default()),
                worker_start_pending: Some(
                    dedup_protocol::proto::RuntimeOwnershipMetrics::default(),
                ),
                worker_decode: Some(dedup_protocol::proto::RuntimeOwnershipMetrics::default()),
                worker_feature: Some(dedup_protocol::proto::RuntimeOwnershipMetrics::default()),
                worker_result_wait: Some(dedup_protocol::proto::RuntimeOwnershipMetrics::default()),
                worker_phase_unknown: Some(
                    dedup_protocol::proto::RuntimeOwnershipMetrics::default(),
                ),
                content_output_credit_owned: Some(
                    dedup_protocol::proto::RuntimeOwnershipMetrics::default(),
                ),
                hash_refill_token_available: Some(
                    dedup_protocol::proto::RuntimeOwnershipMetrics::default(),
                ),
                decode_credit_owned: Some(dedup_protocol::proto::RuntimeOwnershipMetrics::default()),
                item_completion_latency: Some(dedup_protocol::proto::RuntimeLatencyHistogram {
                    count: 1,
                    p95_ms: Some(42),
                    ..Default::default()
                }),
                ..Default::default()
            }),
        },
    };
    let state = dedup_desktop_core::view_state::RuntimeTaskControllerState::from_parts_for_test(
        summaries,
        Some(node_key),
        Some(details),
        true,
        Some("节点连接已断开".into()),
    );

    apply_event(&window, &binding, UiEvent::RuntimeTasksChanged(state));

    let node = window.get_tasks().row_data(0).expect("应映射节点任务");
    assert_eq!(node.runtime_id, "runtime-node");
    assert_eq!(node.owner_kind, "node");
    assert_eq!(node.node_index, 3);
    assert_eq!(node.machine_id, "machine-unique-7");
    assert!(node.stale);
    let desktop = window.get_tasks().row_data(1).expect("应映射 Desktop 任务");
    assert_eq!(desktop.runtime_id, "runtime-desktop");
    assert_eq!(desktop.owner_kind, "desktop");
    assert_eq!(desktop.machine_id, "machine-a、machine-b");
    assert!(!desktop.stale);

    let stage = window
        .get_runtime_stages()
        .row_data(0)
        .expect("应映射运行阶段");
    assert_eq!(stage.counts, "7 / —");
    assert_eq!(stage.speed, "2.0 KiB/s");
    assert_eq!(stage.eta, "—");
    assert_eq!(stage.elapsed, "2.5 秒");
    let worker = window
        .get_runtime_workers()
        .row_data(0)
        .expect("应映射 Worker");
    assert_eq!(worker.identity, "槽位 2");
    assert_eq!(worker.step, "生成缩略图");
    assert_eq!(worker.cache_detail, "复用本地缩略图");
    assert_eq!(worker.path, r"D:\Media\clip.mp4");
    assert_eq!(worker.disk, "PhysicalDisk7");
    assert_eq!(worker.speed, "3.5 文件/秒");
    assert_eq!(worker.phase, "特征计算");
    assert_eq!(worker.cpu_weight, "3");
    assert_eq!(worker.decoder_threads, "2");
    assert!(
        window
            .get_runtime_execution_config()
            .contains("Hash 并发 4")
    );
    assert!(
        window
            .get_runtime_pipeline_metrics()
            .contains("Hash队列 当前 0 / 峰值 2 / 容量 4")
    );
    assert!(
        window
            .get_runtime_pipeline_metrics()
            .contains("等待 —；耗时 —"),
        "空直方图必须显示占位符，不能伪造零延迟",
    );
    assert!(
        window
            .get_runtime_pipeline_metrics()
            .contains("Hash字节 4.0 KiB")
    );
    assert!(
        window
            .get_runtime_pipeline_metrics()
            .contains("Hash / media")
    );
    assert!(
        window
            .get_runtime_pipeline_metrics()
            .contains("Hash等待许可 当前 1 / 峰值 2 / 容量 3")
    );
    assert!(
        window
            .get_runtime_pipeline_metrics()
            .contains("Worker phase")
    );
    assert!(window.get_runtime_pipeline_metrics().contains("credit"));
    assert!(
        window
            .get_runtime_pipeline_metrics()
            .contains("item P95 42ms")
    );
    assert_eq!(window.get_runtime_failures().row_count(), 20);
    assert_eq!(
        window.get_runtime_failures().row_data(0).unwrap().message,
        "读取失败 5"
    );
    assert_eq!(
        window.get_runtime_failures().row_data(19).unwrap().message,
        "读取失败 24"
    );
    assert_eq!(window.get_runtime_detail_machine_id(), "machine-unique-7");
    assert!(window.get_runtime_detail_stale());
    assert_eq!(window.get_runtime_detail_error(), "节点连接已断开");

    window.invoke_select_runtime_task("node".into(), 3, "runtime-node".into());
    assert!(matches!(
        next(&mut receiver),
        UiCommand::SelectRuntimeTask {
            key: dedup_desktop_core::runtime_tasks::RuntimeTaskKey {
                owner: dedup_desktop_core::runtime_tasks::RuntimeTaskOwner::Node { node_index: 3 },
                ref id,
            }
        } if id == "runtime-node"
    ));
    assert!(receiver.try_recv().is_err(), "选择回调只能转发一次");

    window.invoke_select_runtime_task("desktop".into(), 99, "runtime-desktop".into());
    assert!(matches!(
        next(&mut receiver),
        UiCommand::SelectRuntimeTask {
            key: dedup_desktop_core::runtime_tasks::RuntimeTaskKey {
                owner: dedup_desktop_core::runtime_tasks::RuntimeTaskOwner::Desktop,
                ref id,
            }
        } if id == "runtime-desktop"
    ));
    assert!(receiver.try_recv().is_err(), "Desktop 选择也只能转发一次");
}

#[test]
fn unavailable_node_and_desktop_telemetry_use_dash() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, _receiver) = mpsc::channel(4);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let node_key = dedup_desktop_core::runtime_tasks::RuntimeTaskKey {
        owner: dedup_desktop_core::runtime_tasks::RuntimeTaskOwner::Node { node_index: 0 },
        id: "legacy-node".into(),
    };
    let node_summary = dedup_desktop_core::runtime_tasks::RuntimeTaskSnapshot {
        key: node_key.clone(),
        machine_ids: vec!["legacy-machine".into()],
        kind: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskKind::Node,
        title: "旧节点任务".into(),
        state: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskState::Running,
        overall_completed: 0,
        overall_total: None,
        overall_failed: 0,
        overall_skipped: 0,
        stages: Vec::new(),
        failures: Vec::new(),
    };
    let legacy = dedup_desktop_core::view_state::RuntimeTaskControllerState::from_parts_for_test(
        vec![node_summary],
        Some(node_key),
        Some(
            dedup_desktop_core::view_state::RuntimeTaskDetailsView::Node {
                node_index: 0,
                machine_id: "legacy-machine".into(),
                details: dedup_protocol::proto::RuntimeTaskDetails::default(),
            },
        ),
        false,
        None,
    );
    apply_event(&window, &binding, UiEvent::RuntimeTasksChanged(legacy));
    assert_eq!(window.get_runtime_execution_config(), "—");
    assert_eq!(window.get_runtime_pipeline_metrics(), "—");

    let desktop_key = dedup_desktop_core::runtime_tasks::RuntimeTaskKey {
        owner: dedup_desktop_core::runtime_tasks::RuntimeTaskOwner::Desktop,
        id: "desktop-owned".into(),
    };
    let desktop_summary = dedup_desktop_core::runtime_tasks::RuntimeTaskSnapshot {
        key: desktop_key.clone(),
        machine_ids: vec!["desktop-machine".into()],
        kind: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskKind::CrossAnalysis,
        title: "Desktop 自有任务".into(),
        state: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskState::Running,
        overall_completed: 0,
        overall_total: Some(0),
        overall_failed: 0,
        overall_skipped: 0,
        stages: Vec::new(),
        failures: Vec::new(),
    };
    let desktop = dedup_desktop_core::view_state::RuntimeTaskControllerState::from_parts_for_test(
        vec![desktop_summary.clone()],
        Some(desktop_key),
        Some(dedup_desktop_core::view_state::RuntimeTaskDetailsView::Desktop(desktop_summary)),
        false,
        None,
    );
    apply_event(&window, &binding, UiEvent::RuntimeTasksChanged(desktop));
    assert_eq!(window.get_runtime_execution_config(), "—");
    assert_eq!(window.get_runtime_pipeline_metrics(), "—");
}

/// 构造一个会被旧 ViewChanged 快照覆盖的运行任务与持久任务对照样本。
fn single_owner_task_fixture() -> (
    dedup_desktop_core::view_state::RuntimeTaskControllerState,
    DesktopViewState,
) {
    let runtime_key = dedup_desktop_core::runtime_tasks::RuntimeTaskKey {
        owner: dedup_desktop_core::runtime_tasks::RuntimeTaskOwner::Node { node_index: 0 },
        id: "runtime-base-compute".into(),
    };
    let runtime = dedup_desktop_core::view_state::RuntimeTaskControllerState::from_parts_for_test(
        vec![dedup_desktop_core::runtime_tasks::RuntimeTaskSnapshot {
            key: runtime_key.clone(),
            machine_ids: vec!["machine-runtime".into()],
            kind: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskKind::Node,
            title: "基础计算".into(),
            state: dedup_desktop_core::runtime_tasks::DesktopRuntimeTaskState::Running,
            overall_completed: 2,
            overall_total: Some(10),
            overall_failed: 0,
            overall_skipped: 0,
            stages: Vec::new(),
            failures: Vec::new(),
        }],
        Some(runtime_key),
        None,
        false,
        None,
    );

    let mut view = DesktopViewState::new(
        DesktopConfig::default(),
        DesktopPaths {
            data: PathBuf::from(r"C:\fixture\desktop"),
            logs: PathBuf::from(r"C:\fixture\desktop\logs"),
            cache: PathBuf::from(r"C:\fixture\desktop\cache"),
            config: PathBuf::from(r"C:\fixture\desktop\config.toml"),
        },
    );
    view.upsert_task(TaskView {
        task_id: "legacy-base-compute".into(),
        node_index: 0,
        title: "base_compute".into(),
        stage: "节点任务".into(),
        state: ViewTaskState::Completed,
        completed_items: 10,
        total_items: 10,
        failed_items: 0,
        skipped_incomplete: 0,
    });
    (runtime, view)
}

/// 运行任务快照一旦发布，普通 ViewChanged 不能夺走列表、身份、标题或计数所有权。
#[test]
fn runtime_tasks_are_stable_when_view_events_arrive_in_both_orders() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, _receiver) = mpsc::channel(8);
    let binding = bind_commands(&window, sender, DesktopConfig::default());
    let (runtime, view) = single_owner_task_fixture();

    apply_event(
        &window,
        &binding,
        UiEvent::RuntimeTasksChanged(runtime.clone()),
    );
    for _round in 0..3 {
        let row = window.get_tasks().row_data(0).expect("应保留运行任务");
        assert_eq!(row.owner_kind, "node");
        assert_eq!(row.runtime_id, "runtime-base-compute");
        assert_eq!(row.title, "基础计算");
        assert_eq!(window.get_running_count(), 1);

        apply_event(
            &window,
            &binding,
            UiEvent::ViewChanged(Box::new(view.clone())),
        );
        let row = window
            .get_tasks()
            .row_data(0)
            .expect("ViewChanged 后仍应保留运行任务");
        assert_eq!(row.owner_kind, "node");
        assert_eq!(row.runtime_id, "runtime-base-compute");
        assert_eq!(row.title, "基础计算");
        assert_eq!(window.get_running_count(), 1);

        apply_event(
            &window,
            &binding,
            UiEvent::RuntimeTasksChanged(runtime.clone()),
        );
    }

    // 反向顺序也重复验证：普通视图先到达时不能清空已发布的统一运行任务。
    for _round in 0..3 {
        apply_event(
            &window,
            &binding,
            UiEvent::ViewChanged(Box::new(view.clone())),
        );
        let row = window
            .get_tasks()
            .row_data(0)
            .expect("反向事件后仍应保留运行任务");
        assert_eq!(row.owner_kind, "node");
        assert_eq!(row.runtime_id, "runtime-base-compute");
        assert_eq!(row.title, "基础计算");
        assert_eq!(window.get_running_count(), 1);

        apply_event(
            &window,
            &binding,
            UiEvent::RuntimeTasksChanged(runtime.clone()),
        );
    }
}

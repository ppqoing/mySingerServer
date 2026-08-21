use std::{io::Cursor, sync::Arc};

use dedup_core::{DeleteMode, DesktopConfig, EnumeratorKind};
use dedup_desktop_core::{
    app::{UiCommand, UiEvent},
    results::GroupKind,
    review::{QuickReviewRule, ReviewDecision},
};
use dedup_desktop_ui::{MainWindow, apply_event, bind_commands};
use tokio::sync::mpsc;

fn next(receiver: &mut mpsc::Receiver<UiCommand>) -> UiCommand {
    receiver.try_recv().expect("回调应把命令发送到真实 channel")
}

#[test]
fn file_faults_callbacks_page_and_clear_the_selected_online_node_only() {
    i_slint_backend_testing::init_no_event_loop();
    let accessible = |window: &MainWindow, label: &str| {
        i_slint_backend_testing::ElementHandle::find_by_accessible_label(window, label)
            .next()
            .unwrap_or_else(|| panic!("应找到可访问元素：{label}"))
    };

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
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
    state.set_node_identity(0, "machine-online");
    state.set_node_connection(
        0,
        dedup_desktop_core::view_state::NodeConnectionState::Online,
        None,
    );
    let offline = state.add_node("10.0.0.9", 39091).unwrap();
    state.set_node_identity(offline, "machine-offline");
    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));
    window.invoke_navigate_to(6);
    accessible(&window, "日志与诊断").invoke_accessible_default_action();
    accessible(&window, "诊断内容滚动区").scroll(0.0, -10000.0);
    slint::platform::update_timers_and_animations();

    window.invoke_select_file_fault_node(0);
    accessible(&window, "加载文件故障").invoke_accessible_default_action();
    assert!(matches!(
        next(&mut receiver),
        UiCommand::LoadFileFaults { node_index: 0, ref cursor } if cursor.is_empty()
    ));

    apply_event(
        &window,
        &binding,
        UiEvent::FileFaultsChanged(
            dedup_desktop_core::view_state::FileFaultDiagnosticsState {
                selected_node_index: Some(0),
                rows: vec![dedup_desktop_core::view_state::FileFaultView {
                    machine_id: "machine-online".into(),
                    normalized_path: r"d:\media\broken.mp4".into(),
                    display_path: r"D:\Media\broken.mp4".into(),
                    file_size: 4096,
                    fault_kind: "suspected_physical_read".into(),
                    stage: "read".into(),
                    error_code: Some(23),
                    message: "读取块重试耗尽".into(),
                }],
                next_cursor: "next-page".into(),
                cleanup_summary: Some(
                    dedup_desktop_core::view_state::DiskFullCleanupSummaryView {
                        triggered_at_unix_ms: 1234,
                        deleted_files: 3,
                        deleted_bytes: 8192,
                        skipped_active: 1,
                        skipped_other_disk: 2,
                        failed_files: 0,
                    },
                ),
                loading: false,
                error: None,
            },
        ),
    );
    assert_eq!(slint::Model::row_count(&window.get_file_fault_rows()), 1);
    accessible(&window, "加载下一页").invoke_accessible_default_action();
    assert!(matches!(
        next(&mut receiver),
        UiCommand::LoadFileFaults { node_index: 0, ref cursor } if cursor == "next-page"
    ));
    window.invoke_clear_file_fault(0);
    assert!(matches!(
        next(&mut receiver),
        UiCommand::ClearFileFault {
            node_index: 0,
            ref machine_id,
            ref normalized_path,
            ref fault_kind,
        } if machine_id == "machine-online"
            && normalized_path == r"d:\media\broken.mp4"
            && fault_kind == "suspected_physical_read"
    ));

    window.invoke_select_file_fault_node(1);
    assert_eq!(slint::Model::row_count(&window.get_file_fault_rows()), 0);
    assert_eq!(window.get_file_fault_next_cursor(), "");
    window.invoke_load_file_faults(false);
    window.invoke_clear_file_fault(0);
    assert!(receiver.try_recv().is_err(), "离线节点诊断动作不得发送命令");
}

#[test]
fn root_callbacks_emit_their_ui_commands_and_reject_invalid_settings() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let (sender, mut receiver) = mpsc::channel(32);
    let _binding = bind_commands(&window, sender, DesktopConfig::default());

    window.invoke_start_scan(-9, "D:\\Evidence".into(), true, 1);
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
    window.invoke_start_scan(4, "E:\\Archive".into(), false, 7);
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
    window.set_postgres_url("postgres://dedup:secret@10.0.0.20:5432/media".into());
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
                Some("postgres://dedup:secret@10.0.0.20:5432/media")
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
    find("保存并重启").invoke_accessible_default_action();
    match next(&mut receiver) {
        UiCommand::SaveNodeConfigAndRestart { node_index, config } => {
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
    window.invoke_select_node_config(1);
    assert_eq!(window.get_node_config_selected_index(), 1);
    assert!(!window.get_node_config_loaded(), "切换节点必须清除远程快照");
    assert!(!window.get_node_config_dirty());
    assert_eq!(window.get_scan_root(), "", "切换节点必须清除旧扫描路径");
    assert_eq!(find("加载配置").accessible_enabled(), Some(false));
    assert_eq!(find("保存并重启").accessible_enabled(), Some(false));
    find("加载配置").invoke_accessible_default_action();
    find("保存并重启").invoke_accessible_default_action();
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
    apply_event(&window, &binding, UiEvent::ViewChanged(Box::new(state)));
    assert!(window.get_node_config_loaded());
    assert!(window.get_node_config_dirty());
    assert_eq!(window.get_node_config_listen_ip(), "198.51.100.23");
    assert_eq!(window.get_node_config_data_path(), "edited-data");
    assert_eq!(window.get_node_config_retries(), 9);

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
}

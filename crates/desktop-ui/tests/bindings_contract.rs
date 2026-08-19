use dedup_core::{DeleteMode, DesktopConfig, EnumeratorKind};
use dedup_desktop_core::{
    app::UiCommand,
    results::GroupKind,
    review::{QuickReviewRule, ReviewDecision},
};
use dedup_desktop_ui::{MainWindow, bind_commands};
use tokio::sync::mpsc;

fn next(receiver: &mut mpsc::Receiver<UiCommand>) -> UiCommand {
    receiver.try_recv().expect("回调应把命令发送到真实 channel")
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

//! Slint 回调到 `UiCommand` 的轻量绑定，以及 `UiEvent` 到窗口属性的整体替换。

use std::sync::{Arc, Mutex};

use dedup_core::{DeleteMode, DesktopConfig, EnumeratorKind, Thresholds};
use dedup_desktop_core::{
    app::{UiCommand, UiEvent},
    results::GroupKind,
    review::{QuickReviewRule, ReviewDecision},
    view_state::{NodeConnectionState, ViewTaskState},
};
use slint::{ComponentHandle, Image, Rgba8Pixel, SharedPixelBuffer, SharedString};
use tokio::sync::mpsc;

use crate::{MainWindow, models};

/// GUI 回调与事件应用共享的最新已验证配置。
///
/// Slint 回调只读取控件值、构造命令并 `try_send`；网络、数据库和文件写入仍由
/// `dedup-desktop-core::app` 的异步控制循环执行。
#[derive(Clone)]
pub struct UiBinding {
    config: Arc<Mutex<DesktopConfig>>,
}

/// 绑定主窗口全部任务 21 回调，并返回事件应用所需的共享配置快照。
pub fn bind_commands(
    window: &MainWindow,
    sender: mpsc::Sender<UiCommand>,
    initial: DesktopConfig,
) -> UiBinding {
    let binding = UiBinding {
        config: Arc::new(Mutex::new(initial)),
    };

    bind_simple(window, &sender);
    bind_results(window, &sender);
    let add_sender = sender.clone();
    let add_window = window.as_weak();
    window.on_add_node(move |ip, port| {
        send(
            &add_sender,
            UiCommand::AddNode {
                ip: ip.to_string(),
                port: port.try_into().unwrap_or(0),
            },
            &add_window,
        );
    });
    let edit_sender = sender.clone();
    let edit_window = window.as_weak();
    window.on_edit_node(move |index, ip, port| {
        send(
            &edit_sender,
            UiCommand::EditNode {
                index: index.max(0) as usize,
                ip: ip.to_string(),
                port: port.try_into().unwrap_or(0),
            },
            &edit_window,
        );
    });
    let remove_sender = sender.clone();
    let remove_window = window.as_weak();
    window.on_remove_node(move |index| {
        send(
            &remove_sender,
            UiCommand::RemoveNode {
                index: index.max(0) as usize,
            },
            &remove_window,
        );
    });
    let sync_sender = sender.clone();
    let sync_window = window.as_weak();
    window.on_sync_node(move |index| {
        send(
            &sync_sender,
            UiCommand::SyncNow {
                index: index.max(0) as usize,
            },
            &sync_window,
        );
    });
    let browse_sender = sender.clone();
    let browse_window = window.as_weak();
    window.on_browse_paths(move |index, path| {
        send(
            &browse_sender,
            UiCommand::BrowsePaths {
                node_index: index.max(0) as usize,
                parent_path: path.to_string(),
                cursor: String::new(),
            },
            &browse_window,
        );
    });
    let scan_sender = sender.clone();
    let scan_window = window.as_weak();
    window.on_start_scan(move |index, path, force, enumerator| {
        send(
            &scan_sender,
            UiCommand::CreateScan {
                node_index: index.max(0) as usize,
                roots: vec![path.to_string()],
                force_recalculate: force,
                enumerator: if enumerator == 1 {
                    EnumeratorKind::Everything
                } else {
                    EnumeratorKind::WindowsWalker
                },
            },
            &scan_window,
        );
    });
    let cancel_sender = sender.clone();
    let cancel_window = window.as_weak();
    window.on_cancel_task(move |index, task_id| {
        send(
            &cancel_sender,
            UiCommand::CancelTask {
                node_index: index.max(0) as usize,
                task_id: task_id.to_string(),
            },
            &cancel_window,
        );
    });

    let save_sender = sender;
    let save_window = window.as_weak();
    let save_config = Arc::clone(&binding.config);
    window.on_save_settings(move || {
        let Some(window) = save_window.upgrade() else {
            return;
        };
        match settings_from_window(&window, &save_config) {
            Ok(config) => send(
                &save_sender,
                UiCommand::SaveSettings(config),
                &window.as_weak(),
            ),
            Err(error) => window.set_last_error(error.into()),
        }
    });
    binding
}

/// 在 Slint 线程整体应用一个不可变 core 事件。
pub fn apply_event(window: &MainWindow, binding: &UiBinding, event: UiEvent) {
    match event {
        UiEvent::ViewChanged(state) => {
            *binding.config.lock().expect("UI 配置锁未中毒") = state.config().clone();
            window.set_nodes(models::nodes(&state));
            window.set_tasks(models::tasks(&state));
            window.set_online_count(
                state
                    .nodes()
                    .iter()
                    .filter(|node| node.connection == NodeConnectionState::Online)
                    .count() as i32,
            );
            window.set_running_count(
                state
                    .tasks()
                    .iter()
                    .filter(|task| {
                        matches!(task.state, ViewTaskState::Queued | ViewTaskState::Running)
                    })
                    .count() as i32,
            );
            let sync = state
                .nodes()
                .iter()
                .filter_map(|node| node.stats.as_ref())
                .fold((0_u64, 0_u64), |total, stats| {
                    (
                        total.0.saturating_add(stats.sync_high_seq),
                        total.1.saturating_add(stats.outbox_high_seq),
                    )
                });
            window.set_sync_text(format!("{} / {}", sync.0, sync.1).into());
            let filtering = state.filtering_availability();
            window.set_filtering_enabled(filtering.enabled);
            window.set_filtering_reason(filtering.reason.into());
            let (postgres, postgres_color) = models::postgres_health(&state);
            window.set_postgres_status(postgres);
            window.set_postgres_color(postgres_color);
            window.set_data_path(path(&state.paths().data));
            window.set_logs_path(path(&state.paths().logs));
            window.set_cache_path(path(&state.paths().cache));
            window.set_config_path(path(&state.paths().config));
            apply_settings(window, state.config());
            window.set_last_error(SharedString::default());
        }
        UiEvent::PathsChanged {
            parent_path,
            entries,
            next_cursor,
            ..
        } => {
            window.set_last_error(
                format!(
                    "路径 {}：{} 项{}",
                    if parent_path.is_empty() {
                        "盘符".to_owned()
                    } else {
                        parent_path
                    },
                    entries.len(),
                    if next_cursor.is_empty() {
                        ""
                    } else {
                        "（还有下一页）"
                    }
                )
                .into(),
            );
        }
        UiEvent::AnalysisStarted {
            central,
            run_id,
            status,
        } => {
            window.set_result_run_id(run_id.clone().into());
            window.set_result_source_index(i32::from(central));
            if central {
                window.set_cross_status(status.clone().into());
            }
            window.set_last_error(format!("分析已创建：{run_id} · {status}").into());
        }
        UiEvent::CrossAnalysisChanged(report) => {
            window.set_result_run_id(report.run_id.as_uuid().to_string().into());
            window.set_result_source_index(1);
            window.set_cross_status(report.status.as_str().into());
            window.set_cross_summary(
                format!(
                    "候选 {} · 未决 {} · 二筛任务 {} · 跳过不完整 {}",
                    report.candidate_count,
                    report.unresolved_candidates,
                    report.phase2_task_count,
                    report.skipped_incomplete
                )
                .into(),
            );
            window.set_last_error(SharedString::default());
        }
        UiEvent::GroupsChanged(page) => {
            window.set_groups(models::groups(&page));
            window.set_group_next_cursor(page.next_cursor.clone().unwrap_or_default().into());
            window.set_selected_group_id(SharedString::default());
            window.set_members(empty_members());
            window.set_member_next_cursor(SharedString::default());
            window.set_last_error(SharedString::default());
        }
        UiEvent::MembersChanged { group_id, page } => {
            window.set_selected_group_id(group_id.into());
            apply_members(window, &page);
            window.set_last_error(SharedString::default());
        }
        UiEvent::PreviewReady {
            machine_id,
            normalized_path,
            display_path,
            file_kind,
            bytes,
        } => match decode_preview(&bytes) {
            Ok((image, width, height)) => {
                window.set_preview_image(image);
                window.set_preview_info(
                    format!(
                        "{} · {}×{} · {} · {}",
                        if file_kind == "contact_sheet" {
                            "JPG 联系表"
                        } else {
                            "原图"
                        },
                        width,
                        height,
                        models::bytes(bytes.len() as u64),
                        display_path
                    )
                    .into(),
                );
                window.set_last_error(SharedString::default());
                finish_preview(window, &machine_id, &normalized_path, true);
            }
            Err(error) => {
                window.set_last_error(error.into());
                finish_preview(window, &machine_id, &normalized_path, false);
            }
        },
        UiEvent::PreviewFailed {
            machine_id,
            normalized_path,
            error,
        } => {
            window.set_last_error(error.into());
            finish_preview(window, &machine_id, &normalized_path, false);
        }
        UiEvent::ReviewChanged(page) => {
            apply_members(window, &page);
            window.set_last_error(SharedString::default());
        }
        UiEvent::DeleteConfirmationChanged(confirmation) => {
            window.set_delete_file_count(confirmation.file_count as i32);
            window.set_delete_node_count(confirmation.node_count as i32);
            window.set_delete_reclaimable(models::bytes(confirmation.reclaimable_bytes).into());
            window.set_delete_mode(
                if confirmation.mode == DeleteMode::Permanent {
                    "永久删除"
                } else {
                    "回收站"
                }
                .into(),
            );
            window.set_delete_can_execute(confirmation.can_execute);
            window.set_delete_warning(confirmation.warning.into());
            window.set_delete_dialog_open(true);
        }
        UiEvent::DeleteFinished(summary) => {
            window.set_delete_dialog_open(false);
            window.set_groups(empty_groups());
            window.set_members(empty_members());
            window.set_selected_group_id(SharedString::default());
            window.set_group_next_cursor(SharedString::default());
            window.set_member_next_cursor(SharedString::default());
            window.set_last_error(summary.into());
        }
        UiEvent::Error(error) => window.set_last_error(error.into()),
        UiEvent::ShutdownComplete => {
            let _ = window.hide();
        }
    }
}

fn bind_results(window: &MainWindow, sender: &mpsc::Sender<UiCommand>) {
    let analysis_sender = sender.clone();
    let analysis_window = window.as_weak();
    window.on_start_local_analysis(move |node_index, task_ids, kind| {
        send(
            &analysis_sender,
            UiCommand::StartLocalAnalysis {
                node_index: node_index.max(0) as usize,
                scan_task_ids: task_ids.to_string(),
                kind: group_kind(kind),
            },
            &analysis_window,
        );
    });

    let cross_sender = sender.clone();
    let cross_window = window.as_weak();
    window.on_start_cross_analysis(move |selections| {
        send(
            &cross_sender,
            UiCommand::StartCrossAnalysis {
                selections: selections.to_string(),
            },
            &cross_window,
        );
    });
    let poll_sender = sender.clone();
    let poll_window = window.as_weak();
    window.on_poll_cross_analysis(move || {
        send(&poll_sender, UiCommand::PollCrossAnalysis, &poll_window);
    });
    let retry_sender = sender.clone();
    let retry_window = window.as_weak();
    window.on_retry_cross_analysis(move || {
        send(&retry_sender, UiCommand::RetryCrossAnalysis, &retry_window);
    });

    let groups_sender = sender.clone();
    let groups_window = window.as_weak();
    window.on_load_groups(move |central, node_index, run_id, kind, cursor| {
        send(
            &groups_sender,
            UiCommand::LoadGroups {
                central,
                node_index: node_index.max(0) as usize,
                analysis_run_id: run_id.to_string(),
                kind: group_kind(kind),
                cursor: cursor.to_string(),
            },
            &groups_window,
        );
    });
    let members_sender = sender.clone();
    let members_window = window.as_weak();
    window.on_load_members(move |central, node_index, run_id, group_id, kind, cursor| {
        send(
            &members_sender,
            UiCommand::LoadMembers {
                central,
                node_index: node_index.max(0) as usize,
                analysis_run_id: run_id.to_string(),
                group_id: group_id.to_string(),
                kind: group_kind(kind),
                cursor: cursor.to_string(),
            },
            &members_window,
        );
    });

    let review_sender = sender.clone();
    let review_window = window.as_weak();
    window.on_save_review(move |machine_id, path, decision| {
        send(
            &review_sender,
            UiCommand::SaveReview {
                machine_id: machine_id.to_string(),
                normalized_path: path.to_string(),
                decision: review_decision(decision),
            },
            &review_window,
        );
    });
    let quick_sender = sender.clone();
    let quick_window = window.as_weak();
    window.on_quick_review(move |rule, value| {
        send(
            &quick_sender,
            UiCommand::ApplyQuickReview(quick_rule(rule, value.to_string())),
            &quick_window,
        );
    });
    let preview_sender = sender.clone();
    let preview_window = window.as_weak();
    window.on_load_preview(move |machine_id, path| {
        send(
            &preview_sender,
            UiCommand::LoadPreview {
                machine_id: machine_id.to_string(),
                normalized_path: path.to_string(),
            },
            &preview_window,
        );
    });

    let prepare_sender = sender.clone();
    let prepare_window = window.as_weak();
    window.on_prepare_delete(move || {
        send(&prepare_sender, UiCommand::PrepareDelete, &prepare_window);
    });
    let confirm_sender = sender.clone();
    let confirm_window = window.as_weak();
    window.on_confirm_delete(move || {
        send(&confirm_sender, UiCommand::ConfirmDelete, &confirm_window);
    });
}

fn apply_members(window: &MainWindow, page: &dedup_desktop_core::results::MemberPage) {
    window.set_members(models::members(page));
    window.set_member_next_cursor(page.next_cursor.clone().unwrap_or_default().into());
}

fn finish_preview(window: &MainWindow, machine_id: &str, normalized_path: &str, succeeded: bool) {
    window.set_preview_result_machine(machine_id.into());
    window.set_preview_result_path(normalized_path.into());
    window.set_preview_result_succeeded(succeeded);
    window.set_preview_result_sequence(window.get_preview_result_sequence().wrapping_add(1));
}

fn empty_groups() -> slint::ModelRc<crate::UiGroupRow> {
    slint::ModelRc::new(slint::VecModel::from(Vec::new()))
}

fn empty_members() -> slint::ModelRc<crate::UiMemberRow> {
    slint::ModelRc::new(slint::VecModel::from(Vec::new()))
}

fn decode_preview(bytes: &[u8]) -> Result<(Image, u32, u32), String> {
    let decoded = image::load_from_memory(bytes)
        .map_err(|error| format!("预览格式无法解码：{error}"))?
        .into_rgba8();
    let (width, height) = decoded.dimensions();
    let buffer = SharedPixelBuffer::<Rgba8Pixel>::clone_from_slice(decoded.as_raw(), width, height);
    Ok((Image::from_rgba8(buffer), width, height))
}

fn group_kind(value: i32) -> GroupKind {
    match value {
        1 => GroupKind::SimilarImage,
        2 => GroupKind::SimilarVideo,
        _ => GroupKind::Exact,
    }
}

fn review_decision(value: i32) -> ReviewDecision {
    match value {
        1 => ReviewDecision::Keep,
        2 => ReviewDecision::Delete,
        _ => ReviewDecision::Undecided,
    }
}

fn quick_rule(value: i32, path: String) -> QuickReviewRule {
    match value {
        1 => QuickReviewRule::HighestResolution,
        2 => QuickReviewRule::HighestQuality,
        3 => QuickReviewRule::PathContains(path),
        _ => QuickReviewRule::LargestFile,
    }
}

fn bind_simple(window: &MainWindow, sender: &mpsc::Sender<UiCommand>) {
    let connect_sender = sender.clone();
    let connect_window = window.as_weak();
    window.on_connect_all(move || {
        send(&connect_sender, UiCommand::ConnectAll, &connect_window);
    });
    let refresh_sender = sender.clone();
    let refresh_window = window.as_weak();
    window.on_refresh(move || {
        send(&refresh_sender, UiCommand::Refresh, &refresh_window);
    });
}

fn send(sender: &mpsc::Sender<UiCommand>, command: UiCommand, window: &slint::Weak<MainWindow>) {
    if let Err(error) = sender.try_send(command)
        && let Some(window) = window.upgrade()
    {
        window.set_last_error(format!("命令队列不可用：{error}").into());
    }
}

fn settings_from_window(
    window: &MainWindow,
    shared: &Arc<Mutex<DesktopConfig>>,
) -> Result<DesktopConfig, String> {
    let mut config = shared.lock().map_err(|_| "配置锁不可用")?.clone();
    let postgres = window.get_postgres_url().trim().to_owned();
    config.postgres_url = (!postgres.is_empty()).then_some(postgres);
    config.reconnect_interval_seconds = window
        .get_reconnect_seconds()
        .try_into()
        .map_err(|_| "重连间隔无效")?;
    config.delete_mode = if window.get_delete_mode_index() == 1 {
        DeleteMode::Permanent
    } else {
        DeleteMode::RecycleBin
    };
    config.thresholds = Thresholds {
        pdq_quality_min: parse(window.get_pdq_quality(), "PDQ Quality")?,
        aspect_tolerance: parse(window.get_aspect_tolerance(), "长宽比容差")?,
        pdq_hamming_max: parse(window.get_pdq_hamming(), "PDQ 汉明")?,
        phash_part_hamming_max: parse(window.get_phash_hamming(), "pHash 汉明")?,
        phash_min_passed_parts: parse(window.get_phash_parts(), "pHash 通过块")?,
        sobel_min: parse(window.get_sobel_min(), "Sobel 阈值")?,
        video_min_valid_frames: parse(window.get_video_valid(), "视频有效帧")?,
        video_stage1_min: parse(window.get_video_stage1(), "视频一筛")?,
        video_stage2_min: parse(window.get_video_stage2(), "视频二筛")?,
    };
    config.validate().map_err(|error| error.to_string())?;
    Ok(config)
}

fn apply_settings(window: &MainWindow, config: &DesktopConfig) {
    window.set_postgres_url(config.postgres_url.clone().unwrap_or_default().into());
    window.set_reconnect_seconds(config.reconnect_interval_seconds as i32);
    window.set_delete_mode_index(i32::from(config.delete_mode == DeleteMode::Permanent));
    window.set_pdq_quality(config.thresholds.pdq_quality_min.to_string().into());
    window.set_aspect_tolerance(format!("{:.2}", config.thresholds.aspect_tolerance).into());
    window.set_pdq_hamming(config.thresholds.pdq_hamming_max.to_string().into());
    window.set_phash_hamming(config.thresholds.phash_part_hamming_max.to_string().into());
    window.set_phash_parts(config.thresholds.phash_min_passed_parts.to_string().into());
    window.set_sobel_min(format!("{:.2}", config.thresholds.sobel_min).into());
    window.set_video_valid(config.thresholds.video_min_valid_frames.to_string().into());
    window.set_video_stage1(format!("{:.2}", config.thresholds.video_stage1_min).into());
    window.set_video_stage2(format!("{:.2}", config.thresholds.video_stage2_min).into());
}

fn parse<T>(value: SharedString, field: &str) -> Result<T, String>
where
    T: std::str::FromStr,
{
    value
        .trim()
        .parse()
        .map_err(|_| format!("{field} 不是有效数值"))
}

fn path(value: &std::path::Path) -> SharedString {
    value.to_string_lossy().into_owned().into()
}

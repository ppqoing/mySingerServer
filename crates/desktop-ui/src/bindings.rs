//! Slint 回调到 `UiCommand` 的轻量绑定，以及 `UiEvent` 到窗口属性的整体替换。

use std::sync::{Arc, Mutex};

use dedup_core::{DeleteMode, DesktopConfig, EnumeratorKind, Thresholds};
use dedup_desktop_core::{
    app::{UiCommand, UiEvent},
    view_state::{NodeConnectionState, ViewTaskState},
};
use slint::{ComponentHandle, SharedString};
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

/// 绑定主窗口全部任务 17 回调，并返回事件应用所需的共享配置快照。
pub fn bind_commands(
    window: &MainWindow,
    sender: mpsc::Sender<UiCommand>,
    initial: DesktopConfig,
) -> UiBinding {
    let binding = UiBinding {
        config: Arc::new(Mutex::new(initial)),
    };

    bind_simple(window, &sender);
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
        UiEvent::Error(error) => window.set_last_error(error.into()),
        UiEvent::ShutdownComplete => {
            let _ = window.hide();
        }
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

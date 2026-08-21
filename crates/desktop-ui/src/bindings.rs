//! Slint 回调到 `UiCommand` 的轻量绑定，以及 `UiEvent` 到窗口属性的整体替换。

use std::sync::{Arc, Mutex};

use dedup_core::{
    DeleteMode, DesktopConfig, DiskReadConfig, EnumeratorKind, NodeConfig, NodePathsConfig,
    Thresholds, WorkerConfig, WorkerMode,
};
use dedup_desktop_core::{
    app::{UiCommand, UiEvent},
    results::GroupKind,
    review::{QuickReviewRule, ReviewDecision},
    runtime_tasks::{RuntimeTaskKey, RuntimeTaskOwner},
    view_state::{FileFaultDiagnosticsState, NodeConnectionState, ViewTaskState},
};
use slint::{
    ComponentHandle, Image, Model, ModelRc, Rgba8Pixel, SharedPixelBuffer, SharedString, VecModel,
};
use tokio::sync::mpsc;

use crate::{MainWindow, UiFileFaultRow, models};

/// GUI 回调与事件应用共享的最新已验证配置。
///
/// Slint 回调只读取控件值、构造命令并 `try_send`；网络、数据库和文件写入仍由
/// `dedup-desktop-core::app` 的异步控制循环执行。
#[derive(Clone)]
pub struct UiBinding {
    config: Arc<Mutex<DesktopConfig>>,
    node_config: Arc<Mutex<Option<LoadedNodeConfig>>>,
    selected_node: Arc<Mutex<NodeConfigSelection>>,
}

#[derive(Clone, Debug, PartialEq)]
struct LoadedNodeConfig {
    config: NodeConfig,
    machine_id: String,
    version_sha256: String,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
struct NodeConfigSelection {
    index: i32,
    machine_id: String,
}

/// 绑定主窗口全部任务 21 回调，并返回事件应用所需的共享配置快照。
pub fn bind_commands(
    window: &MainWindow,
    sender: mpsc::Sender<UiCommand>,
    initial: DesktopConfig,
) -> UiBinding {
    let binding = UiBinding {
        config: Arc::new(Mutex::new(initial)),
        node_config: Arc::new(Mutex::new(None)),
        selected_node: Arc::new(Mutex::new(NodeConfigSelection {
            index: window.get_node_config_selected_index().max(0),
            machine_id: String::new(),
        })),
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
    let runtime_sender = sender.clone();
    let runtime_window = window.as_weak();
    window.on_select_runtime_task(move |owner_kind, node_index, runtime_id| {
        let owner = match owner_kind.as_str() {
            "desktop" => RuntimeTaskOwner::Desktop,
            "node" => RuntimeTaskOwner::Node {
                node_index: node_index.max(0) as usize,
            },
            _ => {
                if let Some(window) = runtime_window.upgrade() {
                    window.set_last_error("运行任务归属无效".into());
                }
                return;
            }
        };
        send(
            &runtime_sender,
            UiCommand::SelectRuntimeTask {
                key: RuntimeTaskKey {
                    owner,
                    id: runtime_id.to_string(),
                },
            },
            &runtime_window,
        );
    });

    bind_node_config(
        window,
        &sender,
        &binding.node_config,
        &binding.selected_node,
    );
    bind_file_faults(window, &sender);

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

fn bind_node_config(
    window: &MainWindow,
    sender: &mpsc::Sender<UiCommand>,
    loaded: &Arc<Mutex<Option<LoadedNodeConfig>>>,
    selected: &Arc<Mutex<NodeConfigSelection>>,
) {
    let select_window = window.as_weak();
    let select_loaded = Arc::clone(loaded);
    let selected_for_callback = Arc::clone(selected);
    window.on_select_node_config(move |index| {
        let Some(window) = select_window.upgrade() else {
            return;
        };
        let index = index.max(0);
        window.set_node_config_selected_index(index);
        window.set_node_config_node_online(selected_node_online(&window, index));
        let changed = update_node_config_selection(&window, index, &selected_for_callback);
        if changed {
            clear_node_config_context(&window, &select_loaded);
        }
    });

    let load_sender = sender.clone();
    let load_window = window.as_weak();
    let load_loaded = Arc::clone(loaded);
    window.on_load_node_config(move || {
        let Some(window) = load_window.upgrade() else {
            return;
        };
        if !window.get_node_config_node_online() || window.get_node_config_saving() {
            return;
        }
        clear_node_config_context(&window, &load_loaded);
        send(
            &load_sender,
            UiCommand::LoadNodeConfig {
                node_index: window.get_node_config_selected_index().max(0) as usize,
            },
            &window.as_weak(),
        );
    });

    let edit_window = window.as_weak();
    let edit_loaded = Arc::clone(loaded);
    window.on_node_config_edited(move || {
        let Some(window) = edit_window.upgrade() else {
            return;
        };
        let is_dirty = match node_config_from_window(&window) {
            Ok(current) => {
                edit_loaded
                    .lock()
                    .expect("Node 配置锁未中毒")
                    .as_ref()
                    .map(|loaded| &loaded.config)
                    != Some(&current)
            }
            Err(_) => true,
        };
        window.set_node_config_dirty(is_dirty);
    });

    let save_sender = sender.clone();
    let save_window = window.as_weak();
    let save_loaded = Arc::clone(loaded);
    window.on_save_node_config_and_restart(move || {
        let Some(window) = save_window.upgrade() else {
            return;
        };
        if !window.get_node_config_node_online()
            || !window.get_node_config_loaded()
            || window.get_node_config_saving()
        {
            return;
        }
        let config = match node_config_from_window(&window) {
            Ok(config) => config,
            Err(error) => {
                window.set_last_error(error.into());
                return;
            }
        };
        if save_loaded
            .lock()
            .expect("Node 配置锁未中毒")
            .as_ref()
            .map(|loaded| &loaded.config)
            == Some(&config)
        {
            window.set_node_config_dirty(false);
            return;
        }
        let wire = match (&config).try_into() {
            Ok(wire) => wire,
            Err(error) => {
                window.set_last_error(format!("Node 配置无效：{error}").into());
                return;
            }
        };
        send(
            &save_sender,
            UiCommand::SaveNodeConfigAndRestart {
                node_index: window.get_node_config_selected_index().max(0) as usize,
                config: wire,
            },
            &window.as_weak(),
        );
    });
}

fn bind_file_faults(window: &MainWindow, sender: &mpsc::Sender<UiCommand>) {
    let select_sender = sender.clone();
    let select_window = window.as_weak();
    window.on_select_file_fault_node(move |index| {
        let Some(window) = select_window.upgrade() else {
            return;
        };
        let index = index.max(0);
        let machine_id = slint::Model::row_data(&window.get_nodes(), index as usize)
            .map(|node| node.machine_id.to_string())
            .filter(|machine_id| machine_id != "尚未握手")
            .unwrap_or_default();
        window.set_file_fault_selected_node(index);
        window.set_file_fault_node_online(selected_node_online(&window, index));
        window.set_file_fault_rows(ModelRc::new(VecModel::from(
            Vec::<UiFileFaultRow>::new(),
        )));
        window.set_file_fault_next_cursor(SharedString::default());
        window.set_file_fault_error(SharedString::default());
        window.set_disk_cleanup_summary("尚无磁盘满清理记录".into());
        send(
            &select_sender,
            UiCommand::SelectFileFaultNode {
                node_index: index as usize,
                machine_id,
            },
            &window.as_weak(),
        );
    });

    let load_sender = sender.clone();
    let load_window = window.as_weak();
    window.on_load_file_faults(move |next_page| {
        let Some(window) = load_window.upgrade() else {
            return;
        };
        if !window.get_file_fault_node_online() || window.get_file_fault_loading() {
            return;
        }
        send(
            &load_sender,
            UiCommand::LoadFileFaults {
                node_index: window.get_file_fault_selected_node().max(0) as usize,
                cursor: if next_page {
                    window.get_file_fault_next_cursor().to_string()
                } else {
                    String::new()
                },
            },
            &window.as_weak(),
        );
    });

    let clear_sender = sender.clone();
    let clear_window = window.as_weak();
    window.on_clear_file_fault(move |index| {
        let Some(window) = clear_window.upgrade() else {
            return;
        };
        if !window.get_file_fault_node_online() || window.get_file_fault_loading() {
            return;
        }
        let Some(row) = window
            .get_file_fault_rows()
            .row_data(index.max(0) as usize)
        else {
            return;
        };
        let fault_kind = match row.fault_kind {
            1 => "suspected_physical_read",
            2 => "worker_crash",
            _ => return,
        };
        send(
            &clear_sender,
            UiCommand::ClearFileFault {
                node_index: window.get_file_fault_selected_node().max(0) as usize,
                machine_id: row.machine_id.to_string(),
                normalized_path: row.normalized_path.to_string(),
                fault_kind: fault_kind.into(),
            },
            &window.as_weak(),
        );
    });
}

fn update_node_config_selection(
    window: &MainWindow,
    index: i32,
    selected: &Arc<Mutex<NodeConfigSelection>>,
) -> bool {
    let next = node_config_selection(window, index);
    let mut previous = selected.lock().expect("Node 选择锁未中毒");
    let index_changed = previous.index != next.index;
    let machine_changed = previous.index == next.index
        && !previous.machine_id.is_empty()
        && !next.machine_id.is_empty()
        && previous.machine_id != next.machine_id;
    *previous = next;
    index_changed || machine_changed
}

fn node_config_selection(window: &MainWindow, index: i32) -> NodeConfigSelection {
    let machine_id = slint::Model::row_data(&window.get_nodes(), index.max(0) as usize)
        .map(|node| node.machine_id.to_string())
        .filter(|machine_id| machine_id != "尚未握手")
        .unwrap_or_default();
    NodeConfigSelection {
        index: index.max(0),
        machine_id,
    }
}

fn selected_node_online(window: &MainWindow, index: i32) -> bool {
    slint::Model::row_data(&window.get_nodes(), index.max(0) as usize)
        .is_some_and(|node| node.status == "在线" && node.machine_id != "尚未握手")
}

fn clear_node_config_form(window: &MainWindow) {
    window.set_node_config_loaded(false);
    window.set_node_config_dirty(false);
    window.set_node_config_saving(false);
    window.set_node_config_machine_id(SharedString::default());
    window.set_node_config_version(SharedString::default());
    window.set_node_config_phase("未加载".into());
    window.set_node_config_error(SharedString::default());
    window.set_node_config_listen_ip(SharedString::default());
    window.set_node_config_port(39091);
    window.set_node_config_enumerator_index(1);
    window.set_node_config_data_path(SharedString::default());
    window.set_node_config_config_path(SharedString::default());
    window.set_node_config_log_path(SharedString::default());
    window.set_node_config_cache_path(SharedString::default());
    window.set_node_config_hdd_threads(1);
    window.set_node_config_ssd_threads(2);
    window.set_node_config_unknown_threads(1);
    window.set_node_config_total_threads(4);
    window.set_node_config_block_size(4 * 1024 * 1024);
    window.set_node_config_timeout_seconds(3);
    window.set_node_config_retries(2);
    window.set_node_config_legacy_workers(1);
    window.set_node_config_worker_mode_index(0);
    window.set_node_config_reserved_cores(1);
    window.set_node_config_manual_workers(1);
    window.set_node_config_logical_cpus(0);
    window.set_node_config_effective_workers(0);
    window.set_scan_root(SharedString::default());
}

fn clear_node_config_context(window: &MainWindow, loaded: &Arc<Mutex<Option<LoadedNodeConfig>>>) {
    clear_node_config_form(window);
    *loaded.lock().expect("Node 配置锁未中毒") = None;
}

fn apply_file_fault_state(window: &MainWindow, state: &FileFaultDiagnosticsState) {
    if let Some(index) = state.selected_node_index {
        window.set_file_fault_selected_node(index as i32);
        window.set_file_fault_node_online(selected_node_online(window, index as i32));
    }
    if state.rows.iter().any(|fault| {
        !matches!(
            fault.fault_kind.as_str(),
            "suspected_physical_read" | "worker_crash"
        )
    }) {
        window.set_file_fault_rows(ModelRc::new(VecModel::from(
            Vec::<UiFileFaultRow>::new(),
        )));
        window.set_file_fault_next_cursor(SharedString::default());
        window.set_file_fault_loading(false);
        window.set_file_fault_error("未知文件故障类别，响应已拒绝".into());
        window.set_disk_cleanup_summary("尚无磁盘满清理记录".into());
        return;
    }
    let rows = state
        .rows
        .iter()
        .filter_map(|fault| {
            let (fault_kind, fault_kind_text) = match fault.fault_kind.as_str() {
                "suspected_physical_read" => (1, "疑似物理读取故障"),
                "worker_crash" => (2, "Worker 崩溃"),
                _ => return None,
            };
            Some(UiFileFaultRow {
                machine_id: fault.machine_id.clone().into(),
                normalized_path: fault.normalized_path.clone().into(),
                display_path: fault.display_path.clone().into(),
                file_size: models::bytes(fault.file_size).into(),
                fault_kind,
                fault_kind_text: fault_kind_text.into(),
                stage: fault.stage.clone().into(),
                error_code: fault
                    .error_code
                    .map_or_else(|| "—".into(), |code| code.to_string().into()),
                message: fault.message.clone().into(),
            })
        })
        .collect::<Vec<_>>();
    window.set_file_fault_rows(ModelRc::new(VecModel::from(rows)));
    window.set_file_fault_next_cursor(state.next_cursor.clone().into());
    window.set_file_fault_loading(state.loading);
    window.set_file_fault_error(state.error.clone().unwrap_or_default().into());
    window.set_disk_cleanup_summary(
        state.cleanup_summary.as_ref().map_or_else(
            || "尚无磁盘满清理记录".to_owned(),
            |summary| {
                format!(
                    "最近磁盘满清理：触发 {} ms · 删除 {} 个 / {} · 活动跳过 {} · 异盘跳过 {} · 失败 {}",
                    summary.triggered_at_unix_ms,
                    summary.deleted_files,
                    models::bytes(summary.deleted_bytes),
                    summary.skipped_active,
                    summary.skipped_other_disk,
                    summary.failed_files,
                )
            },
        )
        .into(),
    );
}

fn node_config_from_window(window: &MainWindow) -> Result<NodeConfig, String> {
    let listen_ip = window
        .get_node_config_listen_ip()
        .trim()
        .parse()
        .map_err(|_| "Node 监听 IP 无效".to_owned())?;
    let port = window
        .get_node_config_port()
        .try_into()
        .map_err(|_| "Node 监听端口无效".to_owned())?;
    let config = NodeConfig {
        listen_ip,
        port,
        worker_count: positive_usize(window.get_node_config_legacy_workers(), "兼容 Worker 数量")?,
        enumerator: if window.get_node_config_enumerator_index() == 0 {
            EnumeratorKind::WindowsWalker
        } else {
            EnumeratorKind::Everything
        },
        paths: NodePathsConfig {
            data_path: window.get_node_config_data_path().to_string(),
            config_path: window.get_node_config_config_path().to_string(),
            log_path: window.get_node_config_log_path().to_string(),
            cache_path: window.get_node_config_cache_path().to_string(),
        },
        read: DiskReadConfig {
            hdd_threads_per_disk: positive_usize(
                window.get_node_config_hdd_threads(),
                "机械硬盘每盘读取线程",
            )?,
            ssd_threads_per_disk: positive_usize(
                window.get_node_config_ssd_threads(),
                "固态硬盘每盘读取线程",
            )?,
            unknown_threads_per_disk: positive_usize(
                window.get_node_config_unknown_threads(),
                "未知磁盘每盘读取线程",
            )?,
            total_threads: positive_usize(window.get_node_config_total_threads(), "总读取线程")?,
            block_size_bytes: positive_usize(window.get_node_config_block_size(), "读取块大小")?,
            block_timeout_seconds: window
                .get_node_config_timeout_seconds()
                .try_into()
                .map_err(|_| "单块读取超时无效".to_owned())?,
            block_retries: window
                .get_node_config_retries()
                .try_into()
                .map_err(|_| "读取重试次数无效".to_owned())?,
        },
        worker: WorkerConfig {
            mode: if window.get_node_config_worker_mode_index() == 0 {
                WorkerMode::Automatic
            } else {
                WorkerMode::Manual
            },
            reserved_cores: window
                .get_node_config_reserved_cores()
                .try_into()
                .map_err(|_| "自动模式保留核心无效".to_owned())?,
            manual_worker_count: positive_usize(
                window.get_node_config_manual_workers(),
                "手动 Worker 数量",
            )?,
        },
    };
    config.validate().map_err(|error| error.to_string())?;
    Ok(config)
}

fn positive_usize(value: i32, field: &str) -> Result<usize, String> {
    value.try_into().map_err(|_| format!("{field} 无效"))
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
            window.set_node_config_options(models::node_config_options(&state));
            let selected = state
                .node_config()
                .selected_node_index()
                .unwrap_or_else(|| window.get_node_config_selected_index().max(0) as usize);
            window.set_node_config_selected_index(selected as i32);
            window.set_node_config_node_online(state.nodes().get(selected).is_some_and(|node| {
                node.connection == NodeConnectionState::Online && node.machine_id.is_some()
            }));
            if update_node_config_selection(window, selected as i32, &binding.selected_node) {
                clear_node_config_context(window, &binding.node_config);
            }
            let file_fault_selected = state
                .file_faults()
                .selected_node_index
                .unwrap_or_else(|| window.get_file_fault_selected_node().max(0) as usize);
            window.set_file_fault_selected_node(file_fault_selected as i32);
            window.set_file_fault_node_online(
                state
                    .nodes()
                    .get(file_fault_selected)
                    .is_some_and(|node| {
                        node.connection == NodeConnectionState::Online
                            && node.machine_id.is_some()
                    }),
            );
            apply_file_fault_state(window, state.file_faults());
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
        UiEvent::FileFaultsChanged(state) => apply_file_fault_state(window, &state),
        UiEvent::RuntimeTasksChanged(state) => {
            let runtime = models::runtime_tasks(&state);
            window.set_tasks(runtime.tasks);
            window.set_runtime_stages(runtime.stages);
            window.set_runtime_workers(runtime.workers);
            window.set_runtime_failures(runtime.failures);
            window.set_runtime_detail_title(runtime.title);
            window.set_runtime_detail_machine_id(runtime.machine_id);
            window.set_runtime_detail_state(runtime.state);
            window.set_runtime_detail_counts(runtime.counts);
            window.set_runtime_detail_stale(runtime.stale);
            window.set_runtime_detail_error(runtime.error);
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
        UiEvent::NodeConfigChanged(state) => apply_node_config_state(window, binding, &state),
        UiEvent::Error(error) => window.set_last_error(error.into()),
        UiEvent::ShutdownComplete => {
            let _ = window.hide();
        }
    }
}

fn apply_node_config_state(
    window: &MainWindow,
    binding: &UiBinding,
    state: &dedup_desktop_core::view_state::NodeConfigControllerState,
) {
    if let Some(index) = state.selected_node_index() {
        window.set_node_config_selected_index(index as i32);
        window.set_node_config_node_online(selected_node_online(window, index as i32));
        if update_node_config_selection(window, index as i32, &binding.selected_node) {
            clear_node_config_context(window, &binding.node_config);
        }
    }
    window.set_node_config_saving(state.is_in_progress());
    window.set_node_config_phase(node_config_phase(state.phase()).into());
    window.set_node_config_error(state.error().unwrap_or_default().into());
    if let Some(error) = state.error() {
        window.set_last_error(error.into());
    }

    let Some(snapshot) = state.snapshot() else {
        window.set_node_config_phase(
            if !window.get_node_config_loaded()
                && state.phase() == dedup_desktop_core::view_state::NodeConfigSavePhase::Idle
            {
                "未加载"
            } else {
                node_config_phase(state.phase())
            }
            .into(),
        );
        window.set_node_config_error(state.error().unwrap_or_default().into());
        return;
    };
    let snapshot_unchanged = binding
        .node_config
        .lock()
        .expect("Node 配置锁未中毒")
        .as_ref()
        .is_some_and(|loaded| {
            loaded.machine_id == snapshot.machine_id
                && loaded.version_sha256 == snapshot.version_sha256
        });
    if snapshot_unchanged {
        window.set_node_config_logical_cpus(snapshot.logical_cpu_count as i32);
        window.set_node_config_effective_workers(snapshot.effective_worker_count as i32);
        return;
    }
    let Some(wire) = snapshot.config.as_ref() else {
        clear_node_config_form(window);
        window.set_node_config_error("Node 配置响应缺少 config".into());
        window.set_last_error("Node 配置响应缺少 config".into());
        *binding.node_config.lock().expect("Node 配置锁未中毒") = None;
        return;
    };
    let config = match NodeConfig::try_from(wire.clone()) {
        Ok(config) => config,
        Err(error) => {
            clear_node_config_form(window);
            window.set_node_config_error(format!("Node 配置无效：{error}").into());
            window.set_last_error(format!("Node 配置无效：{error}").into());
            *binding.node_config.lock().expect("Node 配置锁未中毒") = None;
            return;
        }
    };
    apply_node_config(window, &config);
    window.set_node_config_loaded(true);
    window.set_node_config_dirty(false);
    window.set_node_config_saving(state.is_in_progress());
    window.set_node_config_machine_id(snapshot.machine_id.clone().into());
    window.set_node_config_version(snapshot.version_sha256.clone().into());
    window.set_node_config_logical_cpus(snapshot.logical_cpu_count as i32);
    window.set_node_config_effective_workers(snapshot.effective_worker_count as i32);
    window.set_node_config_phase(node_config_phase(state.phase()).into());
    window.set_node_config_error(state.error().unwrap_or_default().into());
    *binding.node_config.lock().expect("Node 配置锁未中毒") = Some(LoadedNodeConfig {
        config,
        machine_id: snapshot.machine_id.clone(),
        version_sha256: snapshot.version_sha256.clone(),
    });
}

fn apply_node_config(window: &MainWindow, config: &NodeConfig) {
    window.set_node_config_listen_ip(config.listen_ip.to_string().into());
    window.set_node_config_port(i32::from(config.port));
    window.set_node_config_enumerator_index(i32::from(
        config.enumerator == EnumeratorKind::Everything,
    ));
    window.set_node_config_data_path(config.paths.data_path.clone().into());
    window.set_node_config_config_path(config.paths.config_path.clone().into());
    window.set_node_config_log_path(config.paths.log_path.clone().into());
    window.set_node_config_cache_path(config.paths.cache_path.clone().into());
    window.set_node_config_hdd_threads(config.read.hdd_threads_per_disk as i32);
    window.set_node_config_ssd_threads(config.read.ssd_threads_per_disk as i32);
    window.set_node_config_unknown_threads(config.read.unknown_threads_per_disk as i32);
    window.set_node_config_total_threads(config.read.total_threads as i32);
    window.set_node_config_block_size(config.read.block_size_bytes as i32);
    window.set_node_config_timeout_seconds(config.read.block_timeout_seconds as i32);
    window.set_node_config_retries(config.read.block_retries as i32);
    window.set_node_config_legacy_workers(config.worker_count as i32);
    window.set_node_config_worker_mode_index(i32::from(config.worker.mode == WorkerMode::Manual));
    window.set_node_config_reserved_cores(config.worker.reserved_cores as i32);
    window.set_node_config_manual_workers(config.worker.manual_worker_count as i32);
}

fn node_config_phase(phase: dedup_desktop_core::view_state::NodeConfigSavePhase) -> &'static str {
    match phase {
        dedup_desktop_core::view_state::NodeConfigSavePhase::Idle => "已加载",
        dedup_desktop_core::view_state::NodeConfigSavePhase::Validating => "正在校验",
        dedup_desktop_core::view_state::NodeConfigSavePhase::Saving => "正在保存",
        dedup_desktop_core::view_state::NodeConfigSavePhase::Restarting => "Node 正在重启",
        dedup_desktop_core::view_state::NodeConfigSavePhase::WaitingForReconnect => {
            "等待同一机器重连"
        }
        dedup_desktop_core::view_state::NodeConfigSavePhase::Verifying => "正在验证新配置",
        dedup_desktop_core::view_state::NodeConfigSavePhase::Completed => "保存并重启完成",
        dedup_desktop_core::view_state::NodeConfigSavePhase::Failed => "保存失败",
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

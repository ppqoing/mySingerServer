//! Slint 回调到 `UiCommand` 的轻量绑定，以及 `UiEvent` 到窗口属性的整体替换。

use std::sync::{Arc, Mutex};

use dedup_core::{
    DeleteMode, DesktopConfig, DiskReadConfig, EnumeratorKind, NodeConfig, NodePathsConfig,
    NodePostgresConfig, Thresholds, WorkerConfig, WorkerMode,
};
use dedup_desktop_core::{
    app::{UiCommand, UiEvent},
    results::GroupKind,
    review::{QuickReviewRule, ReviewDecision},
    runtime_tasks::{RuntimeTaskKey, RuntimeTaskOwner},
    view_state::NodeConnectionState,
};
use slint::{
    ComponentHandle, Image, Model, ModelRc, Rgba8Pixel, SharedPixelBuffer, SharedString, VecModel,
};
use tokio::sync::mpsc;

use crate::{MainWindow, UiPathRow, UiScanRootRow, models};

/// GUI 回调与事件应用共享的最新已验证配置。
///
/// Slint 回调只读取控件值、构造命令并 `try_send`；网络、数据库和文件写入仍由
/// `dedup-desktop-core::app` 的异步控制循环执行。
#[derive(Clone)]
pub struct UiBinding {
    config: Arc<Mutex<DesktopConfig>>,
    /// 记录最后一次写入数据库表单的持久连接串，避免周期视图刷新覆盖未保存输入。
    applied_postgres_url: Arc<Mutex<Option<Option<String>>>>,
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
        applied_postgres_url: Arc::new(Mutex::new(None)),
        node_config: Arc::new(Mutex::new(None)),
        selected_node: Arc::new(Mutex::new(NodeConfigSelection {
            index: window.get_node_config_selected_index().max(0),
            machine_id: String::new(),
        })),
    };

    bind_scan_roots(window);
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
    window.on_start_scan(move |index, roots, force, enumerator| {
        let roots = match scan_root_values(&roots) {
            Ok(roots) => roots,
            Err(error) => {
                if let Some(window) = scan_window.upgrade() {
                    window.set_last_error(error.into());
                }
                return;
            }
        };
        send(
            &scan_sender,
            UiCommand::CreateScan {
                node_index: index.max(0) as usize,
                roots,
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
    let database_sender = sender.clone();
    let database_window = window.as_weak();
    window.on_test_database_connection(move || {
        let Some(window) = database_window.upgrade() else {
            return;
        };
        let url = match postgres_url_from_window(&window) {
            Ok(Some(url)) => url,
            Ok(None) => {
                window.set_database_testing(false);
                window.set_database_test_error("请先填写 PostgreSQL 服务器".into());
                return;
            }
            Err(error) => {
                window.set_database_testing(false);
                window.set_database_test_error(error.into());
                return;
            }
        };
        window.set_database_testing(true);
        window.set_database_test_status("正在连接并校验 schema…".into());
        window.set_database_test_error(SharedString::default());
        if let Err(error) = database_sender.try_send(UiCommand::TestDatabaseConnection { url }) {
            window.set_database_testing(false);
            window.set_database_test_error(format!("命令队列不可用：{error}").into());
        }
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
            window.set_scan_roots(ModelRc::new(VecModel::from(Vec::<UiScanRootRow>::new())));
            window.set_scan_roots_valid(false);
            window.set_path_picker_open(false);
            window.set_path_picker_target_index(-1);
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
    window.on_save_node_config(move || {
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
            UiCommand::SaveNodeConfig {
                node_index: window.get_node_config_selected_index().max(0) as usize,
                config: wire,
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
    let postgres = NodePostgresConfig::default();
    window.set_node_config_postgres_enabled(postgres.enabled);
    window.set_node_config_postgres_host(postgres.host.into());
    window.set_node_config_postgres_port(i32::from(postgres.port));
    window.set_node_config_postgres_database(postgres.database.into());
    window.set_node_config_postgres_username(postgres.username.into());
    window.set_node_config_postgres_password(postgres.password.into());
    window.set_node_config_postgres_timeout_seconds(postgres.connect_timeout_seconds as i32);
    window.set_scan_root(SharedString::default());
}

fn clear_node_config_context(window: &MainWindow, loaded: &Arc<Mutex<Option<LoadedNodeConfig>>>) {
    clear_node_config_form(window);
    *loaded.lock().expect("Node 配置锁未中毒") = None;
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
        postgres: NodePostgresConfig {
            enabled: window.get_node_config_postgres_enabled(),
            host: window.get_node_config_postgres_host().trim().to_owned(),
            port: window
                .get_node_config_postgres_port()
                .try_into()
                .map_err(|_| "Node PostgreSQL 端口无效".to_owned())?,
            database: window.get_node_config_postgres_database().trim().to_owned(),
            username: window.get_node_config_postgres_username().trim().to_owned(),
            password: window.get_node_config_postgres_password().to_string(),
            connect_timeout_seconds: window
                .get_node_config_postgres_timeout_seconds()
                .try_into()
                .map_err(|_| "Node PostgreSQL 连接超时无效".to_owned())?,
        },
    };
    config.validate().map_err(|error| error.to_string())?;
    Ok(config)
}

fn positive_usize(value: i32, field: &str) -> Result<usize, String> {
    value.try_into().map_err(|_| format!("{field} 无效"))
}

/// 扫描根 Item 只修改窗口模型，不新增后端命令；开始扫描时统一读取完整集合。
fn bind_scan_roots(window: &MainWindow) {
    let select_window = window.as_weak();
    window.on_select_scan_node(move |index| {
        let Some(window) = select_window.upgrade() else {
            return;
        };
        let index = index.max(0);
        if window.get_scan_node_index() == index {
            return;
        }
        window.set_scan_node_index(index);
        window.set_scan_root(SharedString::default());
        window.set_scan_roots(ModelRc::new(VecModel::from(Vec::<UiScanRootRow>::new())));
        window.set_scan_roots_valid(false);
        window.set_path_picker_open(false);
        window.set_path_picker_node_index(index);
        window.set_path_picker_target_index(-1);
        window.set_path_picker_current_path(SharedString::default());
        window.set_path_picker_parent_path(SharedString::default());
        window.set_path_picker_status(SharedString::default());
        window.set_path_picker_directories(ModelRc::new(VecModel::from(Vec::<UiPathRow>::new())));
        window.set_last_error("节点已切换，请重新添加扫描路径".into());
    });

    let add_window = window.as_weak();
    window.on_add_scan_root(move || {
        let Some(window) = add_window.upgrade() else {
            return;
        };
        let mut rows = scan_root_rows(&window.get_scan_roots());
        rows.push(UiScanRootRow {
            path: SharedString::default(),
        });
        replace_scan_roots(&window, rows);
    });

    let update_window = window.as_weak();
    window.on_update_scan_root(move |index, path| {
        let Some(window) = update_window.upgrade() else {
            return;
        };
        let mut rows = scan_root_rows(&window.get_scan_roots());
        let index = index.max(0) as usize;
        let Some(row) = rows.get_mut(index) else {
            window.set_last_error("扫描路径 Item 已不存在".into());
            return;
        };
        row.path = path;
        replace_scan_roots(&window, rows);
    });

    let remove_window = window.as_weak();
    window.on_remove_scan_root(move |index| {
        let Some(window) = remove_window.upgrade() else {
            return;
        };
        let mut rows = scan_root_rows(&window.get_scan_roots());
        let index = index.max(0) as usize;
        if index >= rows.len() {
            window.set_last_error("扫描路径 Item 已不存在".into());
            return;
        }
        rows.remove(index);
        replace_scan_roots(&window, rows);
    });
}

fn scan_root_rows(model: &ModelRc<UiScanRootRow>) -> Vec<UiScanRootRow> {
    (0..model.row_count())
        .filter_map(|index| model.row_data(index))
        .collect()
}

fn replace_scan_roots(window: &MainWindow, rows: Vec<UiScanRootRow>) {
    let model = ModelRc::new(VecModel::from(rows));
    let validation = scan_root_values(&model);
    window.set_scan_root(
        model
            .row_data(0)
            .map_or_else(SharedString::default, |row| row.path),
    );
    window.set_scan_roots(model);
    match validation {
        Ok(_) => {
            window.set_scan_roots_valid(true);
            window.set_last_error(SharedString::default());
        }
        Err(error) => {
            window.set_scan_roots_valid(false);
            window.set_last_error(error.into());
        }
    }
}

fn scan_root_values(model: &ModelRc<UiScanRootRow>) -> Result<Vec<String>, String> {
    if model.row_count() == 0 {
        return Err("请至少添加一个扫描路径".to_owned());
    }
    let mut roots = Vec::with_capacity(model.row_count());
    let mut keys = std::collections::BTreeSet::new();
    for index in 0..model.row_count() {
        let path = model
            .row_data(index)
            .map(|row| row.path.trim().to_owned())
            .unwrap_or_default();
        if path.is_empty() {
            return Err(format!("扫描路径 {} 为空", index + 1));
        }
        let key = path
            .replace('/', "\\")
            .trim_end_matches('\\')
            .to_lowercase();
        if !keys.insert(key) {
            return Err(format!("扫描路径 {} 与已有路径重复", index + 1));
        }
        roots.push(path);
    }
    Ok(roots)
}

/// 在 Slint 线程整体应用一个不可变 core 事件。
pub fn apply_event(window: &MainWindow, binding: &UiBinding, event: UiEvent) {
    match event {
        UiEvent::ViewChanged(state) => {
            *binding.config.lock().expect("UI 配置锁未中毒") = state.config().clone();
            let apply_postgres_fields = {
                let current = state.config().postgres_url.clone();
                let mut applied = binding
                    .applied_postgres_url
                    .lock()
                    .expect("PostgreSQL 表单快照锁未中毒");
                if applied.as_ref() == Some(&current) {
                    false
                } else {
                    *applied = Some(current);
                    true
                }
            };
            window.set_nodes(models::nodes(&state));
            window.set_scan_node_options(models::scan_node_options(&state));
            window.set_online_count(
                state
                    .nodes()
                    .iter()
                    .filter(|node| node.connection == NodeConnectionState::Online)
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
            apply_settings(window, state.config(), apply_postgres_fields);
            window.set_last_error(SharedString::default());
        }
        UiEvent::PathsChanged {
            node_index,
            parent_path,
            entries,
            next_cursor,
        } => {
            if window.get_path_picker_open()
                && window.get_path_picker_node_index() == node_index as i32
            {
                let directory_count = entries.iter().filter(|entry| entry.is_directory).count();
                window.set_path_picker_directories(models::paths(&entries));
                window.set_path_picker_parent_path(models::path_parent(&parent_path).into());
                window.set_path_picker_current_path(parent_path.into());
                window.set_path_picker_status(
                    if next_cursor.is_empty() {
                        format!("{directory_count} 个子目录")
                    } else {
                        format!("已显示 {directory_count} 个子目录；当前目录项目较多")
                    }
                    .into(),
                );
                window.set_last_error(SharedString::default());
            } else {
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
        }
        UiEvent::DatabaseDiagnosticsChanged(result) => {
            window.set_database_testing(false);
            match result {
                Ok(()) => {
                    window.set_database_test_status("连接成功 · Rust V2 schema 正常".into());
                    window.set_database_test_error(SharedString::default());
                    window.set_last_error(SharedString::default());
                }
                Err(error) => {
                    window.set_database_test_status("连接失败".into());
                    window.set_database_test_error(error.clone().into());
                    window.set_last_error(error.into());
                }
            }
        }
        UiEvent::RuntimeTasksChanged(state) => {
            let runtime = models::runtime_tasks(&state);
            // 运行任务控制器独占任务列表和运行中计数，普通视图事件无权覆盖它们。
            window.set_tasks(runtime.tasks);
            window.set_running_count(runtime.running_count);
            window.set_runtime_stages(runtime.stages);
            window.set_runtime_workers(runtime.workers);
            window.set_runtime_failures(runtime.failures);
            window.set_runtime_detail_title(runtime.title);
            window.set_runtime_detail_machine_id(runtime.machine_id);
            window.set_runtime_detail_state(runtime.state);
            window.set_runtime_detail_counts(runtime.counts);
            window.set_runtime_execution_config(runtime.execution_config);
            window.set_runtime_pipeline_metrics(runtime.pipeline_metrics);
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
    window.set_node_config_postgres_enabled(config.postgres.enabled);
    window.set_node_config_postgres_host(config.postgres.host.clone().into());
    window.set_node_config_postgres_port(i32::from(config.postgres.port));
    window.set_node_config_postgres_database(config.postgres.database.clone().into());
    window.set_node_config_postgres_username(config.postgres.username.clone().into());
    window.set_node_config_postgres_password(config.postgres.password.clone().into());
    window.set_node_config_postgres_timeout_seconds(config.postgres.connect_timeout_seconds as i32);
}

fn node_config_phase(phase: dedup_desktop_core::view_state::NodeConfigSavePhase) -> &'static str {
    match phase {
        dedup_desktop_core::view_state::NodeConfigSavePhase::Idle => "已加载",
        dedup_desktop_core::view_state::NodeConfigSavePhase::Saving => "正在保存",
        dedup_desktop_core::view_state::NodeConfigSavePhase::Completed => {
            "保存完成（重启 Node 后生效）"
        }
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
    config.postgres_url = postgres_url_from_window(window)?;
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

fn apply_settings(window: &MainWindow, config: &DesktopConfig, apply_postgres_fields: bool) {
    if apply_postgres_fields {
        window.set_postgres_url(config.postgres_url.clone().unwrap_or_default().into());
        let postgres = postgres_fields_from_url(config.postgres_url.as_deref());
        window.set_postgres_host(postgres.host.into());
        window.set_postgres_port(postgres.port);
        window.set_postgres_database(postgres.database.into());
        window.set_postgres_username(postgres.username.into());
        window.set_postgres_password(postgres.password.into());
    }
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

/// PostgreSQL URL 拆分后的基础连接字段，只存在于桌面 UI 编辑边界。
#[derive(Debug, Default, Eq, PartialEq)]
struct PostgresConnectionFields {
    /// 服务器 IPv4、IPv6 或主机名。
    host: String,
    /// PostgreSQL TCP 端口。
    port: i32,
    /// 目标数据库名称。
    database: String,
    /// 登录用户名。
    username: String,
    /// 登录密码。
    password: String,
}

/// 将 UI 基础字段编码为既有 `DesktopConfig.postgres_url`，不改变配置文件结构。
fn postgres_url_from_window(window: &MainWindow) -> Result<Option<String>, String> {
    let host = window.get_postgres_host().trim().to_owned();
    if host.is_empty() {
        return Ok(None);
    }
    if host
        .chars()
        .any(|character| character.is_whitespace() || matches!(character, '/' | '@' | '?' | '#'))
    {
        return Err("PostgreSQL 服务器地址无效".into());
    }

    let database = window.get_postgres_database().trim().to_owned();
    if database.is_empty() {
        return Err("PostgreSQL 数据库名不能为空".into());
    }
    let username = window.get_postgres_username().trim().to_owned();
    if username.is_empty() {
        return Err("PostgreSQL 用户名不能为空".into());
    }
    let port = u16::try_from(window.get_postgres_port())
        .ok()
        .filter(|port| *port != 0)
        .ok_or_else(|| "PostgreSQL 端口无效".to_owned())?;
    let password = window.get_postgres_password();
    let authority_host = if host.contains(':') && !(host.starts_with('[') && host.ends_with(']')) {
        format!("[{host}]")
    } else {
        host
    };

    Ok(Some(format!(
        "postgresql://{}:{}@{}:{}/{}",
        encode_postgres_component(&username),
        encode_postgres_component(password.as_str()),
        authority_host,
        port,
        encode_postgres_component(&database),
    )))
}

/// 把现有 PostgreSQL URL 拆回基础字段；缺失或不支持的 URL 返回空字段与默认端口。
fn postgres_fields_from_url(url: Option<&str>) -> PostgresConnectionFields {
    let Some(raw_url) = url.map(str::trim).filter(|value| !value.is_empty()) else {
        return PostgresConnectionFields {
            port: 5432,
            ..PostgresConnectionFields::default()
        };
    };
    let Some(connection) = raw_url
        .strip_prefix("postgresql://")
        .or_else(|| raw_url.strip_prefix("postgres://"))
    else {
        return PostgresConnectionFields {
            port: 5432,
            ..PostgresConnectionFields::default()
        };
    };

    let (credentials, endpoint) = connection.rsplit_once('@').unwrap_or(("", connection));
    let (encoded_username, encoded_password) =
        credentials.split_once(':').unwrap_or((credentials, ""));
    let (authority, database_path) = endpoint.split_once('/').unwrap_or((endpoint, ""));
    let encoded_database = database_path
        .split(|character| matches!(character, '?' | '#'))
        .next()
        .unwrap_or_default();
    let (host, port) = split_postgres_authority(authority);

    PostgresConnectionFields {
        host: host.to_owned(),
        port: i32::from(port),
        database: decode_postgres_component(encoded_database),
        username: decode_postgres_component(encoded_username),
        password: decode_postgres_component(encoded_password),
    }
}

/// 解析 PostgreSQL authority，并兼容带方括号的 IPv6 地址。
fn split_postgres_authority(authority: &str) -> (&str, u16) {
    if let Some(ipv6) = authority.strip_prefix('[')
        && let Some(end) = ipv6.find(']')
    {
        let host = &ipv6[..end];
        let port = ipv6[end + 1..]
            .strip_prefix(':')
            .and_then(|value| value.parse().ok())
            .unwrap_or(5432);
        return (host, port);
    }
    if let Some((host, port)) = authority.rsplit_once(':')
        && let Ok(port) = port.parse()
    {
        return (host, port);
    }
    (authority, 5432)
}

/// 使用 URL unreserved 规则编码用户名、密码和数据库名的 UTF-8 字节。
fn encode_postgres_component(value: &str) -> String {
    let mut encoded = String::with_capacity(value.len());
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_' | b'~') {
            encoded.push(char::from(byte));
        } else {
            encoded.push_str(&format!("%{byte:02X}"));
        }
    }
    encoded
}

/// 解码 URL 百分号字节；无效编码保持原字节，避免加载配置时丢失用户输入。
fn decode_postgres_component(value: &str) -> String {
    let bytes = value.as_bytes();
    let mut decoded = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'%'
            && index + 2 < bytes.len()
            && let (Some(high), Some(low)) =
                (hex_value(bytes[index + 1]), hex_value(bytes[index + 2]))
        {
            decoded.push((high << 4) | low);
            index += 3;
        } else {
            decoded.push(bytes[index]);
            index += 1;
        }
    }
    String::from_utf8(decoded).unwrap_or_else(|_| value.to_owned())
}

/// 把单个 ASCII 十六进制字符转换为数值。
fn hex_value(value: u8) -> Option<u8> {
    match value {
        b'0'..=b'9' => Some(value - b'0'),
        b'a'..=b'f' => Some(value - b'a' + 10),
        b'A'..=b'F' => Some(value - b'A' + 10),
        _ => None,
    }
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

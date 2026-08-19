//! Slint 回调与异步节点/中心服务之间的单向命令和事件通道。

use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
    sync::Arc,
};

use dedup_core::{DesktopConfig, EnumeratorKind, TaskId};
use dedup_protocol::proto;
use tokio::sync::mpsc;
use uuid::Uuid;

use crate::{
    central::{CentralError, CentralStore},
    node_session::NodeSession,
    sync::{SyncEngine, SyncTrigger},
    view_state::{
        DesktopPaths, DesktopViewState, NodeConnectionState, NodeRuntimeStats, PostgresHealth,
        TaskView, ViewTaskState,
    },
};

/// Slint 回调允许发送的管理命令；回调自身不执行网络或文件 IO。
#[derive(Clone, Debug)]
pub enum UiCommand {
    /// 追加手工节点并保存配置。
    AddNode {
        /// IPv4 或 IPv6 文本。
        ip: String,
        /// TCP 端口。
        port: u16,
    },
    /// 修改一个手工节点并保存配置。
    EditNode {
        /// 节点列表索引。
        index: usize,
        /// 新 IP 文本。
        ip: String,
        /// 新 TCP 端口。
        port: u16,
    },
    /// 移除一个手工节点并保存配置。
    RemoveNode {
        /// 节点列表索引。
        index: usize,
    },
    /// 并行连接全部手工节点并刷新 PostgreSQL 能力。
    ConnectAll,
    /// 刷新全部在线节点状态和任务列表。
    Refresh,
    /// 使用节点唯一同步路径立即追赶 PostgreSQL。
    SyncNow {
        /// 节点列表索引。
        index: usize,
    },
    /// 在指定节点创建扫描任务。
    CreateScan {
        /// 节点列表索引。
        node_index: usize,
        /// 已通过节点路径浏览选中的根目录。
        roots: Vec<String>,
        /// 是否明确忽略已有特征。
        force_recalculate: bool,
        /// Windows Walker 或 Everything。
        enumerator: EnumeratorKind,
    },
    /// 取消一个节点持久任务。
    CancelTask {
        /// 节点列表索引。
        node_index: usize,
        /// UUID 文本任务 ID。
        task_id: String,
    },
    /// 分页浏览节点目录。
    BrowsePaths {
        /// 节点列表索引。
        node_index: usize,
        /// 空字符串表示盘符列表。
        parent_path: String,
        /// 节点返回的不透明下一页游标。
        cursor: String,
    },
    /// 保存完整配置；校验失败保持旧配置。
    SaveSettings(DesktopConfig),
    /// 有序结束后台控制循环。
    Shutdown,
}

/// 路径选择器展示的一行节点文件系统结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PathEntryView {
    /// 节点可访问的实际路径。
    pub display_path: String,
    /// 是否可以继续向下浏览。
    pub is_directory: bool,
}

/// 异步 core 发给 Slint UI 线程的不可变事件。
#[derive(Clone, Debug, PartialEq)]
pub enum UiEvent {
    /// 整体替换节点、任务、设置和诊断状态。
    ViewChanged(Box<DesktopViewState>),
    /// 返回一页路径浏览结果。
    PathsChanged {
        /// 结果所属节点。
        node_index: usize,
        /// 当前父路径。
        parent_path: String,
        /// 当前页目录/文件。
        entries: Vec<PathEntryView>,
        /// 空字符串表示已到末页。
        next_cursor: String,
    },
    /// 显示一次边界错误，不替换已有可用状态。
    Error(String),
    /// 后台已经停止，GUI 可以退出。
    ShutdownComplete,
}

/// GUI 线程持有的可克隆命令发送端。
#[derive(Clone)]
pub struct DesktopApp {
    commands: mpsc::Sender<UiCommand>,
}

impl DesktopApp {
    /// 启动唯一后台控制循环并返回 GUI 命令句柄与事件接收端。
    pub fn start(config: DesktopConfig, paths: DesktopPaths) -> (Self, mpsc::Receiver<UiEvent>) {
        let config_path = paths.config.clone();
        let state = DesktopViewState::new(config, paths);
        let (commands, command_receiver) = mpsc::channel(64);
        let (events, event_receiver) = mpsc::channel(64);
        tokio::spawn(run_controller(state, config_path, command_receiver, events));
        (Self { commands }, event_receiver)
    }

    /// 返回 Slint 回调可复制进闭包的有界发送端。
    pub fn command_sender(&self) -> mpsc::Sender<UiCommand> {
        self.commands.clone()
    }

    /// 从异步调用方排入一个命令。
    pub async fn send(&self, command: UiCommand) -> Result<(), mpsc::error::SendError<UiCommand>> {
        self.commands.send(command).await
    }
}

async fn run_controller(
    mut state: DesktopViewState,
    config_path: PathBuf,
    mut commands: mpsc::Receiver<UiCommand>,
    events: mpsc::Sender<UiEvent>,
) {
    let mut sessions = BTreeMap::<usize, Arc<NodeSession>>::new();
    let mut central = connect_central(&mut state).await;
    let mut sync_engines = BTreeMap::<usize, SyncEngine>::new();
    publish(&events, &state).await;
    while let Some(command) = commands.recv().await {
        let result = match command {
            UiCommand::AddNode { ip, port } => state
                .add_node(&ip, port)
                .map(|_| ())
                .map_err(|error| error.to_string())
                .and_then(|_| persist(&config_path, state.config())),
            UiCommand::EditNode { index, ip, port } => state
                .edit_node(index, &ip, port)
                .map_err(|error| error.to_string())
                .and_then(|_| {
                    sessions.clear();
                    persist(&config_path, state.config())
                }),
            UiCommand::RemoveNode { index } => state
                .remove_node(index)
                .map_err(|error| error.to_string())
                .and_then(|_| {
                    sessions.clear();
                    persist(&config_path, state.config())
                }),
            UiCommand::ConnectAll => {
                connect_all(&mut state, &mut sessions).await;
                if central.is_none() {
                    central = connect_central(&mut state).await;
                }
                Ok(())
            }
            UiCommand::Refresh => {
                refresh_nodes(&mut state, &sessions, central.as_ref()).await;
                Ok(())
            }
            UiCommand::SyncNow { index } => {
                sync_now(
                    index,
                    &mut state,
                    &sessions,
                    central.as_mut(),
                    &mut sync_engines,
                )
                .await
            }
            UiCommand::CreateScan {
                node_index,
                roots,
                force_recalculate,
                enumerator,
            } => {
                create_scan(
                    node_index,
                    roots,
                    force_recalculate,
                    enumerator,
                    &mut state,
                    &sessions,
                )
                .await
            }
            UiCommand::CancelTask {
                node_index,
                task_id,
            } => cancel_task(node_index, &task_id, &sessions).await,
            UiCommand::BrowsePaths {
                node_index,
                parent_path,
                cursor,
            } => browse_paths(node_index, &parent_path, &cursor, &sessions, &events).await,
            UiCommand::SaveSettings(config) => {
                let result = state
                    .apply_settings(config)
                    .map_err(|error| error.to_string())
                    .and_then(|_| persist(&config_path, state.config()));
                if result.is_ok() {
                    sessions.clear();
                    sync_engines.clear();
                    central = connect_central(&mut state).await;
                }
                result
            }
            UiCommand::Shutdown => {
                let _ = events.send(UiEvent::ShutdownComplete).await;
                break;
            }
        };
        if let Err(error) = result {
            let _ = events.send(UiEvent::Error(error)).await;
        }
        publish(&events, &state).await;
    }
}

async fn connect_all(
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
) {
    sessions.clear();
    let endpoints = state
        .nodes()
        .iter()
        .enumerate()
        .map(|(index, node)| (index, node.endpoint.clone()))
        .collect::<Vec<_>>();
    for (index, _) in &endpoints {
        state.set_node_connection(*index, NodeConnectionState::Connecting, None);
    }
    let mut attempts = tokio::task::JoinSet::new();
    for (index, endpoint) in endpoints {
        attempts.spawn(async move { (index, NodeSession::connect(endpoint).await) });
    }
    while let Some(joined) = attempts.join_next().await {
        let Ok((index, result)) = joined else {
            continue;
        };
        match result {
            Ok(session) => {
                let session = Arc::new(session);
                match session.status().await {
                    Ok(status) => {
                        state.set_node_identity(index, session.machine_id().as_str());
                        state.set_node_connection(
                            index,
                            NodeConnectionState::Online,
                            Some(runtime_stats(status, 0)),
                        );
                        sessions.insert(index, session);
                    }
                    Err(error) => state.set_node_error(index, error.to_string()),
                }
            }
            Err(error) => state.set_node_error(index, error.to_string()),
        }
    }
}

async fn refresh_nodes(
    state: &mut DesktopViewState,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
) {
    for (index, session) in sessions {
        match session.status().await {
            Ok(status) => {
                let sync = if let Some(central) = central {
                    central.sync_cursor(session.machine_id()).await.unwrap_or(0)
                } else {
                    0
                };
                state.set_node_connection(
                    *index,
                    NodeConnectionState::Online,
                    Some(runtime_stats(status, sync)),
                );
                if let Ok(page) = session.list_tasks("", 1000).await {
                    for task in page.tasks {
                        if let Ok(task) = task_view(*index, task) {
                            state.upsert_task(task);
                        }
                    }
                }
            }
            Err(error) => state.set_node_error(*index, error.to_string()),
        }
    }
}

async fn sync_now(
    index: usize,
    state: &mut DesktopViewState,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&mut CentralStore>,
    engines: &mut BTreeMap<usize, SyncEngine>,
) -> Result<(), String> {
    let session = sessions
        .get(&index)
        .ok_or_else(|| "节点当前未连接".to_owned())?;
    let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
    let report = engines
        .entry(index)
        .or_default()
        .sync_node(session.as_ref(), central, SyncTrigger::Manual)
        .await
        .map_err(|error| error.to_string())?;
    let status = session.status().await.map_err(|error| error.to_string())?;
    state.set_node_connection(
        index,
        NodeConnectionState::Online,
        Some(runtime_stats(status, report.committed_seq)),
    );
    Ok(())
}

async fn create_scan(
    node_index: usize,
    roots: Vec<String>,
    force_recalculate: bool,
    enumerator: EnumeratorKind,
    state: &mut DesktopViewState,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
) -> Result<(), String> {
    let session = sessions
        .get(&node_index)
        .ok_or_else(|| "节点当前未连接".to_owned())?;
    let enumerator = match enumerator {
        EnumeratorKind::WindowsWalker => "windows_walker",
        EnumeratorKind::Everything => "everything",
    };
    let task_id = session
        .create_scan(roots, force_recalculate, enumerator)
        .await
        .map_err(|error| error.to_string())?;
    let task = session
        .query_task(task_id)
        .await
        .map_err(|error| error.to_string())?;
    state.upsert_task(task_view(node_index, task)?);
    Ok(())
}

async fn cancel_task(
    node_index: usize,
    task_id: &str,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
) -> Result<(), String> {
    let session = sessions
        .get(&node_index)
        .ok_or_else(|| "节点当前未连接".to_owned())?;
    let task_id = Uuid::parse_str(task_id)
        .map(TaskId::from_uuid)
        .map_err(|_| "任务 ID 不是 UUID".to_owned())?;
    session
        .cancel_task(task_id)
        .await
        .map_err(|error| error.to_string())
}

async fn browse_paths(
    node_index: usize,
    parent_path: &str,
    cursor: &str,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let session = sessions
        .get(&node_index)
        .ok_or_else(|| "节点当前未连接".to_owned())?;
    let page = session
        .browse_paths(parent_path, cursor, 500)
        .await
        .map_err(|error| error.to_string())?;
    events
        .send(UiEvent::PathsChanged {
            node_index,
            parent_path: page.parent_path,
            entries: page
                .entries
                .into_iter()
                .map(|entry| PathEntryView {
                    display_path: entry.display_path,
                    is_directory: entry.is_directory,
                })
                .collect(),
            next_cursor: page.next_cursor,
        })
        .await
        .map_err(|_| "UI 事件通道已经关闭".to_owned())
}

async fn connect_central(state: &mut DesktopViewState) -> Option<CentralStore> {
    let Some(url) = state.config().postgres_url.clone() else {
        state.set_postgres_health(PostgresHealth::Disabled);
        return None;
    };
    state.set_postgres_health(PostgresHealth::Connecting);
    match CentralStore::connect(&url).await {
        Ok(store) => {
            state.set_postgres_health(PostgresHealth::Ready);
            Some(store)
        }
        Err(CentralError::SchemaMissing { .. }) => {
            state.set_postgres_health(PostgresHealth::SchemaMissing);
            None
        }
        Err(error) => {
            state.set_postgres_health(PostgresHealth::Error(error.to_string()));
            None
        }
    }
}

fn runtime_stats(status: proto::NodeStatus, sync_high_seq: u64) -> NodeRuntimeStats {
    NodeRuntimeStats {
        worker_count: status.worker_count,
        busy_workers: status.busy_workers,
        queued_items: status.queued_items,
        running_items: status.running_items,
        outbox_high_seq: status.outbox_high_seq,
        sync_high_seq,
    }
}

fn task_view(node_index: usize, task: proto::TaskSummary) -> Result<TaskView, String> {
    let state = match proto::TaskState::try_from(task.state).unwrap_or_default() {
        proto::TaskState::TaskQueued => ViewTaskState::Queued,
        proto::TaskState::TaskRunning => ViewTaskState::Running,
        proto::TaskState::TaskCompleted => ViewTaskState::Completed,
        proto::TaskState::TaskFailed => ViewTaskState::Failed,
        proto::TaskState::TaskCancelled => ViewTaskState::Cancelled,
        proto::TaskState::Unspecified => return Err("节点任务状态未指定".into()),
    };
    Ok(TaskView {
        task_id: task.task_id,
        node_index,
        title: task.task_kind,
        stage: "节点任务".into(),
        state,
        completed_items: task.completed_items,
        total_items: task.total_items,
        failed_items: task.failed_items,
        skipped_incomplete: task.skipped_items,
    })
}

fn persist(path: &Path, config: &DesktopConfig) -> Result<(), String> {
    fs::write(path, config.to_toml().map_err(|error| error.to_string())?)
        .map_err(|error| error.to_string())
}

async fn publish(events: &mpsc::Sender<UiEvent>, state: &DesktopViewState) {
    let _ = events
        .send(UiEvent::ViewChanged(Box::new(state.clone())))
        .await;
}

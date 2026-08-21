//! Slint 回调与异步节点/中心服务之间的单向命令和事件通道。

use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
    sync::Arc,
    time::Duration,
};

use dedup_core::{
    AnalysisRunId, DeleteMode, DesktopConfig, EnumeratorKind, LocationKey, MachineId, NodeConfig,
    NodeEndpoint, NormalizedPath, TaskId,
};
use dedup_protocol::proto;
use tokio::{
    sync::mpsc,
    task::JoinHandle,
    time::{Instant, Interval, MissedTickBehavior, interval},
};
use uuid::Uuid;

use crate::{
    analysis::{CrossAnalysisCoordinator, CrossNodeSelection, CrossPollReport},
    central::{
        CentralDeleteOutcome, CentralDeleteResult, CentralDeleteSelection, CentralError,
        CentralReviewDecision, CentralStore,
    },
    delete::{DeleteConfirmation, ReviewGroup},
    node_session::NodeSession,
    results::{
        GroupKind, GroupPage, MemberPage, MemberView, ResultScope, group_page_from_central,
        group_page_from_node, load_preview, member_page_from_central, member_page_from_node,
    },
    review::{QuickReviewRule, ReviewBoard, ReviewDecision},
    sync::{
        AUTO_CATCH_UP_INTERVAL_SECONDS, SyncEngine, SyncError, SyncTriggerReceiver,
        SyncTriggerSender, sync_trigger_channel,
    },
    view_state::{
        DesktopPaths, DesktopViewState, NodeConfigControllerState, NodeConfigSavePhase,
        NodeConnectionState, NodeRuntimeStats, PostgresHealth, TaskView, ViewTaskState,
    },
};

const NODE_CONFIG_RECONNECT_ATTEMPTS: u64 = 3;

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
    /// 从一个或多个已完成扫描任务创建节点本地分析。
    StartLocalAnalysis {
        /// 运行分析的节点索引。
        node_index: usize,
        /// 逗号分隔的节点扫描任务 UUID。
        scan_task_ids: String,
        /// 精确、相似图片或相似视频。
        kind: GroupKind,
    },
    /// 从 `节点索引:扫描任务 UUID` 列表创建跨机器中心分析。
    StartCrossAnalysis {
        /// 逗号分隔的节点索引和任务 ID。
        selections: String,
    },
    /// 推进当前进程创建的跨机器运行直到下一门禁。
    PollCrossAnalysis,
    /// 对当前 Partial 中心运行显式重试未解决二筛项。
    RetryCrossAnalysis,
    /// 使用稳定游标加载本地或中心重复组。
    LoadGroups {
        /// `true` 表示 PostgreSQL，`false` 表示节点 SQLite。
        central: bool,
        /// 本地来源节点索引；中心来源忽略。
        node_index: usize,
        /// UUID 文本运行 ID。
        analysis_run_id: String,
        /// 页面需要的结果类别。
        kind: GroupKind,
        /// 空字符串为第一页。
        cursor: String,
    },
    /// 加载一个重复组的活动成员。
    LoadMembers {
        /// `true` 表示 PostgreSQL，`false` 表示节点 SQLite。
        central: bool,
        /// 本地来源节点索引。
        node_index: usize,
        /// UUID 文本运行 ID。
        analysis_run_id: String,
        /// 持久组 ID。
        group_id: String,
        /// 组类别决定预览类型。
        kind: GroupKind,
        /// 空字符串为第一页。
        cursor: String,
    },
    /// 保存一个成员的 Keep/Delete/Undecided 标记。
    SaveReview {
        /// 成员物理机器 ID。
        machine_id: String,
        /// 成员规范路径。
        normalized_path: String,
        /// 新标记。
        decision: ReviewDecision,
    },
    /// 对当前已载入组应用一个只更新标记的快捷规则。
    ApplyQuickReview(QuickReviewRule),
    /// 为当前在线成员按需读取原图或 JPG 联系表。
    LoadPreview {
        /// 成员物理机器 ID。
        machine_id: String,
        /// 成员规范路径。
        normalized_path: String,
    },
    /// 生成删除摘要并打开确认对话框，不执行文件操作。
    PrepareDelete,
    /// 执行最近一次仍有效且通过门禁的删除确认。
    ConfirmDelete,
    /// 按手工节点索引加载当前远程 Node 配置。
    LoadNodeConfig {
        /// 用户当前选择的节点列表索引。
        node_index: usize,
    },
    /// 使用已加载摘要保存远程配置并等待同一机器重连验证。
    SaveNodeConfigAndRestart {
        /// 用户发起操作时的节点列表索引。
        node_index: usize,
        /// 设置页提交的完整 wire 配置值。
        config: proto::NodeConfigValue,
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
    /// 远程 Node 配置表单或保存阶段已经改变。
    NodeConfigChanged(NodeConfigControllerState),
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
    /// 本地或中心分析运行已经创建。
    AnalysisStarted {
        /// `true` 表示中心运行。
        central: bool,
        /// UUID 文本运行 ID。
        run_id: String,
        /// 当前持久状态。
        status: String,
    },
    /// 跨机器运行推进到一个可观察门禁。
    CrossAnalysisChanged(CrossPollReport),
    /// 替换当前结果列表页。
    GroupsChanged(Box<GroupPage>),
    /// 替换当前选中组及其成员页。
    MembersChanged {
        /// 持久组 ID。
        group_id: String,
        /// 当前页成员。
        page: Box<MemberPage>,
    },
    /// 原图或视频联系表已经完整读入内存。
    PreviewReady {
        /// 预览请求所属物理机器 ID。
        machine_id: String,
        /// 预览请求使用的规范路径。
        normalized_path: String,
        /// 预览所属路径。
        display_path: String,
        /// `original` 或 `contact_sheet`。
        file_kind: String,
        /// 原始编码数据，不写入缓存目录。
        bytes: Arc<[u8]>,
    },
    /// 一次带身份的预览请求已经失败。
    PreviewFailed {
        /// 预览请求所属物理机器 ID。
        machine_id: String,
        /// 预览请求使用的规范路径。
        normalized_path: String,
        /// 原始业务错误文本。
        error: String,
    },
    /// 快捷或单项复核已持久化，返回刷新后的成员。
    ReviewChanged(Box<MemberPage>),
    /// 删除执行前的显式确认摘要。
    DeleteConfirmationChanged(DeleteConfirmation),
    /// 本地或中心删除结果已经应用并应刷新结果页。
    DeleteFinished(String),
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

struct LoadedMembersContext {
    scope: ResultScope,
    kind: GroupKind,
    group_id: String,
    items: Vec<MemberView>,
    next_cursor: Option<String>,
}

struct PreparedDeleteContext {
    scope: ResultScope,
    group_id: String,
    confirmation: DeleteConfirmation,
    items: Vec<MemberView>,
}

#[derive(Clone)]
struct PendingNodeConfigSave {
    machine_id: String,
    endpoint: NodeEndpoint,
    saved_version_sha256: String,
    deadline: Instant,
}

/// 一个节点独占的同步触发端和后台任务；丢弃时立即结束其 PG 连接与同步请求。
struct NodeSyncWorker {
    id: Uuid,
    triggers: SyncTriggerSender,
    task: JoinHandle<()>,
}

impl Drop for NodeSyncWorker {
    fn drop(&mut self) {
        self.task.abort();
    }
}

/// 节点同步后台任务返回给唯一控制循环的不可变结果。
struct NodeSyncEvent {
    index: usize,
    worker_id: Uuid,
    machine_id: String,
    outcome: NodeSyncOutcome,
}

/// 同步成功携带真实节点状态；失败明确指出应重建哪一侧连接。
enum NodeSyncOutcome {
    Succeeded {
        committed_seq: u64,
        status: proto::NodeStatus,
    },
    Failed {
        message: String,
        session_failed: bool,
        central_failed: bool,
    },
}

async fn run_controller(
    mut state: DesktopViewState,
    config_path: PathBuf,
    mut commands: mpsc::Receiver<UiCommand>,
    events: mpsc::Sender<UiEvent>,
) {
    let mut sessions = BTreeMap::<usize, Arc<NodeSession>>::new();
    let mut central = connect_central(&mut state).await;
    let mut sync_workers = BTreeMap::<usize, NodeSyncWorker>::new();
    let (sync_result_sender, mut sync_results) = mpsc::unbounded_channel();
    let mut cross_analysis: Option<CrossAnalysisCoordinator> = None;
    let mut loaded_members: Option<LoadedMembersContext> = None;
    let mut prepared_delete: Option<PreparedDeleteContext> = None;
    let mut pending_node_config: Option<PendingNodeConfigSave> = None;
    let mut reconnect_ticks = repeating_interval(state.config().reconnect_interval_seconds);
    let mut catch_up_ticks = repeating_interval(AUTO_CATCH_UP_INTERVAL_SECONDS);
    catch_up_ticks.tick().await;
    publish(&events, &state).await;
    loop {
        let command = tokio::select! {
            command = commands.recv() => match command {
                Some(command) => command,
                None => break,
            },
            _ = reconnect_ticks.tick() => {
                let pending_endpoint = pending_node_config
                    .as_ref()
                    .map(|pending| pending.endpoint.clone());
                reconnect_and_sync(
                    &mut state,
                    &mut sessions,
                    &mut central,
                    &mut sync_workers,
                    &sync_result_sender,
                    pending_endpoint.as_ref(),
                ).await;
                verify_reconnected_node_config(
                    &mut state,
                    &mut sessions,
                    &mut sync_workers,
                    &mut pending_node_config,
                    &events,
                    &sync_result_sender,
                ).await;
                publish(&events, &state).await;
                continue;
            }
            _ = catch_up_ticks.tick() => {
                catch_up_and_refresh(
                    &mut state,
                    &mut sessions,
                    &mut central,
                    &mut sync_workers,
                    &sync_result_sender,
                ).await;
                publish(&events, &state).await;
                continue;
            }
            Some(sync_result) = sync_results.recv() => {
                apply_sync_result(
                    sync_result,
                    &mut state,
                    &mut sessions,
                    &mut central,
                    &mut sync_workers,
                    &events,
                ).await;
                publish(&events, &state).await;
                continue;
            }
        };
        let result = match command {
            UiCommand::AddNode { ip, port } => state
                .add_node(&ip, port)
                .map(|_| ())
                .map_err(|error| error.to_string())
                .and_then(|_| persist(&config_path, state.config())),
            UiCommand::EditNode { index, ip, port } => {
                if state.node_config().is_in_progress() {
                    Err(node_config_target_change_error())
                } else {
                    state
                        .edit_node(index, &ip, port)
                        .map_err(|error| error.to_string())
                        .and_then(|_| {
                            sessions.clear();
                            sync_workers.clear();
                            persist(&config_path, state.config())
                        })
                }
            }
            UiCommand::RemoveNode { index } => {
                if state.node_config().is_in_progress() {
                    Err(node_config_target_change_error())
                } else {
                    state
                        .remove_node(index)
                        .map_err(|error| error.to_string())
                        .and_then(|_| {
                            sessions.clear();
                            sync_workers.clear();
                            persist(&config_path, state.config())
                        })
                }
            }
            UiCommand::ConnectAll => {
                if state.node_config().is_in_progress() {
                    Err(node_config_target_change_error())
                } else {
                    sync_workers.clear();
                    connect_all(&mut state, &mut sessions).await;
                    if central.is_none() {
                        central = connect_central(&mut state).await;
                    }
                    let indexes = sessions.keys().copied().collect::<Vec<_>>();
                    ensure_sync_workers(
                        &indexes,
                        &sessions,
                        state.config().postgres_url.as_deref(),
                        &mut sync_workers,
                        &sync_result_sender,
                    );
                    queue_automatic(&indexes, &sync_workers, AutomaticSyncCause::Connected).await;
                    Ok(())
                }
            }
            UiCommand::Refresh => {
                let report = refresh_nodes(&mut state, &sessions, central.as_ref()).await;
                apply_refresh_report(
                    report,
                    &mut state,
                    &mut sessions,
                    &mut central,
                    &mut sync_workers,
                );
                Ok(())
            }
            UiCommand::SyncNow { index } => queue_manual(index, &sync_workers).await,
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
            UiCommand::StartLocalAnalysis {
                node_index,
                scan_task_ids,
                kind,
            } => {
                start_local_analysis(
                    node_index,
                    &scan_task_ids,
                    kind,
                    state.config(),
                    &sessions,
                    &events,
                )
                .await
            }
            UiCommand::StartCrossAnalysis { selections } => {
                start_cross_analysis(
                    &selections,
                    state.config(),
                    &sessions,
                    central.as_mut(),
                    &mut cross_analysis,
                    &events,
                )
                .await
            }
            UiCommand::PollCrossAnalysis => {
                poll_cross_analysis(
                    false,
                    &sessions,
                    central.as_mut(),
                    cross_analysis.as_mut(),
                    &events,
                )
                .await
            }
            UiCommand::RetryCrossAnalysis => {
                poll_cross_analysis(
                    true,
                    &sessions,
                    central.as_mut(),
                    cross_analysis.as_mut(),
                    &events,
                )
                .await
            }
            UiCommand::LoadGroups {
                central: use_central,
                node_index,
                analysis_run_id,
                kind,
                cursor,
            } => {
                load_groups(
                    use_central,
                    node_index,
                    &analysis_run_id,
                    kind,
                    &cursor,
                    &sessions,
                    central.as_ref(),
                    &events,
                )
                .await
            }
            UiCommand::LoadMembers {
                central: use_central,
                node_index,
                analysis_run_id,
                group_id,
                kind,
                cursor,
            } => {
                let result = load_members(
                    use_central,
                    node_index,
                    &analysis_run_id,
                    &group_id,
                    kind,
                    &cursor,
                    &sessions,
                    central.as_ref(),
                )
                .await;
                match result {
                    Ok(context) => {
                        let page = MemberPage {
                            items: context.items.clone(),
                            next_cursor: context.next_cursor.clone(),
                        };
                        loaded_members = Some(context);
                        prepared_delete = None;
                        send_event(
                            &events,
                            UiEvent::MembersChanged {
                                group_id,
                                page: Box::new(page),
                            },
                        )
                        .await
                    }
                    Err(error) => Err(error),
                }
            }
            UiCommand::SaveReview {
                machine_id,
                normalized_path,
                decision,
            } => {
                prepared_delete = None;
                save_one_review(
                    &machine_id,
                    &normalized_path,
                    decision,
                    loaded_members.as_mut(),
                    &sessions,
                    central.as_ref(),
                    &events,
                )
                .await
            }
            UiCommand::ApplyQuickReview(rule) => {
                prepared_delete = None;
                apply_quick_review(
                    rule,
                    loaded_members.as_mut(),
                    &sessions,
                    central.as_ref(),
                    &events,
                )
                .await
            }
            UiCommand::LoadPreview {
                machine_id,
                normalized_path,
            } => {
                load_member_preview(
                    &machine_id,
                    &normalized_path,
                    loaded_members.as_ref(),
                    &sessions,
                    &events,
                )
                .await
            }
            UiCommand::PrepareDelete => {
                prepare_delete(
                    state.config().delete_mode,
                    loaded_members.as_ref(),
                    &sessions,
                    central.as_ref(),
                    &mut prepared_delete,
                    &events,
                )
                .await
            }
            UiCommand::ConfirmDelete => {
                let result = confirm_delete(
                    prepared_delete.as_ref(),
                    &sessions,
                    central.as_mut(),
                    &events,
                )
                .await;
                if result.is_ok() {
                    prepared_delete = None;
                    loaded_members = None;
                }
                result
            }
            UiCommand::LoadNodeConfig { node_index } => {
                if state.node_config().is_in_progress() {
                    Err(node_config_target_change_error())
                } else {
                    pending_node_config = None;
                    state.select_node_config(node_index);
                    publish_node_config(&events, &state).await;
                    load_node_config(node_index, &mut state, &sessions, &events).await
                }
            }
            UiCommand::SaveNodeConfigAndRestart { node_index, config } => {
                if state.node_config().is_in_progress() {
                    Err(node_config_target_change_error())
                } else {
                    let result = save_node_config_and_restart(
                        node_index,
                        config,
                        &mut state,
                        &mut sessions,
                        &mut sync_workers,
                        &events,
                    )
                    .await;
                    match result {
                        Ok(pending) => {
                            pending_node_config = Some(pending);
                            Ok(())
                        }
                        Err(error) => {
                            state.fail_node_config(error.clone());
                            publish_node_config(&events, &state).await;
                            Err(error)
                        }
                    }
                }
            }
            UiCommand::SaveSettings(config) => {
                let result = if state.node_config().is_in_progress() {
                    Err(node_config_target_change_error())
                } else {
                    state
                        .apply_settings(config)
                        .map_err(|error| error.to_string())
                        .and_then(|_| persist(&config_path, state.config()))
                };
                if result.is_ok() {
                    pending_node_config = None;
                    sessions.clear();
                    sync_workers.clear();
                    central = connect_central(&mut state).await;
                    reconnect_ticks = repeating_interval(state.config().reconnect_interval_seconds);
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
    connect_missing(state, sessions).await;
}

/// 并行连接当前配置中尚无活动会话的节点，并返回本轮新建会话索引。
///
/// 失败节点只更新视图，保留在配置中等待下一次固定间隔重连。
async fn connect_missing(
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
) -> Vec<usize> {
    connect_missing_except(state, sessions, None).await
}

async fn connect_missing_except(
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    excluded_endpoint: Option<&NodeEndpoint>,
) -> Vec<usize> {
    let endpoints = state
        .nodes()
        .iter()
        .enumerate()
        .filter(|(index, node)| {
            !sessions.contains_key(index) && excluded_endpoint != Some(&node.endpoint)
        })
        .map(|(index, node)| (index, node.endpoint.clone()))
        .collect::<Vec<_>>();
    for (index, _) in &endpoints {
        state.set_node_connection(*index, NodeConnectionState::Connecting, None);
    }
    let mut attempts = tokio::task::JoinSet::new();
    for (index, endpoint) in endpoints {
        attempts.spawn(async move { (index, NodeSession::connect(endpoint).await) });
    }
    let mut connected = Vec::new();
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
                        connected.push(index);
                    }
                    Err(error) => state.set_node_error(index, error.to_string()),
                }
            }
            Err(error) => state.set_node_error(index, error.to_string()),
        }
    }
    connected
}

#[derive(Default)]
/// 一次状态刷新发现的断线会话和首次进入完成态的任务所属节点。
struct RefreshReport {
    /// 状态请求失败、必须丢弃旧 TCP 会话的节点索引。
    failed_sessions: Vec<usize>,
    /// 本轮首次观察到任务完成、应立即走自动同步路径的节点索引。
    completed_nodes: Vec<usize>,
    /// 中心 cursor 查询失败；控制器必须丢弃旧 PG client，等待下一次重连。
    central_error: Option<String>,
}

/// 查询所有活动会话的节点状态与持久任务，并归并进唯一桌面视图。
async fn refresh_nodes(
    state: &mut DesktopViewState,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
) -> RefreshReport {
    let mut report = RefreshReport::default();
    for (index, session) in sessions {
        match session.status().await {
            Ok(status) => {
                let sync = if let Some(central) = central {
                    match central.sync_cursor(session.machine_id()).await {
                        Ok(cursor) => cursor,
                        Err(error) => {
                            report.central_error.get_or_insert(error.to_string());
                            0
                        }
                    }
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
                            let became_completed = task.state == ViewTaskState::Completed
                                && !state.tasks().iter().any(|current| {
                                    current.task_id == task.task_id
                                        && current.state == ViewTaskState::Completed
                                });
                            state.upsert_task(task);
                            if became_completed && !report.completed_nodes.contains(index) {
                                report.completed_nodes.push(*index);
                            }
                        }
                    }
                }
            }
            Err(error) => {
                state.set_node_error(*index, error.to_string());
                report.failed_sessions.push(*index);
            }
        }
    }
    report
}

#[derive(Clone, Copy)]
/// 自动同步的三个固定来源；它们与手动操作进入同一节点级通道。
enum AutomaticSyncCause {
    Connected,
    TaskCompleted,
    CatchUp,
}

/// 为在线节点创建彼此独立的后台同步循环；每个循环独占自己的 PG 连接。
fn ensure_sync_workers(
    indexes: &[usize],
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    postgres_url: Option<&str>,
    workers: &mut BTreeMap<usize, NodeSyncWorker>,
    results: &mpsc::UnboundedSender<NodeSyncEvent>,
) {
    let Some(postgres_url) = postgres_url else {
        return;
    };
    for index in indexes {
        if workers.contains_key(index) {
            continue;
        }
        let Some(session) = sessions.get(index) else {
            continue;
        };
        let worker_id = Uuid::now_v7();
        let (triggers, receiver) = sync_trigger_channel(8);
        let task = tokio::spawn(run_node_sync_worker(
            *index,
            worker_id,
            Arc::clone(session),
            postgres_url.to_owned(),
            receiver,
            results.clone(),
        ));
        workers.insert(
            *index,
            NodeSyncWorker {
                id: worker_id,
                triggers,
                task,
            },
        );
    }
}

/// 节点后台循环顺序消费统一触发通道；PG 失败只丢弃本循环连接并等待下一触发重建。
async fn run_node_sync_worker(
    index: usize,
    worker_id: Uuid,
    session: Arc<NodeSession>,
    postgres_url: String,
    mut triggers: SyncTriggerReceiver,
    results: mpsc::UnboundedSender<NodeSyncEvent>,
) {
    let engine = SyncEngine::new();
    let machine_id = session.machine_id().as_str().to_owned();
    let mut central = None;
    while let Some(trigger) = triggers.next().await {
        if central.is_none() {
            match CentralStore::connect(&postgres_url).await {
                Ok(store) => central = Some(store),
                Err(error) => {
                    let _ = results.send(NodeSyncEvent {
                        index,
                        worker_id,
                        machine_id: machine_id.clone(),
                        outcome: NodeSyncOutcome::Failed {
                            message: error.to_string(),
                            session_failed: false,
                            central_failed: true,
                        },
                    });
                    continue;
                }
            }
        }
        let outcome = engine
            .sync_node(
                session.as_ref(),
                central.as_mut().expect("PG 连接已经建立"),
                trigger,
            )
            .await;
        match outcome {
            Ok(report) => match session.status().await {
                Ok(status) => {
                    let _ = results.send(NodeSyncEvent {
                        index,
                        worker_id,
                        machine_id: machine_id.clone(),
                        outcome: NodeSyncOutcome::Succeeded {
                            committed_seq: report.committed_seq,
                            status,
                        },
                    });
                }
                Err(error) => {
                    let _ = results.send(NodeSyncEvent {
                        index,
                        worker_id,
                        machine_id: machine_id.clone(),
                        outcome: NodeSyncOutcome::Failed {
                            message: error.to_string(),
                            session_failed: true,
                            central_failed: false,
                        },
                    });
                    break;
                }
            },
            Err(error) => {
                let session_failed = matches!(&error, SyncError::Session(_));
                let central_failed = matches!(&error, SyncError::Central(_));
                let _ = results.send(NodeSyncEvent {
                    index,
                    worker_id,
                    machine_id: machine_id.clone(),
                    outcome: NodeSyncOutcome::Failed {
                        message: error.to_string(),
                        session_failed,
                        central_failed,
                    },
                });
                if central_failed {
                    central = None;
                }
                if session_failed {
                    break;
                }
            }
        }
    }
}

/// 把连接、任务完成或五秒 tick 放入每个节点自己的同一有界触发通道。
async fn queue_automatic(
    indexes: &[usize],
    workers: &BTreeMap<usize, NodeSyncWorker>,
    cause: AutomaticSyncCause,
) {
    for index in indexes {
        let Some(worker) = workers.get(index) else {
            continue;
        };
        let _ = match cause {
            AutomaticSyncCause::Connected => worker.triggers.connected().await,
            AutomaticSyncCause::TaskCompleted => worker.triggers.task_completed().await,
            AutomaticSyncCause::CatchUp => worker.triggers.catch_up_tick().await,
        };
    }
}

/// 手动按钮只排入节点现有的同步通道，不在控制循环内执行网络或 PG IO。
async fn queue_manual(
    index: usize,
    workers: &BTreeMap<usize, NodeSyncWorker>,
) -> Result<(), String> {
    workers
        .get(&index)
        .ok_or_else(|| "节点未连接或 PostgreSQL 中心模式未启用".to_owned())?
        .triggers
        .manual()
        .await
        .map_err(|error| error.to_string())
}

/// 应用一次刷新结果，并关闭已经确定失效的节点会话或中心连接。
fn apply_refresh_report(
    report: RefreshReport,
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    central: &mut Option<CentralStore>,
    workers: &mut BTreeMap<usize, NodeSyncWorker>,
) {
    for index in report.failed_sessions {
        sessions.remove(&index);
        workers.remove(&index);
    }
    if let Some(error) = report.central_error {
        central.take();
        state.set_postgres_health(PostgresHealth::Error(error));
    }
}

/// 把后台同步结果归并回唯一视图；过期机器结果不能写入已编辑的节点行。
async fn apply_sync_result(
    result: NodeSyncEvent,
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    central: &mut Option<CentralStore>,
    workers: &mut BTreeMap<usize, NodeSyncWorker>,
    events: &mpsc::Sender<UiEvent>,
) {
    if workers.get(&result.index).map(|worker| worker.id) != Some(result.worker_id) {
        return;
    }
    let current_machine = sessions
        .get(&result.index)
        .map(|session| session.machine_id().as_str());
    if current_machine != Some(result.machine_id.as_str()) {
        return;
    }
    match result.outcome {
        NodeSyncOutcome::Succeeded {
            committed_seq,
            status,
        } => state.set_node_connection(
            result.index,
            NodeConnectionState::Online,
            Some(runtime_stats(status, committed_seq)),
        ),
        NodeSyncOutcome::Failed {
            message,
            session_failed,
            central_failed,
        } => {
            if central_failed {
                central.take();
                state.set_postgres_health(PostgresHealth::Error(message.clone()));
            }
            if session_failed {
                sessions.remove(&result.index);
                workers.remove(&result.index);
                state.set_node_error(result.index, message.clone());
            }
            let _ = events
                .send(UiEvent::Error(format!(
                    "节点 {} 自动同步失败：{message}",
                    result.index
                )))
                .await;
        }
    }
}

/// 固定重连 tick：清除已断会话、重试节点/中心，并给新连接或刚完成任务排队。
async fn reconnect_and_sync(
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    central: &mut Option<CentralStore>,
    workers: &mut BTreeMap<usize, NodeSyncWorker>,
    results: &mpsc::UnboundedSender<NodeSyncEvent>,
    excluded_endpoint: Option<&NodeEndpoint>,
) {
    let report = refresh_nodes(state, sessions, central.as_ref()).await;
    let completed_nodes = report.completed_nodes.clone();
    apply_refresh_report(report, state, sessions, central, workers);
    if central.is_none() && state.config().postgres_url.is_some() {
        *central = connect_central(state).await;
    }
    let connected_nodes = connect_missing_except(state, sessions, excluded_endpoint).await;
    let mut worker_indexes = connected_nodes.clone();
    worker_indexes.extend(completed_nodes.iter().copied());
    ensure_sync_workers(
        &worker_indexes,
        sessions,
        state.config().postgres_url.as_deref(),
        workers,
        results,
    );
    queue_automatic(&connected_nodes, workers, AutomaticSyncCause::Connected).await;
    queue_automatic(&completed_nodes, workers, AutomaticSyncCause::TaskCompleted).await;
}

/// 固定五秒追赶 tick：刷新任务后只排队，不等待任一节点完成同步。
async fn catch_up_and_refresh(
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    central: &mut Option<CentralStore>,
    workers: &mut BTreeMap<usize, NodeSyncWorker>,
    results: &mpsc::UnboundedSender<NodeSyncEvent>,
) {
    let report = refresh_nodes(state, sessions, central.as_ref()).await;
    let completed_nodes = report.completed_nodes.clone();
    apply_refresh_report(report, state, sessions, central, workers);
    let indexes = sessions.keys().copied().collect::<Vec<_>>();
    ensure_sync_workers(
        &indexes,
        sessions,
        state.config().postgres_url.as_deref(),
        workers,
        results,
    );
    queue_automatic(&completed_nodes, workers, AutomaticSyncCause::TaskCompleted).await;
    let catch_up_nodes = indexes
        .into_iter()
        .filter(|index| !completed_nodes.contains(index))
        .collect::<Vec<_>>();
    queue_automatic(&catch_up_nodes, workers, AutomaticSyncCause::CatchUp).await;
}

/// 创建错过 tick 后从当前时刻继续计时的固定间隔，避免恢复时突发补跑。
fn repeating_interval(seconds: u64) -> Interval {
    let mut ticks = interval(Duration::from_secs(seconds));
    ticks.set_missed_tick_behavior(MissedTickBehavior::Delay);
    ticks
}

async fn load_node_config(
    node_index: usize,
    state: &mut DesktopViewState,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let session = sessions
        .get(&node_index)
        .ok_or_else(|| format!("节点 {node_index} 未连接，无法加载远程配置"))?;
    let machine_id = session.machine_id().as_str().to_owned();
    let endpoint = session.endpoint().clone();
    let snapshot = session
        .get_node_config()
        .await
        .map_err(|error| error.to_string())?;
    if snapshot.machine_id != machine_id {
        return Err(format!(
            "远程配置归属机器不匹配：握手 {machine_id}，快照 {}",
            snapshot.machine_id
        ));
    }
    state.set_node_config_snapshot(node_index, endpoint, machine_id, snapshot);
    publish_node_config(events, state).await;
    Ok(())
}

async fn save_node_config_and_restart(
    node_index: usize,
    config: proto::NodeConfigValue,
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    sync_workers: &mut BTreeMap<usize, NodeSyncWorker>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<PendingNodeConfigSave, String> {
    let loaded = state.node_config();
    if loaded.selected_node_index() != Some(node_index) {
        return Err("保存目标与已加载远程配置不是同一节点".into());
    }
    let expected_version_sha256 = loaded
        .snapshot()
        .ok_or_else(|| "请先加载远程 Node 配置".to_owned())?
        .version_sha256
        .clone();
    let loaded_machine_id = loaded
        .target_machine_id()
        .ok_or_else(|| "已加载配置缺少目标机器 ID".to_owned())?
        .to_owned();
    let loaded_endpoint = loaded
        .target_endpoint()
        .ok_or_else(|| "已加载配置缺少目标 endpoint".to_owned())?
        .clone();
    let session = sessions
        .get(&node_index)
        .cloned()
        .ok_or_else(|| format!("节点 {node_index} 未连接，无法保存远程配置"))?;
    if session.machine_id().as_str() != loaded_machine_id || session.endpoint() != &loaded_endpoint
    {
        return Err(format!(
            "节点会话已变化：加载目标 {} / {}:{}，当前会话 {} / {}:{}",
            loaded_machine_id,
            loaded_endpoint.ip,
            loaded_endpoint.port,
            session.machine_id().as_str(),
            session.endpoint().ip,
            session.endpoint().port
        ));
    }
    let machine_id = loaded_machine_id;
    let endpoint = loaded_endpoint;

    state.set_node_config_phase(NodeConfigSavePhase::Validating);
    publish_node_config(events, state).await;
    NodeConfig::try_from(config.clone()).map_err(|error| error.to_string())?;

    state.set_node_config_phase(NodeConfigSavePhase::Saving);
    publish_node_config(events, state).await;
    let accepted = session
        .save_node_config_and_restart(&expected_version_sha256, config)
        .await
        .map_err(|error| error.to_string())?;
    if accepted.machine_id != machine_id {
        return Err(format!(
            "保存响应机器不匹配：目标 {machine_id}，响应 {}",
            accepted.machine_id
        ));
    }
    if accepted.saved_version_sha256.is_empty() {
        return Err("保存响应缺少新配置摘要".into());
    }

    state.set_node_config_save_target(
        machine_id.clone(),
        endpoint.clone(),
        accepted.saved_version_sha256.clone(),
    );
    state.set_node_config_phase(NodeConfigSavePhase::Restarting);
    publish_node_config(events, state).await;

    sessions.remove(&node_index);
    sync_workers.remove(&node_index);
    state.set_node_connection(node_index, NodeConnectionState::Offline, None);
    state.set_node_config_phase(NodeConfigSavePhase::WaitingForReconnect);
    publish_node_config(events, state).await;

    let timeout_seconds = state
        .config()
        .reconnect_interval_seconds
        .saturating_mul(NODE_CONFIG_RECONNECT_ATTEMPTS)
        .max(1);
    Ok(PendingNodeConfigSave {
        machine_id,
        endpoint,
        saved_version_sha256: accepted.saved_version_sha256,
        deadline: Instant::now() + Duration::from_secs(timeout_seconds),
    })
}

async fn verify_reconnected_node_config(
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    sync_workers: &mut BTreeMap<usize, NodeSyncWorker>,
    pending: &mut Option<PendingNodeConfigSave>,
    events: &mpsc::Sender<UiEvent>,
    sync_results: &mpsc::UnboundedSender<NodeSyncEvent>,
) {
    let Some(target) = pending.clone() else {
        return;
    };
    if Instant::now() >= target.deadline {
        state.fail_node_config(node_config_timeout_message(&target));
        pending.take();
        publish_node_config(events, state).await;
        return;
    }

    let existing = sessions
        .iter()
        .find(|(_, session)| session.endpoint() == &target.endpoint)
        .map(|(index, session)| (*index, Arc::clone(session)));
    let (index, session) = if let Some(candidate) = existing {
        candidate
    } else {
        let session = match NodeSession::connect(target.endpoint.clone()).await {
            Ok(session) => Arc::new(session),
            Err(_) => {
                if Instant::now() >= target.deadline {
                    state.fail_node_config(node_config_timeout_message(&target));
                    pending.take();
                    publish_node_config(events, state).await;
                }
                return;
            }
        };
        if Instant::now() >= target.deadline {
            state.fail_node_config(node_config_timeout_message(&target));
            pending.take();
            publish_node_config(events, state).await;
            return;
        }
        let Some(index) = state
            .nodes()
            .iter()
            .position(|node| node.endpoint == target.endpoint)
        else {
            state.fail_node_config("等待重连期间目标 endpoint 已从配置移除");
            pending.take();
            publish_node_config(events, state).await;
            return;
        };
        (index, session)
    };

    if session.machine_id().as_str() != target.machine_id {
        let message = format!(
            "重连机器不匹配：目标 {}，实际 {}",
            target.machine_id,
            session.machine_id().as_str()
        );
        sessions.remove(&index);
        sync_workers.remove(&index);
        state.set_node_error(index, message.clone());
        state.fail_node_config(message);
        pending.take();
        publish_node_config(events, state).await;
        return;
    }

    state.set_node_config_phase(NodeConfigSavePhase::Verifying);
    publish_node_config(events, state).await;
    let result = session.get_node_config().await;
    match result {
        Ok(snapshot)
            if snapshot.machine_id == target.machine_id
                && snapshot.version_sha256 == target.saved_version_sha256 =>
        {
            state.set_node_identity(index, target.machine_id.clone());
            state.set_node_connection(index, NodeConnectionState::Online, None);
            sessions.insert(index, Arc::clone(&session));
            state.complete_node_config(snapshot);
            ensure_sync_workers(
                &[index],
                sessions,
                state.config().postgres_url.as_deref(),
                sync_workers,
                sync_results,
            );
            queue_automatic(&[index], sync_workers, AutomaticSyncCause::Connected).await;
        }
        Ok(snapshot) => {
            let message = format!(
                "重连配置验证失败：目标机器 {} / 摘要 {}，实际机器 {} / 摘要 {}",
                target.machine_id,
                target.saved_version_sha256,
                snapshot.machine_id,
                snapshot.version_sha256
            );
            state.set_node_error(index, message.clone());
            state.fail_node_config(message);
        }
        Err(error) => {
            let message = format!("重连后加载 Node 配置失败：{error}");
            state.set_node_error(index, message.clone());
            state.fail_node_config(message);
        }
    }
    pending.take();
    publish_node_config(events, state).await;
}

fn node_config_timeout_message(target: &PendingNodeConfigSave) -> String {
    format!(
        "等待机器 {} 在 {}:{} 重连超时",
        target.machine_id, target.endpoint.ip, target.endpoint.port
    )
}

fn node_config_target_change_error() -> String {
    "远程 Node 配置重启验证进行中，不能切换、编辑或移除目标节点".into()
}

async fn publish_node_config(events: &mpsc::Sender<UiEvent>, state: &DesktopViewState) {
    let _ = events
        .send(UiEvent::NodeConfigChanged(state.node_config().clone()))
        .await;
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

async fn start_local_analysis(
    node_index: usize,
    task_text: &str,
    kind: GroupKind,
    config: &DesktopConfig,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let session = sessions
        .get(&node_index)
        .ok_or_else(|| "节点当前未连接".to_owned())?;
    let tasks = parse_task_ids(task_text)?;
    let run_id = session
        .create_local_analysis(&tasks, wire_group_kind(kind), &config.thresholds)
        .await
        .map_err(|error| error.to_string())?;
    let run = session
        .query_analysis_run(run_id)
        .await
        .map_err(|error| error.to_string())?;
    send_event(
        events,
        UiEvent::AnalysisStarted {
            central: false,
            run_id: run.analysis_run_id,
            status: run.state,
        },
    )
    .await
}

async fn start_cross_analysis(
    text: &str,
    config: &DesktopConfig,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&mut CentralStore>,
    current: &mut Option<CrossAnalysisCoordinator>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let parsed = parse_cross_selections(text)?;
    let mut selected = Vec::with_capacity(parsed.len());
    for (index, task_id) in &parsed {
        let session = sessions
            .get(index)
            .ok_or_else(|| format!("节点索引 {index} 当前未连接"))?;
        selected.push(CrossNodeSelection::new(session.as_ref(), *task_id));
    }
    let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
    let coordinator = CrossAnalysisCoordinator::start(central, &selected, config.thresholds)
        .await
        .map_err(|error| error.to_string())?;
    let run_id = coordinator.run_id();
    *current = Some(coordinator);
    send_event(
        events,
        UiEvent::AnalysisStarted {
            central: true,
            run_id: run_id.as_uuid().to_string(),
            status: "collecting_stage1".into(),
        },
    )
    .await
}

async fn poll_cross_analysis(
    retry: bool,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&mut CentralStore>,
    current: Option<&mut CrossAnalysisCoordinator>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let coordinator = current.ok_or_else(|| "当前进程尚未创建跨机器分析".to_owned())?;
    let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
    let online = sessions
        .values()
        .map(Arc::as_ref)
        .collect::<Vec<&NodeSession>>();
    let report = if retry {
        coordinator.retry_unresolved(central, &online).await
    } else {
        coordinator.poll(central, &online).await
    }
    .map_err(|error| error.to_string())?;
    send_event(events, UiEvent::CrossAnalysisChanged(report)).await
}

#[allow(clippy::too_many_arguments)]
async fn load_groups(
    use_central: bool,
    node_index: usize,
    run_text: &str,
    kind: GroupKind,
    cursor: &str,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let run_id = parse_analysis_id(run_text)?;
    let page = if use_central {
        let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
        let mut page = group_page_from_central(
            central
                .page_groups(run_id, non_empty(cursor), 100)
                .await
                .map_err(|error| error.to_string())?,
        );
        page.items.retain(|group| group.kind == kind);
        page
    } else {
        let session = sessions
            .get(&node_index)
            .ok_or_else(|| "节点当前未连接".to_owned())?;
        group_page_from_node(
            session
                .list_groups(run_id, wire_group_kind(kind), cursor, 100)
                .await
                .map_err(|error| error.to_string())?,
        )
        .map_err(|error| error.to_string())?
    };
    send_event(events, UiEvent::GroupsChanged(Box::new(page))).await
}

#[allow(clippy::too_many_arguments)]
async fn load_members(
    use_central: bool,
    node_index: usize,
    run_text: &str,
    group_id: &str,
    kind: GroupKind,
    cursor: &str,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
) -> Result<LoadedMembersContext, String> {
    let run_id = parse_analysis_id(run_text)?;
    let (scope, page) = if use_central {
        let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
        let online = sessions
            .values()
            .map(|session| session.machine_id().clone())
            .collect::<std::collections::BTreeSet<_>>();
        let page = central
            .page_group_members(run_id, group_id, non_empty(cursor), 200)
            .await
            .map_err(|error| error.to_string())?;
        (
            ResultScope::Central { run_id },
            member_page_from_central(page, |machine| online.contains(machine)),
        )
    } else {
        let session = sessions
            .get(&node_index)
            .ok_or_else(|| "节点当前未连接".to_owned())?;
        let page = session
            .list_group_members(run_id, group_id, cursor, 200)
            .await
            .map_err(|error| error.to_string())?;
        (
            ResultScope::Local { node_index, run_id },
            member_page_from_node(page, true).map_err(|error| error.to_string())?,
        )
    };
    Ok(LoadedMembersContext {
        scope,
        kind,
        group_id: group_id.into(),
        items: page.items,
        next_cursor: page.next_cursor,
    })
}

#[allow(clippy::too_many_arguments)]
async fn save_one_review(
    machine: &str,
    path: &str,
    decision: ReviewDecision,
    context: Option<&mut LoadedMembersContext>,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
    let location = location(machine, path)?;
    if !context.items.iter().any(|item| item.location == location) {
        return Err("复核位置不在当前组窗口中".into());
    }
    persist_review(
        context.scope,
        &context.group_id,
        &location,
        decision,
        sessions,
        central,
    )
    .await?;
    if let Some(member) = context
        .items
        .iter_mut()
        .find(|member| member.location == location)
    {
        member.review = decision;
    }
    publish_members(events, context).await
}

async fn apply_quick_review(
    rule: QuickReviewRule,
    context: Option<&mut LoadedMembersContext>,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
    let mut board = ReviewBoard::from_members(&context.items);
    let changes = board.apply_quick_rule(&context.items, rule);
    if changes.is_empty() {
        return Err("当前组没有满足快捷规则的活动成员".into());
    }
    for change in &changes {
        persist_review(
            context.scope,
            &context.group_id,
            &change.location,
            change.decision,
            sessions,
            central,
        )
        .await?;
    }
    for member in &mut context.items {
        member.review = board.decision(&member.location);
    }
    publish_members(events, context).await
}

async fn persist_review(
    scope: ResultScope,
    group_id: &str,
    location: &LocationKey,
    decision: ReviewDecision,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
) -> Result<(), String> {
    match scope {
        ResultScope::Local { node_index, run_id } => sessions
            .get(&node_index)
            .ok_or_else(|| "节点当前未连接".to_owned())?
            .save_review_mark(run_id, group_id, location, wire_review(decision))
            .await
            .map_err(|error| error.to_string()),
        ResultScope::Central { run_id } => central
            .ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?
            .save_review_mark(run_id, group_id, location, central_review(decision))
            .await
            .map_err(|error| error.to_string()),
    }
}

async fn load_member_preview(
    machine: &str,
    path: &str,
    context: Option<&LoadedMembersContext>,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let result: Result<_, String> = async {
        let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
        let location = location(machine, path)?;
        let member = context
            .items
            .iter()
            .find(|member| member.location == location)
            .ok_or_else(|| "预览位置不在当前组窗口中".to_owned())?;
        if !member.actions.preview {
            return Err("成员离线或已经失活，不能预览".into());
        }
        let session = session_by_machine(sessions, location.machine_id())
            .ok_or_else(|| "成员所在节点当前未连接".to_owned())?;
        let preview = load_preview(session, &location, context.kind)
            .await
            .map_err(|error| error.to_string())?;
        Ok((member.display_path.clone(), preview))
    }
    .await;
    let event = match result {
        Ok((display_path, preview)) => UiEvent::PreviewReady {
            machine_id: machine.to_owned(),
            normalized_path: path.to_owned(),
            display_path,
            file_kind: preview.file_kind.into(),
            bytes: Arc::from(preview.bytes),
        },
        Err(error) => UiEvent::PreviewFailed {
            machine_id: machine.to_owned(),
            normalized_path: path.to_owned(),
            error,
        },
    };
    send_event(events, event).await
}

async fn prepare_delete(
    mode: DeleteMode,
    context: Option<&LoadedMembersContext>,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
    prepared: &mut Option<PreparedDeleteContext>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
    let group = load_complete_review_group(context, sessions, central).await?;
    let confirmation = DeleteConfirmation::from_groups(mode, std::slice::from_ref(&group));
    let items = group
        .members
        .into_iter()
        .filter(|member| member.active && member.review == ReviewDecision::Delete)
        .collect();
    *prepared = Some(PreparedDeleteContext {
        scope: context.scope,
        group_id: context.group_id.clone(),
        confirmation: confirmation.clone(),
        items,
    });
    send_event(events, UiEvent::DeleteConfirmationChanged(confirmation)).await
}

async fn load_complete_review_group(
    context: &LoadedMembersContext,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
) -> Result<ReviewGroup, String> {
    let mut members = Vec::new();
    let mut cursor = None::<String>;
    let mut seen = std::collections::BTreeSet::new();
    loop {
        let page = match context.scope {
            ResultScope::Local { node_index, run_id } => {
                let session = sessions
                    .get(&node_index)
                    .ok_or_else(|| "节点当前未连接".to_owned())?;
                member_page_from_node(
                    session
                        .list_group_members(
                            run_id,
                            &context.group_id,
                            cursor.as_deref().unwrap_or_default(),
                            200,
                        )
                        .await
                        .map_err(|error| error.to_string())?,
                    true,
                )
                .map_err(|error| error.to_string())?
            }
            ResultScope::Central { run_id } => {
                let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
                let online = sessions
                    .values()
                    .map(|session| session.machine_id().clone())
                    .collect::<std::collections::BTreeSet<_>>();
                member_page_from_central(
                    central
                        .page_group_members(run_id, &context.group_id, cursor.as_deref(), 200)
                        .await
                        .map_err(|error| error.to_string())?,
                    |machine| online.contains(machine),
                )
            }
        };
        members.extend(page.items);
        let Some(next) = page.next_cursor else {
            break;
        };
        if !seen.insert(next.clone()) {
            return Err("组成员分页游标没有前进".into());
        }
        cursor = Some(next);
    }
    Ok(ReviewGroup::new(&context.group_id, members))
}

async fn confirm_delete(
    prepared: Option<&PreparedDeleteContext>,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&mut CentralStore>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let prepared = prepared.ok_or_else(|| "请先生成并检查删除确认摘要".to_owned())?;
    if !prepared.confirmation.can_execute {
        return Err("删除确认门禁未通过".into());
    }
    let summary = match prepared.scope {
        ResultScope::Local { node_index, run_id } => {
            let items = prepared
                .items
                .iter()
                .map(|member| proto::DeleteItem {
                    delete_item_id: String::new(),
                    group_id: prepared.group_id.clone(),
                    location: Some((&member.location).into()),
                    expected_content: Some((&member.content).into()),
                    outcome: String::new(),
                    message: String::new(),
                })
                .collect();
            let batch = sessions
                .get(&node_index)
                .ok_or_else(|| "节点当前未连接".to_owned())?
                .create_delete_batch(run_id, items, prepared.confirmation.mode)
                .await
                .map_err(|error| error.to_string())?;
            summarize_delete_items(&batch.items)
        }
        ResultScope::Central { run_id } => {
            execute_central_delete(
                run_id,
                &prepared.group_id,
                &prepared.items,
                prepared.confirmation.mode,
                sessions,
                central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?,
            )
            .await?
        }
    };
    send_event(events, UiEvent::DeleteFinished(summary)).await
}

async fn execute_central_delete(
    run_id: AnalysisRunId,
    group_id: &str,
    confirmed: &[MemberView],
    mode: DeleteMode,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: &mut CentralStore,
) -> Result<String, String> {
    let selected = confirmed
        .iter()
        .map(|member| {
            CentralDeleteSelection::new(
                group_id.to_owned(),
                member.location.clone(),
                member.content,
            )
        })
        .collect::<Vec<_>>();
    let plan = central
        .create_delete_plan(run_id, &selected, mode)
        .await
        .map_err(|error| error.to_string())?;
    let mut by_machine = BTreeMap::<MachineId, Vec<_>>::new();
    for item in &plan.items {
        by_machine
            .entry(item.location.machine_id().clone())
            .or_default()
            .push(item);
    }
    let mut all_items = Vec::new();
    for (machine, items) in by_machine {
        let session = session_by_machine(sessions, &machine)
            .ok_or_else(|| format!("节点 {} 当前未连接", machine.as_str()))?;
        let wire = items
            .into_iter()
            .map(|item| proto::DeleteItem {
                delete_item_id: item.item_id.clone(),
                group_id: item.group_id.clone(),
                location: Some((&item.location).into()),
                expected_content: Some((&item.expected).into()),
                outcome: String::new(),
                message: String::new(),
            })
            .collect();
        let response = session
            .execute_central_delete_batch(&plan.batch_id, wire, mode)
            .await
            .map_err(|error| error.to_string())?;
        let results = response
            .items
            .iter()
            .map(central_delete_result)
            .collect::<Result<Vec<_>, String>>()?;
        central
            .apply_delete_results(&plan.batch_id, &results)
            .await
            .map_err(|error| error.to_string())?;
        all_items.extend(response.items);
    }
    Ok(summarize_delete_items(&all_items))
}

fn central_delete_result(item: &proto::DeleteItem) -> Result<CentralDeleteResult, String> {
    let outcome = match item.outcome.as_str() {
        "recycled" => CentralDeleteOutcome::Recycled,
        "deleted" => CentralDeleteOutcome::Deleted,
        "skipped" => CentralDeleteOutcome::Skipped,
        "failed" => CentralDeleteOutcome::Failed,
        _ => return Err("节点返回未知删除结果".into()),
    };
    Ok(CentralDeleteResult {
        item_id: item.delete_item_id.clone(),
        outcome,
        message: (!item.message.is_empty()).then(|| item.message.clone()),
    })
}

fn summarize_delete_items(items: &[proto::DeleteItem]) -> String {
    let success = items
        .iter()
        .filter(|item| matches!(item.outcome.as_str(), "recycled" | "deleted"))
        .count();
    let skipped = items
        .iter()
        .filter(|item| item.outcome == "skipped")
        .count();
    let failed = items.iter().filter(|item| item.outcome == "failed").count();
    format!("删除完成：成功 {success}，跳过 {skipped}，失败 {failed}")
}

async fn publish_members(
    events: &mpsc::Sender<UiEvent>,
    context: &LoadedMembersContext,
) -> Result<(), String> {
    send_event(
        events,
        UiEvent::ReviewChanged(Box::new(MemberPage {
            items: context.items.clone(),
            next_cursor: context.next_cursor.clone(),
        })),
    )
    .await
}

async fn send_event(events: &mpsc::Sender<UiEvent>, event: UiEvent) -> Result<(), String> {
    events
        .send(event)
        .await
        .map_err(|_| "UI 事件通道已经关闭".to_owned())
}

fn parse_task_ids(text: &str) -> Result<Vec<TaskId>, String> {
    let tasks = text
        .split([',', ';', '\n'])
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(|value| {
            Uuid::parse_str(value)
                .map(TaskId::from_uuid)
                .map_err(|_| format!("任务 ID 不是 UUID: {value}"))
        })
        .collect::<Result<Vec<_>, _>>()?;
    if tasks.is_empty() {
        Err("至少填写一个扫描任务 ID".into())
    } else {
        Ok(tasks)
    }
}

fn parse_cross_selections(text: &str) -> Result<Vec<(usize, TaskId)>, String> {
    let selections = text
        .split([',', ';', '\n'])
        .map(str::trim)
        .filter(|value| !value.is_empty())
        .map(|value| {
            let (index, task) = value
                .split_once(':')
                .ok_or_else(|| format!("跨机器选择格式应为 节点索引:任务ID: {value}"))?;
            Ok((
                index
                    .parse::<usize>()
                    .map_err(|_| format!("节点索引无效: {index}"))?,
                Uuid::parse_str(task)
                    .map(TaskId::from_uuid)
                    .map_err(|_| format!("任务 ID 不是 UUID: {task}"))?,
            ))
        })
        .collect::<Result<Vec<_>, String>>()?;
    if selections.is_empty() {
        Err("至少填写一个节点任务选择".into())
    } else {
        Ok(selections)
    }
}

fn parse_analysis_id(value: &str) -> Result<AnalysisRunId, String> {
    Uuid::parse_str(value.trim())
        .map(AnalysisRunId::from_uuid)
        .map_err(|_| "分析运行 ID 不是 UUID".into())
}

fn location(machine: &str, path: &str) -> Result<LocationKey, String> {
    Ok(LocationKey::new(
        MachineId::parse(machine).map_err(|error| error.to_string())?,
        NormalizedPath::new(path).map_err(|error| error.to_string())?,
    ))
}

fn session_by_machine<'a>(
    sessions: &'a BTreeMap<usize, Arc<NodeSession>>,
    machine: &MachineId,
) -> Option<&'a NodeSession> {
    sessions
        .values()
        .find(|session| session.machine_id() == machine)
        .map(Arc::as_ref)
}

const fn wire_group_kind(kind: GroupKind) -> proto::GroupKind {
    match kind {
        GroupKind::Exact => proto::GroupKind::GroupExact,
        GroupKind::SimilarImage => proto::GroupKind::GroupSimilarImage,
        GroupKind::SimilarVideo => proto::GroupKind::GroupSimilarVideo,
    }
}

const fn wire_review(decision: ReviewDecision) -> proto::ReviewDecision {
    match decision {
        ReviewDecision::Undecided => proto::ReviewDecision::ReviewUndecided,
        ReviewDecision::Keep => proto::ReviewDecision::ReviewKeep,
        ReviewDecision::Delete => proto::ReviewDecision::ReviewDelete,
    }
}

const fn central_review(decision: ReviewDecision) -> CentralReviewDecision {
    match decision {
        ReviewDecision::Undecided => CentralReviewDecision::Undecided,
        ReviewDecision::Keep => CentralReviewDecision::Keep,
        ReviewDecision::Delete => CentralReviewDecision::Delete,
    }
}

fn non_empty(value: &str) -> Option<&str> {
    (!value.is_empty()).then_some(value)
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

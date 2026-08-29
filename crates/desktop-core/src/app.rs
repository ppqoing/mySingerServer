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
    NormalizedPath, TaskId,
};
use dedup_protocol::proto;
use tokio::{
    sync::mpsc,
    task::JoinHandle,
    time::{Interval, MissedTickBehavior, interval},
};
use uuid::Uuid;

use crate::{
    analysis::{CrossAnalysisCoordinator, CrossNodeSelection, CrossPollReport},
    central::{
        CentralAnalysisStatus, CentralDeleteOutcome, CentralDeleteResult, CentralDeleteSelection,
        CentralError, CentralReviewDecision, CentralStore, inspect_database,
    },
    delete::{DeleteConfirmation, ReviewGroup},
    node_session::NodeSession,
    results::{
        CentralResultWindowCache, GroupKind, MemberView, ResultScope, ResultWindowRequest,
        ResultWindowState, load_preview,
    },
    review::{QuickReviewRule, ReviewBoard, ReviewDecision},
    runtime_tasks::{
        DesktopRuntimeTaskRegistry, DesktopRuntimeTaskReporter, DesktopRuntimeTaskState,
        RuntimeTaskKey, RuntimeTaskOwner,
    },
    sync::{
        AUTO_CATCH_UP_INTERVAL_SECONDS, SyncEngine, SyncError, SyncTriggerReceiver,
        SyncTriggerSender, sync_trigger_channel,
    },
    view_state::{
        DesktopPaths, DesktopViewState, NodeConfigControllerState, NodeConfigSavePhase,
        NodeConnectionState, NodeRuntimeStats, PostgresHealth, RuntimeTaskControllerState,
        RuntimeTaskDetailsView,
    },
};

const RUNTIME_TASK_REFRESH_SECONDS: u64 = 2;
/// Node 主动运行事件桥的固定有界容量，发送方在满载时等待消费。
const RUNTIME_EVENT_BRIDGE_CAPACITY: usize = 64;

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
    /// 从 `节点索引:扫描任务 UUID` 列表创建跨机器中心分析。
    StartCrossAnalysis {
        /// 逗号分隔的节点索引和任务 ID。
        selections: String,
    },
    /// 推进当前进程创建的跨机器运行直到下一门禁。
    PollCrossAnalysis,
    /// 对当前 Partial 中心运行显式重试未解决二筛项。
    RetryCrossAnalysis,
    /// 请求一个已完成中心分析的有限组窗口；中心游标只留在 Core 内部。
    RequestGroupWindow {
        /// 运行身份与窗口范围。
        request: ResultWindowRequest,
    },
    /// 请求一个已完成中心分析的有限成员窗口；中心游标不进入 UI 契约。
    RequestMemberWindow {
        /// 运行身份、类别和窗口范围。
        request: ResultWindowRequest,
        /// 需要显示的持久组 ID。
        group_id: String,
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
    /// 使用已加载摘要保存远程 Node 配置；新配置在重启 Node 后生效。
    SaveNodeConfig {
        /// 用户发起操作时的节点列表索引。
        node_index: usize,
        /// 设置页提交的完整 wire 配置值。
        config: proto::NodeConfigValue,
    },
    /// 切换任务中心选中项并立即拉取其详情。
    SelectRuntimeTask {
        /// Node/Desktop 共用的稳定运行任务键。
        key: RuntimeTaskKey,
    },
    /// 保存完整配置；校验失败保持旧配置。
    SaveSettings(DesktopConfig),
    /// 使用页面当前未保存的连接串测试 PostgreSQL schema。
    TestDatabaseConnection {
        /// 由 UI 基础字段编码得到的临时连接串。
        url: String,
    },
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
    /// 运行任务摘要、选择、详情或 stale 状态已经改变。
    RuntimeTasksChanged(RuntimeTaskControllerState),
    /// 临时数据库连接测试及固定 schema 校验已经完成。
    DatabaseDiagnosticsChanged(Result<(), String>),
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
    /// 中心分析运行已经创建。
    AnalysisStarted {
        /// 运行来源固定为 PostgreSQL 中心。
        central: bool,
        /// UUID 文本运行 ID。
        run_id: String,
        /// 当前持久状态。
        status: String,
    },
    /// 跨机器运行推进到一个可观察门禁。
    CrossAnalysisChanged(CrossPollReport),
    /// 替换当前中心结果窗口；窗口行从不追加历史页。
    GroupsChanged(Box<ResultWindowState<crate::results::GroupView>>),
    /// 替换当前选中组及其成员页。
    MembersChanged {
        /// 持久组 ID。
        group_id: String,
        /// 当前有限成员窗口。
        window: Box<ResultWindowState<MemberView>>,
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
    /// 快捷或单项复核已更新当前进程投影，返回替换后的成员窗口。
    ReviewChanged(Box<ResultWindowState<MemberView>>),
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
    /// 当前 desktop.exe 进程内唯一的运行任务 registry。
    runtime_tasks: DesktopRuntimeTaskRegistry,
    commands: mpsc::Sender<UiCommand>,
}

impl DesktopApp {
    /// 启动唯一后台控制循环并返回 GUI 命令句柄与事件接收端。
    pub fn start(config: DesktopConfig, paths: DesktopPaths) -> (Self, mpsc::Receiver<UiEvent>) {
        let config_path = paths.config.clone();
        let state = DesktopViewState::new(config, paths);
        let runtime_tasks = DesktopRuntimeTaskRegistry::new();
        let (commands, command_receiver) = mpsc::channel(64);
        let (events, event_receiver) = mpsc::channel(64);
        tokio::spawn(run_controller(
            state,
            config_path,
            command_receiver,
            events,
            runtime_tasks.clone(),
        ));
        (
            Self {
                runtime_tasks,
                commands,
            },
            event_receiver,
        )
    }

    /// 返回当前 Desktop 进程共享的临时运行任务 registry。
    pub fn runtime_tasks(&self) -> DesktopRuntimeTaskRegistry {
        self.runtime_tasks.clone()
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
    /// 当前中心窗口对应的运行身份，便于迟到响应门禁。
    run_id: AnalysisRunId,
    items: Vec<MemberView>,
    /// 当前进程内的复核投影，切换运行或组时整体重建。
    review_board: ReviewBoard,
    /// UI 当前可见窗口边界；不会保存中心游标。
    window: ResultWindowState<MemberView>,
}

struct PreparedDeleteContext {
    scope: ResultScope,
    group_id: String,
    confirmation: DeleteConfirmation,
    items: Vec<MemberView>,
    /// 冻结确认集合对应的唯一删除运行详情。
    runtime: DesktopRuntimeTaskReporter,
}

/// 当前进程正在推进的唯一跨机器分析及其运行详情。
struct ActiveCrossAnalysis {
    /// 既有 PostgreSQL 协调器。
    coordinator: CrossAnalysisCoordinator,
    /// Desktop 临时运行详情句柄。
    runtime: DesktopRuntimeTaskReporter,
    /// 冻结输入涉及的唯一物理节点数。
    node_count: usize,
}

/// 记录结果窗口当前请求，拒绝迟到的旧运行或旧组响应。
#[derive(Clone, Debug, Eq, PartialEq)]
struct ActiveWindowRequest {
    /// 当前组窗口请求。
    groups: Option<ResultWindowRequest>,
    /// 当前成员窗口请求与组 ID。
    members: Option<(String, ResultWindowRequest)>,
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

/// 一个 Node 管理会话对应的唯一运行任务事件监督器。
struct NodeRuntimeWatcher {
    /// 每次新会话生成的代次，阻止旧连接事件覆盖新机器。
    generation: Uuid,
    /// 启动监督器时冻结的握手机器 ID。
    machine_id: String,
    /// 用于确认当前 sessions 仍是同一个连接对象。
    session: Arc<NodeSession>,
    /// 持续等待该连接主动事件的后台任务。
    task: JoinHandle<()>,
}

impl Drop for NodeRuntimeWatcher {
    fn drop(&mut self) {
        self.task.abort();
    }
}

/// Node 运行任务监督器返回给唯一控制循环的带归属结果。
struct NodeRuntimeEvent {
    /// 当前 Desktop 配置中的节点索引。
    node_index: usize,
    /// 监督器代次。
    generation: Uuid,
    /// 监督器启动时冻结的机器 ID。
    machine_id: String,
    /// 主动终态或连接错误。
    outcome: NodeRuntimeEventOutcome,
}

/// Node 主动运行任务事件或事件流终止原因。
enum NodeRuntimeEventOutcome {
    /// Node 推送的终态变化。
    Changed(proto::RuntimeTaskChanged),
    /// 当前管理连接已断开，旧详情必须标记 stale。
    Disconnected(String),
}

async fn run_controller(
    mut state: DesktopViewState,
    config_path: PathBuf,
    mut commands: mpsc::Receiver<UiCommand>,
    events: mpsc::Sender<UiEvent>,
    runtime_tasks: DesktopRuntimeTaskRegistry,
) {
    let mut sessions = BTreeMap::<usize, Arc<NodeSession>>::new();
    let mut central = connect_central(&mut state).await;
    let mut sync_workers = BTreeMap::<usize, NodeSyncWorker>::new();
    let (sync_result_sender, mut sync_results) = mpsc::unbounded_channel();
    let mut runtime_view = RuntimeTaskControllerState::default();
    let mut runtime_watchers = BTreeMap::<usize, NodeRuntimeWatcher>::new();
    let (runtime_event_sender, mut runtime_events) = mpsc::channel(RUNTIME_EVENT_BRIDGE_CAPACITY);
    let mut cross_analysis: Option<ActiveCrossAnalysis> = None;
    let mut loaded_members: Option<LoadedMembersContext> = None;
    let mut prepared_delete: Option<PreparedDeleteContext> = None;
    // 复核决定只保留在当前 Desktop 进程，并按中心运行与组隔离。
    let mut review_boards = BTreeMap::<(AnalysisRunId, String), ReviewBoard>::new();
    let mut result_windows = CentralResultWindowCache::new();
    let mut group_window_state = ResultWindowState::<crate::results::GroupView>::empty();
    let mut member_window_state = ResultWindowState::<MemberView>::empty();
    let mut active_window_request = ActiveWindowRequest {
        groups: None,
        members: None,
    };
    let mut reconnect_ticks = repeating_interval(state.config().reconnect_interval_seconds);
    let mut catch_up_ticks = repeating_interval(AUTO_CATCH_UP_INTERVAL_SECONDS);
    let mut runtime_ticks = runtime_task_interval();
    catch_up_ticks.tick().await;
    runtime_ticks.tick().await;
    // 先发布统一运行任务所有者的空快照，再发布普通视图，避免 UI 启动时出现第二个任务来源。
    publish_runtime_tasks(&events, &runtime_view).await;
    publish(&events, &state).await;
    loop {
        let command = tokio::select! {
            command = commands.recv() => match command {
                Some(command) => command,
                None => break,
            },
            _ = reconnect_ticks.tick() => {
                let result_lost = reconnect_and_sync(
                    &mut state,
                    &mut sessions,
                    &mut central,
                    &mut sync_workers,
                    &sync_result_sender,
                    &runtime_tasks,
                ).await;
                if result_lost {
                    mark_result_windows_stale(
                        &mut group_window_state,
                        &mut member_window_state,
                        &mut loaded_members,
                        &mut prepared_delete,
                        &mut result_windows,
                        &active_window_request,
                        &events,
                    ).await;
                }
                reconcile_and_publish_runtime_tasks(
                    &sessions,
                    &mut runtime_watchers,
                    &runtime_event_sender,
                    &mut runtime_view,
                    &runtime_tasks,
                    &events,
                ).await;
                publish(&events, &state).await;
                continue;
            }
            _ = catch_up_ticks.tick() => {
                let result_lost = catch_up_and_refresh(
                    &mut state,
                    &mut sessions,
                    &mut central,
                    &mut sync_workers,
                    &sync_result_sender,
                    &runtime_tasks,
                ).await;
                if result_lost {
                    mark_result_windows_stale(
                        &mut group_window_state,
                        &mut member_window_state,
                        &mut loaded_members,
                        &mut prepared_delete,
                        &mut result_windows,
                        &active_window_request,
                        &events,
                    ).await;
                }
                reconcile_and_publish_runtime_tasks(
                    &sessions,
                    &mut runtime_watchers,
                    &runtime_event_sender,
                    &mut runtime_view,
                    &runtime_tasks,
                    &events,
                ).await;
                publish(&events, &state).await;
                continue;
            }
            Some(sync_result) = sync_results.recv() => {
                let result_lost = apply_sync_result(
                    sync_result,
                    &mut state,
                    &mut sessions,
                    &mut central,
                    &mut sync_workers,
                    &events,
                ).await;
                if result_lost {
                    mark_result_windows_stale(
                        &mut group_window_state,
                        &mut member_window_state,
                        &mut loaded_members,
                        &mut prepared_delete,
                        &mut result_windows,
                        &active_window_request,
                        &events,
                    ).await;
                }
                reconcile_and_publish_runtime_tasks(
                    &sessions,
                    &mut runtime_watchers,
                    &runtime_event_sender,
                    &mut runtime_view,
                    &runtime_tasks,
                    &events,
                ).await;
                publish(&events, &state).await;
                continue;
            }
            _ = runtime_ticks.tick() => {
                reconcile_runtime_watchers(
                    &sessions,
                    &mut runtime_watchers,
                    &runtime_event_sender,
                    &mut runtime_view,
                );
                refresh_runtime_tasks(&mut runtime_view, &sessions, &runtime_tasks).await;
                publish_runtime_tasks(&events, &runtime_view).await;
                continue;
            }
            Some(runtime_event) = runtime_events.recv() => {
                apply_runtime_event(
                    runtime_event,
                    &mut state,
                    &mut sessions,
                    &mut sync_workers,
                    &mut runtime_watchers,
                    &mut runtime_view,
                    &runtime_tasks,
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
                        &runtime_tasks,
                    );
                    queue_automatic(&indexes, &sync_workers, AutomaticSyncCause::Connected).await;
                    Ok(())
                }
            }
            UiCommand::Refresh => {
                let report = refresh_nodes(&mut state, &sessions, central.as_ref()).await;
                let result_lost = apply_refresh_report(
                    report,
                    &mut state,
                    &mut sessions,
                    &mut central,
                    &mut sync_workers,
                );
                if result_lost {
                    mark_result_windows_stale(
                        &mut group_window_state,
                        &mut member_window_state,
                        &mut loaded_members,
                        &mut prepared_delete,
                        &mut result_windows,
                        &active_window_request,
                        &events,
                    )
                    .await;
                }
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
                    &sessions,
                    &mut runtime_view,
                    &runtime_tasks,
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
            UiCommand::StartCrossAnalysis { selections } => {
                start_cross_analysis(
                    &selections,
                    state.config(),
                    &sessions,
                    central.as_mut(),
                    &mut cross_analysis,
                    &events,
                    &runtime_tasks,
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
            UiCommand::RequestGroupWindow { request } => {
                async {
                    let previous = active_window_request.groups.replace(request.clone());
                    let scope_changed = previous.as_ref().is_some_and(|old| {
                        old.analysis_run_id != request.analysis_run_id || old.kind != request.kind
                    });
                    if scope_changed {
                        if let Some(old_run) = previous
                            .as_ref()
                            .and_then(|old| parse_analysis_id(&old.analysis_run_id).ok())
                        {
                            result_windows.clear_run(old_run);
                        }
                        loaded_members = None;
                        prepared_delete = None;
                        member_window_state = ResultWindowState::empty();
                        active_window_request.members = None;
                        send_event(
                            &events,
                            UiEvent::MembersChanged {
                                group_id: String::new(),
                                window: Box::new(ResultWindowState::empty()),
                            },
                        )
                        .await?;
                        group_window_state = ResultWindowState::empty();
                    }
                    let loading = group_window_state.with_loading(true);
                    group_window_state = loading.clone();
                    send_event(&events, UiEvent::GroupsChanged(Box::new(loading))).await?;
                    let result =
                        load_group_window(&mut result_windows, &request, central.as_ref()).await;
                    if active_window_request.groups.as_ref() != Some(&request) {
                        Ok(())
                    } else {
                        match result {
                            Ok(window) => {
                                group_window_state = window.clone();
                                loaded_members = None;
                                prepared_delete = None;
                                member_window_state = ResultWindowState::empty();
                                send_event(&events, UiEvent::GroupsChanged(Box::new(window))).await
                            }
                            Err(error) => {
                                let stale = group_window_state.as_stale();
                                group_window_state = stale.clone();
                                send_event(&events, UiEvent::GroupsChanged(Box::new(stale)))
                                    .await?;
                                Err(error)
                            }
                        }
                    }
                }
                .await
            }
            UiCommand::RequestMemberWindow { request, group_id } => {
                async {
                    // 成员范围变化后，旧确认快照不再与当前窗口绑定。
                    prepared_delete = None;
                    let previous = active_window_request
                        .members
                        .replace((group_id.clone(), request.clone()));
                    let context_changed = previous.as_ref().is_some_and(|(old_group, old)| {
                        old_group != &group_id
                            || old.analysis_run_id != request.analysis_run_id
                            || old.kind != request.kind
                    });
                    if context_changed {
                        loaded_members = None;
                        prepared_delete = None;
                        member_window_state = ResultWindowState::empty();
                    }
                    let loading = member_window_state.with_loading(true);
                    member_window_state = loading.clone();
                    send_event(
                        &events,
                        UiEvent::MembersChanged {
                            group_id: group_id.clone(),
                            window: Box::new(loading),
                        },
                    )
                    .await?;
                    let result = load_member_window(
                        &mut result_windows,
                        &request,
                        &group_id,
                        &sessions,
                        central.as_ref(),
                    )
                    .await;
                    if active_window_request.members.as_ref()
                        != Some(&(group_id.clone(), request.clone()))
                    {
                        Ok(())
                    } else {
                        match result {
                            Ok(mut window) => {
                                let run_id = parse_analysis_id(&request.analysis_run_id)?;
                                let board_key = (run_id, group_id.clone());
                                let board = review_boards
                                    .entry(board_key)
                                    .or_insert_with(|| {
                                        ReviewBoard::for_central(run_id, &group_id, &window.items)
                                    })
                                    .clone();
                                for member in &mut window.items {
                                    member.review = board.decision(&member.location);
                                }
                                let context = LoadedMembersContext {
                                    scope: ResultScope::Central { run_id },
                                    kind: request.kind,
                                    group_id: group_id.clone(),
                                    run_id,
                                    items: window.items.clone(),
                                    review_board: board,
                                    window: window.clone(),
                                };
                                member_window_state = window.clone();
                                loaded_members = Some(context);
                                prepared_delete = None;
                                send_event(
                                    &events,
                                    UiEvent::MembersChanged {
                                        group_id,
                                        window: Box::new(window),
                                    },
                                )
                                .await
                            }
                            Err(error) => {
                                let stale = member_window_state.as_stale();
                                member_window_state = stale.clone();
                                send_event(
                                    &events,
                                    UiEvent::MembersChanged {
                                        group_id,
                                        window: Box::new(stale),
                                    },
                                )
                                .await?;
                                Err(error)
                            }
                        }
                    }
                }
                .await
            }
            UiCommand::SaveReview {
                machine_id,
                normalized_path,
                decision,
            } => {
                prepared_delete = None;
                if let Err(error) = result_window_write_error(
                    &group_window_state,
                    &member_window_state,
                    loaded_members.as_ref(),
                ) {
                    Err(error)
                } else {
                    save_one_review(
                        &machine_id,
                        &normalized_path,
                        decision,
                        loaded_members.as_mut(),
                        &mut review_boards,
                        central.as_ref(),
                        &events,
                    )
                    .await
                }
            }
            UiCommand::ApplyQuickReview(rule) => {
                prepared_delete = None;
                if let Err(error) = result_window_write_error(
                    &group_window_state,
                    &member_window_state,
                    loaded_members.as_ref(),
                ) {
                    Err(error)
                } else {
                    apply_quick_review(
                        rule,
                        loaded_members.as_mut(),
                        &mut review_boards,
                        central.as_ref(),
                        &events,
                    )
                    .await
                }
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
                if let Err(error) = result_window_write_error(
                    &group_window_state,
                    &member_window_state,
                    loaded_members.as_ref(),
                ) {
                    Err(error)
                } else {
                    prepare_delete(
                        state.config().delete_mode,
                        loaded_members.as_ref(),
                        &sessions,
                        central.as_ref(),
                        &mut prepared_delete,
                        &events,
                        &runtime_tasks,
                    )
                    .await
                }
            }
            UiCommand::ConfirmDelete => {
                let result = if let Err(error) = result_window_write_error(
                    &group_window_state,
                    &member_window_state,
                    loaded_members.as_ref(),
                ) {
                    Err(error)
                } else {
                    confirm_delete(
                        prepared_delete.as_ref(),
                        &sessions,
                        central.as_mut(),
                        &events,
                    )
                    .await
                };
                if result.is_ok() {
                    prepared_delete = None;
                    loaded_members = None;
                    if let Some(request) = active_window_request.groups.as_ref() {
                        if let Ok(run_id) = parse_analysis_id(&request.analysis_run_id) {
                            result_windows.clear_run(run_id);
                            review_boards.retain(|(id, _), _| *id != run_id);
                        }
                    }
                    group_window_state = ResultWindowState::empty();
                    member_window_state = ResultWindowState::empty();
                    let _ = send_event(
                        &events,
                        UiEvent::GroupsChanged(Box::new(ResultWindowState::empty())),
                    )
                    .await;
                }
                result
            }
            UiCommand::LoadNodeConfig { node_index } => {
                if state.node_config().is_in_progress() {
                    Err(node_config_target_change_error())
                } else {
                    state.select_node_config(node_index);
                    publish_node_config(&events, &state).await;
                    load_node_config(node_index, &mut state, &sessions, &events).await
                }
            }
            UiCommand::SaveNodeConfig { node_index, config } => {
                if state.node_config().is_in_progress() {
                    Err(node_config_target_change_error())
                } else {
                    let result =
                        save_node_config(node_index, config, &mut state, &sessions, &events).await;
                    if let Err(error) = &result {
                        state.fail_node_config(error.clone());
                        publish_node_config(&events, &state).await;
                    }
                    result
                }
            }
            UiCommand::SelectRuntimeTask { key } => {
                runtime_view.select(key);
                refresh_runtime_tasks(&mut runtime_view, &sessions, &runtime_tasks).await;
                publish_runtime_tasks(&events, &runtime_view).await;
                Ok(())
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
                    sessions.clear();
                    sync_workers.clear();
                    central = connect_central(&mut state).await;
                    mark_result_windows_stale(
                        &mut group_window_state,
                        &mut member_window_state,
                        &mut loaded_members,
                        &mut prepared_delete,
                        &mut result_windows,
                        &active_window_request,
                        &events,
                    )
                    .await;
                    reconnect_ticks = repeating_interval(state.config().reconnect_interval_seconds);
                }
                result
            }
            UiCommand::TestDatabaseConnection { url } => {
                let database_events = events.clone();
                tokio::spawn(async move {
                    let result = inspect_database(&url)
                        .await
                        .map_err(|error| error.to_string());
                    let _ = database_events
                        .send(UiEvent::DatabaseDiagnosticsChanged(result))
                        .await;
                });
                Ok(())
            }
            UiCommand::Shutdown => {
                let _ = events.send(UiEvent::ShutdownComplete).await;
                break;
            }
        };
        if let Err(error) = result {
            let _ = events.send(UiEvent::Error(error)).await;
        }
        reconcile_and_publish_runtime_tasks(
            &sessions,
            &mut runtime_watchers,
            &runtime_event_sender,
            &mut runtime_view,
            &runtime_tasks,
            &events,
        )
        .await;
        publish(&events, &state).await;
    }
}

/// 创建固定两秒、错过后不补跑的运行任务刷新时钟。
fn runtime_task_interval() -> Interval {
    let mut ticks = interval(Duration::from_secs(RUNTIME_TASK_REFRESH_SECONDS));
    ticks.set_missed_tick_behavior(MissedTickBehavior::Delay);
    ticks
}

/// 会话集合变化时立即建立监督器并发布一次列表/选中详情。
async fn reconcile_and_publish_runtime_tasks(
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    watchers: &mut BTreeMap<usize, NodeRuntimeWatcher>,
    runtime_events: &mpsc::Sender<NodeRuntimeEvent>,
    view: &mut RuntimeTaskControllerState,
    registry: &DesktopRuntimeTaskRegistry,
    ui_events: &mpsc::Sender<UiEvent>,
) {
    if reconcile_runtime_watchers(sessions, watchers, runtime_events, view) {
        refresh_runtime_tasks(view, sessions, registry).await;
        publish_runtime_tasks(ui_events, view).await;
    }
}

/// 让监督器集合与当前活动会话严格一一对应，并返回集合是否发生变化。
fn reconcile_runtime_watchers(
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    watchers: &mut BTreeMap<usize, NodeRuntimeWatcher>,
    events: &mpsc::Sender<NodeRuntimeEvent>,
    view: &mut RuntimeTaskControllerState,
) -> bool {
    let obsolete = watchers
        .iter()
        .filter_map(|(index, watcher)| {
            let current = sessions.get(index);
            let matches = current.is_some_and(|session| {
                Arc::ptr_eq(session, &watcher.session)
                    && session.machine_id().as_str() == watcher.machine_id
            });
            (!matches).then_some(*index)
        })
        .collect::<Vec<_>>();
    let mut changed = !obsolete.is_empty();
    for index in obsolete {
        watchers.remove(&index);
        if selected_node_index(view) == Some(index) {
            view.mark_stale(format!("节点 {index} 运行任务连接已失效"));
        }
    }

    for (index, session) in sessions {
        if watchers.contains_key(index) {
            continue;
        }
        changed = true;
        let generation = Uuid::now_v7();
        let machine_id = session.machine_id().as_str().to_owned();
        let event_session = Arc::clone(session);
        let event_sender = events.clone();
        let event_machine = machine_id.clone();
        let event_index = *index;
        let task = tokio::spawn(async move {
            loop {
                let outcome = match event_session.next_runtime_event().await {
                    Ok(event) => NodeRuntimeEventOutcome::Changed(event),
                    Err(error) => NodeRuntimeEventOutcome::Disconnected(error.to_string()),
                };
                let disconnected = matches!(outcome, NodeRuntimeEventOutcome::Disconnected(_));
                if event_sender
                    .send(NodeRuntimeEvent {
                        node_index: event_index,
                        generation,
                        machine_id: event_machine.clone(),
                        outcome,
                    })
                    .await
                    .is_err()
                {
                    break;
                }
                if disconnected {
                    break;
                }
            }
        });
        watchers.insert(
            *index,
            NodeRuntimeWatcher {
                generation,
                machine_id,
                session: Arc::clone(session),
                task,
            },
        );
    }
    changed
}

/// 只应用当前代次和机器的主动事件；旧会话结果直接丢弃。
async fn apply_runtime_event(
    event: NodeRuntimeEvent,
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    sync_workers: &mut BTreeMap<usize, NodeSyncWorker>,
    watchers: &mut BTreeMap<usize, NodeRuntimeWatcher>,
    view: &mut RuntimeTaskControllerState,
    registry: &DesktopRuntimeTaskRegistry,
    ui_events: &mpsc::Sender<UiEvent>,
) {
    let valid = watchers.get(&event.node_index).is_some_and(|watcher| {
        runtime_event_identity_matches(
            watcher.generation,
            &watcher.machine_id,
            event.generation,
            &event.machine_id,
        ) && sessions.get(&event.node_index).is_some_and(|session| {
            Arc::ptr_eq(session, &watcher.session)
                && session.machine_id().as_str() == event.machine_id
        })
    });
    if !valid {
        return;
    }

    match event.outcome {
        NodeRuntimeEventOutcome::Changed(changed) => {
            let completed = changed.state == "completed";
            refresh_runtime_tasks(view, sessions, registry).await;
            if completed {
                // 任务终态由 RuntimeTask 事件提供；不再从旧持久任务列表推断完成。
                queue_automatic(
                    &[event.node_index],
                    sync_workers,
                    AutomaticSyncCause::TaskCompleted,
                )
                .await;
            }
        }
        NodeRuntimeEventOutcome::Disconnected(message) => {
            sessions.remove(&event.node_index);
            sync_workers.remove(&event.node_index);
            watchers.remove(&event.node_index);
            state.set_node_error(event.node_index, message.clone());
            if selected_node_index(view) == Some(event.node_index) {
                view.mark_stale(message);
            }
        }
    }
    publish_runtime_tasks(ui_events, view).await;
}

/// 比较监督器与事件的冻结代次和机器身份；测试用公开边界验证旧事件必被拒绝。
#[doc(hidden)]
pub fn runtime_event_identity_matches(
    current_generation: Uuid,
    current_machine_id: &str,
    event_generation: Uuid,
    event_machine_id: &str,
) -> bool {
    current_generation == event_generation && current_machine_id == event_machine_id
}

/// 合并 Desktop registry 与全部在线 Node 摘要，并只刷新当前选中详情。
async fn refresh_runtime_tasks(
    view: &mut RuntimeTaskControllerState,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    registry: &DesktopRuntimeTaskRegistry,
) {
    let mut summaries = registry.list();
    let mut errors = Vec::new();
    for (node_index, session) in sessions {
        match list_node_runtime_tasks(*node_index, session).await {
            Ok(mut tasks) => summaries.append(&mut tasks),
            Err(error) => {
                errors.push(format!("节点 {node_index} 运行任务列表失败: {error}"));
            }
        }
    }
    summaries.sort_by(|left, right| left.key.cmp(&right.key));
    summaries.dedup_by(|left, right| left.key == right.key);
    view.replace_summaries(summaries, (!errors.is_empty()).then(|| errors.join("；")));
    refresh_selected_runtime_details(view, sessions, registry).await;
}

/// 读取单个 Node 的全部稳定游标页，并用真实握手机器 ID 建立统一摘要。
async fn list_node_runtime_tasks(
    node_index: usize,
    session: &Arc<NodeSession>,
) -> Result<Vec<crate::runtime_tasks::RuntimeTaskSnapshot>, String> {
    let mut cursor = String::new();
    let mut tasks = Vec::new();
    loop {
        let page = session
            .list_runtime_tasks(&cursor, 100)
            .await
            .map_err(|error| error.to_string())?;
        tasks.extend(page.tasks.into_iter().map(|summary| {
            DesktopRuntimeTaskRegistry::node_snapshot(node_index, session.machine_id(), summary)
        }));
        if page.next_cursor.is_empty() {
            break;
        }
        if page.next_cursor == cursor {
            return Err("Node 返回了未推进的运行任务游标".into());
        }
        cursor = page.next_cursor;
    }
    Ok(tasks)
}

/// 只刷新当前选中详情；失败时保留最后成功数据并标记 stale。
async fn refresh_selected_runtime_details(
    view: &mut RuntimeTaskControllerState,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    registry: &DesktopRuntimeTaskRegistry,
) {
    let Some(selected) = view.selected().cloned() else {
        return;
    };
    match selected.owner {
        RuntimeTaskOwner::Desktop => match registry.details(&selected) {
            Some(details) => view.set_details(RuntimeTaskDetailsView::Desktop(details)),
            None => view.mark_stale("Desktop 运行任务已不存在"),
        },
        RuntimeTaskOwner::Node { node_index } => {
            let Some(session) = sessions.get(&node_index) else {
                view.mark_stale(format!("节点 {node_index} 未连接，保留最后运行详情"));
                return;
            };
            let machine_id = session.machine_id().as_str().to_owned();
            match session.runtime_task_details(&selected.id).await {
                Ok(details) => {
                    let response_machine = details
                        .summary
                        .as_ref()
                        .map(|summary| summary.machine_id.as_str())
                        .unwrap_or_default();
                    if !response_machine.is_empty() && response_machine != machine_id {
                        view.mark_stale(format!("节点 {node_index} 运行详情机器归属不匹配"));
                    } else {
                        view.set_details(RuntimeTaskDetailsView::Node {
                            node_index,
                            machine_id,
                            details,
                        });
                    }
                }
                Err(error) => {
                    view.mark_stale(format!("节点 {node_index} 运行任务详情失败: {error}"))
                }
            }
        }
    }
}

/// 返回当前选择所属的节点索引；Desktop 任务没有节点索引。
fn selected_node_index(view: &RuntimeTaskControllerState) -> Option<usize> {
    match view.selected().map(|key| &key.owner) {
        Some(RuntimeTaskOwner::Node { node_index }) => Some(*node_index),
        _ => None,
    }
}

/// 向 UI 发布独立运行任务快照，不重放其它配置或表单状态。
async fn publish_runtime_tasks(events: &mpsc::Sender<UiEvent>, view: &RuntimeTaskControllerState) {
    let _ = events
        .send(UiEvent::RuntimeTasksChanged(view.clone()))
        .await;
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
    let endpoints = state
        .nodes()
        .iter()
        .enumerate()
        .filter(|(index, _)| !sessions.contains_key(index))
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
    /// 中心 cursor 查询失败；控制器必须丢弃旧 PG client，等待下一次重连。
    central_error: Option<String>,
}

/// 查询所有活动会话的节点状态；运行任务由独立 RuntimeTask 控制器刷新。
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
    runtime_tasks: &DesktopRuntimeTaskRegistry,
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
            runtime_tasks.clone(),
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
    runtime_tasks: DesktopRuntimeTaskRegistry,
) {
    let engine = SyncEngine::new();
    let machine_id = session.machine_id().as_str().to_owned();
    let mut central = None;
    while let Some(trigger) = triggers.next().await {
        let runtime = runtime_tasks
            .begin_or_merge_sync(session.machine_id(), format!("同步节点 {machine_id}"));
        if central.is_none() {
            match CentralStore::connect(&postgres_url).await {
                Ok(store) => central = Some(store),
                Err(error) => {
                    runtime.record_failure("acknowledging", "", error.to_string());
                    let _ = runtime.finish(DesktopRuntimeTaskState::Failed);
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
            .sync_node_with_progress(
                session.as_ref(),
                central.as_mut().expect("PG 连接已经建立"),
                trigger,
                |progress| runtime.update_sync_progress(progress),
            )
            .await;
        // 当前同步运行期间进入队列的触发已经由同一任务覆盖，避免完成后重复显示新行。
        triggers.drain_pending();
        match outcome {
            Ok(report) => match session.status().await {
                Ok(status) => {
                    let _ = runtime.finish(DesktopRuntimeTaskState::Completed);
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
                    runtime.record_failure("caught_up", "", error.to_string());
                    let _ = runtime.finish(DesktopRuntimeTaskState::Failed);
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
                runtime.record_failure("incremental", "", error.to_string());
                let _ = runtime.finish(DesktopRuntimeTaskState::Failed);
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
) -> bool {
    let mut result_lost = false;
    for index in report.failed_sessions {
        sessions.remove(&index);
        workers.remove(&index);
    }
    if let Some(error) = report.central_error {
        central.take();
        state.set_postgres_health(PostgresHealth::Error(error));
        result_lost = true;
    }
    result_lost
}

/// 把后台同步结果归并回唯一视图；过期机器结果不能写入已编辑的节点行。
async fn apply_sync_result(
    result: NodeSyncEvent,
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    central: &mut Option<CentralStore>,
    workers: &mut BTreeMap<usize, NodeSyncWorker>,
    events: &mpsc::Sender<UiEvent>,
) -> bool {
    if workers.get(&result.index).map(|worker| worker.id) != Some(result.worker_id) {
        return false;
    }
    let current_machine = sessions
        .get(&result.index)
        .map(|session| session.machine_id().as_str());
    if current_machine != Some(result.machine_id.as_str()) {
        return false;
    }
    let mut result_lost = false;
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
                result_lost = true;
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
    result_lost
}

/// 固定重连 tick：清除已断会话并重试节点/中心；任务终态由 RuntimeTask 事件触发同步。
async fn reconnect_and_sync(
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    central: &mut Option<CentralStore>,
    workers: &mut BTreeMap<usize, NodeSyncWorker>,
    results: &mpsc::UnboundedSender<NodeSyncEvent>,
    runtime_tasks: &DesktopRuntimeTaskRegistry,
) -> bool {
    let report = refresh_nodes(state, sessions, central.as_ref()).await;
    let result_lost = apply_refresh_report(report, state, sessions, central, workers);
    if central.is_none() && state.config().postgres_url.is_some() {
        *central = connect_central(state).await;
    }
    let connected_nodes = connect_missing(state, sessions).await;
    ensure_sync_workers(
        &connected_nodes,
        sessions,
        state.config().postgres_url.as_deref(),
        workers,
        results,
        runtime_tasks,
    );
    queue_automatic(&connected_nodes, workers, AutomaticSyncCause::Connected).await;
    result_lost
}

/// 固定五秒追赶 tick：刷新任务后只排队，不等待任一节点完成同步。
async fn catch_up_and_refresh(
    state: &mut DesktopViewState,
    sessions: &mut BTreeMap<usize, Arc<NodeSession>>,
    central: &mut Option<CentralStore>,
    workers: &mut BTreeMap<usize, NodeSyncWorker>,
    results: &mpsc::UnboundedSender<NodeSyncEvent>,
    runtime_tasks: &DesktopRuntimeTaskRegistry,
) -> bool {
    let report = refresh_nodes(state, sessions, central.as_ref()).await;
    let result_lost = apply_refresh_report(report, state, sessions, central, workers);
    let indexes = sessions.keys().copied().collect::<Vec<_>>();
    ensure_sync_workers(
        &indexes,
        sessions,
        state.config().postgres_url.as_deref(),
        workers,
        results,
        runtime_tasks,
    );
    queue_automatic(&indexes, workers, AutomaticSyncCause::CatchUp).await;
    result_lost
}

/// 中心连接失效时保留已读窗口，但撤销所有会改变结果的进程内操作。
async fn mark_result_windows_stale(
    groups: &mut ResultWindowState<crate::results::GroupView>,
    members: &mut ResultWindowState<MemberView>,
    loaded: &mut Option<LoadedMembersContext>,
    prepared: &mut Option<PreparedDeleteContext>,
    cache: &mut CentralResultWindowCache,
    active: &ActiveWindowRequest,
    events: &mpsc::Sender<UiEvent>,
) {
    *prepared = None;
    for run_id in active
        .groups
        .iter()
        .chain(active.members.iter().map(|(_, request)| request))
        .filter_map(|request| parse_analysis_id(&request.analysis_run_id).ok())
    {
        cache.clear_run(run_id);
    }
    if active.groups.is_some() {
        let stale = groups.as_stale();
        *groups = stale.clone();
        let _ = send_event(events, UiEvent::GroupsChanged(Box::new(stale))).await;
    }
    if let Some((group_id, _)) = active.members.as_ref() {
        let stale = members.as_stale();
        *members = stale.clone();
        if let Some(context) = loaded.as_mut() {
            context.window = stale.clone();
        }
        let _ = send_event(
            events,
            UiEvent::MembersChanged {
                group_id: group_id.clone(),
                window: Box::new(stale),
            },
        )
        .await;
    }
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

async fn save_node_config(
    node_index: usize,
    config: proto::NodeConfigValue,
    state: &mut DesktopViewState,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let loaded = state.node_config();
    if loaded.selected_node_index() != Some(node_index) {
        return Err("保存目标与已加载远程配置不是同一节点".into());
    }
    let expected_version_sha256 = loaded
        .snapshot()
        .ok_or_else(|| "请先加载远程 Node 配置".to_owned())?
        .version_sha256
        .clone();
    let previous_snapshot = loaded
        .snapshot()
        .cloned()
        .ok_or_else(|| "请先加载远程 Node 配置".to_owned())?;
    let session = sessions
        .get(&node_index)
        .cloned()
        .ok_or_else(|| format!("节点 {node_index} 未连接，无法保存远程配置"))?;
    let current_machine_id = session.machine_id().as_str().to_owned();
    if previous_snapshot.machine_id != current_machine_id {
        return Err(format!(
            "保存目标机器身份已变化：已加载机器 {}，当前会话机器 {}，请重新加载配置",
            previous_snapshot.machine_id, current_machine_id
        ));
    }
    NodeConfig::try_from(config.clone()).map_err(|error| error.to_string())?;

    state.set_node_config_phase(NodeConfigSavePhase::Saving);
    publish_node_config(events, state).await;
    let saved_config = config.clone();
    let saved = session
        .save_node_config(&expected_version_sha256, config)
        .await
        .map_err(|error| error.to_string())?;
    if saved.machine_id != session.machine_id().as_str() {
        return Err(format!(
            "保存响应机器不匹配：目标 {}，响应 {}",
            session.machine_id().as_str(),
            saved.machine_id
        ));
    }
    if saved.saved_version_sha256.is_empty() {
        return Err("保存响应缺少新配置摘要".into());
    }
    let snapshot = proto::NodeConfigSnapshot {
        machine_id: saved.machine_id,
        version_sha256: saved.saved_version_sha256,
        config: Some(saved_config),
        logical_cpu_count: previous_snapshot.logical_cpu_count,
        effective_worker_count: previous_snapshot.effective_worker_count,
    };
    state.complete_node_config(snapshot);
    publish_node_config(events, state).await;
    Ok(())
}

fn node_config_target_change_error() -> String {
    "远程 Node 配置保存进行中，不能切换、编辑或移除目标节点".into()
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
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    runtime_view: &mut RuntimeTaskControllerState,
    runtime_tasks: &DesktopRuntimeTaskRegistry,
) -> Result<(), String> {
    let session = sessions
        .get(&node_index)
        .ok_or_else(|| "节点当前未连接".to_owned())?;
    let enumerator = match enumerator {
        EnumeratorKind::WindowsWalker => "windows_walker",
        EnumeratorKind::Everything => "everything",
    };
    session
        .create_scan(roots, force_recalculate, enumerator)
        .await
        .map_err(|error| error.to_string())?;
    // CreateScan 只返回接受结果；任务详情统一从当前进程 RuntimeTask 快照读取。
    refresh_runtime_tasks(runtime_view, sessions, runtime_tasks).await;
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

async fn start_cross_analysis(
    text: &str,
    config: &DesktopConfig,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&mut CentralStore>,
    current: &mut Option<ActiveCrossAnalysis>,
    events: &mpsc::Sender<UiEvent>,
    runtime_tasks: &DesktopRuntimeTaskRegistry,
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
    let machines = selected
        .iter()
        .map(|selection| selection.session.machine_id().clone())
        .collect::<Vec<_>>();
    let runtime = runtime_tasks.begin_cross_analysis(
        run_id.as_uuid().to_string(),
        &machines,
        "重复文件清单（多机）",
    );
    *current = Some(ActiveCrossAnalysis {
        coordinator,
        runtime,
        node_count: machines.len(),
    });
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
    current: Option<&mut ActiveCrossAnalysis>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let active = current.ok_or_else(|| "当前进程尚未创建跨机器分析".to_owned())?;
    let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
    let online = sessions
        .values()
        .map(Arc::as_ref)
        .collect::<Vec<&NodeSession>>();
    let polled = if retry {
        active.coordinator.retry_unresolved(central, &online).await
    } else {
        active.coordinator.poll(central, &online).await
    };
    let report = match polled {
        Ok(report) => report,
        Err(error) => {
            let _ = active.runtime.finish(DesktopRuntimeTaskState::Failed);
            return Err(error.to_string());
        }
    };
    active.runtime.update_cross_poll(&report, active.node_count);
    let terminal = match report.status {
        CentralAnalysisStatus::Completed => Some(DesktopRuntimeTaskState::Completed),
        CentralAnalysisStatus::Partial => Some(DesktopRuntimeTaskState::Failed),
        CentralAnalysisStatus::Cancelled => Some(DesktopRuntimeTaskState::Cancelled),
        _ => None,
    };
    if let Some(state) = terminal {
        let _ = active.runtime.finish(state);
    }
    send_event(events, UiEvent::CrossAnalysisChanged(report)).await
}

/// 确认中心运行已经完成；未完成运行绝不能向结果页提供数据。
async fn ensure_completed(
    central: Option<&CentralStore>,
    run_id: AnalysisRunId,
) -> Result<(), String> {
    let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
    let snapshot = central
        .analysis_run_snapshot(run_id)
        .await
        .map_err(|error| error.to_string())?;
    if snapshot.status != CentralAnalysisStatus::Completed {
        return Err(format!(
            "中心分析尚未完成，当前状态为 {}",
            snapshot.status.as_str()
        ));
    }
    Ok(())
}

/// 通过中心有限窗口缓存读取一页组结果；不会回退到节点 SQLite。
async fn load_group_window(
    cache: &mut CentralResultWindowCache,
    request: &ResultWindowRequest,
    central: Option<&CentralStore>,
) -> Result<ResultWindowState<crate::results::GroupView>, String> {
    let run_id = parse_analysis_id(&request.analysis_run_id)?;
    ensure_completed(central, run_id).await?;
    cache
        .load_groups(
            central.expect("完成门禁已确认中心连接存在"),
            run_id,
            request.kind,
            request.start_index,
            request.normalized_visible_count(),
        )
        .await
        .map_err(|error| error.to_string())
}

/// 通过中心有限窗口缓存读取成员，并依据当前在线会话计算预览门禁。
async fn load_member_window(
    cache: &mut CentralResultWindowCache,
    request: &ResultWindowRequest,
    group_id: &str,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
) -> Result<ResultWindowState<MemberView>, String> {
    let run_id = parse_analysis_id(&request.analysis_run_id)?;
    ensure_completed(central, run_id).await?;
    let online = sessions
        .values()
        .map(|session| session.machine_id().clone())
        .collect::<std::collections::BTreeSet<_>>();
    cache
        .load_members(
            central.expect("完成门禁已确认中心连接存在"),
            run_id,
            group_id,
            request.start_index,
            request.normalized_visible_count(),
            |machine| online.contains(machine),
        )
        .await
        .map_err(|error| error.to_string())
}

/// 结果窗口加载中或文件库已变化时，只允许读取和预览，不接受复核/删除写入。
fn result_window_write_error(
    groups: &ResultWindowState<crate::results::GroupView>,
    members: &ResultWindowState<MemberView>,
    context: Option<&LoadedMembersContext>,
) -> Result<(), String> {
    let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
    if groups.loading || members.loading || context.window.loading {
        return Err("结果窗口正在加载，请等待当前窗口完成".into());
    }
    if groups.stale || members.stale || context.window.stale {
        return Err("文件库已变化，结果只读".into());
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn save_one_review(
    machine: &str,
    path: &str,
    decision: ReviewDecision,
    context: Option<&mut LoadedMembersContext>,
    review_boards: &mut BTreeMap<(AnalysisRunId, String), ReviewBoard>,
    central: Option<&CentralStore>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
    let location = location(machine, path)?;
    if !context.items.iter().any(|item| item.location == location) {
        return Err("复核位置不在当前组窗口中".into());
    }
    let run_id = context.run_id;
    let group_id = context.group_id.clone();
    persist_central_review(central, run_id, &group_id, &location, decision).await?;
    context.review_board.set(location.clone(), decision);
    review_boards.insert((run_id, group_id), context.review_board.clone());
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
    review_boards: &mut BTreeMap<(AnalysisRunId, String), ReviewBoard>,
    central: Option<&CentralStore>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
    let mut next_board = context.review_board.clone();
    let changes = next_board.apply_quick_rule(&context.items, rule);
    if changes.is_empty() {
        return Err("当前组没有满足快捷规则的活动成员".into());
    }
    for change in &changes {
        persist_central_review(
            central,
            context.run_id,
            &context.group_id,
            &change.location,
            change.decision,
        )
        .await?;
    }
    context.review_board = next_board;
    review_boards.insert(
        (context.run_id, context.group_id.clone()),
        context.review_board.clone(),
    );
    for member in &mut context.items {
        member.review = context.review_board.decision(&member.location);
    }
    publish_members(events, context).await
}

/// 暂时保留中心复核持久化兼容调用；Task13 再移除该边界。
async fn persist_central_review(
    central: Option<&CentralStore>,
    run_id: AnalysisRunId,
    group_id: &str,
    location: &LocationKey,
    decision: ReviewDecision,
) -> Result<(), String> {
    central
        .ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?
        .save_review_mark(run_id, group_id, location, central_review(decision))
        .await
        .map_err(|error| error.to_string())
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
    runtime_tasks: &DesktopRuntimeTaskRegistry,
) -> Result<(), String> {
    let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
    let group = load_complete_review_group(context, sessions, central).await?;
    let confirmation = DeleteConfirmation::from_groups(mode, std::slice::from_ref(&group));
    let items = group
        .members
        .into_iter()
        .filter(|member| member.active && member.review == ReviewDecision::Delete)
        .collect::<Vec<_>>();
    let machines = items
        .iter()
        .map(|member| member.location.machine_id().clone())
        .collect::<Vec<_>>();
    let runtime = runtime_tasks.begin_delete(
        Uuid::now_v7().to_string(),
        &machines,
        format!("删除组 {}", context.group_id),
        items.len() as u64,
    );
    runtime.mark_delete_prepared();
    *prepared = Some(PreparedDeleteContext {
        scope: context.scope,
        group_id: context.group_id.clone(),
        confirmation: confirmation.clone(),
        items,
        runtime,
    });
    send_event(events, UiEvent::DeleteConfirmationChanged(confirmation)).await
}

async fn load_complete_review_group(
    context: &LoadedMembersContext,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: Option<&CentralStore>,
) -> Result<ReviewGroup, String> {
    let central = central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?;
    let online = sessions
        .values()
        .map(|session| session.machine_id().clone())
        .collect::<std::collections::BTreeSet<_>>();
    let mut cache = CentralResultWindowCache::new();
    let mut members = cache
        .load_all_members(central, context.run_id, &context.group_id, |machine| {
            online.contains(machine)
        })
        .await
        .map_err(|error| error.to_string())?;
    for member in &mut members {
        member.review = context.review_board.decision(&member.location);
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
    let executed: Result<Vec<proto::DeleteItem>, String> = async {
        execute_central_delete(
            prepared.scope.run_id(),
            &prepared.group_id,
            &prepared.items,
            prepared.confirmation.mode,
            sessions,
            central.ok_or_else(|| "PostgreSQL 中心模式未启用".to_owned())?,
        )
        .await
    }
    .await;
    match executed {
        Ok(items) => {
            prepared.runtime.finish_delete_results(&items);
            let state = if items.iter().any(|item| item.outcome == "failed") {
                DesktopRuntimeTaskState::Failed
            } else {
                DesktopRuntimeTaskState::Completed
            };
            let _ = prepared.runtime.finish(state);
            send_event(
                events,
                UiEvent::DeleteFinished(summarize_delete_items(&items)),
            )
            .await
        }
        Err(error) => {
            prepared
                .runtime
                .record_failure("delete_items", "", error.clone());
            let _ = prepared.runtime.finish(DesktopRuntimeTaskState::Failed);
            Err(error)
        }
    }
}

async fn execute_central_delete(
    run_id: AnalysisRunId,
    group_id: &str,
    confirmed: &[MemberView],
    mode: DeleteMode,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: &mut CentralStore,
) -> Result<Vec<proto::DeleteItem>, String> {
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
    Ok(all_items)
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
        UiEvent::ReviewChanged(Box::new(ResultWindowState {
            start_index: context.window.start_index,
            total_rows: context.window.total_rows,
            items: context.items.clone(),
            loading: false,
            stale: context.window.stale,
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

const fn central_review(decision: ReviewDecision) -> CentralReviewDecision {
    match decision {
        ReviewDecision::Undecided => CentralReviewDecision::Undecided,
        ReviewDecision::Keep => CentralReviewDecision::Keep,
        ReviewDecision::Delete => CentralReviewDecision::Delete,
    }
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

fn persist(path: &Path, config: &DesktopConfig) -> Result<(), String> {
    fs::write(path, config.to_toml().map_err(|error| error.to_string())?)
        .map_err(|error| error.to_string())
}

async fn publish(events: &mpsc::Sender<UiEvent>, state: &DesktopViewState) {
    let _ = events
        .send(UiEvent::ViewChanged(Box::new(state.clone())))
        .await;
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 构造带明确运行状态的 Node 主动事件。
    fn runtime_event(index: usize, state: &str) -> NodeRuntimeEvent {
        NodeRuntimeEvent {
            node_index: index,
            generation: Uuid::from_u128(1),
            machine_id: "91".repeat(32),
            outcome: NodeRuntimeEventOutcome::Changed(proto::RuntimeTaskChanged {
                runtime_task_id: format!("task-{index}"),
                state: state.into(),
                ..Default::default()
            }),
        }
    }

    #[tokio::test]
    async fn runtime_event_bridge_backpressures_and_keeps_terminal_event() {
        let (sender, mut receiver) = mpsc::channel(RUNTIME_EVENT_BRIDGE_CAPACITY);
        for index in 0..64 {
            sender.send(runtime_event(index, "running")).await.unwrap();
        }
        let terminal_sender = sender.clone();
        let pending =
            tokio::spawn(async move { terminal_sender.send(runtime_event(64, "completed")).await });
        tokio::task::yield_now().await;
        assert!(
            !pending.is_finished(),
            "64 槽事件桥满时，终态发送必须等待消费而不是绕过背压"
        );

        let _ = receiver.recv().await.unwrap();
        pending.await.unwrap().unwrap();
        let mut terminal_seen = false;
        while let Ok(event) = receiver.try_recv() {
            terminal_seen |= matches!(
                event.outcome,
                NodeRuntimeEventOutcome::Changed(proto::RuntimeTaskChanged { ref state, .. })
                    if state == "completed"
            );
        }
        assert!(terminal_seen, "解除背压后终态事件必须保留在桥中");
    }
}

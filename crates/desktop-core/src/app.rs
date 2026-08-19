//! Slint 回调与异步节点/中心服务之间的单向命令和事件通道。

use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
    sync::Arc,
};

use dedup_core::{
    AnalysisRunId, DeleteMode, DesktopConfig, EnumeratorKind, LocationKey, MachineId,
    NormalizedPath, TaskId,
};
use dedup_protocol::proto;
use tokio::sync::mpsc;
use uuid::Uuid;

use crate::{
    analysis::{CrossAnalysisCoordinator, CrossNodeSelection, CrossPollReport},
    central::{
        CentralDeleteOutcome, CentralDeleteResult, CentralError, CentralReviewDecision,
        CentralStore,
    },
    delete::{DeleteConfirmation, ReviewGroup},
    node_session::NodeSession,
    results::{
        GroupKind, GroupPage, MemberPage, MemberView, ResultScope, group_page_from_central,
        group_page_from_node, load_preview, member_page_from_central, member_page_from_node,
    },
    review::{QuickReviewRule, ReviewBoard, ReviewDecision},
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
        /// 预览所属路径。
        display_path: String,
        /// `original` 或 `contact_sheet`。
        file_kind: String,
        /// 原始编码数据，不写入缓存目录。
        bytes: Arc<[u8]>,
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
    let mut cross_analysis: Option<CrossAnalysisCoordinator> = None;
    let mut loaded_members: Option<LoadedMembersContext> = None;
    let mut prepared_delete: Option<PreparedDeleteContext> = None;
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
    send_event(
        events,
        UiEvent::PreviewReady {
            display_path: member.display_path.clone(),
            file_kind: preview.file_kind.into(),
            bytes: Arc::from(preview.bytes),
        },
    )
    .await
}

async fn prepare_delete(
    mode: DeleteMode,
    context: Option<&LoadedMembersContext>,
    prepared: &mut Option<PreparedDeleteContext>,
    events: &mpsc::Sender<UiEvent>,
) -> Result<(), String> {
    let context = context.ok_or_else(|| "尚未载入重复组成员".to_owned())?;
    let confirmation = DeleteConfirmation::from_groups(
        mode,
        &[ReviewGroup::new(&context.group_id, context.items.clone())],
    );
    *prepared = Some(PreparedDeleteContext {
        scope: context.scope,
        group_id: context.group_id.clone(),
        confirmation: confirmation.clone(),
    });
    send_event(events, UiEvent::DeleteConfirmationChanged(confirmation)).await
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
            let batch = sessions
                .get(&node_index)
                .ok_or_else(|| "节点当前未连接".to_owned())?
                .create_delete_batch(
                    run_id,
                    vec![prepared.group_id.clone()],
                    prepared.confirmation.mode,
                )
                .await
                .map_err(|error| error.to_string())?;
            summarize_delete_items(&batch.items)
        }
        ResultScope::Central { run_id } => {
            execute_central_delete(
                run_id,
                &prepared.group_id,
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
    mode: DeleteMode,
    sessions: &BTreeMap<usize, Arc<NodeSession>>,
    central: &mut CentralStore,
) -> Result<String, String> {
    let plan = central
        .create_delete_plan(run_id, &[group_id.into()], mode)
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

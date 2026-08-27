//! 管理端不可变快照所依据的纯节点、任务、设置和诊断状态。

use std::{net::IpAddr, path::PathBuf, str::FromStr};

use dedup_core::{CoreError, DesktopConfig, NodeEndpoint};
use dedup_protocol::proto;
use thiserror::Error;

use crate::runtime_tasks::{RuntimeTaskKey, RuntimeTaskSnapshot};

/// `data/desktop` 下供设置诊断页展示的绝对路径。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DesktopPaths {
    /// 桌面端数据根目录。
    pub data: PathBuf,
    /// 20 MiB × 10 滚动日志目录。
    pub logs: PathBuf,
    /// 管理端临时缓存目录。
    pub cache: PathBuf,
    /// 用户可编辑 TOML 配置文件。
    pub config: PathBuf,
}

/// 手工节点当前连接状态；错误文本单独保存在节点行中。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NodeConnectionState {
    /// 尚未连接或用户主动断开。
    Offline,
    /// 正在建立 TCP 与 V2 Hello。
    Connecting,
    /// 会话已握手并可执行命令。
    Online,
    /// 最近一次连接失败，等待固定间隔重试。
    Error,
}

/// NodeStatus 与中心同步游标映射出的运行统计。
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct NodeRuntimeStats {
    /// 节点配置的 Worker 总数。
    pub worker_count: u32,
    /// 当前正在执行媒体任务的 Worker 数。
    pub busy_workers: u32,
    /// SQLite queued 任务项数。
    pub queued_items: u64,
    /// SQLite running 任务项数。
    pub running_items: u64,
    /// 节点当前 outbox 最高序号。
    pub outbox_high_seq: u64,
    /// PostgreSQL 已提交的该节点游标。
    pub sync_high_seq: u64,
}

/// 节点列表中的一行稳定视图。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NodeView {
    /// 用户手工配置的 IP 与端口。
    pub endpoint: NodeEndpoint,
    /// 离线、连接中、在线或错误。
    pub connection: NodeConnectionState,
    /// Hello 后取得的物理机器 ID；离线时可保留上次值。
    pub machine_id: Option<String>,
    /// 在线节点最新状态统计。
    pub stats: Option<NodeRuntimeStats>,
    /// 最近连接或命令错误。
    pub error_text: Option<String>,
}

/// UI 使用的节点任务状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ViewTaskState {
    /// 等待领取。
    Queued,
    /// 正在执行。
    Running,
    /// 全部项已终态。
    Completed,
    /// 任务级失败。
    Failed,
    /// 用户取消。
    Cancelled,
}

/// 扫描或分析任务列表中的一个不可变进度快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskView {
    /// 节点持久任务 ID 文本。
    pub task_id: String,
    /// 手工节点列表索引。
    pub node_index: usize,
    /// 用户可读任务标题。
    pub title: String,
    /// 枚举、MD5、一筛、二筛或收尾阶段。
    pub stage: String,
    /// 当前持久状态。
    pub state: ViewTaskState,
    /// 成功、失败或取消的终态项数。
    pub completed_items: u64,
    /// 任务总项数。
    pub total_items: u64,
    /// 文件级失败数量。
    pub failed_items: u64,
    /// 一筛数据不完整而跳过的内容数量。
    pub skipped_incomplete: u64,
}

impl TaskView {
    /// 返回 `0..=100` 整数进度，空任务视为尚未开始。
    pub fn progress_percent(&self) -> u8 {
        self.completed_items
            .saturating_mul(100)
            .checked_div(self.total_items)
            .unwrap_or(0)
            .min(100) as u8
    }
}

/// 管理端观察到的 PostgreSQL 中心能力状态。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum PostgresHealth {
    /// 用户尚未配置连接串。
    Disabled,
    /// 正在连接并校验 V2 schema。
    Connecting,
    /// schema 已验证，中心功能可用。
    Ready,
    /// 数据库可达但尚未手工执行固定建库脚本。
    SchemaMissing,
    /// 连接或 schema 不兼容错误。
    Error(String),
}

/// 一个按钮是否可执行及其禁用原因。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ActionAvailability {
    /// `true` 时 UI 可以发送命令。
    pub enabled: bool,
    /// 禁用时直接显示给用户的原因。
    pub reason: String,
}

/// 远程 Node 配置保存与重连验证的严格阶段。
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum NodeConfigSavePhase {
    /// 尚未保存或只完成了配置加载。
    #[default]
    Idle,
    /// 正在 Desktop 边界验证待保存字段。
    Validating,
    /// 正在向冻结的旧会话发送版本化保存请求。
    Saving,
    /// Node 已接受保存并准备替代进程。
    Restarting,
    /// 旧会话已失效，按机器 ID 和原 endpoint 等待重连。
    WaitingForReconnect,
    /// 已重连同一机器，正在重新加载并核对新摘要。
    Verifying,
    /// 机器 ID 与保存摘要均验证一致。
    Completed,
    /// 校验、保存、重连或验证失败，错误文本保留。
    Failed,
}

/// 设置页消费的远程 Node 配置快照和保存生命周期。
#[derive(Clone, Debug, Default, PartialEq)]
pub struct NodeConfigControllerState {
    selected_node_index: Option<usize>,
    target_machine_id: Option<String>,
    target_endpoint: Option<NodeEndpoint>,
    snapshot: Option<proto::NodeConfigSnapshot>,
    phase: NodeConfigSavePhase,
    saved_version_sha256: Option<String>,
    error: Option<String>,
}

impl NodeConfigControllerState {
    /// 返回设置页当前选择的手工节点索引。
    pub const fn selected_node_index(&self) -> Option<usize> {
        self.selected_node_index
    }

    /// 返回加载或完成验证后的远程原始配置快照。
    pub const fn snapshot(&self) -> Option<&proto::NodeConfigSnapshot> {
        self.snapshot.as_ref()
    }

    /// 返回当前保存/重连阶段。
    pub const fn phase(&self) -> NodeConfigSavePhase {
        self.phase
    }

    /// 返回是否处于不能切换节点或修改 endpoint 的非终态。
    pub const fn is_in_progress(&self) -> bool {
        matches!(
            self.phase,
            NodeConfigSavePhase::Validating
                | NodeConfigSavePhase::Saving
                | NodeConfigSavePhase::Restarting
                | NodeConfigSavePhase::WaitingForReconnect
                | NodeConfigSavePhase::Verifying
        )
    }

    /// 返回冻结的目标物理机器 ID。
    pub fn target_machine_id(&self) -> Option<&str> {
        self.target_machine_id.as_deref()
    }

    /// 返回冻结的手工 endpoint。
    pub const fn target_endpoint(&self) -> Option<&NodeEndpoint> {
        self.target_endpoint.as_ref()
    }

    /// 返回 Node 接受保存后报告的新版本摘要。
    pub fn saved_version_sha256(&self) -> Option<&str> {
        self.saved_version_sha256.as_deref()
    }

    /// 返回终态失败原因。
    pub fn error(&self) -> Option<&str> {
        self.error.as_deref()
    }
}

/// 设置诊断页展示的一条已批准文件故障投影。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FileFaultView {
    /// 物理机器 ID。
    pub machine_id: String,
    /// 规范路径。
    pub normalized_path: String,
    /// 实际显示路径。
    pub display_path: String,
    /// 文件大小。
    pub file_size: u64,
    /// `suspected_physical_read` 或 `worker_crash`。
    pub fault_kind: String,
    /// 故障阶段。
    pub stage: String,
    /// 可选 Windows 错误码。
    pub error_code: Option<i32>,
    /// 最近诊断文案。
    pub message: String,
}

/// Node 进程内最近一次磁盘满清理摘要。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DiskFullCleanupSummaryView {
    /// 触发 Unix 毫秒。
    pub triggered_at_unix_ms: u64,
    /// 删除文件数。
    pub deleted_files: u64,
    /// 删除字节数。
    pub deleted_bytes: u64,
    /// 活动租约跳过数。
    pub skipped_active: u64,
    /// 异盘跳过数。
    pub skipped_other_disk: u64,
    /// 失败文件数。
    pub failed_files: u64,
}

/// 当前选中运行任务的详情来源；Node 保留握手机器身份以隔离过期会话。
#[derive(Clone, Debug, PartialEq)]
pub enum RuntimeTaskDetailsView {
    /// Node 进程通过同一管理会话返回的协议详情。
    Node {
        /// 当前 Desktop 配置中的节点索引。
        node_index: usize,
        /// 拉取详情时冻结的真实握手机器 ID。
        machine_id: String,
        /// Node 返回的阶段、Worker 与最近失败详情。
        details: proto::RuntimeTaskDetails,
    },
    /// Desktop 进程内 registry 的完整临时快照。
    Desktop(RuntimeTaskSnapshot),
}

/// UI 任务中心消费的统一 Node/Desktop 运行任务状态。
#[derive(Clone, Debug, Default, PartialEq)]
pub struct RuntimeTaskControllerState {
    /// 两秒刷新或终态事件归并后的稳定摘要列表。
    summaries: Vec<RuntimeTaskSnapshot>,
    /// 用户当前选择的统一任务键。
    selected: Option<RuntimeTaskKey>,
    /// 只为当前选中任务保留的详情。
    details: Option<RuntimeTaskDetailsView>,
    /// 断线或详情拉取失败时为真，旧详情仍保留供诊断。
    stale: bool,
    /// 最近一次列表、详情或事件连接错误。
    error: Option<String>,
}

impl RuntimeTaskControllerState {
    /// 为 UI 契约测试装配一个完整不可变快照；生产状态仍只由控制循环更新。
    #[doc(hidden)]
    pub fn from_parts_for_test(
        summaries: Vec<RuntimeTaskSnapshot>,
        selected: Option<RuntimeTaskKey>,
        details: Option<RuntimeTaskDetailsView>,
        stale: bool,
        error: Option<String>,
    ) -> Self {
        Self {
            summaries,
            selected,
            details,
            stale,
            error,
        }
    }

    /// 返回 Node/Desktop 合并后的运行任务摘要。
    pub fn summaries(&self) -> &[RuntimeTaskSnapshot] {
        &self.summaries
    }

    /// 返回用户当前选中的统一任务键。
    pub const fn selected(&self) -> Option<&RuntimeTaskKey> {
        self.selected.as_ref()
    }

    /// 返回当前详情；过期时仍可读取最后成功快照。
    pub const fn details(&self) -> Option<&RuntimeTaskDetailsView> {
        self.details.as_ref()
    }

    /// 返回详情是否因断线或请求失败而过期。
    pub const fn is_stale(&self) -> bool {
        self.stale
    }

    /// 返回最近一次运行任务监督错误。
    pub fn error(&self) -> Option<&str> {
        self.error.as_deref()
    }

    /// 切换选择时先清除旧详情，防止旧机器内容短暂归给新任务。
    pub(crate) fn select(&mut self, key: RuntimeTaskKey) {
        if self.selected.as_ref() != Some(&key) {
            self.selected = Some(key);
            self.details = None;
            self.stale = false;
            self.error = None;
        }
    }

    /// 原子替换本轮摘要，并记录不影响旧详情展示的列表错误。
    pub(crate) fn replace_summaries(
        &mut self,
        summaries: Vec<RuntimeTaskSnapshot>,
        error: Option<String>,
    ) {
        self.summaries = summaries;
        if let Some(error) = error {
            self.stale = true;
            self.error = Some(error);
        }
    }

    /// 保存当前选择对应的最新详情并清除 stale 标记。
    pub(crate) fn set_details(&mut self, details: RuntimeTaskDetailsView) {
        self.details = Some(details);
        self.stale = false;
        self.error = None;
    }

    /// 保留最后详情，仅把当前选择标记为过期并公开原因。
    pub(crate) fn mark_stale(&mut self, error: impl Into<String>) {
        self.stale = true;
        self.error = Some(error.into());
    }
}

/// 设置诊断页当前节点、分页结果和内存清理摘要。
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct FileFaultDiagnosticsState {
    /// 当前选择节点。
    pub selected_node_index: Option<usize>,
    /// 选择时冻结的机器身份；离线后保留以防跨节点结果混入。
    pub selected_machine_id: Option<String>,
    /// 当前已加载记录。
    pub rows: Vec<FileFaultView>,
    /// 空字符串表示没有下一页。
    pub next_cursor: String,
    /// Node 最近磁盘满清理摘要。
    pub cleanup_summary: Option<DiskFullCleanupSummaryView>,
    /// 是否正在加载或清除。
    pub loading: bool,
    /// 最近诊断命令错误。
    pub error: Option<String>,
}

/// 手工编辑节点或保存设置时的唯一边界错误。
#[derive(Debug, Error)]
pub enum ViewStateError {
    /// IP 文本不能解析为 IPv4/IPv6。
    #[error("IP 地址无效: {0}")]
    InvalidIp(String),
    /// 节点端口不能为零。
    #[error("节点端口不能为 0")]
    InvalidPort,
    /// 节点索引已经不存在。
    #[error("节点索引不存在: {0}")]
    MissingNode(usize),
    /// 相同 IP:port 已经配置。
    #[error("节点地址已经存在")]
    DuplicateNode,
    /// 配置阈值或重连间隔无效。
    #[error(transparent)]
    Core(#[from] CoreError),
}

fn clear_file_fault_page(state: &mut FileFaultDiagnosticsState) {
    state.rows.clear();
    state.next_cursor.clear();
    state.cleanup_summary = None;
    state.loading = false;
    state.error = None;
}

/// Slint UI 每次整体替换的管理端状态快照。
#[derive(Clone, Debug, PartialEq)]
pub struct DesktopViewState {
    config: DesktopConfig,
    paths: DesktopPaths,
    nodes: Vec<NodeView>,
    tasks: Vec<TaskView>,
    postgres: PostgresHealth,
    node_config: NodeConfigControllerState,
    file_faults: FileFaultDiagnosticsState,
}

impl DesktopViewState {
    /// 从已验证配置与绝对应用路径创建初始离线视图。
    pub fn new(config: DesktopConfig, paths: DesktopPaths) -> Self {
        let postgres = if config.postgres_url.is_some() {
            PostgresHealth::Connecting
        } else {
            PostgresHealth::Disabled
        };
        let nodes = config.nodes.iter().cloned().map(offline_node).collect();
        Self {
            config,
            paths,
            nodes,
            tasks: Vec::new(),
            postgres,
            node_config: NodeConfigControllerState::default(),
            file_faults: FileFaultDiagnosticsState::default(),
        }
    }

    /// 返回当前已经过边界校验的桌面配置。
    pub const fn config(&self) -> &DesktopConfig {
        &self.config
    }

    /// 返回诊断页显示的绝对路径。
    pub const fn paths(&self) -> &DesktopPaths {
        &self.paths
    }

    /// 返回按配置顺序排列的节点行。
    pub fn nodes(&self) -> &[NodeView] {
        &self.nodes
    }

    /// 返回按最近事件更新的任务列表。
    pub fn tasks(&self) -> &[TaskView] {
        &self.tasks
    }

    /// 返回远程 Node 配置加载与保存状态。
    pub const fn node_config(&self) -> &NodeConfigControllerState {
        &self.node_config
    }

    /// 返回设置页文件故障诊断状态。
    pub const fn file_faults(&self) -> &FileFaultDiagnosticsState {
        &self.file_faults
    }

    pub(crate) fn select_file_fault_node(&mut self, index: usize, machine_id: String) {
        self.file_faults = FileFaultDiagnosticsState {
            selected_node_index: Some(index),
            selected_machine_id: Some(machine_id),
            ..FileFaultDiagnosticsState::default()
        };
    }

    pub(crate) fn set_file_faults(&mut self, state: FileFaultDiagnosticsState) {
        self.file_faults = state;
    }

    /// 切换设置页节点时立即清除旧表单、摘要和保存状态。
    pub(crate) fn select_node_config(&mut self, index: usize) {
        self.node_config = NodeConfigControllerState {
            selected_node_index: Some(index),
            ..NodeConfigControllerState::default()
        };
    }

    /// 保存一次已验证归属的远程配置快照。
    pub(crate) fn set_node_config_snapshot(
        &mut self,
        index: usize,
        endpoint: NodeEndpoint,
        machine_id: String,
        snapshot: proto::NodeConfigSnapshot,
    ) {
        self.node_config = NodeConfigControllerState {
            selected_node_index: Some(index),
            target_machine_id: Some(machine_id),
            target_endpoint: Some(endpoint),
            snapshot: Some(snapshot),
            phase: NodeConfigSavePhase::Idle,
            saved_version_sha256: None,
            error: None,
        };
    }

    /// 更新保存阶段并在非失败阶段清除旧错误。
    pub(crate) fn set_node_config_phase(&mut self, phase: NodeConfigSavePhase) {
        self.node_config.phase = phase;
        if phase != NodeConfigSavePhase::Failed {
            self.node_config.error = None;
        }
    }

    /// 冻结保存目标及 Node 接受的新摘要。
    pub(crate) fn set_node_config_save_target(
        &mut self,
        machine_id: String,
        endpoint: NodeEndpoint,
        saved_version_sha256: String,
    ) {
        self.node_config.target_machine_id = Some(machine_id);
        self.node_config.target_endpoint = Some(endpoint);
        self.node_config.saved_version_sha256 = Some(saved_version_sha256);
    }

    /// 用重连验证得到的新快照完成状态机。
    pub(crate) fn complete_node_config(&mut self, snapshot: proto::NodeConfigSnapshot) {
        self.node_config.snapshot = Some(snapshot);
        self.node_config.phase = NodeConfigSavePhase::Completed;
        self.node_config.error = None;
    }

    /// 保留明确错误并进入 Failed。
    pub(crate) fn fail_node_config(&mut self, message: impl Into<String>) {
        self.node_config.phase = NodeConfigSavePhase::Failed;
        self.node_config.error = Some(message.into());
    }

    /// 仅供单向事件归并器更新已有任务；UI 只能读取克隆快照。
    pub fn tasks_mut(&mut self) -> &mut [TaskView] {
        &mut self.tasks
    }

    /// 解析并追加一个手工 IP:port，返回稳定列表索引。
    pub fn add_node(&mut self, ip: &str, port: u16) -> Result<usize, ViewStateError> {
        let endpoint = endpoint(ip, port)?;
        self.ensure_unique(&endpoint, None)?;
        self.config.nodes.push(endpoint.clone());
        self.nodes.push(offline_node(endpoint));
        Ok(self.nodes.len() - 1)
    }

    /// 修改指定手工节点地址并清空旧会话运行态。
    pub fn edit_node(&mut self, index: usize, ip: &str, port: u16) -> Result<(), ViewStateError> {
        if index >= self.nodes.len() {
            return Err(ViewStateError::MissingNode(index));
        }
        let endpoint = endpoint(ip, port)?;
        self.ensure_unique(&endpoint, Some(index))?;
        self.config.nodes[index] = endpoint.clone();
        self.nodes[index] = offline_node(endpoint);
        self.node_config = NodeConfigControllerState::default();
        Ok(())
    }

    /// 移除节点和其视图行；已存在任务保留历史但不再可操作。
    pub fn remove_node(&mut self, index: usize) -> Result<(), ViewStateError> {
        if index >= self.nodes.len() {
            return Err(ViewStateError::MissingNode(index));
        }
        self.config.nodes.remove(index);
        self.nodes.remove(index);
        self.node_config = NodeConfigControllerState::default();
        Ok(())
    }

    /// 应用一次连接状态和可选 NodeStatus/同步统计。
    pub fn set_node_connection(
        &mut self,
        index: usize,
        connection: NodeConnectionState,
        stats: Option<NodeRuntimeStats>,
    ) {
        if let Some(node) = self.nodes.get_mut(index) {
            node.connection = connection;
            node.stats = stats;
            if connection != NodeConnectionState::Error {
                node.error_text = None;
            }
        }
        if self.file_faults.selected_node_index == Some(index)
            && connection != NodeConnectionState::Online
        {
            clear_file_fault_page(&mut self.file_faults);
        }
    }

    /// 保存握手取得的物理机器 ID；配置文件仍只保存手工 IP:port。
    pub fn set_node_identity(&mut self, index: usize, machine_id: impl Into<String>) {
        let machine_id = machine_id.into();
        if let Some(node) = self.nodes.get_mut(index) {
            node.machine_id = Some(machine_id.clone());
        }
        if self.file_faults.selected_node_index == Some(index)
            && self.file_faults.selected_machine_id.as_deref() != Some(machine_id.as_str())
        {
            self.file_faults.selected_machine_id = Some(machine_id);
            clear_file_fault_page(&mut self.file_faults);
        }
    }

    /// 保存一个节点错误并使该行进入 Error 状态。
    pub fn set_node_error(&mut self, index: usize, message: impl Into<String>) {
        if let Some(node) = self.nodes.get_mut(index) {
            node.connection = NodeConnectionState::Error;
            node.error_text = Some(message.into());
            node.stats = None;
        }
        if self.file_faults.selected_node_index == Some(index) {
            clear_file_fault_page(&mut self.file_faults);
        }
    }

    /// 按任务 ID 替换已有进度或追加新任务。
    pub fn upsert_task(&mut self, task: TaskView) {
        if let Some(current) = self
            .tasks
            .iter_mut()
            .find(|current| current.task_id == task.task_id)
        {
            *current = task;
        } else {
            self.tasks.push(task);
        }
    }

    /// 设置 PG 诊断状态；SchemaMissing 不改变节点本地能力。
    pub fn set_postgres_health(&mut self, health: PostgresHealth) {
        self.postgres = health;
    }

    /// 返回 PG 状态对应的直接诊断文本。
    pub fn postgres_message(&self) -> String {
        match &self.postgres {
            PostgresHealth::Disabled => "未配置 PostgreSQL；仅启用单机功能".into(),
            PostgresHealth::Connecting => "正在连接并校验 PostgreSQL schema".into(),
            PostgresHealth::Ready => "PostgreSQL V2 schema 正常".into(),
            PostgresHealth::SchemaMissing => {
                "中心 schema 缺失，请手动执行 schema/central-v2.sql".into()
            }
            PostgresHealth::Error(error) => format!("PostgreSQL 不可用：{error}"),
        }
    }

    /// 本地模式不依赖 PostgreSQL；至少保留一个手工节点即可进入。
    pub fn local_mode_enabled(&self) -> bool {
        !self.nodes.is_empty()
    }

    /// 中心模式只在 PG schema 完整且至少一个节点在线时启用。
    pub fn central_mode_enabled(&self) -> bool {
        self.postgres == PostgresHealth::Ready
            && self
                .nodes
                .iter()
                .any(|node| node.connection == NodeConnectionState::Online)
    }

    /// 任一节点仍有 queued/running 工作时，统一禁用本地和中心筛选。
    pub fn filtering_availability(&self) -> ActionAvailability {
        let busy = self
            .tasks
            .iter()
            .any(|task| matches!(task.state, ViewTaskState::Queued | ViewTaskState::Running))
            || self.nodes.iter().any(|node| {
                node.stats
                    .as_ref()
                    .is_some_and(|stats| stats.queued_items > 0 || stats.running_items > 0)
            });
        if busy {
            ActionAvailability {
                enabled: false,
                reason: "等待所有节点计算完成后才能开始筛选".into(),
            }
        } else {
            ActionAvailability {
                enabled: true,
                reason: String::new(),
            }
        }
    }

    /// 验证完整设置后一次替换；失败时保留旧配置和视图。
    pub fn apply_settings(&mut self, config: DesktopConfig) -> Result<(), ViewStateError> {
        config.validate()?;
        let nodes = config.nodes.iter().cloned().map(offline_node).collect();
        self.config = config;
        self.nodes = nodes;
        self.postgres = if self.config.postgres_url.is_some() {
            PostgresHealth::Connecting
        } else {
            PostgresHealth::Disabled
        };
        self.node_config = NodeConfigControllerState::default();
        Ok(())
    }

    fn ensure_unique(
        &self,
        endpoint: &NodeEndpoint,
        editing: Option<usize>,
    ) -> Result<(), ViewStateError> {
        if self
            .nodes
            .iter()
            .enumerate()
            .any(|(index, node)| Some(index) != editing && node.endpoint == *endpoint)
        {
            Err(ViewStateError::DuplicateNode)
        } else {
            Ok(())
        }
    }
}

fn endpoint(ip: &str, port: u16) -> Result<NodeEndpoint, ViewStateError> {
    if port == 0 {
        return Err(ViewStateError::InvalidPort);
    }
    Ok(NodeEndpoint {
        ip: IpAddr::from_str(ip).map_err(|_| ViewStateError::InvalidIp(ip.into()))?,
        port,
    })
}

fn offline_node(endpoint: NodeEndpoint) -> NodeView {
    NodeView {
        endpoint,
        connection: NodeConnectionState::Offline,
        machine_id: None,
        stats: None,
        error_text: None,
    }
}

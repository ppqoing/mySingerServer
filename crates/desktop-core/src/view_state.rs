//! 管理端不可变快照所依据的纯节点、任务、设置和诊断状态。

use std::{net::IpAddr, path::PathBuf, str::FromStr};

use dedup_core::{CoreError, DesktopConfig, NodeEndpoint};
use thiserror::Error;

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

/// Slint UI 每次整体替换的管理端状态快照。
#[derive(Clone, Debug, PartialEq)]
pub struct DesktopViewState {
    config: DesktopConfig,
    paths: DesktopPaths,
    nodes: Vec<NodeView>,
    tasks: Vec<TaskView>,
    postgres: PostgresHealth,
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
        Ok(())
    }

    /// 移除节点和其视图行；已存在任务保留历史但不再可操作。
    pub fn remove_node(&mut self, index: usize) -> Result<(), ViewStateError> {
        if index >= self.nodes.len() {
            return Err(ViewStateError::MissingNode(index));
        }
        self.config.nodes.remove(index);
        self.nodes.remove(index);
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
    }

    /// 保存握手取得的物理机器 ID；配置文件仍只保存手工 IP:port。
    pub fn set_node_identity(&mut self, index: usize, machine_id: impl Into<String>) {
        if let Some(node) = self.nodes.get_mut(index) {
            node.machine_id = Some(machine_id.into());
        }
    }

    /// 保存一个节点错误并使该行进入 Error 状态。
    pub fn set_node_error(&mut self, index: usize, message: impl Into<String>) {
        if let Some(node) = self.nodes.get_mut(index) {
            node.connection = NodeConnectionState::Error;
            node.error_text = Some(message.into());
            node.stats = None;
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

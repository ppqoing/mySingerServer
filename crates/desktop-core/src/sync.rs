//! 中心游标优先 ACK、1000 条增量事务和整次回滚快照的单一路径同步器。

use dedup_core::MachineId;
use dedup_protocol::proto;
use thiserror::Error;
use tokio::{
    sync::{Mutex, mpsc},
    time::{Duration, interval},
};

use crate::node_session::{NodeSession, SessionError};

/// 节点增量和快照每次固定拉取的最大行数。
pub const SYNC_BATCH_SIZE: u32 = 1000;
/// 自动追赶检查固定间隔；连接成功和任务完成是另外两个自动触发点。
pub const AUTO_CATCH_UP_INTERVAL_SECONDS: u64 = 5;
/// 全量快照的固定表顺序；原媒体与本地联系表引用不进入中心数据库。
pub const SNAPSHOT_TABLES: &[&str] = &[
    "contents",
    "files",
    "image_stage1",
    "image_stage2",
    "video_metadata",
    "video_frame_stage1",
    "video_frame_stage2",
    "deletion_tombstones",
];

/// 同一同步循环的来源只影响 UI 文案，不改变事务、ACK 或重试语义。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SyncTrigger {
    /// 连接成功、任务完成或五秒追赶检查触发。
    Automatic,
    /// 用户点击“立即同步”后进入同一等待队列。
    Manual,
}

/// 每个节点唯一同步触发通道的可克隆发送端。
#[derive(Clone)]
pub struct SyncTriggerSender {
    sender: mpsc::Sender<SyncTrigger>,
}

/// 每个节点唯一同步触发通道的接收端；会话监督器顺序消费并调用 `sync_node`。
pub struct SyncTriggerReceiver {
    receiver: mpsc::Receiver<SyncTrigger>,
}

/// 同步触发通道已经随节点会话结束。
#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("节点同步触发通道已关闭")]
pub struct SyncTriggerClosed;

/// 创建一个有界节点级触发通道；自动与手动请求共享相同队列。
pub fn sync_trigger_channel(capacity: usize) -> (SyncTriggerSender, SyncTriggerReceiver) {
    let (sender, receiver) = mpsc::channel(capacity);
    (
        SyncTriggerSender { sender },
        SyncTriggerReceiver { receiver },
    )
}

impl SyncTriggerSender {
    /// 节点握手成功后排入一次自动同步。
    pub async fn connected(&self) -> Result<(), SyncTriggerClosed> {
        self.send(SyncTrigger::Automatic).await
    }

    /// 收到节点任务完成事件后排入一次自动同步。
    pub async fn task_completed(&self) -> Result<(), SyncTriggerClosed> {
        self.send(SyncTrigger::Automatic).await
    }

    /// 五秒追赶定时器触发时排入一次自动同步。
    pub async fn catch_up_tick(&self) -> Result<(), SyncTriggerClosed> {
        self.send(SyncTrigger::Automatic).await
    }

    /// 用户点击“立即同步”后排入同一通道。
    pub async fn manual(&self) -> Result<(), SyncTriggerClosed> {
        self.send(SyncTrigger::Manual).await
    }

    /// 持续以固定五秒间隔产生唯一允许的周期触发，通道关闭后自然退出。
    pub async fn run_catch_up_timer(self) {
        let mut ticks = interval(Duration::from_secs(AUTO_CATCH_UP_INTERVAL_SECONDS));
        ticks.tick().await;
        loop {
            ticks.tick().await;
            if self.catch_up_tick().await.is_err() {
                break;
            }
        }
    }

    async fn send(&self, trigger: SyncTrigger) -> Result<(), SyncTriggerClosed> {
        self.sender
            .send(trigger)
            .await
            .map_err(|_| SyncTriggerClosed)
    }
}

impl SyncTriggerReceiver {
    /// 等待下一次自动或手动同步请求；发送端全部关闭时返回 `None`。
    pub async fn next(&mut self) -> Option<SyncTrigger> {
        self.receiver.recv().await
    }
}

/// 管理界面显示的同步阶段。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SyncPhase {
    /// 正在把中心持久游标先 ACK 给节点。
    Acknowledging,
    /// 正在拉取并提交最多 1000 条增量。
    Incremental,
    /// 增量已裁剪，正在整次读取固定快照。
    Snapshot,
    /// 节点与 PostgreSQL 游标已经追平。
    CaughtUp,
}

/// 一次同步结束后供 UI 和日志使用的确定性摘要。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SyncReport {
    /// 自动或手动触发源。
    pub trigger: SyncTrigger,
    /// PostgreSQL 最终提交序号。
    pub committed_seq: u64,
    /// 本轮观察到的节点最高序号。
    pub node_high_seq: u64,
    /// 成功提交的增量批次数。
    pub batch_count: u64,
    /// 成功提交的增量变更数，不含快照行。
    pub change_count: u64,
    /// 成功读取并写入事务的快照页数。
    pub snapshot_page_count: u64,
}

/// 同步运行中的不可变进度快照；UI 可直接替换旧值而不读取网络或数据库。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SyncProgress {
    /// 自动或手动触发源。
    pub trigger: SyncTrigger,
    /// 当前 ACK、增量、快照或追平阶段。
    pub phase: SyncPhase,
    /// 当前已持久提交中心序号。
    pub committed_seq: u64,
    /// 本轮观察到的节点高水位。
    pub node_high_seq: u64,
    /// 已提交增量批次数。
    pub batch_count: u64,
    /// 已提交增量行数。
    pub change_count: u64,
    /// 已写入当前完整快照事务的页数。
    pub snapshot_page_count: u64,
}

/// 节点会话、中心事务或快照协议错误。
#[derive(Debug, Error)]
pub enum SyncError {
    /// TCP 会话或节点协议错误。
    #[error(transparent)]
    Session(#[from] SessionError),
    /// PostgreSQL 中心存储错误。
    #[error(transparent)]
    Central(#[from] crate::central::CentralError),
    /// 节点已裁剪中心需要的增量，必须改走全量快照。
    #[error("节点增量已经裁剪，需要全量快照")]
    SnapshotRequired,
    /// fake、协调器或持久化适配器提供的明确失败原因。
    #[error("同步后端失败: {0}")]
    Backend(String),
    /// 快照 token、表名或游标响应不符合当前请求。
    #[error("快照响应无效: {0}")]
    InvalidSnapshot(String),
}

/// 同步器依赖的最小节点协议，允许用内存 fake 验证提交/ACK 故障窗口。
#[allow(async_fn_in_trait)]
pub trait SyncNodeClient: Sync {
    /// 当前连接对应的稳定物理机器 ID。
    fn machine_id(&self) -> &MachineId;
    /// 把 PostgreSQL 已提交序号幂等 ACK 给节点。
    async fn acknowledge(&self, committed_seq: u64) -> Result<(), SyncError>;
    /// 拉取游标之后的有序增量。
    async fn pull_changes(
        &self,
        after_seq: u64,
        limit: u32,
    ) -> Result<proto::SyncChangeBatch, SyncError>;
    /// 开启新的固定快照；断线重连必须重新调用。
    async fn begin_snapshot(&self) -> Result<proto::BeginSnapshot, SyncError>;
    /// 读取快照表的一页。
    async fn read_snapshot_page(
        &self,
        request: proto::ReadSnapshotPage,
    ) -> Result<proto::ReadSnapshotPage, SyncError>;
}

/// 一个尚未提交的中心快照事务；Drop 必须回滚所有已写页面。
#[allow(async_fn_in_trait)]
pub trait SyncSnapshot {
    /// 把当前表的一页版本化载荷写入同一个中心事务。
    async fn apply_page(&mut self, table_name: &str, rows: &[Vec<u8>]) -> Result<(), SyncError>;
    /// 原子提交完整快照并把中心游标推进到 snapshot highwater。
    async fn commit(self) -> Result<u64, SyncError>;
}

/// 同步器依赖的中心存储接口；生产实现是唯一拥有 PostgreSQL 的 `CentralStore`。
#[allow(async_fn_in_trait)]
pub trait SyncRepository {
    /// 与一次借用绑定的快照事务类型。
    type Snapshot<'a>: SyncSnapshot
    where
        Self: 'a;

    /// 读取指定机器已持久提交的中心游标。
    async fn cursor(&self, machine_id: &MachineId) -> Result<u64, SyncError>;
    /// 原子提交一批增量并返回新的中心游标。
    async fn apply_batch(
        &mut self,
        machine_id: &MachineId,
        batch: &proto::SyncChangeBatch,
    ) -> Result<u64, SyncError>;
    /// 开始“先失效旧位置、再写全量行、最后推进游标”的中心事务。
    async fn begin_snapshot(
        &mut self,
        machine_id: &MachineId,
        snapshot_high_seq: u64,
    ) -> Result<Self::Snapshot<'_>, SyncError>;
}

/// 每个节点持有一个实例；内部互斥锁使自动与手动触发只排队运行一条同步路径。
#[derive(Default)]
pub struct SyncEngine {
    gate: Mutex<()>,
}

impl SyncEngine {
    /// 创建一个尚未运行同步的节点级引擎。
    pub const fn new() -> Self {
        Self {
            gate: Mutex::const_new(()),
        }
    }

    /// 先 ACK 中心游标，再反复提交 1000 条增量；需要时整次替换快照。
    pub async fn sync_node<N, R>(
        &self,
        node: &N,
        repository: &mut R,
        trigger: SyncTrigger,
    ) -> Result<SyncReport, SyncError>
    where
        N: SyncNodeClient,
        R: SyncRepository,
    {
        self.sync_node_with_progress(node, repository, trigger, |_| {})
            .await
    }

    /// 执行同一同步逻辑，并在阶段或已提交计数变化时发布轻量进度快照。
    pub async fn sync_node_with_progress<N, R, F>(
        &self,
        node: &N,
        repository: &mut R,
        trigger: SyncTrigger,
        mut publish: F,
    ) -> Result<SyncReport, SyncError>
    where
        N: SyncNodeClient,
        R: SyncRepository,
        F: FnMut(SyncProgress),
    {
        let _single_loop = self.gate.lock().await;
        let machine_id = node.machine_id();
        let mut committed_seq = repository.cursor(machine_id).await?;
        let mut report = SyncReport {
            trigger,
            committed_seq,
            node_high_seq: committed_seq,
            batch_count: 0,
            change_count: 0,
            snapshot_page_count: 0,
        };
        publish(progress(report, SyncPhase::Acknowledging));
        node.acknowledge(committed_seq).await?;

        loop {
            publish(progress(report, SyncPhase::Incremental));
            let batch = match node.pull_changes(committed_seq, SYNC_BATCH_SIZE).await {
                Ok(batch) => batch,
                Err(SyncError::SnapshotRequired) => {
                    publish(progress(report, SyncPhase::Snapshot));
                    let (snapshot_seq, pages) = replace_from_snapshot(node, repository).await?;
                    committed_seq = snapshot_seq;
                    report.committed_seq = snapshot_seq;
                    report.node_high_seq = report.node_high_seq.max(snapshot_seq);
                    report.snapshot_page_count += pages;
                    publish(progress(report, SyncPhase::Acknowledging));
                    node.acknowledge(snapshot_seq).await?;
                    continue;
                }
                Err(error) => return Err(error),
            };
            report.node_high_seq = report.node_high_seq.max(batch.high_seq);
            if batch.changes.is_empty() {
                report.committed_seq = committed_seq;
                publish(progress(report, SyncPhase::CaughtUp));
                return Ok(report);
            }
            let change_count = batch.changes.len() as u64;
            committed_seq = repository.apply_batch(machine_id, &batch).await?;
            report.committed_seq = committed_seq;
            report.batch_count += 1;
            report.change_count += change_count;
            publish(progress(report, SyncPhase::Acknowledging));
            node.acknowledge(committed_seq).await?;
        }
    }
}

const fn progress(report: SyncReport, phase: SyncPhase) -> SyncProgress {
    SyncProgress {
        trigger: report.trigger,
        phase,
        committed_seq: report.committed_seq,
        node_high_seq: report.node_high_seq,
        batch_count: report.batch_count,
        change_count: report.change_count,
        snapshot_page_count: report.snapshot_page_count,
    }
}

async fn replace_from_snapshot<N, R>(node: &N, repository: &mut R) -> Result<(u64, u64), SyncError>
where
    N: SyncNodeClient,
    R: SyncRepository,
{
    let begin = node.begin_snapshot().await?;
    if begin.snapshot_token.is_empty() {
        return Err(SyncError::InvalidSnapshot("节点返回空 token".into()));
    }
    let mut transaction = repository
        .begin_snapshot(node.machine_id(), begin.snapshot_high_seq)
        .await?;
    let mut page_count = 0;
    for table_name in SNAPSHOT_TABLES {
        let mut cursor = String::new();
        loop {
            let page = node
                .read_snapshot_page(proto::ReadSnapshotPage {
                    snapshot_token: begin.snapshot_token.clone(),
                    table_name: (*table_name).into(),
                    cursor: cursor.clone(),
                    limit: SYNC_BATCH_SIZE,
                    rows: Vec::new(),
                    next_cursor: String::new(),
                    done: false,
                })
                .await?;
            if page.snapshot_token != begin.snapshot_token || page.table_name != *table_name {
                return Err(SyncError::InvalidSnapshot(
                    "token 或表名与请求不一致".into(),
                ));
            }
            transaction.apply_page(table_name, &page.rows).await?;
            page_count += 1;
            if page.done {
                break;
            }
            if page.next_cursor.is_empty() || page.next_cursor == cursor {
                return Err(SyncError::InvalidSnapshot("非末页没有前进游标".into()));
            }
            cursor = page.next_cursor;
        }
    }
    let committed = transaction.commit().await?;
    Ok((committed, page_count))
}

#[allow(async_fn_in_trait)]
impl SyncNodeClient for NodeSession {
    fn machine_id(&self) -> &MachineId {
        self.machine_id()
    }

    async fn acknowledge(&self, committed_seq: u64) -> Result<(), SyncError> {
        NodeSession::acknowledge(self, committed_seq)
            .await
            .map_err(map_session_error)
    }

    async fn pull_changes(
        &self,
        after_seq: u64,
        limit: u32,
    ) -> Result<proto::SyncChangeBatch, SyncError> {
        NodeSession::pull_changes(self, after_seq, limit)
            .await
            .map_err(map_session_error)
    }

    async fn begin_snapshot(&self) -> Result<proto::BeginSnapshot, SyncError> {
        NodeSession::begin_snapshot(self)
            .await
            .map_err(map_session_error)
    }

    async fn read_snapshot_page(
        &self,
        request: proto::ReadSnapshotPage,
    ) -> Result<proto::ReadSnapshotPage, SyncError> {
        NodeSession::read_snapshot_page(self, request)
            .await
            .map_err(map_session_error)
    }
}

fn map_session_error(error: SessionError) -> SyncError {
    match error {
        SessionError::Protocol { code, .. }
            if code == proto::ErrorCode::SnapshotRequired as i32 =>
        {
            SyncError::SnapshotRequired
        }
        other => SyncError::Session(other),
    }
}

#[allow(async_fn_in_trait)]
impl SyncRepository for crate::central::CentralStore {
    type Snapshot<'a> = crate::central::CentralSnapshot<'a>;

    async fn cursor(&self, machine_id: &MachineId) -> Result<u64, SyncError> {
        Ok(self.sync_cursor(machine_id).await?)
    }

    async fn apply_batch(
        &mut self,
        machine_id: &MachineId,
        batch: &proto::SyncChangeBatch,
    ) -> Result<u64, SyncError> {
        Ok(self.apply_sync_batch(machine_id, batch).await?)
    }

    async fn begin_snapshot(
        &mut self,
        machine_id: &MachineId,
        snapshot_high_seq: u64,
    ) -> Result<Self::Snapshot<'_>, SyncError> {
        Ok(self
            .begin_snapshot_replace(machine_id, snapshot_high_seq)
            .await?)
    }
}

#[allow(async_fn_in_trait)]
impl SyncSnapshot for crate::central::CentralSnapshot<'_> {
    async fn apply_page(&mut self, table_name: &str, rows: &[Vec<u8>]) -> Result<(), SyncError> {
        Ok(crate::central::CentralSnapshot::apply_page(self, table_name, rows).await?)
    }

    async fn commit(self) -> Result<u64, SyncError> {
        Ok(crate::central::CentralSnapshot::commit(self).await?)
    }
}

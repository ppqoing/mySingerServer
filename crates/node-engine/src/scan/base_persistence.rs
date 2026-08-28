//! 基础计算任务独占的 SQLite 单写 actor、同步查询句柄和异步终态 ACK。

#[cfg(feature = "test-hooks")]
use std::sync::{
    Arc, Condvar, Mutex,
    atomic::{AtomicBool, Ordering},
};
#[cfg(all(test, not(feature = "test-hooks")))]
use std::sync::{Arc, Mutex};
use std::{
    sync::mpsc::{self, Receiver, RecvTimeoutError, SyncSender, TryRecvError, TrySendError},
    thread,
    time::{Duration, Instant},
};

use dedup_core::{ContentKey, MachineId, MediaKind, TaskId};
use dedup_node_store::{
    BaseCacheRecord, ClaimedTaskItem, ContentId, ContentRecord, NodeStore, ScannedPath, StoreError,
    SyncBatch, SyncState, TaskItemIdentity, TaskSnapshot, TaskStageWrite,
};
#[cfg(feature = "test-hooks")]
use tokio::sync::Notify;
use tokio::sync::mpsc::{UnboundedReceiver, UnboundedSender};

use super::ScanError;
use crate::task_files::TaskFileIdentity;

/// actor 中执行的一次拥有型同步 Store 调用。
type StoreCall = Box<dyn FnOnce(&mut NodeStore) -> bool + Send + 'static>;
/// actor 中执行的一次拥有型终态持久化操作。
type PersistOperation =
    Box<dyn FnOnce(&mut NodeStore) -> Result<BasePersistOutcome, String> + Send + 'static>;

#[cfg(feature = "test-hooks")]
#[derive(Default)]
/// 测试控制端与 actor waiter 共享的通知状态。
struct BasePersistTestState {
    /// actor 已取得首条持久化消息的异步通知。
    entered: Notify,
    /// 是否已经放行；重复 release 和先 release 后 enter 均安全。
    released: Mutex<bool>,
    /// actor 阻塞线程等待控制端放行。
    release_signal: Condvar,
    /// `BaseStoreActor::finish` 已真实 join writer 线程。
    writer_joined: AtomicBool,
}

#[cfg(feature = "test-hooks")]
#[doc(hidden)]
/// 测试专用控制端；显式 release 可重复，Drop 会兜底自动放行。
pub struct BasePersistTestController {
    /// 仅控制端拥有此类型；actor 只持有独立 waiter。
    shared: Arc<BasePersistTestState>,
}

#[cfg(feature = "test-hooks")]
#[doc(hidden)]
/// actor 专用首条持久化 waiter，不具备控制端 Drop 语义。
pub struct BasePersistTestWaiter {
    /// 与唯一控制端共享进入和放行状态。
    shared: Arc<BasePersistTestState>,
}

#[cfg(feature = "test-hooks")]
impl BasePersistTestController {
    /// 创建彼此分离的控制端和 actor waiter。
    pub fn new() -> (Self, BasePersistTestWaiter) {
        let shared = Arc::new(BasePersistTestState::default());
        (
            Self {
                shared: Arc::clone(&shared),
            },
            BasePersistTestWaiter { shared },
        )
    }

    /// 等待 actor 已取得首条消息但尚未执行 SQLite 操作。
    pub async fn wait_until_entered(&self) {
        self.shared.entered.notified().await;
    }

    /// 放行首条持久化，供测试继续等待真实 Applied ACK。
    pub fn release(&self) {
        *self
            .shared
            .released
            .lock()
            .expect("首条持久化 gate 锁不应中毒") = true;
        self.shared.release_signal.notify_all();
    }

    /// 返回 task-local writer 是否已经被 `finish` 真实 join。
    pub fn writer_joined(&self) -> bool {
        self.shared.writer_joined.load(Ordering::Acquire)
    }
}

#[cfg(feature = "test-hooks")]
impl Drop for BasePersistTestController {
    /// 测试 panic、取消或提前返回时自动解除 actor 阻塞。
    fn drop(&mut self) {
        self.release();
    }
}

#[cfg(feature = "test-hooks")]
impl BasePersistTestWaiter {
    /// 在 actor 阻塞线程报告 entered，并等待测试显式放行。
    fn enter_and_wait(self) {
        self.shared.entered.notify_one();
        let guard = self
            .shared
            .released
            .lock()
            .expect("首条持久化 gate 锁不应中毒");
        drop(
            self.shared
                .release_signal
                .wait_while(guard, |released| !*released)
                .expect("首条持久化 gate 等待不应中毒"),
        );
    }
}

/// 每条 persist 前的静态分派 hook；默认实现编译为空操作。
trait BeforePersist: Send + 'static {
    /// 在 actor 执行拥有型持久化操作前运行。
    fn before_persist(&mut self);
}

/// 默认生产路径的零大小空 hook。
struct NoBeforePersist;

impl BeforePersist for NoBeforePersist {
    #[inline(always)]
    fn before_persist(&mut self) {}
}

/// 基础计算持久化消息的拥有型身份。
///
/// `Legacy` 兼容尚未迁移的 SQLite 任务调用；`TaskFile` 保存 TSV 行的完整身份，
/// 包括运行、物理盘 lane、行偏移、行长度、任务项和缺失掩码，绝不退化为单独的 item ID。
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) enum BasePersistIdentity {
    /// 旧任务表路径使用的任务项身份。
    Legacy(TaskItemIdentity),
    /// 瞬态任务文件路径使用的完整行身份。
    TaskFile(TaskFileIdentity),
}

impl BasePersistIdentity {
    /// 返回用于兼容旧日志和运行时 map 的任务项字符串。
    pub(crate) fn item_id(&self) -> String {
        match self {
            Self::Legacy(identity) => identity.item_id.clone(),
            Self::TaskFile(identity) => identity.item_id().to_string(),
        }
    }

    /// 返回旧任务身份中的任务 ID；瞬态任务文件身份没有合成的任务 ID。
    pub(crate) fn task_id(&self) -> Option<&TaskId> {
        match self {
            Self::Legacy(identity) => Some(&identity.task_id),
            Self::TaskFile(_) => None,
        }
    }

    /// 借用瞬态任务文件的完整行身份；旧身份返回 `None`。
    pub(crate) fn task_file_identity(&self) -> Option<&TaskFileIdentity> {
        match self {
            Self::Legacy(_) => None,
            Self::TaskFile(identity) => Some(identity),
        }
    }

    /// 消费身份并取出瞬态任务文件的完整行身份；旧身份返回 `None`。
    pub(crate) fn into_task_file(self) -> Option<TaskFileIdentity> {
        match self {
            Self::Legacy(_) => None,
            Self::TaskFile(identity) => Some(identity),
        }
    }
}

#[cfg(feature = "test-hooks")]
/// feature 专用首条 persist hook；后续消息不再检查控制状态。
struct FirstPersistTestHook(Option<BasePersistTestWaiter>);

#[cfg(feature = "test-hooks")]
impl BeforePersist for FirstPersistTestHook {
    fn before_persist(&mut self) {
        if let Some(waiter) = self.0.take() {
            waiter.enter_and_wait();
        }
    }
}

/// 一条不携带 Worker、CPU 或媒体许可的拥有型持久化消息。
pub(crate) struct BasePersistMessage {
    /// 用于 ACK 精确归并的 task/item/content 身份。
    pub(crate) identity: BasePersistIdentity,
    /// 协调器创建消息的单调时刻，用于计算真实持久化排队等待。
    enqueued_at: Instant,
    /// actor 独占 NodeStore 后执行的事务操作。
    operation: PersistOperation,
}

impl BasePersistMessage {
    /// 把已经释放计算资源的拥有型数据封装为单写操作。
    pub(crate) fn new<F>(identity: TaskItemIdentity, operation: F) -> Self
    where
        F: FnOnce(&mut NodeStore) -> Result<BasePersistOutcome, String> + Send + 'static,
    {
        Self {
            identity: BasePersistIdentity::Legacy(identity),
            enqueued_at: Instant::now(),
            operation: Box::new(operation),
        }
    }

    /// 把瞬态任务文件的完整身份封装为单写操作，不追加或改写任务文件行。
    pub(crate) fn new_task_file<F>(identity: TaskFileIdentity, operation: F) -> Self
    where
        F: FnOnce(&mut NodeStore) -> Result<BasePersistOutcome, String> + Send + 'static,
    {
        Self {
            identity: BasePersistIdentity::TaskFile(identity),
            enqueued_at: Instant::now(),
            operation: Box::new(operation),
        }
    }
}

/// SQLite 已确认后的运行时汇总动作；协调器收到 ACK 后才可应用。
pub(crate) enum BasePersistOutcome {
    /// 一个文件成功持久化。
    Succeeded {
        worker_slot: Option<u32>,
        /// 仅 path 完整缓存命中在 Applied ACK 后增加缓存命中汇总。
        cache_hit: bool,
        /// Applied ACK 确认的真实媒体类型，用于内存吞吐分桶。
        media_kind: MediaKind,
        /// 枚举阶段冻结的真实文件大小，用于内存吞吐分桶。
        file_size: u64,
    },
    /// 一个文件失败持久化，并保留显示路径和诊断。
    Failed {
        display_path: String,
        message: String,
        worker_slot: Option<u32>,
        skipped_incomplete: bool,
    },
    /// 活动项确认取消；只推进 skipped，不报告成功或文件失败。
    Cancelled { worker_slot: Option<u32> },
    /// 取消或晚到结果被事务门禁忽略。
    Ignored,
}

impl BasePersistOutcome {
    /// 判断该 ACK 是否代表具体 item 已 Applied；Ignored 不应记录完成时延。
    pub(crate) const fn is_applied(&self) -> bool {
        !matches!(self, Self::Ignored)
    }
}

/// actor 对一条持久化消息的完成回执。
pub(crate) struct BasePersistAck {
    /// 回执对应的精确身份。
    pub(crate) identity: BasePersistIdentity,
    /// 从协调器创建消息到 actor 取得消息的真实等待耗时。
    pub(crate) queue_wait: Duration,
    /// 不含测试 gate 的真实 SQLite 操作耗时。
    pub(crate) transaction_elapsed: Duration,
    /// 成功动作或导致 actor 停止的持久化错误。
    pub(crate) result: Result<BasePersistOutcome, String>,
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};

    use dedup_core::{ContentKey, DisplayPath, MachineId, NormalizedPath};
    use dedup_node_store::{NodeStore, ScannedPath};
    use dedup_windows::{LocalDiskKind, PhysicalDiskId};
    use uuid::Uuid;

    use crate::{
        scan::TaskDiskLane,
        task_files::{
            TaskFileIdentity, TaskFileRecord, TaskWorkKind, TaskWorkMask, TransientTaskFileSet,
        },
    };

    use super::{
        BasePersistMessage, BasePersistOutcome, BasePersistSendError, BaseStoreActor,
        BaseStoreHandle,
    };

    /// ACK 的 Applied 与 Ignored 必须可区分，时延只记录 Applied 终态。
    #[test]
    fn applied_ack_is_distinguished_from_ignored_ack() {
        assert!(!BasePersistOutcome::Ignored.is_applied());
    }

    /// 批量缓存查询必须穿过同一个 Store actor，并按输入位置返回结果。
    #[tokio::test]
    async fn batch_cache_lookup_contract_preserves_path_and_key_positions() {
        let machine = MachineId::from_sha256([0xB1; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let (actor, handle, _acks) = BaseStoreActor::spawn(store, 2);
        let path = std::path::PathBuf::from(r"C:\batch-contract.bin");
        let scanned = ScannedPath::new(
            NormalizedPath::new(&path).unwrap(),
            DisplayPath::new(&path).unwrap(),
            17,
        );
        let key = ContentKey::new([0xB2; 16], 17);

        let path_results = handle
            .lookup_base_cache_by_paths(&[scanned.clone(), scanned.clone()])
            .unwrap();
        let key_results = handle.lookup_base_cache_by_keys(&[key, key]).unwrap();

        assert_eq!(path_results.len(), 2);
        assert_eq!(key_results.len(), 2);
        assert!(path_results.iter().all(Option::is_none));
        assert!(key_results.iter().all(Option::is_none));

        drop(handle);
        actor.finish().await.unwrap();
    }

    /// 任务文件身份经过单写 actor 后必须完整回传，不能退化为只有 item_id。
    #[tokio::test]
    async fn task_file_persist_ack_preserves_full_identity() {
        let root = tempfile::tempdir().unwrap();
        let run_id = Uuid::now_v7().to_string();
        let lane = TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([7]).unwrap(),
            physical_disk_numbers: vec![7],
            disk_kind: LocalDiskKind::Hdd,
            configured_weight: 1,
            per_disk_limit: 1,
        };
        let path = std::path::PathBuf::from(r"C:\task-file-persist.bin");
        let row = TaskFileRecord {
            item_id: Uuid::now_v7(),
            work_kind: TaskWorkKind::Base,
            scanned: ScannedPath::new(
                NormalizedPath::new(&path).unwrap(),
                DisplayPath::new(&path).unwrap(),
                17,
            ),
            known_md5: None,
            missing: TaskWorkMask::from_bits(1 << 3).unwrap(),
        };
        let mut files = TransientTaskFileSet::create(root.path(), &run_id).unwrap();
        let identity = files
            .append_batch(&lane, std::slice::from_ref(&row))
            .unwrap()[0]
            .clone();
        let (_, taken_record) = files.take_lane_exact(&identity, &row).unwrap().unwrap();
        assert_eq!(taken_record, row);

        let machine = MachineId::from_sha256([0xB3; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 1);
        let expected = identity.clone();
        assert!(
            handle
                .try_persist(BasePersistMessage::new_task_file(identity, |_store| {
                    Ok(BasePersistOutcome::Ignored)
                }))
                .is_ok()
        );

        let acknowledgement = acknowledgements.recv().await.unwrap();
        assert_eq!(
            acknowledgement.identity.task_file_identity(),
            Some(&expected)
        );
        assert_eq!(
            acknowledgement.identity.item_id(),
            expected.item_id().to_string()
        );
        assert_eq!(acknowledgement.identity.into_task_file(), Some(expected));

        drop(handle);
        actor.finish().await.unwrap();
    }

    /// 构造真实任务文件身份，供消息所有权和 ACK 测试复用。
    fn task_file_identities(count: usize) -> (tempfile::TempDir, Vec<TaskFileIdentity>) {
        let root = tempfile::tempdir().unwrap();
        let run_id = Uuid::now_v7().to_string();
        let lane = TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([9]).unwrap(),
            physical_disk_numbers: vec![9],
            disk_kind: LocalDiskKind::Hdd,
            configured_weight: 1,
            per_disk_limit: 1,
        };
        let mut files = TransientTaskFileSet::create(root.path(), &run_id).unwrap();
        let rows = (0..count)
            .map(|index| {
                let path = std::path::PathBuf::from(format!(r"C:\task-file-{index}.bin"));
                TaskFileRecord {
                    item_id: Uuid::now_v7(),
                    work_kind: TaskWorkKind::Base,
                    scanned: ScannedPath::new(
                        NormalizedPath::new(&path).unwrap(),
                        DisplayPath::new(&path).unwrap(),
                        17,
                    ),
                    known_md5: None,
                    missing: TaskWorkMask::from_bits(1 << 3).unwrap(),
                }
            })
            .collect::<Vec<_>>();
        let identities = files.append_batch(&lane, &rows).unwrap();
        (root, identities)
    }

    /// 满队列归还的消息必须继续拥有完整任务文件身份，不能丢失行定位字段。
    #[test]
    fn full_persist_queue_returns_original_task_file_message() {
        let (_root, identities) = task_file_identities(2);
        let (call_tx, _call_rx) = std::sync::mpsc::sync_channel(1);
        let (persist_tx, persist_rx) = std::sync::mpsc::sync_channel(1);
        let handle = BaseStoreHandle {
            calls: call_tx,
            persists: persist_tx,
            machine_id: MachineId::from_sha256([0xB4; 32]),
            #[cfg(test)]
            key_lookup_batches: Arc::new(Mutex::new(Vec::new())),
        };
        assert!(
            handle
                .try_persist(BasePersistMessage::new_task_file(
                    identities[0].clone(),
                    |_store| Ok(BasePersistOutcome::Ignored),
                ))
                .is_ok()
        );

        let returned = match handle.try_persist(BasePersistMessage::new_task_file(
            identities[1].clone(),
            |_store| Ok(BasePersistOutcome::Ignored),
        )) {
            Err(BasePersistSendError::Full(message)) => message,
            Ok(()) => panic!("容量为 1 的持久化队列不应接受第二条消息"),
            Err(BasePersistSendError::Closed(_)) => panic!("持久化队列不应提前关闭"),
        };
        assert_eq!(returned.identity.task_file_identity(), Some(&identities[1]));
        drop(persist_rx);
    }

    /// 已关闭队列归还的消息也必须保留完整任务文件身份，供调用方诊断或清理。
    #[test]
    fn closed_persist_queue_returns_original_task_file_message() {
        let (_root, identities) = task_file_identities(1);
        let (call_tx, _call_rx) = std::sync::mpsc::sync_channel(1);
        let (persist_tx, persist_rx) = std::sync::mpsc::sync_channel(1);
        drop(persist_rx);
        let handle = BaseStoreHandle {
            calls: call_tx,
            persists: persist_tx,
            machine_id: MachineId::from_sha256([0xB5; 32]),
            #[cfg(test)]
            key_lookup_batches: Arc::new(Mutex::new(Vec::new())),
        };

        let returned = match handle.try_persist(BasePersistMessage::new_task_file(
            identities[0].clone(),
            |_store| Ok(BasePersistOutcome::Ignored),
        )) {
            Err(BasePersistSendError::Closed(message)) => message,
            Ok(()) => panic!("已关闭持久化队列不应接受消息"),
            Err(BasePersistSendError::Full(_)) => panic!("已关闭队列不应报告容量已满"),
        };
        assert_eq!(returned.identity.task_file_identity(), Some(&identities[0]));
    }
}

/// 满队列时归还消息所有权，协调器可继续消费 ACK 后重试。
pub(crate) enum BasePersistSendError {
    /// 有界队列已满，原消息仍由调用方持有。
    Full(BasePersistMessage),
    /// actor 已停止，原消息仍由调用方持有。
    Closed(BasePersistMessage),
}

/// BaseCompute 对 task-local 单写 actor 的有界句柄。
#[derive(Clone)]
pub(crate) struct BaseStoreHandle {
    calls: SyncSender<StoreCall>,
    persists: SyncSender<BasePersistMessage>,
    machine_id: MachineId,
    /// 测试专用窄观测：记录每次 content-key 批量查询的输入大小。
    #[cfg(test)]
    key_lookup_batches: Arc<Mutex<Vec<usize>>>,
}

impl BaseStoreHandle {
    /// 返回 actor 独占 Store 的物理机器身份，不发送 SQLite 命令。
    pub(crate) fn machine_id(&self) -> &MachineId {
        &self.machine_id
    }

    /// 非阻塞投递终态；满队列时协调器必须先消费 ACK 或控制事件。
    pub(crate) fn try_persist(
        &self,
        message: BasePersistMessage,
    ) -> Result<(), BasePersistSendError> {
        self.persists
            .try_send(message)
            .map_err(|error| match error {
                TrySendError::Full(message) => BasePersistSendError::Full(message),
                TrySendError::Disconnected(message) => BasePersistSendError::Closed(message),
            })
    }

    /// 在 actor 线程串行执行一个短 Store 调用；错误会关闭该任务 writer。
    fn call<T, F>(&self, operation: F) -> Result<T, StoreError>
    where
        T: Send + 'static,
        F: FnOnce(&mut NodeStore) -> Result<T, StoreError> + Send + 'static,
    {
        let (result_tx, result_rx) = mpsc::sync_channel(1);
        let call = Box::new(move |store: &mut NodeStore| {
            let result = operation(store);
            let succeeded = result.is_ok();
            let _ = result_tx.send(result);
            succeeded
        });
        self.calls.send(call).map_err(|_| {
            StoreError::InvalidState("基础持久化 actor 已关闭 Store 调用通道".into())
        })?;
        result_rx.recv().map_err(|_| {
            StoreError::InvalidState("基础持久化 actor 未返回 Store 调用结果".into())
        })?
    }

    pub(crate) fn task_snapshot(&self, task_id: TaskId) -> Result<TaskSnapshot, StoreError> {
        self.call(move |store| store.task_snapshot(task_id))
    }

    pub(crate) fn reserve_scan_path(
        &self,
        task_id: TaskId,
        scanned: &ScannedPath,
        now_ms: i64,
    ) -> Result<Option<String>, StoreError> {
        let scanned = scanned.clone();
        self.call(move |store| store.reserve_scan_path(task_id, &scanned, now_ms))
    }

    /// 在同一个 actor call 中按输入位置批量读取 path 基础缓存；调用方不得逐项回查。
    pub(crate) fn lookup_base_cache_by_paths(
        &self,
        paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, StoreError> {
        let paths = paths.to_vec();
        self.call(move |store| store.lookup_base_cache_by_paths(&paths))
    }

    /// 在同一个 actor call 中按输入位置批量读取 content 基础缓存；供后续 content 游标使用。
    pub(crate) fn lookup_base_cache_by_keys(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, StoreError> {
        #[cfg(test)]
        self.key_lookup_batches
            .lock()
            .expect("批量查询观测锁不应中毒")
            .push(keys.len());
        let keys = keys.to_vec();
        self.call(move |store| store.lookup_base_cache_by_keys(&keys))
    }

    /// 返回测试期间实际提交的 content-key 批次大小，不影响生产执行路径。
    #[cfg(test)]
    pub(crate) fn lookup_key_batch_sizes_for_test(&self) -> Vec<usize> {
        self.key_lookup_batches
            .lock()
            .expect("批量查询观测锁不应中毒")
            .clone()
    }

    pub(crate) fn load_base_cache_record(
        &self,
        content_id: ContentId,
    ) -> Result<BaseCacheRecord, StoreError> {
        self.call(move |store| store.load_base_cache_record(content_id))
    }

    pub(crate) fn import_base_cache_record(
        &self,
        scanned: &ScannedPath,
        record: &BaseCacheRecord,
    ) -> Result<ContentRecord, StoreError> {
        let scanned = scanned.clone();
        let record = record.clone();
        self.call(move |store| store.import_base_cache_record(&scanned, &record))
    }

    pub(crate) fn queue_scan_item_for_read(&self, item_id: &str) -> Result<(), StoreError> {
        let item_id = item_id.to_owned();
        self.call(move |store| store.queue_scan_item_for_read(&item_id))
    }

    pub(crate) fn upsert_content_and_location(
        &self,
        scanned: &ScannedPath,
        md5: [u8; 16],
        media_kind: MediaKind,
    ) -> Result<ContentRecord, StoreError> {
        let scanned = scanned.clone();
        self.call(move |store| store.upsert_content_and_location(&scanned, md5, media_kind))
    }

    pub(crate) fn set_running_item_content_and_stage(
        &self,
        item_id: &str,
        content_id: ContentId,
        stage: &str,
    ) -> Result<(), StoreError> {
        let item_id = item_id.to_owned();
        let stage = stage.to_owned();
        self.call(move |store| {
            store.set_running_item_content_and_stage(&item_id, content_id, &stage)
        })
    }

    pub(crate) fn claim_next_item(
        &self,
        task_id: TaskId,
        now_ms: i64,
    ) -> Result<Option<ClaimedTaskItem>, StoreError> {
        self.call(move |store| store.claim_next_item(task_id, now_ms))
    }

    pub(crate) fn finalize_scan_task_from_items(
        &self,
        task_id: TaskId,
        now_ms: i64,
    ) -> Result<u64, StoreError> {
        self.call(move |store| store.finalize_scan_task_from_items(task_id, now_ms))
    }

    pub(crate) fn sync_state(&self) -> Result<SyncState, StoreError> {
        self.call(|store| store.sync_state())
    }

    pub(crate) fn pull_changes(&self, after: u64, limit: usize) -> Result<SyncBatch, StoreError> {
        self.call(move |store| store.pull_changes(after, limit))
    }

    pub(crate) fn ack_changes(&self, committed: u64) -> Result<(), StoreError> {
        self.call(move |store| store.ack_changes(committed))
    }

    pub(crate) fn save_task_stage(
        &self,
        task_id: TaskId,
        write: TaskStageWrite,
    ) -> Result<(), StoreError> {
        self.call(move |store| store.save_task_stage(task_id, write))
    }
}

/// 持有 actor 线程并在任务结束时取回原 NodeStore。
pub(crate) struct BaseStoreActor {
    thread: thread::JoinHandle<NodeStore>,
    /// 测试专用 join 观测；默认构建完全不包含该字段。
    #[cfg(feature = "test-hooks")]
    test_state: Option<Arc<BasePersistTestState>>,
}

impl BaseStoreActor {
    /// 启动 task-local writer；persist 队列容量由 Worker 与产品通道上限共同约束。
    pub(crate) fn spawn(
        store: NodeStore,
        persist_capacity: usize,
    ) -> (Self, BaseStoreHandle, UnboundedReceiver<BasePersistAck>) {
        Self::spawn_with_hook(store, persist_capacity, NoBeforePersist)
    }

    #[cfg(feature = "test-hooks")]
    /// 启动可在首条持久化前暂停的 writer；默认构建不包含此入口。
    pub(crate) fn spawn_with_first_persist_waiter(
        store: NodeStore,
        persist_capacity: usize,
        first_persist_waiter: BasePersistTestWaiter,
    ) -> (Self, BaseStoreHandle, UnboundedReceiver<BasePersistAck>) {
        let test_state = Arc::clone(&first_persist_waiter.shared);
        let (mut actor, handle, acknowledgements) = Self::spawn_with_hook(
            store,
            persist_capacity,
            FirstPersistTestHook(Some(first_persist_waiter)),
        );
        actor.test_state = Some(test_state);
        (actor, handle, acknowledgements)
    }

    /// 使用静态分派 hook 启动 actor；生产实例传入零大小空 hook。
    fn spawn_with_hook<H: BeforePersist>(
        store: NodeStore,
        persist_capacity: usize,
        before_persist: H,
    ) -> (Self, BaseStoreHandle, UnboundedReceiver<BasePersistAck>) {
        let machine_id = store.machine_id().clone();
        let (call_tx, call_rx) = mpsc::sync_channel(persist_capacity.max(1));
        let (persist_tx, persist_rx) = mpsc::sync_channel(persist_capacity.max(1));
        let (ack_tx, ack_rx) = tokio::sync::mpsc::unbounded_channel();
        let thread =
            thread::spawn(move || run_actor(store, call_rx, persist_rx, ack_tx, before_persist));
        (
            Self {
                thread,
                #[cfg(feature = "test-hooks")]
                test_state: None,
            },
            BaseStoreHandle {
                calls: call_tx,
                persists: persist_tx,
                machine_id,
                #[cfg(test)]
                key_lookup_batches: Arc::new(Mutex::new(Vec::new())),
            },
            ack_rx,
        )
    }

    /// 等待 writer 排空并取回原 Store，供现有借用 API 恢复所有权。
    pub(crate) async fn finish(self) -> Result<NodeStore, ScanError> {
        #[cfg(feature = "test-hooks")]
        let test_state = self.test_state;
        let store = tokio::task::spawn_blocking(move || self.thread.join())
            .await
            .map_err(|error| ScanError::Stage1(format!("基础持久化 actor join 失败: {error}")))?
            .map_err(|_| ScanError::Stage1("基础持久化 actor 线程 panic".into()))?;
        #[cfg(feature = "test-hooks")]
        if let Some(test_state) = test_state {
            test_state.writer_joined.store(true, Ordering::Release);
        }
        Ok(store)
    }
}

/// actor 主循环优先排空有界持久化队列，再处理同步 Store 调用。
fn run_actor<H: BeforePersist>(
    mut store: NodeStore,
    calls: Receiver<StoreCall>,
    persists: Receiver<BasePersistMessage>,
    acknowledgements: UnboundedSender<BasePersistAck>,
    mut before_persist: H,
) -> NodeStore {
    let mut calls_closed = false;
    let mut persists_closed = false;
    while !calls_closed || !persists_closed {
        match persists.try_recv() {
            Ok(message) => {
                let queue_wait = message.enqueued_at.elapsed();
                before_persist.before_persist();
                if !apply_persist_message(&mut store, message, queue_wait, &acknowledgements) {
                    break;
                }
                continue;
            }
            Err(TryRecvError::Disconnected) => persists_closed = true,
            Err(TryRecvError::Empty) => {}
        }
        match calls.recv_timeout(Duration::from_millis(1)) {
            Ok(call) => {
                if !call(&mut store) {
                    break;
                }
            }
            Err(RecvTimeoutError::Disconnected) => calls_closed = true,
            Err(RecvTimeoutError::Timeout) => {}
        }
    }
    store
}

/// 执行一条终态并发送 ACK；任何持久化错误都会关闭本任务 writer。
fn apply_persist_message(
    store: &mut NodeStore,
    message: BasePersistMessage,
    queue_wait: Duration,
    acknowledgements: &UnboundedSender<BasePersistAck>,
) -> bool {
    let BasePersistMessage {
        identity,
        enqueued_at: _,
        operation,
    } = message;
    let transaction_started = Instant::now();
    let result = operation(store);
    let transaction_elapsed = transaction_started.elapsed();
    let succeeded = result.is_ok();
    let _ = acknowledgements.send(BasePersistAck {
        identity,
        queue_wait,
        transaction_elapsed,
        result,
    });
    succeeded
}

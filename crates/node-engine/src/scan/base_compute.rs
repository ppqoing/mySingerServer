//! Node 基础计算任务：缓存判定、Worker 两段会话和持续补位调度。

use std::{
    collections::{BTreeMap, BTreeSet, VecDeque},
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

#[cfg(feature = "test-hooks")]
use std::sync::{Mutex, OnceLock};

use dedup_core::{ContentKey, DiskReadConfig, MediaKind, TaskId};
use dedup_node_store::{
    BaseCacheRecord, ClaimedTaskItem, ContentId, FileFaultKind, FileFaultRecord, NodeStore,
    PersistentStageState, ScannedPath, TaskItemApplyResult, TaskItemCompletion, TaskItemIdentity,
    TaskStageWrite, TaskStatus, classify_cache_completeness,
};
use dedup_protocol::{
    BASE_MISSING_CONTACT_SHEET, BASE_MISSING_PROBE, BASE_MISSING_STAGE1, proto,
    proto::worker_envelope,
};
use dedup_windows::ReadCancellationToken;
use tokio::{
    sync::{Notify, Semaphore, mpsc::error::TrySendError},
    task::JoinSet,
};

use crate::{
    RemoteFeatureCache,
    artifact_registry::RegenerableArtifactRegistry,
    contact_sheet_cache::ContactSheetCacheEntry,
    disk_full_cleanup::DiskFullCleaner,
    io::ReadFailure,
    runtime_tasks::{
        RuntimeExecutionConfigUpdate, RuntimeFailureUpdate, RuntimePipelineControl,
        RuntimePipelineOwnership, RuntimePipelineQueue, RuntimeProgressUnit, RuntimeStage,
        RuntimeStageUpdate, RuntimeTaskReporter, RuntimeWorkerUpdate,
    },
    worker::{
        BaseComputeOutput, Stage1Output, WorkerEvent, WorkerFileIdentity, WorkerPool,
        decode_base_compute_payload,
    },
};

#[cfg(feature = "test-hooks")]
use super::base_persistence::BasePersistTestWaiter;
use super::{
    PipelineFileReader, PipelineLimits, ReadProduct, ScanError, ScanOptions, ScanSummary,
    base_flow_control::{
        ContentDeparture, ContentOutputCredit, ContentOutputCredits, DecodeCredit, DecodeCredits,
        HashPhaseGuard, HashPhaseTracker, HashRefillController, HashStartResult,
        MediaAcquirePhaseGuard, MediaAcquirePhaseTracker,
    },
    base_persistence::{
        BasePersistAck, BasePersistMessage, BasePersistOutcome, BasePersistSendError,
        BaseStoreActor, BaseStoreHandle,
    },
    cache_resolver::{
        CacheContextKey, CacheResolution, CacheResolutionKind, CacheResolveRequest,
        CacheResolverHandle, ContentResolveItem, MAX_CACHE_BATCH_ITEMS, MAX_CONTENT_REMOTE_SLOTS,
        PATH_REMOTE_SLOTS, PathResolveItem, spawn_cache_resolver,
    },
};

#[cfg(test)]
use super::base_persistence::BasePersistIdentity;

/// 中心缓存批量查询的固定上限，避免一次 PostgreSQL 数组无限增长。
const REMOTE_LOOKUP_BATCH_SIZE: usize = MAX_CACHE_BATCH_ITEMS;
/// actor 同时保留的 path 上下文固定上限；不随通道容量乘法放大。
const MAX_PATH_CACHE_CONTEXTS: usize = 2_000;
/// 在泛型读取器边界擦除后的媒体许可；只保留 Send 和 RAII 释放语义。
type ErasedMediaPermit = Box<dyn Send + 'static>;
/// 媒体许可 JoinSet 的单项所有权结果。
type MediaAcquireOutput = (
    BaseComputeJob,
    Result<Option<ErasedMediaPermit>, ReadFailure>,
    MediaAcquirePhaseGuard,
);
/// 一个 Hash future 的持久项、结果和真实完整处理耗时。
type HashTaskOutput = (
    ClaimedTaskItem,
    Result<HashedBaseItem, HashReadFailure>,
    Duration,
    HashPhaseGuard,
);

/// Hash 读取失败时把 output credit 一并带到 terminal persist 边界。
struct HashReadFailure {
    /// 原始文件读取错误，保持既有取消和诊断语义。
    error: ReadFailure,
    /// 直到失败项入持久化队列后才释放的 output credit。
    output_credit: ContentOutputCredit,
}
/// Node 在 Worker 返回 MD5 后做出的可复用缓存与缺失部分判定。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct BaseComputeDecision {
    media_kind: MediaKind,
    missing_parts: u32,
}

impl BaseComputeDecision {
    /// 将统一缓存完整性结果转换为既有基础计算缺失位。
    pub fn for_cache(
        cached: Option<&BaseCacheRecord>,
        contact_sheet_exists: bool,
        force_recompute: bool,
    ) -> Self {
        if force_recompute || cached.is_none() {
            return Self {
                media_kind: cached.map_or(MediaKind::Other, |record| record.media_kind),
                missing_parts: BASE_MISSING_PROBE
                    | BASE_MISSING_STAGE1
                    | BASE_MISSING_CONTACT_SHEET,
            };
        }
        let cached = cached.expect("上方已处理无缓存分支");
        let completeness = classify_cache_completeness(cached, contact_sheet_exists);
        Self {
            media_kind: cached.media_kind,
            missing_parts: completeness.base_missing_parts,
        }
    }

    /// 返回 Worker 续算协议使用的缺失位掩码。
    pub const fn missing_parts(self) -> u32 {
        self.missing_parts
    }

    /// 返回缓存已知媒体类型；新内容在探测前为 `Other`。
    pub const fn media_kind(self) -> MediaKind {
        self.media_kind
    }
}

/// 测试专用 JoinSet 观察门禁；生产入口保持 None 等价的原有 join 调度。
#[derive(Clone, Default)]
struct JoinObservationHooks {
    /// Hash future 完成后、协调器 join 前的观察门禁。
    hash: Option<Arc<JoinObservationGate>>,
    /// Media future 完成后、协调器 join 前的观察门禁。
    media: Option<Arc<JoinObservationGate>>,
}

/// 控制确定性 completed-unjoined/ready 正值窗口，不依赖 sleep 或概率轮询。
struct JoinObservationGate {
    /// 需要完成后才向测试暴露窗口的 future 数量。
    expected: usize,
    /// 已完成并已更新阶段计数的 future 数量。
    completed: AtomicUsize,
    /// 测试等待完成通知，使用计数二次检查避免通知丢失。
    completed_notify: Notify,
    /// 协调器已经把 completed/ready 正值重新投影到 runtime snapshot。
    projected: AtomicBool,
    /// 测试等待协调器完成一次正值投影。
    projected_notify: Notify,
    /// 是否允许协调器真正 join 这些 future。
    released: AtomicBool,
    /// 协调器等待测试释放 join 的通知。
    release_notify: Notify,
}

impl JoinObservationGate {
    /// 创建一个 test-hooks JoinSet 门禁；expected 为零时直接放行。
    #[cfg(feature = "test-hooks")]
    fn new(expected: usize) -> Arc<Self> {
        Arc::new(Self {
            expected,
            completed: AtomicUsize::new(0),
            completed_notify: Notify::new(),
            projected: AtomicBool::new(expected == 0),
            projected_notify: Notify::new(),
            released: AtomicBool::new(expected == 0),
            release_notify: Notify::new(),
        })
    }

    /// 记录一个 future 已完成阶段迁移，达到 expected 后唤醒观察者。
    fn mark_completed(&self) {
        let completed = self.completed.fetch_add(1, Ordering::AcqRel) + 1;
        if completed >= self.expected {
            self.completed_notify.notify_waiters();
        }
    }

    /// 判断协调器是否仍需暂停对应 JoinSet 的 join 分支。
    fn is_paused(&self) -> bool {
        !self.released.load(Ordering::Acquire)
    }

    /// 判断 completed/ready 是否已经由协调器投影到 runtime snapshot。
    fn is_projected(&self) -> bool {
        self.projected.load(Ordering::Acquire)
    }

    /// 在协调器成功更新 snapshot 后标记一次正值投影。
    fn mark_projected(&self) {
        if self.completed.load(Ordering::Acquire) < self.expected {
            return;
        }
        if self
            .projected
            .compare_exchange(false, true, Ordering::AcqRel, Ordering::Acquire)
            .is_ok()
        {
            self.projected_notify.notify_waiters();
        }
    }

    /// 释放 join 分支并唤醒协调器；重复释放安全。
    #[cfg(feature = "test-hooks")]
    fn release(&self) {
        self.released.store(true, Ordering::Release);
        self.release_notify.notify_waiters();
    }

    /// 测试等待所有目标 future 完成且阶段计数已经写入。
    async fn wait_for_completed(&self) {
        loop {
            if self.completed.load(Ordering::Acquire) >= self.expected {
                return;
            }
            let notified = self.completed_notify.notified();
            if self.completed.load(Ordering::Acquire) >= self.expected {
                return;
            }
            notified.await;
        }
    }

    /// 等待协调器完成阶段正值投影，避免测试读取上一轮 snapshot。
    #[cfg(feature = "test-hooks")]
    async fn wait_for_projected(&self) {
        loop {
            if self.is_projected() {
                return;
            }
            let notified = self.projected_notify.notified();
            if self.is_projected() {
                return;
            }
            notified.await;
        }
    }

    /// 协调器等待测试释放 join；没有 gate 时不会进入该 future。
    async fn wait_for_release(&self) {
        while !self.released.load(Ordering::Acquire) {
            let notified = self.release_notify.notified();
            if self.released.load(Ordering::Acquire) {
                return;
            }
            notified.await;
        }
    }
}

/// 把 test-hooks 的可选 join 门禁转换为协调器可直接 select 的 future。
async fn wait_for_join_release(gate: Option<Arc<JoinObservationGate>>) {
    if let Some(gate) = gate {
        gate.wait_for_release().await;
    } else {
        std::future::pending::<()>().await;
    }
}

/// 让协调器在 future 完成后再走一轮统一 ownership 投影。
async fn wait_for_join_projection(gate: Option<Arc<JoinObservationGate>>) {
    if let Some(gate) = gate {
        gate.wait_for_completed().await;
    } else {
        std::future::pending::<()>().await;
    }
}

/// test-hooks 可见的真实 JoinSet 阶段观察控制器。
#[cfg(feature = "test-hooks")]
#[derive(Clone)]
pub struct BaseComputeJoinObservationHooks {
    /// Hash 与 Media 两个确定性 join 门禁。
    inner: JoinObservationHooks,
}

#[cfg(feature = "test-hooks")]
impl BaseComputeJoinObservationHooks {
    /// 按预期完成数量创建 Hash completed-unjoined 与 Media ready 门禁。
    pub fn new(expected_hash: usize, expected_media: usize) -> Self {
        Self {
            inner: JoinObservationHooks {
                hash: Some(JoinObservationGate::new(expected_hash)),
                media: Some(JoinObservationGate::new(expected_media)),
            },
        }
    }

    /// 等待 Hash future 已完成但协调器尚未 join 的确定性窗口。
    pub async fn wait_for_hash_completed(&self) {
        self.inner.hash.as_ref().unwrap().wait_for_projected().await;
    }

    /// 等待 Media future 已完成但协调器尚未 join 的确定性窗口。
    pub async fn wait_for_media_ready(&self) {
        self.inner
            .media
            .as_ref()
            .unwrap()
            .wait_for_projected()
            .await;
    }

    /// 放行 Hash JoinSet 归并，观察者应先断言 completed-unjoined 正值。
    pub fn release_hash_join(&self) {
        self.inner.hash.as_ref().unwrap().release();
    }

    /// 放行 Media JoinSet 归并，观察者应先断言 ready 与 permit-ready 子集。
    pub fn release_media_join(&self) {
        self.inner.media.as_ref().unwrap().release();
    }
}

/// 基础计算任务的单写者编排入口。
pub struct BaseComputeEngine;

#[cfg(feature = "test-hooks")]
static CLAIM_OBSERVER: OnceLock<Mutex<Option<Arc<AtomicUsize>>>> = OnceLock::new();

#[cfg(feature = "test-hooks")]
static LOCAL_CONTENT_LOOKUP_OBSERVER: OnceLock<Mutex<Option<Arc<AtomicUsize>>>> = OnceLock::new();

/// test-hooks 下观察真实 SQLite claim 尝试次数，生产构建不包含该状态或调用。
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub struct BaseComputeClaimObserverGuard {
    /// 当前测试安装的计数器，Drop 时只移除同一个观察者。
    counter: Arc<AtomicUsize>,
}

/// test-hooks 下观察本地 content 批量查询次数，生产构建不包含该状态或调用。
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub struct BaseComputeLocalContentLookupObserverGuard {
    /// 当前测试安装的计数器，Drop 时只移除同一个观察者。
    counter: Arc<AtomicUsize>,
}

#[cfg(feature = "test-hooks")]
impl Drop for BaseComputeClaimObserverGuard {
    /// 测试结束自动清除全局观察器，避免相邻管线互相污染。
    fn drop(&mut self) {
        if let Some(slot) = CLAIM_OBSERVER.get() {
            let mut current = slot.lock().expect("claim observer 锁不应中毒");
            if current
                .as_ref()
                .is_some_and(|installed| Arc::ptr_eq(installed, &self.counter))
            {
                *current = None;
            }
        }
    }
}

#[cfg(feature = "test-hooks")]
impl Drop for BaseComputeLocalContentLookupObserverGuard {
    /// 测试结束自动清除全局观察器，避免相邻管线互相污染。
    fn drop(&mut self) {
        if let Some(slot) = LOCAL_CONTENT_LOOKUP_OBSERVER.get() {
            let mut current = slot.lock().expect("local content observer 锁不应中毒");
            if current
                .as_ref()
                .is_some_and(|installed| Arc::ptr_eq(installed, &self.counter))
            {
                *current = None;
            }
        }
    }
}

/// 记录一次已经取得 output credit、即将执行 SQLite claim 的尝试。
#[cfg(feature = "test-hooks")]
#[inline]
fn record_claim_attempt() {
    if let Some(slot) = CLAIM_OBSERVER.get()
        && let Some(counter) = slot.lock().expect("claim observer 锁不应中毒").as_ref()
    {
        counter.fetch_add(1, Ordering::AcqRel);
    }
}

/// 记录一次即将交给 Store actor 的本地 content 批量查询。
#[cfg(feature = "test-hooks")]
#[inline]
fn record_local_content_lookup() {
    if let Some(slot) = LOCAL_CONTENT_LOOKUP_OBSERVER.get()
        && let Some(counter) = slot
            .lock()
            .expect("local content observer 锁不应中毒")
            .as_ref()
    {
        counter.fetch_add(1, Ordering::AcqRel);
    }
}

impl BaseComputeEngine {
    /// 安装仅供 test-hooks 使用的真实 SQLite claim 计数器，不改变生产公共 API 或开销。
    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    pub fn install_claim_observer_for_test(
        counter: Arc<AtomicUsize>,
    ) -> BaseComputeClaimObserverGuard {
        let slot = CLAIM_OBSERVER.get_or_init(|| Mutex::new(None));
        let mut current = slot.lock().expect("claim observer 锁不应中毒");
        assert!(current.is_none(), "同一测试进程只能安装一个 claim observer");
        *current = Some(Arc::clone(&counter));
        BaseComputeClaimObserverGuard { counter }
    }

    /// 安装仅供 test-hooks 使用的本地 content 批量查询计数器。
    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    pub fn install_local_content_lookup_observer_for_test(
        counter: Arc<AtomicUsize>,
    ) -> BaseComputeLocalContentLookupObserverGuard {
        let slot = LOCAL_CONTENT_LOOKUP_OBSERVER.get_or_init(|| Mutex::new(None));
        let mut current = slot.lock().expect("local content observer 锁不应中毒");
        assert!(
            current.is_none(),
            "同一测试进程只能安装一个 local content observer"
        );
        *current = Some(Arc::clone(&counter));
        BaseComputeLocalContentLookupObserverGuard { counter }
    }

    /// 从已完成枚举的固定文件清单运行缓存查询、Worker 计算和 SQLite 单写者收尾。
    #[allow(clippy::too_many_arguments)]
    pub async fn run_existing<R, F>(
        store: &mut NodeStore,
        worker_pool: &mut WorkerPool,
        remote: R,
        remote_available: bool,
        task_id: TaskId,
        options: ScanOptions,
        rows: Vec<ScannedPath>,
        contact_sheet_root: &std::path::Path,
        reader: F,
        limits: PipelineLimits,
        read_config: &DiskReadConfig,
        cancellation: ReadCancellationToken,
        reporter: &RuntimeTaskReporter,
        artifact_registry: &Arc<RegenerableArtifactRegistry>,
        disk_full_cleaner: &DiskFullCleaner,
        now_ms: i64,
    ) -> Result<ScanSummary, ScanError>
    where
        R: RemoteFeatureCache,
        F: PipelineFileReader,
    {
        Self::run_existing_with_actor_factory(
            store,
            worker_pool,
            remote,
            remote_available,
            task_id,
            options,
            rows,
            contact_sheet_root,
            reader,
            limits,
            read_config,
            cancellation,
            reporter,
            artifact_registry,
            disk_full_cleaner,
            now_ms,
            JoinObservationHooks::default(),
            BaseStoreActor::spawn,
        )
        .await
    }

    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    /// 测试专用入口：暂停真实 JoinSet 归并以观察 completed-unjoined/ready 正值窗口。
    #[allow(clippy::too_many_arguments)]
    pub async fn run_existing_with_join_observation_for_test<R, F>(
        store: &mut NodeStore,
        worker_pool: &mut WorkerPool,
        remote: R,
        remote_available: bool,
        task_id: TaskId,
        options: ScanOptions,
        rows: Vec<ScannedPath>,
        contact_sheet_root: &std::path::Path,
        reader: F,
        limits: PipelineLimits,
        read_config: &DiskReadConfig,
        cancellation: ReadCancellationToken,
        reporter: &RuntimeTaskReporter,
        artifact_registry: &Arc<RegenerableArtifactRegistry>,
        disk_full_cleaner: &DiskFullCleaner,
        now_ms: i64,
        hooks: BaseComputeJoinObservationHooks,
    ) -> Result<ScanSummary, ScanError>
    where
        R: RemoteFeatureCache,
        F: PipelineFileReader,
    {
        Self::run_existing_with_actor_factory(
            store,
            worker_pool,
            remote,
            remote_available,
            task_id,
            options,
            rows,
            contact_sheet_root,
            reader,
            limits,
            read_config,
            cancellation,
            reporter,
            artifact_registry,
            disk_full_cleaner,
            now_ms,
            hooks.inner,
            BaseStoreActor::spawn,
        )
        .await
    }

    #[cfg(feature = "test-hooks")]
    #[doc(hidden)]
    /// 测试专用入口：首条持久化已被 actor 取得后，等待 gate 放行再执行事务。
    #[allow(clippy::too_many_arguments)]
    pub async fn run_existing_with_first_persist_gate_for_test<R, F>(
        store: &mut NodeStore,
        worker_pool: &mut WorkerPool,
        remote: R,
        remote_available: bool,
        task_id: TaskId,
        options: ScanOptions,
        rows: Vec<ScannedPath>,
        contact_sheet_root: &std::path::Path,
        reader: F,
        limits: PipelineLimits,
        read_config: &DiskReadConfig,
        cancellation: ReadCancellationToken,
        reporter: &RuntimeTaskReporter,
        artifact_registry: &Arc<RegenerableArtifactRegistry>,
        disk_full_cleaner: &DiskFullCleaner,
        now_ms: i64,
        first_persist_waiter: BasePersistTestWaiter,
    ) -> Result<ScanSummary, ScanError>
    where
        R: RemoteFeatureCache,
        F: PipelineFileReader,
    {
        Self::run_existing_with_actor_factory(
            store,
            worker_pool,
            remote,
            remote_available,
            task_id,
            options,
            rows,
            contact_sheet_root,
            reader,
            limits,
            read_config,
            cancellation,
            reporter,
            artifact_registry,
            disk_full_cleaner,
            now_ms,
            JoinObservationHooks::default(),
            move |store, persist_capacity| {
                BaseStoreActor::spawn_with_first_persist_waiter(
                    store,
                    persist_capacity,
                    first_persist_waiter,
                )
            },
        )
        .await
    }

    /// 用静态 actor factory 复用运行与 Store 恢复流程。
    #[allow(clippy::too_many_arguments)]
    async fn run_existing_with_actor_factory<R, F, S>(
        store: &mut NodeStore,
        worker_pool: &mut WorkerPool,
        remote: R,
        remote_available: bool,
        task_id: TaskId,
        options: ScanOptions,
        rows: Vec<ScannedPath>,
        contact_sheet_root: &std::path::Path,
        reader: F,
        limits: PipelineLimits,
        read_config: &DiskReadConfig,
        cancellation: ReadCancellationToken,
        reporter: &RuntimeTaskReporter,
        artifact_registry: &Arc<RegenerableArtifactRegistry>,
        disk_full_cleaner: &DiskFullCleaner,
        now_ms: i64,
        join_observation: JoinObservationHooks,
        spawn_actor: S,
    ) -> Result<ScanSummary, ScanError>
    where
        R: RemoteFeatureCache,
        F: PipelineFileReader,
        S: FnOnce(
            NodeStore,
            usize,
        ) -> (
            BaseStoreActor,
            BaseStoreHandle,
            tokio::sync::mpsc::UnboundedReceiver<BasePersistAck>,
        ),
    {
        let machine_id = store.machine_id().clone();
        let replacement = NodeStore::open_in_memory(machine_id)?;
        let actor_store = std::mem::replace(store, replacement);
        let worker_capacity = worker_pool.worker_process_ids().len();
        let persist_capacity = worker_capacity
            .saturating_mul(2)
            .clamp(1, 512)
            .min(limits.channel_capacity().max(1));
        let (actor, handle, mut persist_acks) = spawn_actor(actor_store, persist_capacity);
        let result = Self::run_with_store_actor(
            &handle,
            &mut persist_acks,
            worker_pool,
            remote,
            remote_available,
            task_id,
            options,
            rows,
            contact_sheet_root,
            reader,
            limits,
            read_config,
            cancellation,
            reporter,
            artifact_registry,
            disk_full_cleaner,
            now_ms,
            join_observation,
        )
        .await;
        drop(handle);
        drop(persist_acks);
        let restored = actor.finish().await?;
        *store = restored;
        result
    }

    /// 使用 task-local 单写句柄执行基础计算协调器。
    #[allow(clippy::too_many_arguments)]
    async fn run_with_store_actor<R, F>(
        store: &BaseStoreHandle,
        persist_acks: &mut tokio::sync::mpsc::UnboundedReceiver<BasePersistAck>,
        worker_pool: &mut WorkerPool,
        remote: R,
        remote_available: bool,
        task_id: TaskId,
        options: ScanOptions,
        rows: Vec<ScannedPath>,
        contact_sheet_root: &std::path::Path,
        reader: F,
        limits: PipelineLimits,
        read_config: &DiskReadConfig,
        cancellation: ReadCancellationToken,
        reporter: &RuntimeTaskReporter,
        artifact_registry: &Arc<RegenerableArtifactRegistry>,
        disk_full_cleaner: &DiskFullCleaner,
        now_ms: i64,
        join_observation: JoinObservationHooks,
    ) -> Result<ScanSummary, ScanError>
    where
        R: RemoteFeatureCache,
        F: PipelineFileReader,
    {
        let total_files = rows.len();
        let total_bytes = rows.iter().map(|row| row.file_size).sum();
        let worker_capacity = worker_pool.worker_process_ids().len();
        if worker_capacity == 0 {
            return Err(ScanError::Stage1("WorkerPool 没有可用 Worker".into()));
        }
        let hash_capacity = limits.max_read_tasks();
        let queue_capacity = limits.channel_capacity();
        let decode_ownership_capacity = decode_credit_capacity(worker_capacity)?;
        let persist_ownership_capacity = MAX_CACHE_BATCH_ITEMS.saturating_add(worker_capacity);
        reporter
            .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
                hash_tasks: runtime_u32(hash_capacity),
                path_cache_queue_capacity: runtime_u32(PATH_REMOTE_SLOTS),
                content_cache_queue_capacity: runtime_u32(queue_capacity),
                decode_queue_capacity: runtime_u32(decode_ownership_capacity),
                persist_queue_capacity: runtime_u32(persist_ownership_capacity),
                worker_slots: runtime_u32(worker_capacity),
                cpu_budget: runtime_u32(worker_pool.cpu_budget()),
                global_disk_permits: runtime_u32(read_config.total_threads),
                hdd_per_disk_permits: runtime_u32(read_config.hdd_threads_per_disk),
                ssd_per_disk_permits: runtime_u32(read_config.ssd_threads_per_disk),
                unknown_per_disk_permits: runtime_u32(read_config.unknown_threads_per_disk),
            })
            .map_err(runtime_error)?;
        reporter
            .freeze_base_compute_totals_nowait(runtime_u64(total_files))
            .map_err(runtime_error)?;
        let lookup_started = wall_clock_ms();
        reporter
            .start_stage_nowait(RuntimeStage::LookupBaseCache, RuntimeProgressUnit::Files)
            .map_err(runtime_error)?;
        save_base_stage(
            store,
            task_id,
            RuntimeStage::LookupBaseCache,
            PersistentStageState::Running,
            0,
            Some(runtime_u64(total_files)),
            0,
            0,
            Some(lookup_started),
            None,
            None,
        )?;

        let mut summary = ScanSummary {
            task_id,
            total_files,
            total_bytes,
            cache_hits: 0,
            hashed: 0,
            reused_contents: 0,
            scheduled_stage1: 0,
            skipped_incomplete: 0,
            file_failures: 0,
            outbox_high_seq: 0,
        };
        let compute_started = wall_clock_ms();
        reporter
            .update_stage_nowait(RuntimeStageUpdate::running(
                RuntimeStage::ComputeBaseFeatures,
                RuntimeProgressUnit::Files,
                0,
                Some(runtime_u64(total_files)),
            ))
            .map_err(runtime_error)?;
        save_base_stage(
            store,
            task_id,
            RuntimeStage::ComputeBaseFeatures,
            PersistentStageState::Running,
            0,
            Some(runtime_u64(total_files)),
            0,
            0,
            Some(compute_started),
            None,
            None,
        )?;

        let mut cache_warning = remote.startup_warning().map(str::to_owned);
        if let Some(warning) = cache_warning.as_deref() {
            report_cache_warning(reporter, warning)?;
        }
        let CacheResolverHandle {
            path_requests,
            content_requests,
            mut resolutions,
            content_credits,
            task: resolver_task,
        } = spawn_cache_resolver(
            remote,
            remote_available,
            queue_capacity,
            worker_capacity.min(MAX_CONTENT_REMOTE_SLOTS),
        );
        let mut cache_remote_available = remote_available;
        let mut remaining_rows = VecDeque::from(rows);
        let mut lookup_completed = 0_u64;
        let mut lookup_finished = false;
        let mut next_request_id = 1_u64;
        // 两类请求各自保留一个未发送状态，任何 lane 背压都不会占住另一 lane。
        let mut pending_path_request = None;
        let mut pending_content_request = None;
        let mut path_batches = BTreeMap::<u64, PathBatchContext>::new();
        let mut path_contexts = BTreeMap::<CacheContextKey, PathResolveContext>::new();
        let mut content_batches = BTreeMap::<u64, ContentBatchContext>::new();
        let mut content_contexts = BTreeMap::<CacheContextKey, ContentResolveContext>::new();
        let mut pending_content_resolution = None;
        // Hash 输出 credit 与远端 content 查询 credit 相互独立，前者随文件移动并由 RAII 归还。
        let output_credits = ContentOutputCredits::new(queue_capacity);
        // Decode credit 固定为 2W，与 Worker admission 独立，Started 前一直保留。
        let decode_credits = DecodeCredits::new(decode_ownership_capacity);
        let mut hash_refill = HashRefillController::new(hash_capacity);
        // 总 ownership 分别受 path/content context、Hash future、pending 和 Worker active 硬上限约束。
        let mut hashing = JoinSet::new();
        let mut pending_hashed = VecDeque::with_capacity(queue_capacity);
        // 本地 SQLite 批量结果按 Hash 队首顺序消费；背压时游标保留，不重复查询。
        let mut pending_local_content_batch = None;
        let mut pending_compute = VecDeque::with_capacity(queue_capacity);
        let mut media_acquiring = JoinSet::new();
        let mut active = BTreeMap::<String, ActiveBase>::new();
        // 阶段 tracker 只汇总真实 future/guard；不改变原有队列或调度容量。
        let hash_phases = HashPhaseTracker::new();
        let media_phases = MediaAcquirePhaseTracker::new();
        // 按 item_id 保存等待/ready future 身份，供缓存等待资源归属校验使用。
        let mut hash_item_ids = BTreeSet::<String>::new();
        let mut media_item_ids = BTreeSet::<String>::new();
        // 每个具体 item 首次成功 claim 的单调起点；reserve 不会写入此表。
        let mut item_started_at = BTreeMap::<String, Instant>::new();
        // dispatch ACK 尚未返回期间只允许一个完整的 PendingWorkerDispatch。
        let mut pending_worker_dispatch = None::<PendingWorkerDispatch>;
        // 仅保留 item 身份供缓存等待校验与遥测；所有资源 ownership 在上面的 pending 中。
        let mut worker_dispatching_item = None::<String>;
        // 未入 actor 的消息和等待 ACK 的消息分别计数，二者归零前任务不得 finalize。
        let mut pending_persist = VecDeque::<BasePersistMessage>::new();
        let mut persist_in_flight = 0_usize;
        // 一个真实 select epoch 只允许成功启动一个 Hash；同步处理不得自行放行下一次。
        let mut hash_spawn_allowed = true;
        let run_result: Result<(), ScanError> = async {
            loop {
                flush_persist_messages(store, &mut pending_persist, &mut persist_in_flight)?;
                let store_ready = persist_in_flight == 0 && pending_persist.is_empty();
                if store_ready {
                    ensure_task_running(store, task_id, &cancellation)?;
                } else {
                    ensure_not_cancelled(&cancellation)?;
                }
                ensure_content_output_bound(
                    queue_capacity,
                    &pending_hashed,
                    &pending_compute,
                    &content_contexts,
                )?;

                try_send_cache_request(&path_requests, &mut pending_path_request)?;
                try_send_cache_request(&content_requests, &mut pending_content_request)?;

                if store_ready
                    && pending_path_request.is_none()
                    && path_batches.len() < PATH_REMOTE_SLOTS
                    && !remaining_rows.is_empty()
                {
                    prepare_path_batch(
                        store,
                        task_id,
                        &options,
                        contact_sheet_root,
                        &mut remaining_rows,
                        &mut next_request_id,
                        cache_remote_available,
                        &mut pending_path_request,
                        &mut path_batches,
                        &mut path_contexts,
                        &hash_item_ids,
                        &media_item_ids,
                        &active,
                        &worker_dispatching_item,
                        &mut lookup_completed,
                        total_files,
                        lookup_started,
                        reporter,
                        &mut pending_persist,
                        cache_warning.as_deref(),
                        now_ms,
                        &mut hash_refill,
                    )?;
                    try_send_cache_request(&path_requests, &mut pending_path_request)?;
                }

                // Hash ready 结果仍在 JoinSet 中时，先逐项归并；归并完这一波再形成 content
                // cursor，避免已完成的一整窗口被拆成多个伪小批次，同时不等待仍在读取的慢 Hash。
                let hash_ready_results_pending = hash_phases.snapshot().completed_unjoined != 0;
                if store_ready && !hash_ready_results_pending {
                    prepare_content_batch(
                        store,
                        task_id,
                        &options,
                        contact_sheet_root,
                        queue_capacity,
                        cache_remote_available,
                        &content_credits,
                        &decode_credits,
                        &mut next_request_id,
                        &mut pending_hashed,
                        &mut pending_local_content_batch,
                        &mut pending_compute,
                        &mut pending_content_request,
                        &mut content_batches,
                        &mut content_contexts,
                        &hash_item_ids,
                        &media_item_ids,
                        &active,
                        &worker_dispatching_item,
                        &mut pending_persist,
                        &mut summary,
                        &mut hash_refill,
                        now_ms,
                    )?;
                }
                try_send_cache_request(&content_requests, &mut pending_content_request)?;

                fill_media_acquires(
                    worker_capacity,
                    &reader,
                    &cancellation,
                    &mut pending_compute,
                    &mut media_acquiring,
                    &active,
                    &media_phases,
                    &mut media_item_ids,
                    &output_credits,
                    &mut hash_refill,
                    &join_observation,
                )?;

                // 每次主循环只尝试一次 Hash admission；成功后必须等待真实 select 分支完成。
                if store_ready && hash_spawn_allowed {
                    let hash_start = try_start_one_hash_task(
                        store,
                        task_id,
                        now_ms,
                        hash_capacity,
                        &reader,
                        &cancellation,
                        &mut hashing,
                        &hash_phases,
                        &mut hash_item_ids,
                        &mut item_started_at,
                        &output_credits,
                        &mut hash_refill,
                        lookup_finished,
                        &join_observation,
                    )?;
                    if matches!(hash_start, HashStartResult::Started) {
                        hash_spawn_allowed = false;
                    }
                }

                update_pipeline_ownership(
                    reporter,
                    hashing.len(),
                    &hash_phases,
                    hash_capacity,
                    &output_credits,
                    &hash_refill,
                    &decode_credits,
                    path_batches.len(),
                    content_contexts.len(),
                    decode_queue_owned(
                        &pending_compute,
                        &media_acquiring,
                        &active,
                        pending_worker_dispatch.as_ref(),
                    )?,
                    persist_queue_owned(&pending_persist, persist_in_flight)?,
                    &media_phases,
                    media_acquiring.len(),
                    worker_capacity,
                    &active,
                    &worker_dispatching_item,
                )?;
                if let Some(gate) = join_observation.hash.as_ref() {
                    gate.mark_projected();
                }
                if let Some(gate) = join_observation.media.as_ref() {
                    gate.mark_projected();
                }

                if store_ready
                    && !lookup_finished
                    && remaining_rows.is_empty()
                    && pending_path_request.is_none()
                    && path_batches.is_empty()
                    && path_contexts.is_empty()
                {
                    finish_lookup_stage(
                        store,
                        task_id,
                        total_files,
                        lookup_started,
                        reporter,
                        cache_warning.clone(),
                    )?;
                    lookup_finished = true;
                    // lookup 完成只关闭上游；仍需下一次持有 token 的空 claim 才能权威耗尽。
                    hash_refill.on_upstream_closed();
                }

                if lookup_finished
                    && hash_refill.input_exhausted()
                    && hashing.is_empty()
                    && pending_hashed.is_empty()
                    && pending_local_content_batch.is_none()
                    && pending_compute.is_empty()
                    && pending_content_request.is_none()
                    && content_batches.is_empty()
                    && content_contexts.is_empty()
                    && pending_content_resolution.is_none()
                    && media_acquiring.is_empty()
                    && active.is_empty()
                    && pending_persist.is_empty()
                    && persist_in_flight == 0
                {
                    break;
                }
                let content_output_room =
                    content_output_owned(&pending_hashed, &pending_compute)? < queue_capacity;
                let worker_admission_room = active
                    .len()
                    .checked_add(media_acquiring.len())
                    .is_some_and(|owned| owned < worker_capacity);
                let content_cursor_can_advance = (worker_admission_room || content_output_room)
                    && content_cursor_has_admission(
                        pending_content_resolution.as_ref(),
                        &content_contexts,
                        contact_sheet_root,
                        options.force_recompute,
                        &decode_credits,
                    );
                // test-hooks 可暂停真实 JoinSet 的 join 分支；生产 hooks 为空，条件恒为 false。
                let hash_join_paused = join_observation
                    .hash
                    .as_ref()
                    .is_some_and(|gate| gate.is_paused());
                let media_join_paused = join_observation
                    .media
                    .as_ref()
                    .is_some_and(|gate| gate.is_paused());
                let hash_projection_pending = hash_join_paused
                    && join_observation
                        .hash
                        .as_ref()
                        .is_some_and(|gate| !gate.is_projected());
                let media_projection_pending = media_join_paused
                    && join_observation
                        .media
                        .as_ref()
                        .is_some_and(|gate| !gate.is_projected());
                // 预热优先把已有 output credit 逐步交给 Hash；credit 用尽或上游暂时为空后才归并。
                let warmup_preload_pending = hash_refill.phase()
                    == super::base_flow_control::HashRefillPhase::Warmup
                    && hash_refill.available() > 0
                    && !hash_refill.waiting_for_upstream_publish()
                    && output_credits.available_permits() > 0;
                // cursor 每项先 cooperative yield；恢复 poll 后 biased Worker 分支可抢先归并终态。
                tokio::select! {
                    biased;
                    acknowledgement = persist_acks.recv(), if persist_in_flight > 0 => {
                        let acknowledgement = acknowledgement.ok_or_else(|| {
                            ScanError::Stage1("基础持久化 actor 在 ACK 前关闭".into())
                        })?;
                        apply_persist_ack(
                            acknowledgement,
                            &mut persist_in_flight,
                            reporter,
                            &mut summary,
                            &mut item_started_at,
                        ).await?;
                    }
                    event = worker_pool.next_event(), if !active.is_empty() => {
                        let event = event.ok_or_else(|| ScanError::Stage1("WorkerPool 已关闭".into()))?;
                        ensure_not_cancelled(&cancellation)?;
                        handle_worker_event(
                            event, task_id, contact_sheet_root, reporter, artifact_registry,
                            disk_full_cleaner, &mut active, &mut pending_persist, now_ms,
                        ).await?;
                        flush_persist_messages(
                            store,
                            &mut pending_persist,
                            &mut persist_in_flight,
                        )?;
                    }
                     _ = wait_for_join_projection(join_observation.media.clone()), if media_projection_pending => {}
                     _ = wait_for_join_release(join_observation.media.clone()), if media_join_paused && !media_projection_pending => {}
                     acquired = media_acquiring.join_next(), if !media_acquiring.is_empty() && !media_join_paused => {
                         let (job, acquired, media_guard) = acquired
                             .ok_or_else(|| ScanError::Stage1("媒体许可任务意外为空".into()))?
                             .map_err(|error| ScanError::Stage1(format!("媒体许可任务退出异常: {error}")))?;
                         media_item_ids.remove(&job.item_id);
                         // JoinSet 已移除该 future；ready 计数在此归并边界统一清理。
                         drop(media_guard);
                         ensure_not_cancelled(&cancellation)?;
                         match acquired {
                             Ok(media_permit) => {
                                 let pending = PendingWorkerDispatch {
                                     job,
                                     media_permit,
                                 };
                                 worker_dispatching_item = Some(pending.job.item_id.clone());
                                 pending_worker_dispatch = Some(pending);
                                 update_pipeline_ownership(
                                     reporter,
                                     hashing.len(),
                                     &hash_phases,
                                     hash_capacity,
                                     &output_credits,
                                     &hash_refill,
                                     &decode_credits,
                                     path_batches.len(),
                                     content_contexts.len(),
                                     decode_queue_owned(
                                         &pending_compute,
                                         &media_acquiring,
                                         &active,
                                         pending_worker_dispatch.as_ref(),
                                     )?,
                                     persist_queue_owned(&pending_persist, persist_in_flight)?,
                                     &media_phases,
                                     media_acquiring.len(),
                                     worker_capacity,
                                     &active,
                                     &worker_dispatching_item,
                                 )?;
                                 let dispatch_result = dispatch_compute_job(
                                    store,
                                    worker_pool,
                                    task_id,
                                    pending_worker_dispatch
                                        .take()
                                        .expect("dispatching 状态必须持有完整 job"),
                                    read_config,
                                     &cancellation,
                                     &mut active,
                                     &mut summary,
                                  )
                                  .await;
                                 worker_dispatching_item = None;
                                 update_pipeline_ownership(
                                     reporter,
                                     hashing.len(),
                                     &hash_phases,
                                     hash_capacity,
                                     &output_credits,
                                     &hash_refill,
                                     &decode_credits,
                                     path_batches.len(),
                                     content_contexts.len(),
                                     decode_queue_owned(
                                         &pending_compute,
                                         &media_acquiring,
                                         &active,
                                         pending_worker_dispatch.as_ref(),
                                     )?,
                                     persist_queue_owned(&pending_persist, persist_in_flight)?,
                                     &media_phases,
                                     media_acquiring.len(),
                                     worker_capacity,
                                     &active,
                                     &worker_dispatching_item,
                                 )?;
                                 dispatch_result?;
                             }
                             Err(ReadFailure::Cancelled) => return Err(ScanError::Cancelled),
                             Err(error) => {
                                 fail_media_permit_file(
                                    task_id,
                                    &job,
                                    error.to_string(),
                                    &mut pending_persist,
                                    now_ms,
                                )?;
                             }
                         }
                        ensure_worker_admission_bound(
                            worker_capacity,
                            &active,
                            &media_acquiring,
                        )?;
                    }
                    _ = tokio::task::yield_now(), if store_ready && content_cursor_can_advance => {
                        ensure_task_running(store, task_id, &cancellation)?;
                        let (finished, job) = process_next_content_resolution(
                            pending_content_resolution
                                .as_mut()
                                .expect("content cursor 分支只在结果存在时启用"),
                            store,
                            task_id,
                            &options,
                            contact_sheet_root,
                            &mut pending_persist,
                            &mut summary,
                            &mut content_contexts,
                            &decode_credits,
                            now_ms,
                            &mut hash_refill,
                        )?;
                        if let Some(job) = job {
                            if !worker_admission_room && !content_output_room {
                                return Err(ScanError::Stage1(
                                    "content cursor 消费时没有有界 Worker 输出槽位".into(),
                                ));
                            }
                            pending_compute.push_back(job);
                            fill_media_acquires(
                                worker_capacity,
                                &reader,
                                &cancellation,
                                &mut pending_compute,
                                &mut media_acquiring,
                                &active,
                                &media_phases,
                                &mut media_item_ids,
                                &output_credits,
                                &mut hash_refill,
                                &join_observation,
                            )?;
                        }
                        ensure_content_output_bound(
                            queue_capacity,
                            &pending_hashed,
                            &pending_compute,
                            &content_contexts,
                        )?;
                        if finished {
                            pending_content_resolution = None;
                        }
                    }
                    resolution = resolutions.recv(), if store_ready && pending_content_resolution.is_none() => {
                        let resolution = resolution
                            .ok_or_else(|| ScanError::Stage1("缓存 resolver 意外关闭结果通道".into()))?;
                        ensure_task_running(store, task_id, &cancellation)?;
                        if let Some(warning) = resolution.warning.as_deref() {
                            if cache_warning.is_none() {
                                report_cache_warning(reporter, warning)?;
                                cache_warning = Some(warning.to_owned());
                            }
                            cache_remote_available = false;
                        }
                        if matches!(&resolution.kind, CacheResolutionKind::Paths(_)) {
                            handle_path_resolution(
                                resolution,
                                store,
                                task_id,
                                &options,
                                contact_sheet_root,
                                reporter,
                                &mut path_batches,
                                &mut path_contexts,
                                &mut lookup_completed,
                                total_files,
                                lookup_started,
                                cache_warning.as_deref(),
                                &mut pending_persist,
                                now_ms,
                                &mut hash_refill,
                            )?;
                        } else {
                            pending_content_resolution = Some(begin_content_resolution(
                                resolution,
                                &mut content_batches,
                                reporter,
                            )?);
                        }
                    }
                    _ = wait_for_join_projection(join_observation.hash.clone()), if hash_projection_pending => {}
                    _ = wait_for_join_release(join_observation.hash.clone()), if hash_join_paused && !hash_projection_pending => {}
                    // Hash future 自身已经持有 output credit，归并不会新增 ownership，不能被 pending 队列满阻塞。
                    hashed = hashing.join_next(), if store_ready && !hashing.is_empty() && !hash_join_paused && !warmup_preload_pending => {
                        let hashed = hashed
                            .ok_or_else(|| ScanError::Stage1("Hash 读取任务意外为空".into()))?
                            .map_err(|error| ScanError::Stage1(error.to_string()))?;
                        ensure_task_running(store, task_id, &cancellation)?;
                        handle_hash_result(
                            &mut pending_hashed,
                            &mut pending_persist,
                            hashed,
                            reporter,
                            now_ms,
                            &mut hash_item_ids,
                            &mut hash_refill,
                        )?;
                        // 只让出一个调度轮次收集已经 ready 的 Hash；不等待慢 Hash 或凑满批次。
                        tokio::task::yield_now().await;
                        drain_one_ready_hash_result(
                            store,
                            task_id,
                            &cancellation,
                            queue_capacity,
                            &mut hashing,
                            &mut pending_hashed,
                            &pending_compute,
                            &content_contexts,
                            &mut pending_persist,
                            reporter,
                            now_ms,
                            &mut hash_item_ids,
                            &mut hash_refill,
                        )?;
                    }
                    permit = path_requests.reserve(), if pending_path_request.is_some() => {
                        ensure_not_cancelled(&cancellation)?;
                        let permit = permit
                            .map_err(|_| ScanError::Stage1("缓存 resolver path 通道已关闭".into()))?;
                        permit.send(pending_path_request.take().expect("path permit 只在请求存在时启用"));
                    }
                    permit = content_requests.reserve(), if pending_content_request.is_some() => {
                        ensure_not_cancelled(&cancellation)?;
                        let permit = permit
                            .map_err(|_| ScanError::Stage1("缓存 resolver content 通道已关闭".into()))?;
                        permit.send(pending_content_request.take().expect("content permit 只在请求存在时启用"));
                    }
                    _ = tokio::time::sleep(Duration::from_millis(10)) => {
                        if store_ready {
                            ensure_task_running(store, task_id, &cancellation)?;
                        } else {
                            ensure_not_cancelled(&cancellation)?;
                        }
                    }
                }
                // 只有上面的 tokio::select! 真正完成一个分支后才开启下一 epoch。
                hash_spawn_allowed = true;
            }
            Ok(())
        }
        .await;

        drop(path_requests);
        drop(content_requests);
        drop(resolutions);
        if let Err(error) = run_result {
            cancellation.cancel();
            media_acquiring.abort_all();
            while media_acquiring.join_next().await.is_some() {}
            let _ = worker_pool
                .cancel_task(&task_id.as_uuid().to_string())
                .await;
            for (item_id, work) in &mut active {
                if let Some(slot) = work.worker_slot {
                    let _ = reporter.worker_released_nowait(slot, item_id);
                }
                work.release_media_permit();
            }
            active.clear();
            drain_hash_tasks(&mut hashing).await;
            hash_item_ids.clear();
            media_item_ids.clear();
            item_started_at.clear();
            // 错误/取消收尾清空所有本地 ownership 容器，再统一投影字段 12–23 的零值。
            pending_hashed.clear();
            let _ = pending_local_content_batch.take();
            pending_compute.clear();
            path_batches.clear();
            path_contexts.clear();
            content_batches.clear();
            content_contexts.clear();
            let _ = pending_content_resolution.take();
            let _ = pending_path_request.take();
            let _ = pending_content_request.take();
            pending_persist.clear();
            persist_in_flight = 0;
            let _ = persist_in_flight;
            // 若取消恰好落在派发 ACK 边界，待派发 job 的 Drop 负责归还 decode credit。
            pending_worker_dispatch.take();
            worker_dispatching_item = None;
            hash_refill.finish();
            let cleanup_projection = update_pipeline_ownership(
                reporter,
                hashing.len(),
                &hash_phases,
                hash_capacity,
                &output_credits,
                &hash_refill,
                &decode_credits,
                path_batches.len(),
                content_contexts.len(),
                0,
                0,
                &media_phases,
                media_acquiring.len(),
                worker_capacity,
                &active,
                &worker_dispatching_item,
            );
            if let Err(projection_error) = cleanup_projection {
                // 保留更早的权威取消/错误；投影失败只记录诊断，不替换原错误。
                tracing::error!(error = %projection_error, "基础计算清理后的 ownership 零值投影失败");
            }
            let _ = resolver_task.await;
            return Err(error);
        }

        let resolver_exit = resolver_task
            .await
            .map_err(|error| ScanError::Stage1(format!("缓存 resolver 退出异常: {error}")))?;
        let mut remote = resolver_exit.remote;
        let mut remote_available = resolver_exit.remote_available;

        summary.outbox_high_seq = store.finalize_scan_task_from_items(task_id, now_ms)?;
        if remote_available {
            publish_outbox(store, &mut remote, &mut remote_available).await;
        }
        let task = store.task_snapshot(task_id)?;
        let skipped = task
            .cancelled
            .saturating_add(runtime_u64(summary.skipped_incomplete));
        let failed = task
            .failed
            .saturating_sub(runtime_u64(summary.skipped_incomplete));
        reporter
            .update_stage_nowait(RuntimeStageUpdate {
                stage: RuntimeStage::ComputeBaseFeatures,
                state: proto::RuntimeStageState::RuntimeStageCompleted,
                unit: RuntimeProgressUnit::Files,
                completed: task.succeeded,
                total: Some(runtime_u64(total_files)),
                failed,
                skipped,
            })
            .map_err(runtime_error)?;
        reporter
            .update_overall_nowait(
                task.succeeded,
                Some(runtime_u64(total_files)),
                failed,
                skipped,
            )
            .map_err(runtime_error)?;
        save_base_stage(
            store,
            task_id,
            RuntimeStage::ComputeBaseFeatures,
            PersistentStageState::Completed,
            task.succeeded,
            Some(runtime_u64(total_files)),
            failed,
            skipped,
            Some(compute_started),
            Some(wall_clock_ms()),
            cache_warning,
        )?;
        Ok(summary)
    }
}

/// 已在 SQLite 保留的枚举行。
struct ReservedBase {
    scanned: ScannedPath,
    item_id: String,
}

/// Node 完成 MD5 且已经释放 Hash 磁盘许可的任务项。
struct HashedBaseItem {
    /// 当前基础计算任务身份。
    task_id: TaskId,
    /// 持久任务项身份。
    item_id: String,
    /// 枚举时冻结的路径与文件大小。
    scanned: ScannedPath,
    /// Node 读取完整文件得到的 MD5。
    md5: [u8; 16],
    /// Hash 调度解析出的物理盘显示身份。
    physical_disk_id: String,
    /// 从 Hash claim 起随文件移动的内容输出 credit。
    output_credit: ContentOutputCredit,
}

/// 缓存判定后等待一次性 Worker 补算的冻结作业。
struct BaseComputeJob {
    /// 持久任务项身份。
    item_id: String,
    /// 枚举时冻结的路径与文件大小。
    scanned: ScannedPath,
    /// Node 已计算并用于内容身份的 MD5。
    md5: [u8; 16],
    /// Hash 阶段解析出的物理盘显示身份。
    physical_disk_id: String,
    /// NodeStore 中已经关联的位置内容 ID。
    content_id: ContentId,
    /// 冻结的媒体类型与缺失掩码判定。
    decision: BaseComputeDecision,
    /// 首次注册媒体请求前仍由该文件持有的内容输出 credit。
    output_credit: Option<ContentOutputCredit>,
    /// 从 content 规划到权威 Worker Started 前持续持有的解码 credit。
    decode_credit: Option<DecodeCredit>,
}

/// content 规划阶段的只读结果；CacheHit 不需要取得解码 credit。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ContentResolutionNeed {
    /// 本地或远端已有足够完整的基础缓存，可直接结束任务项。
    CacheHit,
    /// 仍有缺失字段，需要取得 decode credit 后构造 Worker job。
    WorkerCompute,
}

/// 派发 ACK 返回前由协调器独占的 Worker 作业和媒体许可。
struct PendingWorkerDispatch {
    /// 尚未进入 active 的完整计算 job，含 decode credit。
    job: BaseComputeJob,
    /// Worker 读取开始前必须随 job 一起转移的媒体许可。
    media_permit: Option<ErasedMediaPermit>,
}

/// actor 等待一批 path 远端结果时保存的批次计数。
struct PathBatchContext {
    /// 本批在冻结输入中的原始行数，包含被 SQLite 去重跳过的行。
    input_count: usize,
    /// 必须与 resolver 结果逐项对应的远端查询数。
    expected_items: usize,
}

/// path 远端候选返回前由 actor 独占的 SQLite 决策上下文。
struct PathResolveContext {
    /// 已保留的持久任务项和冻结路径。
    reserved: ReservedBase,
    /// 查询远端前读取的 SQLite 候选。
    local: Option<BaseCacheRecord>,
}

/// actor 等待一批 content 远端结果时保存的批次计数。
struct ContentBatchContext {
    /// 必须与 resolver 结果逐项对应的远端查询数。
    expected_items: usize,
    /// 本批随 request/result 移动的显式完成许可数。
    completion_credits: usize,
}

/// content 远端候选返回前由 actor 独占的 Hash 与 SQLite 候选。
struct ContentResolveContext {
    /// 已完成 MD5 且不再持有磁盘读取许可的固定输入。
    hashed: HashedBaseItem,
    /// 查询远端前读取的 SQLite 候选。
    local: Option<BaseCacheRecord>,
}

/// 一项已经由 Store actor 批量读取、等待按 Hash 队首消费的本地缓存结果。
struct LocalContentLookup {
    /// 必须与 `pending_hashed` 队首一致的持久任务项身份。
    item_id: String,
    /// 由 MD5 和冻结文件大小组成的内容键，用于拒绝游标错位。
    content_key: ContentKey,
    /// 本次批量 SQLite 查询返回的完整基础缓存记录。
    local: Option<BaseCacheRecord>,
}

/// 本地 content 批量查询游标；不持有 Worker、CPU、磁盘或完成许可。
struct PendingLocalContentBatch {
    /// 按原始 Hash 队列位置排列的本地结果，背压时保留未消费队首。
    items: VecDeque<LocalContentLookup>,
}

/// actor 每轮只消费一项的 content 结果游标；完整 permit 随 cursor 保留到批次结束。
struct PendingContentResolution {
    /// 原始有界结果及其 completion permit。
    resolution: CacheResolution,
    /// 本批已经消费的稳定身份，拒绝重复项。
    seen: BTreeSet<CacheContextKey>,
}

/// 完整内容缓存命中后交给 actor 落库的显式完成消息。
struct CacheHitCompletion {
    /// 结果所属任务和持久任务项身份。
    key: CacheContextKey,
    /// SQLite 已绑定的内容身份。
    content_id: ContentId,
    /// 缓存记录确认的媒体类型，Applied ACK 后用于吞吐分桶。
    media_kind: MediaKind,
    /// 枚举阶段冻结的文件大小，Applied ACK 后用于吞吐分桶。
    file_size: u64,
    /// Hash 后完整命中项持有到 terminal persist 消息边界的 output credit；path hit 为 None。
    output_credit: Option<ContentOutputCredit>,
}

/// content 缓存排名后的固定 actor 动作；Worker 不会再次查询缓存。
enum ResolvedBaseItem {
    /// 完整缓存命中，直接结束任务项且不占 Worker。
    CacheHit(CacheHitCompletion),
    /// 仍有缺失字段，进入有界 Worker 待派发队列。
    Compute(BaseComputeJob),
}

/// 已交给一次性 Worker 的文件状态；不携带 Hash 磁盘许可。
struct ActiveBase {
    /// 枚举时冻结的路径与文件大小。
    scanned: ScannedPath,
    /// NodeStore 中已经关联的位置内容 ID。
    content_id: ContentId,
    /// Worker 只计算指定缺失部分的冻结判定。
    decision: BaseComputeDecision,
    /// Worker 终态必须原样返回的 Node 内容 MD5。
    expected_md5: [u8; 16],
    /// Worker 尚未报告源读取完成时持有的媒体磁盘许可。
    media_permit: Option<ErasedMediaPermit>,
    /// 实际 Worker 启动后回填的运行槽位。
    worker_slot: Option<u32>,
    /// dispatch 时冻结的完整文件身份；Started 必须逐字段匹配后才可建立 slot。
    worker_identity: WorkerFileIdentity,
    /// ACK 后到权威 Started 之间仍持有的解码 credit。
    decode_credit: Option<DecodeCredit>,
    /// Started 后最近一次权威 Worker phase；缺失、Idle、Unspecified 均投影为 unknown。
    worker_phase: Option<proto::RuntimeWorkerPhase>,
}

impl ActiveBase {
    /// 幂等释放媒体许可；Worker 槽位与 CPU 尾段继续存活。
    fn release_media_permit(&mut self) {
        drop(self.media_permit.take());
    }

    /// 先释放媒体许可，再生成可跨线程发送的纯持久化数据。
    fn into_persist_work(mut self) -> PersistBaseWork {
        self.release_media_permit();
        // Started 前收到终态、取消或派发失败时，由 ActiveBase/待派发 job 的 Drop
        // 统一归还 decode credit；Started 后该字段已经为空，不会重复释放。
        drop(self.decode_credit.take());
        PersistBaseWork {
            scanned: self.scanned,
            content_id: self.content_id,
            decision: self.decision,
            expected_md5: self.expected_md5,
            worker_slot: self.worker_slot,
        }
    }
}

/// Worker 资源已经释放后交给单写 actor 的拥有型文件数据。
struct PersistBaseWork {
    /// 枚举时冻结的路径与文件大小，仅用于错误详情。
    scanned: ScannedPath,
    /// Worker 领取前冻结的 SQLite 内容身份。
    content_id: ContentId,
    /// Worker 只计算指定缺失部分的冻结判定。
    decision: BaseComputeDecision,
    /// Node Hash 阶段确定的内容 MD5。
    expected_md5: [u8; 16],
    /// 仅用于 ACK 后清理运行时显示，不代表 Worker 或 CPU 许可所有权。
    worker_slot: Option<u32>,
}

/// 先完成整次枚举，再一次性持久化固定任务项总数。
fn reserve_rows(
    store: &BaseStoreHandle,
    task_id: TaskId,
    rows: Vec<ScannedPath>,
    now_ms: i64,
) -> Result<Vec<ReservedBase>, ScanError> {
    rows.into_iter()
        .filter_map(
            |scanned| match store.reserve_scan_path(task_id, &scanned, now_ms) {
                Ok(Some(item_id)) => Some(Ok(ReservedBase { scanned, item_id })),
                Ok(None) => None,
                Err(error) => Some(Err(error.into())),
            },
        )
        .collect()
}

/// 在任何新 reserve/claim/submit/dispatch 或迟到结果写入前复查任务取消状态。
fn ensure_task_running(
    store: &BaseStoreHandle,
    task_id: TaskId,
    cancellation: &ReadCancellationToken,
) -> Result<(), ScanError> {
    if cancellation.is_cancelled() || store.task_snapshot(task_id)?.status == TaskStatus::Cancelled
    {
        return Err(ScanError::Cancelled);
    }
    Ok(())
}

/// 持久化事务占用 SQLite writer 时只检查进程内取消，避免协调器自阻塞。
fn ensure_not_cancelled(cancellation: &ReadCancellationToken) -> Result<(), ScanError> {
    if cancellation.is_cancelled() {
        return Err(ScanError::Cancelled);
    }
    Ok(())
}

/// 非阻塞填充有界持久化队列；满队列保留消息，让主循环先消费 ACK。
fn flush_persist_messages(
    store: &BaseStoreHandle,
    pending: &mut VecDeque<BasePersistMessage>,
    in_flight: &mut usize,
) -> Result<(), ScanError> {
    while let Some(message) = pending.pop_front() {
        match store.try_persist(message) {
            Ok(()) => {
                *in_flight = in_flight
                    .checked_add(1)
                    .ok_or_else(|| ScanError::Stage1("基础持久化 pending ACK 计数溢出".into()))?;
            }
            Err(BasePersistSendError::Full(message)) => {
                pending.push_front(message);
                break;
            }
            Err(BasePersistSendError::Closed(message)) => {
                return Err(ScanError::Stage1(format!(
                    "基础持久化 actor 已关闭: item_id={}",
                    message.identity.item_id()
                )));
            }
        }
    }
    Ok(())
}

/// 把不占 Worker 的缓存命中或读取失败统一封装为 guarded 完成消息。
fn queue_guarded_completion(
    pending: &mut VecDeque<BasePersistMessage>,
    identity: TaskItemIdentity,
    completion: TaskItemCompletion,
    outcome: BasePersistOutcome,
    output_credit: Option<ContentOutputCredit>,
    now_ms: i64,
) {
    let operation_identity = identity.clone();
    pending.push_back(BasePersistMessage::new(identity, move |store| {
        // 把 credit 捕获进 actor operation；消息入队/actor gate 期间保持 ownership，
        // operation 返回后由闭包 Drop 恰好释放一次。
        let _output_credit = output_credit;
        let result = store
            .complete_item_guarded(&operation_identity, completion, now_ms)
            .map_err(|error| error.to_string())?;
        guarded_outcome(&operation_identity, result, outcome)
    }));
}

/// 仅在 SQLite ACK 后推进汇总与运行时报告；发送成功本身不算文件成功。
async fn apply_persist_ack(
    acknowledgement: BasePersistAck,
    in_flight: &mut usize,
    reporter: &RuntimeTaskReporter,
    summary: &mut ScanSummary,
    item_started_at: &mut BTreeMap<String, Instant>,
) -> Result<(), ScanError> {
    *in_flight = in_flight
        .checked_sub(1)
        .ok_or_else(|| ScanError::Stage1("收到没有 pending 的基础持久化 ACK".into()))?;
    reporter
        .record_queue_wait_nowait(RuntimePipelineQueue::Persist, acknowledgement.queue_wait)
        .map_err(runtime_error)?;
    reporter
        .record_queue_service_nowait(
            RuntimePipelineQueue::Persist,
            acknowledgement.transaction_elapsed,
        )
        .map_err(runtime_error)?;
    let outcome = acknowledgement.result.map_err(|error| {
        let task_id = acknowledgement.identity.task_id().map_or_else(
            || "<task-file>".to_owned(),
            |task_id| task_id.as_uuid().to_string(),
        );
        ScanError::Stage1(format!(
            "基础持久化失败: task_id={}, item_id={}, error={error}",
            task_id,
            acknowledgement.identity.item_id()
        ))
    })?;
    let started_at = item_started_at.remove(&acknowledgement.identity.item_id());
    if outcome.is_applied()
        && let Some(started_at) = started_at
    {
        reporter
            .record_item_completion_latency_nowait(started_at.elapsed())
            .map_err(runtime_error)?;
    }
    match outcome {
        BasePersistOutcome::Succeeded {
            worker_slot,
            cache_hit,
            media_kind,
            file_size,
        } => {
            if cache_hit {
                summary.cache_hits += 1;
            }
            reporter
                .advance_stage_outcome_nowait(
                    RuntimeStage::ComputeBaseFeatures,
                    RuntimeProgressUnit::Files,
                    1,
                    0,
                    0,
                )
                .map_err(runtime_error)?;
            reporter
                .record_media_throughput_nowait(media_kind, file_size)
                .map_err(runtime_error)?;
            if let Some(slot) = worker_slot {
                let _ = reporter.worker_completed(slot).await;
            }
        }
        BasePersistOutcome::Failed {
            display_path,
            message,
            worker_slot,
            skipped_incomplete,
        } => {
            summary.file_failures += 1;
            if skipped_incomplete {
                summary.skipped_incomplete += 1;
            }
            reporter
                .record_failure_nowait(RuntimeFailureUpdate {
                    stage: RuntimeStage::ComputeBaseFeatures,
                    display_path,
                    message,
                })
                .map_err(runtime_error)?;
            reporter
                .advance_stage_outcome_nowait(
                    RuntimeStage::ComputeBaseFeatures,
                    RuntimeProgressUnit::Files,
                    0,
                    u64::from(!skipped_incomplete),
                    u64::from(skipped_incomplete),
                )
                .map_err(runtime_error)?;
            if let Some(slot) = worker_slot {
                let _ = reporter.worker_completed(slot).await;
            }
        }
        BasePersistOutcome::Cancelled { worker_slot } => {
            reporter
                .advance_stage_outcome_nowait(
                    RuntimeStage::ComputeBaseFeatures,
                    RuntimeProgressUnit::Files,
                    0,
                    0,
                    1,
                )
                .map_err(runtime_error)?;
            if let Some(slot) = worker_slot {
                let _ = reporter.worker_completed(slot).await;
            }
        }
        BasePersistOutcome::Ignored => {
            let task_id = acknowledgement.identity.task_id().map_or_else(
                || "<task-file>".to_owned(),
                |task_id| task_id.as_uuid().to_string(),
            );
            tracing::debug!(
                task_id = %task_id,
                item_id = %acknowledgement.identity.item_id(),
                "忽略已经失活的基础持久化结果"
            );
        }
    }
    Ok(())
}

/// 尝试发送一个缓存请求；满通道只保留原请求，绝不在 actor 中等待。
fn try_send_cache_request(
    sender: &tokio::sync::mpsc::Sender<CacheResolveRequest>,
    pending: &mut Option<CacheResolveRequest>,
) -> Result<(), ScanError> {
    let Some(request) = pending.take() else {
        return Ok(());
    };
    match sender.try_send(request) {
        Ok(()) => Ok(()),
        Err(TrySendError::Full(request)) => {
            *pending = Some(request);
            Ok(())
        }
        Err(TrySendError::Closed(_)) => {
            Err(ScanError::Stage1("缓存 resolver 请求通道已关闭".into()))
        }
    }
}

/// 分配跨 path/content lane 唯一的请求身份。
fn allocate_cache_request_id(next_request_id: &mut u64) -> Result<u64, ScanError> {
    let request_id = *next_request_id;
    *next_request_id = request_id
        .checked_add(1)
        .ok_or_else(|| ScanError::Stage1("缓存 resolver 请求身份耗尽".into()))?;
    Ok(request_id)
}

/// 从冻结清单准备一批 path 查询；本地完整项直接完成，远端候选异步返回。
#[allow(clippy::too_many_arguments)]
fn prepare_path_batch(
    store: &BaseStoreHandle,
    task_id: TaskId,
    options: &ScanOptions,
    contact_sheet_root: &std::path::Path,
    remaining_rows: &mut VecDeque<ScannedPath>,
    next_request_id: &mut u64,
    remote_available: bool,
    pending_request: &mut Option<CacheResolveRequest>,
    path_batches: &mut BTreeMap<u64, PathBatchContext>,
    path_contexts: &mut BTreeMap<CacheContextKey, PathResolveContext>,
    hash_item_ids: &BTreeSet<String>,
    media_item_ids: &BTreeSet<String>,
    active: &BTreeMap<String, ActiveBase>,
    worker_dispatching_item: &Option<String>,
    lookup_completed: &mut u64,
    total_files: usize,
    lookup_started: u64,
    reporter: &RuntimeTaskReporter,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    cache_warning: Option<&str>,
    now_ms: i64,
    refill: &mut HashRefillController,
) -> Result<(), ScanError> {
    let context_room = MAX_PATH_CACHE_CONTEXTS
        .checked_sub(path_contexts.len())
        .ok_or_else(|| ScanError::Stage1("path 缓存上下文超过产品硬上限".into()))?;
    if context_room == 0 || remaining_rows.is_empty() {
        return Ok(());
    }
    let input_count = remaining_rows
        .len()
        .min(REMOTE_LOOKUP_BATCH_SIZE)
        .min(context_room);
    let mut rows = Vec::with_capacity(input_count);
    for _ in 0..input_count {
        rows.push(
            remaining_rows
                .pop_front()
                .expect("批次长度来自 remaining_rows 当前长度"),
        );
    }
    let reserved = reserve_rows(store, task_id, rows, now_ms)?;
    let local_records = store.lookup_base_cache_by_paths(
        &reserved
            .iter()
            .map(|row| row.scanned.clone())
            .collect::<Vec<_>>(),
    )?;
    if local_records.len() != reserved.len() {
        return Err(ScanError::Stage1(
            "SQLite path 基础缓存批量返回数量不匹配".into(),
        ));
    }

    let mut remote_items = Vec::with_capacity(reserved.len());
    let mut remote_contexts = Vec::with_capacity(reserved.len());
    for (reserved, local) in reserved.into_iter().zip(local_records) {
        if remote_available && !options.force_recompute && !cache_fully_computed(local.as_ref()) {
            let key = CacheContextKey {
                task_id,
                item_id: reserved.item_id.clone(),
            };
            ensure_cache_wait_holds_no_compute_resource(
                &key.item_id,
                hash_item_ids,
                media_item_ids,
                active,
                worker_dispatching_item.as_ref(),
            )?;
            remote_items.push(PathResolveItem {
                key: key.clone(),
                scanned: reserved.scanned.clone(),
            });
            remote_contexts.push((key, PathResolveContext { reserved, local }));
        } else {
            apply_path_context(
                store,
                task_id,
                options,
                contact_sheet_root,
                PathResolveContext { reserved, local },
                None,
                pending_persist,
                now_ms,
                refill,
            )?;
        }
    }

    if remote_items.is_empty() {
        return advance_lookup_progress(
            store,
            task_id,
            input_count,
            lookup_completed,
            total_files,
            lookup_started,
            reporter,
            cache_warning,
        );
    }
    if path_contexts.len() + remote_contexts.len() > MAX_PATH_CACHE_CONTEXTS {
        return Err(ScanError::Stage1("path 缓存上下文超过产品硬上限".into()));
    }
    let request_id = allocate_cache_request_id(next_request_id)?;
    for (key, context) in remote_contexts {
        if path_contexts.insert(key, context).is_some() {
            return Err(ScanError::Stage1("path 缓存上下文身份重复".into()));
        }
    }
    path_batches.insert(
        request_id,
        PathBatchContext {
            input_count,
            expected_items: remote_items.len(),
        },
    );
    *pending_request = Some(CacheResolveRequest::Paths {
        request_id,
        enqueued_at: Instant::now(),
        machine_id: store.machine_id().clone(),
        items: remote_items,
    });
    Ok(())
}

/// 按冻结 item 身份证明缓存等待项未持有 Hash、媒体或 Worker 资源。
fn ensure_cache_wait_holds_no_compute_resource(
    item_id: &str,
    hash_item_ids: &BTreeSet<String>,
    media_item_ids: &BTreeSet<String>,
    active: &BTreeMap<String, ActiveBase>,
    worker_dispatching_item: Option<&String>,
) -> Result<(), ScanError> {
    if hash_item_ids.contains(item_id)
        || media_item_ids.contains(item_id)
        || active.contains_key(item_id)
        || worker_dispatching_item.is_some_and(|dispatching| dispatching == item_id)
    {
        return Err(ScanError::Stage1(format!(
            "CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION: item_id={item_id}"
        )));
    }
    Ok(())
}

/// 应用一项 path 本地/远端排名结果，并立即产出缓存命中或 Hash 队列项。
#[allow(clippy::too_many_arguments)]
fn apply_path_context(
    store: &BaseStoreHandle,
    task_id: TaskId,
    options: &ScanOptions,
    contact_sheet_root: &std::path::Path,
    context: PathResolveContext,
    remote: Option<BaseCacheRecord>,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    now_ms: i64,
    refill: &mut HashRefillController,
) -> Result<(), ScanError> {
    let mut cached = context.local;
    if remote
        .as_ref()
        .is_some_and(|record| cache_rank(Some(record)) > cache_rank(cached.as_ref()))
    {
        let record = remote.as_ref().expect("已确认远端候选排名更高");
        let content = store.import_base_cache_record(&context.reserved.scanned, record)?;
        cached = Some(store.load_base_cache_record(content.id)?);
    }
    let contact_exists = contact_sheet_valid_for_record(contact_sheet_root, cached.as_ref());
    let decision =
        BaseComputeDecision::for_cache(cached.as_ref(), contact_exists, options.force_recompute);
    if decision.missing_parts() == 0 {
        let media_kind = decision.media_kind();
        let file_size = context.reserved.scanned.file_size;
        let content_id = cached.and_then(|record| record.content_id);
        queue_guarded_completion(
            pending_persist,
            TaskItemIdentity {
                task_id,
                item_id: context.reserved.item_id.clone(),
                content_id: None,
            },
            TaskItemCompletion::Succeeded { content_id },
            BasePersistOutcome::Succeeded {
                worker_slot: None,
                cache_hit: true,
                media_kind,
                file_size,
            },
            None,
            now_ms,
        );
    } else {
        store.queue_scan_item_for_read(&context.reserved.item_id)?;
        refill.on_upstream_item_published();
    }
    Ok(())
}

/// 路径批次完整归并后同时推进运行时和 SQLite 可恢复进度。
#[allow(clippy::too_many_arguments)]
fn advance_lookup_progress(
    store: &BaseStoreHandle,
    task_id: TaskId,
    batch_count: usize,
    lookup_completed: &mut u64,
    total_files: usize,
    lookup_started: u64,
    reporter: &RuntimeTaskReporter,
    cache_warning: Option<&str>,
) -> Result<(), ScanError> {
    *lookup_completed = lookup_completed
        .checked_add(runtime_u64(batch_count))
        .ok_or_else(|| ScanError::Stage1("path 缓存进度溢出".into()))?;
    if *lookup_completed > runtime_u64(total_files) {
        return Err(ScanError::Stage1("path 缓存进度超过冻结总数".into()));
    }
    reporter
        .update_stage_nowait(RuntimeStageUpdate::running(
            RuntimeStage::LookupBaseCache,
            RuntimeProgressUnit::Files,
            *lookup_completed,
            Some(runtime_u64(total_files)),
        ))
        .map_err(runtime_error)?;
    save_base_stage(
        store,
        task_id,
        RuntimeStage::LookupBaseCache,
        PersistentStageState::Running,
        *lookup_completed,
        Some(runtime_u64(total_files)),
        0,
        0,
        Some(lookup_started),
        None,
        cache_warning.map(str::to_owned),
    )
}

/// 所有 path 批次完成后关闭 lookup 阶段；计算阶段可已并行推进。
fn finish_lookup_stage(
    store: &BaseStoreHandle,
    task_id: TaskId,
    total_files: usize,
    lookup_started: u64,
    reporter: &RuntimeTaskReporter,
    cache_warning: Option<String>,
) -> Result<(), ScanError> {
    reporter
        .update_stage_nowait(RuntimeStageUpdate::running(
            RuntimeStage::LookupBaseCache,
            RuntimeProgressUnit::Files,
            runtime_u64(total_files),
            Some(runtime_u64(total_files)),
        ))
        .map_err(runtime_error)?;
    reporter
        .finish_stage_nowait(
            RuntimeStage::LookupBaseCache,
            proto::RuntimeStageState::RuntimeStageCompleted,
            Some(runtime_u64(total_files)),
        )
        .map_err(runtime_error)?;
    save_base_stage(
        store,
        task_id,
        RuntimeStage::LookupBaseCache,
        PersistentStageState::Completed,
        runtime_u64(total_files),
        Some(runtime_u64(total_files)),
        0,
        0,
        Some(lookup_started),
        Some(wall_clock_ms()),
        cache_warning,
    )
}

/// 把一次性远端降级告警写入运行时详情；resolver 自身只负责日志和 local-only 锁定。
fn report_cache_warning(reporter: &RuntimeTaskReporter, warning: &str) -> Result<(), ScanError> {
    reporter
        .record_failure_nowait(RuntimeFailureUpdate {
            stage: RuntimeStage::LookupBaseCache,
            display_path: String::new(),
            message: format!("警告: {warning}"),
        })
        .map_err(runtime_error)
}

/// 为当前 Hash 队首建立一次本地 SQLite 批量查询，并冻结输入位置顺序。
fn load_local_content_batch(
    store: &BaseStoreHandle,
    pending_hashed: &VecDeque<HashedBaseItem>,
    pending_batch: &mut Option<PendingLocalContentBatch>,
) -> Result<(), ScanError> {
    if pending_batch.is_some() || pending_hashed.is_empty() {
        return Ok(());
    }
    let identities = pending_hashed
        .iter()
        .take(REMOTE_LOOKUP_BATCH_SIZE)
        .map(|hashed| {
            (
                hashed.item_id.clone(),
                ContentKey::new(hashed.md5, hashed.scanned.file_size),
            )
        })
        .collect::<Vec<_>>();
    let keys = identities
        .iter()
        .map(|(_, content_key)| content_key.clone())
        .collect::<Vec<_>>();
    #[cfg(feature = "test-hooks")]
    record_local_content_lookup();
    let records = store.lookup_base_cache_by_keys(&keys)?;
    if records.len() != identities.len() {
        return Err(ScanError::Stage1(format!(
            "本地 content 批量查询结果数量不匹配: expected={}, actual={}",
            identities.len(),
            records.len()
        )));
    }
    let items = identities
        .into_iter()
        .zip(records)
        .map(|((item_id, content_key), local)| LocalContentLookup {
            item_id,
            content_key,
            local,
        })
        .collect();
    *pending_batch = Some(PendingLocalContentBatch { items });
    Ok(())
}

/// 把当前已经 ready 的 Hash 合并为 content 批次；本地结果由批量游标逐项消费。
#[allow(clippy::too_many_arguments)]
fn prepare_content_batch(
    store: &BaseStoreHandle,
    task_id: TaskId,
    options: &ScanOptions,
    contact_sheet_root: &std::path::Path,
    queue_capacity: usize,
    remote_available: bool,
    content_credits: &Arc<Semaphore>,
    decode_credits: &DecodeCredits,
    next_request_id: &mut u64,
    pending_hashed: &mut VecDeque<HashedBaseItem>,
    pending_local_batch: &mut Option<PendingLocalContentBatch>,
    pending_compute: &mut VecDeque<BaseComputeJob>,
    pending_request: &mut Option<CacheResolveRequest>,
    content_batches: &mut BTreeMap<u64, ContentBatchContext>,
    content_contexts: &mut BTreeMap<CacheContextKey, ContentResolveContext>,
    hash_item_ids: &BTreeSet<String>,
    media_item_ids: &BTreeSet<String>,
    active: &BTreeMap<String, ActiveBase>,
    worker_dispatching_item: &Option<String>,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    summary: &mut ScanSummary,
    refill: &mut HashRefillController,
    now_ms: i64,
) -> Result<(), ScanError> {
    if pending_request.is_some() {
        return Ok(());
    }
    if pending_local_batch
        .as_ref()
        .is_some_and(|batch| batch.items.is_empty())
    {
        *pending_local_batch = None;
    }
    if pending_local_batch.is_none() && pending_hashed.is_empty() {
        return Ok(());
    }
    ensure_content_output_bound(
        queue_capacity,
        pending_hashed,
        pending_compute,
        content_contexts,
    )?;
    load_local_content_batch(store, pending_hashed, pending_local_batch)?;
    let remote_budget = content_credits
        .available_permits()
        .min(queue_capacity.saturating_sub(content_contexts.len()))
        .min(REMOTE_LOOKUP_BATCH_SIZE);
    let mut remote_items = Vec::new();
    let mut remote_contexts = Vec::new();
    while pending_local_batch
        .as_ref()
        .is_some_and(|batch| !batch.items.is_empty())
        && remote_items.len() < REMOTE_LOOKUP_BATCH_SIZE
    {
        // 先只读两个队首并验证批量结果没有错位；资源不足时两边都不移动。
        let hashed_view = pending_hashed.front().ok_or_else(|| {
            ScanError::Stage1("本地 content 游标仍有结果，但 Hash 队列已经为空".into())
        })?;
        let local_view = pending_local_batch
            .as_ref()
            .and_then(|batch| batch.items.front())
            .expect("循环条件已经确认本地 content 游标非空");
        let hashed_item_id = hashed_view.item_id.clone();
        let content_key = ContentKey::new(hashed_view.md5, hashed_view.scanned.file_size);
        if hashed_view.task_id != task_id {
            return Err(ScanError::Stage1(format!(
                "Hash 结果任务身份不匹配: expected={}, actual={}",
                task_id.as_uuid(),
                hashed_view.task_id.as_uuid()
            )));
        }
        if local_view.item_id != hashed_item_id || local_view.content_key != content_key {
            return Err(ScanError::Stage1(format!(
                "本地 content 游标与 Hash 队首身份不匹配: expected_item={}, actual_item={}",
                hashed_item_id, local_view.item_id
            )));
        }
        let local = local_view.local.as_ref();
        let requires_remote =
            remote_available && !options.force_recompute && !cache_fully_computed(local);
        if !requires_remote {
            let need = content_resolution_need(
                local,
                None,
                contact_sheet_valid_for_record(contact_sheet_root, local),
                options.force_recompute,
            );
            let decode_credit = match need {
                ContentResolutionNeed::CacheHit => None,
                ContentResolutionNeed::WorkerCompute => {
                    let Some(credit) = decode_credits.try_acquire() else {
                        break;
                    };
                    Some(credit)
                }
            };
            let local = pending_local_batch
                .as_mut()
                .and_then(|batch| batch.items.pop_front())
                .expect("只读规划成功后本地 content 游标队首仍应存在")
                .local;
            let hashed = pending_hashed
                .pop_front()
                .expect("只读规划成功后仍应保留 Hash 队首");
            let resolved = resolve_content_context(
                store,
                options,
                contact_sheet_root,
                ContentResolveContext { hashed, local },
                None,
                summary,
                refill,
                decode_credit,
            )?;
            apply_resolved_base_item(task_id, pending_compute, pending_persist, resolved, now_ms)?;
            continue;
        }
        if remote_items.len() >= remote_budget {
            break;
        }
        let key = CacheContextKey {
            task_id,
            item_id: hashed_item_id,
        };
        ensure_cache_wait_holds_no_compute_resource(
            &key.item_id,
            hash_item_ids,
            media_item_ids,
            active,
            worker_dispatching_item.as_ref(),
        )?;
        let local_lookup = pending_local_batch
            .as_mut()
            .and_then(|batch| batch.items.pop_front())
            .expect("远端 content 规划通过后本地游标队首仍应存在");
        remote_items.push(ContentResolveItem {
            key: key.clone(),
            content_key: local_lookup.content_key,
        });
        let hashed = pending_hashed
            .pop_front()
            .expect("远端 content 规划通过后仍应保留 Hash 队首");
        remote_contexts.push((
            key,
            ContentResolveContext {
                hashed,
                local: local_lookup.local,
            },
        ));
    }

    if pending_local_batch
        .as_ref()
        .is_some_and(|batch| batch.items.is_empty())
    {
        *pending_local_batch = None;
    }

    if remote_items.is_empty() {
        ensure_content_output_bound(
            queue_capacity,
            pending_hashed,
            pending_compute,
            content_contexts,
        )?;
        return Ok(());
    }
    let credit_count = u32::try_from(remote_items.len())
        .map_err(|_| ScanError::Stage1("content 完成许可数量超过 u32".into()))?;
    let completion_credits = Arc::new(
        Arc::clone(content_credits)
            .try_acquire_many_owned(credit_count)
            .map_err(|_| ScanError::Stage1("content 完成许可与 actor 上下文不一致".into()))?,
    );
    for (key, context) in remote_contexts {
        if content_contexts.insert(key, context).is_some() {
            return Err(ScanError::Stage1("content 缓存上下文身份重复".into()));
        }
    }
    ensure_content_output_bound(
        queue_capacity,
        pending_hashed,
        pending_compute,
        content_contexts,
    )?;
    let request_id = allocate_cache_request_id(next_request_id)?;
    content_batches.insert(
        request_id,
        ContentBatchContext {
            expected_items: remote_items.len(),
            completion_credits: completion_credits.num_permits(),
        },
    );
    *pending_request = Some(CacheResolveRequest::Contents {
        request_id,
        enqueued_at: Instant::now(),
        items: remote_items,
        completion_credits,
    });
    Ok(())
}

/// 归并 path resolver 结果；每项立即产生缓存命中或可领取 Hash work。
#[allow(clippy::too_many_arguments)]
fn handle_path_resolution(
    resolution: CacheResolution,
    store: &BaseStoreHandle,
    task_id: TaskId,
    options: &ScanOptions,
    contact_sheet_root: &std::path::Path,
    reporter: &RuntimeTaskReporter,
    path_batches: &mut BTreeMap<u64, PathBatchContext>,
    path_contexts: &mut BTreeMap<CacheContextKey, PathResolveContext>,
    lookup_completed: &mut u64,
    total_files: usize,
    lookup_started: u64,
    cache_warning: Option<&str>,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    now_ms: i64,
    refill: &mut HashRefillController,
) -> Result<(), ScanError> {
    if resolution.completion_credit_count() != 0 {
        return Err(ScanError::Stage1(
            "path 缓存结果不得携带 content 完成许可".into(),
        ));
    }
    let CacheResolutionKind::Paths(items) = resolution.kind else {
        return Err(ScanError::Stage1("path 归并收到 content 缓存结果".into()));
    };
    let batch = path_batches.remove(&resolution.request_id).ok_or_else(|| {
        ScanError::Stage1(format!(
            "收到未知 path 缓存请求结果: {}",
            resolution.request_id
        ))
    })?;
    if let Some(wait) = resolution.queue_wait {
        reporter
            .record_queue_wait_nowait(RuntimePipelineQueue::PathCache, wait)
            .map_err(runtime_error)?;
    }
    if let Some(elapsed) = resolution.query_elapsed {
        reporter
            .record_queue_service_nowait(RuntimePipelineQueue::PathCache, elapsed)
            .map_err(runtime_error)?;
    }
    if items.len() != batch.expected_items {
        return Err(ScanError::Stage1("path 缓存 actor 结果数量不匹配".into()));
    }
    let mut seen = BTreeSet::new();
    for item in items {
        if item.key.task_id != task_id || !seen.insert(item.key.clone()) {
            return Err(ScanError::Stage1("path 缓存结果身份无效或重复".into()));
        }
        let context = path_contexts
            .remove(&item.key)
            .ok_or_else(|| ScanError::Stage1("path 缓存结果缺少 actor 上下文".into()))?;
        apply_path_context(
            store,
            task_id,
            options,
            contact_sheet_root,
            context,
            item.remote,
            pending_persist,
            now_ms,
            refill,
        )?;
    }
    advance_lookup_progress(
        store,
        task_id,
        batch.input_count,
        lookup_completed,
        total_files,
        lookup_started,
        reporter,
        cache_warning,
    )
}

/// 校验多项 content 结果并建立只由 actor 持有的逐项消费游标。
fn begin_content_resolution(
    resolution: CacheResolution,
    content_batches: &mut BTreeMap<u64, ContentBatchContext>,
    reporter: &RuntimeTaskReporter,
) -> Result<PendingContentResolution, ScanError> {
    let CacheResolutionKind::Contents(items) = &resolution.kind else {
        return Err(ScanError::Stage1("content 游标收到 path 缓存结果".into()));
    };
    let batch = content_batches
        .remove(&resolution.request_id)
        .ok_or_else(|| {
            ScanError::Stage1(format!(
                "收到未知 content 缓存请求结果: {}",
                resolution.request_id
            ))
        })?;
    if let Some(wait) = resolution.queue_wait {
        reporter
            .record_queue_wait_nowait(RuntimePipelineQueue::ContentCache, wait)
            .map_err(runtime_error)?;
    }
    if let Some(elapsed) = resolution.query_elapsed {
        reporter
            .record_queue_service_nowait(RuntimePipelineQueue::ContentCache, elapsed)
            .map_err(runtime_error)?;
    }
    let credits = resolution.completion_credit_count();
    if items.len() != batch.expected_items
        || credits != batch.completion_credits
        || items.len() != credits
    {
        return Err(ScanError::Stage1(
            "content 缓存 actor 结果与完成许可数量不匹配".into(),
        ));
    }
    Ok(PendingContentResolution {
        resolution,
        seen: BTreeSet::new(),
    })
}

/// 消费一项 content 结果；每轮返回 actor select，使 Worker 终态保持最高优先级。
#[allow(clippy::too_many_arguments)]
fn process_next_content_resolution(
    cursor: &mut PendingContentResolution,
    store: &BaseStoreHandle,
    task_id: TaskId,
    options: &ScanOptions,
    contact_sheet_root: &std::path::Path,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    summary: &mut ScanSummary,
    content_contexts: &mut BTreeMap<CacheContextKey, ContentResolveContext>,
    decode_credits: &DecodeCredits,
    now_ms: i64,
    refill: &mut HashRefillController,
) -> Result<(bool, Option<BaseComputeJob>), ScanError> {
    let CacheResolutionKind::Contents(items) = &mut cursor.resolution.kind else {
        return Err(ScanError::Stage1("content 游标类型在消费期间改变".into()));
    };
    let item_view = items
        .front()
        .ok_or_else(|| ScanError::Stage1("content 游标缺少待消费项目".into()))?;
    let context_view = content_contexts
        .get(&item_view.key)
        .ok_or_else(|| ScanError::Stage1("content 缓存结果缺少 actor 上下文".into()))?;
    let contact_exists = selected_contact_sheet_valid(
        contact_sheet_root,
        context_view.local.as_ref(),
        item_view.remote.as_ref(),
    );
    let need = content_resolution_need(
        context_view.local.as_ref(),
        item_view.remote.as_ref(),
        contact_exists,
        options.force_recompute,
    );
    let decode_credit = match need {
        ContentResolutionNeed::CacheHit => None,
        ContentResolutionNeed::WorkerCompute => {
            let Some(credit) = decode_credits.try_acquire() else {
                // 没有 credit 时不消费 cursor 或 actor context，等待下一轮 Started/终态。
                return Ok((false, None));
            };
            Some(credit)
        }
    };
    let item = items
        .pop_front()
        .expect("只读规划成功后 content cursor 仍应保留队首");
    if item.key.task_id != task_id || !cursor.seen.insert(item.key.clone()) {
        return Err(ScanError::Stage1("content 缓存结果身份无效或重复".into()));
    }
    let context = content_contexts
        .remove(&item.key)
        .ok_or_else(|| ScanError::Stage1("content 缓存结果缺少 actor 上下文".into()))?;
    let resolved = resolve_content_context(
        store,
        options,
        contact_sheet_root,
        context,
        item.remote,
        summary,
        refill,
        decode_credit,
    )?;
    let job = match resolved {
        ResolvedBaseItem::CacheHit(completion) => {
            complete_cache_hit(task_id, pending_persist, completion, now_ms)?;
            None
        }
        ResolvedBaseItem::Compute(job) => Some(job),
    };
    Ok((items.is_empty(), job))
}

/// 判断 content cursor 当前队首是否可以推进；CacheHit 不需要 credit，WorkerCompute 必须先有 credit。
fn content_cursor_has_admission(
    cursor: Option<&PendingContentResolution>,
    content_contexts: &BTreeMap<CacheContextKey, ContentResolveContext>,
    contact_sheet_root: &std::path::Path,
    force_recompute: bool,
    decode_credits: &DecodeCredits,
) -> bool {
    let Some(cursor) = cursor else {
        return false;
    };
    let CacheResolutionKind::Contents(items) = &cursor.resolution.kind else {
        return false;
    };
    let Some(item) = items.front() else {
        return false;
    };
    let Some(context) = content_contexts.get(&item.key) else {
        return false;
    };
    let need = content_resolution_need(
        context.local.as_ref(),
        item.remote.as_ref(),
        selected_contact_sheet_valid(
            contact_sheet_root,
            context.local.as_ref(),
            item.remote.as_ref(),
        ),
        force_recompute,
    );
    matches!(need, ContentResolutionNeed::CacheHit) || decode_credits.available_permits() > 0
}

/// 在 SQLite actor 内完成本地/远端内容排名、导入、位置 upsert 与阶段冻结。
fn resolve_content_context(
    store: &BaseStoreHandle,
    options: &ScanOptions,
    contact_sheet_root: &std::path::Path,
    context: ContentResolveContext,
    remote: Option<BaseCacheRecord>,
    summary: &mut ScanSummary,
    refill: &mut HashRefillController,
    decode_credit: Option<DecodeCredit>,
) -> Result<ResolvedBaseItem, ScanError> {
    // 先完成只读规划；没有 decode credit 时在任何 import/upsert/Store 写入前返回。
    let planned_need = content_resolution_need(
        context.local.as_ref(),
        remote.as_ref(),
        selected_contact_sheet_valid(contact_sheet_root, context.local.as_ref(), remote.as_ref()),
        options.force_recompute,
    );
    match (planned_need, decode_credit.is_some()) {
        (ContentResolutionNeed::WorkerCompute, false) => {
            return Err(ScanError::Stage1(
                "Worker 计算候选缺少 decode credit".into(),
            ));
        }
        (ContentResolutionNeed::CacheHit, true) => {
            return Err(ScanError::Stage1("缓存命中不应持有 decode credit".into()));
        }
        _ => {}
    }
    let hashed = context.hashed;
    let mut cached = context.local;
    if remote
        .as_ref()
        .is_some_and(|record| cache_rank(Some(record)) > cache_rank(cached.as_ref()))
    {
        let record = remote.as_ref().expect("已确认远端内容候选排名更高");
        let content = store.import_base_cache_record(&hashed.scanned, record)?;
        summary.reused_contents += 1;
        cached = Some(store.load_base_cache_record(content.id)?);
    }
    let content_id = if let Some(cached) = cached.as_ref() {
        let content_id = cached.content_id.expect("SQLite 缓存必须有本地内容 ID");
        store.upsert_content_and_location(&hashed.scanned, hashed.md5, cached.media_kind)?;
        content_id
    } else {
        store
            .upsert_content_and_location(&hashed.scanned, hashed.md5, MediaKind::Other)?
            .id
    };
    let contact_exists = contact_sheet_valid_for_record(contact_sheet_root, cached.as_ref());
    let decision =
        BaseComputeDecision::for_cache(cached.as_ref(), contact_exists, options.force_recompute);
    debug_assert_eq!(
        decision.missing_parts() == 0,
        matches!(planned_need, ContentResolutionNeed::CacheHit)
    );
    store.set_running_item_content_and_stage(&hashed.item_id, content_id, "base_compute")?;
    summary.hashed += 1;
    if decision.missing_parts() == 0 {
        refill.on_content_departed(ContentDeparture::TerminalItem);
        return Ok(ResolvedBaseItem::CacheHit(CacheHitCompletion {
            key: CacheContextKey {
                task_id: hashed.task_id,
                item_id: hashed.item_id,
            },
            content_id,
            media_kind: decision.media_kind(),
            file_size: hashed.scanned.file_size,
            output_credit: Some(hashed.output_credit),
        }));
    }
    Ok(ResolvedBaseItem::Compute(BaseComputeJob {
        item_id: hashed.item_id,
        scanned: hashed.scanned,
        md5: hashed.md5,
        physical_disk_id: hashed.physical_disk_id,
        content_id,
        decision,
        output_credit: Some(hashed.output_credit),
        decode_credit,
    }))
}

/// 应用完整缓存命中；显式身份校验后才把成功写入 SQLite。
fn complete_cache_hit(
    task_id: TaskId,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    completion: CacheHitCompletion,
    now_ms: i64,
) -> Result<(), ScanError> {
    if completion.key.task_id != task_id {
        return Err(ScanError::Stage1("缓存命中完成消息任务身份不匹配".into()));
    }
    queue_guarded_completion(
        pending_persist,
        TaskItemIdentity {
            task_id,
            item_id: completion.key.item_id,
            content_id: Some(completion.content_id),
        },
        TaskItemCompletion::Succeeded {
            content_id: Some(completion.content_id),
        },
        BasePersistOutcome::Succeeded {
            worker_slot: None,
            cache_hit: false,
            media_kind: completion.media_kind,
            file_size: completion.file_size,
        },
        completion.output_credit,
        now_ms,
    );
    Ok(())
}

/// 将 content 排名动作应用到缓存命中或有界 Worker 待派发队列。
fn apply_resolved_base_item(
    task_id: TaskId,
    pending_compute: &mut VecDeque<BaseComputeJob>,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    resolved: ResolvedBaseItem,
    now_ms: i64,
) -> Result<(), ScanError> {
    match resolved {
        ResolvedBaseItem::CacheHit(completion) => {
            complete_cache_hit(task_id, pending_persist, completion, now_ms)
        }
        ResolvedBaseItem::Compute(job) => {
            pending_compute.push_back(job);
            Ok(())
        }
    }
}

/// 在一个 select epoch 内最多领取并启动一个 Hash，并先取得独立 output credit。
#[allow(clippy::too_many_arguments)]
fn try_start_one_hash_task<F: PipelineFileReader>(
    store: &BaseStoreHandle,
    task_id: TaskId,
    now_ms: i64,
    hash_capacity: usize,
    reader: &F,
    cancellation: &ReadCancellationToken,
    hashing: &mut JoinSet<HashTaskOutput>,
    phases: &HashPhaseTracker,
    hash_item_ids: &mut BTreeSet<String>,
    item_started_at: &mut BTreeMap<String, Instant>,
    output_credits: &ContentOutputCredits,
    refill: &mut HashRefillController,
    upstream_closed: bool,
    join_observation: &JoinObservationHooks,
) -> Result<HashStartResult, ScanError> {
    if hashing.len() >= hash_capacity {
        return Ok(HashStartResult::NoTaskSlot);
    }
    if upstream_closed {
        // 关闭只允许最后一次 claim；空结果仍由 observe_empty_claim 权威耗尽。
        refill.on_upstream_closed();
    }
    if refill.input_exhausted() {
        return Ok(HashStartResult::InputExhausted);
    }
    if !refill.can_attempt_claim() {
        return Ok(if refill.waiting_for_upstream_publish() {
            HashStartResult::WaitingForUpstream
        } else {
            HashStartResult::NoToken
        });
    }
    let Some(output_credit) = output_credits.try_acquire() else {
        return Ok(HashStartResult::NoOutputCredit);
    };
    #[cfg(feature = "test-hooks")]
    record_claim_attempt();
    let Some(claimed) = store.claim_next_item(task_id, now_ms)? else {
        drop(output_credit);
        refill.observe_empty_claim();
        return Ok(if refill.input_exhausted() {
            HashStartResult::InputExhausted
        } else {
            HashStartResult::WaitingForUpstream
        });
    };
    item_started_at
        .entry(claimed.item_id.clone())
        .or_insert_with(Instant::now);
    if claimed.stage != "read_md5" {
        return Err(ScanError::Stage1(format!(
            "基础计算队列包含未知阶段: {}",
            claimed.stage
        )));
    }
    let scanned = scanned_from_claimed(&claimed)?;
    let task_reader = reader.clone();
    let task_cancellation = cancellation.clone();
    let phase_guard = phases.guard();
    let started_signal = phase_guard.read_started_signal();
    let completion_gate = join_observation.hash.clone();
    hash_item_ids.insert(claimed.item_id.clone());
    hashing.spawn(async move {
        let started = Instant::now();
        let result = hash_one(
            task_reader,
            task_id,
            scanned,
            task_cancellation,
            started_signal,
            output_credit,
        )
        .await;
        phase_guard.mark_completed_unjoined();
        if let Some(gate) = completion_gate {
            gate.mark_completed();
        }
        (claimed, result, started.elapsed(), phase_guard)
    });
    if !refill.consume_after_started() {
        return Err(ScanError::Stage1(
            "Hash spawn 已成功但 refill token 不足".into(),
        ));
    }
    Ok(HashStartResult::Started)
}

/// 完整读取单个文件，并在返回任何无许可数据前显式释放 Hash 许可。
async fn hash_one<F: PipelineFileReader>(
    reader: F,
    task_id: TaskId,
    scanned: ScannedPath,
    cancellation: ReadCancellationToken,
    started: super::base_flow_control::HashReadStartedSignal,
    output_credit: ContentOutputCredit,
) -> Result<HashedBaseItem, HashReadFailure> {
    let product = match reader
        .read_with_phase(scanned.clone(), cancellation, started)
        .await
    {
        Ok(product) => product,
        Err(error) => {
            return Err(HashReadFailure {
                error,
                output_credit,
            });
        }
    };
    let ReadProduct { md5, lease } = product;
    drop(lease);
    let physical_disk_id = reader.physical_disk_id(scanned.display_path.as_path());
    Ok(HashedBaseItem {
        task_id,
        item_id: String::new(),
        scanned,
        md5,
        physical_disk_id,
        output_credit,
    })
}

/// 把持久任务项恢复为 Worker 使用的文件身份。
fn scanned_from_claimed(claimed: &ClaimedTaskItem) -> Result<ScannedPath, ScanError> {
    let location = claimed
        .location
        .as_ref()
        .ok_or_else(|| ScanError::Stage1("基础计算项缺少机器与规范路径".into()))?;
    let display_path = claimed
        .display_path
        .clone()
        .ok_or_else(|| ScanError::Stage1("基础计算项缺少显示路径".into()))?;
    let file_size = claimed
        .file_size
        .ok_or_else(|| ScanError::Stage1("基础计算项缺少文件大小".into()))?;
    Ok(ScannedPath::new(
        location.normalized_path().clone(),
        display_path,
        file_size,
    ))
}

/// 把一个 Hash 终态归并到有界队列；单文件失败只结束该任务项。
#[allow(clippy::too_many_arguments)]
fn handle_hash_result(
    pending_hashed: &mut VecDeque<HashedBaseItem>,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    result: HashTaskOutput,
    reporter: &RuntimeTaskReporter,
    now_ms: i64,
    hash_item_ids: &mut BTreeSet<String>,
    refill: &mut HashRefillController,
) -> Result<(), ScanError> {
    let (claimed, result, elapsed, phase_guard) = result;
    hash_item_ids.remove(&claimed.item_id);
    let scanned = scanned_from_claimed(&claimed)?;
    reporter
        .record_queue_service_nowait(RuntimePipelineQueue::Hash, elapsed)
        .map_err(runtime_error)?;
    match result {
        Ok(mut hashed) => {
            reporter
                .record_hash_bytes_nowait(scanned.file_size)
                .map_err(runtime_error)?;
            hashed.item_id = claimed.item_id;
            pending_hashed.push_back(hashed);
        }
        Err(HashReadFailure {
            error: ReadFailure::Cancelled,
            output_credit,
        }) => {
            // Hash future 返回取消时只 Drop credit，不伪造 terminal departure token。
            drop(output_credit);
            return Err(ScanError::Cancelled);
        }
        Err(HashReadFailure {
            error,
            output_credit,
        }) => {
            // Hash 失败仍把 credit 捕获进 terminal persist operation，直到 actor 边界再 Drop。
            refill.on_content_departed(ContentDeparture::TerminalItem);
            let message = error.to_string();
            queue_guarded_completion(
                pending_persist,
                TaskItemIdentity {
                    task_id: claimed.task_id,
                    item_id: claimed.item_id,
                    content_id: claimed.content_id,
                },
                TaskItemCompletion::Failed(message.clone()),
                BasePersistOutcome::Failed {
                    display_path: scanned
                        .display_path
                        .as_path()
                        .to_string_lossy()
                        .into_owned(),
                    message,
                    worker_slot: None,
                    skipped_incomplete: false,
                },
                Some(output_credit),
                now_ms,
            );
        }
    }
    // 归并完成后才释放 completed-unjoined；归并失败同样不能留下阶段计数。
    drop(phase_guard);
    Ok(())
}

/// 统计尚未派发 Worker 的 Hash/compute 输出所有权。
fn content_output_owned(
    pending_hashed: &VecDeque<HashedBaseItem>,
    pending_compute: &VecDeque<BaseComputeJob>,
) -> Result<usize, ScanError> {
    pending_hashed
        .len()
        .checked_add(pending_compute.len())
        .ok_or_else(|| ScanError::Stage1("content 输出 ownership 计数溢出".into()))
}

/// 校验 actor 的 pending 输出与远端上下文分别没有突破产品硬上限。
fn ensure_content_output_bound(
    queue_capacity: usize,
    pending_hashed: &VecDeque<HashedBaseItem>,
    pending_compute: &VecDeque<BaseComputeJob>,
    content_contexts: &BTreeMap<CacheContextKey, ContentResolveContext>,
) -> Result<(), ScanError> {
    let pending_owned = content_output_owned(pending_hashed, pending_compute)?;
    if pending_owned > queue_capacity {
        return Err(ScanError::Stage1(format!(
            "content pending ownership 超过产品上限: owned={pending_owned}, limit={queue_capacity}"
        )));
    }
    if content_contexts.len() > queue_capacity {
        return Err(ScanError::Stage1(format!(
            "content context ownership 超过产品上限: owned={}, limit={queue_capacity}",
            content_contexts.len()
        )));
    }
    Ok(())
}

/// 单次调度让出后最多归并一个已完成 Hash；不会排空 ready 队列后批量补位。
#[allow(clippy::too_many_arguments)]
fn drain_one_ready_hash_result(
    store: &BaseStoreHandle,
    task_id: TaskId,
    cancellation: &ReadCancellationToken,
    queue_capacity: usize,
    hashing: &mut JoinSet<HashTaskOutput>,
    pending_hashed: &mut VecDeque<HashedBaseItem>,
    pending_compute: &VecDeque<BaseComputeJob>,
    content_contexts: &BTreeMap<CacheContextKey, ContentResolveContext>,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    reporter: &RuntimeTaskReporter,
    now_ms: i64,
    hash_item_ids: &mut BTreeSet<String>,
    refill: &mut HashRefillController,
) -> Result<(), ScanError> {
    let Some(joined) = hashing.try_join_next() else {
        return Ok(());
    };
    let result = joined.map_err(|error| ScanError::Stage1(error.to_string()))?;
    ensure_task_running(store, task_id, cancellation)?;
    handle_hash_result(
        pending_hashed,
        pending_persist,
        result,
        reporter,
        now_ms,
        hash_item_ids,
        refill,
    )?;
    ensure_content_output_bound(
        queue_capacity,
        pending_hashed,
        pending_compute,
        content_contexts,
    )?;
    Ok(())
}

/// 取消后等待所有 Hash future 结束，确保其中的读取许可已经归还。
async fn drain_hash_tasks(hashing: &mut JoinSet<HashTaskOutput>) {
    while hashing.join_next().await.is_some() {}
}

/// 把待计算项搬入独立媒体许可 JoinSet，绝不在 actor 派发路径中等待磁盘。
fn fill_media_acquires<F: PipelineFileReader>(
    worker_capacity: usize,
    reader: &F,
    cancellation: &ReadCancellationToken,
    pending_compute: &mut VecDeque<BaseComputeJob>,
    media_acquiring: &mut JoinSet<MediaAcquireOutput>,
    active: &BTreeMap<String, ActiveBase>,
    phases: &MediaAcquirePhaseTracker,
    media_item_ids: &mut BTreeSet<String>,
    output_credits: &ContentOutputCredits,
    refill: &mut HashRefillController,
    join_observation: &JoinObservationHooks,
) -> Result<(), ScanError> {
    // 预热阶段先让 Hash 消费可用 output credit，避免首个媒体请求过早清空 warmup token。
    if refill.phase() == super::base_flow_control::HashRefillPhase::Warmup
        && refill.available() > 0
        && !refill.waiting_for_upstream_publish()
        && output_credits.available_permits() > 0
    {
        return Ok(());
    }
    ensure_worker_admission_bound(worker_capacity, active, media_acquiring)?;
    while active
        .len()
        .checked_add(media_acquiring.len())
        .is_some_and(|owned| owned < worker_capacity)
    {
        let Some(job) = pending_compute.pop_front() else {
            break;
        };
        // 注册 MediaRequested 即代表离开内容阶段；credit 归还与 token 补位各只发生一次。
        refill.on_content_departed(ContentDeparture::MediaRequested);
        let mut job = job;
        drop(job.output_credit.take());
        let acquire = reader.acquire_media_permit(job.scanned.clone(), cancellation.clone());
        let phase_guard = phases.guard();
        let completion_gate = join_observation.media.clone();
        media_item_ids.insert(job.item_id.clone());
        media_acquiring.spawn(async move {
            let acquired = acquire
                .await
                .map(|permit| permit.map(|permit| Box::new(permit) as ErasedMediaPermit));
            phase_guard.mark_ready(acquired.as_ref().is_ok_and(Option::is_some));
            if let Some(gate) = completion_gate {
                gate.mark_completed();
            }
            (job, acquired, phase_guard)
        });
    }
    ensure_worker_admission_bound(worker_capacity, active, media_acquiring)
}

/// 校验活动 Worker 与许可获取 future 的总所有权不超过真实 Worker 数。
fn ensure_worker_admission_bound(
    worker_capacity: usize,
    active: &BTreeMap<String, ActiveBase>,
    media_acquiring: &JoinSet<MediaAcquireOutput>,
) -> Result<(), ScanError> {
    let owned = active
        .len()
        .checked_add(media_acquiring.len())
        .ok_or_else(|| ScanError::Stage1("Worker admission ownership 计数溢出".into()))?;
    if owned > worker_capacity {
        return Err(ScanError::Stage1(format!(
            "Worker admission ownership 超过真实容量: owned={owned}, limit={worker_capacity}"
        )));
    }
    Ok(())
}

/// 统计尚未进入 Started 身份边界的完整解码 ownership。
fn decode_queue_owned(
    pending_compute: &VecDeque<BaseComputeJob>,
    media_acquiring: &JoinSet<MediaAcquireOutput>,
    active: &BTreeMap<String, ActiveBase>,
    pending_worker_dispatch: Option<&PendingWorkerDispatch>,
) -> Result<usize, ScanError> {
    pending_compute
        .len()
        .checked_add(media_acquiring.len())
        .and_then(|owned| owned.checked_add(usize::from(pending_worker_dispatch.is_some())))
        .and_then(|owned| {
            owned.checked_add(
                active
                    .values()
                    .filter(|work| work.worker_slot.is_none())
                    .count(),
            )
        })
        .ok_or_else(|| ScanError::Stage1("decode ownership 计数溢出".into()))
}

/// 统计协调器本地 pending、writer 执行中和 writer 通道排队的总 ownership。
fn persist_queue_owned(
    pending_persist: &VecDeque<BasePersistMessage>,
    persist_in_flight: usize,
) -> Result<usize, ScanError> {
    pending_persist
        .len()
        .checked_add(persist_in_flight)
        .ok_or_else(|| ScanError::Stage1("persist ownership 计数溢出".into()))
}

/// 媒体许可普通失败只结束当前文件，并让主循环继续补充 Worker admission。
fn fail_media_permit_file(
    task_id: TaskId,
    job: &BaseComputeJob,
    message: String,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    now_ms: i64,
) -> Result<(), ScanError> {
    let display_path = job
        .scanned
        .display_path
        .as_path()
        .to_string_lossy()
        .into_owned();
    tracing::error!(
        item_id = job.item_id,
        path = %display_path,
        error = %message,
        "媒体读取许可失败，跳过当前文件并继续"
    );
    queue_guarded_completion(
        pending_persist,
        TaskItemIdentity {
            task_id,
            item_id: job.item_id.clone(),
            content_id: Some(job.content_id),
        },
        TaskItemCompletion::Failed(message.clone()),
        BasePersistOutcome::Failed {
            display_path,
            message,
            worker_slot: None,
            skipped_incomplete: false,
        },
        None,
        now_ms,
    );
    Ok(())
}

/// 缓存缺失时向空闲容量派发一条 V5 一次性基础计算请求。
#[allow(clippy::too_many_arguments)]
async fn dispatch_compute_job(
    store: &BaseStoreHandle,
    worker_pool: &WorkerPool,
    task_id: TaskId,
    pending: PendingWorkerDispatch,
    read_config: &DiskReadConfig,
    cancellation: &ReadCancellationToken,
    active: &mut BTreeMap<String, ActiveBase>,
    summary: &mut ScanSummary,
) -> Result<(), ScanError> {
    let PendingWorkerDispatch { job, media_permit } = pending;
    let identity = WorkerFileIdentity {
        machine_id: store.machine_id().clone(),
        normalized_path: job.scanned.normalized_path.clone(),
        display_path: job.scanned.display_path.clone(),
        file_size: job.scanned.file_size,
        stage: "base_compute".into(),
        physical_disk_id: job.physical_disk_id.clone(),
    };
    let worker_identity = identity.clone();
    let item_id = job.item_id.clone();
    worker_pool
        .dispatch_scan(
            proto::WorkerEnvelope {
                payload: Some(worker_envelope::Payload::ComputeBaseFeatures(
                    proto::ComputeBaseFeatures {
                        task_id: task_id.as_uuid().to_string(),
                        item_id: item_id.clone(),
                        machine_id: store.machine_id().as_str().to_owned(),
                        normalized_path: job.scanned.normalized_path.as_str().to_owned(),
                        display_path: job
                            .scanned
                            .display_path
                            .as_path()
                            .to_string_lossy()
                            .into_owned(),
                        file_size: job.scanned.file_size,
                        physical_disk_id: job.physical_disk_id.clone(),
                        md5: job.md5.to_vec(),
                        media_kind: proto_media_kind(job.decision.media_kind()) as i32,
                        missing_parts: job.decision.missing_parts(),
                        block_size_bytes: runtime_u64(read_config.block_size_bytes),
                        block_timeout_ms: read_config.block_timeout_seconds.saturating_mul(1_000),
                        block_retries: read_config.block_retries,
                        decoder_threads: worker_pool.decoder_threads_for(job.decision.media_kind()),
                    },
                )),
            },
            cancellation.clone(),
            true,
            identity,
        )
        .await
        .map_err(|error| ScanError::Stage1(format!("Worker 派发失败: {error}")))?;
    active.insert(
        item_id,
        ActiveBase {
            scanned: job.scanned,
            content_id: job.content_id,
            decision: job.decision,
            expected_md5: job.md5,
            media_permit,
            worker_slot: None,
            worker_identity,
            decode_credit: job.decode_credit,
            worker_phase: None,
        },
    );
    summary.scheduled_stage1 += 1;
    Ok(())
}

/// 归并一次性 Worker 的非终态读取事件与唯一终态结果。
#[allow(clippy::too_many_arguments)]
async fn handle_worker_event(
    event: WorkerEvent,
    task_id: TaskId,
    contact_sheet_root: &std::path::Path,
    reporter: &RuntimeTaskReporter,
    artifact_registry: &Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: &DiskFullCleaner,
    active: &mut BTreeMap<String, ActiveBase>,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    now_ms: i64,
) -> Result<(), ScanError> {
    let task_text = task_id.as_uuid().to_string();
    match event {
        WorkerEvent::Started {
            task_id: event_task,
            item_id,
            slot,
            process_id,
            identity,
            cpu_weight,
            decoder_threads,
            queue_wait_us,
        } if event_task == task_text => {
            let Some(work) = active.get_mut(&item_id) else {
                // Started 必须匹配当前 active 身份；孤儿事件不能写入 slot/CPU 资源。
                tracing::warn!(item_id, slot, "忽略没有对应基础计算活动项的 Worker Started");
                return Ok(());
            };
            if let Some(mismatch) = worker_identity_mismatch(&work.worker_identity, &identity) {
                // 同 task/item 仍可能来自错误路径或旧 Worker；身份不全等时不得释放资源。
                tracing::warn!(
                    item_id,
                    slot,
                    mismatched_fields = %mismatch,
                    "忽略 Worker Started：冻结文件身份不匹配"
                );
                return Ok(());
            }
            work.worker_slot = Some(slot);
            // 只有匹配身份的权威 Started 才越过 decode credit 边界；ACK 本身不释放。
            drop(work.decode_credit.take());
            // Started 只建立 slot 身份；权威 phase 尚未到来前必须保持 unknown。
            work.worker_phase = None;
            reporter
                .worker_started(RuntimeWorkerUpdate {
                    slot,
                    process_id,
                    item_id: item_id.clone(),
                    stage: RuntimeStage::ComputeBaseFeatures,
                    display_path: identity
                        .display_path
                        .as_path()
                        .to_string_lossy()
                        .into_owned(),
                    physical_disk_id: identity.physical_disk_id,
                    completed_files: 0,
                    speed_per_second: 0.0,
                    current_step: "等待 Worker 阶段事件".into(),
                    cache_detail: String::new(),
                    phase: None,
                    cpu_weight: Some(cpu_weight),
                    decoder_threads,
                })
                .await
                .map_err(runtime_error)?;
            reporter
                .record_queue_wait_nowait(
                    RuntimePipelineQueue::Decode,
                    Duration::from_micros(queue_wait_us),
                )
                .map_err(runtime_error)?;
        }
        WorkerEvent::PhaseChanged {
            task_id: event_task,
            item_id,
            slot,
            phase,
            request_elapsed_us,
        } if event_task == task_text => {
            if let Some(work) = active.get_mut(&item_id)
                && work.worker_slot == Some(slot)
            {
                work.worker_phase = Some(phase);
            }
            reporter
                .worker_phase_nowait(
                    slot,
                    &item_id,
                    phase,
                    request_elapsed_us.map(Duration::from_micros),
                )
                .map_err(runtime_error)?;
        }
        WorkerEvent::BaseSourceReadComplete {
            task_id: event_task,
            item_id,
            slot,
            request_elapsed_us,
        } if event_task == task_text => {
            if let Some(work) = active.get_mut(&item_id) {
                work.release_media_permit();
            }
            reporter
                .worker_source_read_complete_nowait(
                    slot,
                    &item_id,
                    request_elapsed_us.map(Duration::from_micros),
                )
                .map_err(runtime_error)?;
        }
        WorkerEvent::Completed {
            task_id: event_task,
            item_id,
            response,
        } if event_task == task_text => {
            if let Some(slot) = active.get(&item_id).and_then(|work| work.worker_slot) {
                reporter
                    .worker_released_nowait(slot, &item_id)
                    .map_err(runtime_error)?;
            }
            queue_base_result(
                task_id,
                &item_id,
                response,
                contact_sheet_root,
                artifact_registry,
                disk_full_cleaner,
                active,
                pending_persist,
                now_ms,
            )?;
        }
        WorkerEvent::Crashed {
            task_id: event_task,
            item_id,
            identity,
            process_id,
            exit_code,
            message,
        } if event_task == task_text => {
            log_worker_crash(
                &event_task,
                &item_id,
                &identity,
                process_id,
                exit_code,
                &message,
            );
            let work = active.remove(&item_id).ok_or_else(|| {
                ScanError::Stage1(format!(
                    "Worker 崩溃事件缺少基础计算活动项: item_id={item_id}"
                ))
            })?;
            let work = work.into_persist_work();
            let display_path = work
                .scanned
                .display_path
                .as_path()
                .to_string_lossy()
                .into_owned();
            let worker_slot = work.worker_slot;
            if let Some(slot) = worker_slot {
                reporter
                    .worker_released_nowait(slot, &item_id)
                    .map_err(runtime_error)?;
            }
            let task_identity = TaskItemIdentity {
                task_id,
                item_id: item_id.clone(),
                content_id: Some(work.content_id),
            };
            let fault = FileFaultRecord {
                machine_id: identity.machine_id,
                normalized_path: identity.normalized_path,
                display_path: identity.display_path,
                file_size: identity.file_size,
                kind: FileFaultKind::WorkerCrash,
                stage: identity.stage,
                windows_error_code: None,
                read_offset: None,
                read_size: None,
                worker_pid: process_id,
                worker_exit_code: exit_code,
                first_seen_at_ms: nonnegative_u64(now_ms),
                last_seen_at_ms: nonnegative_u64(now_ms),
                occurrence_count: 1,
                message: message.clone(),
            };
            let operation_identity = task_identity.clone();
            let operation_message = message.clone();
            pending_persist.push_back(BasePersistMessage::new(task_identity, move |store| {
                let result = store
                    .fail_running_item_with_file_fault_guarded(
                        &operation_identity,
                        &fault,
                        &operation_message,
                        now_ms,
                    )
                    .map_err(|error| error.to_string())?;
                guarded_outcome(
                    &operation_identity,
                    result,
                    BasePersistOutcome::Failed {
                        display_path,
                        message: operation_message,
                        worker_slot,
                        skipped_incomplete: true,
                    },
                )
            }));
        }
        WorkerEvent::Cancelled {
            task_id: event_task,
            item_id,
        } if event_task == task_text => {
            let Some(work) = active.remove(&item_id) else {
                tracing::warn!(item_id, "忽略没有对应活动文件的 Worker 取消结果");
                return Ok(());
            };
            let work = work.into_persist_work();
            let worker_slot = work.worker_slot;
            if let Some(slot) = worker_slot {
                reporter
                    .worker_released_nowait(slot, &item_id)
                    .map_err(runtime_error)?;
            }
            let task_identity = TaskItemIdentity {
                task_id,
                item_id,
                content_id: Some(work.content_id),
            };
            let operation_identity = task_identity.clone();
            pending_persist.push_back(BasePersistMessage::new(task_identity, move |store| {
                let result = store
                    .complete_item_guarded(
                        &operation_identity,
                        TaskItemCompletion::Cancelled,
                        now_ms,
                    )
                    .map_err(|error| error.to_string())?;
                guarded_outcome(
                    &operation_identity,
                    result,
                    BasePersistOutcome::Cancelled { worker_slot },
                )
            }));
        }
        WorkerEvent::InfrastructureFailure { message } => {
            return Err(ScanError::Stage1(message));
        }
        other => {
            tracing::warn!(event = ?other, task_id = %task_text, "忽略基础计算期间收到的其他任务事件");
        }
    }
    Ok(())
}

/// 先移除活动项并释放全部许可，再把纯拥有型结果送入持久化队列。
#[allow(clippy::too_many_arguments)]
fn queue_base_result(
    task_id: TaskId,
    item_id: &str,
    response: proto::WorkerEnvelope,
    contact_sheet_root: &std::path::Path,
    artifact_registry: &Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: &DiskFullCleaner,
    active: &mut BTreeMap<String, ActiveBase>,
    pending_persist: &mut VecDeque<BasePersistMessage>,
    now_ms: i64,
) -> Result<(), ScanError> {
    let Some(work) = active.remove(item_id) else {
        tracing::warn!(item_id, "忽略没有对应活动文件的基础计算结果");
        return Ok(());
    };
    let work = work.into_persist_work();
    let identity = TaskItemIdentity {
        task_id,
        item_id: item_id.to_owned(),
        content_id: Some(work.content_id),
    };
    let operation_identity = identity.clone();
    let contact_sheet_root = contact_sheet_root.to_path_buf();
    let artifact_registry = Arc::clone(artifact_registry);
    let disk_full_cleaner = disk_full_cleaner.clone();
    pending_persist.push_back(BasePersistMessage::new(identity, move |store| {
        persist_base_result(
            store,
            &operation_identity,
            response,
            &contact_sheet_root,
            &artifact_registry,
            &disk_full_cleaner,
            work,
            now_ms,
        )
    }));
    Ok(())
}

/// 在单写 actor 中解析 Worker 终态并提交 guarded 完成或一筛事务。
#[allow(clippy::too_many_arguments)]
fn persist_base_result(
    store: &mut NodeStore,
    identity: &TaskItemIdentity,
    response: proto::WorkerEnvelope,
    contact_sheet_root: &std::path::Path,
    artifact_registry: &Arc<RegenerableArtifactRegistry>,
    disk_full_cleaner: &DiskFullCleaner,
    work: PersistBaseWork,
    now_ms: i64,
) -> Result<BasePersistOutcome, String> {
    let content_id = work.content_id;
    let decision = work.decision;
    match response.payload {
        Some(worker_envelope::Payload::BaseComputeResult(result)) => {
            let returned_md5: [u8; 16] = match result.md5.as_slice().try_into() {
                Ok(md5) if md5 == work.expected_md5 => md5,
                Ok(_) => {
                    return persist_failed_base(
                        store,
                        identity,
                        &work,
                        "Worker 基础结果 MD5 与 Node 内容身份不一致".into(),
                        now_ms,
                    );
                }
                Err(_) => {
                    return persist_failed_base(
                        store,
                        identity,
                        &work,
                        "Worker 基础结果 MD5 长度不是 16 字节".into(),
                        now_ms,
                    );
                }
            };
            debug_assert_eq!(returned_md5, work.expected_md5);
            if decision.missing_parts() != 0 && result.payload.is_empty() {
                return persist_failed_base(
                    store,
                    identity,
                    &work,
                    "Worker 基础结果在仍有缺失字段时返回空 payload".into(),
                    now_ms,
                );
            }
            let output = if result.payload.is_empty() {
                BaseComputeOutput {
                    probe: None,
                    stage1_frames: None,
                    contact_sheet_jpeg: None,
                }
            } else {
                match decode_base_compute_payload(&result.payload) {
                    Ok(output) => output,
                    Err(error) => {
                        return persist_failed_base(
                            store,
                            identity,
                            &work,
                            format!("基础计算结果解析失败: {error}"),
                            now_ms,
                        );
                    }
                }
            };
            if decision.missing_parts() != 0 && output.probe.is_none() {
                return persist_failed_base(
                    store,
                    identity,
                    &work,
                    "Worker 基础结果在仍有缺失字段时未返回 probe".into(),
                    now_ms,
                );
            }
            if decision.missing_parts() == 0 {
                let result = store
                    .complete_item_guarded(
                        identity,
                        TaskItemCompletion::Succeeded {
                            content_id: Some(content_id),
                        },
                        now_ms,
                    )
                    .map_err(|error| error.to_string())?;
                return guarded_outcome(
                    identity,
                    result,
                    BasePersistOutcome::Succeeded {
                        worker_slot: work.worker_slot,
                        cache_hit: false,
                        media_kind: decision.media_kind(),
                        file_size: work.scanned.file_size,
                    },
                );
            } else {
                let cached = store
                    .load_base_cache_record(content_id)
                    .map_err(|error| error.to_string())?;
                let media_kind = output
                    .probe
                    .as_ref()
                    .map_or(cached.media_kind, |probe| probe.media_kind);
                let stage1_output = Stage1Output {
                    media_kind,
                    width: output
                        .probe
                        .as_ref()
                        .map_or(cached.width.unwrap_or_default(), |probe| probe.width),
                    height: output
                        .probe
                        .as_ref()
                        .map_or(cached.height.unwrap_or_default(), |probe| probe.height),
                    duration_ms: output
                        .probe
                        .as_ref()
                        .and_then(|probe| probe.duration_ms)
                        .or(cached.duration_ms),
                    frames: output.stage1_frames.unwrap_or_default(),
                    contact_sheet_jpeg: output.contact_sheet_jpeg,
                };
                let contact =
                    ContactSheetCacheEntry::from_md5(contact_sheet_root, cached.content_key.md5());
                let prepared = match super::engine::prepare_stage1_writes(
                    store,
                    &identity.item_id,
                    &contact,
                    decision.missing_parts(),
                    decision.missing_parts() & BASE_MISSING_CONTACT_SHEET != 0,
                    Some(artifact_registry),
                    Some(disk_full_cleaner),
                    stage1_output,
                ) {
                    Ok(prepared) => prepared,
                    Err(error) => {
                        return persist_failed_base(
                            store,
                            identity,
                            &work,
                            format!("基础特征准备失败: {error}"),
                            now_ms,
                        );
                    }
                };
                let published_contact = match prepared.contact {
                    Some(contact) => match contact.publish() {
                        Ok(contact) => Some(contact),
                        Err(error) => {
                            return persist_failed_base(
                                store,
                                identity,
                                &work,
                                format!("视频缩略图保存失败: {error}"),
                                now_ms,
                            );
                        }
                    },
                    None => None,
                };
                let mut writes = prepared.writes;
                if let Some(contact) = &published_contact {
                    writes.push(contact.feature_write());
                }
                let result = match store.commit_scan_stage1_guarded(
                    identity,
                    prepared.media_kind,
                    writes,
                    now_ms,
                ) {
                    Ok(result) => result,
                    Err(error) => {
                        return match rollback_published_contact(published_contact) {
                            Ok(()) => Err(error.to_string()),
                            Err(cleanup) => Err(format!("{error}; {cleanup}")),
                        };
                    }
                };
                return match result {
                    TaskItemApplyResult::Applied(_) => {
                        if let Some(contact) = published_contact {
                            contact.confirm();
                        }
                        Ok(BasePersistOutcome::Succeeded {
                            worker_slot: work.worker_slot,
                            cache_hit: false,
                            media_kind: prepared.media_kind,
                            file_size: work.scanned.file_size,
                        })
                    }
                    TaskItemApplyResult::IgnoredInactive => {
                        rollback_published_contact(published_contact)?;
                        Ok(BasePersistOutcome::Ignored)
                    }
                    TaskItemApplyResult::IdentityMismatch => {
                        rollback_published_contact(published_contact)?;
                        Err(format!(
                            "基础持久化身份不匹配: task_id={}, item_id={}",
                            identity.task_id.as_uuid(),
                            identity.item_id
                        ))
                    }
                };
            }
        }
        Some(worker_envelope::Payload::WorkerFailure(failure)) => {
            persist_failed_base(store, identity, &work, failure.message, now_ms)
        }
        _ => persist_failed_base(
            store,
            identity,
            &work,
            "Worker 返回了非基础计算响应".into(),
            now_ms,
        ),
    }
}

/// 回滚可选联系表；复用旧 final 时只释放租约，不删除用户已有文件。
fn rollback_published_contact(
    contact: Option<super::engine::PublishedContactSheet>,
) -> Result<(), String> {
    contact
        .map_or(Ok(()), super::engine::PublishedContactSheet::rollback)
        .map_err(|error| error.to_string())
}

/// 在同一 writer 中提交文件失败；ACK 前不改变运行时成功/失败汇总。
fn persist_failed_base(
    store: &mut NodeStore,
    identity: &TaskItemIdentity,
    work: &PersistBaseWork,
    message: String,
    now_ms: i64,
) -> Result<BasePersistOutcome, String> {
    let display_path = work
        .scanned
        .display_path
        .as_path()
        .to_string_lossy()
        .into_owned();
    tracing::error!(item_id = identity.item_id, path = %display_path, error = %message, "基础特征文件处理失败，跳过并继续");
    let result = store
        .complete_item_guarded(
            identity,
            TaskItemCompletion::Failed(message.clone()),
            now_ms,
        )
        .map_err(|error| error.to_string())?;
    guarded_outcome(
        identity,
        result,
        BasePersistOutcome::Failed {
            display_path,
            message,
            worker_slot: work.worker_slot,
            skipped_incomplete: false,
        },
    )
}

/// 把 Store 的活动/身份判定映射为 ACK；身份错配是任务级完整性错误。
fn guarded_outcome(
    identity: &TaskItemIdentity,
    result: TaskItemApplyResult,
    applied: BasePersistOutcome,
) -> Result<BasePersistOutcome, String> {
    match result {
        TaskItemApplyResult::Applied(_) => Ok(applied),
        TaskItemApplyResult::IgnoredInactive => Ok(BasePersistOutcome::Ignored),
        TaskItemApplyResult::IdentityMismatch => Err(format!(
            "基础持久化身份不匹配: task_id={}, item_id={}",
            identity.task_id.as_uuid(),
            identity.item_id
        )),
    }
}

/// 判断一份缓存是否已经具备基础特征；视频联系表由调用方另行判断。
fn cache_fully_computed(cached: Option<&BaseCacheRecord>) -> bool {
    cached.is_some_and(|cached| {
        BaseComputeDecision::for_cache(Some(cached), true, false).missing_parts() == 0
    })
}

/// 比较两份缓存的可复用完整度，中心更完整时才覆盖本地部分缓存。
pub(crate) fn cache_rank(cached: Option<&BaseCacheRecord>) -> u8 {
    cached.map_or(0, |cached| {
        let decision = BaseComputeDecision::for_cache(Some(cached), true, false);
        1 + u8::from(decision.missing_parts() & BASE_MISSING_PROBE == 0)
            + u8::from(decision.missing_parts() & BASE_MISSING_STAGE1 == 0)
    })
}

/// 只把本机缓存记录中与 MD5 派生路径完全一致且可解码的联系表视为命中。
fn contact_sheet_valid_for_record(
    contact_sheet_root: &std::path::Path,
    cached: Option<&BaseCacheRecord>,
) -> bool {
    cached.is_some_and(|record| {
        ContactSheetCacheEntry::from_md5(contact_sheet_root, record.content_key.md5())
            .is_valid(record.contact_sheet_relative_path.as_deref())
    })
}

/// 选择完整度更高的缓存；中心记录没有本机 artifact，按可导入字段作薄适配。
fn selected_contact_sheet_valid(
    contact_sheet_root: &std::path::Path,
    local: Option<&BaseCacheRecord>,
    remote: Option<&BaseCacheRecord>,
) -> bool {
    if remote.is_some_and(|record| cache_rank(Some(record)) > cache_rank(local)) {
        // 远端只提供可导入字段，本机联系表必须由本机路径和文件重新验证。
        return false;
    }
    contact_sheet_valid_for_record(contact_sheet_root, local)
}

/// 计算固定的 2W 解码等待容量；零 Worker 和 usize 溢出都必须显式失败。
fn decode_credit_capacity(worker_capacity: usize) -> Result<usize, ScanError> {
    worker_capacity
        .checked_mul(2)
        .filter(|capacity| *capacity > 0)
        .ok_or_else(|| ScanError::Stage1("decode credit 容量无效或溢出".into()))
}

/// 逐字段比较 dispatch 冻结身份，返回便于日志排查的差异字段名。
fn worker_identity_mismatch(
    expected: &WorkerFileIdentity,
    actual: &WorkerFileIdentity,
) -> Option<String> {
    let mut mismatches = Vec::new();
    if expected.machine_id != actual.machine_id {
        mismatches.push("machine_id");
    }
    if expected.normalized_path != actual.normalized_path {
        mismatches.push("normalized_path");
    }
    if expected.display_path != actual.display_path {
        mismatches.push("display_path");
    }
    if expected.file_size != actual.file_size {
        mismatches.push("file_size");
    }
    if expected.stage != actual.stage {
        mismatches.push("stage");
    }
    if expected.physical_disk_id != actual.physical_disk_id {
        mismatches.push("physical_disk_id");
    }
    (!mismatches.is_empty()).then(|| mismatches.join(","))
}

/// 在不消费上下文、不写 Store 的前提下判断当前 content 是否需要 Worker。
fn content_resolution_need(
    local: Option<&BaseCacheRecord>,
    remote: Option<&BaseCacheRecord>,
    contact_sheet_exists: bool,
    force_recompute: bool,
) -> ContentResolutionNeed {
    let cached = if remote
        .as_ref()
        .is_some_and(|record| cache_rank(Some(record)) > cache_rank(local))
    {
        remote
    } else {
        local
    };
    if BaseComputeDecision::for_cache(cached, contact_sheet_exists, force_recompute).missing_parts()
        == 0
    {
        ContentResolutionNeed::CacheHit
    } else {
        ContentResolutionNeed::WorkerCompute
    }
}

/// 把领域媒体类型映射为 Worker 协议枚举。
const fn proto_media_kind(media_kind: MediaKind) -> proto::MediaKind {
    match media_kind {
        MediaKind::Image => proto::MediaKind::MediaImage,
        MediaKind::Video => proto::MediaKind::MediaVideo,
        MediaKind::Other => proto::MediaKind::MediaOther,
    }
}

/// 在任务结束边界分批发布 outbox；失败只降级，不回滚本地成功结果。
async fn publish_outbox<R: RemoteFeatureCache>(
    store: &BaseStoreHandle,
    remote: &mut R,
    remote_available: &mut bool,
) {
    let machine_id = store.machine_id().clone();
    let mut after = match store.sync_state() {
        Ok(state) => state.acked_seq,
        Err(error) => {
            tracing::warn!(error = %error, "读取 SQLite 同步游标失败");
            return;
        }
    };
    loop {
        let batch = match store.pull_changes(after, REMOTE_LOOKUP_BATCH_SIZE) {
            Ok(batch) => batch,
            Err(error) => {
                tracing::warn!(error = %error, "读取 SQLite outbox 失败");
                return;
            }
        };
        if batch.changes.is_empty() {
            break;
        }
        let protocol_batch = proto::SyncChangeBatch {
            changes: batch.changes,
            high_seq: batch.high_seq,
            pruned_through_seq: batch.pruned_through_seq,
        };
        match remote.publish_outbox(&machine_id, &protocol_batch).await {
            Ok(committed) => {
                if let Err(error) = store.ack_changes(committed) {
                    tracing::warn!(error = %error, "保存 PostgreSQL ACK 失败");
                    return;
                }
                after = committed;
            }
            Err(error) => {
                tracing::warn!(error = %error, "发布 PostgreSQL outbox 失败，保留 SQLite 待下次重试");
                *remote_available = false;
                return;
            }
        }
    }
}

/// 保存基础计算阶段的可恢复进度与实际墙钟时间。
#[allow(clippy::too_many_arguments)]
fn save_base_stage(
    store: &BaseStoreHandle,
    task_id: TaskId,
    stage: RuntimeStage,
    state: PersistentStageState,
    completed: u64,
    total: Option<u64>,
    failed: u64,
    skipped: u64,
    started_at_ms: Option<u64>,
    finished_at_ms: Option<u64>,
    warning_text: Option<String>,
) -> Result<(), ScanError> {
    store.save_task_stage(
        task_id,
        TaskStageWrite {
            stage_id: stage.id().into(),
            state,
            completed,
            total,
            failed,
            skipped,
            started_at_ms,
            finished_at_ms,
            warning_text,
        },
    )?;
    Ok(())
}

/// 返回持久阶段使用的当前墙钟毫秒。
fn wall_clock_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .try_into()
        .unwrap_or(u64::MAX)
}

/// 记录 Node 观察到的 Worker 崩溃及完整文件上下文，供现场日志直接定位文件。
fn log_worker_crash(
    task_id: &str,
    item_id: &str,
    identity: &WorkerFileIdentity,
    worker_pid: Option<u32>,
    worker_exit_code: Option<i32>,
    message: &str,
) {
    tracing::error!(
        task_id = %task_id,
        item_id = %item_id,
        file_path = %identity.display_path.as_path().display(),
        normalized_path = %identity.normalized_path.as_str(),
        crash_stage = %identity.stage,
        worker_pid = ?worker_pid,
        worker_exit_code = ?worker_exit_code,
        error = %message,
        "Worker 计算文件时崩溃，当前文件已标记失败并继续任务"
    );
}

/// 把运行时 registry 错误转换为扫描任务基础设施错误。
fn runtime_error(error: impl ToString) -> ScanError {
    ScanError::Stage1(error.to_string())
}

/// 把产品硬上限饱和投影为协议 u32，避免平台 usize 泄漏到 wire。
fn runtime_u32(value: usize) -> u32 {
    value.try_into().unwrap_or(u32::MAX)
}

/// 把平台 usize 饱和投影为协议 u64，禁止窄平台转换产生回绕。
fn runtime_u64(value: usize) -> u64 {
    value.try_into().unwrap_or(u64::MAX)
}

/// 把持久化时间戳投影为非负 u64；异常负值按零处理而不回绕。
fn nonnegative_u64(value: i64) -> u64 {
    value.try_into().unwrap_or(0)
}

/// 从协调器真实 ownership 投影当前五段队列；Worker 资源由 Started/terminal 身份事件维护。
fn update_pipeline_ownership(
    reporter: &RuntimeTaskReporter,
    hash: usize,
    hash_phases: &HashPhaseTracker,
    hash_capacity: usize,
    output_credits: &ContentOutputCredits,
    hash_refill: &HashRefillController,
    decode_credits: &DecodeCredits,
    path_cache: usize,
    content_cache: usize,
    decode: usize,
    persist: usize,
    media_phases: &MediaAcquirePhaseTracker,
    media_acquiring_len: usize,
    worker_capacity: usize,
    active: &BTreeMap<String, ActiveBase>,
    worker_dispatching_item: &Option<String>,
) -> Result<(), ScanError> {
    let decode_credit_owned = decode_credits.owned();
    if decode_credit_owned > decode_credits.capacity() {
        return Err(ScanError::Stage1(format!(
            "decode credit ownership 超过固定容量: owned={decode_credit_owned}, limit={}",
            decode_credits.capacity()
        )));
    }
    if decode_credit_owned != decode {
        return Err(ScanError::Stage1(format!(
            "decode credit ownership 守恒违例: credit={decode_credit_owned}, states={decode}"
        )));
    }
    for (queue, current) in [
        (RuntimePipelineQueue::Hash, hash),
        (RuntimePipelineQueue::PathCache, path_cache),
        (RuntimePipelineQueue::ContentCache, content_cache),
        (RuntimePipelineQueue::Decode, decode),
        (RuntimePipelineQueue::Persist, persist),
    ] {
        reporter
            .update_queue_nowait(queue, current)
            .map_err(runtime_error)?;
    }

    // output credit 是独立 RAII ownership，refill token 是不参与 ownership 求和的控制状态。
    reporter
        .update_ownership_nowait(
            RuntimePipelineOwnership::ContentOutputCreditOwned,
            runtime_u64(output_credits.owned()),
            runtime_u64(output_credits.capacity()),
        )
        .map_err(runtime_error)?;
    reporter
        .update_control_state_nowait(
            RuntimePipelineControl::HashRefillTokenAvailable,
            runtime_u64(hash_refill.available()),
            runtime_u64(hash_refill.capacity()),
        )
        .map_err(runtime_error)?;
    reporter
        .update_ownership_nowait(
            RuntimePipelineOwnership::DecodeCreditOwned,
            runtime_u64(decode_credit_owned),
            runtime_u64(decode_credits.capacity()),
        )
        .map_err(runtime_error)?;

    let hash_snapshot = hash_phases.snapshot();
    if hash_snapshot.total() != hash {
        return Err(ScanError::Stage1(format!(
            "Hash phase ownership 守恒违例: join_set={hash}, phases={}",
            hash_snapshot.total()
        )));
    }
    let media_snapshot = media_phases.snapshot();
    // JoinSet 仍持有全部 future；ready 是其中已完成但尚未 join 的子集。
    if media_snapshot.total() != media_acquiring_len {
        return Err(ScanError::Stage1(format!(
            "Media phase ownership 守恒违例: join_set={media_acquiring_len}, phases={}",
            media_snapshot.total()
        )));
    }
    if media_snapshot.total() < media_snapshot.ready {
        return Err(ScanError::Stage1(
            "Media phase ownership 守恒违例: ready 超过 JoinSet".into(),
        ));
    }
    if media_snapshot.permit_ready > media_snapshot.ready {
        return Err(ScanError::Stage1(
            "Media phase ownership 守恒违例: permit-ready 超过 ready".into(),
        ));
    }
    let mut worker_start_pending = 0_usize;
    let mut worker_decode = 0_usize;
    let mut worker_feature = 0_usize;
    let mut worker_result_wait = 0_usize;
    let mut worker_phase_unknown = 0_usize;
    for work in active.values() {
        if work.worker_slot.is_none() {
            worker_start_pending += 1;
            continue;
        }
        match work.worker_phase {
            Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode) => worker_decode += 1,
            Some(proto::RuntimeWorkerPhase::RuntimeWorkerFeature) => worker_feature += 1,
            Some(proto::RuntimeWorkerPhase::RuntimeWorkerResultWait) => worker_result_wait += 1,
            Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle)
            | Some(proto::RuntimeWorkerPhase::Unspecified)
            | None => worker_phase_unknown += 1,
        }
    }
    let active_phases = worker_start_pending
        .checked_add(worker_decode)
        .and_then(|value| value.checked_add(worker_feature))
        .and_then(|value| value.checked_add(worker_result_wait))
        .and_then(|value| value.checked_add(worker_phase_unknown))
        .ok_or_else(|| ScanError::Stage1("Worker phase ownership 计数溢出".into()))?;
    if active_phases != active.len()
        || worker_start_pending
            != active
                .values()
                .filter(|work| work.worker_slot.is_none())
                .count()
    {
        return Err(ScanError::Stage1(format!(
            "Worker phase ownership 守恒违例: active={}, phases={active_phases}",
            active.len()
        )));
    }
    let worker_dispatching = usize::from(worker_dispatching_item.is_some());
    let worker_admission = active
        .len()
        .checked_add(media_snapshot.total())
        .and_then(|value| value.checked_add(worker_dispatching))
        .ok_or_else(|| ScanError::Stage1("Worker admission ownership 计数溢出".into()))?;
    if worker_admission > worker_capacity {
        return Err(ScanError::Stage1(format!(
            "Worker admission ownership 守恒违例: owned={worker_admission}, limit={worker_capacity}"
        )));
    }
    let hash_capacity = u64::from(runtime_u32(hash_capacity));
    let media_capacity = u64::from(runtime_u32(worker_capacity));
    let worker_capacity = media_capacity;
    for (kind, current, capacity) in [
        (
            RuntimePipelineOwnership::HashWaitingPermit,
            hash_snapshot.waiting_permit,
            hash_capacity,
        ),
        (
            RuntimePipelineOwnership::HashReading,
            hash_snapshot.reading,
            hash_capacity,
        ),
        (
            RuntimePipelineOwnership::HashCompletedUnjoined,
            hash_snapshot.completed_unjoined,
            hash_capacity,
        ),
        (
            RuntimePipelineOwnership::MediaPermitWaiting,
            media_snapshot.waiting,
            worker_capacity,
        ),
        (
            RuntimePipelineOwnership::MediaAcquireReady,
            media_snapshot.ready,
            worker_capacity,
        ),
        (
            RuntimePipelineOwnership::MediaPermitReady,
            media_snapshot.permit_ready,
            worker_capacity,
        ),
        (
            RuntimePipelineOwnership::WorkerDispatching,
            worker_dispatching,
            worker_capacity,
        ),
        (
            RuntimePipelineOwnership::WorkerStartPending,
            worker_start_pending,
            worker_capacity,
        ),
        (
            RuntimePipelineOwnership::WorkerDecode,
            worker_decode,
            worker_capacity,
        ),
        (
            RuntimePipelineOwnership::WorkerFeature,
            worker_feature,
            worker_capacity,
        ),
        (
            RuntimePipelineOwnership::WorkerResultWait,
            worker_result_wait,
            worker_capacity,
        ),
        (
            RuntimePipelineOwnership::WorkerPhaseUnknown,
            worker_phase_unknown,
            worker_capacity,
        ),
    ] {
        reporter
            .update_ownership_nowait(kind, runtime_u64(current), capacity)
            .map_err(runtime_error)?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::{
        collections::{BTreeMap, BTreeSet, VecDeque},
        io::{self, Write},
        sync::{Arc, Mutex},
        time::{Duration, Instant},
    };

    use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath, TaskId};
    use dedup_node_store::NodeStore;
    use tracing_subscriber::fmt::MakeWriter;

    use crate::{
        artifact_registry::RegenerableArtifactRegistry,
        disk_full_cleanup::{DiskFullCleaner, SystemArtifactDiskResolver},
        runtime_tasks::{
            RuntimeExecutionConfigUpdate, RuntimeProgressUnit, RuntimeStage, RuntimeTaskKind,
            RuntimeTaskRegistry,
        },
    };

    use super::{
        ActiveBase, BaseComputeDecision, BasePersistAck, BasePersistIdentity, BasePersistOutcome,
        ContentOutputCredits, ContentResolutionNeed, HashPhaseTracker, HashRefillController,
        MediaAcquirePhaseTracker, ScanError, ScanSummary, TaskItemIdentity, WorkerEvent,
        WorkerFileIdentity, apply_persist_ack, content_resolution_need, decode_credit_capacity,
        ensure_cache_wait_holds_no_compute_resource, handle_worker_event, log_worker_crash,
        update_pipeline_ownership,
    };

    /// 运行时解码等待边界固定为 Worker 数的两倍，零值与溢出不得静默下溢。
    #[test]
    fn decode_credit_capacity_is_exactly_twice_worker_count() {
        assert_eq!(decode_credit_capacity(12).unwrap(), 24);
        assert!(decode_credit_capacity(0).is_err());
        assert!(decode_credit_capacity(usize::MAX).is_err());
    }

    /// 规划阶段只读判断 Worker 需求；完整缓存命中不应消耗 decode credit。
    #[test]
    fn content_resolution_need_does_not_consume_credit_for_cache_hit() {
        let cached = dedup_node_store::BaseCacheRecord {
            content_id: None,
            content_key: dedup_core::ContentKey::new([7; 16], 100),
            media_kind: MediaKind::Other,
            base_complete: true,
            width: None,
            height: None,
            duration_ms: None,
            stage1: None,
            image_stage2: None,
            video_stage2: Box::new([None; 6]),
            contact_sheet_relative_path: None,
        };
        assert_eq!(
            content_resolution_need(Some(&cached), None, true, false),
            ContentResolutionNeed::CacheHit
        );
        assert_eq!(
            content_resolution_need(None, None, true, false),
            ContentResolutionNeed::WorkerCompute
        );
    }

    /// 同 task/item 但冻结文件身份不一致的 Started 不得释放 admission 或 decode credit。
    #[tokio::test]
    async fn started_identity_mismatch_keeps_admission_until_exact_match() {
        let registry = RuntimeTaskRegistry::new();
        let machine = MachineId::from_sha256([0xA1; 32]);
        let task_id = TaskId::new();
        let reporter = registry
            .begin(
                RuntimeTaskKind::BaseCompute,
                machine.clone(),
                "Started 身份冻结",
            )
            .await;
        reporter
            .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
                hash_tasks: 1,
                path_cache_queue_capacity: 1,
                content_cache_queue_capacity: 1,
                decode_queue_capacity: 2,
                persist_queue_capacity: 1,
                worker_slots: 1,
                cpu_budget: 1,
                global_disk_permits: 1,
                hdd_per_disk_permits: 1,
                ssd_per_disk_permits: 1,
                unknown_per_disk_permits: 1,
            })
            .unwrap();
        let directory = tempfile::tempdir().unwrap();
        let cache_root = directory.path().join("cache");
        std::fs::create_dir_all(&cache_root).unwrap();
        let artifacts =
            Arc::new(RegenerableArtifactRegistry::new(directory.path(), &cache_root).unwrap());
        let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
        let path = directory.path().join("frozen.bin");
        let scanned = dedup_node_store::ScannedPath::new(
            NormalizedPath::new(&path).unwrap(),
            DisplayPath::new(&path).unwrap(),
            1024,
        );
        let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let content_id = store
            .upsert_content_and_location(&scanned, [0xA2; 16], MediaKind::Other)
            .unwrap()
            .id;
        let expected_identity = WorkerFileIdentity {
            machine_id: machine.clone(),
            normalized_path: scanned.normalized_path.clone(),
            display_path: scanned.display_path.clone(),
            file_size: scanned.file_size,
            stage: "base_compute".into(),
            physical_disk_id: "disk-frozen".into(),
        };
        let decode_credits = super::DecodeCredits::new(2);
        let mut active = BTreeMap::from([(
            "frozen-item".to_owned(),
            ActiveBase {
                scanned,
                content_id,
                decision: BaseComputeDecision::for_cache(None, false, false),
                expected_md5: [0xA2; 16],
                media_permit: None,
                worker_slot: None,
                decode_credit: decode_credits.try_acquire(),
                worker_identity: expected_identity.clone(),
                worker_phase: None,
            },
        )]);
        let mut pending_persist = VecDeque::new();
        let output_credits = ContentOutputCredits::new(1);
        let hash_refill = HashRefillController::new(1);
        let hash_phases = HashPhaseTracker::new();
        let media_phases = MediaAcquirePhaseTracker::new();
        let publish = |active: &BTreeMap<String, ActiveBase>, decode: usize| {
            update_pipeline_ownership(
                &reporter,
                0,
                &hash_phases,
                1,
                &output_credits,
                &hash_refill,
                &decode_credits,
                0,
                0,
                decode,
                0,
                &media_phases,
                0,
                1,
                active,
                &None,
            )
        };
        publish(&active, 1).unwrap();

        let mut wrong_identity = expected_identity.clone();
        wrong_identity.file_size += 1;
        handle_worker_event(
            WorkerEvent::Started {
                task_id: task_id.as_uuid().to_string(),
                item_id: "frozen-item".into(),
                slot: 7,
                process_id: Some(7007),
                identity: wrong_identity,
                cpu_weight: 1,
                decoder_threads: Some(1),
                queue_wait_us: 1,
            },
            task_id,
            directory.path(),
            &reporter,
            &artifacts,
            &cleaner,
            &mut active,
            &mut pending_persist,
            0,
        )
        .await
        .unwrap();
        publish(&active, 1).unwrap();

        let details = registry.details(reporter.id()).await.unwrap();
        let metrics = details.pipeline_metrics.as_ref().unwrap();
        assert_eq!(active["frozen-item"].worker_slot, None);
        assert_eq!(decode_credits.owned(), 1);
        assert!(details.workers.is_empty());
        assert_eq!(
            metrics.worker_start_pending.as_ref().unwrap().current,
            Some(1)
        );
        assert_eq!(
            metrics.decode_credit_owned.as_ref().unwrap().current,
            Some(1)
        );

        handle_worker_event(
            WorkerEvent::Started {
                task_id: task_id.as_uuid().to_string(),
                item_id: "frozen-item".into(),
                slot: 7,
                process_id: Some(7007),
                identity: expected_identity,
                cpu_weight: 1,
                decoder_threads: Some(1),
                queue_wait_us: 1,
            },
            task_id,
            directory.path(),
            &reporter,
            &artifacts,
            &cleaner,
            &mut active,
            &mut pending_persist,
            0,
        )
        .await
        .unwrap();
        publish(&active, 0).unwrap();

        let details = registry.details(reporter.id()).await.unwrap();
        let metrics = details.pipeline_metrics.unwrap();
        assert_eq!(active["frozen-item"].worker_slot, Some(7));
        assert_eq!(decode_credits.owned(), 0);
        assert_eq!(details.workers.len(), 1);
        assert_eq!(
            metrics.worker_start_pending.as_ref().unwrap().current,
            Some(0)
        );
        assert_eq!(
            metrics.decode_credit_owned.as_ref().unwrap().current,
            Some(0)
        );
    }

    /// 保存单个测试 subscriber 输出的共享字节缓冲区。
    #[derive(Clone, Default)]
    struct SharedLogBuffer(Arc<Mutex<Vec<u8>>>);

    impl SharedLogBuffer {
        /// 返回当前捕获的 UTF-8 日志文本。
        fn text(&self) -> String {
            String::from_utf8(self.0.lock().unwrap().clone()).unwrap()
        }
    }

    /// 把 tracing 格式化输出追加到共享测试缓冲区。
    struct SharedLogWriter(SharedLogBuffer);

    impl Write for SharedLogWriter {
        fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
            self.0.0.lock().unwrap().extend_from_slice(buffer);
            Ok(buffer.len())
        }

        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    impl<'a> MakeWriter<'a> for SharedLogBuffer {
        type Writer = SharedLogWriter;

        fn make_writer(&'a self) -> Self::Writer {
            SharedLogWriter(self.clone())
        }
    }

    #[test]
    fn worker_crash_log_contains_full_path_and_process_context() {
        let output = SharedLogBuffer::default();
        let subscriber = tracing_subscriber::fmt()
            .without_time()
            .with_ansi(false)
            .with_target(false)
            .with_writer(output.clone())
            .finish();
        let identity = WorkerFileIdentity {
            machine_id: MachineId::from_sha256([0x74; 32]),
            normalized_path: NormalizedPath::new(r"I:\媒体库\歌手 A\现场\崩溃样本.mp4").unwrap(),
            display_path: DisplayPath::new(r"I:\媒体库\歌手 A\现场\崩溃样本.mp4").unwrap(),
            file_size: 4096,
            stage: "base_compute".into(),
            physical_disk_id: "disk-log".into(),
        };

        tracing::subscriber::with_default(subscriber, || {
            log_worker_crash(
                "task-log",
                "item-log",
                &identity,
                Some(10528),
                Some(0xc000_0374_u32 as i32),
                "Worker 管道断开",
            );
        });

        let log = output.text();
        assert!(log.contains(r"I:\媒体库\歌手 A\现场\崩溃样本.mp4"));
        assert!(log.contains("task_id=task-log"));
        assert!(log.contains("item_id=item-log"));
        assert!(log.contains("crash_stage=base_compute"));
        assert!(log.contains("worker_pid=Some(10528)"));
        assert!(log.contains("worker_exit_code=Some(-1073740940)"));
    }

    /// 孤儿 Started 不得登记 Worker slot/CPU，随后重复终态也必须保持无泄漏。
    #[tokio::test]
    async fn orphan_started_does_not_leak_runtime_worker_resources() {
        let registry = RuntimeTaskRegistry::new();
        let task_id = TaskId::new();
        let reporter = registry
            .begin(
                RuntimeTaskKind::BaseCompute,
                MachineId::from_sha256([0x91; 32]),
                "孤儿 Started 回归",
            )
            .await;
        reporter
            .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
                hash_tasks: 1,
                path_cache_queue_capacity: 1,
                content_cache_queue_capacity: 1,
                decode_queue_capacity: 1,
                persist_queue_capacity: 1,
                worker_slots: 1,
                cpu_budget: 3,
                global_disk_permits: 1,
                hdd_per_disk_permits: 1,
                ssd_per_disk_permits: 1,
                unknown_per_disk_permits: 1,
            })
            .unwrap();

        let directory = tempfile::tempdir().unwrap();
        let cache_root = directory.path().join("cache");
        std::fs::create_dir_all(&cache_root).unwrap();
        let artifacts =
            Arc::new(RegenerableArtifactRegistry::new(directory.path(), &cache_root).unwrap());
        let cleaner = DiskFullCleaner::new(Arc::clone(&artifacts), SystemArtifactDiskResolver);
        let identity = WorkerFileIdentity {
            machine_id: MachineId::from_sha256([0x92; 32]),
            normalized_path: NormalizedPath::new(r"I:\孤儿\sample.mp4").unwrap(),
            display_path: DisplayPath::new(r"I:\孤儿\sample.mp4").unwrap(),
            file_size: 1024,
            stage: "base_compute".into(),
            physical_disk_id: "disk-orphan".into(),
        };
        let event_task = task_id.as_uuid().to_string();
        let mut active = BTreeMap::new();
        let mut pending_persist = VecDeque::new();

        handle_worker_event(
            WorkerEvent::Started {
                task_id: event_task.clone(),
                item_id: "orphan-item".into(),
                slot: 0,
                process_id: Some(4321),
                identity,
                cpu_weight: 3,
                decoder_threads: Some(2),
                queue_wait_us: 17,
            },
            task_id,
            directory.path(),
            &reporter,
            &artifacts,
            &cleaner,
            &mut active,
            &mut pending_persist,
            0,
        )
        .await
        .unwrap();

        let details = registry.details(reporter.id()).await.unwrap();
        let metrics = details.pipeline_metrics.as_ref().unwrap();
        assert_eq!(
            details.workers.len(),
            0,
            "孤儿 Started 不得留下 Worker slot"
        );
        assert_eq!(
            metrics.worker_slots.as_ref().unwrap().current,
            Some(0),
            "孤儿 Started 不得占用 Worker slot"
        );
        assert_eq!(
            metrics.cpu_weight.as_ref().unwrap().current,
            Some(0),
            "孤儿 Started 不得占用 CPU weight"
        );

        handle_worker_event(
            WorkerEvent::Cancelled {
                task_id: event_task.clone(),
                item_id: "orphan-item".into(),
            },
            task_id,
            directory.path(),
            &reporter,
            &artifacts,
            &cleaner,
            &mut active,
            &mut pending_persist,
            0,
        )
        .await
        .unwrap();
        let details_after_terminal = registry.details(reporter.id()).await.unwrap();
        assert!(details_after_terminal.workers.is_empty());
        assert_eq!(
            details_after_terminal
                .pipeline_metrics
                .unwrap()
                .worker_slots
                .unwrap()
                .current,
            Some(0)
        );
    }

    /// Applied success/failure/cancel 都计时，Ignored ACK 只清理 map 不产生样本。
    #[tokio::test]
    async fn item_latency_records_every_applied_terminal_but_ignores_inactive_ack() {
        let registry = RuntimeTaskRegistry::new();
        let reporter = registry
            .begin(
                RuntimeTaskKind::BaseCompute,
                MachineId::from_sha256([0x93; 32]),
                "Applied latency 终态",
            )
            .await;
        reporter
            .configure_pipeline_nowait(RuntimeExecutionConfigUpdate {
                hash_tasks: 1,
                path_cache_queue_capacity: 1,
                content_cache_queue_capacity: 1,
                decode_queue_capacity: 1,
                persist_queue_capacity: 1,
                worker_slots: 1,
                cpu_budget: 1,
                global_disk_permits: 1,
                hdd_per_disk_permits: 1,
                ssd_per_disk_permits: 1,
                unknown_per_disk_permits: 1,
            })
            .unwrap();
        reporter
            .start_stage_nowait(
                RuntimeStage::ComputeBaseFeatures,
                RuntimeProgressUnit::Files,
            )
            .unwrap();

        let task_id = TaskId::new();
        let mut summary = ScanSummary {
            task_id,
            total_files: 4,
            total_bytes: 0,
            cache_hits: 0,
            hashed: 0,
            reused_contents: 0,
            scheduled_stage1: 0,
            skipped_incomplete: 0,
            file_failures: 0,
            outbox_high_seq: 0,
        };
        let mut in_flight = 4;
        let mut item_started_at = BTreeMap::new();
        let outcomes = [
            (
                "applied-success",
                BasePersistOutcome::Succeeded {
                    worker_slot: None,
                    cache_hit: false,
                    media_kind: MediaKind::Other,
                    file_size: 10,
                },
            ),
            (
                "applied-failure",
                BasePersistOutcome::Failed {
                    display_path: "failure.bin".into(),
                    message: "测试失败".into(),
                    worker_slot: None,
                    skipped_incomplete: false,
                },
            ),
            (
                "applied-cancel",
                BasePersistOutcome::Cancelled { worker_slot: None },
            ),
            ("inactive-ignored", BasePersistOutcome::Ignored),
        ];
        for (item_id, outcome) in outcomes {
            item_started_at.insert(item_id.to_owned(), Instant::now());
            apply_persist_ack(
                BasePersistAck {
                    identity: BasePersistIdentity::Legacy(TaskItemIdentity {
                        task_id,
                        item_id: item_id.to_owned(),
                        content_id: None,
                    }),
                    queue_wait: Duration::ZERO,
                    transaction_elapsed: Duration::ZERO,
                    result: Ok(outcome),
                },
                &mut in_flight,
                &reporter,
                &mut summary,
                &mut item_started_at,
            )
            .await
            .unwrap();
        }

        assert!(
            item_started_at.is_empty(),
            "四种 Applied/ignored ACK 都必须清理 map"
        );
        let metrics = registry
            .details(reporter.id())
            .await
            .unwrap()
            .pipeline_metrics
            .unwrap();
        assert_eq!(
            metrics
                .item_completion_latency
                .expect("三个 Applied ACK 必须有 latency histogram")
                .count,
            3
        );
        assert_eq!(summary.file_failures, 1);
    }

    /// 缓存等待必须按冻结 item_id 精确判定资源所有权并返回稳定错误码。
    #[test]
    fn cache_wait_resource_ownership_uses_exact_item_identity() {
        let hash_item_ids = BTreeSet::from(["hash-item".to_owned()]);
        let media_item_ids = BTreeSet::from(["media-item".to_owned()]);
        let active = BTreeMap::new();

        let error = ensure_cache_wait_holds_no_compute_resource(
            "hash-item",
            &hash_item_ids,
            &media_item_ids,
            &active,
            None,
        )
        .expect_err("持有 Hash 资源的具体 item 不得进入 cache wait");
        assert!(matches!(
            error,
            ScanError::Stage1(message)
                if message == "CACHE_WAIT_RESOURCE_OWNERSHIP_VIOLATION: item_id=hash-item"
        ));
        assert!(
            ensure_cache_wait_holds_no_compute_resource(
                "different-item",
                &hash_item_ids,
                &media_item_ids,
                &active,
                None,
            )
            .is_ok()
        );
    }
}

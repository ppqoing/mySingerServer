//! Node 进程生命周期内的运行任务阶段、Worker、速度与最近失败快照。

use std::{
    collections::{BTreeMap, VecDeque},
    sync::{Arc, RwLock},
    time::{Duration, Instant},
};

use dedup_core::{MachineId, MediaKind};
use dedup_protocol::{MAX_RUNTIME_FAILURES, proto};
use thiserror::Error;
use tokio::sync::broadcast;
use uuid::Uuid;

const SPEED_WINDOW: Duration = Duration::from_secs(10);
/// 运行中进度对管理端合并发布的固定间隔。
const PROGRESS_PUBLISH_INTERVAL: Duration = Duration::from_secs(2);
/// 运行时延迟直方图的固定有限上界；第十六桶表示正无穷。
const LATENCY_UPPER_BOUNDS_MS: [u64; 15] = [
    1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1_000, 2_000, 5_000, 10_000, 30_000,
];
/// 小文件大小桶上界，不包含 16 MiB。
const SMALL_MEDIA_BYTES: u64 = 16 * 1024 * 1024;
/// 中文件大小桶上界，不包含 256 MiB。
const MEDIUM_MEDIA_BYTES: u64 = 256 * 1024 * 1024;

/// 可注入的进程内单调时钟。
pub trait RuntimeTaskClock: Send + Sync + 'static {
    /// 返回自任意固定原点起的单调时长。
    fn now(&self) -> Duration;
}

#[derive(Debug)]
struct SystemClock(Instant);

impl RuntimeTaskClock for SystemClock {
    fn now(&self) -> Duration {
        self.0.elapsed()
    }
}

/// Node 运行任务类别。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RuntimeTaskKind {
    /// 枚举、缓存查询和 Worker 基础特征计算。
    BaseCompute,
    /// 扫描与一筛。
    Scan,
    /// 单机重复文件清单生成。
    LocalAnalysis,
    /// Node 持久二次特征计算。
    Stage2Compute,
    /// 二筛计算。
    Stage2,
    /// 删除。
    Delete,
}

impl RuntimeTaskKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::BaseCompute => "base_compute",
            Self::Scan => "scan",
            Self::LocalAnalysis => "duplicate_list",
            Self::Stage2Compute => "stage2_compute",
            Self::Stage2 => "stage2",
            Self::Delete => "delete",
        }
    }
}

/// 固定英文 ID 与中文显示名的运行阶段。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimeStage {
    /// 基础任务完整枚举文件清单。
    EnumerateFiles,
    /// 基础任务查询 SQLite 和可选 PostgreSQL 缓存。
    LookupBaseCache,
    /// Worker 完成 MD5、缩略图和一筛基础特征。
    ComputeBaseFeatures,
    /// 二次任务查询 SQLite 和可选 PostgreSQL 缓存。
    LookupStage2Cache,
    /// Worker 计算缺失的图片或视频二次特征。
    ComputeStage2Features,
    /// 准备。
    Prepare,
    /// 枚举。
    Enumerate,
    /// 缓存查询。
    CacheLookup,
    /// 读取与 MD5。
    ReadMd5,
    /// 媒体探测与一筛。
    ProbeStage1,
    /// 持久化与收尾。
    PersistFinalize,
    /// 验证选择。
    ValidateSelection,
    /// 删除文件。
    DeleteFiles,
    /// 收缩数据库。
    ShrinkDatabase,
    /// 发布结果。
    PublishResults,
    /// 从冻结输入生成重复候选。
    BuildCandidates,
    /// 查询、复用并派发候选所需的二次特征。
    DispatchStage2,
    /// 使用完整二次特征精准判重并保存清单。
    FinalCompare,
    /// 冻结分析输入。
    FreezeInputs,
    /// 加载特征。
    LoadFeatures,
    /// 生成一筛候选。
    Stage1Candidates,
    /// 补齐二筛。
    FillStage2,
    /// 聚类。
    Cluster,
    /// 保存结果。
    SaveResults,
    /// 重新验证删除选择。
    RevalidateSelection,
    /// 派发删除节点。
    DispatchNodes,
    /// 删除项目。
    DeleteItems,
    /// 汇总删除。
    Summarize,
}

impl RuntimeStage {
    /// 稳定英文阶段 ID。
    pub const fn id(self) -> &'static str {
        match self {
            Self::EnumerateFiles => "enumerate_files",
            Self::LookupBaseCache => "lookup_base_cache",
            Self::ComputeBaseFeatures => "compute_base_features",
            Self::LookupStage2Cache => "lookup_stage2_cache",
            Self::ComputeStage2Features => "compute_stage2_features",
            Self::Prepare => "prepare",
            Self::Enumerate => "enumerate",
            Self::CacheLookup => "cache_lookup",
            Self::ReadMd5 => "read_md5",
            Self::ProbeStage1 => "probe_stage1",
            Self::PersistFinalize => "persist_finalize",
            Self::ValidateSelection => "validate_selection",
            Self::DeleteFiles => "delete_files",
            Self::ShrinkDatabase => "shrink_database",
            Self::PublishResults => "publish_results",
            Self::BuildCandidates => "build_candidates",
            Self::DispatchStage2 => "dispatch_stage2",
            Self::FinalCompare => "final_compare",
            Self::FreezeInputs => "freeze_inputs",
            Self::LoadFeatures => "load_features",
            Self::Stage1Candidates => "stage1_candidates",
            Self::FillStage2 => "fill_stage2",
            Self::Cluster => "cluster",
            Self::SaveResults => "save_results",
            Self::RevalidateSelection => "revalidate_selection",
            Self::DispatchNodes => "dispatch_nodes",
            Self::DeleteItems => "delete_items",
            Self::Summarize => "summarize",
        }
    }

    /// 固定中文阶段名。
    pub const fn display_name(self) -> &'static str {
        match self {
            Self::EnumerateFiles => "枚举文件",
            Self::LookupBaseCache => "查询基础缓存",
            Self::ComputeBaseFeatures => "计算基础特征",
            Self::LookupStage2Cache => "查询二次特征缓存",
            Self::ComputeStage2Features => "计算二次特征",
            Self::Prepare => "准备",
            Self::Enumerate => "枚举文件",
            Self::CacheLookup => "缓存查询",
            Self::ReadMd5 => "读取与 MD5",
            Self::ProbeStage1 => "媒体探测与一筛",
            Self::PersistFinalize => "持久化与收尾",
            Self::ValidateSelection => "验证删除选择",
            Self::DeleteFiles => "删除文件",
            Self::ShrinkDatabase => "收缩数据库",
            Self::PublishResults => "发布结果",
            Self::BuildCandidates => "生成候选",
            Self::DispatchStage2 => "派发二次特征",
            Self::FinalCompare => "精准判重",
            Self::FreezeInputs => "冻结输入",
            Self::LoadFeatures => "加载特征",
            Self::Stage1Candidates => "一筛候选",
            Self::FillStage2 => "补齐二筛",
            Self::Cluster => "聚类",
            Self::SaveResults => "保存结果",
            Self::RevalidateSelection => "重新验证选择",
            Self::DispatchNodes => "派发节点",
            Self::DeleteItems => "删除项目",
            Self::Summarize => "汇总",
        }
    }

    /// 从稳定阶段 ID 解析当前进程可显示的运行阶段。
    pub fn from_id(value: &str) -> Option<Self> {
        [
            Self::EnumerateFiles,
            Self::LookupBaseCache,
            Self::ComputeBaseFeatures,
            Self::LookupStage2Cache,
            Self::ComputeStage2Features,
            Self::Prepare,
            Self::Enumerate,
            Self::CacheLookup,
            Self::ReadMd5,
            Self::ProbeStage1,
            Self::PersistFinalize,
            Self::ValidateSelection,
            Self::DeleteFiles,
            Self::ShrinkDatabase,
            Self::PublishResults,
            Self::BuildCandidates,
            Self::DispatchStage2,
            Self::FinalCompare,
            Self::FreezeInputs,
            Self::LoadFeatures,
            Self::Stage1Candidates,
            Self::FillStage2,
            Self::Cluster,
            Self::SaveResults,
            Self::RevalidateSelection,
            Self::DispatchNodes,
            Self::DeleteItems,
            Self::Summarize,
        ]
        .into_iter()
        .find(|stage| stage.id() == value)
    }
}

/// 阶段计数单位。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RuntimeProgressUnit {
    /// 文件数。
    Files,
    /// 字节数。
    Bytes,
    /// 普通项目数。
    Items,
    /// 候选对。
    CandidatePairs,
    /// 删除项。
    DeleteItems,
}

impl RuntimeProgressUnit {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Files => "files",
            Self::Bytes => "bytes",
            Self::Items => "items",
            Self::CandidatePairs => "candidate_pairs",
            Self::DeleteItems => "delete_items",
        }
    }
}

/// 整体运行状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RuntimeTaskState {
    /// 正在运行。
    Running,
    /// 成功完成。
    Completed,
    /// 失败结束。
    Failed,
    /// 取消结束。
    Cancelled,
}

impl RuntimeTaskState {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Running => "running",
            Self::Completed => "completed",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }

    const fn is_terminal(self) -> bool {
        !matches!(self, Self::Running)
    }
}

/// 一次阶段进度更新。
#[derive(Clone, Debug)]
pub struct RuntimeStageUpdate {
    /// 固定阶段。
    pub stage: RuntimeStage,
    /// 新阶段状态。
    pub state: proto::RuntimeStageState,
    /// 计数单位。
    pub unit: RuntimeProgressUnit,
    /// 已完成数。
    pub completed: u64,
    /// `None` 表示总数未知。
    pub total: Option<u64>,
    /// 失败数。
    pub failed: u64,
    /// 跳过数。
    pub skipped: u64,
}

impl RuntimeStageUpdate {
    /// 创建 Running 更新。
    pub const fn running(
        stage: RuntimeStage,
        unit: RuntimeProgressUnit,
        completed: u64,
        total: Option<u64>,
    ) -> Self {
        Self {
            stage,
            state: proto::RuntimeStageState::RuntimeStageRunning,
            unit,
            completed,
            total,
            failed: 0,
            skipped: 0,
        }
    }
}

/// 一个 Worker slot 的当前运行投影。
#[derive(Clone, Debug)]
pub struct RuntimeWorkerUpdate {
    /// 稳定 slot。
    pub slot: u32,
    /// 可选进程 ID。
    pub process_id: Option<u32>,
    /// 当前 Worker 正在处理的稳定任务项身份。
    pub item_id: String,
    /// 当前阶段。
    pub stage: RuntimeStage,
    /// 当前文件路径。
    pub display_path: String,
    /// 物理盘身份。
    pub physical_disk_id: String,
    /// 已完成文件数。
    pub completed_files: u64,
    /// 每秒速度。
    pub speed_per_second: f64,
    /// Worker 当前文件会话子步骤。
    pub current_step: String,
    /// 缓存命中、缩略图复用或原文件回退说明。
    pub cache_detail: String,
    /// Worker 自己即时上报的真实子阶段；缺失时不得推断。
    pub phase: Option<proto::RuntimeWorkerPhase>,
    /// 当前项实际占用的 CPU 权重。
    pub cpu_weight: Option<u32>,
    /// 当前项显式传给 FFmpeg 的解码线程数。
    pub decoder_threads: Option<u32>,
}

/// 基础计算实际采用的硬上限，由编排入口一次性投影。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RuntimeExecutionConfigUpdate {
    /// 同时运行的 Hash 任务数。
    pub hash_tasks: u32,
    /// path 缓存 lane 容量。
    pub path_cache_queue_capacity: u32,
    /// content 缓存 lane 容量。
    pub content_cache_queue_capacity: u32,
    /// 待处理、媒体许可获取中和已派发未 Started 的解码总 ownership 容量。
    pub decode_queue_capacity: u32,
    /// 协调器 pending、writer 执行中和 writer 通道排队的持久化总 ownership 容量。
    pub persist_queue_capacity: u32,
    /// 实际 Worker 进程槽位数。
    pub worker_slots: u32,
    /// WorkerPool 统一 CPU 权重预算。
    pub cpu_budget: u32,
    /// 全部物理盘共享的读取许可数。
    pub global_disk_permits: u32,
    /// 单块 HDD 读取许可数。
    pub hdd_per_disk_permits: u32,
    /// 单块 SSD 读取许可数。
    pub ssd_per_disk_permits: u32,
    /// 单块未知磁盘读取许可数。
    pub unknown_per_disk_permits: u32,
}

impl RuntimeExecutionConfigUpdate {
    /// 转换成向管理端公开的可选字段协议结构。
    fn snapshot(self) -> proto::RuntimeExecutionConfig {
        proto::RuntimeExecutionConfig {
            hash_tasks: Some(self.hash_tasks),
            path_cache_queue_capacity: Some(self.path_cache_queue_capacity),
            content_cache_queue_capacity: Some(self.content_cache_queue_capacity),
            decode_queue_capacity: Some(self.decode_queue_capacity),
            persist_queue_capacity: Some(self.persist_queue_capacity),
            worker_slots: Some(self.worker_slots),
            cpu_budget: Some(self.cpu_budget),
            global_disk_permits: Some(self.global_disk_permits),
            hdd_per_disk_permits: Some(self.hdd_per_disk_permits),
            ssd_per_disk_permits: Some(self.ssd_per_disk_permits),
            unknown_per_disk_permits: Some(self.unknown_per_disk_permits),
        }
    }
}

/// 基础计算五段有界队列。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimePipelineQueue {
    /// 正在读取并计算 MD5 的 Node Hash 队列。
    Hash,
    /// Hash 前 path 缓存 lane。
    PathCache,
    /// Hash 后 content 缓存 lane。
    ContentCache,
    /// 等待媒体许可或 Worker 的解码队列。
    Decode,
    /// 等待 SQLite actor 或 ACK 的持久化队列。
    Persist,
}

/// 基础计算共享资源类别。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimePipelineResource {
    /// Node Hash 读取许可。
    HashIo,
    /// Worker 媒体读取许可。
    MediaIo,
    /// WorkerPool CPU 权重。
    CpuWeight,
    /// Worker 进程槽位。
    WorkerSlots,
}

/// 逐物理盘读取许可所属的真实读取阶段。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimeDiskReadClass {
    /// Node 在 MD5 阶段持有的读取许可。
    Hash,
    /// Worker 在媒体探测或解码阶段持有的读取许可。
    Media,
}

/// 基础计算中必须与真实所有权一一对应的细分状态。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimePipelineOwnership {
    /// 等待 Hash 读取许可。
    HashWaitingPermit,
    /// 正在读取并计算 Hash。
    HashReading,
    /// Hash 已完成但尚未加入后续任务。
    HashCompletedUnjoined,
    /// 等待媒体读取许可。
    MediaPermitWaiting,
    /// 已准备尝试取得媒体许可。
    MediaAcquireReady,
    /// 已取得媒体读取许可。
    MediaPermitReady,
    /// 正在派发 Worker。
    WorkerDispatching,
    /// 已派发但尚未收到 Worker Started。
    WorkerStartPending,
    /// Worker 正在解码。
    WorkerDecode,
    /// Worker 正在计算特征。
    WorkerFeature,
    /// 等待 Worker 返回结果。
    WorkerResultWait,
    /// Worker 阶段未知但仍持有任务所有权。
    WorkerPhaseUnknown,
    /// 持有 content 输出 credit。
    ContentOutputCreditOwned,
    /// 持有 decode credit。
    DecodeCreditOwned,
}

/// 不持有外部资源、仅描述协调器控制状态的运行指标。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimePipelineControl {
    /// 当前是否有可用的 Hash refill token。
    HashRefillTokenAvailable,
}

/// 最近失败更新。
#[derive(Clone, Debug)]
pub struct RuntimeFailureUpdate {
    /// 失败阶段。
    pub stage: RuntimeStage,
    /// 文件路径。
    pub display_path: String,
    /// 失败文案。
    pub message: String,
}

/// 终态或阶段倒退等 registry 错误。
#[derive(Debug, Error)]
pub enum RuntimeTaskError {
    /// 任务已经终态。
    #[error("运行任务已经终态")]
    Terminal,
    /// 阶段状态倒退或重复终态。
    #[error("运行阶段状态不能倒退或重复终态")]
    StageRegression,
    /// 任务 ID 不存在。
    #[error("运行任务不存在")]
    Missing,
    /// finish 只接受终态。
    #[error("finish 必须使用终态")]
    NotTerminal,
    /// 当前 ownership 超过配置的真实硬上限。
    #[error("运行指标当前 ownership 超过容量")]
    CapacityExceeded,
    /// 读取许可生命周期的等待、持有或累计转换不合法。
    #[error("逐盘读取许可状态转换不合法")]
    InvalidTransition,
}

/// Node 进程内唯一运行任务 registry。
#[derive(Clone)]
pub struct RuntimeTaskRegistry {
    inner: Arc<RegistryInner>,
}

struct RegistryInner {
    tasks: RwLock<BTreeMap<String, TaskEntry>>,
    clock: Arc<dyn RuntimeTaskClock>,
    events: broadcast::Sender<proto::RuntimeTaskChanged>,
}

impl RuntimeTaskRegistry {
    /// 使用系统单调时钟创建空 registry。
    pub fn new() -> Self {
        Self::with_clock(Arc::new(SystemClock(Instant::now())))
    }

    /// 使用可控单调时钟创建空 registry。
    pub fn with_clock<C>(clock: Arc<C>) -> Self
    where
        C: RuntimeTaskClock,
    {
        let (events, _) = broadcast::channel(64);
        Self {
            inner: Arc::new(RegistryInner {
                tasks: RwLock::new(BTreeMap::new()),
                clock,
                events,
            }),
        }
    }

    /// 创建一个只在本进程存在的运行任务。
    pub async fn begin(
        &self,
        kind: RuntimeTaskKind,
        machine_id: MachineId,
        title: impl Into<String>,
    ) -> RuntimeTaskReporter {
        self.begin_with_id(Uuid::new_v4().to_string(), kind, machine_id, title)
            .await
    }

    /// 用业务 ID 创建当前进程唯一的运行任务。
    ///
    /// 协议、任务中心和后台 reporter 共用该 ID，调用者不得保存业务 ID 到运行 ID 的映射。
    pub async fn begin_with_id(
        &self,
        task_id: impl Into<String>,
        kind: RuntimeTaskKind,
        machine_id: MachineId,
        title: impl Into<String>,
    ) -> RuntimeTaskReporter {
        let task_id = task_id.into();
        self.inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned")
            .insert(
                task_id.clone(),
                TaskEntry {
                    machine_id: machine_id.as_str().into(),
                    kind,
                    title: title.into(),
                    state: RuntimeTaskState::Running,
                    overall_completed: 0,
                    overall_total: None,
                    overall_failed: 0,
                    overall_skipped: 0,
                    outbox_high_seq: None,
                    stages: BTreeMap::new(),
                    workers: BTreeMap::new(),
                    failures: VecDeque::new(),
                    execution_config: None,
                    pipeline_metrics: None,
                    last_published_at: None,
                    progress_dirty: false,
                },
            );
        RuntimeTaskReporter {
            registry: self.clone(),
            task_id,
        }
    }

    /// 返回按 ID 稳定排列的摘要。
    pub async fn list(&self) -> Vec<proto::RuntimeTaskSummary> {
        self.inner
            .tasks
            .read()
            .expect("runtime registry lock poisoned")
            .iter()
            .map(|(id, task)| task.summary(id))
            .collect()
    }

    /// 返回单个运行任务完整详情。
    pub async fn details(&self, task_id: &str) -> Option<proto::RuntimeTaskDetails> {
        let now = self.inner.clock.now();
        self.inner
            .tasks
            .read()
            .expect("runtime registry lock poisoned")
            .get(task_id)
            .map(|task| task.details(task_id, now))
    }

    /// 订阅终态事件。
    pub fn subscribe(&self) -> broadcast::Receiver<proto::RuntimeTaskChanged> {
        self.inner.events.subscribe()
    }

    /// 标记一次运行中变化；首个快照和阶段终态立即发布，其余变化等待两秒 tick。
    fn touch(&self, task_id: &str, immediate: bool) {
        let now = self.inner.clock.now();
        let event = {
            let mut tasks = self
                .inner
                .tasks
                .write()
                .expect("runtime registry lock poisoned");
            let Some(task) = tasks.get_mut(task_id) else {
                return;
            };
            if immediate || task.last_published_at.is_none() {
                task.last_published_at = Some(now);
                task.progress_dirty = false;
                Some(proto::RuntimeTaskChanged {
                    runtime_task_id: task_id.into(),
                    state: task.state.as_str().into(),
                    outbox_high_seq: task.outbox_high_seq,
                })
            } else {
                task.progress_dirty = true;
                None
            }
        };
        if let Some(event) = event {
            let _ = self.inner.events.send(event);
        }
    }

    /// 发布距离上次快照已满两秒的最新运行中状态。
    fn publish_due(&self) {
        let now = self.inner.clock.now();
        let events = {
            let mut tasks = self
                .inner
                .tasks
                .write()
                .expect("runtime registry lock poisoned");
            tasks
                .iter_mut()
                .filter_map(|(task_id, task)| {
                    let due = task.progress_dirty
                        && task.last_published_at.is_some_and(|published| {
                            now.saturating_sub(published) >= PROGRESS_PUBLISH_INTERVAL
                        });
                    due.then(|| {
                        task.last_published_at = Some(now);
                        task.progress_dirty = false;
                        proto::RuntimeTaskChanged {
                            runtime_task_id: task_id.clone(),
                            state: task.state.as_str().into(),
                            outbox_high_seq: task.outbox_high_seq,
                        }
                    })
                })
                .collect::<Vec<_>>()
        };
        for event in events {
            let _ = self.inner.events.send(event);
        }
    }
}

impl Default for RuntimeTaskRegistry {
    fn default() -> Self {
        Self::new()
    }
}

/// 由 Node actor 每两秒驱动一次的运行进度合并发布器。
#[derive(Clone)]
pub struct RuntimeProgressPublisher {
    registry: RuntimeTaskRegistry,
}

impl RuntimeProgressPublisher {
    /// 绑定一个 registry；发布器不创建额外线程。
    pub const fn new(registry: RuntimeTaskRegistry) -> Self {
        Self { registry }
    }

    /// 刷出已经达到两秒间隔的最新快照。
    pub fn tick(&self) {
        self.registry.publish_due();
    }
}

/// 只冻结 registry 句柄和 task ID 的进度报告器。
#[derive(Clone)]
pub struct RuntimeTaskReporter {
    registry: RuntimeTaskRegistry,
    task_id: String,
}

impl RuntimeTaskReporter {
    /// 返回运行任务 ID。
    pub fn id(&self) -> &str {
        &self.task_id
    }

    /// 更新总体计数。
    pub async fn update_overall(
        &self,
        completed: u64,
        total: Option<u64>,
        failed: u64,
        skipped: u64,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_completed = completed;
        task.overall_total = total;
        task.overall_failed = failed;
        task.overall_skipped = skipped;
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 在同步分析/删除边界设置总体计数和已知总数。
    pub fn update_overall_nowait(
        &self,
        completed: u64,
        total: Option<u64>,
        failed: u64,
        skipped: u64,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_completed = completed;
        task.overall_total = total;
        task.overall_failed = failed;
        task.overall_skipped = skipped;
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 在单 SQLite writer 成功/失败终态边界实时推进总体计数。
    pub fn advance_overall_nowait(
        &self,
        completed: u64,
        failed: u64,
        skipped: u64,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_completed = task.overall_completed.saturating_add(completed);
        task.overall_failed = task.overall_failed.saturating_add(failed);
        task.overall_skipped = task.overall_skipped.saturating_add(skipped);
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 枚举 channel 关闭时立即冻结扫描文件总量，不等待读取或 Worker。
    pub fn freeze_scan_totals_nowait(&self, files: u64) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_total = Some(files);
        for (kind, total) in [
            (RuntimeStage::Enumerate, files),
            (RuntimeStage::CacheLookup, files),
            (RuntimeStage::ProbeStage1, files),
            (RuntimeStage::PersistFinalize, files),
        ] {
            let stage = task.stages.entry(kind).or_default();
            stage.total = Some(total);
        }
        let stage = task.stages.entry(RuntimeStage::Enumerate).or_default();
        let completed = stage.completed.max(files);
        let failed = stage.failed;
        let skipped = stage.skipped;
        task.update_stage(
            RuntimeStageUpdate {
                stage: RuntimeStage::Enumerate,
                state: proto::RuntimeStageState::RuntimeStageCompleted,
                unit: RuntimeProgressUnit::Files,
                completed,
                total: Some(files),
                failed,
                skipped,
            },
            now,
        )?;
        drop(tasks);
        self.registry.touch(&self.task_id, true);
        Ok(())
    }

    /// 枚举结束时一次冻结基础计算总文件数，并立即完成枚举阶段。
    pub fn freeze_base_compute_totals_nowait(&self, files: u64) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_total = Some(files);
        for stage_kind in [
            RuntimeStage::EnumerateFiles,
            RuntimeStage::LookupBaseCache,
            RuntimeStage::ComputeBaseFeatures,
        ] {
            task.stages.entry(stage_kind).or_default().total = Some(files);
        }
        let stage = task.stages.entry(RuntimeStage::EnumerateFiles).or_default();
        // 枚举边界和基础计算入口可能确认同一份固定清单；相同总数重复确认不应被当作阶段倒退。
        if stage.state == proto::RuntimeStageState::RuntimeStageCompleted
            && stage.completed == files
            && stage.total == Some(files)
        {
            return Ok(());
        }
        let failed = stage.failed;
        let skipped = stage.skipped;
        task.update_stage(
            RuntimeStageUpdate {
                stage: RuntimeStage::EnumerateFiles,
                state: proto::RuntimeStageState::RuntimeStageCompleted,
                unit: RuntimeProgressUnit::Files,
                completed: files,
                total: Some(files),
                failed,
                skipped,
            },
            now,
        )?;
        drop(tasks);
        self.registry.touch(&self.task_id, true);
        Ok(())
    }

    /// 更新一个固定阶段。
    pub async fn update_stage(&self, update: RuntimeStageUpdate) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let immediate = is_stage_terminal(update.state);
        task.update_stage(update, now)?;
        drop(tasks);
        self.registry.touch(&self.task_id, immediate);
        Ok(())
    }

    /// 在同步流水线边界无等待更新阶段。
    pub fn update_stage_nowait(&self, update: RuntimeStageUpdate) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let immediate = is_stage_terminal(update.state);
        active_task(&mut tasks, &self.task_id)?.update_stage(update, now)?;
        drop(tasks);
        self.registry.touch(&self.task_id, immediate);
        Ok(())
    }

    /// 在阶段首次开始实际工作时启动独立计时；重复调用不会重置起点或进度。
    pub fn start_stage_nowait(
        &self,
        stage_kind: RuntimeStage,
        unit: RuntimeProgressUnit,
    ) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let stage = task.stages.entry(stage_kind).or_default();
        if stage.state == proto::RuntimeStageState::RuntimeStageRunning {
            return Ok(());
        }
        let update = RuntimeStageUpdate {
            stage: stage_kind,
            state: proto::RuntimeStageState::RuntimeStageRunning,
            unit,
            completed: stage.completed,
            total: stage.total,
            failed: stage.failed,
            skipped: stage.skipped,
        };
        task.update_stage(update, now)?;
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 在真实进度完成边界增加阶段 completed；调用方负责选择正确计数单位。
    pub fn advance_stage_nowait(
        &self,
        stage_kind: RuntimeStage,
        unit: RuntimeProgressUnit,
        amount: u64,
    ) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let stage = task.stages.entry(stage_kind).or_default();
        let update = RuntimeStageUpdate {
            stage: stage_kind,
            state: proto::RuntimeStageState::RuntimeStageRunning,
            unit,
            completed: stage.completed.saturating_add(amount),
            total: stage.total,
            failed: stage.failed,
            skipped: stage.skipped,
        };
        task.update_stage(update, now)?;
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 为单文件终态同时增加成功、失败和跳过计数。
    pub fn advance_stage_outcome_nowait(
        &self,
        stage_kind: RuntimeStage,
        unit: RuntimeProgressUnit,
        completed: u64,
        failed: u64,
        skipped: u64,
    ) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let stage = task.stages.entry(stage_kind).or_default();
        let update = RuntimeStageUpdate {
            stage: stage_kind,
            state: proto::RuntimeStageState::RuntimeStageRunning,
            unit,
            completed: stage.completed.saturating_add(completed),
            total: stage.total,
            failed: stage.failed.saturating_add(failed),
            skipped: stage.skipped.saturating_add(skipped),
        };
        task.update_stage(update, now)?;
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 以当前 completed 冻结阶段总数并进入终态。
    pub fn finish_stage_nowait(
        &self,
        stage_kind: RuntimeStage,
        state: proto::RuntimeStageState,
        total: Option<u64>,
    ) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let stage = task.stages.entry(stage_kind).or_default();
        let (unit, completed, failed, skipped) =
            (stage.unit, stage.completed, stage.failed, stage.skipped);
        task.update_stage(
            RuntimeStageUpdate {
                stage: stage_kind,
                state,
                unit,
                completed,
                total,
                failed,
                skipped,
            },
            now,
        )?;
        drop(tasks);
        self.registry.touch(&self.task_id, true);
        Ok(())
    }

    /// 在同步 Store 终态边界追加最近失败。
    pub fn record_failure_nowait(
        &self,
        failure: RuntimeFailureUpdate,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.failures.push_back(failure);
        while task.failures.len() > MAX_RUNTIME_FAILURES {
            task.failures.pop_front();
        }
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 为当前基础计算任务登记实际配置并启用进程内流水线指标。
    pub fn configure_pipeline_nowait(
        &self,
        config: RuntimeExecutionConfigUpdate,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.execution_config = Some(config.snapshot());
        task.pipeline_metrics = Some(PipelineMetricsEntry::new(config));
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 在真实开始等待物理盘读取许可时增加每个底层盘的等待计数。
    pub fn disk_read_waiting_nowait(
        &self,
        disk_ids: &[String],
        class: RuntimeDiskReadClass,
        capacity: u64,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics_result(|metrics| {
            metrics.disk_read_waiting(disk_ids, class, capacity)
        })
    }

    /// 在未取得许可而取消等待时减少每个底层盘的等待计数。
    pub fn disk_read_wait_cancelled_nowait(
        &self,
        disk_ids: &[String],
        class: RuntimeDiskReadClass,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics_result(|metrics| {
            metrics.disk_read_wait_cancelled(disk_ids, class)
        })
    }

    /// 在真实许可取得边界原子地把等待转换为活跃并累计授予次数。
    pub fn disk_read_acquired_nowait(
        &self,
        disk_ids: &[String],
        class: RuntimeDiskReadClass,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics_result(|metrics| metrics.disk_read_acquired(disk_ids, class))
    }

    /// 在真实许可 Drop 边界减少活跃数并累计释放次数。
    pub fn disk_read_released_nowait(
        &self,
        disk_ids: &[String],
        class: RuntimeDiskReadClass,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics_result(|metrics| metrics.disk_read_released(disk_ids, class))
    }

    /// 发布一个真实 ownership 的当前值、容量和生命周期峰值。
    pub fn update_ownership_nowait(
        &self,
        kind: RuntimePipelineOwnership,
        current: u64,
        capacity: u64,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics_result(|metrics| {
            metrics.update_ownership(kind, current, capacity)
        })
    }

    /// 发布一个不参与 RAII 守恒的协调器 control-state 当前值。
    pub fn update_control_state_nowait(
        &self,
        kind: RuntimePipelineControl,
        current: u64,
        capacity: u64,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics_result(|metrics| {
            metrics.update_control_state(kind, current, capacity)
        })
    }

    /// 更新一个真实队列的当前 ownership，并保留峰值。
    pub fn update_queue_nowait(
        &self,
        queue: RuntimePipelineQueue,
        current: usize,
    ) -> Result<(), RuntimeTaskError> {
        let current = current.try_into().unwrap_or(u64::MAX);
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let entry = active_task(&mut tasks, &self.task_id)?
            .pipeline_metrics
            .as_mut()
            .ok_or(RuntimeTaskError::Missing)?
            .queues
            .get_mut(&queue)
            .expect("配置时必须建立全部流水线队列");
        if current > entry.capacity {
            return Err(RuntimeTaskError::CapacityExceeded);
        }
        entry.update(current);
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 记录一个队列项目的真实等待耗时。
    pub fn record_queue_wait_nowait(
        &self,
        queue: RuntimePipelineQueue,
        duration: Duration,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            metrics
                .queues
                .get_mut(&queue)
                .expect("配置时必须建立全部流水线队列")
                .wait_latency
                .record(duration);
        })
    }

    /// 记录一个队列项目的真实处理耗时。
    pub fn record_queue_service_nowait(
        &self,
        queue: RuntimePipelineQueue,
        duration: Duration,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            metrics
                .queues
                .get_mut(&queue)
                .expect("配置时必须建立全部流水线队列")
                .service_latency
                .record(duration);
        })
    }

    /// 更新一个真实共享资源的当前占用，并保留峰值。
    pub fn update_resource_nowait(
        &self,
        resource: RuntimePipelineResource,
        current: usize,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            metrics
                .resources
                .get_mut(&resource)
                .expect("配置时必须建立全部流水线资源")
                .update(current.try_into().unwrap_or(u64::MAX));
        })
    }

    /// 记录共享资源许可的真实等待耗时。
    pub fn record_resource_wait_nowait(
        &self,
        resource: RuntimePipelineResource,
        duration: Duration,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            metrics
                .resources
                .get_mut(&resource)
                .expect("配置时必须建立全部流水线资源")
                .wait_latency
                .record(duration);
        })
    }

    /// 记录共享资源许可的真实持有耗时。
    pub fn record_resource_service_nowait(
        &self,
        resource: RuntimePipelineResource,
        duration: Duration,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            metrics
                .resources
                .get_mut(&resource)
                .expect("配置时必须建立全部流水线资源")
                .service_latency
                .record(duration);
        })
    }

    /// 累加一个已完成任务项的真实端到端完成耗时。
    pub fn record_item_completion_latency_nowait(
        &self,
        duration: Duration,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            metrics.item_completion_latency.record(duration);
        })
    }

    /// 在真实许可取得边界原子增加占用，并记录本次等待耗时。
    pub fn resource_acquired_nowait(
        &self,
        resource: RuntimePipelineResource,
        wait: Duration,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            let entry = metrics
                .resources
                .get_mut(&resource)
                .expect("配置时必须建立全部流水线资源");
            entry.update(entry.current.saturating_add(1));
            entry.wait_latency.record(wait);
        })
    }

    /// 在真实许可 Drop 边界原子减少占用，并记录本次持有耗时。
    pub fn resource_released_nowait(
        &self,
        resource: RuntimePipelineResource,
        service: Duration,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            let entry = metrics
                .resources
                .get_mut(&resource)
                .expect("配置时必须建立全部流水线资源");
            entry.update(entry.current.saturating_sub(1));
            entry.service_latency.record(service);
        })
    }

    /// 累加 Node Hash 阶段实际读取的字节数。
    pub fn record_hash_bytes_nowait(&self, bytes: u64) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            metrics.hash_bytes = metrics.hash_bytes.saturating_add(bytes);
        })
    }

    /// 仅在 Applied ACK 后按真实媒体类型和文件大小累计吞吐。
    pub fn record_media_throughput_nowait(
        &self,
        media_kind: MediaKind,
        file_size: u64,
    ) -> Result<(), RuntimeTaskError> {
        self.with_pipeline_metrics(|metrics| {
            let size_bucket = if file_size < SMALL_MEDIA_BYTES {
                "small"
            } else if file_size < MEDIUM_MEDIA_BYTES {
                "medium"
            } else {
                "large"
            };
            let media_kind = match media_kind {
                MediaKind::Image => proto::MediaKind::MediaImage as i32,
                MediaKind::Video => proto::MediaKind::MediaVideo as i32,
                MediaKind::Other => proto::MediaKind::MediaOther as i32,
            };
            let throughput = metrics
                .throughput
                .entry((media_kind, size_bucket))
                .or_default();
            throughput.files = throughput.files.saturating_add(1);
            throughput.bytes = throughput.bytes.saturating_add(file_size);
        })
    }

    /// 在持锁闭包内修改已配置的内存指标，并沿用两秒发布合并。
    fn with_pipeline_metrics(
        &self,
        update: impl FnOnce(&mut PipelineMetricsEntry),
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let metrics = active_task(&mut tasks, &self.task_id)?
            .pipeline_metrics
            .as_mut()
            .ok_or(RuntimeTaskError::Missing)?;
        update(metrics);
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 在持锁闭包内修改可失败的流水线指标，并仅在成功后发布变化。
    fn with_pipeline_metrics_result(
        &self,
        update: impl FnOnce(&mut PipelineMetricsEntry) -> Result<(), RuntimeTaskError>,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let metrics = active_task(&mut tasks, &self.task_id)?
            .pipeline_metrics
            .as_mut()
            .ok_or(RuntimeTaskError::Missing)?;
        update(metrics)?;
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// UPSERT 一个 Worker slot。
    pub async fn update_worker(&self, worker: RuntimeWorkerUpdate) -> Result<(), RuntimeTaskError> {
        self.worker_started(worker).await
    }

    /// 在真实 Pool Started 边界更新 slot，不重置累计完成数/速度样本。
    pub async fn worker_started(
        &self,
        worker: RuntimeWorkerUpdate,
    ) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let previous = task.workers.get(&worker.slot).map(|entry| {
            (
                !entry.current_item_id.is_empty(),
                entry.current_item_id == worker.item_id,
                entry.cpu_weight.unwrap_or_default() as u64,
            )
        });
        if !previous.is_some_and(|(_, same_item, _)| same_item)
            && let Some(metrics) = task.pipeline_metrics.as_mut()
        {
            let previous_active = previous.is_some_and(|(active, _, _)| active);
            let previous_cpu = previous.map_or(0, |(_, _, cpu)| cpu);
            let next_slots = metrics
                .resources
                .get(&RuntimePipelineResource::WorkerSlots)
                .expect("配置时必须建立 Worker slot 资源")
                .current
                .saturating_sub(u64::from(previous_active))
                .saturating_add(1);
            let next_cpu = metrics
                .resources
                .get(&RuntimePipelineResource::CpuWeight)
                .expect("配置时必须建立 CPU 权重资源")
                .current
                .saturating_sub(previous_cpu)
                .saturating_add(worker.cpu_weight.unwrap_or_default() as u64);
            ensure_resource_capacity(metrics, RuntimePipelineResource::WorkerSlots, next_slots)?;
            ensure_resource_capacity(metrics, RuntimePipelineResource::CpuWeight, next_cpu)?;
            metrics
                .resources
                .get_mut(&RuntimePipelineResource::WorkerSlots)
                .expect("配置时必须建立 Worker slot 资源")
                .update(next_slots);
            metrics
                .resources
                .get_mut(&RuntimePipelineResource::CpuWeight)
                .expect("配置时必须建立 CPU 权重资源")
                .update(next_cpu);
        }
        task.workers
            .entry(worker.slot)
            .or_default()
            .started(worker, now);
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 更新既有 Worker slot 的当前子步骤和缓存来源，不改变累计完成数。
    pub fn worker_step_nowait(
        &self,
        slot: u32,
        current_step: impl Into<String>,
        cache_detail: impl Into<String>,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let worker = active_task(&mut tasks, &self.task_id)?
            .workers
            .get_mut(&slot)
            .ok_or(RuntimeTaskError::Missing)?;
        worker.current_step = current_step.into();
        worker.cache_detail = cache_detail.into();
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 按 task item 身份更新 Worker 显式阶段；迟到事件不会污染复用后的 slot。
    pub fn worker_phase_nowait(
        &self,
        slot: u32,
        item_id: &str,
        phase: proto::RuntimeWorkerPhase,
        _request_elapsed: Option<Duration>,
    ) -> Result<(), RuntimeTaskError> {
        if phase == proto::RuntimeWorkerPhase::Unspecified {
            return Ok(());
        }
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let worker = task
            .workers
            .get_mut(&slot)
            .ok_or(RuntimeTaskError::Missing)?;
        if worker.current_item_id != item_id {
            return Ok(());
        }
        worker.phase = Some(phase);
        worker.current_step = worker_phase_display(phase).into();
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 记录源读取结束耗时但保持当前 Worker phase 不变。
    pub fn worker_source_read_complete_nowait(
        &self,
        slot: u32,
        item_id: &str,
        request_elapsed: Option<Duration>,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let worker = task.workers.get(&slot).ok_or(RuntimeTaskError::Missing)?;
        if worker.current_item_id != item_id {
            return Ok(());
        }
        if let (Some(duration), Some(metrics)) = (request_elapsed, task.pipeline_metrics.as_mut()) {
            metrics
                .queues
                .get_mut(&RuntimePipelineQueue::Decode)
                .expect("配置时必须建立解码队列")
                .service_latency
                .record(duration);
        }
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 在 Worker terminal/crash/cancel 边界立即清理匹配项，不等待持久化 ACK。
    pub fn worker_released_nowait(&self, slot: u32, item_id: &str) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let worker = task
            .workers
            .get_mut(&slot)
            .ok_or(RuntimeTaskError::Missing)?;
        if worker.current_item_id != item_id {
            return Ok(());
        }
        let cpu_weight = worker.cpu_weight.unwrap_or_default() as u64;
        worker.release();
        if let Some(metrics) = task.pipeline_metrics.as_mut() {
            let slots = metrics
                .resources
                .get_mut(&RuntimePipelineResource::WorkerSlots)
                .expect("配置时必须建立 Worker slot 资源");
            slots.update(slots.current.saturating_sub(1));
            let cpu = metrics
                .resources
                .get_mut(&RuntimePipelineResource::CpuWeight)
                .expect("配置时必须建立 CPU 权重资源");
            cpu.update(cpu.current.saturating_sub(cpu_weight));
        }
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 在实际后台清理完成后把全部当前 ownership 归零，生命周期峰值保持不变。
    pub fn clear_pipeline_ownership_nowait(&self) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        active_task(&mut tasks, &self.task_id)?.clear_current_ownership();
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 在真实 Pool terminal event 边界给 slot 完成文件数加一并更新 10 秒速度。
    pub async fn worker_completed(&self, slot: u32) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.workers.entry(slot).or_default().completed(now);
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 追加最近失败并只保留末尾 20 条。
    pub async fn record_failure(
        &self,
        failure: RuntimeFailureUpdate,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.failures.push_back(failure);
        while task.failures.len() > MAX_RUNTIME_FAILURES {
            task.failures.pop_front();
        }
        drop(tasks);
        self.registry.touch(&self.task_id, false);
        Ok(())
    }

    /// 进入一次不可逆终态并广播一次。
    pub async fn finish(&self, state: RuntimeTaskState) -> Result<(), RuntimeTaskError> {
        self.finish_with_optional_outbox_high_seq(state, None)
    }

    /// 带 SQLite outbox 真实高水位进入终态，并与终态事件原子发布。
    pub async fn finish_with_outbox_high_seq(
        &self,
        state: RuntimeTaskState,
        outbox_high_seq: u64,
    ) -> Result<(), RuntimeTaskError> {
        self.finish_with_optional_outbox_high_seq(state, Some(outbox_high_seq))
    }

    /// 在同一 registry 写锁内更新终态和可选 outbox 高水位。
    fn finish_with_optional_outbox_high_seq(
        &self,
        state: RuntimeTaskState,
        outbox_high_seq: Option<u64>,
    ) -> Result<(), RuntimeTaskError> {
        if !state.is_terminal() {
            return Err(RuntimeTaskError::NotTerminal);
        }
        let mut tasks = self
            .registry
            .inner
            .tasks
            .write()
            .expect("runtime registry lock poisoned");
        let task = tasks
            .get_mut(&self.task_id)
            .ok_or(RuntimeTaskError::Missing)?;
        if task.state.is_terminal() {
            return Err(RuntimeTaskError::Terminal);
        }
        let now = self.registry.inner.clock.now();
        let stage_terminal = match state {
            RuntimeTaskState::Completed => proto::RuntimeStageState::RuntimeStageCompleted,
            RuntimeTaskState::Failed => proto::RuntimeStageState::RuntimeStageFailed,
            RuntimeTaskState::Cancelled => proto::RuntimeStageState::RuntimeStageSkipped,
            RuntimeTaskState::Running => unreachable!(),
        };
        for stage in task.stages.values_mut() {
            if !is_stage_terminal(stage.state) {
                stage.state = stage_terminal;
                stage.ended_at = Some(now);
            }
        }
        // 正常路径应已完成逐项释放；这里仅保证关机强制中止也不会发布自相矛盾的终态。
        task.clear_current_ownership();
        task.state = state;
        task.outbox_high_seq = outbox_high_seq;
        task.progress_dirty = false;
        task.last_published_at = Some(now);
        let overall_completed = task.overall_completed;
        let overall_failed = task.overall_failed;
        let overall_skipped = task.overall_skipped;
        let has_pipeline_metrics = task.pipeline_metrics.is_some();
        drop(tasks);
        tracing::info!(
            runtime_task_id = self.task_id,
            state = state.as_str(),
            overall_completed,
            overall_failed,
            overall_skipped,
            has_pipeline_metrics,
            "运行任务进入终态"
        );
        let _ = self.registry.inner.events.send(proto::RuntimeTaskChanged {
            runtime_task_id: self.task_id.clone(),
            state: state.as_str().into(),
            outbox_high_seq,
        });
        Ok(())
    }
}

fn active_task<'a>(
    tasks: &'a mut BTreeMap<String, TaskEntry>,
    task_id: &str,
) -> Result<&'a mut TaskEntry, RuntimeTaskError> {
    let task = tasks.get_mut(task_id).ok_or(RuntimeTaskError::Missing)?;
    if task.state.is_terminal() {
        return Err(RuntimeTaskError::Terminal);
    }
    Ok(task)
}

struct TaskEntry {
    machine_id: String,
    kind: RuntimeTaskKind,
    title: String,
    state: RuntimeTaskState,
    overall_completed: u64,
    overall_total: Option<u64>,
    overall_failed: u64,
    overall_skipped: u64,
    /// 终态时由调用方提供的真实 SQLite outbox 高水位；普通终态保持缺失。
    outbox_high_seq: Option<u64>,
    stages: BTreeMap<RuntimeStage, StageEntry>,
    workers: BTreeMap<u32, WorkerEntry>,
    failures: VecDeque<RuntimeFailureUpdate>,
    /// 当前进程实际采用的基础计算配置；未报告配置的任务保持缺失。
    execution_config: Option<proto::RuntimeExecutionConfig>,
    /// 当前进程内存指标；不会写入 SQLite 或 PostgreSQL。
    pipeline_metrics: Option<PipelineMetricsEntry>,
    /// 最近一次已通知管理端的单调时刻。
    last_published_at: Option<Duration>,
    /// 上次通知后是否还有尚未刷出的运行中变化。
    progress_dirty: bool,
}

impl TaskEntry {
    /// 清理当前进程 ownership 投影；累计峰值、延迟和吞吐均保留。
    fn clear_current_ownership(&mut self) {
        for worker in self.workers.values_mut() {
            worker.release();
        }
        if let Some(metrics) = self.pipeline_metrics.as_mut() {
            for queue in metrics.queues.values_mut() {
                queue.current = 0;
            }
            for resource in metrics.resources.values_mut() {
                resource.current = 0;
            }
            for ownership in metrics.ownership.values_mut() {
                ownership.current = 0;
            }
            for control in metrics.control_state.values_mut() {
                control.current = 0;
            }
            for disk_read in metrics.disk_reads.values_mut() {
                disk_read.clear_current();
            }
        }
    }

    fn update_stage(
        &mut self,
        update: RuntimeStageUpdate,
        now: Duration,
    ) -> Result<(), RuntimeTaskError> {
        let stage = self.stages.entry(update.stage).or_default();
        if is_stage_terminal(stage.state) || stage_rank(update.state) < stage_rank(stage.state) {
            return Err(RuntimeTaskError::StageRegression);
        }
        if update.state == proto::RuntimeStageState::RuntimeStageRunning
            && stage.started_at.is_none()
        {
            stage.started_at = Some(now);
        }
        if update.completed < stage.completed {
            stage.samples.clear();
        }
        stage.samples.push_back((now, update.completed));
        while stage
            .samples
            .front()
            .is_some_and(|(time, _)| now.saturating_sub(*time) > SPEED_WINDOW)
        {
            stage.samples.pop_front();
        }
        stage.state = update.state;
        stage.unit = update.unit;
        stage.completed = update.completed;
        stage.total = update.total;
        stage.failed = update.failed;
        stage.skipped = update.skipped;
        if is_stage_terminal(update.state) {
            stage.ended_at = Some(now);
        }
        Ok(())
    }

    fn summary(&self, id: &str) -> proto::RuntimeTaskSummary {
        let running = self
            .stages
            .iter()
            .filter(|(_, stage)| stage.state == proto::RuntimeStageState::RuntimeStageRunning)
            .map(|(stage, _)| stage.display_name())
            .collect::<Vec<_>>();
        let stage_summary = match running.as_slice() {
            [] => String::new(),
            [only] => (*only).into(),
            many => format!("{}并行", many.join(" / ")),
        };
        proto::RuntimeTaskSummary {
            runtime_task_id: id.into(),
            machine_id: self.machine_id.clone(),
            task_kind: self.kind.as_str().into(),
            title: self.title.clone(),
            state: self.state.as_str().into(),
            stage_summary,
            overall_completed: self.overall_completed,
            overall_total: self.overall_total.unwrap_or_default(),
            overall_total_known: self.overall_total.is_some(),
            overall_failed: self.overall_failed,
            overall_skipped: self.overall_skipped,
            outbox_high_seq: self.outbox_high_seq,
        }
    }

    fn details(&self, id: &str, now: Duration) -> proto::RuntimeTaskDetails {
        proto::RuntimeTaskDetails {
            summary: Some(self.summary(id)),
            stages: self
                .stages
                .iter()
                .map(|(kind, stage)| stage.snapshot(*kind, now))
                .collect(),
            workers: self.workers.values().map(WorkerEntry::snapshot).collect(),
            failures: self
                .failures
                .iter()
                .map(|failure| proto::RuntimeFailureDetails {
                    stage_id: failure.stage.id().into(),
                    display_path: failure.display_path.clone(),
                    message: failure.message.clone(),
                })
                .collect(),
            execution_config: self.execution_config.clone(),
            pipeline_metrics: self
                .pipeline_metrics
                .as_ref()
                .map(PipelineMetricsEntry::snapshot),
        }
    }
}

/// 一个固定容量队列的当前值、峰值和延迟分布。
struct QueueMetricsEntry {
    current: u64,
    peak: u64,
    capacity: u64,
    wait_latency: LatencyHistogramEntry,
    service_latency: LatencyHistogramEntry,
}

impl QueueMetricsEntry {
    /// 使用真实硬上限创建零占用队列指标。
    const fn new(capacity: u64) -> Self {
        Self {
            current: 0,
            peak: 0,
            capacity,
            wait_latency: LatencyHistogramEntry::new(),
            service_latency: LatencyHistogramEntry::new(),
        }
    }

    /// 更新当前占用并保留生命周期峰值。
    fn update(&mut self, current: u64) {
        self.current = current;
        self.peak = self.peak.max(current);
    }

    /// 生成协议快照；空直方图保持缺失。
    fn snapshot(&self) -> proto::RuntimeQueueMetrics {
        proto::RuntimeQueueMetrics {
            current: Some(self.current),
            peak: Some(self.peak),
            capacity: Some(self.capacity),
            wait_latency: self.wait_latency.snapshot(),
            service_latency: self.service_latency.snapshot(),
        }
    }
}

/// 一类共享资源的当前值、峰值和延迟分布。
struct ResourceMetricsEntry {
    current: u64,
    peak: u64,
    capacity: u64,
    wait_latency: LatencyHistogramEntry,
    service_latency: LatencyHistogramEntry,
}

impl ResourceMetricsEntry {
    /// 使用真实硬上限创建零占用资源指标。
    const fn new(capacity: u64) -> Self {
        Self {
            current: 0,
            peak: 0,
            capacity,
            wait_latency: LatencyHistogramEntry::new(),
            service_latency: LatencyHistogramEntry::new(),
        }
    }

    /// 更新当前占用并保留生命周期峰值。
    fn update(&mut self, current: u64) {
        self.current = current;
        self.peak = self.peak.max(current);
    }

    /// 生成协议快照；空直方图保持缺失。
    fn snapshot(&self) -> proto::RuntimeResourceMetrics {
        proto::RuntimeResourceMetrics {
            current: Some(self.current),
            peak: Some(self.peak),
            capacity: Some(self.capacity),
            wait_latency: self.wait_latency.snapshot(),
            service_latency: self.service_latency.snapshot(),
        }
    }
}

/// 固定 16 桶的有界延迟直方图，不保留逐项样本。
struct LatencyHistogramEntry {
    buckets: [u64; 16],
    count: u64,
    max_ms: u64,
}

impl LatencyHistogramEntry {
    /// 创建无样本的固定桶。
    const fn new() -> Self {
        Self {
            buckets: [0; 16],
            count: 0,
            max_ms: 0,
        }
    }

    /// 把一次真实耗时放入第一个包含它的固定桶。
    fn record(&mut self, duration: Duration) {
        let millis = duration.as_millis().try_into().unwrap_or(u64::MAX);
        let bucket = LATENCY_UPPER_BOUNDS_MS
            .iter()
            .position(|upper| millis <= *upper)
            .unwrap_or(LATENCY_UPPER_BOUNDS_MS.len());
        self.buckets[bucket] = self.buckets[bucket].saturating_add(1);
        self.count = self.count.saturating_add(1);
        self.max_ms = self.max_ms.max(millis);
    }

    /// 按固定桶上界返回百分位；正无穷桶使用本任务真实最大值。
    fn percentile(&self, percentile: u64) -> Option<u64> {
        if self.count == 0 {
            return None;
        }
        let rank = self.count.saturating_mul(percentile).saturating_add(99) / 100;
        let mut cumulative = 0_u64;
        for (index, count) in self.buckets.iter().enumerate() {
            cumulative = cumulative.saturating_add(*count);
            if cumulative >= rank {
                return LATENCY_UPPER_BOUNDS_MS
                    .get(index)
                    .copied()
                    .or(Some(self.max_ms));
            }
        }
        Some(self.max_ms)
    }

    /// 生成完整固定桶；无样本时返回缺失而不是伪造零延迟。
    fn snapshot(&self) -> Option<proto::RuntimeLatencyHistogram> {
        (self.count > 0).then(|| proto::RuntimeLatencyHistogram {
            buckets: self
                .buckets
                .iter()
                .enumerate()
                .map(|(index, count)| proto::RuntimeLatencyBucket {
                    upper_bound_ms: LATENCY_UPPER_BOUNDS_MS.get(index).copied(),
                    count: *count,
                })
                .collect(),
            count: self.count,
            p50_ms: self.percentile(50),
            p95_ms: self.percentile(95),
            p99_ms: self.percentile(99),
            max_ms: Some(self.max_ms),
        })
    }
}

/// 已由 Applied ACK 确认的媒体吞吐累加值。
#[derive(Default)]
struct ThroughputEntry {
    files: u64,
    bytes: u64,
}

/// 一个真实 ownership 状态的当前值、历史峰值和固定容量。
struct OwnershipMetricsEntry {
    current: u64,
    peak: u64,
    capacity: u64,
}

impl OwnershipMetricsEntry {
    /// 使用首次发布的容量建立零占用 ownership 条目。
    const fn new(capacity: u64) -> Self {
        Self {
            current: 0,
            peak: 0,
            capacity,
        }
    }

    /// 更新当前 ownership 并保留生命周期峰值。
    fn update(&mut self, current: u64) {
        self.current = current;
        self.peak = self.peak.max(current);
    }

    /// 生成保持 None/Some(0) 区别的协议快照。
    fn snapshot(&self) -> proto::RuntimeOwnershipMetrics {
        proto::RuntimeOwnershipMetrics {
            current: Some(self.current),
            peak: Some(self.peak),
            capacity: Some(self.capacity),
        }
    }
}

/// 一个协调器 control-state 的当前值、历史峰值和固定容量。
struct ControlMetricsEntry {
    current: u64,
    peak: u64,
    capacity: u64,
}

impl ControlMetricsEntry {
    /// 使用首次发布的容量建立零状态 control-state 条目。
    const fn new(capacity: u64) -> Self {
        Self {
            current: 0,
            peak: 0,
            capacity,
        }
    }

    /// 更新控制状态并保留生命周期峰值；不执行 ownership 释放。
    fn update(&mut self, current: u64) {
        self.current = current;
        self.peak = self.peak.max(current);
    }

    /// 生成统一 wire shape 的控制状态快照。
    fn snapshot(&self) -> proto::RuntimeOwnershipMetrics {
        proto::RuntimeOwnershipMetrics {
            current: Some(self.current),
            peak: Some(self.peak),
            capacity: Some(self.capacity),
        }
    }
}

/// 基础计算任务的全部进程内实时指标。
struct PipelineMetricsEntry {
    queues: BTreeMap<RuntimePipelineQueue, QueueMetricsEntry>,
    resources: BTreeMap<RuntimePipelineResource, ResourceMetricsEntry>,
    /// 只保存生产者实际发布过的 ownership 状态，避免伪造零值。
    ownership: BTreeMap<RuntimePipelineOwnership, OwnershipMetricsEntry>,
    /// 独立保存不参与 RAII 守恒的协调器状态。
    control_state: BTreeMap<RuntimePipelineControl, ControlMetricsEntry>,
    hash_bytes: u64,
    throughput: BTreeMap<(i32, &'static str), ThroughputEntry>,
    /// 以物理盘 ID 排序的真实读取许可生命周期指标。
    disk_reads: BTreeMap<String, DiskReadMetricsEntry>,
    /// 已完成 item 的端到端耗时直方图。
    item_completion_latency: LatencyHistogramEntry,
}

impl PipelineMetricsEntry {
    /// 按实际配置建立全部队列和资源容量。
    fn new(config: RuntimeExecutionConfigUpdate) -> Self {
        Self {
            queues: BTreeMap::from([
                (
                    RuntimePipelineQueue::Hash,
                    QueueMetricsEntry::new(config.hash_tasks.into()),
                ),
                (
                    RuntimePipelineQueue::PathCache,
                    QueueMetricsEntry::new(config.path_cache_queue_capacity.into()),
                ),
                (
                    RuntimePipelineQueue::ContentCache,
                    QueueMetricsEntry::new(config.content_cache_queue_capacity.into()),
                ),
                (
                    RuntimePipelineQueue::Decode,
                    QueueMetricsEntry::new(config.decode_queue_capacity.into()),
                ),
                (
                    RuntimePipelineQueue::Persist,
                    QueueMetricsEntry::new(config.persist_queue_capacity.into()),
                ),
            ]),
            resources: BTreeMap::from([
                (
                    RuntimePipelineResource::HashIo,
                    ResourceMetricsEntry::new(config.global_disk_permits.into()),
                ),
                (
                    RuntimePipelineResource::MediaIo,
                    ResourceMetricsEntry::new(config.global_disk_permits.into()),
                ),
                (
                    RuntimePipelineResource::CpuWeight,
                    ResourceMetricsEntry::new(config.cpu_budget.into()),
                ),
                (
                    RuntimePipelineResource::WorkerSlots,
                    ResourceMetricsEntry::new(config.worker_slots.into()),
                ),
            ]),
            ownership: BTreeMap::new(),
            control_state: BTreeMap::new(),
            hash_bytes: 0,
            throughput: BTreeMap::new(),
            disk_reads: BTreeMap::new(),
            item_completion_latency: LatencyHistogramEntry::new(),
        }
    }

    /// 更新真实 ownership；首次成功发布时固定容量并创建条目。
    fn update_ownership(
        &mut self,
        kind: RuntimePipelineOwnership,
        current: u64,
        capacity: u64,
    ) -> Result<(), RuntimeTaskError> {
        if current > capacity {
            return Err(RuntimeTaskError::CapacityExceeded);
        }
        let entry = self
            .ownership
            .entry(kind)
            .or_insert_with(|| OwnershipMetricsEntry::new(capacity));
        if current > entry.capacity {
            return Err(RuntimeTaskError::CapacityExceeded);
        }
        entry.update(current);
        Ok(())
    }

    /// 更新独立 control-state；它不创建或修改 ownership 条目。
    fn update_control_state(
        &mut self,
        kind: RuntimePipelineControl,
        current: u64,
        capacity: u64,
    ) -> Result<(), RuntimeTaskError> {
        if current > capacity {
            return Err(RuntimeTaskError::CapacityExceeded);
        }
        let entry = self
            .control_state
            .entry(kind)
            .or_insert_with(|| ControlMetricsEntry::new(capacity));
        if current > entry.capacity {
            return Err(RuntimeTaskError::CapacityExceeded);
        }
        entry.update(current);
        Ok(())
    }

    /// 在保持输入首见顺序去重后返回本次要更新的物理盘 ID。
    fn unique_disk_ids<'a>(disk_ids: &'a [String]) -> Vec<&'a str> {
        let mut seen = BTreeMap::new();
        disk_ids
            .iter()
            .filter_map(|disk_id| {
                seen.insert(disk_id.as_str(), ())
                    .is_none()
                    .then_some(disk_id.as_str())
            })
            .collect()
    }

    /// 记录开始等待，并在首次观察或容量收紧时校验真实活跃数。
    fn disk_read_waiting(
        &mut self,
        disk_ids: &[String],
        class: RuntimeDiskReadClass,
        capacity: u64,
    ) -> Result<(), RuntimeTaskError> {
        let disk_ids = Self::validated_disk_ids(disk_ids)?;
        for disk_id in &disk_ids {
            if let Some(entry) = self.disk_reads.get(*disk_id) {
                let next_capacity = entry.capacity.min(capacity);
                if entry.active_total()? > next_capacity {
                    return Err(RuntimeTaskError::InvalidTransition);
                }
                entry
                    .waiting(class)
                    .checked_add(1)
                    .ok_or(RuntimeTaskError::InvalidTransition)?;
            }
        }
        for disk_id in disk_ids {
            let entry = self
                .disk_reads
                .entry(disk_id.into())
                .or_insert_with(|| DiskReadMetricsEntry::new(capacity));
            entry.capacity = entry.capacity.min(capacity);
            entry.increment_waiting(class)?;
        }
        Ok(())
    }

    /// 取消等待；先完整校验复合盘，再一次完成所有盘的转换。
    fn disk_read_wait_cancelled(
        &mut self,
        disk_ids: &[String],
        class: RuntimeDiskReadClass,
    ) -> Result<(), RuntimeTaskError> {
        let disk_ids = Self::validated_disk_ids(disk_ids)?;
        self.validate_disk_reads(&disk_ids, |entry| entry.waiting(class) > 0)?;
        for disk_id in disk_ids {
            self.disk_reads
                .get_mut(disk_id)
                .expect("已校验的逐盘指标必须存在")
                .decrement_waiting(class)?;
        }
        Ok(())
    }

    /// 把等待原子转换为活跃；失败时所有底层盘均保持原快照。
    fn disk_read_acquired(
        &mut self,
        disk_ids: &[String],
        class: RuntimeDiskReadClass,
    ) -> Result<(), RuntimeTaskError> {
        let disk_ids = Self::validated_disk_ids(disk_ids)?;
        for disk_id in &disk_ids {
            let entry = self
                .disk_reads
                .get(*disk_id)
                .ok_or(RuntimeTaskError::InvalidTransition)?;
            if entry.waiting(class) == 0 || entry.granted(class).checked_add(1).is_none() {
                return Err(RuntimeTaskError::InvalidTransition);
            }
            if entry.active_total()? >= entry.capacity {
                return Err(RuntimeTaskError::CapacityExceeded);
            }
        }
        for disk_id in disk_ids {
            self.disk_reads
                .get_mut(disk_id)
                .expect("已校验的逐盘指标必须存在")
                .acquire(class)?;
        }
        Ok(())
    }

    /// 在许可 Drop 边界减少活跃数，并保证累计释放从不超过累计授予。
    fn disk_read_released(
        &mut self,
        disk_ids: &[String],
        class: RuntimeDiskReadClass,
    ) -> Result<(), RuntimeTaskError> {
        let disk_ids = Self::validated_disk_ids(disk_ids)?;
        self.validate_disk_reads(&disk_ids, |entry| {
            entry.active(class) > 0
                && entry.released(class) < entry.granted(class)
                && entry.released(class).checked_add(1).is_some()
        })?;
        for disk_id in disk_ids {
            self.disk_reads
                .get_mut(disk_id)
                .expect("已校验的逐盘指标必须存在")
                .release(class)?;
        }
        Ok(())
    }

    /// 验证输入至少含有一个非空物理盘 ID，并保留非空 ID 的原始拼写。
    fn validated_disk_ids<'a>(disk_ids: &'a [String]) -> Result<Vec<&'a str>, RuntimeTaskError> {
        if disk_ids.is_empty() || disk_ids.iter().any(|disk_id| disk_id.trim().is_empty()) {
            return Err(RuntimeTaskError::InvalidTransition);
        }
        Ok(Self::unique_disk_ids(disk_ids))
    }

    /// 验证复合盘涉及的每个已有条目，避免半更新的遥测快照。
    fn validate_disk_reads(
        &self,
        disk_ids: &[&str],
        predicate: impl Fn(&DiskReadMetricsEntry) -> bool,
    ) -> Result<(), RuntimeTaskError> {
        for disk_id in disk_ids {
            let entry = self
                .disk_reads
                .get(*disk_id)
                .ok_or(RuntimeTaskError::InvalidTransition)?;
            if !predicate(entry) {
                return Err(RuntimeTaskError::InvalidTransition);
            }
        }
        Ok(())
    }

    /// 生成当前不可持久化的协议快照。
    fn snapshot(&self) -> proto::RuntimePipelineMetrics {
        let queue = |kind| self.queues.get(&kind).map(QueueMetricsEntry::snapshot);
        let resource = |kind| {
            self.resources
                .get(&kind)
                .map(ResourceMetricsEntry::snapshot)
        };
        let ownership = |kind| {
            self.ownership
                .get(&kind)
                .map(OwnershipMetricsEntry::snapshot)
        };
        let control = |kind| {
            self.control_state
                .get(&kind)
                .map(ControlMetricsEntry::snapshot)
        };
        proto::RuntimePipelineMetrics {
            hash_queue: queue(RuntimePipelineQueue::Hash),
            path_cache_queue: queue(RuntimePipelineQueue::PathCache),
            content_cache_queue: queue(RuntimePipelineQueue::ContentCache),
            decode_queue: queue(RuntimePipelineQueue::Decode),
            persist_queue: queue(RuntimePipelineQueue::Persist),
            hash_io: resource(RuntimePipelineResource::HashIo),
            media_io: resource(RuntimePipelineResource::MediaIo),
            cpu_weight: resource(RuntimePipelineResource::CpuWeight),
            worker_slots: resource(RuntimePipelineResource::WorkerSlots),
            hash_bytes: Some(self.hash_bytes),
            media_throughput: self
                .throughput
                .iter()
                .map(
                    |((media_kind, size_bucket), value)| proto::RuntimeMediaThroughput {
                        media_kind: *media_kind,
                        size_bucket: (*size_bucket).into(),
                        files: value.files,
                        bytes: value.bytes,
                    },
                )
                .collect(),
            hash_waiting_permit: ownership(RuntimePipelineOwnership::HashWaitingPermit),
            hash_reading: ownership(RuntimePipelineOwnership::HashReading),
            hash_completed_unjoined: ownership(RuntimePipelineOwnership::HashCompletedUnjoined),
            media_permit_waiting: ownership(RuntimePipelineOwnership::MediaPermitWaiting),
            media_acquire_ready: ownership(RuntimePipelineOwnership::MediaAcquireReady),
            media_permit_ready: ownership(RuntimePipelineOwnership::MediaPermitReady),
            worker_dispatching: ownership(RuntimePipelineOwnership::WorkerDispatching),
            worker_start_pending: ownership(RuntimePipelineOwnership::WorkerStartPending),
            worker_decode: ownership(RuntimePipelineOwnership::WorkerDecode),
            worker_feature: ownership(RuntimePipelineOwnership::WorkerFeature),
            worker_result_wait: ownership(RuntimePipelineOwnership::WorkerResultWait),
            worker_phase_unknown: ownership(RuntimePipelineOwnership::WorkerPhaseUnknown),
            content_output_credit_owned: ownership(
                RuntimePipelineOwnership::ContentOutputCreditOwned,
            ),
            hash_refill_token_available: control(RuntimePipelineControl::HashRefillTokenAvailable),
            decode_credit_owned: ownership(RuntimePipelineOwnership::DecodeCreditOwned),
            item_completion_latency: self.item_completion_latency.snapshot(),
            disk_reads: self
                .disk_reads
                .iter()
                .map(|(disk_id, entry)| entry.snapshot(disk_id))
                .collect(),
        }
    }
}

/// 单一类别在一个物理盘上的等待、活跃和累计许可状态。
#[derive(Default)]
struct DiskReadClassEntry {
    waiting: u64,
    active: u64,
    granted: u64,
    released: u64,
}

/// 一个物理盘的读取许可状态；容量用于 Hash 和媒体活跃数的合计上限。
struct DiskReadMetricsEntry {
    capacity: u64,
    hash: DiskReadClassEntry,
    media: DiskReadClassEntry,
}

impl DiskReadMetricsEntry {
    /// 按首次观察到的物理盘容量创建零值状态。
    const fn new(capacity: u64) -> Self {
        Self {
            capacity,
            hash: DiskReadClassEntry {
                waiting: 0,
                active: 0,
                granted: 0,
                released: 0,
            },
            media: DiskReadClassEntry {
                waiting: 0,
                active: 0,
                granted: 0,
                released: 0,
            },
        }
    }

    /// 返回给定读取类别的可变状态。
    fn class_mut(&mut self, class: RuntimeDiskReadClass) -> &mut DiskReadClassEntry {
        match class {
            RuntimeDiskReadClass::Hash => &mut self.hash,
            RuntimeDiskReadClass::Media => &mut self.media,
        }
    }

    /// 返回给定读取类别的状态。
    fn class(&self, class: RuntimeDiskReadClass) -> &DiskReadClassEntry {
        match class {
            RuntimeDiskReadClass::Hash => &self.hash,
            RuntimeDiskReadClass::Media => &self.media,
        }
    }

    /// 返回两个读取类别合计的真实活跃许可数。
    fn active_total(&self) -> Result<u64, RuntimeTaskError> {
        self.hash
            .active
            .checked_add(self.media.active)
            .ok_or(RuntimeTaskError::InvalidTransition)
    }

    /// 返回给定类别的等待许可数。
    fn waiting(&self, class: RuntimeDiskReadClass) -> u64 {
        self.class(class).waiting
    }

    /// 返回给定类别的活跃许可数。
    fn active(&self, class: RuntimeDiskReadClass) -> u64 {
        self.class(class).active
    }

    /// 返回给定类别的累计授予许可数。
    fn granted(&self, class: RuntimeDiskReadClass) -> u64 {
        self.class(class).granted
    }

    /// 返回给定类别的累计释放许可数。
    fn released(&self, class: RuntimeDiskReadClass) -> u64 {
        self.class(class).released
    }

    /// 增加等待数，溢出时明确失败。
    fn increment_waiting(&mut self, class: RuntimeDiskReadClass) -> Result<(), RuntimeTaskError> {
        let entry = self.class_mut(class);
        entry.waiting = entry
            .waiting
            .checked_add(1)
            .ok_or(RuntimeTaskError::InvalidTransition)?;
        Ok(())
    }

    /// 等待转活跃并累计授予，调用方已完成容量和完整复合盘校验。
    fn acquire(&mut self, class: RuntimeDiskReadClass) -> Result<(), RuntimeTaskError> {
        let entry = self.class_mut(class);
        entry.waiting = entry
            .waiting
            .checked_sub(1)
            .ok_or(RuntimeTaskError::InvalidTransition)?;
        entry.active = entry
            .active
            .checked_add(1)
            .ok_or(RuntimeTaskError::InvalidTransition)?;
        entry.granted = entry
            .granted
            .checked_add(1)
            .ok_or(RuntimeTaskError::InvalidTransition)?;
        Ok(())
    }

    /// 减少等待数；调用方已校验非负。
    fn decrement_waiting(&mut self, class: RuntimeDiskReadClass) -> Result<(), RuntimeTaskError> {
        let entry = self.class_mut(class);
        entry.waiting = entry
            .waiting
            .checked_sub(1)
            .ok_or(RuntimeTaskError::InvalidTransition)?;
        Ok(())
    }

    /// 减少活跃数并累计释放，调用方已校验累计守恒。
    fn release(&mut self, class: RuntimeDiskReadClass) -> Result<(), RuntimeTaskError> {
        let entry = self.class_mut(class);
        entry.active = entry
            .active
            .checked_sub(1)
            .ok_or(RuntimeTaskError::InvalidTransition)?;
        entry.released = entry
            .released
            .checked_add(1)
            .ok_or(RuntimeTaskError::InvalidTransition)?;
        Ok(())
    }

    /// 任务终态只归零瞬时状态，累计授予和释放用于完整生命周期审计。
    fn clear_current(&mut self) {
        self.hash.waiting = 0;
        self.hash.active = 0;
        self.media.waiting = 0;
        self.media.active = 0;
    }

    /// 生成保持 None/Some(0) 区分的逐盘协议快照。
    fn snapshot(&self, physical_disk_id: &str) -> proto::RuntimeDiskReadMetrics {
        proto::RuntimeDiskReadMetrics {
            physical_disk_id: physical_disk_id.into(),
            capacity: Some(self.capacity),
            hash_waiting: Some(self.hash.waiting),
            media_waiting: Some(self.media.waiting),
            hash_active: Some(self.hash.active),
            media_active: Some(self.media.active),
            hash_granted_total: Some(self.hash.granted),
            media_granted_total: Some(self.media.granted),
            hash_released_total: Some(self.hash.released),
            media_released_total: Some(self.media.released),
        }
    }
}

/// 校验资源身份事件计算出的当前值没有突破实际配置。
fn ensure_resource_capacity(
    metrics: &PipelineMetricsEntry,
    resource: RuntimePipelineResource,
    current: u64,
) -> Result<(), RuntimeTaskError> {
    let capacity = metrics
        .resources
        .get(&resource)
        .expect("配置时必须建立全部流水线资源")
        .capacity;
    if current > capacity {
        return Err(RuntimeTaskError::CapacityExceeded);
    }
    Ok(())
}

#[derive(Default)]
struct WorkerEntry {
    slot: u32,
    process_id: Option<u32>,
    current_item_id: String,
    stage: Option<RuntimeStage>,
    display_path: String,
    physical_disk_id: String,
    completed_files: u64,
    speed_per_second: f64,
    current_step: String,
    cache_detail: String,
    phase: Option<proto::RuntimeWorkerPhase>,
    cpu_weight: Option<u32>,
    decoder_threads: Option<u32>,
    samples: VecDeque<(Duration, u64)>,
}

impl WorkerEntry {
    /// 把 slot 恢复为空闲；累计完成数和速度样本保持不变。
    fn release(&mut self) {
        self.current_item_id.clear();
        self.stage = None;
        self.display_path.clear();
        self.physical_disk_id.clear();
        self.current_step = "空闲".into();
        self.cache_detail.clear();
        self.phase = Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle);
        self.cpu_weight = None;
        self.decoder_threads = None;
    }

    fn started(&mut self, update: RuntimeWorkerUpdate, now: Duration) {
        self.slot = update.slot;
        self.process_id = update.process_id;
        self.current_item_id = update.item_id;
        self.stage = Some(update.stage);
        self.display_path = update.display_path;
        self.physical_disk_id = update.physical_disk_id;
        self.current_step = update.current_step;
        self.cache_detail = update.cache_detail;
        self.phase = update.phase;
        self.cpu_weight = update.cpu_weight;
        self.decoder_threads = update.decoder_threads;
        if self.samples.is_empty() {
            self.samples.push_back((now, self.completed_files));
        }
    }

    fn completed(&mut self, now: Duration) {
        self.completed_files = self.completed_files.saturating_add(1);
        self.samples.push_back((now, self.completed_files));
        while self
            .samples
            .front()
            .is_some_and(|(time, _)| now.saturating_sub(*time) > SPEED_WINDOW)
        {
            self.samples.pop_front();
        }
        self.speed_per_second = sample_speed(&self.samples);
    }

    fn snapshot(&self) -> proto::RuntimeWorkerDetails {
        proto::RuntimeWorkerDetails {
            slot: self.slot,
            process_id: self.process_id,
            stage_id: self
                .stage
                .map_or_else(|| "idle".into(), |stage| stage.id().into()),
            display_path: self.display_path.clone(),
            physical_disk_id: self.physical_disk_id.clone(),
            completed_files: self.completed_files,
            speed_per_second: self.speed_per_second,
            current_step: self.current_step.clone(),
            cache_detail: self.cache_detail.clone(),
            phase: self.phase.map(|phase| phase as i32),
            cpu_weight: self.cpu_weight,
            decoder_threads: self.decoder_threads,
        }
    }
}

struct StageEntry {
    state: proto::RuntimeStageState,
    unit: RuntimeProgressUnit,
    completed: u64,
    total: Option<u64>,
    failed: u64,
    skipped: u64,
    started_at: Option<Duration>,
    ended_at: Option<Duration>,
    /// 重启前已经累计的阶段耗时。
    accumulated_elapsed: Duration,
    samples: VecDeque<(Duration, u64)>,
}

impl Default for StageEntry {
    fn default() -> Self {
        Self {
            state: proto::RuntimeStageState::Unspecified,
            unit: RuntimeProgressUnit::Items,
            completed: 0,
            total: None,
            failed: 0,
            skipped: 0,
            started_at: None,
            ended_at: None,
            accumulated_elapsed: Duration::ZERO,
            samples: VecDeque::new(),
        }
    }
}

impl StageEntry {
    fn snapshot(&self, kind: RuntimeStage, now: Duration) -> proto::RuntimeStageDetails {
        let speed = self.speed();
        let eta_ms = self.total.and_then(|total| {
            (speed > 0.0).then(|| {
                (((total.saturating_sub(self.completed)) as f64 / speed) * 1000.0)
                    .round()
                    .clamp(0.0, u64::MAX as f64) as u64
            })
        });
        let live_elapsed = self
            .started_at
            .map(|start| self.ended_at.unwrap_or(now).saturating_sub(start))
            .unwrap_or_default();
        let elapsed = self.accumulated_elapsed.saturating_add(live_elapsed);
        proto::RuntimeStageDetails {
            stage_id: kind.id().into(),
            display_name: kind.display_name().into(),
            state: self.state as i32,
            unit: self.unit.as_str().into(),
            completed: self.completed,
            total: self.total.unwrap_or_default(),
            total_known: self.total.is_some(),
            failed: self.failed,
            skipped: self.skipped,
            speed_per_second: speed,
            elapsed_ms: elapsed.as_millis().try_into().unwrap_or(u64::MAX),
            eta_ms,
        }
    }

    fn speed(&self) -> f64 {
        let Some((first_time, first_value)) = self.samples.front() else {
            return 0.0;
        };
        let Some((last_time, last_value)) = self.samples.back() else {
            return 0.0;
        };
        let seconds = last_time.saturating_sub(*first_time).as_secs_f64();
        if seconds <= 0.0 || last_value < first_value {
            0.0
        } else {
            finite_nonnegative((*last_value - *first_value) as f64 / seconds)
        }
    }
}

fn stage_rank(state: proto::RuntimeStageState) -> u8 {
    match state {
        proto::RuntimeStageState::Unspecified => 0,
        proto::RuntimeStageState::RuntimeStageWaiting => 1,
        proto::RuntimeStageState::RuntimeStageRunning => 2,
        proto::RuntimeStageState::RuntimeStageCompleted
        | proto::RuntimeStageState::RuntimeStageFailed
        | proto::RuntimeStageState::RuntimeStageSkipped => 3,
    }
}

fn is_stage_terminal(state: proto::RuntimeStageState) -> bool {
    stage_rank(state) == 3
}

fn finite_nonnegative(value: f64) -> f64 {
    if value.is_finite() && value >= 0.0 {
        value
    } else {
        0.0
    }
}

/// 将显式 Worker 阶段转换成简短中文显示；未指定值不会由调用方写入。
const fn worker_phase_display(phase: proto::RuntimeWorkerPhase) -> &'static str {
    match phase {
        proto::RuntimeWorkerPhase::RuntimeWorkerIdle => "空闲",
        proto::RuntimeWorkerPhase::RuntimeWorkerDecode => "解码",
        proto::RuntimeWorkerPhase::RuntimeWorkerFeature => "特征计算",
        proto::RuntimeWorkerPhase::RuntimeWorkerResultWait => "等待结果发送",
        proto::RuntimeWorkerPhase::Unspecified => "—",
    }
}

fn sample_speed(samples: &VecDeque<(Duration, u64)>) -> f64 {
    let (Some((first_time, first)), Some((last_time, last))) = (samples.front(), samples.back())
    else {
        return 0.0;
    };
    let seconds = last_time.saturating_sub(*first_time).as_secs_f64();
    if seconds <= 0.0 || last < first {
        0.0
    } else {
        finite_nonnegative((*last - *first) as f64 / seconds)
    }
}

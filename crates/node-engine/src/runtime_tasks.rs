//! Node 进程生命周期内的运行任务阶段、Worker、速度与最近失败快照。

use std::{
    collections::{BTreeMap, VecDeque},
    sync::{Arc, RwLock},
    time::{Duration, Instant},
};

use dedup_core::MachineId;
use dedup_protocol::{MAX_RUNTIME_FAILURES, proto};
use thiserror::Error;
use tokio::sync::broadcast;
use uuid::Uuid;

const SPEED_WINDOW: Duration = Duration::from_secs(10);

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
    /// Node 重启后为未完成持久任务创建的临时恢复包装。
    Recovery,
    /// 扫描与一筛。
    Scan,
    /// 本地分析。
    LocalAnalysis,
    /// 二筛计算。
    Stage2,
    /// 删除。
    Delete,
}

impl RuntimeTaskKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Recovery => "recovery",
            Self::Scan => "scan",
            Self::LocalAnalysis => "local_analysis",
            Self::Stage2 => "stage2",
            Self::Delete => "delete",
        }
    }
}

/// 固定英文 ID 与中文显示名的运行阶段。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimeStage {
    /// 重启后校验并重新排队未完成持久任务。
    RecoveryValidate,
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
            Self::RecoveryValidate => "recovery_validate",
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
            Self::RecoveryValidate => "恢复与校验",
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
        let task_id = Uuid::new_v4().to_string();
        self.inner.tasks.write().expect("runtime registry lock poisoned").insert(
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
                stages: BTreeMap::new(),
                workers: BTreeMap::new(),
                failures: VecDeque::new(),
            },
        );
        RuntimeTaskReporter {
            registry: self.clone(),
            task_id,
        }
    }

    /// 为一个未完成持久任务创建全新恢复详情，不复用 SQLite 任务 ID 或旧历史。
    pub async fn begin_recovery(
        &self,
        machine_id: MachineId,
        title: impl Into<String>,
        pending_items: u64,
    ) -> RuntimeTaskReporter {
        let reporter = self
            .begin(RuntimeTaskKind::Recovery, machine_id, title)
            .await;
        let _ = reporter.update_overall(0, Some(pending_items), 0, 0).await;
        let _ = reporter
            .update_stage(RuntimeStageUpdate::running(
                RuntimeStage::RecoveryValidate,
                RuntimeProgressUnit::Items,
                0,
                Some(pending_items),
            ))
            .await;
        reporter
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
}

impl Default for RuntimeTaskRegistry {
    fn default() -> Self {
        Self::new()
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
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_completed = completed;
        task.overall_total = total;
        task.overall_failed = failed;
        task.overall_skipped = skipped;
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
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_completed = completed;
        task.overall_total = total;
        task.overall_failed = failed;
        task.overall_skipped = skipped;
        Ok(())
    }

    /// 在单 SQLite writer 成功/失败终态边界实时推进总体计数。
    pub fn advance_overall_nowait(
        &self,
        completed: u64,
        failed: u64,
        skipped: u64,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_completed = task.overall_completed.saturating_add(completed);
        task.overall_failed = task.overall_failed.saturating_add(failed);
        task.overall_skipped = task.overall_skipped.saturating_add(skipped);
        Ok(())
    }

    /// 枚举 channel 关闭时立即冻结扫描文件/字节总量，不等待读取或 Worker。
    pub fn freeze_scan_totals_nowait(
        &self,
        files: u64,
        bytes: u64,
    ) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.overall_total = Some(files);
        for (kind, total) in [
            (RuntimeStage::Enumerate, files),
            (RuntimeStage::CacheLookup, files),
            (RuntimeStage::ReadMd5, bytes),
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
        task.update_stage(RuntimeStageUpdate {
            stage: RuntimeStage::Enumerate,
            state: proto::RuntimeStageState::RuntimeStageCompleted,
            unit: RuntimeProgressUnit::Files,
            completed,
            total: Some(files),
            failed,
            skipped,
        }, now)
    }

    /// 更新一个固定阶段。
    pub async fn update_stage(&self, update: RuntimeStageUpdate) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.update_stage(update, now)
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
        active_task(&mut tasks, &self.task_id)?.update_stage(update, now)
    }

    /// 在真实成功读取块后增加阶段 completed。
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
        task.update_stage(update, now)
    }

    /// 以当前 completed 冻结阶段总数并进入终态。
    pub fn finish_stage_nowait(
        &self,
        stage_kind: RuntimeStage,
        state: proto::RuntimeStageState,
        total: Option<u64>,
    ) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        let stage = task.stages.entry(stage_kind).or_default();
        let (unit, completed, failed, skipped) =
            (stage.unit, stage.completed, stage.failed, stage.skipped);
        task.update_stage(RuntimeStageUpdate {
            stage: stage_kind,
            state,
            unit,
            completed,
            total,
            failed,
            skipped,
        }, now)
    }

    /// 在同步 Store 终态边界追加最近失败。
    pub fn record_failure_nowait(
        &self,
        failure: RuntimeFailureUpdate,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.failures.push_back(failure);
        while task.failures.len() > MAX_RUNTIME_FAILURES {
            task.failures.pop_front();
        }
        Ok(())
    }

    /// UPSERT 一个 Worker slot。
    pub async fn update_worker(&self, worker: RuntimeWorkerUpdate) -> Result<(), RuntimeTaskError> {
        self.worker_started(worker).await
    }

    /// 在真实 Pool Started 边界更新 slot，不重置累计完成数/速度样本。
    pub async fn worker_started(&self, worker: RuntimeWorkerUpdate) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.workers.entry(worker.slot).or_default().started(worker, now);
        Ok(())
    }

    /// 在真实 Pool terminal event 边界给 slot 完成文件数加一并更新 10 秒速度。
    pub async fn worker_completed(&self, slot: u32) -> Result<(), RuntimeTaskError> {
        let now = self.registry.inner.clock.now();
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.workers.entry(slot).or_default().completed(now);
        Ok(())
    }

    /// 追加最近失败并只保留末尾 20 条。
    pub async fn record_failure(
        &self,
        failure: RuntimeFailureUpdate,
    ) -> Result<(), RuntimeTaskError> {
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
        let task = active_task(&mut tasks, &self.task_id)?;
        task.failures.push_back(failure);
        while task.failures.len() > MAX_RUNTIME_FAILURES {
            task.failures.pop_front();
        }
        Ok(())
    }

    /// 进入一次不可逆终态并广播一次。
    pub async fn finish(&self, state: RuntimeTaskState) -> Result<(), RuntimeTaskError> {
        if !state.is_terminal() {
            return Err(RuntimeTaskError::NotTerminal);
        }
        let mut tasks = self.registry.inner.tasks.write().expect("runtime registry lock poisoned");
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
        task.state = state;
        drop(tasks);
        let _ = self.registry.inner.events.send(proto::RuntimeTaskChanged {
            runtime_task_id: self.task_id.clone(),
            state: state.as_str().into(),
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
    stages: BTreeMap<RuntimeStage, StageEntry>,
    workers: BTreeMap<u32, WorkerEntry>,
    failures: VecDeque<RuntimeFailureUpdate>,
}

impl TaskEntry {
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
            workers: self
                .workers
                .values()
                .map(WorkerEntry::snapshot)
                .collect(),
            failures: self
                .failures
                .iter()
                .map(|failure| proto::RuntimeFailureDetails {
                    stage_id: failure.stage.id().into(),
                    display_path: failure.display_path.clone(),
                    message: failure.message.clone(),
                })
                .collect(),
        }
    }
}

#[derive(Default)]
struct WorkerEntry {
    slot: u32,
    process_id: Option<u32>,
    stage: Option<RuntimeStage>,
    display_path: String,
    physical_disk_id: String,
    completed_files: u64,
    speed_per_second: f64,
    samples: VecDeque<(Duration, u64)>,
}

impl WorkerEntry {
    fn started(&mut self, update: RuntimeWorkerUpdate, now: Duration) {
        self.slot = update.slot;
        self.process_id = update.process_id;
        self.stage = Some(update.stage);
        self.display_path = update.display_path;
        self.physical_disk_id = update.physical_disk_id;
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
            stage_id: self.stage.map_or_else(|| "idle".into(), |stage| stage.id().into()),
            display_path: self.display_path.clone(),
            physical_disk_id: self.physical_disk_id.clone(),
            completed_files: self.completed_files,
            speed_per_second: self.speed_per_second,
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
        let elapsed = self
            .started_at
            .map(|start| self.ended_at.unwrap_or(now).saturating_sub(start))
            .unwrap_or_default();
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

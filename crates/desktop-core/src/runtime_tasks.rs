//! Desktop 进程内跨机器分析、同步和删除运行详情 registry。

use std::{
    collections::BTreeMap,
    sync::{Arc, RwLock},
};

use dedup_core::MachineId;
use dedup_protocol::proto;
use thiserror::Error;
use uuid::Uuid;

use crate::{
    analysis::CrossPollReport,
    central::CentralAnalysisStatus,
    sync::{SyncPhase, SyncProgress},
};

/// 一个固定 Desktop 阶段的稳定协议字段。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RuntimeStageSpec {
    /// 稳定英文 ID。
    pub id: &'static str,
    /// 用户可读中文名称。
    pub display_name: &'static str,
    /// 进度单位。
    pub unit: &'static str,
}

/// 跨机器分析七阶段。
pub const CROSS_ANALYSIS_STAGES: [RuntimeStageSpec; 7] = [
    RuntimeStageSpec {
        id: "collect_node_inputs",
        display_name: "收集节点输入",
        unit: "nodes",
    },
    RuntimeStageSpec {
        id: "freeze_inputs",
        display_name: "冻结输入",
        unit: "files",
    },
    RuntimeStageSpec {
        id: "stage1_screening",
        display_name: "一筛",
        unit: "candidate_pairs",
    },
    RuntimeStageSpec {
        id: "dispatch_stage2",
        display_name: "分发二筛",
        unit: "items",
    },
    RuntimeStageSpec {
        id: "wait_nodes",
        display_name: "等待节点",
        unit: "nodes",
    },
    RuntimeStageSpec {
        id: "evaluate_stage2",
        display_name: "二筛判定",
        unit: "candidate_pairs",
    },
    RuntimeStageSpec {
        id: "cluster_save",
        display_name: "聚类与保存",
        unit: "candidate_pairs",
    },
];

/// Desktop 同步四阶段，直接对应 `SyncProgress` 的四种 phase。
pub const SYNC_STAGES: [RuntimeStageSpec; 4] = [
    RuntimeStageSpec {
        id: "acknowledging",
        display_name: "确认高水位",
        unit: "changes",
    },
    RuntimeStageSpec {
        id: "incremental",
        display_name: "增量同步",
        unit: "changes",
    },
    RuntimeStageSpec {
        id: "snapshot",
        display_name: "完整快照",
        unit: "pages",
    },
    RuntimeStageSpec {
        id: "caught_up",
        display_name: "同步追平",
        unit: "changes",
    },
];

/// Desktop 观察删除确认和执行的四阶段。
pub const DELETE_STAGES: [RuntimeStageSpec; 4] = [
    RuntimeStageSpec {
        id: "revalidate_selection",
        display_name: "复验选择",
        unit: "delete_items",
    },
    RuntimeStageSpec {
        id: "dispatch_nodes",
        display_name: "按节点分发",
        unit: "nodes",
    },
    RuntimeStageSpec {
        id: "delete_items",
        display_name: "删除项目",
        unit: "delete_items",
    },
    RuntimeStageSpec {
        id: "summarize",
        display_name: "汇总结果",
        unit: "delete_items",
    },
];

/// 统一任务归属。
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum RuntimeTaskOwner {
    /// 节点进程任务，索引来自当前 Desktop 配置。
    Node {
        /// 当前 Desktop 配置中的节点索引。
        node_index: usize,
    },
    /// Desktop 自己拥有的跨机器、同步或删除任务。
    Desktop,
}

/// 跨 Node/Desktop 列表使用的稳定键。
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct RuntimeTaskKey {
    /// 所有者。
    pub owner: RuntimeTaskOwner,
    /// 所有者内部稳定 ID。
    pub id: String,
}

/// Desktop 运行任务类别。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DesktopRuntimeTaskKind {
    /// 由 Node registry 提供的投影。
    Node,
    /// 跨机器分析。
    CrossAnalysis,
    /// 单机器同步。
    Sync,
    /// 删除确认与执行。
    Delete,
}

/// 任务中心需要固定展示的三类计算任务。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskKindView {
    /// 枚举、缓存查询和基础特征计算。
    BaseCompute,
    /// 候选生成、二次派发和精准判重。
    DuplicateList,
    /// 二次缓存查询和二次特征计算。
    Stage2Compute,
    /// 同步、删除或兼容旧任务。
    Other,
}

impl TaskKindView {
    /// 从 Node 稳定任务类别解析任务中心分类。
    pub fn from_node_kind(kind: &str) -> Self {
        match kind {
            "base_compute" => Self::BaseCompute,
            "duplicate_list" => Self::DuplicateList,
            "stage2_compute" => Self::Stage2Compute,
            _ => Self::Other,
        }
    }

    /// 返回三类计算任务的固定中文标题；其他任务沿用后端标题。
    pub const fn title(self) -> Option<&'static str> {
        match self {
            Self::BaseCompute => Some("基础计算"),
            Self::DuplicateList => Some("重复文件清单"),
            Self::Stage2Compute => Some("二次特征计算"),
            Self::Other => None,
        }
    }
}

/// 任务整体终态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DesktopRuntimeTaskState {
    /// 正在运行。
    Running,
    /// 成功完成。
    Completed,
    /// 失败结束。
    Failed,
    /// 取消结束。
    Cancelled,
}

impl DesktopRuntimeTaskState {
    const fn is_terminal(self) -> bool {
        !matches!(self, Self::Running)
    }
}

/// 阶段状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RuntimeStageState {
    /// 等待上游。
    Waiting,
    /// 正在运行。
    Running,
    /// 成功完成。
    Completed,
    /// 失败结束。
    Failed,
    /// 未执行。
    Skipped,
}

impl RuntimeStageState {
    /// 是否为不可逆终态。
    pub const fn is_terminal(self) -> bool {
        matches!(self, Self::Completed | Self::Failed | Self::Skipped)
    }
}

/// 一个阶段的临时快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RuntimeStageSnapshot {
    /// 稳定阶段 ID。
    pub stage_id: String,
    /// 中文显示名。
    pub display_name: String,
    /// 当前状态。
    pub state: RuntimeStageState,
    /// 进度单位。
    pub unit: String,
    /// 成功完成数。
    pub completed: u64,
    /// 已知总数；None 表示未知。
    pub total: Option<u64>,
    /// 失败数。
    pub failed: u64,
    /// 跳过数。
    pub skipped: u64,
}

/// 最近失败。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RuntimeFailureSnapshot {
    /// 所属阶段。
    pub stage_id: String,
    /// 可选路径。
    pub display_path: String,
    /// 失败文案。
    pub message: String,
}

/// Node/Desktop 统一任务摘要和详情。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RuntimeTaskSnapshot {
    /// 统一键。
    pub key: RuntimeTaskKey,
    /// 真实握手机器 ID；Desktop 跨机器任务可有多个。
    pub machine_ids: Vec<String>,
    /// 类别。
    pub kind: DesktopRuntimeTaskKind,
    /// 标题。
    pub title: String,
    /// 整体状态。
    pub state: DesktopRuntimeTaskState,
    /// 成功总数。
    pub overall_completed: u64,
    /// 已知总体数量。
    pub overall_total: Option<u64>,
    /// 总体失败数。
    pub overall_failed: u64,
    /// 总体跳过数。
    pub overall_skipped: u64,
    /// 固定阶段。
    pub stages: Vec<RuntimeStageSnapshot>,
    /// 最近失败。
    pub failures: Vec<RuntimeFailureSnapshot>,
}

/// registry 更新错误。
#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum DesktopRuntimeTaskError {
    /// 任务不存在。
    #[error("Desktop 运行任务不存在")]
    Missing,
    /// 任务已经终态。
    #[error("Desktop 运行任务已经终态")]
    Terminal,
}

#[derive(Default)]
struct RegistryState {
    tasks: BTreeMap<RuntimeTaskKey, RuntimeTaskSnapshot>,
    active_sync: BTreeMap<String, RuntimeTaskKey>,
}

/// Desktop 进程生命周期内唯一临时 registry。
#[derive(Clone, Default)]
pub struct DesktopRuntimeTaskRegistry {
    inner: Arc<RwLock<RegistryState>>,
}

impl DesktopRuntimeTaskRegistry {
    /// 创建空 registry；不读取任何持久任务。
    pub fn new() -> Self {
        Self::default()
    }

    /// 稳定顺序列出全部 Desktop-owned 任务。
    pub fn list(&self) -> Vec<RuntimeTaskSnapshot> {
        self.inner.read().unwrap().tasks.values().cloned().collect()
    }

    /// 返回完整临时快照。
    pub fn snapshot(&self) -> Vec<RuntimeTaskSnapshot> {
        self.list()
    }

    /// 按统一键读取详情。
    pub fn details(&self, key: &RuntimeTaskKey) -> Option<RuntimeTaskSnapshot> {
        self.inner.read().unwrap().tasks.get(key).cloned()
    }

    /// 开始跨机器分析任务。
    pub fn begin_cross_analysis(
        &self,
        id: impl Into<String>,
        machines: &[MachineId],
        title: impl Into<String>,
    ) -> DesktopRuntimeTaskReporter {
        self.begin(
            RuntimeTaskKey {
                owner: RuntimeTaskOwner::Desktop,
                id: id.into(),
            },
            DesktopRuntimeTaskKind::CrossAnalysis,
            machines,
            title,
            &CROSS_ANALYSIS_STAGES,
            None,
        )
    }

    /// 开始删除观察任务；不会创建任何删除命令或选择项。
    pub fn begin_delete(
        &self,
        id: impl Into<String>,
        machines: &[MachineId],
        title: impl Into<String>,
        total_items: u64,
    ) -> DesktopRuntimeTaskReporter {
        self.begin(
            RuntimeTaskKey {
                owner: RuntimeTaskOwner::Desktop,
                id: id.into(),
            },
            DesktopRuntimeTaskKind::Delete,
            machines,
            title,
            &DELETE_STAGES,
            Some(total_items),
        )
    }

    /// 为同一机器合并活动同步触发；上一轮终态后创建新任务。
    pub fn begin_or_merge_sync(
        &self,
        machine: &MachineId,
        title: impl Into<String>,
    ) -> DesktopRuntimeTaskReporter {
        let machine_text = machine.as_str().to_owned();
        let mut state = self.inner.write().unwrap();
        if let Some(key) = state.active_sync.get(&machine_text)
            && state
                .tasks
                .get(key)
                .is_some_and(|task| task.state == DesktopRuntimeTaskState::Running)
        {
            return DesktopRuntimeTaskReporter {
                registry: self.clone(),
                key: key.clone(),
            };
        }
        let key = RuntimeTaskKey {
            owner: RuntimeTaskOwner::Desktop,
            id: Uuid::now_v7().to_string(),
        };
        let task = task_snapshot(
            key.clone(),
            DesktopRuntimeTaskKind::Sync,
            std::slice::from_ref(machine),
            title.into(),
            &SYNC_STAGES,
            None,
        );
        state.tasks.insert(key.clone(), task);
        state.active_sync.insert(machine_text, key.clone());
        DesktopRuntimeTaskReporter {
            registry: self.clone(),
            key,
        }
    }

    /// 把 Node summary 转成统一键；机器身份始终使用当前握手值。
    pub fn node_snapshot(
        node_index: usize,
        handshake_machine: &MachineId,
        summary: proto::RuntimeTaskSummary,
    ) -> RuntimeTaskSnapshot {
        let title = TaskKindView::from_node_kind(&summary.task_kind)
            .title()
            .map_or(summary.title, str::to_owned);
        RuntimeTaskSnapshot {
            key: RuntimeTaskKey {
                owner: RuntimeTaskOwner::Node { node_index },
                id: summary.runtime_task_id,
            },
            machine_ids: vec![handshake_machine.as_str().to_owned()],
            kind: DesktopRuntimeTaskKind::Node,
            title,
            state: task_state_from_text(&summary.state),
            overall_completed: summary.overall_completed,
            overall_total: summary.overall_total_known.then_some(summary.overall_total),
            overall_failed: summary.overall_failed,
            overall_skipped: summary.overall_skipped,
            stages: Vec::new(),
            failures: Vec::new(),
        }
    }

    fn begin(
        &self,
        key: RuntimeTaskKey,
        kind: DesktopRuntimeTaskKind,
        machines: &[MachineId],
        title: impl Into<String>,
        specs: &[RuntimeStageSpec],
        total: Option<u64>,
    ) -> DesktopRuntimeTaskReporter {
        let task = task_snapshot(key.clone(), kind, machines, title.into(), specs, total);
        self.inner
            .write()
            .unwrap()
            .tasks
            .entry(key.clone())
            .or_insert(task);
        DesktopRuntimeTaskReporter {
            registry: self.clone(),
            key,
        }
    }
}

/// 一个 Desktop 任务的更新句柄。
#[derive(Clone)]
pub struct DesktopRuntimeTaskReporter {
    registry: DesktopRuntimeTaskRegistry,
    key: RuntimeTaskKey,
}

impl DesktopRuntimeTaskReporter {
    /// 返回统一任务键。
    pub const fn key(&self) -> &RuntimeTaskKey {
        &self.key
    }

    /// 应用跨机器 coordinator 的真实 poll 摘要。
    pub fn update_cross_poll(&self, report: &CrossPollReport, node_count: usize) {
        crate::diagnostics::record_warning(
            self.with_task(|task| update_cross_task(task, report, node_count)),
            "desktop_runtime_tasks",
            "update_cross_poll",
        );
    }

    /// 应用 SyncEngine 的真实阶段/计数回调。
    pub fn update_sync_progress(&self, progress: SyncProgress) {
        crate::diagnostics::record_warning(
            self.with_task(|task| update_sync_task(task, progress)),
            "desktop_runtime_tasks",
            "update_sync_progress",
        );
    }

    /// 删除确认摘要成功生成后完成复验阶段。
    pub fn mark_delete_prepared(&self) {
        crate::diagnostics::record_warning(
            self.with_task(|task| {
                let total = task.overall_total;
                set_stage(
                    task,
                    "revalidate_selection",
                    RuntimeStageState::Completed,
                    total.unwrap_or(0),
                    total,
                    0,
                    0,
                );
            }),
            "desktop_runtime_tasks",
            "mark_delete_prepared",
        );
    }

    /// 观察既有删除命令返回并在事务完成后发布结果。
    pub fn finish_delete_results(&self, items: &[proto::DeleteItem]) {
        crate::diagnostics::record_warning(
            self.with_task(|task| {
                let mut machines = task.machine_ids.len() as u64;
                if machines == 0 {
                    machines = 1;
                }
                set_stage(
                    task,
                    "dispatch_nodes",
                    RuntimeStageState::Completed,
                    machines,
                    Some(machines),
                    0,
                    0,
                );
                let completed = items
                    .iter()
                    .filter(|item| matches!(item.outcome.as_str(), "deleted" | "recycled"))
                    .count() as u64;
                let failed = items.iter().filter(|item| item.outcome == "failed").count() as u64;
                let skipped = items
                    .iter()
                    .filter(|item| item.outcome == "skipped")
                    .count() as u64;
                let total = items.len() as u64;
                set_stage(
                    task,
                    "delete_items",
                    if failed == 0 {
                        RuntimeStageState::Completed
                    } else {
                        RuntimeStageState::Failed
                    },
                    completed,
                    Some(total),
                    failed,
                    skipped,
                );
                set_stage(
                    task,
                    "summarize",
                    RuntimeStageState::Completed,
                    total,
                    Some(total),
                    0,
                    0,
                );
                task.overall_completed = completed;
                task.overall_total = Some(total);
                task.overall_failed = failed;
                task.overall_skipped = skipped;
                for item in items.iter().filter(|item| item.outcome == "failed") {
                    task.failures.push(RuntimeFailureSnapshot {
                        stage_id: "delete_items".into(),
                        display_path: item
                            .location
                            .as_ref()
                            .map_or_else(String::new, |location| location.normalized_path.clone()),
                        message: item.message.clone(),
                    });
                }
            }),
            "desktop_runtime_tasks",
            "finish_delete_results",
        );
    }

    /// 在既有命令返回错误后记录一条运行失败，不改变业务命令或重试语义。
    pub fn record_failure(
        &self,
        stage_id: impl Into<String>,
        display_path: impl Into<String>,
        message: impl Into<String>,
    ) {
        crate::diagnostics::record_warning(
            self.with_task(|task| {
                task.failures.push(RuntimeFailureSnapshot {
                    stage_id: stage_id.into(),
                    display_path: display_path.into(),
                    message: message.into(),
                });
                task.overall_failed = task.overall_failed.saturating_add(1);
            }),
            "desktop_runtime_tasks",
            "record_failure",
        );
    }

    /// 进入不可逆终态。
    pub fn finish(&self, state: DesktopRuntimeTaskState) -> Result<(), DesktopRuntimeTaskError> {
        if !state.is_terminal() {
            return Err(DesktopRuntimeTaskError::Terminal);
        }
        let mut registry = self.registry.inner.write().unwrap();
        let task = registry
            .tasks
            .get_mut(&self.key)
            .ok_or(DesktopRuntimeTaskError::Missing)?;
        if task.state.is_terminal() {
            return Err(DesktopRuntimeTaskError::Terminal);
        }
        task.state = state;
        let stage_state = match state {
            DesktopRuntimeTaskState::Completed => RuntimeStageState::Completed,
            DesktopRuntimeTaskState::Failed => RuntimeStageState::Failed,
            DesktopRuntimeTaskState::Cancelled => RuntimeStageState::Skipped,
            DesktopRuntimeTaskState::Running => unreachable!(),
        };
        for stage in &mut task.stages {
            if !stage.state.is_terminal() {
                stage.state = stage_state;
            }
        }
        let sync_machines = if task.kind == DesktopRuntimeTaskKind::Sync {
            task.machine_ids.clone()
        } else {
            Vec::new()
        };
        for machine in sync_machines {
            if registry.active_sync.get(&machine) == Some(&self.key) {
                registry.active_sync.remove(&machine);
            }
        }
        Ok(())
    }

    fn with_task(
        &self,
        update: impl FnOnce(&mut RuntimeTaskSnapshot),
    ) -> Result<(), DesktopRuntimeTaskError> {
        let mut registry = self.registry.inner.write().unwrap();
        let task = registry
            .tasks
            .get_mut(&self.key)
            .ok_or(DesktopRuntimeTaskError::Missing)?;
        if task.state.is_terminal() {
            return Err(DesktopRuntimeTaskError::Terminal);
        }
        update(task);
        Ok(())
    }
}

fn task_snapshot(
    key: RuntimeTaskKey,
    kind: DesktopRuntimeTaskKind,
    machines: &[MachineId],
    title: String,
    specs: &[RuntimeStageSpec],
    total: Option<u64>,
) -> RuntimeTaskSnapshot {
    let mut machine_ids = machines
        .iter()
        .map(|machine| machine.as_str().to_owned())
        .collect::<Vec<_>>();
    machine_ids.sort();
    machine_ids.dedup();
    RuntimeTaskSnapshot {
        key,
        machine_ids,
        kind,
        title,
        state: DesktopRuntimeTaskState::Running,
        overall_completed: 0,
        overall_total: total,
        overall_failed: 0,
        overall_skipped: 0,
        stages: specs
            .iter()
            .map(|spec| RuntimeStageSnapshot {
                stage_id: spec.id.into(),
                display_name: spec.display_name.into(),
                state: RuntimeStageState::Waiting,
                unit: spec.unit.into(),
                completed: 0,
                total: None,
                failed: 0,
                skipped: 0,
            })
            .collect(),
        failures: Vec::new(),
    }
}

fn set_stage(
    task: &mut RuntimeTaskSnapshot,
    id: &str,
    state: RuntimeStageState,
    completed: u64,
    total: Option<u64>,
    failed: u64,
    skipped: u64,
) {
    if let Some(stage) = task.stages.iter_mut().find(|stage| stage.stage_id == id) {
        stage.state = state;
        stage.completed = completed;
        stage.total = total;
        stage.failed = failed;
        stage.skipped = skipped;
    }
}

fn complete_before(task: &mut RuntimeTaskSnapshot, index: usize) {
    for stage in task.stages.iter_mut().take(index) {
        if !stage.state.is_terminal() {
            stage.state = RuntimeStageState::Completed;
        }
    }
}

fn update_cross_task(task: &mut RuntimeTaskSnapshot, report: &CrossPollReport, node_count: usize) {
    let candidate_count = report.candidate_count as u64;
    let unresolved_candidates = report.unresolved_candidates as u64;
    let phase2_task_count = report.phase2_task_count as u64;
    let node_count = node_count as u64;
    task.overall_total = Some(candidate_count);
    let (index, current_state) = match report.status {
        CentralAnalysisStatus::CollectingStage1 => (0, RuntimeStageState::Running),
        CentralAnalysisStatus::Stage1Synced => (1, RuntimeStageState::Running),
        CentralAnalysisStatus::Screening => (2, RuntimeStageState::Running),
        CentralAnalysisStatus::Phase2Dispatched => (4, RuntimeStageState::Running),
        CentralAnalysisStatus::Phase2Synced => (5, RuntimeStageState::Running),
        CentralAnalysisStatus::Finalizing => (6, RuntimeStageState::Running),
        CentralAnalysisStatus::Completed => (7, RuntimeStageState::Completed),
        CentralAnalysisStatus::Partial => (5, RuntimeStageState::Failed),
        CentralAnalysisStatus::Cancelled => (0, RuntimeStageState::Skipped),
    };
    complete_before(task, index);
    if index < task.stages.len() {
        task.stages[index].state = current_state;
    }
    set_stage(
        task,
        "collect_node_inputs",
        if index > 0 {
            RuntimeStageState::Completed
        } else {
            current_state
        },
        node_count,
        Some(node_count),
        0,
        0,
    );
    if index >= 2 {
        set_stage(
            task,
            "stage1_screening",
            if index > 2 {
                RuntimeStageState::Completed
            } else {
                current_state
            },
            candidate_count,
            Some(candidate_count),
            0,
            0,
        );
    }
    if index >= 4 {
        set_stage(
            task,
            "dispatch_stage2",
            RuntimeStageState::Completed,
            phase2_task_count,
            Some(phase2_task_count),
            0,
            0,
        );
        set_stage(
            task,
            "wait_nodes",
            if index > 4 {
                RuntimeStageState::Completed
            } else {
                current_state
            },
            node_count.saturating_sub(phase2_task_count.min(node_count)),
            Some(node_count),
            0,
            0,
        );
    }
    if matches!(report.status, CentralAnalysisStatus::Completed) {
        for stage in &mut task.stages {
            stage.state = RuntimeStageState::Completed;
        }
        task.overall_completed = candidate_count;
    } else if matches!(report.status, CentralAnalysisStatus::Partial) {
        task.overall_failed = unresolved_candidates;
    }
}

fn update_sync_task(task: &mut RuntimeTaskSnapshot, progress: SyncProgress) {
    let (id, completed, total) = match progress.phase {
        SyncPhase::Acknowledging => (
            "acknowledging",
            progress.committed_seq,
            Some(progress.node_high_seq),
        ),
        SyncPhase::Incremental => ("incremental", progress.change_count, None),
        SyncPhase::Snapshot => ("snapshot", progress.snapshot_page_count, None),
        SyncPhase::CaughtUp => (
            "caught_up",
            progress.committed_seq,
            Some(progress.node_high_seq),
        ),
    };
    set_stage(
        task,
        id,
        if progress.phase == SyncPhase::CaughtUp {
            RuntimeStageState::Completed
        } else {
            RuntimeStageState::Running
        },
        completed,
        total,
        0,
        0,
    );
    if progress.phase == SyncPhase::CaughtUp {
        for stage in &mut task.stages {
            stage.state = RuntimeStageState::Completed;
        }
        task.overall_completed = progress.committed_seq;
        task.overall_total = Some(progress.node_high_seq);
    }
}

fn task_state_from_text(state: &str) -> DesktopRuntimeTaskState {
    match state {
        "completed" => DesktopRuntimeTaskState::Completed,
        "failed" => DesktopRuntimeTaskState::Failed,
        "cancelled" => DesktopRuntimeTaskState::Cancelled,
        _ => DesktopRuntimeTaskState::Running,
    }
}

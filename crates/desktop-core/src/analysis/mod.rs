//! 固定高水位的跨机器两层去重编排及其纯决策组件。

use std::{
    collections::{BTreeMap, BTreeSet},
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::{AnalysisRunId, ContentKey, LocationKey, MachineId, TaskId, Thresholds};
use dedup_protocol::{ProtocolError, proto};
use thiserror::Error;

use crate::{
    central::{
        CentralAnalysisInput, CentralAnalysisNode, CentralAnalysisStatus, CentralError,
        CentralStore, PersistentStageState, Stage2DispatchWrite,
    },
    node_session::{NodeSession, SessionError},
    sync::{SyncEngine, SyncError, SyncTrigger},
};

mod dispatch;
mod finalize;
mod gate;
mod screen;
mod task;

pub use dispatch::{
    PlannedStage2Batch, PlannedStage2Item, STAGE2_BATCH_SIZE, Stage2Availability,
    plan_stage2_batches,
};
pub use finalize::build_groups;
pub use gate::{CrossTaskState, GateDecision, GateState, phase2_gate, stage_gate};
pub use screen::{CrossFeatureSet, evaluate_candidates, screen_candidates};
pub use task::{DuplicateListStage, stage_write, stage2_dispatch_stage, waiting_stages};

/// 分页读取节点冻结输入及单次二筛派发的固定上限。
const ANALYSIS_PAGE_SIZE: u32 = 1000;

/// 创建中心运行时选择的一个已连接节点和一个扫描任务。
pub struct CrossNodeSelection<'a> {
    /// 已完成 V2 Hello 的节点会话。
    pub session: &'a NodeSession,
    /// 本运行纳入的持久扫描任务。
    pub scan_task_id: TaskId,
}

impl<'a> CrossNodeSelection<'a> {
    /// 组合一个已连接节点和其扫描任务。
    pub const fn new(session: &'a NodeSession, scan_task_id: TaskId) -> Self {
        Self {
            session,
            scan_task_id,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct SelectedTask {
    machine_id: MachineId,
    task_id: TaskId,
}

/// 一次 `poll` 或显式重试后供 UI 直接展示的轻量结果。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CrossPollReport {
    /// 中心分析运行 ID。
    pub run_id: AnalysisRunId,
    /// 当前已持久状态。
    pub status: CentralAnalysisStatus,
    /// 缺少完整一筛而被跳过的唯一内容数。
    pub skipped_incomplete: usize,
    /// 当前完整候选数量。
    pub candidate_count: usize,
    /// 当前缺少任一端联合二筛的候选数量。
    pub unresolved_candidates: usize,
    /// 当前协调器正在等待的 phase2 任务数。
    pub phase2_task_count: usize,
}

/// 跨机器编排的节点、同步、协议或中心持久化错误。
#[derive(Debug, Error)]
pub enum CrossAnalysisError {
    /// 节点 TCP 会话失败。
    #[error(transparent)]
    Session(#[from] SessionError),
    /// SQLite 到 PostgreSQL 同步失败。
    #[error(transparent)]
    Sync(#[from] SyncError),
    /// PostgreSQL 中心状态或事务失败。
    #[error(transparent)]
    Central(#[from] CentralError),
    /// Protobuf 外部键无效。
    #[error(transparent)]
    Protocol(#[from] ProtocolError),
    /// 调用阶段或当前连接集合不满足编排条件。
    #[error("跨机器分析状态无效: {0}")]
    InvalidState(String),
}

/// 以 PostgreSQL 为唯一中心事实源的固定高水位跨机器分析协调器。
///
/// 本类型只保存当前管理进程需要的连接选择和缓存提示；运行状态、输入、候选、任务高水位
/// 与最终组都写 PostgreSQL。`poll` 不会在任一节点尚在计算时提前筛选。
pub struct CrossAnalysisCoordinator {
    run_id: AnalysisRunId,
    thresholds: Thresholds,
    selected: Vec<SelectedTask>,
    phase2_tasks: Vec<SelectedTask>,
    availability: Vec<Stage2Availability>,
    skipped_incomplete: usize,
    sync_engine: SyncEngine,
}

impl CrossAnalysisCoordinator {
    /// 查询所选任务初始状态，并创建 `collecting_stage1` 中心运行。
    pub async fn start(
        central: &mut CentralStore,
        selections: &[CrossNodeSelection<'_>],
        thresholds: Thresholds,
    ) -> Result<Self, CrossAnalysisError> {
        if selections.is_empty() {
            return Err(CrossAnalysisError::InvalidState(
                "至少选择一个节点扫描任务".into(),
            ));
        }
        let mut selected = Vec::new();
        let mut nodes = Vec::new();
        let mut unique = BTreeSet::new();
        for selection in selections {
            let key = (
                selection.session.machine_id().clone(),
                selection.scan_task_id,
            );
            if !unique.insert(key.clone()) {
                continue;
            }
            let task = selection.session.query_task(selection.scan_task_id).await?;
            let state = task_state(task.state)?;
            let sync_highwater = central.sync_cursor(&key.0).await?;
            selected.push(SelectedTask {
                machine_id: key.0.clone(),
                task_id: key.1,
            });
            nodes.push(CentralAnalysisNode {
                machine_id: key.0,
                task_id: key.1,
                task_highwater: task.outbox_high_seq,
                sync_highwater,
                task_status: task_state_name(state).into(),
            });
        }
        let run_id = central.create_analysis_run(&thresholds, &nodes).await?;
        for stage in waiting_stages() {
            central.save_analysis_stage(run_id, stage).await?;
        }
        Ok(Self {
            run_id,
            thresholds,
            selected,
            phase2_tasks: Vec::new(),
            availability: Vec::new(),
            skipped_incomplete: 0,
            sync_engine: SyncEngine::new(),
        })
    }

    /// 返回当前中心运行 ID，供 UI、分页和复核操作复用。
    pub const fn run_id(&self) -> AnalysisRunId {
        self.run_id
    }

    /// 从 PostgreSQL 恢复一次未完成清单任务，不依赖原 Desktop 进程仍存活。
    pub async fn resume(
        central: &CentralStore,
        run_id: AnalysisRunId,
    ) -> Result<Self, CrossAnalysisError> {
        let snapshot = central.analysis_run_snapshot(run_id).await?;
        let dispatches = central.stage2_dispatches(run_id).await?;
        let phase2_keys = dispatches
            .iter()
            .filter_map(|dispatch| {
                dispatch
                    .node_task_id
                    .map(|task_id| (dispatch.machine_id.clone(), task_id))
            })
            .collect::<BTreeSet<_>>();
        let tasks = central.analysis_node_tasks(run_id).await?;
        let selected = tasks
            .iter()
            .filter(|task| !phase2_keys.contains(&(task.machine_id.clone(), task.task_id)))
            .map(|task| SelectedTask {
                machine_id: task.machine_id.clone(),
                task_id: task.task_id,
            })
            .collect();
        let phase2_tasks = tasks
            .into_iter()
            .filter(|task| phase2_keys.contains(&(task.machine_id.clone(), task.task_id)))
            .map(|task| SelectedTask {
                machine_id: task.machine_id,
                task_id: task.task_id,
            })
            .collect();
        Ok(Self {
            run_id,
            thresholds: snapshot.thresholds,
            selected,
            phase2_tasks,
            availability: Vec::new(),
            skipped_incomplete: 0,
            sync_engine: SyncEngine::new(),
        })
    }

    /// 推进当前运行，直到遇到未满足门禁或到达 Completed/Partial。
    pub async fn poll(
        &mut self,
        central: &mut CentralStore,
        sessions: &[&NodeSession],
    ) -> Result<CrossPollReport, CrossAnalysisError> {
        loop {
            let snapshot = central.analysis_run_snapshot(self.run_id).await?;
            self.thresholds = snapshot.thresholds;
            match snapshot.status {
                CentralAnalysisStatus::CollectingStage1 => {
                    let Some(states) = self
                        .refresh_tasks(central, sessions, &self.selected.clone())
                        .await?
                    else {
                        return Ok(self.report(CentralAnalysisStatus::CollectingStage1, 0, 0));
                    };
                    if stage_gate(&states) == GateDecision::Waiting {
                        return Ok(self.report(CentralAnalysisStatus::CollectingStage1, 0, 0));
                    }
                    central
                        .set_analysis_status(self.run_id, CentralAnalysisStatus::Stage1Synced, None)
                        .await?;
                }
                CentralAnalysisStatus::Stage1Synced => {
                    let (inputs, availability) = self.collect_inputs(sessions, None).await?;
                    self.availability = availability;
                    if !snapshot.inputs_frozen {
                        central.insert_analysis_inputs(self.run_id, &inputs).await?;
                    }
                    central
                        .set_analysis_status(self.run_id, CentralAnalysisStatus::Screening, None)
                        .await?;
                }
                CentralAnalysisStatus::Screening => {
                    let stage_started = wall_clock_ms();
                    central
                        .save_analysis_stage(
                            self.run_id,
                            stage_write(
                                DuplicateListStage::BuildCandidates,
                                PersistentStageState::Running,
                                0,
                                None,
                                0,
                                Some(stage_started),
                                None,
                            ),
                        )
                        .await?;
                    if self.availability.is_empty() {
                        let frozen = central.analysis_inputs(self.run_id).await?;
                        let allowed = frozen
                            .iter()
                            .map(|input| (input.content, input.location.clone()))
                            .collect();
                        self.availability = self.collect_inputs(sessions, Some(&allowed)).await?.1;
                    }
                    let features = central.analysis_features(self.run_id).await?;
                    let (candidates, skipped) = screen_candidates(&features, &self.thresholds);
                    self.skipped_incomplete = skipped;
                    central.replace_candidates(self.run_id, &candidates).await?;
                    central
                        .save_analysis_stage(
                            self.run_id,
                            stage_write(
                                DuplicateListStage::BuildCandidates,
                                PersistentStageState::Completed,
                                candidates.len() as u64,
                                Some(candidates.len() as u64),
                                0,
                                Some(stage_started),
                                Some(wall_clock_ms()),
                            ),
                        )
                        .await?;

                    let dispatch_started = wall_clock_ms();
                    central
                        .save_analysis_stage(
                            self.run_id,
                            stage_write(
                                DuplicateListStage::DispatchStage2,
                                PersistentStageState::Running,
                                0,
                                None,
                                0,
                                Some(dispatch_started),
                                None,
                            ),
                        )
                        .await?;
                    self.restore_phase2_tasks(central).await?;
                    if self.phase2_tasks.is_empty() {
                        let online = online_machines(sessions);
                        let batches = plan_stage2_batches(
                            &candidates,
                            &features,
                            &self.availability,
                            &online,
                            STAGE2_BATCH_SIZE,
                        );
                        self.dispatch_batches(central, sessions, batches).await?;
                    }
                    if self.phase2_tasks.is_empty() {
                        central
                            .save_analysis_stage(
                                self.run_id,
                                stage_write(
                                    DuplicateListStage::DispatchStage2,
                                    PersistentStageState::Completed,
                                    0,
                                    Some(0),
                                    0,
                                    Some(dispatch_started),
                                    Some(wall_clock_ms()),
                                ),
                            )
                            .await?;
                        return self.finish(central).await;
                    }
                    self.save_dispatch_progress(central, dispatch_started)
                        .await?;
                    central
                        .set_analysis_status(
                            self.run_id,
                            CentralAnalysisStatus::Phase2Dispatched,
                            None,
                        )
                        .await?;
                    return Ok(self.report(
                        CentralAnalysisStatus::Phase2Dispatched,
                        candidates.len(),
                        candidates.len(),
                    ));
                }
                CentralAnalysisStatus::Phase2Dispatched => {
                    self.restore_phase2_tasks(central).await?;
                    let dispatch_started = wall_clock_ms();
                    let Some(states) = self
                        .refresh_tasks(central, sessions, &self.phase2_tasks.clone())
                        .await?
                    else {
                        self.save_dispatch_progress(central, dispatch_started)
                            .await?;
                        let count = central.analysis_candidates(self.run_id).await?.len();
                        return Ok(self.report(
                            CentralAnalysisStatus::Phase2Dispatched,
                            count,
                            count,
                        ));
                    };
                    self.save_dispatch_progress(central, dispatch_started)
                        .await?;
                    if phase2_gate(&states) == GateDecision::Waiting {
                        let count = central.analysis_candidates(self.run_id).await?.len();
                        return Ok(self.report(
                            CentralAnalysisStatus::Phase2Dispatched,
                            count,
                            count,
                        ));
                    }
                    return self.finish(central).await;
                }
                CentralAnalysisStatus::Completed | CentralAnalysisStatus::Partial => {
                    let candidates = central.analysis_candidates(self.run_id).await?;
                    let unresolved = candidates
                        .iter()
                        .filter(|candidate| {
                            candidate.status == crate::central::CentralCandidateStatus::Incomplete
                        })
                        .count();
                    return Ok(self.report(snapshot.status, candidates.len(), unresolved));
                }
                CentralAnalysisStatus::Cancelled => {
                    return Ok(self.report(CentralAnalysisStatus::Cancelled, 0, 0));
                }
                CentralAnalysisStatus::Phase2Synced | CentralAnalysisStatus::Finalizing => {
                    return self.finish(central).await;
                }
            }
        }
    }

    /// 从 Partial 显式重试仍不完整的内容；已通过、已拒绝和 PG 已完整内容不再派发。
    pub async fn retry_unresolved(
        &mut self,
        central: &mut CentralStore,
        sessions: &[&NodeSession],
    ) -> Result<CrossPollReport, CrossAnalysisError> {
        let snapshot = central.analysis_run_snapshot(self.run_id).await?;
        if snapshot.status != CentralAnalysisStatus::Partial {
            return Err(CrossAnalysisError::InvalidState(
                "只有 partial 运行可以显式重试二筛".into(),
            ));
        }
        self.thresholds = snapshot.thresholds;
        let features = central.analysis_features(self.run_id).await?;
        let candidates = central.analysis_candidates(self.run_id).await?;
        let (evaluated, unresolved) = evaluate_candidates(&candidates, &features, &self.thresholds);
        central.replace_candidates(self.run_id, &evaluated).await?;
        if unresolved == 0 {
            return self.finish(central).await;
        }

        let frozen = central.analysis_inputs(self.run_id).await?;
        let allowed = frozen
            .iter()
            .map(|input| (input.content, input.location.clone()))
            .collect();
        self.availability = self.collect_inputs(sessions, Some(&allowed)).await?.1;
        self.phase2_tasks.clear();
        let batches = plan_stage2_batches(
            &evaluated,
            &features,
            &self.availability,
            &online_machines(sessions),
            STAGE2_BATCH_SIZE,
        );
        let dispatch_started = wall_clock_ms();
        central
            .save_analysis_stage(
                self.run_id,
                stage_write(
                    DuplicateListStage::DispatchStage2,
                    PersistentStageState::Running,
                    0,
                    None,
                    0,
                    Some(dispatch_started),
                    None,
                ),
            )
            .await?;
        self.dispatch_batches(central, sessions, batches).await?;
        self.save_dispatch_progress(central, dispatch_started)
            .await?;
        if self.phase2_tasks.is_empty() {
            return Ok(self.report(CentralAnalysisStatus::Partial, evaluated.len(), unresolved));
        }
        central
            .set_analysis_status(self.run_id, CentralAnalysisStatus::Phase2Dispatched, None)
            .await?;
        Ok(self.report(
            CentralAnalysisStatus::Phase2Dispatched,
            evaluated.len(),
            unresolved,
        ))
    }

    async fn refresh_tasks(
        &self,
        central: &mut CentralStore,
        sessions: &[&NodeSession],
        tasks: &[SelectedTask],
    ) -> Result<Option<Vec<GateState>>, CrossAnalysisError> {
        let mut summaries = Vec::with_capacity(tasks.len());
        for task in tasks {
            let Some(session) = session_for(sessions, &task.machine_id) else {
                return Ok(None);
            };
            summaries.push((task.clone(), session.query_task(task.task_id).await?));
        }
        let mut synced = BTreeSet::new();
        for (task, _) in &summaries {
            if synced.insert(task.machine_id.clone()) {
                let session =
                    session_for(sessions, &task.machine_id).expect("查询阶段已经确认会话存在");
                self.sync_engine
                    .sync_node(session, central, SyncTrigger::Automatic)
                    .await?;
            }
        }
        let dispatches = central.stage2_dispatches(self.run_id).await?;
        let mut states = Vec::with_capacity(tasks.len());
        for (task, summary) in summaries {
            let state = task_state(summary.state)?;
            let state_name = task_state_name(state);
            let sync_highwater = central.sync_cursor(&task.machine_id).await?;
            let node = CentralAnalysisNode {
                machine_id: task.machine_id.clone(),
                task_id: task.task_id,
                task_highwater: summary.outbox_high_seq,
                sync_highwater,
                task_status: state_name.into(),
            };
            central.update_analysis_node(self.run_id, &node).await?;
            for dispatch in dispatches.iter().filter(|dispatch| {
                dispatch.machine_id == task.machine_id
                    && dispatch.node_task_id == Some(task.task_id)
            }) {
                central
                    .upsert_stage2_dispatch(
                        self.run_id,
                        Stage2DispatchWrite {
                            machine_id: dispatch.machine_id.clone(),
                            content: dispatch.content,
                            node_task_id: dispatch.node_task_id,
                            state: state_name.into(),
                            updated_at_ms: wall_clock_ms(),
                        },
                    )
                    .await?;
            }
            states.push(GateState {
                machine_id: task.machine_id,
                task_id: task.task_id,
                state,
                task_highwater: summary.outbox_high_seq,
                sync_highwater,
            });
        }
        Ok(Some(states))
    }

    async fn collect_inputs(
        &self,
        sessions: &[&NodeSession],
        allowed: Option<&BTreeSet<(ContentKey, LocationKey)>>,
    ) -> Result<(Vec<CentralAnalysisInput>, Vec<Stage2Availability>), CrossAnalysisError> {
        let mut by_machine = BTreeMap::<MachineId, Vec<TaskId>>::new();
        for selected in &self.selected {
            by_machine
                .entry(selected.machine_id.clone())
                .or_default()
                .push(selected.task_id);
        }
        let mut rows = BTreeMap::<(ContentKey, LocationKey), bool>::new();
        for (machine_id, tasks) in by_machine {
            let session = session_for(sessions, &machine_id).ok_or_else(|| {
                CrossAnalysisError::InvalidState(format!("节点 {} 当前未连接", machine_id.as_str()))
            })?;
            let mut cursor = String::new();
            loop {
                let page = session
                    .prepare_analysis_input(self.run_id, &tasks, &cursor, ANALYSIS_PAGE_SIZE)
                    .await?;
                for input in page.inputs {
                    let content: ContentKey = input
                        .content
                        .ok_or_else(|| {
                            CrossAnalysisError::InvalidState("节点分析输入缺少内容键".into())
                        })?
                        .try_into()?;
                    for location in input.locations {
                        let location: LocationKey = location.try_into()?;
                        let key = (content, location);
                        if allowed.is_none_or(|allowed| allowed.contains(&key)) {
                            rows.entry(key)
                                .and_modify(|cached| *cached |= input.stage2_complete)
                                .or_insert(input.stage2_complete);
                        }
                    }
                }
                if page.next_cursor.is_empty() {
                    break;
                }
                if page.next_cursor == cursor {
                    return Err(CrossAnalysisError::InvalidState(
                        "节点分析输入分页游标没有前进".into(),
                    ));
                }
                cursor = page.next_cursor;
            }
        }
        let inputs = rows
            .keys()
            .map(|(content, location)| CentralAnalysisInput {
                content: *content,
                location: location.clone(),
            })
            .collect();
        let availability = rows
            .into_iter()
            .map(
                |((content, location), stage2_complete)| Stage2Availability {
                    content,
                    location,
                    stage2_complete,
                },
            )
            .collect();
        Ok((inputs, availability))
    }

    async fn dispatch_batches(
        &mut self,
        central: &mut CentralStore,
        sessions: &[&NodeSession],
        batches: Vec<PlannedStage2Batch>,
    ) -> Result<(), CrossAnalysisError> {
        for batch in batches {
            let queued_at = wall_clock_ms();
            for item in &batch.items {
                central
                    .upsert_stage2_dispatch(
                        self.run_id,
                        Stage2DispatchWrite {
                            machine_id: batch.machine_id.clone(),
                            content: item.content,
                            node_task_id: None,
                            state: "queued".into(),
                            updated_at_ms: queued_at,
                        },
                    )
                    .await?;
            }
            let session = session_for(sessions, &batch.machine_id)
                .ok_or_else(|| CrossAnalysisError::InvalidState("二筛目标节点已经断开".into()))?;
            let contents = batch
                .items
                .iter()
                .map(|item| item.content)
                .collect::<Vec<_>>();
            let items = batch
                .items
                .into_iter()
                .map(|item| proto::Stage2WorkItem {
                    content: Some((&item.content).into()),
                    source: Some((&item.source).into()),
                    frame_slots: item.frame_slots,
                })
                .collect();
            let task_id = session.dispatch_stage2(self.run_id, items).await?;
            let task = SelectedTask {
                machine_id: batch.machine_id,
                task_id,
            };
            central
                .add_analysis_node_task(
                    self.run_id,
                    &CentralAnalysisNode {
                        machine_id: task.machine_id.clone(),
                        task_id,
                        task_highwater: 0,
                        sync_highwater: central.sync_cursor(&task.machine_id).await?,
                        task_status: "queued".into(),
                    },
                )
                .await?;
            for content in contents {
                central
                    .upsert_stage2_dispatch(
                        self.run_id,
                        Stage2DispatchWrite {
                            machine_id: task.machine_id.clone(),
                            content,
                            node_task_id: Some(task_id),
                            state: "queued".into(),
                            updated_at_ms: wall_clock_ms(),
                        },
                    )
                    .await?;
            }
            self.phase2_tasks.push(task);
        }
        Ok(())
    }

    async fn restore_phase2_tasks(
        &mut self,
        central: &CentralStore,
    ) -> Result<(), CrossAnalysisError> {
        if !self.phase2_tasks.is_empty() {
            return Ok(());
        }
        let dispatches = central.stage2_dispatches(self.run_id).await?;
        let task_keys = dispatches
            .iter()
            .filter_map(|dispatch| {
                dispatch
                    .node_task_id
                    .map(|task_id| (dispatch.machine_id.clone(), task_id))
            })
            .collect::<BTreeSet<_>>();
        if !task_keys.is_empty() {
            self.phase2_tasks = task_keys
                .into_iter()
                .map(|(machine_id, task_id)| SelectedTask {
                    machine_id,
                    task_id,
                })
                .collect();
            return Ok(());
        }
        let selected = self
            .selected
            .iter()
            .map(|task| task.task_id)
            .collect::<BTreeSet<_>>();
        self.phase2_tasks = central
            .analysis_node_tasks(self.run_id)
            .await?
            .into_iter()
            .filter(|task| !selected.contains(&task.task_id))
            .map(|task| SelectedTask {
                machine_id: task.machine_id,
                task_id: task.task_id,
            })
            .collect();
        Ok(())
    }

    /// 以每个内容项的持久派发状态更新二次特征阶段，而不是按批次数计数。
    async fn save_dispatch_progress(
        &self,
        central: &CentralStore,
        started_at_ms: u64,
    ) -> Result<(), CrossAnalysisError> {
        let states = central
            .stage2_dispatches(self.run_id)
            .await?
            .into_iter()
            .map(|dispatch| dispatch.state)
            .collect::<Vec<_>>();
        central
            .save_analysis_stage(
                self.run_id,
                stage2_dispatch_stage(&states, started_at_ms, wall_clock_ms()),
            )
            .await?;
        Ok(())
    }

    async fn finish(
        &mut self,
        central: &mut CentralStore,
    ) -> Result<CrossPollReport, CrossAnalysisError> {
        let final_started = wall_clock_ms();
        central
            .save_analysis_stage(
                self.run_id,
                stage_write(
                    DuplicateListStage::FinalCompare,
                    PersistentStageState::Running,
                    0,
                    None,
                    0,
                    Some(final_started),
                    None,
                ),
            )
            .await?;
        let features = central.analysis_features(self.run_id).await?;
        let candidates = central.analysis_candidates(self.run_id).await?;
        let (evaluated, unresolved) = evaluate_candidates(&candidates, &features, &self.thresholds);
        central.replace_candidates(self.run_id, &evaluated).await?;
        if unresolved > 0 {
            central
                .save_analysis_stage(
                    self.run_id,
                    stage_write(
                        DuplicateListStage::FinalCompare,
                        PersistentStageState::Failed,
                        evaluated.len().saturating_sub(unresolved) as u64,
                        Some(evaluated.len() as u64),
                        unresolved as u64,
                        Some(final_started),
                        Some(wall_clock_ms()),
                    ),
                )
                .await?;
            central
                .set_analysis_status(self.run_id, CentralAnalysisStatus::Partial, None)
                .await?;
            return Ok(self.report(CentralAnalysisStatus::Partial, evaluated.len(), unresolved));
        }
        central
            .set_analysis_status(self.run_id, CentralAnalysisStatus::Phase2Synced, None)
            .await?;
        central
            .set_analysis_status(self.run_id, CentralAnalysisStatus::Finalizing, None)
            .await?;
        let inputs = central.analysis_inputs(self.run_id).await?;
        let groups = build_groups(&inputs, &evaluated);
        central.replace_groups(self.run_id, &groups).await?;
        central
            .save_analysis_stage(
                self.run_id,
                stage_write(
                    DuplicateListStage::FinalCompare,
                    PersistentStageState::Completed,
                    evaluated.len() as u64,
                    Some(evaluated.len() as u64),
                    0,
                    Some(final_started),
                    Some(wall_clock_ms()),
                ),
            )
            .await?;
        central
            .set_analysis_status(self.run_id, CentralAnalysisStatus::Completed, None)
            .await?;
        Ok(self.report(CentralAnalysisStatus::Completed, evaluated.len(), 0))
    }

    const fn report(
        &self,
        status: CentralAnalysisStatus,
        candidate_count: usize,
        unresolved_candidates: usize,
    ) -> CrossPollReport {
        CrossPollReport {
            run_id: self.run_id,
            status,
            skipped_incomplete: self.skipped_incomplete,
            candidate_count,
            unresolved_candidates,
            phase2_task_count: self.phase2_tasks.len(),
        }
    }
}

fn session_for<'a>(
    sessions: &'a [&'a NodeSession],
    machine_id: &MachineId,
) -> Option<&'a NodeSession> {
    sessions
        .iter()
        .copied()
        .find(|session| session.machine_id() == machine_id)
}

fn online_machines(sessions: &[&NodeSession]) -> BTreeSet<MachineId> {
    sessions
        .iter()
        .map(|session| session.machine_id().clone())
        .collect()
}

/// 返回当前墙钟毫秒时间，供跨进程恢复后的阶段计时继续使用。
fn wall_clock_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

fn task_state(value: i32) -> Result<CrossTaskState, CrossAnalysisError> {
    match proto::TaskState::try_from(value).unwrap_or(proto::TaskState::Unspecified) {
        proto::TaskState::TaskQueued => Ok(CrossTaskState::Queued),
        proto::TaskState::TaskRunning => Ok(CrossTaskState::Running),
        proto::TaskState::TaskCompleted => Ok(CrossTaskState::Completed),
        proto::TaskState::TaskFailed => Ok(CrossTaskState::Failed),
        proto::TaskState::TaskCancelled => Ok(CrossTaskState::Cancelled),
        proto::TaskState::Unspecified => Err(CrossAnalysisError::InvalidState(
            "节点返回未指定任务状态".into(),
        )),
    }
}

const fn task_state_name(state: CrossTaskState) -> &'static str {
    match state {
        CrossTaskState::Queued => "queued",
        CrossTaskState::Running => "running",
        CrossTaskState::Completed => "completed",
        CrossTaskState::Failed => "failed",
        CrossTaskState::Cancelled => "cancelled",
    }
}

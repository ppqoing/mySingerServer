//! 不可变分析输入、确认状态机和候选对替换事务。

use dedup_core::{
    AnalysisRunId, ContentKey, CoreError, LocationKey, MachineId, NormalizedPath, TaskId,
    Thresholds,
};
use rusqlite::{OptionalExtension, params};

use crate::{
    NodeStore, StoreError,
    open::{fixed_bytes, sqlite_integer},
};

/// 分析运行的数据归属。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisMode {
    /// 节点 SQLite 内完成的单机分析。
    Local,
    /// PostgreSQL 中编排的跨机器分析。
    Central,
}

/// 一次分析运行允许的持久化状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisStatus {
    /// 等待相关一筛任务完成并同步。
    CollectingStage1,
    /// 一筛数据达到固定高水位。
    Stage1Synced,
    /// 正在用完整一筛数据生成候选。
    Screening,
    /// 缺失二筛特征已经一次性批量派发。
    Phase2Dispatched,
    /// 二筛任务完成且数据达到新高水位。
    Phase2Synced,
    /// 正在做联合判定和代表分组。
    Finalizing,
    /// 运行完整结束。
    Completed,
    /// 有缺失结果，保留未解决项等待显式重试。
    Partial,
    /// 用户取消运行。
    Cancelled,
}

/// 数据库保存的分析运行摘要。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AnalysisRunSnapshot {
    /// 当前状态。
    pub status: AnalysisStatus,
    /// 输入是否已经封存。
    pub inputs_frozen: bool,
}

/// 从所选完成任务冻结出的一个稳定内容位置对。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AnalysisInput {
    /// MD5 与文件大小内容键。
    pub content: ContentKey,
    /// 物理机器与规范路径位置键。
    pub location: LocationKey,
}

/// 候选媒体种类。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PairKind {
    /// 相似图片候选。
    Image,
    /// 相似视频候选。
    Video,
}

/// 候选在两层筛选中的状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CandidateStatus {
    /// 一筛通过，尚缺联合二筛结果。
    Stage1Passed,
    /// 联合二筛通过。
    Passed,
    /// 联合二筛未通过。
    Rejected,
    /// 所需特征不完整，不按零分处理。
    Incomplete,
}

/// 一次性替换候选集合时写入的有序内容对。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct CandidateWrite {
    /// 图片或视频。
    pub kind: PairKind,
    /// 固定较小内容键。
    pub left: ContentKey,
    /// 固定较大内容键。
    pub right: ContentKey,
    /// 一筛得分。
    pub stage1_score: f64,
    /// 二筛通过的 pHash 分块数。
    pub phash_passed_parts: Option<u8>,
    /// 联合二筛得分。
    pub stage2_score: Option<f64>,
    /// 候选状态。
    pub status: CandidateStatus,
}

impl NodeStore {
    /// 创建 collecting_stage1 状态并保存九个阈值的 TOML 快照。
    pub fn create_analysis_run(
        &mut self,
        mode: AnalysisMode,
        thresholds: Thresholds,
        now_ms: i64,
    ) -> Result<AnalysisRunId, StoreError> {
        thresholds.validate()?;
        let run_id = AnalysisRunId::new();
        let thresholds_toml = toml::to_string(&thresholds).map_err(CoreError::from)?;
        self.connection.execute(
            "INSERT INTO analysis_runs(
               analysis_run_id,mode,status,thresholds_toml,created_at_ms,updated_at_ms)
             VALUES(?1,?2,'collecting_stage1',?3,?4,?4)",
            params![
                run_id.as_uuid().to_string(),
                mode.as_str(),
                thresholds_toml,
                now_ms
            ],
        )?;
        Ok(run_id)
    }

    /// 按确认链转换状态；任何活动态可取消或转 partial。
    pub fn transition_analysis_run(
        &mut self,
        run_id: AnalysisRunId,
        target: AnalysisStatus,
        now_ms: i64,
    ) -> Result<(), StoreError> {
        let current_text: String = self.connection.query_row(
            "SELECT status FROM analysis_runs WHERE analysis_run_id=?1",
            [run_id.as_uuid().to_string()],
            |row| row.get(0),
        )?;
        let current = AnalysisStatus::parse(&current_text)?;
        if !current.can_transition_to(target) {
            return Err(StoreError::InvalidState(format!(
                "分析状态不能从 {} 转为 {}",
                current.as_str(),
                target.as_str()
            )));
        }
        self.connection.execute(
            "UPDATE analysis_runs SET status=?2,updated_at_ms=?3 WHERE analysis_run_id=?1",
            params![run_id.as_uuid().to_string(), target.as_str(), now_ms],
        )?;
        Ok(())
    }

    /// 返回分析状态和输入冻结标记。
    pub fn analysis_run_snapshot(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<AnalysisRunSnapshot, StoreError> {
        let (status, frozen): (String, i64) = self.connection.query_row(
            "SELECT status,inputs_frozen FROM analysis_runs WHERE analysis_run_id=?1",
            [run_id.as_uuid().to_string()],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )?;
        Ok(AnalysisRunSnapshot {
            status: AnalysisStatus::parse(&status)?,
            inputs_frozen: frozen != 0,
        })
    }

    /// 从所选已完成任务冻结当前活动且内容一致的位置，之后拒绝追加。
    pub fn freeze_analysis_inputs(
        &mut self,
        run_id: AnalysisRunId,
        selected_task_ids: &[TaskId],
        now_ms: i64,
    ) -> Result<usize, StoreError> {
        let transaction = self.connection.transaction()?;
        let frozen: i64 = transaction.query_row(
            "SELECT inputs_frozen FROM analysis_runs WHERE analysis_run_id=?1",
            [run_id.as_uuid().to_string()],
            |row| row.get(0),
        )?;
        if frozen != 0 {
            return Err(StoreError::AnalysisInputsFrozen);
        }
        for task_id in selected_task_ids {
            let status: Option<String> = transaction
                .query_row(
                    "SELECT status FROM tasks WHERE task_id=?1",
                    [task_id.as_uuid().to_string()],
                    |row| row.get(0),
                )
                .optional()?;
            if status.as_deref() != Some("completed") {
                return Err(StoreError::InvalidState(format!(
                    "任务 {} 尚未 completed",
                    task_id.as_uuid()
                )));
            }
            transaction.execute(
                "INSERT OR IGNORE INTO analysis_run_inputs(
                   analysis_run_id,md5,file_size,machine_id,normalized_path)
                 SELECT ?1,c.md5,c.file_size,f.machine_id,f.normalized_path
                 FROM task_items ti
                 JOIN files f ON f.machine_id=ti.machine_id
                   AND f.normalized_path=ti.normalized_path
                   AND f.content_id=ti.content_id AND f.active=1
                 JOIN contents c ON c.content_id=f.content_id
                 WHERE ti.task_id=?2 AND ti.status='succeeded'",
                params![run_id.as_uuid().to_string(), task_id.as_uuid().to_string()],
            )?;
        }
        transaction.execute(
            "UPDATE analysis_runs SET inputs_frozen=1,updated_at_ms=?2
             WHERE analysis_run_id=?1",
            params![run_id.as_uuid().to_string(), now_ms],
        )?;
        let count: i64 = transaction.query_row(
            "SELECT COUNT(*) FROM analysis_run_inputs WHERE analysis_run_id=?1",
            [run_id.as_uuid().to_string()],
            |row| row.get(0),
        )?;
        transaction.commit()?;
        Ok(count as usize)
    }

    /// 按内容键和位置键稳定顺序读取冻结输入。
    pub fn analysis_inputs(&self, run_id: AnalysisRunId) -> Result<Vec<AnalysisInput>, StoreError> {
        let mut statement = self.connection.prepare_cached(
            "SELECT md5,file_size,machine_id,normalized_path
             FROM analysis_run_inputs WHERE analysis_run_id=?1
             ORDER BY md5,file_size,machine_id,normalized_path",
        )?;
        let raw = statement
            .query_map([run_id.as_uuid().to_string()], |row| {
                Ok((
                    row.get::<_, Vec<u8>>(0)?,
                    row.get::<_, i64>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, String>(3)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|(md5, size, machine, path)| {
                Ok(AnalysisInput {
                    content: ContentKey::new(
                        fixed_bytes(md5, "analysis_run_inputs.md5")?,
                        size as u64,
                    ),
                    location: LocationKey::new(
                        MachineId::parse(&machine)?,
                        NormalizedPath::new(path)?,
                    ),
                })
            })
            .collect()
    }

    /// 用一个事务替换当前运行的完整候选集合。
    pub fn replace_candidates(
        &mut self,
        run_id: AnalysisRunId,
        candidates: &[CandidateWrite],
    ) -> Result<(), StoreError> {
        let transaction = self.connection.transaction()?;
        transaction.execute(
            "DELETE FROM candidate_pairs WHERE analysis_run_id=?1",
            [run_id.as_uuid().to_string()],
        )?;
        for candidate in candidates {
            if candidate.left >= candidate.right {
                return Err(StoreError::InvalidState(
                    "候选对必须按 ContentKey 严格升序".into(),
                ));
            }
            if !candidate.stage1_score.is_finite()
                || candidate
                    .stage2_score
                    .is_some_and(|score| !score.is_finite())
            {
                return Err(StoreError::NonFiniteScore);
            }
            transaction.execute(
                "INSERT INTO candidate_pairs(
                   analysis_run_id,pair_kind,left_md5,left_size,right_md5,right_size,
                   stage1_score,phash_passed_parts,stage2_score,status)
                 VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10)",
                params![
                    run_id.as_uuid().to_string(),
                    candidate.kind.as_str(),
                    candidate.left.md5().as_slice(),
                    sqlite_integer(candidate.left.file_size())?,
                    candidate.right.md5().as_slice(),
                    sqlite_integer(candidate.right.file_size())?,
                    candidate.stage1_score,
                    candidate.phash_passed_parts,
                    candidate.stage2_score,
                    candidate.status.as_str()
                ],
            )?;
        }
        transaction.commit()?;
        Ok(())
    }
}

impl AnalysisMode {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Local => "local",
            Self::Central => "central",
        }
    }
}

impl AnalysisStatus {
    const fn as_str(self) -> &'static str {
        match self {
            Self::CollectingStage1 => "collecting_stage1",
            Self::Stage1Synced => "stage1_synced",
            Self::Screening => "screening",
            Self::Phase2Dispatched => "phase2_dispatched",
            Self::Phase2Synced => "phase2_synced",
            Self::Finalizing => "finalizing",
            Self::Completed => "completed",
            Self::Partial => "partial",
            Self::Cancelled => "cancelled",
        }
    }

    fn parse(value: &str) -> Result<Self, StoreError> {
        match value {
            "collecting_stage1" => Ok(Self::CollectingStage1),
            "stage1_synced" => Ok(Self::Stage1Synced),
            "screening" => Ok(Self::Screening),
            "phase2_dispatched" => Ok(Self::Phase2Dispatched),
            "phase2_synced" => Ok(Self::Phase2Synced),
            "finalizing" => Ok(Self::Finalizing),
            "completed" => Ok(Self::Completed),
            "partial" => Ok(Self::Partial),
            "cancelled" => Ok(Self::Cancelled),
            _ => Err(StoreError::InvalidState(format!("未知分析状态: {value}"))),
        }
    }

    const fn can_transition_to(self, target: Self) -> bool {
        if matches!(target, Self::Cancelled | Self::Partial)
            && !matches!(self, Self::Completed | Self::Cancelled)
        {
            return true;
        }
        matches!(
            (self, target),
            (Self::CollectingStage1, Self::Stage1Synced)
                | (Self::Stage1Synced, Self::Screening)
                | (Self::Screening, Self::Phase2Dispatched)
                | (Self::Phase2Dispatched, Self::Phase2Synced)
                | (Self::Phase2Synced, Self::Finalizing)
                | (Self::Finalizing, Self::Completed)
                | (Self::Partial, Self::Phase2Dispatched)
        )
    }
}

impl PairKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Image => "image",
            Self::Video => "video",
        }
    }
}

impl CandidateStatus {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Stage1Passed => "stage1_passed",
            Self::Passed => "passed",
            Self::Rejected => "rejected",
            Self::Incomplete => "incomplete",
        }
    }
}

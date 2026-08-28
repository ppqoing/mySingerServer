//! 当前进程内用户对重复组成员的保留、删除和未决定标记。

use dedup_core::{AnalysisRunId, LocationKey};
use rusqlite::{OptionalExtension, params};

use crate::{NodeStore, StoreError};

/// 一个活动重复组成员的复核决定。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReviewDecision {
    /// 尚未明确处理。
    Undecided,
    /// 明确保留，删除批次以此保护每个组。
    Keep,
    /// 明确加入删除计划。
    Delete,
}

impl NodeStore {
    /// UPSERT 一个成员决定；外键保证只能标记当前组成员。
    pub fn save_review_mark(
        &mut self,
        run_id: AnalysisRunId,
        group_id: &str,
        location: &LocationKey,
        decision: ReviewDecision,
    ) -> Result<(), StoreError> {
        self.connection.execute(
            "INSERT INTO review_marks(
               analysis_run_id,group_id,machine_id,normalized_path,decision)
             VALUES(?1,?2,?3,?4,?5)
             ON CONFLICT(analysis_run_id,group_id,machine_id,normalized_path)
             DO UPDATE SET decision=excluded.decision",
            params![
                run_id.as_uuid().to_string(),
                group_id,
                location.machine_id().as_str(),
                location.normalized_path().as_str(),
                decision.as_str()
            ],
        )?;
        Ok(())
    }

    /// 读取一个成员的当前复核决定；从未标记返回 None。
    pub fn review_mark(
        &self,
        run_id: AnalysisRunId,
        group_id: &str,
        location: &LocationKey,
    ) -> Result<Option<ReviewDecision>, StoreError> {
        let decision: Option<String> = self
            .connection
            .query_row(
                "SELECT decision FROM review_marks
                 WHERE analysis_run_id=?1 AND group_id=?2
                   AND machine_id=?3 AND normalized_path=?4",
                params![
                    run_id.as_uuid().to_string(),
                    group_id,
                    location.machine_id().as_str(),
                    location.normalized_path().as_str()
                ],
                |row| row.get(0),
            )
            .optional()?;
        decision
            .map(|value| ReviewDecision::parse(&value))
            .transpose()
    }
}

impl ReviewDecision {
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::Undecided => "undecided",
            Self::Keep => "keep",
            Self::Delete => "delete",
        }
    }

    fn parse(value: &str) -> Result<Self, StoreError> {
        match value {
            "undecided" => Ok(Self::Undecided),
            "keep" => Ok(Self::Keep),
            "delete" => Ok(Self::Delete),
            _ => Err(StoreError::InvalidState(format!("未知复核决定: {value}"))),
        }
    }
}

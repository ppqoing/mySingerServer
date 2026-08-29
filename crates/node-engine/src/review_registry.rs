//! Node 当前进程内的本地结果复核标记。
//!
//! 复核只服务最近一次结果和当前库版本，不写 SQLite 历史表；进程退出或新结果发布时自然丢弃。

use std::collections::BTreeMap;

use dedup_core::{AnalysisRunId, LocationKey};
use dedup_node_store::ReviewDecision;

/// 最近结果成员的内存复核状态；不承担恢复、历史或跨进程同步。
#[derive(Default)]
pub(crate) struct ReviewRegistry {
    scope: Option<ReviewScope>,
    marks: BTreeMap<(String, LocationKey), ReviewDecision>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct ReviewScope {
    run_id: AnalysisRunId,
    library_revision: u64,
}

impl ReviewRegistry {
    /// 清空当前运行的所有本地复核决定。
    pub(crate) fn clear(&mut self) {
        self.scope = None;
        self.marks.clear();
    }

    /// 写入当前结果成员的决定；作用域变化时先丢弃旧结果的标记。
    pub(crate) fn set(
        &mut self,
        run_id: AnalysisRunId,
        library_revision: u64,
        group_id: String,
        location: LocationKey,
        decision: ReviewDecision,
    ) {
        let scope = ReviewScope {
            run_id,
            library_revision,
        };
        if self.scope != Some(scope) {
            self.clear();
            self.scope = Some(scope);
        }
        if decision == ReviewDecision::Undecided {
            self.marks.remove(&(group_id, location));
        } else {
            self.marks.insert((group_id, location), decision);
        }
    }

    /// 读取当前结果成员的决定；旧运行或旧版本永远不可见。
    pub(crate) fn get(
        &self,
        run_id: AnalysisRunId,
        library_revision: u64,
        group_id: &str,
        location: &LocationKey,
    ) -> ReviewDecision {
        if self.scope
            != Some(ReviewScope {
                run_id,
                library_revision,
            })
        {
            return ReviewDecision::Undecided;
        }
        self.marks
            .get(&(group_id.to_owned(), location.clone()))
            .copied()
            .unwrap_or(ReviewDecision::Undecided)
    }

    /// 返回当前组已标记为指定决定的位置；只复制复核键，不加载结果成员。
    pub(crate) fn locations_with_decision(
        &self,
        run_id: AnalysisRunId,
        library_revision: u64,
        group_id: &str,
        decision: ReviewDecision,
    ) -> Vec<LocationKey> {
        if self.scope
            != Some(ReviewScope {
                run_id,
                library_revision,
            })
        {
            return Vec::new();
        }
        self.marks
            .iter()
            .filter_map(|((marked_group, location), marked_decision)| {
                (marked_group == group_id && *marked_decision == decision).then(|| location.clone())
            })
            .collect()
    }
}

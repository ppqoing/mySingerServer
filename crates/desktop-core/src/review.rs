//! 中心结果当前窗口的进程内复核标记和四种只更新标记的快捷规则。

use std::collections::BTreeMap;

use dedup_core::{AnalysisRunId, LocationKey};

use crate::results::MemberView;

/// 管理端统一的未决定、保留和删除复核标记。
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum ReviewDecision {
    /// 尚未决定。
    #[default]
    Undecided,
    /// 明确保留，作为每组删除保护项。
    Keep,
    /// 明确加入后续删除计划。
    Delete,
}

/// 只改变复核标记的确定性快捷选择规则。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum QuickReviewRule {
    /// 保留文件大小最大的活动成员。
    LargestFile,
    /// 保留像素面积最大的活动成员。
    HighestResolution,
    /// 保留 PDQ Quality 最大的活动成员。
    HighestQuality,
    /// 保留规范路径中包含指定文本的第一个活动成员。
    PathContains(String),
}

/// 一次当前 Desktop 进程内的标记变化。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReviewChange {
    /// 被标记的位置。
    pub location: LocationKey,
    /// 新决定。
    pub decision: ReviewDecision,
}

/// 按中心运行与组隔离的当前 Desktop 进程内复核板。
#[derive(Clone, Debug, Default)]
pub struct ReviewBoard {
    marks: BTreeMap<LocationKey, ReviewDecision>,
    /// 当前临时投影所属的中心运行；切换运行时必须重建板面。
    scope_run_id: Option<AnalysisRunId>,
    /// 当前临时投影所属的组；切换组时必须重建板面。
    scope_group_id: Option<String>,
}

impl ReviewBoard {
    /// 从当前窗口成员建立初始标记；不会读取或恢复持久历史。
    pub fn from_members(members: &[MemberView]) -> Self {
        Self {
            marks: members
                .iter()
                .map(|member| (member.location.clone(), member.review))
                .collect(),
            scope_run_id: None,
            scope_group_id: None,
        }
    }

    /// 从中心已返回的成员建立当前进程临时复核投影，不把旧运行标记带入新组。
    pub fn for_central(
        run_id: AnalysisRunId,
        group_id: impl Into<String>,
        members: &[MemberView],
    ) -> Self {
        let mut board = Self::from_members(members);
        board.scope_run_id = Some(run_id);
        board.scope_group_id = Some(group_id.into());
        board
    }

    /// 判断复核投影是否仍属于指定运行和组，供迟到响应门禁使用。
    pub fn is_scoped_to(&self, run_id: AnalysisRunId, group_id: &str) -> bool {
        self.scope_run_id == Some(run_id) && self.scope_group_id.as_deref() == Some(group_id)
    }

    /// 返回一个位置当前标记；未载入位置视为 Undecided。
    pub fn decision(&self, location: &LocationKey) -> ReviewDecision {
        self.marks.get(location).copied().unwrap_or_default()
    }

    /// 更新一个当前进程标记并返回变化描述；不执行文件或数据库操作。
    pub fn set(&mut self, location: LocationKey, decision: ReviewDecision) -> ReviewChange {
        self.marks.insert(location.clone(), decision);
        ReviewChange { location, decision }
    }

    /// 根据规则选择一个 Keep，其余活动成员标记 Delete；不创建或执行删除批次。
    pub fn apply_quick_rule(
        &mut self,
        members: &[MemberView],
        rule: QuickReviewRule,
    ) -> Vec<ReviewChange> {
        let Some(keep) = select_keep(members, &rule) else {
            return Vec::new();
        };
        members
            .iter()
            .filter(|member| member.active)
            .map(|member| {
                let decision = if member.location == keep.location {
                    ReviewDecision::Keep
                } else {
                    ReviewDecision::Delete
                };
                self.set(member.location.clone(), decision)
            })
            .collect()
    }
}

fn select_keep<'a>(members: &'a [MemberView], rule: &QuickReviewRule) -> Option<&'a MemberView> {
    let active = members.iter().filter(|member| member.active);
    match rule {
        QuickReviewRule::LargestFile => active.max_by_key(|member| member.content.file_size()),
        QuickReviewRule::HighestResolution => active.max_by_key(|member| {
            member
                .dimensions
                .map_or(0, |(width, height)| u64::from(width) * u64::from(height))
        }),
        QuickReviewRule::HighestQuality => active.max_by_key(|member| member.quality.unwrap_or(0)),
        QuickReviewRule::PathContains(text) => active
            .filter(|member| member.display_path.contains(text))
            .min_by(|left, right| left.location.cmp(&right.location)),
    }
}

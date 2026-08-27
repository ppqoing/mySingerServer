//! 显式复核标记和四种只更新标记、不触发文件操作的快捷规则。

use std::collections::BTreeMap;

use dedup_core::LocationKey;

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

/// 一次需要持久化到节点或中心的标记变化。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReviewChange {
    /// 被标记的位置。
    pub location: LocationKey,
    /// 新决定。
    pub decision: ReviewDecision,
}

/// 从持久结果恢复并在 UI 操作间保存的复核板。
#[derive(Clone, Debug, Default)]
pub struct ReviewBoard {
    marks: BTreeMap<LocationKey, ReviewDecision>,
}

impl ReviewBoard {
    /// 从一页或有限窗口成员恢复已有 SQLite/PG 标记。
    pub fn from_members(members: &[MemberView]) -> Self {
        Self {
            marks: members
                .iter()
                .map(|member| (member.location.clone(), member.review))
                .collect(),
        }
    }

    /// 返回一个位置当前标记；未载入位置视为 Undecided。
    pub fn decision(&self, location: &LocationKey) -> ReviewDecision {
        self.marks.get(location).copied().unwrap_or_default()
    }

    /// 更新一个标记并返回可直接交给持久边界的变化。
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

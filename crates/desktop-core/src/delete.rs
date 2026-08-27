//! 删除确认摘要和混合执行结果的轻量 UI 进度模型。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::DeleteMode;

use crate::{results::MemberView, review::ReviewDecision};

/// 删除确认对话框中的一个已选择重复组。
#[derive(Clone, Debug)]
pub struct ReviewGroup {
    /// 持久组 ID。
    pub group_id: String,
    /// 当前已载入并复核的活动成员。
    pub members: Vec<MemberView>,
}

impl ReviewGroup {
    /// 组合一个组及其当前成员。
    pub fn new(group_id: impl Into<String>, members: Vec<MemberView>) -> Self {
        Self {
            group_id: group_id.into(),
            members,
        }
    }
}

/// 创建实际删除请求前显示给用户的完整摘要。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeleteConfirmation {
    /// 回收站或永久删除。
    pub mode: DeleteMode,
    /// 将被处理的明确 Delete 文件数。
    pub file_count: usize,
    /// 这些文件分布的物理节点数。
    pub node_count: usize,
    /// 按已扫描大小估算的释放空间。
    pub reclaimable_bytes: u64,
    /// 是否每组有活动 Keep、至少一项 Delete 且删除目标全部在线。
    pub can_execute: bool,
    /// 对缺 Keep、离线目标或永久删除的直接说明。
    pub warning: String,
}

impl DeleteConfirmation {
    /// 从当前复核标记生成确认摘要；本函数不产生删除批次。
    pub fn from_groups(mode: DeleteMode, groups: &[ReviewGroup]) -> Self {
        let has_keep = !groups.is_empty()
            && groups.iter().all(|group| {
                group
                    .members
                    .iter()
                    .any(|member| member.active && member.review == ReviewDecision::Keep)
            });
        let deletes = groups
            .iter()
            .flat_map(|group| &group.members)
            .filter(|member| member.active && member.review == ReviewDecision::Delete)
            .collect::<Vec<_>>();
        let all_online = deletes.iter().all(|member| member.online);
        let nodes = deletes
            .iter()
            .map(|member| member.location.machine_id().clone())
            .collect::<BTreeSet<_>>();
        let mut reasons = Vec::new();
        if mode == DeleteMode::Permanent {
            reasons.push("永久删除不可从回收站恢复");
        }
        if !has_keep {
            reasons.push("每个组都必须至少标记一个活动 Keep");
        }
        if !all_online {
            reasons.push("删除目标所在节点必须在线");
        }
        if deletes.is_empty() {
            reasons.push("没有明确标记为 Delete 的文件");
        }
        Self {
            mode,
            file_count: deletes.len(),
            node_count: nodes.len(),
            reclaimable_bytes: deletes.iter().map(|item| item.content.file_size()).sum(),
            can_execute: has_keep && all_online && !deletes.is_empty(),
            warning: reasons.join("；"),
        }
    }
}

/// 节点或中心删除项的统一结果。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DeleteItemOutcome {
    /// 已进入 Windows 回收站。
    Recycled,
    /// 已永久删除。
    Deleted,
    /// 文件不存在或身份变化，未删除。
    Skipped,
    /// 文件操作失败。
    Failed,
}

/// 删除确认后逐项更新的状态。
#[derive(Clone, Debug)]
pub struct DeleteProgress {
    /// 持久删除批次 ID。
    pub batch_id: String,
    items: BTreeMap<String, DeleteProgressItem>,
}

#[derive(Clone, Debug)]
struct DeleteProgressItem {
    display_path: String,
    size: u64,
    outcome: Option<DeleteItemOutcome>,
    message: Option<String>,
}

impl DeleteProgress {
    /// 以当前成员路径作为稳定 UI 键创建进度；协议执行层仍使用持久 item ID。
    pub fn new(batch_id: impl Into<String>, members: &[MemberView]) -> Self {
        Self {
            batch_id: batch_id.into(),
            items: members
                .iter()
                .map(|member| {
                    (
                        member.display_path.clone(),
                        DeleteProgressItem {
                            display_path: member.display_path.clone(),
                            size: member.content.file_size(),
                            outcome: None,
                            message: None,
                        },
                    )
                })
                .collect(),
        }
    }

    /// 应用一个结果；重复应用相同结果保持同一状态。
    pub fn apply(
        &mut self,
        display_path: &str,
        outcome: DeleteItemOutcome,
        message: Option<String>,
    ) {
        if let Some(item) = self.items.get_mut(display_path) {
            item.outcome = Some(outcome);
            item.message = message;
        }
    }

    /// 返回尚未成功移除、仍应显示的路径。
    pub fn remaining_paths(&self) -> Vec<&str> {
        self.items
            .values()
            .filter(|item| {
                !matches!(
                    item.outcome,
                    Some(DeleteItemOutcome::Recycled | DeleteItemOutcome::Deleted)
                )
            })
            .map(|item| item.display_path.as_str())
            .collect()
    }

    /// 失败或跳过项必须经用户再次确认后才可重试。
    pub fn retryable_paths(&self) -> Vec<&str> {
        self.items
            .values()
            .filter(|item| {
                matches!(
                    item.outcome,
                    Some(DeleteItemOutcome::Failed | DeleteItemOutcome::Skipped)
                )
            })
            .map(|item| item.display_path.as_str())
            .collect()
    }

    /// 只累计成功进入回收站或永久删除的已释放大小。
    pub fn released_bytes(&self) -> u64 {
        self.items
            .values()
            .filter(|item| {
                matches!(
                    item.outcome,
                    Some(DeleteItemOutcome::Recycled | DeleteItemOutcome::Deleted)
                )
            })
            .map(|item| item.size)
            .sum()
    }
}

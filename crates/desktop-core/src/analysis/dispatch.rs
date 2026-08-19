//! 缺失联合二筛内容的确定性节点选择和有界批次规划。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, LocationKey, MachineId};

use crate::central::{CentralCandidate, CentralCandidateStatus, CentralPairKind};

use super::CrossFeatureSet;

/// 单个节点协议请求允许携带的最大二筛内容数。
pub const STAGE2_BATCH_SIZE: usize = 1000;

/// 冻结输入中一个在线节点对某内容的本地可用性。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Stage2Availability {
    /// 跨机器唯一内容键。
    pub content: ContentKey,
    /// 节点上的冻结来源位置。
    pub location: LocationKey,
    /// 节点 SQLite 是否已有完整联合二筛，可直接重发 outbox。
    pub stage2_complete: bool,
}

/// 派给节点的单个唯一内容。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PlannedStage2Item {
    /// 需要补齐的内容。
    pub content: ContentKey,
    /// 选定节点上的来源位置。
    pub source: LocationKey,
    /// 图片为空；视频是成功一筛的固定槽位。
    pub frame_slots: Vec<u32>,
}

/// 一个节点的一页有界二筛请求。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PlannedStage2Batch {
    /// 接收请求的物理节点。
    pub machine_id: MachineId,
    /// 已按内容键排序且不重复的请求项。
    pub items: Vec<PlannedStage2Item>,
}

/// 跳过 PostgreSQL 已完整的内容，再优先选择有本地缓存的在线节点并生成有界批次。
pub fn plan_stage2_batches(
    candidates: &[CentralCandidate],
    features: &CrossFeatureSet,
    availability: &[Stage2Availability],
    online: &BTreeSet<MachineId>,
    batch_size: usize,
) -> Vec<PlannedStage2Batch> {
    if batch_size == 0 {
        return Vec::new();
    }
    let mut required = BTreeMap::<ContentKey, CentralPairKind>::new();
    for candidate in candidates.iter().filter(|candidate| {
        matches!(
            candidate.status,
            CentralCandidateStatus::Stage1Passed | CentralCandidateStatus::Incomplete
        )
    }) {
        required.entry(candidate.left).or_insert(candidate.kind);
        required.entry(candidate.right).or_insert(candidate.kind);
    }
    let mut by_machine = BTreeMap::<MachineId, Vec<PlannedStage2Item>>::new();
    for (content, kind) in required {
        if features.stage2_complete(content, kind) {
            continue;
        }
        let mut choices = availability
            .iter()
            .filter(|choice| {
                choice.content == content && online.contains(choice.location.machine_id())
            })
            .collect::<Vec<_>>();
        choices.sort_by_key(|choice| {
            (
                !choice.stage2_complete,
                choice.location.machine_id().clone(),
                choice.location.normalized_path().clone(),
            )
        });
        let Some(choice) = choices.first() else {
            continue;
        };
        by_machine
            .entry(choice.location.machine_id().clone())
            .or_default()
            .push(PlannedStage2Item {
                content,
                source: choice.location.clone(),
                frame_slots: match kind {
                    CentralPairKind::Image => Vec::new(),
                    CentralPairKind::Video => features.video_frame_slots(content),
                },
            });
    }

    let mut batches = Vec::new();
    for (machine_id, mut items) in by_machine {
        items.sort_by_key(|item| item.content);
        for chunk in items.chunks(batch_size) {
            batches.push(PlannedStage2Batch {
                machine_id: machine_id.clone(),
                items: chunk.to_vec(),
            });
        }
    }
    batches
}

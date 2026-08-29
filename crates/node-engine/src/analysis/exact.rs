//! 冻结输入上的精确重复分组。

use std::collections::BTreeMap;

use dedup_core::{ContentKey, GroupId};

use super::model::{AnalysisGroup, AnalysisGroupKind, AnalysisGroupMember, ScanAnalysisInput};
use crate::runtime_tasks::{
    RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter,
};

/// 按 ContentKey 聚合位置；至少两个活动位置才形成精确组。
pub(crate) fn exact_groups(inputs: &[ScanAnalysisInput]) -> Vec<AnalysisGroup> {
    let mut locations = BTreeMap::<ContentKey, BTreeMap<_, _>>::new();
    for input in inputs {
        locations
            .entry(input.content)
            .or_default()
            .entry(input.location.clone())
            .or_insert_with(|| input.display_path.clone());
    }
    locations
        .into_iter()
        .filter_map(|(content, locations)| {
            if locations.len() < 2 {
                return None;
            }
            let members = locations
                .into_iter()
                .enumerate()
                .map(|(index, (location, display_path))| {
                    AnalysisGroupMember::new(location, display_path, content, index == 0)
                })
                .collect();
            Some(AnalysisGroup {
                group_id: GroupId::new().as_uuid().to_string(),
                kind: AnalysisGroupKind::Exact,
                representative: content,
                members,
            })
        })
        .collect()
}

/// 在精确分组真实子边界完成后保持精准判重运行态；相似候选进度随后继续推进。
pub(crate) fn exact_groups_with_runtime(
    inputs: &[ScanAnalysisInput],
    reporter: Option<&RuntimeTaskReporter>,
    candidate_total: u64,
) -> Vec<AnalysisGroup> {
    let groups = exact_groups(inputs);
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage: RuntimeStage::FinalCompare,
            state: dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            unit: RuntimeProgressUnit::CandidatePairs,
            completed: 0,
            total: Some(candidate_total),
            failed: 0,
            skipped: 0,
        });
    }
    groups
}

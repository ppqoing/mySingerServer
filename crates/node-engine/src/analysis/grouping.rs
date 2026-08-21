//! 将内容级代表直连组展开为稳定位置成员。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, GroupId, LocationKey, SimilarityEdge, group_by_representative};
use dedup_node_store::{
    AnalysisInput, CandidateStatus, CandidateWrite, GroupKind, GroupMemberWrite, GroupWrite,
    PairKind,
};

use crate::runtime_tasks::{
    RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter,
};

use super::exact::{exact_groups, exact_groups_with_runtime};

/// 最终三类分组数量。
pub(crate) struct GroupCounts {
    pub(crate) exact: usize,
    pub(crate) image: usize,
    pub(crate) video: usize,
}

/// 分别在精确、图片和视频聚类真实子边界推进运行时候选对计数。
pub(crate) fn final_groups_with_runtime(
    inputs: &[AnalysisInput],
    candidates: &[CandidateWrite],
    reporter: Option<&RuntimeTaskReporter>,
) -> (Vec<GroupWrite>, GroupCounts) {
    final_groups_internal(inputs, candidates, reporter)
}

fn final_groups_internal(
    inputs: &[AnalysisInput],
    candidates: &[CandidateWrite],
    reporter: Option<&RuntimeTaskReporter>,
) -> (Vec<GroupWrite>, GroupCounts) {
    let mut locations = BTreeMap::<ContentKey, Vec<LocationKey>>::new();
    for input in inputs {
        locations
            .entry(input.content)
            .or_default()
            .push(input.location.clone());
    }
    for values in locations.values_mut() {
        values.sort();
        values.dedup();
    }

    let mut groups = if reporter.is_some() {
        exact_groups_with_runtime(inputs, reporter, candidates.len() as u64)
    } else {
        exact_groups(inputs)
    };
    let exact = groups.len();
    let image_groups = similar_groups(PairKind::Image, GroupKind::Image, candidates, &locations);
    let image = image_groups.len();
    groups.extend(image_groups);
    report_cluster_progress(
        reporter,
        candidates
            .iter()
            .filter(|candidate| candidate.kind == PairKind::Image)
            .count() as u64,
        candidates.len() as u64,
    );
    let video_groups = similar_groups(PairKind::Video, GroupKind::Video, candidates, &locations);
    let video = video_groups.len();
    groups.extend(video_groups);
    report_cluster_progress(reporter, candidates.len() as u64, candidates.len() as u64);
    (
        groups,
        GroupCounts {
            exact,
            image,
            video,
        },
    )
}

fn report_cluster_progress(reporter: Option<&RuntimeTaskReporter>, completed: u64, total: u64) {
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage: RuntimeStage::Cluster,
            state: dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            unit: RuntimeProgressUnit::CandidatePairs,
            completed,
            total: Some(total),
            failed: 0,
            skipped: 0,
        });
    }
}

fn similar_groups(
    pair_kind: PairKind,
    group_kind: GroupKind,
    candidates: &[CandidateWrite],
    locations: &BTreeMap<ContentKey, Vec<LocationKey>>,
) -> Vec<GroupWrite> {
    let passed = candidates
        .iter()
        .filter(|candidate| {
            candidate.kind == pair_kind && candidate.status == CandidateStatus::Passed
        })
        .collect::<Vec<_>>();
    let contents = passed
        .iter()
        .flat_map(|candidate| [candidate.left, candidate.right])
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    let edges = passed
        .iter()
        .map(|candidate| {
            SimilarityEdge::new(
                candidate.left,
                candidate.right,
                candidate.stage1_score,
                candidate.phash_passed_parts,
                candidate.stage2_score.expect("Passed 候选必有二筛得分"),
            )
        })
        .collect::<Vec<_>>();
    group_by_representative(&contents, &edges)
        .into_iter()
        .map(|group| {
            let mut members = Vec::new();
            for grouped in &group.members {
                let evidence = grouped.evidence;
                for (index, location) in locations[&grouped.content].iter().enumerate() {
                    let representative = grouped.content == group.representative && index == 0;
                    members.push(GroupMemberWrite {
                        location: location.clone(),
                        content: grouped.content,
                        representative,
                        stage1_score: evidence.map_or(1.0, |edge| edge.stage1_score),
                        phash_passed_parts: evidence.and_then(|edge| edge.phash_passed_parts),
                        stage2_score: evidence.map(|edge| edge.stage2_score),
                    });
                }
            }
            members.sort_by(|left, right| left.location.cmp(&right.location));
            GroupWrite {
                group_id: GroupId::new().as_uuid().to_string(),
                kind: group_kind,
                representative: group.representative,
                members,
            }
        })
        .collect()
}

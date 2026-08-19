//! 将内容级代表直连组展开为稳定位置成员。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, GroupId, LocationKey, SimilarityEdge, group_by_representative};
use dedup_node_store::{
    AnalysisInput, CandidateStatus, CandidateWrite, GroupKind, GroupMemberWrite, GroupWrite,
    PairKind,
};

use super::exact::exact_groups;

/// 最终三类分组数量。
pub(crate) struct GroupCounts {
    pub(crate) exact: usize,
    pub(crate) image: usize,
    pub(crate) video: usize,
}

/// 组合精确组与两种相似组；相似内容只加入与代表直接通过的成员。
pub(crate) fn final_groups(
    inputs: &[AnalysisInput],
    candidates: &[CandidateWrite],
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

    let mut groups = exact_groups(inputs);
    let exact = groups.len();
    let image_groups = similar_groups(PairKind::Image, GroupKind::Image, candidates, &locations);
    let image = image_groups.len();
    groups.extend(image_groups);
    let video_groups = similar_groups(PairKind::Video, GroupKind::Video, candidates, &locations);
    let video = video_groups.len();
    groups.extend(video_groups);
    (
        groups,
        GroupCounts {
            exact,
            image,
            video,
        },
    )
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

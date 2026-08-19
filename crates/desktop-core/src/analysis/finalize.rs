//! 中心精确组和共享代表直连相似组的最终展开。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, GroupId, LocationKey, SimilarityEdge, group_by_representative};

use crate::central::{
    CentralAnalysisInput, CentralCandidate, CentralCandidateStatus, CentralGroupKind,
    CentralGroupMember, CentralGroupWrite, CentralPairKind,
};

/// 从冻结位置和最终候选一次生成精确、图片与视频组。
pub fn build_groups(
    inputs: &[CentralAnalysisInput],
    candidates: &[CentralCandidate],
) -> Vec<CentralGroupWrite> {
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
    let mut groups = exact_groups(&locations);
    groups.extend(similar_groups(
        CentralPairKind::Image,
        CentralGroupKind::Image,
        candidates,
        &locations,
    ));
    groups.extend(similar_groups(
        CentralPairKind::Video,
        CentralGroupKind::Video,
        candidates,
        &locations,
    ));
    groups
}

fn exact_groups(locations: &BTreeMap<ContentKey, Vec<LocationKey>>) -> Vec<CentralGroupWrite> {
    locations
        .iter()
        .filter(|(_, positions)| positions.len() >= 2)
        .map(|(content, positions)| CentralGroupWrite {
            group_id: GroupId::new().as_uuid().to_string(),
            kind: CentralGroupKind::Exact,
            representative: *content,
            members: positions
                .iter()
                .enumerate()
                .map(|(index, location)| CentralGroupMember {
                    location: location.clone(),
                    content: *content,
                    representative: index == 0,
                    stage1_score: 1.0,
                    phash_passed_parts: None,
                    stage2_score: None,
                })
                .collect(),
        })
        .collect()
}

fn similar_groups(
    pair_kind: CentralPairKind,
    group_kind: CentralGroupKind,
    candidates: &[CentralCandidate],
    locations: &BTreeMap<ContentKey, Vec<LocationKey>>,
) -> Vec<CentralGroupWrite> {
    let passed = candidates
        .iter()
        .filter(|candidate| {
            candidate.kind == pair_kind && candidate.status == CentralCandidateStatus::Passed
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
                candidate.stage2_score.expect("Passed 候选必须有二筛得分"),
            )
        })
        .collect::<Vec<_>>();
    group_by_representative(&contents, &edges)
        .into_iter()
        .map(|group| {
            let mut members = Vec::new();
            for grouped in &group.members {
                let evidence = grouped.evidence;
                if let Some(positions) = locations.get(&grouped.content) {
                    for (index, location) in positions.iter().enumerate() {
                        members.push(CentralGroupMember {
                            location: location.clone(),
                            content: grouped.content,
                            representative: grouped.content == group.representative && index == 0,
                            stage1_score: evidence.map_or(1.0, |edge| edge.stage1_score),
                            phash_passed_parts: evidence.and_then(|edge| edge.phash_passed_parts),
                            stage2_score: evidence.map(|edge| edge.stage2_score),
                        });
                    }
                }
            }
            members.sort_by(|left, right| left.location.cmp(&right.location));
            CentralGroupWrite {
                group_id: GroupId::new().as_uuid().to_string(),
                kind: group_kind,
                representative: group.representative,
                members,
            }
        })
        .collect()
}

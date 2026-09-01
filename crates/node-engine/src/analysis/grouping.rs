//! 将内容级代表直连组展开为稳定位置成员。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, GroupId, SimilarityEdge, group_by_representative};

use crate::runtime_tasks::{
    RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter,
};

use super::exact::{exact_groups, exact_groups_with_runtime};
use super::model::{
    AnalysisCandidate, AnalysisCandidateStatus, AnalysisGroup, AnalysisGroupKind,
    AnalysisGroupMember, AnalysisPairKind, ScanAnalysisInput,
};

/// 最终三类分组数量。
pub(crate) struct GroupCounts {
    pub(crate) exact: usize,
    pub(crate) image: usize,
    pub(crate) video: usize,
}

/// 分别在精确、图片和视频聚类真实子边界推进运行时候选对计数。
pub(crate) fn final_groups_with_runtime(
    inputs: &[ScanAnalysisInput],
    candidates: &[AnalysisCandidate],
    reporter: Option<&RuntimeTaskReporter>,
) -> (Vec<AnalysisGroup>, GroupCounts) {
    final_groups_internal(inputs, candidates, reporter)
}

fn final_groups_internal(
    inputs: &[ScanAnalysisInput],
    candidates: &[AnalysisCandidate],
    reporter: Option<&RuntimeTaskReporter>,
) -> (Vec<AnalysisGroup>, GroupCounts) {
    let mut locations = BTreeMap::<ContentKey, BTreeMap<_, _>>::new();
    for input in inputs {
        locations
            .entry(input.content)
            .or_default()
            .entry(input.location.clone())
            .or_insert_with(|| input.display_path.clone());
    }

    let mut groups = if reporter.is_some() {
        exact_groups_with_runtime(inputs, reporter, candidates.len() as u64)
    } else {
        exact_groups(inputs)
    };
    let exact = groups.len();
    let image_groups = similar_groups(
        AnalysisPairKind::Image,
        AnalysisGroupKind::Image,
        candidates,
        &locations,
    );
    let image = image_groups.len();
    groups.extend(image_groups);
    report_cluster_progress(
        reporter,
        candidates
            .iter()
            .filter(|candidate| candidate.kind == AnalysisPairKind::Image)
            .count() as u64,
        candidates.len() as u64,
    );
    let video_groups = similar_groups(
        AnalysisPairKind::Video,
        AnalysisGroupKind::Video,
        candidates,
        &locations,
    );
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
        crate::diagnostics::record_warning(
            reporter.update_stage_nowait(RuntimeStageUpdate {
                stage: RuntimeStage::FinalCompare,
                state: dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
                unit: RuntimeProgressUnit::CandidatePairs,
                completed,
                total: Some(total),
                failed: 0,
                skipped: 0,
            }),
            "analysis_grouping",
            "update_runtime_stage",
        );
    }
}

fn similar_groups(
    pair_kind: AnalysisPairKind,
    group_kind: AnalysisGroupKind,
    candidates: &[AnalysisCandidate],
    locations: &BTreeMap<ContentKey, BTreeMap<dedup_core::LocationKey, dedup_core::DisplayPath>>,
) -> Vec<AnalysisGroup> {
    let passed = candidates
        .iter()
        .filter(|candidate| {
            candidate.kind == pair_kind && candidate.status == AnalysisCandidateStatus::Passed
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
                for (index, (location, display_path)) in
                    locations[&grouped.content].iter().enumerate()
                {
                    let representative = grouped.content == group.representative && index == 0;
                    let mut member = AnalysisGroupMember::new(
                        location.clone(),
                        display_path.clone(),
                        grouped.content,
                        representative,
                    );
                    member.stage1_score = evidence.map_or(1.0, |edge| edge.stage1_score);
                    member.phash_passed_parts = evidence.and_then(|edge| edge.phash_passed_parts);
                    member.stage2_score = evidence.map(|edge| edge.stage2_score);
                    members.push(member);
                }
            }
            members.sort_by(|left, right| left.location.cmp(&right.location));
            AnalysisGroup {
                group_id: GroupId::new().as_uuid().to_string(),
                kind: group_kind,
                representative: group.representative,
                members,
            }
        })
        .collect()
}

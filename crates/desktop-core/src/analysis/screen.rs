//! PostgreSQL 完整特征视图上的图片/视频两层筛选。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, MediaKind, ScreeningOutcome, Thresholds};
use dedup_media::{
    ImageStage1, VideoFrameFeatures, pdq_bands, score_video_stage1, score_video_stage2,
    screen_image_stage1, screen_image_stage2,
};

use crate::central::{CentralCandidate, CentralCandidateStatus, CentralPairKind};

pub use dedup_central_store::CrossFeatureSet;

/// 使用带位置 PDQ band 索引生成完整一筛候选，并返回缺失一筛的媒体内容数量。
pub fn screen_candidates(
    features: &CrossFeatureSet,
    thresholds: &Thresholds,
) -> (Vec<CentralCandidate>, usize) {
    let skipped = features
        .media_kinds
        .iter()
        .filter(|(content, kind)| match kind {
            MediaKind::Image => !features.image_stage1.contains_key(content),
            MediaKind::Video => !features.video_stage1.contains_key(content),
            MediaKind::Other => false,
        })
        .count();
    let mut candidates = image_candidates(&features.image_stage1, thresholds);
    candidates.extend(video_candidates(&features.video_stage1, thresholds));
    candidates
        .sort_by_key(|candidate| (candidate.kind.sort_key(), candidate.left, candidate.right));
    (candidates, skipped)
}

/// 对全部已持久一筛候选执行联合二筛；缺失特征保持 `Incomplete` 而非零分。
pub fn evaluate_candidates(
    candidates: &[CentralCandidate],
    features: &CrossFeatureSet,
    thresholds: &Thresholds,
) -> (Vec<CentralCandidate>, usize) {
    let mut unresolved = 0;
    let evaluated = candidates
        .iter()
        .map(|candidate| {
            let result = match candidate.kind {
                CentralPairKind::Image => evaluate_image(candidate, features, thresholds),
                CentralPairKind::Video => evaluate_video(candidate, features, thresholds),
            };
            if result.status == CentralCandidateStatus::Incomplete {
                unresolved += 1;
            }
            result
        })
        .collect();
    (evaluated, unresolved)
}

fn image_candidates(
    features: &BTreeMap<ContentKey, ImageStage1>,
    thresholds: &Thresholds,
) -> Vec<CentralCandidate> {
    let mut index = BTreeMap::<(usize, u64), Vec<ContentKey>>::new();
    for (content, feature) in features {
        for (band_index, band) in pdq_bands(&feature.pdq).into_iter().enumerate() {
            index.entry((band_index, band)).or_default().push(*content);
        }
    }
    candidate_pairs(index.values())
        .into_iter()
        .filter_map(|(left, right)| {
            let score = screen_image_stage1(&features[&left], &features[&right], thresholds);
            score.passed.then_some(CentralCandidate {
                kind: CentralPairKind::Image,
                left,
                right,
                stage1_score: f64::from(score.score),
                phash_passed_parts: None,
                stage2_score: None,
                status: CentralCandidateStatus::Stage1Passed,
            })
        })
        .collect()
}

fn video_candidates(
    features: &BTreeMap<ContentKey, Box<[Option<ImageStage1>; 6]>>,
    thresholds: &Thresholds,
) -> Vec<CentralCandidate> {
    let mut index = BTreeMap::<(usize, usize, u64), Vec<ContentKey>>::new();
    for (content, frames) in features {
        for (slot, frame) in frames.iter().enumerate() {
            let Some(frame) = frame else { continue };
            for (band_index, band) in pdq_bands(&frame.pdq).into_iter().enumerate() {
                index
                    .entry((slot, band_index, band))
                    .or_default()
                    .push(*content);
            }
        }
    }
    candidate_pairs(index.values())
        .into_iter()
        .filter_map(|(left, right)| {
            let left_frames = stage1_frames(&features[&left]);
            let right_frames = stage1_frames(&features[&right]);
            let score = score_video_stage1(&left_frames, &right_frames, thresholds);
            (score.outcome == ScreeningOutcome::Passed).then_some(CentralCandidate {
                kind: CentralPairKind::Video,
                left,
                right,
                stage1_score: f64::from(score.average),
                phash_passed_parts: None,
                stage2_score: None,
                status: CentralCandidateStatus::Stage1Passed,
            })
        })
        .collect()
}

fn candidate_pairs<'a, I>(buckets: I) -> BTreeSet<(ContentKey, ContentKey)>
where
    I: Iterator<Item = &'a Vec<ContentKey>>,
{
    let mut pairs = BTreeSet::new();
    for bucket in buckets {
        for left in 0..bucket.len() {
            for right in left + 1..bucket.len() {
                pairs.insert((
                    bucket[left].min(bucket[right]),
                    bucket[left].max(bucket[right]),
                ));
            }
        }
    }
    pairs
}

fn stage1_frames(frames: &[Option<ImageStage1>; 6]) -> [VideoFrameFeatures; 6] {
    std::array::from_fn(|slot| VideoFrameFeatures {
        stage1: frames[slot],
        stage2: None,
    })
}

fn evaluate_image(
    candidate: &CentralCandidate,
    features: &CrossFeatureSet,
    thresholds: &Thresholds,
) -> CentralCandidate {
    let (Some(left), Some(right)) = (
        features.image_stage2.get(&candidate.left),
        features.image_stage2.get(&candidate.right),
    ) else {
        return incomplete(candidate);
    };
    let score = screen_image_stage2(left, right, thresholds);
    CentralCandidate {
        phash_passed_parts: Some(score.phash_passed_parts),
        stage2_score: Some(f64::from(score.sobel_score)),
        status: if score.passed {
            CentralCandidateStatus::Passed
        } else {
            CentralCandidateStatus::Rejected
        },
        ..candidate.clone()
    }
}

fn evaluate_video(
    candidate: &CentralCandidate,
    features: &CrossFeatureSet,
    thresholds: &Thresholds,
) -> CentralCandidate {
    let (Some(left_stage1), Some(right_stage1), Some(left_stage2), Some(right_stage2)) = (
        features.video_stage1.get(&candidate.left),
        features.video_stage1.get(&candidate.right),
        features.video_stage2.get(&candidate.left),
        features.video_stage2.get(&candidate.right),
    ) else {
        return incomplete(candidate);
    };
    let left = std::array::from_fn(|slot| VideoFrameFeatures {
        stage1: left_stage1[slot],
        stage2: left_stage2[slot],
    });
    let right = std::array::from_fn(|slot| VideoFrameFeatures {
        stage1: right_stage1[slot],
        stage2: right_stage2[slot],
    });
    let score = score_video_stage2(&left, &right, thresholds);
    let status = match score.outcome {
        ScreeningOutcome::Passed => CentralCandidateStatus::Passed,
        ScreeningOutcome::Rejected => CentralCandidateStatus::Rejected,
        ScreeningOutcome::Incomplete => CentralCandidateStatus::Incomplete,
    };
    CentralCandidate {
        phash_passed_parts: None,
        stage2_score: (status != CentralCandidateStatus::Incomplete)
            .then_some(f64::from(score.average)),
        status,
        ..candidate.clone()
    }
}

fn incomplete(candidate: &CentralCandidate) -> CentralCandidate {
    CentralCandidate {
        phash_passed_parts: None,
        stage2_score: None,
        status: CentralCandidateStatus::Incomplete,
        ..candidate.clone()
    }
}

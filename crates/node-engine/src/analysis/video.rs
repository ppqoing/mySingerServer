//! 六槽 PDQ band 并集候选和视频一筛平均。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, ScreeningOutcome, Thresholds};
use dedup_media::{ImageStage1, VideoFrameFeatures, pdq_bands, score_video_stage1};
use dedup_node_store::{CandidateStatus, CandidateWrite, PairKind};

/// 任一对齐槽位共享 PDQ band 即进入完整六槽一筛，最后按有效帧平均阈值判断。
pub(crate) fn video_candidates(
    features: &BTreeMap<ContentKey, Box<[Option<ImageStage1>; 6]>>,
    thresholds: &Thresholds,
) -> Vec<CandidateWrite> {
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
    let mut pairs = BTreeSet::new();
    for bucket in index.values() {
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
        .into_iter()
        .filter_map(|(left_key, right_key)| {
            let left_frames = frames_for_stage1(&features[&left_key]);
            let right_frames = frames_for_stage1(&features[&right_key]);
            let score = score_video_stage1(&left_frames, &right_frames, thresholds);
            (score.outcome == ScreeningOutcome::Passed).then_some(CandidateWrite {
                kind: PairKind::Video,
                left: left_key,
                right: right_key,
                stage1_score: f64::from(score.average),
                phash_passed_parts: None,
                stage2_score: None,
                status: CandidateStatus::Stage1Passed,
            })
        })
        .collect()
}

pub(crate) fn frames_for_stage1(frames: &[Option<ImageStage1>; 6]) -> [VideoFrameFeatures; 6] {
    std::array::from_fn(|slot| VideoFrameFeatures {
        stage1: frames[slot],
        stage2: None,
    })
}

//! 六槽 PDQ band 并集候选和视频一筛平均。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, ScreeningOutcome, Thresholds};
use dedup_media::{ImageStage1, VideoFrameFeatures, pdq_bands, score_video_stage1};

use super::model::{AnalysisCandidate, AnalysisCandidateStatus, AnalysisPairKind};
use crate::runtime_tasks::{
    RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter,
};

/// 任一对齐槽位共享 PDQ band 即进入完整六槽一筛，最后按有效帧平均阈值判断。
pub(crate) fn video_candidates(
    features: &BTreeMap<ContentKey, Box<[Option<ImageStage1>; 6]>>,
    thresholds: &Thresholds,
) -> Vec<AnalysisCandidate> {
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
            (score.outcome == ScreeningOutcome::Passed).then_some(AnalysisCandidate {
                kind: AnalysisPairKind::Video,
                left: left_key,
                right: right_key,
                stage1_score: f64::from(score.average),
                phash_passed_parts: None,
                stage2_score: None,
                status: AnalysisCandidateStatus::Stage1Passed,
            })
        })
        .collect()
}

/// 生成视频候选并在完整六槽一筛返回后累计真实候选对计数。
pub(crate) fn video_candidates_with_runtime(
    features: &BTreeMap<ContentKey, Box<[Option<ImageStage1>; 6]>>,
    thresholds: &Thresholds,
    reporter: Option<&RuntimeTaskReporter>,
    completed_before: u64,
) -> Vec<AnalysisCandidate> {
    let candidates = video_candidates(features, thresholds);
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage: RuntimeStage::BuildCandidates,
            state: dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            unit: RuntimeProgressUnit::CandidatePairs,
            completed: completed_before.saturating_add(candidates.len() as u64),
            total: None,
            failed: 0,
            skipped: 0,
        });
    }
    candidates
}

pub(crate) fn frames_for_stage1(frames: &[Option<ImageStage1>; 6]) -> [VideoFrameFeatures; 6] {
    std::array::from_fn(|slot| VideoFrameFeatures {
        stage1: frames[slot],
        stage2: None,
    })
}

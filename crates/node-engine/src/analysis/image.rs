//! 图片 PDQ band 候选索引和完整一筛。

use std::collections::{BTreeMap, BTreeSet};

use dedup_core::{ContentKey, Thresholds};
use dedup_media::{ImageStage1, pdq_bands, screen_image_stage1};

use super::model::{AnalysisCandidate, AnalysisCandidateStatus, AnalysisPairKind};
use crate::runtime_tasks::{
    RuntimeProgressUnit, RuntimeStage, RuntimeStageUpdate, RuntimeTaskReporter,
};

/// 先用四个带位置的 PDQ band 生成候选，再执行完整 Quality/比例/汉明门禁。
pub(crate) fn image_candidates(
    features: &BTreeMap<ContentKey, ImageStage1>,
    thresholds: &Thresholds,
) -> Vec<AnalysisCandidate> {
    let mut index = BTreeMap::<(usize, u64), Vec<ContentKey>>::new();
    for (content, feature) in features {
        for (band_index, band) in pdq_bands(&feature.pdq).into_iter().enumerate() {
            index.entry((band_index, band)).or_default().push(*content);
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
        .filter_map(|(left, right)| {
            let score = screen_image_stage1(&features[&left], &features[&right], thresholds);
            score.passed.then_some(AnalysisCandidate {
                kind: AnalysisPairKind::Image,
                left,
                right,
                stage1_score: f64::from(score.score),
                phash_passed_parts: None,
                stage2_score: None,
                status: AnalysisCandidateStatus::Stage1Passed,
            })
        })
        .collect()
}

/// 生成图片候选并在完整图片一筛返回后推进真实候选对计数。
pub(crate) fn image_candidates_with_runtime(
    features: &BTreeMap<ContentKey, ImageStage1>,
    thresholds: &Thresholds,
    reporter: Option<&RuntimeTaskReporter>,
) -> Vec<AnalysisCandidate> {
    let candidates = image_candidates(features, thresholds);
    if let Some(reporter) = reporter {
        let _ = reporter.update_stage_nowait(RuntimeStageUpdate {
            stage: RuntimeStage::BuildCandidates,
            state: dedup_protocol::proto::RuntimeStageState::RuntimeStageRunning,
            unit: RuntimeProgressUnit::CandidatePairs,
            completed: candidates.len() as u64,
            total: None,
            failed: 0,
            skipped: 0,
        });
    }
    candidates
}

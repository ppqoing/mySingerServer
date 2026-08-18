//! 六个固定时间槽位的视频一筛和联合二筛平均规则。

use std::time::Duration;

use dedup_core::{ScreeningOutcome, Thresholds};

use crate::{ImageStage1, ImageStage2, screen_image_stage1, screen_image_stage2};

/// 单个固定视频槽位已经拥有的图片特征。
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct VideoFrameFeatures {
    /// `None` 表示该槽位解码失败，因此不进入有效帧分母。
    pub stage1: Option<ImageStage1>,
    /// `None` 且一筛存在表示二筛尚未完整计算。
    pub stage2: Option<ImageStage2>,
}

/// 一次六帧比较可持久化的平均结果。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct VideoScore {
    /// 通过、拒绝或数据不完整。
    pub outcome: ScreeningOutcome,
    /// 双方都成功解码一筛的对齐槽位数。
    pub valid_frames: u8,
    /// 有效槽位的平均分；没有有效槽位时为零。
    pub average: f32,
}

/// 返回视频六等分区间的中点，即 `(1,3,5,7,9,11)/12`。
pub fn sample_positions(duration: Duration) -> [Duration; 6] {
    std::array::from_fn(|index| duration.mul_f64((2 * index + 1) as f64 / 12.0))
}

/// 对齐比较六个一筛槽位；解码失败不进分母，阈值失败计零分。
pub fn score_video_stage1(
    left: &[VideoFrameFeatures; 6],
    right: &[VideoFrameFeatures; 6],
    thresholds: &Thresholds,
) -> VideoScore {
    let mut valid_frames = 0_u8;
    let mut total = 0.0_f32;
    for (left, right) in left.iter().zip(right) {
        if let (Some(left), Some(right)) = (left.stage1, right.stage1) {
            valid_frames += 1;
            let score = screen_image_stage1(&left, &right, thresholds);
            if score.passed {
                total += score.score;
            }
        }
    }
    finish_score(
        valid_frames,
        total,
        thresholds.video_min_valid_frames,
        thresholds.video_stage1_min,
    )
}

/// 对齐比较联合二筛；有效槽位缺少任一端二筛结果时整体保持 Incomplete。
pub fn score_video_stage2(
    left: &[VideoFrameFeatures; 6],
    right: &[VideoFrameFeatures; 6],
    thresholds: &Thresholds,
) -> VideoScore {
    let valid_frames = left
        .iter()
        .zip(right)
        .filter(|(left, right)| left.stage1.is_some() && right.stage1.is_some())
        .count() as u8;
    if valid_frames < thresholds.video_min_valid_frames {
        return VideoScore {
            outcome: ScreeningOutcome::Incomplete,
            valid_frames,
            average: 0.0,
        };
    }

    let mut total = 0.0_f32;
    for (left, right) in left.iter().zip(right) {
        if left.stage1.is_none() || right.stage1.is_none() {
            continue;
        }
        let (Some(left_stage2), Some(right_stage2)) = (left.stage2, right.stage2) else {
            return VideoScore {
                outcome: ScreeningOutcome::Incomplete,
                valid_frames,
                average: 0.0,
            };
        };
        let score = screen_image_stage2(&left_stage2, &right_stage2, thresholds);
        if score.phash_passed_parts >= thresholds.phash_min_passed_parts {
            total += score.sobel_score;
        }
    }

    finish_score(
        valid_frames,
        total,
        thresholds.video_min_valid_frames,
        thresholds.video_stage2_min,
    )
}

fn finish_score(
    valid_frames: u8,
    total: f32,
    minimum_valid_frames: u8,
    minimum_average: f32,
) -> VideoScore {
    let average = if valid_frames == 0 {
        0.0
    } else {
        total / f32::from(valid_frames)
    };
    let outcome = if valid_frames < minimum_valid_frames {
        ScreeningOutcome::Incomplete
    } else if average >= minimum_average {
        ScreeningOutcome::Passed
    } else {
        ScreeningOutcome::Rejected
    };
    VideoScore {
        outcome,
        valid_frames,
        average,
    }
}

//! 相似媒体判定所用的可配置阈值快照。

use serde::{Deserialize, Serialize};

use crate::CoreError;

/// 一次分析运行完整保存的九个匹配阈值。
#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Serialize)]
#[serde(default)]
pub struct Thresholds {
    /// 参与图片一筛所需的最低 PDQ Quality。
    pub pdq_quality_min: u8,
    /// 两张图片允许的最大相对长宽比差。
    pub aspect_tolerance: f32,
    /// 图片一筛允许的最大 PDQ 汉明距离。
    pub pdq_hamming_max: u16,
    /// 单个 pHash 分块允许的最大汉明距离。
    pub phash_part_hamming_max: u8,
    /// 九个 pHash 分块中至少需要通过的块数。
    pub phash_min_passed_parts: u8,
    /// 图片二筛所需的最低 Sobel 余弦相似度。
    pub sobel_min: f32,
    /// 视频比较所需的最少有效对齐帧数。
    pub video_min_valid_frames: u8,
    /// 视频一筛所需的最低平均分。
    pub video_stage1_min: f32,
    /// 视频联合二筛所需的最低平均分。
    pub video_stage2_min: f32,
}

impl Thresholds {
    /// 在配置边界一次性验证全部阈值，内部算法随后直接使用强类型快照。
    pub fn validate(&self) -> Result<(), CoreError> {
        if self.pdq_quality_min > 100 {
            return Err(invalid("pdq_quality_min", "必须位于 0..=100"));
        }
        validate_score("aspect_tolerance", self.aspect_tolerance)?;
        if self.pdq_hamming_max > 256 {
            return Err(invalid("pdq_hamming_max", "必须位于 0..=256"));
        }
        if self.phash_part_hamming_max > 64 {
            return Err(invalid("phash_part_hamming_max", "必须位于 0..=64"));
        }
        if !(1..=9).contains(&self.phash_min_passed_parts) {
            return Err(invalid("phash_min_passed_parts", "必须位于 1..=9"));
        }
        validate_score("sobel_min", self.sobel_min)?;
        if !(1..=6).contains(&self.video_min_valid_frames) {
            return Err(invalid("video_min_valid_frames", "必须位于 1..=6"));
        }
        validate_score("video_stage1_min", self.video_stage1_min)?;
        validate_score("video_stage2_min", self.video_stage2_min)
    }
}

fn validate_score(field: &'static str, value: f32) -> Result<(), CoreError> {
    if !(0.0..=1.0).contains(&value) {
        return Err(invalid(field, "必须是位于 0.0..=1.0 的有限数值"));
    }
    Ok(())
}

const fn invalid(field: &'static str, reason: &'static str) -> CoreError {
    CoreError::InvalidThreshold { field, reason }
}

impl Default for Thresholds {
    fn default() -> Self {
        Self {
            pdq_quality_min: 50,
            aspect_tolerance: 0.10,
            pdq_hamming_max: 31,
            phash_part_hamming_max: 10,
            phash_min_passed_parts: 8,
            sobel_min: 0.85,
            video_min_valid_frames: 4,
            video_stage1_min: 0.80,
            video_stage2_min: 0.80,
        }
    }
}

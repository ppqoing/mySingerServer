//! 图片一筛、联合二筛和 PDQ band 候选索引的纯比较规则。

use dedup_core::Thresholds;

use crate::{GrayImage, PdqHash, compute_partition_phash, compute_sobel, sobel_cosine};

/// 图片一筛完整特征。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ImageStage1 {
    /// 原图像素宽度。
    pub width: u32,
    /// 原图像素高度。
    pub height: u32,
    /// 规范字节序的 PDQ-256。
    pub pdq: PdqHash,
    /// Meta PDQ 图像域 Quality。
    pub quality: u8,
}

/// 图片联合二筛完整特征。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct ImageStage2 {
    /// 3×3 行优先的 64 位分块 pHash。
    pub phash_parts: [u64; 9],
    /// 4×4 空间格乘八方向 bin 的 L2 归一化 Sobel 向量。
    pub sobel: [f32; 128],
}

/// 可持久化和展示的一筛判定明细。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct ImageStage1Score {
    /// Quality、长宽比和 PDQ 是否全部通过。
    pub passed: bool,
    /// `1 - pdq_hamming / 256`。
    pub score: f32,
    /// 实际 PDQ 汉明距离。
    pub pdq_hamming: u16,
}

/// 可持久化和展示的联合二筛判定明细。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct ImageStage2Score {
    /// pHash 通过块数和 Sobel 是否共同通过。
    pub passed: bool,
    /// 汉明距离不超过单块阈值的块数。
    pub phash_passed_parts: u8,
    /// 两端 128 维 Sobel 余弦相似度。
    pub sobel_score: f32,
}

/// 从同一灰度面一次生成 pHash 和 Sobel 二筛特征。
pub fn compute_image_stage2(image: &GrayImage) -> ImageStage2 {
    ImageStage2 {
        phash_parts: compute_partition_phash(image),
        sobel: compute_sobel(image),
    }
}

/// 使用分析运行冻结的阈值快照执行图片一筛。
pub fn screen_image_stage1(
    left: &ImageStage1,
    right: &ImageStage1,
    thresholds: &Thresholds,
) -> ImageStage1Score {
    let hamming = left.pdq.hamming_distance(&right.pdq);
    let left_aspect = left.width as f32 / left.height as f32;
    let right_aspect = right.width as f32 / right.height as f32;
    let aspect_difference = (left_aspect - right_aspect).abs() / left_aspect.max(right_aspect);
    let passed = left.quality >= thresholds.pdq_quality_min
        && right.quality >= thresholds.pdq_quality_min
        && aspect_difference <= thresholds.aspect_tolerance
        && hamming <= thresholds.pdq_hamming_max;
    ImageStage1Score {
        passed,
        score: 1.0 - f32::from(hamming) / 256.0,
        pdq_hamming: hamming,
    }
}

/// 使用同一阈值快照执行九分块 pHash 与 Sobel 联合二筛。
pub fn screen_image_stage2(
    left: &ImageStage2,
    right: &ImageStage2,
    thresholds: &Thresholds,
) -> ImageStage2Score {
    let passed_parts = left
        .phash_parts
        .iter()
        .zip(right.phash_parts)
        .filter(|(left, right)| {
            (*left ^ *right).count_ones() <= u32::from(thresholds.phash_part_hamming_max)
        })
        .count() as u8;
    let sobel_score = sobel_cosine(&left.sobel, &right.sobel);
    ImageStage2Score {
        passed: passed_parts >= thresholds.phash_min_passed_parts
            && sobel_score >= thresholds.sobel_min,
        phash_passed_parts: passed_parts,
        sobel_score,
    }
}

/// 把 PDQ 规范字节切成四个连续的大端 64 位 band。
pub fn pdq_bands(hash: &PdqHash) -> [u64; 4] {
    std::array::from_fn(|index| {
        let offset = index * 8;
        u64::from_be_bytes(
            hash.as_bytes()[offset..offset + 8]
                .try_into()
                .expect("固定八字节切片"),
        )
    })
}

/// 判断两个 PDQ 是否共享至少一个用于近似候选索引的 band。
pub fn shares_pdq_band(left: &PdqHash, right: &PdqHash) -> bool {
    pdq_bands(left)
        .into_iter()
        .zip(pdq_bands(right))
        .any(|(left, right)| left == right)
}

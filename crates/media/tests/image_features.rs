//! 图片二筛特征的位序、方向、阈值和候选索引契约测试。

use dedup_core::Thresholds;
use dedup_media::{
    GrayImage, ImageStage1, ImageStage2, PdqHash, compute_partition_phash, compute_sobel,
    pdq_bands, phash_parts_to_blob, screen_image_stage1, screen_image_stage2, shares_pdq_band,
    sobel_cosine,
};

fn gray(width: u32, height: u32, pixel: impl Fn(u32, u32) -> u8) -> GrayImage {
    let mut pixels = Vec::with_capacity((width * height) as usize);
    for y in 0..height {
        for x in 0..width {
            pixels.push(pixel(x, y));
        }
    }
    GrayImage::new(width, height, pixels).unwrap()
}

/// 九块必须按 3×3 行优先排列，修改左上块不能污染其余八块。
#[test]
fn phash_parts_are_row_major() {
    let image = gray(96, 96, |x, y| {
        if x < 32 && y < 32 {
            if x < 16 { 20 } else { 230 }
        } else {
            127
        }
    });
    let parts = compute_partition_phash(&image);
    assert_ne!(parts[0], parts[1]);
    assert!(parts[1..].iter().all(|part| *part == parts[1]));
}

/// 横向余弦基对应左上 8×8 DCT 的 `u=1,v=0`，即行优先 bit 1。
#[test]
fn phash_bit_index_is_dct_row_major() {
    let image = gray(96, 96, |x, _| {
        let local_x = (x % 32) as f64;
        let wave = ((2.0 * local_x + 1.0) * std::f64::consts::PI / 64.0).cos();
        (128.0 + 100.0 * wave).round() as u8
    });
    let parts = compute_partition_phash(&image);
    assert!(parts.iter().all(|part| part & (1 << 1) != 0));
}

/// 上中位数配合严格大于最多设置 31 位，平坦输入重复计算必须一致。
#[test]
fn phash_flat_block_uses_strict_upper_median() {
    let image = gray(96, 96, |_, _| 137);
    let first = compute_partition_phash(&image);
    let second = compute_partition_phash(&image);
    assert_eq!(first, second);
    assert!(first.iter().all(|part| part.count_ones() <= 31));
}

/// 九个 `u64` 直接按行优先、小端写成固定 72 字节数据库 BLOB。
#[test]
fn phash_blob_is_72_little_endian_bytes() {
    let parts = [
        0x0102_0304_0506_0708,
        2,
        3,
        4,
        5,
        6,
        7,
        8,
        0x8877_6655_4433_2211,
    ];
    let blob = phash_parts_to_blob(&parts);
    assert_eq!(blob.len(), 72);
    assert_eq!(&blob[..8], &parts[0].to_le_bytes());
    assert_eq!(&blob[64..], &parts[8].to_le_bytes());
}

/// 全零结构的余弦规则必须明确，不能产生除零或 NaN。
#[test]
fn sobel_zero_vector_similarity_is_defined() {
    let zero = [0.0_f32; 128];
    let mut nonzero = zero;
    nonzero[0] = 1.0;
    assert_eq!(sobel_cosine(&zero, &zero), 1.0);
    assert_eq!(sobel_cosine(&zero, &nonzero), 0.0);
}

/// 垂直边缘及其反相都进入无符号方向 bin 0，并保留 4×4 空间格。
#[test]
fn sobel_vertical_edge_uses_bin_zero_and_spatial_cells() {
    let normal = compute_sobel(&gray(128, 128, |x, _| if x < 64 { 20 } else { 230 }));
    let inverted = compute_sobel(&gray(128, 128, |x, _| if x < 64 { 230 } else { 20 }));
    assert_eq!(normal, inverted);
    let nonzero: Vec<_> = normal
        .iter()
        .enumerate()
        .filter_map(|(index, value)| (*value > 0.0).then_some(index))
        .collect();
    assert!(nonzero.iter().all(|index| index % 8 == 0));
    assert!(nonzero.contains(&40)); // cell(1,1), bin 0
    assert!(nonzero.contains(&80)); // cell(2,2), bin 0
}

/// 水平边缘固定进入 π/2 对应的 bin 4。
#[test]
fn sobel_horizontal_edge_uses_bin_four() {
    let histogram = compute_sobel(&gray(128, 128, |_, y| if y < 64 { 20 } else { 230 }));
    assert!(
        histogram
            .iter()
            .enumerate()
            .filter(|(_, value)| **value > 0.0)
            .all(|(index, _)| index % 8 == 4)
    );
}

fn stage1(width: u32, height: u32, quality: u8, bytes: [u8; 32]) -> ImageStage1 {
    ImageStage1 {
        width,
        height,
        pdq: PdqHash::from_bytes(bytes),
        quality,
    }
}

/// 一筛同时消费 Quality、对称长宽比差和 PDQ 汉明阈值，并保存可解释分数。
#[test]
fn image_stage1_uses_complete_threshold_snapshot() {
    let left = stage1(100, 100, 50, [0; 32]);
    let mut one_bit = [0; 32];
    one_bit[0] = 1;
    let passing = stage1(110, 100, 50, one_bit);
    let score = screen_image_stage1(&left, &passing, &Thresholds::default());
    assert!(score.passed);
    assert_eq!(score.pdq_hamming, 1);
    assert_eq!(score.score, 255.0 / 256.0);

    let bad_quality = stage1(110, 100, 49, one_bit);
    assert!(!screen_image_stage1(&left, &bad_quality, &Thresholds::default()).passed);
    let bad_aspect = stage1(112, 100, 50, one_bit);
    assert!(!screen_image_stage1(&left, &bad_aspect, &Thresholds::default()).passed);
}

/// 二筛必须同时满足逐块阈值、最少通过块数和 Sobel 余弦阈值。
#[test]
fn image_stage2_requires_phash_and_sobel() {
    let left = ImageStage2 {
        phash_parts: [0; 9],
        sobel: {
            let mut value = [0.0; 128];
            value[0] = 1.0;
            value
        },
    };
    let right = ImageStage2 {
        phash_parts: [
            0x3ff, 0x3ff, 0x3ff, 0x3ff, 0x3ff, 0x3ff, 0x3ff, 0x3ff, 0x7ff,
        ],
        sobel: left.sobel,
    };
    let score = screen_image_stage2(&left, &right, &Thresholds::default());
    assert!(score.passed);
    assert_eq!(score.phash_passed_parts, 8);
    assert_eq!(score.sobel_score, 1.0);

    let mut orthogonal = right;
    orthogonal.sobel[0] = 0.0;
    orthogonal.sobel[1] = 1.0;
    assert!(!screen_image_stage2(&left, &orthogonal, &Thresholds::default()).passed);
}

/// PDQ band 按规范字节连续切为四个大端 `u64`。
#[test]
fn pdq_bands_use_four_consecutive_big_endian_words() {
    let bytes = std::array::from_fn(|index| index as u8);
    let bands = pdq_bands(&PdqHash::from_bytes(bytes));
    assert_eq!(bands[0], 0x0001_0203_0405_0607);
    assert_eq!(bands[3], 0x1819_1a1b_1c1d_1e1f);
}

/// 四个 band 都变化时即使总汉明距离处于 4..31，也不承诺被近似索引召回。
#[test]
fn pdq_band_index_documents_approximate_recall_boundary() {
    let left = PdqHash::from_bytes([0; 32]);
    let mut four_bits = [0_u8; 32];
    for index in [0, 8, 16, 24] {
        four_bits[index] = 1;
    }
    let four_bits = PdqHash::from_bytes(four_bits);
    assert_eq!(left.hamming_distance(&four_bits), 4);
    assert!(!shares_pdq_band(&left, &four_bits));

    let mut thirty_one_bits = [0_u8; 32];
    for bit in 0..28 {
        thirty_one_bits[bit / 8] |= 1 << (bit % 8);
    }
    for index in [8, 16, 24] {
        thirty_one_bits[index] = 1;
    }
    let thirty_one_bits = PdqHash::from_bytes(thirty_one_bits);
    assert_eq!(left.hamming_distance(&thirty_one_bits), 31);
    assert!(!shares_pdq_band(&left, &thirty_one_bits));
}

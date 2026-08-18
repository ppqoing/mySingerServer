//! 六帧视频评分与 3×2 JPG 联系表的固定语义测试。

use std::time::Duration;

use dedup_core::{ScreeningOutcome, Thresholds};
use dedup_media::{
    ImageStage1, ImageStage2, PdqHash, Rgb24Image, VideoFrameFeatures, encode_contact_sheet,
    sample_positions, score_video_stage1, score_video_stage2,
};

/// 六个采样点固定取六等分区间的中点。
#[test]
fn samples_midpoints_of_six_equal_segments() {
    let positions = sample_positions(Duration::from_secs(120));
    assert_eq!(
        positions.map(|value| value.as_secs()),
        [10, 30, 50, 70, 90, 110]
    );
}

fn image_stage1(byte: u8) -> ImageStage1 {
    let mut pdq = [0_u8; 32];
    pdq[0] = byte;
    ImageStage1 {
        width: 1920,
        height: 1080,
        pdq: PdqHash::from_bytes(pdq),
        quality: 80,
    }
}

fn stage2(axis: usize, changed_parts: usize) -> ImageStage2 {
    let mut sobel = [0.0_f32; 128];
    sobel[axis] = 1.0;
    let mut phash_parts = [0_u64; 9];
    for part in phash_parts.iter_mut().take(changed_parts) {
        *part = 0x7ff;
    }
    ImageStage2 { phash_parts, sobel }
}

fn frame(stage1: Option<ImageStage1>, stage2: Option<ImageStage2>) -> VideoFrameFeatures {
    VideoFrameFeatures { stage1, stage2 }
}

/// 解码失败的槽位不进分母，完整的四个槽位可形成一筛结果。
#[test]
fn video_stage1_averages_only_aligned_decoded_frames() {
    let empty = frame(None, None);
    let left = [
        frame(Some(image_stage1(0)), None),
        frame(Some(image_stage1(0)), None),
        frame(Some(image_stage1(0)), None),
        frame(Some(image_stage1(0)), None),
        empty,
        empty,
    ];
    let right = [
        frame(Some(image_stage1(0)), None),
        frame(Some(image_stage1(0)), None),
        frame(Some(image_stage1(0)), None),
        frame(Some(image_stage1(0)), None),
        frame(Some(image_stage1(0)), None),
        empty,
    ];
    let score = score_video_stage1(&left, &right, &Thresholds::default());
    assert_eq!(score.outcome, ScreeningOutcome::Passed);
    assert_eq!(score.valid_frames, 4);
    assert_eq!(score.average, 1.0);
}

/// 有效帧即使未通过图片一筛也计零分，而不是从分母删除。
#[test]
fn video_stage1_counts_rejected_valid_frame_as_zero() {
    let good = frame(Some(image_stage1(0)), None);
    let mut rejected = image_stage1(0);
    rejected.pdq = PdqHash::from_bytes([0xff; 32]);
    let bad_right = frame(Some(rejected), None);
    let left = [good; 6];
    let mut right = [good; 6];
    right[0] = bad_right;
    let score = score_video_stage1(&left, &right, &Thresholds::default());
    assert_eq!(score.valid_frames, 6);
    assert_eq!(score.average, 5.0 / 6.0);
    assert_eq!(score.outcome, ScreeningOutcome::Passed);
}

/// 低于四个有效对齐帧必须返回 Incomplete，而不是 Rejected。
#[test]
fn video_stage1_requires_minimum_valid_frames() {
    let good = frame(Some(image_stage1(0)), None);
    let missing = frame(None, None);
    let frames = [good, good, good, missing, missing, missing];
    let score = score_video_stage1(&frames, &frames, &Thresholds::default());
    assert_eq!(score.outcome, ScreeningOutcome::Incomplete);
    assert_eq!(score.valid_frames, 3);
}

/// pHash 通过时帧分数取 Sobel；pHash 未通过的有效帧固定计零。
#[test]
fn video_stage2_averages_joint_frame_scores() {
    let first = frame(Some(image_stage1(0)), Some(stage2(0, 0)));
    let phash_rejected = frame(Some(image_stage1(0)), Some(stage2(0, 2)));
    let left = [first; 6];
    let mut right = [first; 6];
    right[0] = phash_rejected;
    let score = score_video_stage2(&left, &right, &Thresholds::default());
    assert_eq!(score.valid_frames, 6);
    assert_eq!(score.average, 5.0 / 6.0);
    assert_eq!(score.outcome, ScreeningOutcome::Passed);
}

/// 已有一筛的有效帧缺任一端二筛特征时，整次二筛必须保持 Incomplete。
#[test]
fn video_stage2_does_not_treat_missing_features_as_zero() {
    let complete = frame(Some(image_stage1(0)), Some(stage2(0, 0)));
    let missing_stage2 = frame(Some(image_stage1(0)), None);
    let left = [complete; 6];
    let mut right = [complete; 6];
    right[2] = missing_stage2;
    let score = score_video_stage2(&left, &right, &Thresholds::default());
    assert_eq!(score.outcome, ScreeningOutcome::Incomplete);
}

fn solid(red: u8, green: u8, blue: u8) -> Rgb24Image {
    Rgb24Image::new(2, 2, [red, green, blue].repeat(4)).unwrap()
}

/// 联系表固定为三列两行，六个槽位按行优先落入 JPG。
#[test]
fn contact_sheet_is_three_by_two_row_major_jpeg() {
    let frames = [
        Some(solid(220, 20, 20)),
        Some(solid(20, 220, 20)),
        Some(solid(20, 20, 220)),
        Some(solid(220, 220, 20)),
        Some(solid(20, 220, 220)),
        Some(solid(220, 20, 220)),
    ];
    let jpeg = encode_contact_sheet(&frames, 2, 2).unwrap();
    assert_eq!(&jpeg[..3], &[0xff, 0xd8, 0xff]);
    let decoded = image::load_from_memory_with_format(&jpeg, image::ImageFormat::Jpeg)
        .unwrap()
        .to_rgb8();
    assert_eq!(decoded.dimensions(), (6, 4));
    let red = decoded.get_pixel(0, 0).0;
    let green = decoded.get_pixel(2, 0).0;
    let blue = decoded.get_pixel(4, 0).0;
    assert!(red[0] > red[1] && red[0] > red[2]);
    assert!(green[1] > green[0] && green[1] > green[2]);
    assert!(blue[2] > blue[0] && blue[2] > blue[1]);
}

/// 缺失槽位在编码前使用固定 `#60656F`，解码后允许 JPG 的微小量化误差。
#[test]
fn contact_sheet_missing_slot_uses_fixed_gray() {
    let frames: [Option<Rgb24Image>; 6] = std::array::from_fn(|_| None);
    let jpeg = encode_contact_sheet(&frames, 2, 2).unwrap();
    let decoded = image::load_from_memory_with_format(&jpeg, image::ImageFormat::Jpeg)
        .unwrap()
        .to_rgb8();
    let pixel = decoded.get_pixel(1, 1).0;
    for (actual, expected) in pixel.into_iter().zip([0x60_u8, 0x65, 0x6f]) {
        assert!((i16::from(actual) - i16::from(expected)).abs() <= 3);
    }
}

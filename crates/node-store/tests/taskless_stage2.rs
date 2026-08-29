//! 无持久任务表的二筛提交事务行为测试。

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_store::{
    FeatureWrite, ImageStage1Fields, NodeStore, ScannedPath, VideoFrameStage1Fields,
    VideoFrameStage2Fields, VideoMetadataFields,
};

fn machine() -> MachineId {
    MachineId::parse("73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae").unwrap()
}

fn scan(path: &str, size: u64) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        size,
    )
}

fn image_stage1() -> ImageStage1Fields {
    ImageStage1Fields::from(ImageStage1 {
        width: 1920,
        height: 1080,
        pdq: PdqHash::from_bytes([7; 32]),
        quality: 80,
    })
}

fn video_frame(slot: u8) -> VideoFrameStage1Fields {
    VideoFrameStage1Fields {
        slot,
        time_ms: u64::from(slot) * 1_000,
        decoded: slot < 4,
        width: (slot < 4).then_some(1920),
        height: (slot < 4).then_some(1080),
        pdq: (slot < 4).then_some(PdqHash::from_bytes([slot; 32])),
        quality: (slot < 4).then_some(80),
    }
}

fn stage2(seed: u64) -> ImageStage2 {
    ImageStage2 {
        phash_parts: [seed; 9],
        sobel: [seed as f32; 128],
    }
}

/// 图片二筛应在一个无任务事务中写入 SQLite 和 outbox，并返回真实高水位。
#[test]
fn taskless_image_stage2_commits_without_task_rows() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-stage2-image.jpg", 100),
            [1; 16],
            MediaKind::Other,
        )
        .unwrap();
    store
        .commit_scan_stage1_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage1(image_stage1())],
        )
        .unwrap();
    let before = store.outbox_high_seq().unwrap();
    let expected = stage2(11);

    let committed = store
        .commit_stage2_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage2(expected)],
        )
        .unwrap();

    assert!(committed > before);
    assert_eq!(committed, store.outbox_high_seq().unwrap());
    assert_eq!(store.page_tasks(None, 10).unwrap().items.len(), 0);
    assert!(matches!(
        store.load_complete_stage2(content.id).unwrap(),
        Some(dedup_node_store::CompleteStage2::Image(value)) if *value == expected
    ));
    assert_eq!(
        store
            .pull_changes(before, 10)
            .unwrap()
            .changes
            .into_iter()
            .map(|change| change.entity_kind)
            .collect::<Vec<_>>(),
        ["image_stage2"]
    );
}

/// 全零 pHash/Sobel 可以是合法的纯色媒体结果，不应被当作占位值拒绝。
#[test]
fn taskless_stage2_accepts_legal_zero_features() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-stage2-zero.jpg", 150),
            [6; 16],
            MediaKind::Other,
        )
        .unwrap();
    store
        .commit_scan_stage1_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage1(image_stage1())],
        )
        .unwrap();
    let zero = ImageStage2 {
        phash_parts: [0; 9],
        sobel: [0.0; 128],
    };

    store
        .commit_stage2_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage2(zero)],
        )
        .unwrap();

    assert!(matches!(
        store.load_complete_stage2(content.id).unwrap(),
        Some(dedup_node_store::CompleteStage2::Image(value)) if *value == zero
    ));
}

/// 视频二筛只写调用方指定的成功槽位，不创建任务行或伪造其他槽位。
#[test]
fn taskless_video_stage2_commits_selected_slots_without_task_rows() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-stage2-video.mp4", 200),
            [2; 16],
            MediaKind::Other,
        )
        .unwrap();
    let mut stage1 = vec![FeatureWrite::VideoMetadata(VideoMetadataFields {
        duration_ms: Some(12_345),
        width: Some(3840),
        height: Some(2160),
    })];
    for slot in 0..6 {
        stage1.push(FeatureWrite::VideoFrameStage1(video_frame(slot)));
    }
    store
        .commit_scan_stage1_taskless(content.id, MediaKind::Video, stage1)
        .unwrap();
    let before = store.outbox_high_seq().unwrap();
    let slot_two = stage2(22);
    let slot_five = stage2(55);

    let committed = store
        .commit_stage2_taskless(
            content.id,
            MediaKind::Video,
            vec![
                FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                    slot: 2,
                    features: slot_two,
                }),
                FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                    slot: 5,
                    features: slot_five,
                }),
            ],
        )
        .unwrap();

    assert!(committed > before);
    assert_eq!(committed, store.outbox_high_seq().unwrap());
    assert_eq!(store.page_tasks(None, 10).unwrap().items.len(), 0);
    let cache = store.load_base_cache_record(content.id).unwrap();
    assert_eq!(cache.video_stage2[2], Some(slot_two));
    assert_eq!(cache.video_stage2[5], Some(slot_five));
    assert!(cache.video_stage2[0].is_none());
    assert!(cache.video_stage2[1].is_none());
    assert_eq!(
        store
            .pull_changes(before, 10)
            .unwrap()
            .changes
            .into_iter()
            .map(|change| change.entity_kind)
            .collect::<Vec<_>>(),
        ["video_frame_stage2", "video_frame_stage2"]
    );
}

/// 二筛事务只能接受对应的二筛结果；后续非法值必须回滚先前写入。
#[test]
fn taskless_stage2_rejects_stage1_and_rolls_back_atomically() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-stage2-rollback.jpg", 300),
            [3; 16],
            MediaKind::Other,
        )
        .unwrap();
    store
        .commit_scan_stage1_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage1(image_stage1())],
        )
        .unwrap();
    let original = stage2(33);
    store
        .commit_stage2_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage2(original)],
        )
        .unwrap();
    let before = store.outbox_high_seq().unwrap();

    let error = store
        .commit_stage2_taskless(
            content.id,
            MediaKind::Image,
            vec![
                FeatureWrite::ImageStage2(stage2(44)),
                FeatureWrite::ImageStage1(image_stage1()),
            ],
        )
        .unwrap_err();

    assert!(matches!(
        error,
        dedup_node_store::StoreError::InvalidFeature("二筛事务只能写入二筛结果")
    ));
    assert_eq!(store.outbox_high_seq().unwrap(), before);
    assert_eq!(store.page_tasks(None, 10).unwrap().items.len(), 0);
    assert!(matches!(
        store.load_complete_stage2(content.id).unwrap(),
        Some(dedup_node_store::CompleteStage2::Image(value)) if *value == original
    ));
}

/// 非有限二筛值是无效占位，不能覆盖既有有效结果或产生 outbox。
#[test]
fn taskless_stage2_rejects_non_finite_features_without_overwrite() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-stage2-nan.jpg", 400),
            [4; 16],
            MediaKind::Other,
        )
        .unwrap();
    store
        .commit_scan_stage1_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage1(image_stage1())],
        )
        .unwrap();
    let original = stage2(66);
    store
        .commit_stage2_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage2(original)],
        )
        .unwrap();
    let before = store.outbox_high_seq().unwrap();
    let mut invalid = stage2(77);
    invalid.sobel[17] = f32::NAN;

    let error = store
        .commit_stage2_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage2(invalid)],
        )
        .unwrap_err();

    assert!(matches!(
        error,
        dedup_node_store::StoreError::NonFiniteSobel
    ));
    assert_eq!(store.outbox_high_seq().unwrap(), before);
    assert!(matches!(
        store.load_complete_stage2(content.id).unwrap(),
        Some(dedup_node_store::CompleteStage2::Image(value)) if *value == original
    ));
}

/// 二筛不能写入尚未完成基础计算的占位内容。
#[test]
fn taskless_stage2_rejects_placeholder_content() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-stage2-placeholder.jpg", 500),
            [5; 16],
            MediaKind::Image,
        )
        .unwrap();
    let before = store.outbox_high_seq().unwrap();

    let error = store
        .commit_stage2_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage2(stage2(88))],
        )
        .unwrap_err();

    assert!(matches!(
        error,
        dedup_node_store::StoreError::InvalidFeature("二筛要求基础计算已完成")
    ));
    assert_eq!(store.outbox_high_seq().unwrap(), before);
    assert!(store.load_complete_stage1(content.id).unwrap().is_none());
    assert!(store.load_complete_stage2(content.id).unwrap().is_none());
}

//! 无持久任务表的一筛提交事务行为测试。

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, PdqHash};
use dedup_node_store::{
    CompleteStage1, FeatureWrite, ImageStage1Fields, NodeStore, ScannedPath,
    VideoFrameStage1Fields, VideoMetadataFields,
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

fn image_stage1(quality: u8) -> ImageStage1Fields {
    ImageStage1Fields::from(ImageStage1 {
        width: 1920,
        height: 1080,
        pdq: PdqHash::from_bytes([7; 32]),
        quality,
    })
}

fn video_frame(slot: u8, decoded: bool) -> VideoFrameStage1Fields {
    VideoFrameStage1Fields {
        slot,
        time_ms: u64::from(slot) * 1_000,
        decoded,
        width: decoded.then_some(1920),
        height: decoded.then_some(1080),
        pdq: decoded.then_some(PdqHash::from_bytes([slot; 32])),
        quality: decoded.then_some(80),
    }
}

fn outbox_kinds(store: &NodeStore, after: u64) -> Vec<String> {
    store
        .pull_changes(after, 100)
        .unwrap()
        .changes
        .into_iter()
        .map(|change| change.entity_kind)
        .collect()
}

/// 一筛可以脱离 tasks/task_items/task_stages 提交完整图片缓存和 outbox。
#[test]
fn taskless_image_stage1_commits_without_task_rows() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-image.jpg", 100),
            [1; 16],
            MediaKind::Other,
        )
        .unwrap();
    let before = store.outbox_high_seq().unwrap();

    let committed = store
        .commit_scan_stage1_taskless(
            content.id,
            MediaKind::Image,
            vec![
                FeatureWrite::ImageStage1(image_stage1(0)),
                FeatureWrite::ContactSheet("contact/taskless-image.jpg".into()),
            ],
        )
        .unwrap();

    assert_eq!(committed, store.outbox_high_seq().unwrap());
    assert!(committed > before);
    assert_eq!(store.page_tasks(None, 10).unwrap().items.len(), 0);
    let cache = store.load_base_cache_record(content.id).unwrap();
    assert_eq!(cache.media_kind, MediaKind::Image);
    assert!(cache.base_complete);
    assert!(matches!(cache.stage1, Some(CompleteStage1::Image(_))));
    assert_eq!(
        store.contact_sheet_path(content.id).unwrap().as_deref(),
        Some("contact/taskless-image.jpg")
    );
    assert_eq!(
        outbox_kinds(&store, before),
        ["content", "image_stage1", "contact_sheet"]
    );
}

/// 视频元数据、六个槽位和联系表必须由同一个无任务事务提交。
#[test]
fn taskless_video_stage1_commits_all_feature_writes_together() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-video.mp4", 200),
            [2; 16],
            MediaKind::Other,
        )
        .unwrap();
    let before = store.outbox_high_seq().unwrap();
    let mut writes = vec![FeatureWrite::VideoMetadata(VideoMetadataFields {
        duration_ms: Some(12_345),
        width: Some(3840),
        height: Some(2160),
    })];
    for slot in 0..6 {
        writes.push(FeatureWrite::VideoFrameStage1(video_frame(slot, slot < 4)));
    }
    writes.push(FeatureWrite::ContactSheet(
        "contact/taskless-video.jpg".into(),
    ));

    let committed = store
        .commit_scan_stage1_taskless(content.id, MediaKind::Video, writes)
        .unwrap();

    assert_eq!(committed, store.outbox_high_seq().unwrap());
    assert_eq!(store.page_tasks(None, 10).unwrap().items.len(), 0);
    let cache = store.load_base_cache_record(content.id).unwrap();
    assert_eq!(cache.media_kind, MediaKind::Video);
    assert!(cache.base_complete);
    assert_eq!(cache.width, Some(3840));
    assert_eq!(cache.height, Some(2160));
    assert_eq!(cache.duration_ms, Some(12_345));
    let Some(CompleteStage1::Video(frames)) = cache.stage1 else {
        panic!("四个成功槽位应形成完整视频一筛");
    };
    assert_eq!(frames.iter().flatten().count(), 4);
    assert_eq!(outbox_kinds(&store, before).len(), 9);
    assert_eq!(outbox_kinds(&store, before).first().unwrap(), "content");
    assert_eq!(
        outbox_kinds(&store, before).last().unwrap(),
        "contact_sheet"
    );
}

/// 任一非法二筛写入都必须回滚之前的内容、特征和 outbox。
#[test]
fn taskless_stage1_rolls_back_when_a_late_stage2_write_is_present() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-rollback.jpg", 300),
            [3; 16],
            MediaKind::Other,
        )
        .unwrap();
    let before = store.outbox_high_seq().unwrap();
    let error = store
        .commit_scan_stage1_taskless(
            content.id,
            MediaKind::Image,
            vec![
                FeatureWrite::ImageStage1(image_stage1(80)),
                FeatureWrite::ImageStage2(dedup_media::ImageStage2 {
                    phash_parts: [9; 9],
                    sobel: [0.0; 128],
                }),
            ],
        )
        .unwrap_err();
    assert!(matches!(
        error,
        dedup_node_store::StoreError::InvalidFeature("扫描一筛事务不能写入二筛结果")
    ));
    assert_eq!(store.outbox_high_seq().unwrap(), before);
    assert_eq!(store.page_tasks(None, 10).unwrap().items.len(), 0);
    let cache = store.load_base_cache_record(content.id).unwrap();
    assert_eq!(cache.media_kind, MediaKind::Other);
    assert!(!cache.base_complete);
    assert!(cache.stage1.is_none());
}

/// 合法 Quality=0 和既有有效字段不能被默认值覆盖或降级。
#[test]
fn taskless_stage1_preserves_valid_zero_and_existing_fields() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\taskless-merge.jpg", 400),
            [4; 16],
            MediaKind::Other,
        )
        .unwrap();
    store
        .commit_scan_stage1_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage1(image_stage1(0))],
        )
        .unwrap();
    store
        .commit_scan_stage1_taskless(
            content.id,
            MediaKind::Image,
            vec![FeatureWrite::ImageStage1(ImageStage1Fields {
                width: Some(0),
                height: Some(0),
                pdq: None,
                quality: None,
            })],
        )
        .unwrap();

    let cache = store.load_base_cache_record(content.id).unwrap();
    assert_eq!(cache.width, Some(1920));
    assert_eq!(cache.height, Some(1080));
    assert!(matches!(
        cache.stage1,
        Some(CompleteStage1::Image(ImageStage1 { quality: 0, .. }))
    ));
}

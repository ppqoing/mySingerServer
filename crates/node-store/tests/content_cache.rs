//! SQLite 新库、路径缓存、内容复用和特征完整性的集成测试。

use dedup_core::{ContentKey, DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_store::{
    CompleteStage1, CompleteStage2, FeatureWrite, ImageStage1Fields, NodeStore, ScannedPath,
    StoreError, VideoFrameStage1Fields,
};
use rusqlite::Connection;
use tempfile::TempDir;

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

/// 空路径创建当前 V2 全量 schema，并写入稳定产品标记。
#[test]
fn creates_current_schema_only_for_new_database() {
    let directory = TempDir::new().unwrap();
    let path = directory.path().join("node.sqlite3");
    let store = NodeStore::open(&path, machine()).unwrap();
    assert_eq!(store.schema_id().unwrap(), "mysingerserver-rust-v2");
    assert_eq!(store.machine_id(), &machine());
}

/// 已含任意旧表但没有 V2 标记的数据库必须拒绝，不能偷偷迁移或覆盖。
#[test]
fn rejects_database_without_v2_marker() {
    let directory = TempDir::new().unwrap();
    let path = directory.path().join("legacy.sqlite3");
    Connection::open(&path)
        .unwrap()
        .execute("CREATE TABLE legacy_files(id INTEGER)", [])
        .unwrap();
    assert!(matches!(
        NodeStore::open(&path, machine()),
        Err(StoreError::IncompatibleSchema)
    ));
}

/// 跳过 MD5 的键只有物理机器、规范路径和文件大小，不读取修改时间。
#[test]
fn cache_key_is_machine_path_and_size_only() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let scanned = scan(r"D:\a.jpg", 99);
    let inserted = store
        .upsert_content_and_location(&scanned, [0x11; 16], MediaKind::Image)
        .unwrap();

    let hit = store
        .lookup_scanned_paths(&[scan(r"d:\A.jpg", 99)])
        .unwrap();
    assert!(hit[0].is_reusable());
    assert_eq!(hit[0].content_key(), Some(inserted.key));
    let miss = store
        .lookup_scanned_paths(&[scan(r"D:\a.jpg", 100)])
        .unwrap();
    assert!(!miss[0].is_reusable());
}

/// MD5 只是查询索引；大小不同必须保留两个内容行。
#[test]
fn equal_md5_with_different_size_does_not_reuse_content() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let first = store
        .upsert_content_and_location(&scan(r"D:\a.bin", 99), [0x22; 16], MediaKind::Other)
        .unwrap();
    let second = store
        .upsert_content_and_location(&scan(r"D:\b.bin", 100), [0x22; 16], MediaKind::Other)
        .unwrap();
    assert_ne!(first.id, second.id);
    assert_ne!(first.key, second.key);
}

/// 新内容的 Other 只是探测前占位；只有显式完成后才能作为基础缓存命中。
#[test]
fn base_complete_distinguishes_placeholder_other_from_confirmed_other() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(&scan(r"D:\unknown.bin", 12), [0x24; 16], MediaKind::Other)
        .unwrap();

    assert!(
        !store
            .load_base_cache_record(content.id)
            .unwrap()
            .base_complete
    );
    store.mark_base_complete(content.id).unwrap();
    assert!(
        store
            .load_base_cache_record(content.id)
            .unwrap()
            .base_complete
    );
}

/// 图片一筛四个字段必须全部存在；查询不会补算缺失 Quality。
#[test]
fn image_stage1_requires_width_height_pdq_and_quality() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(&scan(r"D:\a.jpg", 99), [0x33; 16], MediaKind::Image)
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields {
                width: Some(100),
                height: Some(80),
                pdq: Some(PdqHash::from_bytes([3; 32])),
                quality: None,
            }),
        )
        .unwrap();
    assert!(store.load_complete_stage1(content.id).unwrap().is_none());

    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                width: 100,
                height: 80,
                pdq: PdqHash::from_bytes([3; 32]),
                quality: 70,
            })),
        )
        .unwrap();
    assert!(matches!(
        store.load_complete_stage1(content.id).unwrap(),
        Some(CompleteStage1::Image(_))
    ));
}

fn successful_video_frame(slot: u8) -> VideoFrameStage1Fields {
    VideoFrameStage1Fields {
        slot,
        time_ms: u64::from(slot) * 1000,
        decoded: true,
        width: Some(1920),
        height: Some(1080),
        pdq: Some(PdqHash::from_bytes([slot; 32])),
        quality: Some(80),
    }
}

fn failed_video_frame(slot: u8) -> VideoFrameStage1Fields {
    VideoFrameStage1Fields {
        slot,
        time_ms: u64::from(slot) * 1000,
        decoded: false,
        width: None,
        height: None,
        pdq: None,
        quality: None,
    }
}

/// 视频必须有六个槽位记录，且至少四个成功槽位的一筛字段完整。
#[test]
fn video_stage1_requires_six_slots_and_four_complete_successes() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(&scan(r"D:\a.mp4", 999), [0x44; 16], MediaKind::Video)
        .unwrap();
    for slot in 0..6 {
        let frame = if slot < 3 {
            successful_video_frame(slot)
        } else {
            failed_video_frame(slot)
        };
        store
            .commit_feature_result(content.id, None, FeatureWrite::VideoFrameStage1(frame))
            .unwrap();
    }
    assert!(store.load_complete_stage1(content.id).unwrap().is_none());

    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::VideoFrameStage1(successful_video_frame(3)),
        )
        .unwrap();
    let Some(CompleteStage1::Video(frames)) = store.load_complete_stage1(content.id).unwrap()
    else {
        panic!("四个成功槽位应形成完整视频一筛");
    };
    assert_eq!(frames.iter().flatten().count(), 4);
}

/// 图片二筛只有 pHash 和有限 Sobel 同时提交后才可复用。
#[test]
fn image_stage2_is_loaded_only_as_joint_result() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(&scan(r"D:\a.jpg", 99), [0x55; 16], MediaKind::Image)
        .unwrap();
    let stage2 = ImageStage2 {
        phash_parts: [7; 9],
        sobel: [0.0; 128],
    };
    store
        .commit_feature_result(content.id, None, FeatureWrite::ImageStage2(stage2))
        .unwrap();
    assert!(matches!(
        store.load_complete_stage2(content.id).unwrap(),
        Some(CompleteStage2::Image(value)) if *value == stage2
    ));
}

/// 路径批量查询必须保持输入位置、重复项和缺失项，并一次还原媒体特征。
#[test]
fn base_cache_path_batch_preserves_positions_and_features() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let image = store
        .upsert_content_and_location(&scan(r"D:\image.jpg", 11), [0x61; 16], MediaKind::Image)
        .unwrap();
    store
        .commit_feature_result(
            image.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields {
                width: Some(640),
                height: Some(480),
                pdq: Some(PdqHash::from_bytes([1; 32])),
                quality: Some(91),
            }),
        )
        .unwrap();

    let video = store
        .upsert_content_and_location(&scan(r"D:\video.mp4", 22), [0x62; 16], MediaKind::Video)
        .unwrap();
    store
        .commit_feature_result(
            video.id,
            None,
            FeatureWrite::VideoMetadata(dedup_node_store::VideoMetadataFields {
                duration_ms: Some(12_000),
                width: Some(1920),
                height: Some(1080),
            }),
        )
        .unwrap();
    for slot in 0..6 {
        store
            .commit_feature_result(
                video.id,
                None,
                FeatureWrite::VideoFrameStage1(successful_video_frame(slot)),
            )
            .unwrap();
    }

    let other = store
        .upsert_content_and_location(&scan(r"D:\other.bin", 33), [0x63; 16], MediaKind::Other)
        .unwrap();
    let inputs = vec![
        scan(r"D:\missing.bin", 44),
        scan(r"d:\VIDEO.MP4", 22),
        scan(r"D:\image.jpg", 11),
        scan(r"D:\other.bin", 33),
        scan(r"D:\video.mp4", 22),
    ];

    let results = store.lookup_base_cache_by_paths(&inputs).unwrap();
    assert_eq!(results.len(), inputs.len());
    assert!(results[0].is_none());
    assert_eq!(results[1].as_ref().unwrap().content_id, Some(video.id));
    assert_eq!(results[2].as_ref().unwrap().content_id, Some(image.id));
    assert_eq!(results[3].as_ref().unwrap().content_id, Some(other.id));
    assert_eq!(results[4], results[1]);
    assert!(matches!(
        results[1].as_ref().unwrap().stage1,
        Some(CompleteStage1::Video(ref frames)) if frames.iter().all(Option::is_some)
    ));
    assert!(matches!(
        results[2].as_ref().unwrap().stage1,
        Some(CompleteStage1::Image(feature)) if feature.width == 640 && feature.quality == 91
    ));
}

/// 内容键批量查询必须区分相同 MD5 的不同大小，并保持重复键的结果位置。
#[test]
fn base_cache_key_batch_preserves_duplicates_and_size() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let first = store
        .upsert_content_and_location(&scan(r"D:\first.bin", 51), [0x71; 16], MediaKind::Other)
        .unwrap();
    let second = store
        .upsert_content_and_location(&scan(r"D:\second.bin", 52), [0x71; 16], MediaKind::Other)
        .unwrap();
    let keys = vec![
        first.key,
        ContentKey::new([0x71; 16], 999),
        second.key,
        first.key,
        ContentKey::new([0x72; 16], 51),
    ];

    let results = store.lookup_base_cache_by_keys(&keys).unwrap();
    assert_eq!(results.len(), keys.len());
    assert_eq!(results[0].as_ref().unwrap().content_id, Some(first.id));
    assert!(results[1].is_none());
    assert_eq!(results[2].as_ref().unwrap().content_id, Some(second.id));
    assert_eq!(results[3], results[0]);
    assert!(results[4].is_none());
}

/// 空批次必须直接返回，不能为了构造 SQL 产生任何数据库调用。
#[test]
fn base_cache_batch_empty_input_is_empty() {
    let store = NodeStore::open_in_memory(machine()).unwrap();
    assert!(store.lookup_base_cache_by_paths(&[]).unwrap().is_empty());
    assert!(store.lookup_base_cache_by_keys(&[]).unwrap().is_empty());
}

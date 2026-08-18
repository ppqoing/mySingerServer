//! SQLite 新库、路径缓存、内容复用和特征完整性的集成测试。

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
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

//! SQLite 新库、路径缓存、内容复用和特征完整性的集成测试。

use dedup_core::{ContentKey, DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_node_store::{
    CacheCompleteness, CompleteStage1, CompleteStage2, FeatureWrite, ImageStage1Fields, NodeStore,
    ScannedPath, StoreError, VideoFrameStage1Fields, VideoFrameStage2Fields, VideoMetadataFields,
    classify_cache_completeness,
};
use rusqlite::{Connection, params};
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

/// 单项内容键查询只返回对应的完整基础缓存，并对未知键返回缺失。
#[test]
fn lookup_base_cache_by_key_returns_one_complete_record() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(&scan(r"C:\single.bin", 31), [0x31; 16], MediaKind::Other)
        .unwrap();
    store.mark_base_complete(content.id).unwrap();
    assert_eq!(
        store
            .lookup_base_cache_by_key(&content.key)
            .unwrap()
            .unwrap()
            .content_key,
        content.key
    );
    assert!(
        store
            .lookup_base_cache_by_key(&ContentKey::new([0xFF; 16], 999))
            .unwrap()
            .is_none()
    );
}

/// 空批次必须直接返回，不能为了构造 SQL 产生任何数据库调用。
#[test]
fn base_cache_batch_empty_input_is_empty() {
    let store = NodeStore::open_in_memory(machine()).unwrap();
    assert!(store.lookup_base_cache_by_paths(&[]).unwrap().is_empty());
    assert!(store.lookup_base_cache_by_keys(&[]).unwrap().is_empty());
}

/// 结构完整性分类器必须先区分完整缓存与结构性缺失，而不是依赖字段数值。
#[test]
fn valid_zero_features_are_hits_but_structural_gaps_are_missing() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(&scan(r"D:\zero.jpg", 99), [0x73; 16], MediaKind::Image)
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields {
                width: Some(100),
                height: Some(80),
                pdq: Some(PdqHash::from_bytes([0; 32])),
                quality: Some(0),
            }),
        )
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage2(ImageStage2 {
                phash_parts: [0; 9],
                sobel: [0.0; 128],
            }),
        )
        .unwrap();
    store.mark_base_complete(content.id).unwrap();

    let record = store.load_base_cache_record(content.id).unwrap();
    assert_eq!(
        classify_cache_completeness(&record, true),
        CacheCompleteness::complete()
    );
}

/// 基础缓存批量入口同时还原图片二筛和视频逐槽二筛，重复键保持同一原始结构。
#[test]
fn base_cache_batch_loads_stage2_fields_and_video_slots() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let image = store
        .upsert_content_and_location(
            &scan(r"D:\batch-image.jpg", 101),
            [0x81; 16],
            MediaKind::Image,
        )
        .unwrap();
    store
        .commit_feature_result(
            image.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                width: 100,
                height: 80,
                pdq: PdqHash::from_bytes([0; 32]),
                quality: 0,
            })),
        )
        .unwrap();
    store
        .commit_feature_result(
            image.id,
            None,
            FeatureWrite::ImageStage2(ImageStage2 {
                phash_parts: [0; 9],
                sobel: [0.0; 128],
            }),
        )
        .unwrap();

    let video = store
        .upsert_content_and_location(
            &scan(r"D:\batch-video.mp4", 202),
            [0x82; 16],
            MediaKind::Video,
        )
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
    for slot in [1_u8, 4] {
        store
            .commit_feature_result(
                video.id,
                None,
                FeatureWrite::VideoFrameStage2(dedup_node_store::VideoFrameStage2Fields {
                    slot,
                    features: ImageStage2 {
                        phash_parts: [slot as u64; 9],
                        sobel: [0.0; 128],
                    },
                }),
            )
            .unwrap();
    }

    let results = store
        .lookup_base_cache_by_keys(&[image.key, video.key, image.key])
        .unwrap();
    assert_eq!(results.len(), 3);
    assert!(results[0].as_ref().unwrap().image_stage2.is_some());
    assert_eq!(results[0], results[2]);
    let video_record = results[1].as_ref().unwrap();
    assert!(video_record.video_stage2[1].is_some());
    assert!(video_record.video_stage2[4].is_some());
    assert!(video_record.video_stage2[0].is_none());
}

/// 视频二筛缺失掩码只覆盖一筛成功槽位，不把明确失败槽位伪装成待计算。
#[test]
fn video_stage2_mask_only_covers_successful_stage1_slots() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(&scan(r"D:\masked.mp4", 303), [0x83; 16], MediaKind::Video)
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::VideoMetadata(dedup_node_store::VideoMetadataFields {
                duration_ms: Some(12_000),
                width: Some(1920),
                height: Some(1080),
            }),
        )
        .unwrap();
    for slot in 0..6 {
        let frame = if slot < 4 {
            successful_video_frame(slot)
        } else {
            failed_video_frame(slot)
        };
        store
            .commit_feature_result(content.id, None, FeatureWrite::VideoFrameStage1(frame))
            .unwrap();
    }
    for slot in [0_u8, 1] {
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoFrameStage2(dedup_node_store::VideoFrameStage2Fields {
                    slot,
                    features: ImageStage2 {
                        phash_parts: [0; 9],
                        sobel: [0.0; 128],
                    },
                }),
            )
            .unwrap();
    }
    store.mark_base_complete(content.id).unwrap();

    let record = store.load_base_cache_record(content.id).unwrap();
    assert_eq!(
        classify_cache_completeness(&record, true).video_stage2_missing_slots,
        (1_u8 << 2) | (1_u8 << 3)
    );
}

/// 非法尺寸和错误长度只使当前缓存项缺失，批量中的邻项仍可正常返回。
#[test]
fn malformed_batch_fields_are_missing_without_poisoning_neighbors() {
    let directory = TempDir::new().unwrap();
    let database = directory.path().join("malformed-cache.db");
    let (bad_id, bad_path, good_path);
    {
        let mut store = NodeStore::open(&database, machine()).unwrap();
        let bad = store
            .upsert_content_and_location(&scan(r"D:\bad.jpg", 401), [0x84; 16], MediaKind::Image)
            .unwrap();
        bad_id = bad.id.as_i64();
        bad_path = scan(r"D:\bad.jpg", 401);
        store
            .commit_feature_result(
                bad.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                    width: 100,
                    height: 80,
                    pdq: PdqHash::from_bytes([1; 32]),
                    quality: 80,
                })),
            )
            .unwrap();
        store
            .commit_feature_result(
                bad.id,
                None,
                FeatureWrite::ImageStage2(ImageStage2 {
                    phash_parts: [1; 9],
                    sobel: [0.0; 128],
                }),
            )
            .unwrap();
        store.mark_base_complete(bad.id).unwrap();

        let good = store
            .upsert_content_and_location(&scan(r"D:\good.jpg", 402), [0x85; 16], MediaKind::Image)
            .unwrap();
        good_path = scan(r"D:\good.jpg", 402);
        store
            .commit_feature_result(
                good.id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                    width: 100,
                    height: 80,
                    pdq: PdqHash::from_bytes([2; 32]),
                    quality: 80,
                })),
            )
            .unwrap();
        store
            .commit_feature_result(
                good.id,
                None,
                FeatureWrite::ImageStage2(ImageStage2 {
                    phash_parts: [2; 9],
                    sobel: [0.0; 128],
                }),
            )
            .unwrap();
        store.mark_base_complete(good.id).unwrap();
    }
    let connection = Connection::open(&database).unwrap();
    connection
        .execute_batch("PRAGMA ignore_check_constraints=ON;")
        .unwrap();
    connection
        .execute(
            "UPDATE image_stage1 SET width=0 WHERE content_id=?1",
            [bad_id],
        )
        .unwrap();
    connection
        .execute(
            "UPDATE image_stage2 SET sobel=?1 WHERE content_id=?2",
            params![vec![0_u8], bad_id],
        )
        .unwrap();
    drop(connection);

    let store = NodeStore::open(&database, machine()).unwrap();
    let results = store
        .lookup_base_cache_by_paths(&[bad_path, good_path])
        .unwrap();
    let bad = results[0].as_ref().expect("损坏内容行本身仍应返回结构记录");
    assert!(bad.stage1.is_none(), "非法一筛行不得成为可消费完整结构");
    let bad_completeness = classify_cache_completeness(bad, true);
    assert_ne!(bad_completeness.base_missing_parts, 0);
    assert!(bad_completeness.image_stage2_missing);
    let good = results[1].as_ref().expect("邻项不应被损坏项拖垮");
    assert!(!classify_cache_completeness(good, true).image_stage2_missing);
}

/// Worker 的部分失败结果只能补充空字段，不能覆盖已有有效一筛字段。
#[test]
fn partial_stage1_failure_does_not_overwrite_existing_feature() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(&scan(r"D:\preserve.jpg", 501), [0x86; 16], MediaKind::Image)
        .unwrap();
    let original = ImageStage1 {
        width: 640,
        height: 480,
        pdq: PdqHash::from_bytes([9; 32]),
        quality: 90,
    };
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(original)),
        )
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields {
                width: None,
                height: None,
                pdq: None,
                quality: None,
            }),
        )
        .unwrap();
    assert!(matches!(
        store.load_complete_stage1(content.id).unwrap(),
        Some(CompleteStage1::Image(feature)) if feature == original
    ));
}

/// 部分一筛写入发布的 outbox 必须携带事务提交后的合并字段，而不是输入占位值。
#[test]
fn partial_stage1_outbox_payload_preserves_existing_fields() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\outbox-preserve.jpg", 503),
            [0x88; 16],
            MediaKind::Image,
        )
        .unwrap();
    let original = ImageStage1 {
        width: 640,
        height: 480,
        pdq: PdqHash::from_bytes([8; 32]),
        quality: 89,
    };
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(original)),
        )
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::default()),
        )
        .unwrap();

    let change = store
        .pull_changes(0, 1000)
        .unwrap()
        .changes
        .into_iter()
        .filter(|change| change.entity_kind == "image_stage1")
        .last()
        .expect("部分写入必须发布一筛 outbox");
    assert_eq!(decode_image_stage1_payload(&change.payload), original);
}

/// 图片一筛的非法 Quality 不得覆盖旧值，且 outbox 必须继续发布旧合法字段。
#[test]
fn invalid_image_quality_preserves_existing_feature_and_outbox() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\quality-preserve.jpg", 504),
            [0x89; 16],
            MediaKind::Image,
        )
        .unwrap();
    let original = ImageStage1 {
        width: 640,
        height: 480,
        pdq: PdqHash::from_bytes([7; 32]),
        quality: 90,
    };
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(original)),
        )
        .unwrap();
    let before = store.outbox_high_seq().unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields {
                width: None,
                height: None,
                pdq: None,
                quality: Some(101),
            }),
        )
        .unwrap();

    assert!(matches!(
        store.load_complete_stage1(content.id).unwrap(),
        Some(CompleteStage1::Image(feature)) if feature == original
    ));
    let change = store
        .pull_changes(before, 10)
        .unwrap()
        .changes
        .into_iter()
        .find(|change| change.entity_kind == "image_stage1")
        .expect("非法 Quality 仍需发布合并后的完整一筛");
    assert_eq!(decode_image_stage1_payload(&change.payload), original);
}

/// 视频槽位的非法 Quality 不得覆盖旧值，且 outbox 载荷仍保留旧合法 Quality。
#[test]
fn invalid_video_frame_quality_preserves_existing_feature_and_outbox() {
    let directory = TempDir::new().unwrap();
    let database = directory.path().join("quality-preserve-video.db");
    let mut store = NodeStore::open(&database, machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\quality-preserve.mp4", 505),
            [0x8a; 16],
            MediaKind::Video,
        )
        .unwrap();
    let original = successful_video_frame(0);
    store
        .commit_feature_result(content.id, None, FeatureWrite::VideoFrameStage1(original))
        .unwrap();
    let before = store.outbox_high_seq().unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                slot: 0,
                time_ms: original.time_ms,
                decoded: true,
                width: None,
                height: None,
                pdq: None,
                quality: Some(255),
            }),
        )
        .unwrap();

    let connection = Connection::open(&database).unwrap();
    let row: (i64, i64) = connection
        .query_row(
            "SELECT quality,time_ms FROM video_frame_stage1 WHERE content_id=?1 AND slot=0",
            [content.id.as_i64()],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )
        .unwrap();
    assert_eq!(row, (80, original.time_ms as i64));
    let change = store
        .pull_changes(before, 10)
        .unwrap()
        .changes
        .into_iter()
        .find(|change| change.entity_kind == "video_frame_stage1")
        .expect("非法 Quality 仍需发布合并后的槽位一筛");
    assert_eq!(decode_video_frame_stage1_quality(&change.payload), 80);
}

/// 公开二筛重发入口必须拒绝 stage1 结构损坏的图片，不得新增 outbox。
#[test]
fn republish_complete_stage2_rejects_invalid_image_stage1() {
    let directory = TempDir::new().unwrap();
    let database = directory.path().join("republish-invalid-image.db");
    let machine_id = machine();
    let mut store = NodeStore::open(&database, machine_id.clone()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\republish-invalid.jpg", 506),
            [0x8b; 16],
            MediaKind::Image,
        )
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(ImageStage1 {
                width: 640,
                height: 480,
                pdq: PdqHash::from_bytes([6; 32]),
                quality: 90,
            })),
        )
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage2(ImageStage2 {
                phash_parts: [0; 9],
                sobel: [0.0; 128],
            }),
        )
        .unwrap();
    store.mark_base_complete(content.id).unwrap();
    let before = store.outbox_high_seq().unwrap();

    let connection = Connection::open(&database).unwrap();
    connection
        .execute_batch("PRAGMA ignore_check_constraints=ON;")
        .unwrap();
    connection
        .execute(
            "UPDATE image_stage1 SET width=0 WHERE content_id=?1",
            [content.id.as_i64()],
        )
        .unwrap();
    drop(connection);

    assert!(!store.republish_complete_stage2(content.id).unwrap());
    assert_eq!(store.outbox_high_seq().unwrap(), before);
}

/// 负视频时长必须保持探测缺失，不能转换成巨大的合法无符号时长或重发二筛。
#[test]
fn negative_video_duration_is_missing_and_blocks_stage2_republish() {
    let directory = TempDir::new().unwrap();
    let database = directory.path().join("republish-invalid-video.db");
    let machine_id = machine();
    let mut store = NodeStore::open(&database, machine_id.clone()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\republish-invalid.mp4", 507),
            [0x8c; 16],
            MediaKind::Video,
        )
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::VideoMetadata(VideoMetadataFields {
                duration_ms: Some(12_000),
                width: Some(1920),
                height: Some(1080),
            }),
        )
        .unwrap();
    for slot in 0..6 {
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoFrameStage1(successful_video_frame(slot)),
            )
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                    slot,
                    features: ImageStage2 {
                        phash_parts: [slot as u64; 9],
                        sobel: [0.0; 128],
                    },
                }),
            )
            .unwrap();
    }
    store.mark_base_complete(content.id).unwrap();
    let before = store.outbox_high_seq().unwrap();

    let connection = Connection::open(&database).unwrap();
    connection
        .execute(
            "UPDATE video_metadata SET duration_ms=-1 WHERE content_id=?1",
            [content.id.as_i64()],
        )
        .unwrap();
    drop(connection);

    let record = store.load_base_cache_record(content.id).unwrap();
    assert_eq!(record.duration_ms, None);
    assert_ne!(
        classify_cache_completeness(&record, true).base_missing_parts
            & dedup_protocol::BASE_MISSING_PROBE,
        0
    );
    assert!(!store.republish_complete_stage2(content.id).unwrap());
    assert_eq!(store.outbox_high_seq().unwrap(), before);
}

/// 解码 NodeStore 已提交的一筛载荷，验证对端看到的是合并后的有效结构。
fn decode_image_stage1_payload(payload: &[u8]) -> ImageStage1 {
    assert_eq!(payload.first().copied(), Some(1));
    let mut at = 1;
    let read_u8 = |payload: &[u8], at: &mut usize| {
        let value = *payload.get(*at).expect("载荷字段完整");
        *at += 1;
        value
    };
    let read_u32 = |payload: &[u8], at: &mut usize| {
        let end = (*at).checked_add(4).expect("载荷位置不溢出");
        let bytes: [u8; 4] = payload
            .get(*at..end)
            .expect("载荷字段完整")
            .try_into()
            .unwrap();
        *at = end;
        u32::from_be_bytes(bytes)
    };
    let read_bytes = |payload: &[u8], at: &mut usize| {
        let length = read_u32(payload, at) as usize;
        let end = (*at).checked_add(length).expect("载荷长度不溢出");
        let bytes = payload.get(*at..end).expect("载荷字段完整").to_vec();
        *at = end;
        bytes
    };
    let key_md5 = read_bytes(payload, &mut at);
    assert_eq!(key_md5.len(), 16);
    let _file_size = {
        let end = at + 8;
        let bytes: [u8; 8] = payload
            .get(at..end)
            .expect("内容键完整")
            .try_into()
            .unwrap();
        at = end;
        u64::from_be_bytes(bytes)
    };
    assert_eq!(read_u8(payload, &mut at), 1);
    let width = read_u32(payload, &mut at);
    assert_eq!(read_u8(payload, &mut at), 1);
    let height = read_u32(payload, &mut at);
    assert_eq!(read_u8(payload, &mut at), 1);
    let pdq = read_bytes(payload, &mut at);
    assert_eq!(read_u8(payload, &mut at), 1);
    let quality = read_u8(payload, &mut at);
    assert_eq!(at, payload.len());
    ImageStage1 {
        width,
        height,
        pdq: PdqHash::from_bytes(pdq.try_into().expect("PDQ 长度固定")),
        quality,
    }
}

/// 解码视频槽位一筛载荷的 Quality，验证 outbox 使用事务合并值。
fn decode_video_frame_stage1_quality(payload: &[u8]) -> u8 {
    assert_eq!(payload.first().copied(), Some(1));
    let mut at = 1;
    let read_u8 = |payload: &[u8], at: &mut usize| {
        let value = *payload.get(*at).expect("载荷字段完整");
        *at += 1;
        value
    };
    let read_u32 = |payload: &[u8], at: &mut usize| {
        let end = (*at).checked_add(4).expect("载荷位置不溢出");
        let bytes: [u8; 4] = payload
            .get(*at..end)
            .expect("载荷字段完整")
            .try_into()
            .unwrap();
        *at = end;
        u32::from_be_bytes(bytes)
    };
    let skip_bytes = |payload: &[u8], at: &mut usize| {
        let length = read_u32(payload, at) as usize;
        *at = (*at).checked_add(length).expect("载荷长度不溢出");
        assert!(*at <= payload.len(), "载荷字段完整");
    };
    skip_bytes(payload, &mut at);
    at = at.checked_add(8).expect("内容键完整");
    assert_eq!(read_u8(payload, &mut at), 0);
    at = at.checked_add(8).expect("时间字段完整");
    assert_eq!(read_u8(payload, &mut at), 1);
    for _ in 0..2 {
        if read_u8(payload, &mut at) == 1 {
            at = at.checked_add(4).expect("尺寸字段完整");
        }
    }
    if read_u8(payload, &mut at) == 1 {
        skip_bytes(payload, &mut at);
    }
    assert_eq!(read_u8(payload, &mut at), 1);
    let quality = read_u8(payload, &mut at);
    assert_eq!(at, payload.len());
    quality
}

/// Worker 返回非法零尺寸时只能保持缺失，不能覆盖已有有效一筛字段。
#[test]
fn invalid_stage1_dimensions_do_not_overwrite_existing_feature() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let content = store
        .upsert_content_and_location(
            &scan(r"D:\preserve-zero.jpg", 502),
            [0x87; 16],
            MediaKind::Image,
        )
        .unwrap();
    let original = ImageStage1 {
        width: 640,
        height: 480,
        pdq: PdqHash::from_bytes([9; 32]),
        quality: 90,
    };
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields::from(original)),
        )
        .unwrap();
    store
        .commit_feature_result(
            content.id,
            None,
            FeatureWrite::ImageStage1(ImageStage1Fields {
                width: Some(0),
                height: Some(0),
                pdq: None,
                quality: None,
            }),
        )
        .unwrap();

    assert!(matches!(
        store.load_complete_stage1(content.id).unwrap(),
        Some(CompleteStage1::Image(feature)) if feature == original
    ));
}

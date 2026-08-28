//! SQLite outbox ACK、裁剪、SnapshotRequired 与只读快照测试。

use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_store::{
    FeatureWrite, NodeStore, ScannedPath, StoreError, SyncState, VideoFrameStage1Fields,
    VideoMetadataFields,
};
use rusqlite::{Connection, params};
use tempfile::tempdir;

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

fn outbox_with_sequences(count: u8) -> NodeStore {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    for value in 1..=count {
        store.record_sync_change("test", vec![value]).unwrap();
    }
    store
}

/// ACK 先推进已提交边界，再只裁剪不大于该边界的 outbox 行。
#[test]
fn commit_then_ack_prunes_only_committed_rows() {
    let mut store = outbox_with_sequences(3);
    store.ack_changes(2).unwrap();
    assert_eq!(
        store.sync_state().unwrap(),
        SyncState {
            acked_seq: 2,
            pruned_through_seq: 2,
        }
    );
    assert_eq!(store.pull_changes(2, 1000).unwrap().sequences(), vec![3]);
}

/// 重复 ACK 幂等；越过本地最高序号的 ACK 只能推进到实际已提交上界。
#[test]
fn ack_is_idempotent_and_clamped_to_local_highwater() {
    let mut store = outbox_with_sequences(3);
    store.ack_changes(99).unwrap();
    store.ack_changes(99).unwrap();
    assert_eq!(
        store.sync_state().unwrap(),
        SyncState {
            acked_seq: 3,
            pruned_through_seq: 3,
        }
    );
    assert!(store.pull_changes(3, 1000).unwrap().changes.is_empty());
}

/// 中心游标早于本地已裁剪边界时必须要求全量快照。
#[test]
fn pull_before_pruned_boundary_requires_snapshot() {
    let mut store = outbox_with_sequences(3);
    store.ack_changes(2).unwrap();
    assert!(matches!(
        store.pull_changes(1, 1000),
        Err(StoreError::SnapshotRequired {
            requested_seq: 1,
            pruned_through_seq: 2
        })
    ));
}

/// 快照固定开始时的 outbox 高水位，并按表内主键稳定分页返回完整基础行。
#[test]
fn snapshot_has_fixed_highwater_and_stable_content_order() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    store
        .upsert_content_and_location(&scan(r"D:\b.bin", 2), [2; 16], MediaKind::Other)
        .unwrap();
    store
        .upsert_content_and_location(&scan(r"D:\a.bin", 1), [1; 16], MediaKind::Other)
        .unwrap();
    let expected_highwater = store.outbox_high_seq().unwrap();
    let snapshot = store.begin_snapshot().unwrap();
    assert_eq!(snapshot.high_seq(), expected_highwater);
    let first = snapshot.read_page("contents", "", 1).unwrap();
    assert_eq!(first.rows.len(), 1);
    assert!(!first.done);
    let second = snapshot
        .read_page("contents", first.next_cursor.as_deref().unwrap(), 1)
        .unwrap();
    assert_eq!(second.rows.len(), 1);
    assert!(second.done);
    assert!(first.rows[0].key < second.rows[0].key);
}

/// 网络快照使用独立只读连接；主 actor 后续写入不会改变已经建立的快照视图。
#[test]
fn owned_snapshot_keeps_one_read_transaction_across_pages() {
    let directory = tempfile::tempdir().unwrap();
    let database = directory.path().join("node.db");
    let mut store = NodeStore::open(&database, machine()).unwrap();
    store
        .upsert_content_and_location(&scan(r"D:\before.bin", 1), [1; 16], MediaKind::Other)
        .unwrap();
    let snapshot = store.begin_owned_snapshot().unwrap();
    let frozen_highwater = snapshot.high_seq();

    store
        .upsert_content_and_location(&scan(r"D:\after.bin", 2), [2; 16], MediaKind::Other)
        .unwrap();

    assert!(store.outbox_high_seq().unwrap() > frozen_highwater);
    let page = snapshot.read_page("contents", "", 1000).unwrap();
    assert_eq!(page.rows.len(), 1);
    assert!(matches!(
        snapshot.read_page("contact_sheets", "", 1000),
        Err(StoreError::InvalidSnapshotTable(_))
    ));
}

/// 快照跳过单条畸形视频元数据和槽位，仍按有效邻居推进游标并最终完成。
#[test]
fn snapshot_skips_malformed_video_rows_without_null_overwrite() {
    let directory = tempdir().unwrap();
    let database = directory.path().join("snapshot-malformed-video.db");
    let mut store = NodeStore::open(&database, machine()).unwrap();
    let mut content_ids = Vec::new();
    for (index, md5) in [[1_u8; 16], [2_u8; 16], [3_u8; 16]].into_iter().enumerate() {
        let content = store
            .upsert_content_and_location(
                &scan(&format!(r"D:\snapshot-{index}.mp4"), 100 + index as u64),
                md5,
                MediaKind::Video,
            )
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoMetadata(VideoMetadataFields {
                    duration_ms: Some(1_000),
                    width: Some(640),
                    height: Some(480),
                }),
            )
            .unwrap();
        store
            .commit_feature_result(
                content.id,
                None,
                FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                    slot: 0,
                    time_ms: 500,
                    decoded: false,
                    width: None,
                    height: None,
                    pdq: None,
                    quality: None,
                }),
            )
            .unwrap();
        content_ids.push(content.id);
    }

    let connection = Connection::open(&database).unwrap();
    connection
        .execute_batch("PRAGMA ignore_check_constraints=ON;")
        .unwrap();
    connection
        .execute(
            "UPDATE video_metadata SET duration_ms=-1 WHERE content_id=?1",
            params![content_ids[0].as_i64()],
        )
        .unwrap();
    connection
        .execute(
            "UPDATE video_frame_stage1 SET time_ms=-1 WHERE content_id=?1 AND slot=0",
            params![content_ids[0].as_i64()],
        )
        .unwrap();
    drop(connection);

    let snapshot = store.begin_snapshot().unwrap();
    let metadata_first = snapshot
        .read_page("video_metadata", "", 1)
        .expect("畸形 metadata 不得阻断快照");
    assert_eq!(metadata_first.rows.len(), 1);
    assert!(metadata_first.rows[0].key.starts_with("0202"));
    assert!(!metadata_first.done);
    let metadata_second = snapshot
        .read_page(
            "video_metadata",
            metadata_first.next_cursor.as_deref().unwrap(),
            1,
        )
        .unwrap();
    assert_eq!(metadata_second.rows.len(), 1);
    assert!(metadata_second.rows[0].key.starts_with("0303"));
    assert!(metadata_second.done);
    assert!(metadata_second.next_cursor.is_none());

    let frame_first = snapshot
        .read_page("video_frame_stage1", "", 1)
        .expect("畸形视频槽位不应降级整页");
    assert_eq!(frame_first.rows.len(), 1);
    assert!(frame_first.rows[0].key.starts_with("0202"));
    assert!(!frame_first.done);
    let frame_second = snapshot
        .read_page(
            "video_frame_stage1",
            frame_first.next_cursor.as_deref().unwrap(),
            1,
        )
        .unwrap();
    assert_eq!(frame_second.rows.len(), 1);
    assert!(frame_second.rows[0].key.starts_with("0303"));
    assert!(frame_second.done);
    assert!(frame_second.next_cursor.is_none());
}

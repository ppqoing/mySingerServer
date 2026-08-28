//! 中心缓存字段合并和部分同步载荷的 PostgreSQL 行为测试。

use dedup_core::{ContentKey, MachineId};
use dedup_protocol::proto;

/// 完整字段再次收到部分载荷时保留旧值，初次部分行随后可由完整行补齐。
#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn feature_upsert_preserves_complete_fields_and_fills_initial_partial_rows() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let mut store = dedup_central_store::CentralStore::connect(&url)
        .await
        .unwrap();
    let machine =
        MachineId::parse("9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a")
            .unwrap();
    let image = ContentKey::new([0xa1; 16], 101);
    let video = ContentKey::new([0xb2; 16], 202);
    let initial_partial = ContentKey::new([0xc3; 16], 303);

    store
        .apply_sync_batch(
            &machine,
            &batch(vec![
                content_change(1, image, 1, true),
                image_stage1_change(
                    2,
                    image,
                    Some(640),
                    Some(480),
                    Some(vec![0x11; 32]),
                    Some(90),
                ),
                image_stage2_change(3, image, vec![0x22; 72], vec![0x33; 512]),
                content_change(4, video, 2, true),
                video_metadata_change(5, video, Some(9_000), Some(1_920), Some(1_080)),
                video_frame_stage1_change(
                    6,
                    video,
                    0,
                    4_000,
                    true,
                    Some(1_920),
                    Some(1_080),
                    Some(vec![0x44; 32]),
                    Some(80),
                ),
                video_frame_stage2_change(7, video, 0, vec![0x55; 72], vec![0x66; 512]),
                content_change(8, initial_partial, 1, false),
                image_stage1_change(9, initial_partial, None, None, None, None),
                image_stage2_change(10, initial_partial, Vec::new(), Vec::new()),
            ]),
        )
        .await
        .unwrap();

    store
        .apply_sync_batch(
            &machine,
            &batch(vec![
                content_change(100, image, 1, false),
                image_stage1_change(101, image, None, None, None, Some(101)),
                image_stage2_change(104, image, Vec::new(), Vec::new()),
                video_metadata_change(102, video, None, None, None),
                video_frame_stage1_change(103, video, 0, 4_000, false, None, None, None, Some(255)),
                video_frame_stage2_change(105, video, 0, Vec::new(), Vec::new()),
            ]),
        )
        .await
        .unwrap();

    let partial_image = ContentKey::new([0xd4; 16], 404);
    store
        .apply_sync_batch(
            &machine,
            &batch(vec![
                content_change(201, partial_image, 1, false),
                image_stage1_change(202, partial_image, None, None, None, Some(0)),
            ]),
        )
        .await
        .unwrap();

    let (partial_client, partial_connection) = tokio_postgres::connect(&url, tokio_postgres::NoTls)
        .await
        .unwrap();
    let partial_connection_task = tokio::spawn(async move {
        let _ = partial_connection.await;
    });
    let partial_row = partial_client
        .query_one(
            "SELECT i.width,i.height,i.quality
             FROM image_stage1 i JOIN contents c ON c.content_id=i.content_id
             WHERE c.md5=$1 AND c.file_size=$2",
            &[
                &partial_image.md5().as_slice(),
                &(partial_image.file_size() as i64),
            ],
        )
        .await
        .unwrap();
    assert_eq!(partial_row.get::<_, Option<i32>>(0), None);
    assert_eq!(partial_row.get::<_, Option<i32>>(1), None);
    assert_eq!(partial_row.get::<_, Option<i16>>(2), Some(0));
    drop(partial_client);
    partial_connection_task.abort();

    store
        .apply_sync_batch(
            &machine,
            &batch(vec![
                image_stage1_change(
                    301,
                    initial_partial,
                    Some(800),
                    Some(600),
                    Some(vec![0x77; 32]),
                    Some(70),
                ),
                image_stage2_change(303, initial_partial, vec![0x99; 72], vec![0xaa; 512]),
                image_stage1_change(
                    302,
                    partial_image,
                    Some(1_024),
                    Some(768),
                    Some(vec![0x88; 32]),
                    Some(60),
                ),
            ]),
        )
        .await
        .unwrap();

    let (client, connection) = tokio_postgres::connect(&url, tokio_postgres::NoTls)
        .await
        .unwrap();
    let connection_task = tokio::spawn(async move {
        let _ = connection.await;
    });

    let image_row = client
        .query_one(
            "SELECT i.width,i.height,i.pdq,i.quality,c.base_complete
             FROM image_stage1 i JOIN contents c ON c.content_id=i.content_id
             WHERE c.md5=$1 AND c.file_size=$2",
            &[&image.md5().as_slice(), &(image.file_size() as i64)],
        )
        .await
        .unwrap();
    assert_eq!(image_row.get::<_, Option<i32>>(0), Some(640));
    assert_eq!(image_row.get::<_, Option<i32>>(1), Some(480));
    assert_eq!(image_row.get::<_, Option<Vec<u8>>>(2), Some(vec![0x11; 32]));
    assert_eq!(image_row.get::<_, Option<i16>>(3), Some(90));
    assert!(image_row.get::<_, bool>(4));

    let image_stage2_row = client
        .query_one(
            "SELECT i.phash_parts,i.sobel
             FROM image_stage2 i JOIN contents c ON c.content_id=i.content_id
             WHERE c.md5=$1 AND c.file_size=$2",
            &[&image.md5().as_slice(), &(image.file_size() as i64)],
        )
        .await
        .unwrap();
    assert_eq!(image_stage2_row.get::<_, Vec<u8>>(0), vec![0x22; 72]);
    assert_eq!(image_stage2_row.get::<_, Vec<u8>>(1), vec![0x33; 512]);

    let metadata_row = client
        .query_one(
            "SELECT v.duration_ms,v.width,v.height
             FROM video_metadata v JOIN contents c ON c.content_id=v.content_id
             WHERE c.md5=$1 AND c.file_size=$2",
            &[&video.md5().as_slice(), &(video.file_size() as i64)],
        )
        .await
        .unwrap();
    assert_eq!(metadata_row.get::<_, Option<i64>>(0), Some(9_000));
    assert_eq!(metadata_row.get::<_, Option<i32>>(1), Some(1_920));
    assert_eq!(metadata_row.get::<_, Option<i32>>(2), Some(1_080));

    let frame_row = client
        .query_one(
            "SELECT f.time_ms,f.decoded,f.width,f.height,f.pdq,f.quality
             FROM video_frame_stage1 f JOIN contents c ON c.content_id=f.content_id
             WHERE c.md5=$1 AND c.file_size=$2 AND f.slot=0",
            &[&video.md5().as_slice(), &(video.file_size() as i64)],
        )
        .await
        .unwrap();
    assert_eq!(frame_row.get::<_, i64>(0), 4_000);
    assert!(frame_row.get::<_, bool>(1));
    assert_eq!(frame_row.get::<_, Option<i32>>(2), Some(1_920));
    assert_eq!(frame_row.get::<_, Option<i32>>(3), Some(1_080));
    assert_eq!(frame_row.get::<_, Option<Vec<u8>>>(4), Some(vec![0x44; 32]));
    assert_eq!(frame_row.get::<_, Option<i16>>(5), Some(80));

    let frame_stage2_row = client
        .query_one(
            "SELECT f.phash_parts,f.sobel
             FROM video_frame_stage2 f JOIN contents c ON c.content_id=f.content_id
             WHERE c.md5=$1 AND c.file_size=$2 AND f.slot=0",
            &[&video.md5().as_slice(), &(video.file_size() as i64)],
        )
        .await
        .unwrap();
    assert_eq!(frame_stage2_row.get::<_, Vec<u8>>(0), vec![0x55; 72]);
    assert_eq!(frame_stage2_row.get::<_, Vec<u8>>(1), vec![0x66; 512]);

    let filled_row = client
        .query_one(
            "SELECT i.width,i.height,i.pdq,i.quality
             FROM image_stage1 i JOIN contents c ON c.content_id=i.content_id
             WHERE c.md5=$1 AND c.file_size=$2",
            &[
                &initial_partial.md5().as_slice(),
                &(initial_partial.file_size() as i64),
            ],
        )
        .await
        .unwrap();
    assert_eq!(filled_row.get::<_, Option<i32>>(0), Some(800));
    assert_eq!(filled_row.get::<_, Option<i32>>(1), Some(600));
    assert_eq!(
        filled_row.get::<_, Option<Vec<u8>>>(2),
        Some(vec![0x77; 32])
    );
    assert_eq!(filled_row.get::<_, Option<i16>>(3), Some(70));

    let filled_stage2_row = client
        .query_one(
            "SELECT i.phash_parts,i.sobel
             FROM image_stage2 i JOIN contents c ON c.content_id=i.content_id
             WHERE c.md5=$1 AND c.file_size=$2",
            &[
                &initial_partial.md5().as_slice(),
                &(initial_partial.file_size() as i64),
            ],
        )
        .await
        .unwrap();
    assert_eq!(
        filled_stage2_row.get::<_, Option<Vec<u8>>>(0),
        Some(vec![0x99; 72])
    );
    assert_eq!(
        filled_stage2_row.get::<_, Option<Vec<u8>>>(1),
        Some(vec![0xaa; 512])
    );

    let zero_quality_row = client
        .query_one(
            "SELECT i.width,i.height,i.pdq,i.quality
             FROM image_stage1 i JOIN contents c ON c.content_id=i.content_id
             WHERE c.md5=$1 AND c.file_size=$2",
            &[
                &partial_image.md5().as_slice(),
                &(partial_image.file_size() as i64),
            ],
        )
        .await
        .unwrap();
    assert_eq!(zero_quality_row.get::<_, Option<i16>>(3), Some(60));
    assert_eq!(zero_quality_row.get::<_, Option<i32>>(0), Some(1_024));

    drop(client);
    connection_task.abort();
}

/// 组装中心同步批次，保持输入序号已经由调用方明确指定。
fn batch(changes: Vec<proto::SyncChange>) -> proto::SyncChangeBatch {
    proto::SyncChangeBatch {
        changes,
        high_seq: 0,
        pruned_through_seq: 0,
    }
}

/// 生成版本化内容变更载荷。
fn content_change(
    seq: u64,
    key: ContentKey,
    media_kind: u8,
    base_complete: bool,
) -> proto::SyncChange {
    let payload = Payload::new(2)
        .bytes(&key.md5())
        .u64(key.file_size())
        .u8(media_kind)
        .u8(u8::from(base_complete))
        .finish();
    change(seq, "content", payload)
}

/// 生成图片一筛变更载荷。
fn image_stage1_change(
    seq: u64,
    key: ContentKey,
    width: Option<u32>,
    height: Option<u32>,
    pdq: Option<Vec<u8>>,
    quality: Option<u8>,
) -> proto::SyncChange {
    let payload = Payload::new(1)
        .bytes(&key.md5())
        .u64(key.file_size())
        .optional_u32(width)
        .optional_u32(height)
        .optional_bytes(pdq.as_deref())
        .optional_u8(quality)
        .finish();
    change(seq, "image_stage1", payload)
}

/// 生成图片二筛变更载荷。
fn image_stage2_change(
    seq: u64,
    key: ContentKey,
    phash_parts: Vec<u8>,
    sobel: Vec<u8>,
) -> proto::SyncChange {
    let payload = Payload::new(1)
        .bytes(&key.md5())
        .u64(key.file_size())
        .bytes(&phash_parts)
        .bytes(&sobel)
        .finish();
    change(seq, "image_stage2", payload)
}

/// 生成视频元数据变更载荷。
fn video_metadata_change(
    seq: u64,
    key: ContentKey,
    duration_ms: Option<u64>,
    width: Option<u32>,
    height: Option<u32>,
) -> proto::SyncChange {
    let payload = Payload::new(1)
        .bytes(&key.md5())
        .u64(key.file_size())
        .optional_u64(duration_ms)
        .optional_u32(width)
        .optional_u32(height)
        .finish();
    change(seq, "video_metadata", payload)
}

/// 生成视频一筛槽位变更载荷。
#[allow(clippy::too_many_arguments)]
fn video_frame_stage1_change(
    seq: u64,
    key: ContentKey,
    slot: u8,
    time_ms: u64,
    decoded: bool,
    width: Option<u32>,
    height: Option<u32>,
    pdq: Option<Vec<u8>>,
    quality: Option<u8>,
) -> proto::SyncChange {
    let payload = Payload::new(1)
        .bytes(&key.md5())
        .u64(key.file_size())
        .u8(slot)
        .u64(time_ms)
        .u8(u8::from(decoded))
        .optional_u32(width)
        .optional_u32(height)
        .optional_bytes(pdq.as_deref())
        .optional_u8(quality)
        .finish();
    change(seq, "video_frame_stage1", payload)
}

/// 生成视频二筛槽位变更载荷。
fn video_frame_stage2_change(
    seq: u64,
    key: ContentKey,
    slot: u8,
    phash_parts: Vec<u8>,
    sobel: Vec<u8>,
) -> proto::SyncChange {
    let payload = Payload::new(1)
        .bytes(&key.md5())
        .u64(key.file_size())
        .u8(slot)
        .bytes(&phash_parts)
        .bytes(&sobel)
        .finish();
    change(seq, "video_frame_stage2", payload)
}

/// 组装带实体类型的同步变更。
fn change(seq: u64, entity_kind: &str, payload: Vec<u8>) -> proto::SyncChange {
    proto::SyncChange {
        seq,
        entity_kind: entity_kind.into(),
        payload,
    }
}

/// 测试使用的版本化大端字段编码器。
struct Payload(Vec<u8>);

impl Payload {
    /// 创建指定版本的载荷。
    fn new(version: u8) -> Self {
        Self(vec![version])
    }

    /// 写入长度前缀字节串。
    fn bytes(mut self, value: &[u8]) -> Self {
        self.0
            .extend_from_slice(&(value.len() as u32).to_be_bytes());
        self.0.extend_from_slice(value);
        self
    }

    /// 写入无符号 64 位整数。
    fn u64(mut self, value: u64) -> Self {
        self.0.extend_from_slice(&value.to_be_bytes());
        self
    }

    /// 写入无符号 8 位整数。
    fn u8(mut self, value: u8) -> Self {
        self.0.push(value);
        self
    }

    /// 写入可选无符号 64 位整数。
    fn optional_u64(self, value: Option<u64>) -> Self {
        match value {
            Some(value) => self.u8(1).u64(value),
            None => self.u8(0),
        }
    }

    /// 写入可选无符号 32 位整数。
    fn optional_u32(self, value: Option<u32>) -> Self {
        match value {
            Some(value) => self.u8(1).u32(value),
            None => self.u8(0),
        }
    }

    /// 写入无符号 32 位整数。
    fn u32(mut self, value: u32) -> Self {
        self.0.extend_from_slice(&value.to_be_bytes());
        self
    }

    /// 写入可选字节串。
    fn optional_bytes(self, value: Option<&[u8]>) -> Self {
        match value {
            Some(value) => self.u8(1).bytes(value),
            None => self.u8(0),
        }
    }

    /// 写入可选无符号 8 位整数。
    fn optional_u8(self, value: Option<u8>) -> Self {
        match value {
            Some(value) => self.u8(1).u8(value),
            None => self.u8(0),
        }
    }

    /// 取出已完成的载荷。
    fn finish(self) -> Vec<u8> {
        self.0
    }
}

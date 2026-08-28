//! 中心缓存字段合并和部分同步载荷的 PostgreSQL 行为测试。

use std::fmt::Debug;

use dedup_core::{ContentKey, MachineId};
use dedup_protocol::proto;
use tokio_postgres::types::{FromSqlOwned, ToSql};
use tokio_postgres::{Client, Row};
use uuid::Uuid;

/// 完整字段再次收到部分载荷时保留旧值，初次部分行随后可由完整行补齐。
#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn feature_upsert_preserves_complete_fields_and_fills_initial_partial_rows() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    if let Err(error) = run_feature_upsert_case(&url).await {
        panic!("{error}");
    }
}

/// 执行一个带唯一键和异步清理守卫的中心缓存行为用例。
async fn run_feature_upsert_case(url: &str) -> Result<(), String> {
    let data = TestData::new();
    let cleanup = CleanupGuard::new(url, data.clone());
    let case_result = run_feature_upsert_case_body(url, &data).await;
    let cleanup_result = cleanup.cleanup().await;
    match (case_result, cleanup_result) {
        (Ok(()), Ok(())) => Ok(()),
        (Err(case_error), Ok(())) => Err(case_error),
        (Ok(()), Err(cleanup_error)) => Err(format!("清理中心测试数据失败: {cleanup_error}")),
        (Err(case_error), Err(cleanup_error)) => Err(format!(
            "{case_error}；清理中心测试数据失败: {cleanup_error}"
        )),
    }
}

/// 执行中心同步和查询断言；所有失败均通过 Result 返回给外层清理守卫。
async fn run_feature_upsert_case_body(url: &str, data: &TestData) -> Result<(), String> {
    let mut store = dedup_central_store::CentralStore::connect(url)
        .await
        .map_err(|error| format!("连接中心失败: {error}"))?;
    store
        .apply_sync_batch(
            &data.machine,
            &batch(vec![
                content_change(1, data.image, 1, true),
                image_stage1_change(
                    2,
                    data.image,
                    Some(640),
                    Some(480),
                    Some(vec![0x11; 32]),
                    Some(90),
                ),
                image_stage2_change(3, data.image, vec![0x22; 72], vec![0x33; 512]),
                content_change(4, data.video, 2, true),
                video_metadata_change(5, data.video, Some(9_000), Some(1_920), Some(1_080)),
                video_frame_stage1_change(
                    6,
                    data.video,
                    0,
                    4_000,
                    true,
                    Some(1_920),
                    Some(1_080),
                    Some(vec![0x44; 32]),
                    Some(80),
                ),
                video_frame_stage2_change(7, data.video, 0, vec![0x55; 72], vec![0x66; 512]),
                content_change(8, data.initial_partial, 1, false),
                image_stage1_change(9, data.initial_partial, None, None, None, None),
                image_stage2_change(10, data.initial_partial, Vec::new(), Vec::new()),
                file_change(11, &data.machine, &data.normalized_path, data.image),
            ]),
        )
        .await
        .map_err(|error| format!("写入完整中心特征失败: {error}"))?;

    store
        .apply_sync_batch(
            &data.machine,
            &batch(vec![
                content_change(100, data.image, 1, false),
                image_stage1_change(101, data.image, None, None, None, Some(101)),
                image_stage2_change(104, data.image, Vec::new(), Vec::new()),
                video_metadata_change(102, data.video, None, None, None),
                video_frame_stage1_change(
                    103,
                    data.video,
                    0,
                    4_000,
                    false,
                    None,
                    None,
                    None,
                    Some(255),
                ),
                video_frame_stage2_change(105, data.video, 0, Vec::new(), Vec::new()),
            ]),
        )
        .await
        .map_err(|error| format!("写入部分中心特征失败: {error}"))?;

    store
        .apply_sync_batch(
            &data.machine,
            &batch(vec![
                content_change(201, data.partial_image, 1, false),
                image_stage1_change(202, data.partial_image, None, None, None, Some(0)),
            ]),
        )
        .await
        .map_err(|error| format!("写入初次部分图片特征失败: {error}"))?;

    let partial_md5 = data.partial_image.md5();
    let partial_size = i64::try_from(data.partial_image.file_size())
        .map_err(|error| format!("部分图片大小转换失败: {error}"))?;
    let partial_row = query_one(
        url,
        "SELECT i.width,i.height,i.quality
         FROM image_stage1 i JOIN contents c ON c.content_id=i.content_id
         WHERE c.md5=$1 AND c.file_size=$2",
        &[&partial_md5.as_slice(), &partial_size],
    )
    .await?;
    require_eq(
        "初次部分图片宽度",
        column::<Option<i32>>(&partial_row, 0, "图片宽度")?,
        None::<i32>,
    )?;
    require_eq(
        "初次部分图片高度",
        column::<Option<i32>>(&partial_row, 1, "图片高度")?,
        None::<i32>,
    )?;
    require_eq(
        "初次部分图片合法零 Quality",
        column(&partial_row, 2, "图片 Quality")?,
        Some(0_i16),
    )?;

    store
        .apply_sync_batch(
            &data.machine,
            &batch(vec![
                image_stage1_change(
                    301,
                    data.initial_partial,
                    Some(800),
                    Some(600),
                    Some(vec![0x77; 32]),
                    Some(70),
                ),
                image_stage2_change(303, data.initial_partial, vec![0x99; 72], vec![0xaa; 512]),
                image_stage1_change(
                    302,
                    data.partial_image,
                    Some(1_024),
                    Some(768),
                    Some(vec![0x88; 32]),
                    Some(60),
                ),
            ]),
        )
        .await
        .map_err(|error| format!("补齐中心特征失败: {error}"))?;
    drop(store);

    let image_md5 = data.image.md5();
    let image_size = i64::try_from(data.image.file_size())
        .map_err(|error| format!("图片大小转换失败: {error}"))?;
    let image_row = query_one(
        url,
        "SELECT i.width,i.height,i.pdq,i.quality,c.base_complete
         FROM image_stage1 i JOIN contents c ON c.content_id=i.content_id
         WHERE c.md5=$1 AND c.file_size=$2",
        &[&image_md5.as_slice(), &image_size],
    )
    .await?;
    require_eq(
        "既有图片宽度",
        column(&image_row, 0, "图片宽度")?,
        Some(640_i32),
    )?;
    require_eq(
        "既有图片高度",
        column(&image_row, 1, "图片高度")?,
        Some(480_i32),
    )?;
    require_eq(
        "既有图片 PDQ",
        column(&image_row, 2, "图片 PDQ")?,
        Some(vec![0x11; 32]),
    )?;
    require_eq(
        "既有图片 Quality",
        column(&image_row, 3, "图片 Quality")?,
        Some(90_i16),
    )?;
    require_eq("既有内容基础完成", column(&image_row, 4, "基础完成")?, true)?;

    let image_stage2_row = query_one(
        url,
        "SELECT i.phash_parts,i.sobel
         FROM image_stage2 i JOIN contents c ON c.content_id=i.content_id
         WHERE c.md5=$1 AND c.file_size=$2",
        &[&image_md5.as_slice(), &image_size],
    )
    .await?;
    require_eq(
        "既有图片 pHash",
        column(&image_stage2_row, 0, "图片 pHash")?,
        Some(vec![0x22; 72]),
    )?;
    require_eq(
        "既有图片 Sobel",
        column(&image_stage2_row, 1, "图片 Sobel")?,
        Some(vec![0x33; 512]),
    )?;

    let video_md5 = data.video.md5();
    let video_size = i64::try_from(data.video.file_size())
        .map_err(|error| format!("视频大小转换失败: {error}"))?;
    let metadata_row = query_one(
        url,
        "SELECT v.duration_ms,v.width,v.height
         FROM video_metadata v JOIN contents c ON c.content_id=v.content_id
         WHERE c.md5=$1 AND c.file_size=$2",
        &[&video_md5.as_slice(), &video_size],
    )
    .await?;
    require_eq(
        "既有视频时长",
        column(&metadata_row, 0, "视频时长")?,
        Some(9_000_i64),
    )?;
    require_eq(
        "既有视频宽度",
        column(&metadata_row, 1, "视频宽度")?,
        Some(1_920_i32),
    )?;
    require_eq(
        "既有视频高度",
        column(&metadata_row, 2, "视频高度")?,
        Some(1_080_i32),
    )?;

    let frame_row = query_one(
        url,
        "SELECT f.time_ms,f.decoded,f.width,f.height,f.pdq,f.quality
         FROM video_frame_stage1 f JOIN contents c ON c.content_id=f.content_id
         WHERE c.md5=$1 AND c.file_size=$2 AND f.slot=0",
        &[&video_md5.as_slice(), &video_size],
    )
    .await?;
    require_eq(
        "既有视频帧时间",
        column(&frame_row, 0, "帧时间")?,
        4_000_i64,
    )?;
    require_eq(
        "既有视频帧 decoded",
        column(&frame_row, 1, "帧 decoded")?,
        true,
    )?;
    require_eq(
        "既有视频帧宽度",
        column(&frame_row, 2, "帧宽度")?,
        Some(1_920_i32),
    )?;
    require_eq(
        "既有视频帧高度",
        column(&frame_row, 3, "帧高度")?,
        Some(1_080_i32),
    )?;
    require_eq(
        "既有视频帧 PDQ",
        column(&frame_row, 4, "帧 PDQ")?,
        Some(vec![0x44; 32]),
    )?;
    require_eq(
        "既有视频帧 Quality",
        column(&frame_row, 5, "帧 Quality")?,
        Some(80_i16),
    )?;

    let frame_stage2_row = query_one(
        url,
        "SELECT f.phash_parts,f.sobel
         FROM video_frame_stage2 f JOIN contents c ON c.content_id=f.content_id
         WHERE c.md5=$1 AND c.file_size=$2 AND f.slot=0",
        &[&video_md5.as_slice(), &video_size],
    )
    .await?;
    require_eq(
        "既有视频帧 pHash",
        column(&frame_stage2_row, 0, "帧 pHash")?,
        Some(vec![0x55; 72]),
    )?;
    require_eq(
        "既有视频帧 Sobel",
        column(&frame_stage2_row, 1, "帧 Sobel")?,
        Some(vec![0x66; 512]),
    )?;

    let initial_md5 = data.initial_partial.md5();
    let initial_size = i64::try_from(data.initial_partial.file_size())
        .map_err(|error| format!("初次部分内容大小转换失败: {error}"))?;
    let filled_row = query_one(
        url,
        "SELECT i.width,i.height,i.pdq,i.quality
         FROM image_stage1 i JOIN contents c ON c.content_id=i.content_id
         WHERE c.md5=$1 AND c.file_size=$2",
        &[&initial_md5.as_slice(), &initial_size],
    )
    .await?;
    require_eq(
        "补齐图片宽度",
        column(&filled_row, 0, "图片宽度")?,
        Some(800_i32),
    )?;
    require_eq(
        "补齐图片高度",
        column(&filled_row, 1, "图片高度")?,
        Some(600_i32),
    )?;
    require_eq(
        "补齐图片 PDQ",
        column(&filled_row, 2, "图片 PDQ")?,
        Some(vec![0x77; 32]),
    )?;
    require_eq(
        "补齐图片 Quality",
        column(&filled_row, 3, "图片 Quality")?,
        Some(70_i16),
    )?;

    let filled_stage2_row = query_one(
        url,
        "SELECT i.phash_parts,i.sobel
         FROM image_stage2 i JOIN contents c ON c.content_id=i.content_id
         WHERE c.md5=$1 AND c.file_size=$2",
        &[&initial_md5.as_slice(), &initial_size],
    )
    .await?;
    require_eq(
        "补齐图片 pHash",
        column(&filled_stage2_row, 0, "图片 pHash")?,
        Some(vec![0x99; 72]),
    )?;
    require_eq(
        "补齐图片 Sobel",
        column(&filled_stage2_row, 1, "图片 Sobel")?,
        Some(vec![0xaa; 512]),
    )?;

    let partial_image_row = query_one(
        url,
        "SELECT i.width,i.height,i.pdq,i.quality
         FROM image_stage1 i JOIN contents c ON c.content_id=i.content_id
         WHERE c.md5=$1 AND c.file_size=$2",
        &[&partial_md5.as_slice(), &partial_size],
    )
    .await?;
    require_eq(
        "补齐零 Quality 图片宽度",
        column(&partial_image_row, 0, "图片宽度")?,
        Some(1_024_i32),
    )?;
    require_eq(
        "补齐零 Quality 图片高度",
        column(&partial_image_row, 1, "图片高度")?,
        Some(768_i32),
    )?;
    require_eq(
        "补齐零 Quality 图片 PDQ",
        column(&partial_image_row, 2, "图片 PDQ")?,
        Some(vec![0x88; 32]),
    )?;
    require_eq(
        "补齐零 Quality 图片 Quality",
        column(&partial_image_row, 3, "图片 Quality")?,
        Some(60_i16),
    )?;

    let location_machine = data.machine.as_str();
    let location_row = query_one(
        url,
        "SELECT file_size,active FROM file_locations
         WHERE machine_id=$1 AND normalized_path=$2",
        &[&location_machine, &data.normalized_path],
    )
    .await?;
    require_eq(
        "唯一位置文件大小",
        column(&location_row, 0, "位置文件大小")?,
        Some(101_i64),
    )?;
    require_eq(
        "唯一位置活动状态",
        column(&location_row, 1, "位置活动状态")?,
        true,
    )?;
    Ok(())
}

/// 测试数据使用 UUID 派生的机器、内容和位置键，避免跨次运行互相污染。
#[derive(Clone)]
struct TestData {
    machine: MachineId,
    image: ContentKey,
    video: ContentKey,
    initial_partial: ContentKey,
    partial_image: ContentKey,
    normalized_path: String,
}

impl TestData {
    /// 生成一次用例的全套唯一跨边界键。
    fn new() -> Self {
        let uuid = Uuid::new_v4();
        let mut machine_bytes = [0_u8; 32];
        machine_bytes[..16].copy_from_slice(uuid.as_bytes());
        machine_bytes[16..].copy_from_slice(uuid.as_bytes());
        Self {
            machine: MachineId::from_sha256(machine_bytes),
            image: unique_content_key(&uuid, 1, 101),
            video: unique_content_key(&uuid, 2, 202),
            initial_partial: unique_content_key(&uuid, 3, 303),
            partial_image: unique_content_key(&uuid, 4, 404),
            normalized_path: format!(r"D:\task4-upsert-{}.jpg", uuid.simple()),
        }
    }

    /// 返回本次用例所有可能写入内容表的键。
    fn content_keys(&self) -> [ContentKey; 4] {
        [
            self.image,
            self.video,
            self.initial_partial,
            self.partial_image,
        ]
    }
}

/// 用 UUID 和标记派生不重复的内容键。
fn unique_content_key(uuid: &Uuid, marker: u8, file_size: u64) -> ContentKey {
    let mut md5 = *uuid.as_bytes();
    md5[0] = md5[0].wrapping_add(marker);
    md5[15] = md5[15].wrapping_add(marker.wrapping_mul(17));
    ContentKey::new(md5, file_size)
}

/// 用显式事务清理本次用例的所有中心关联行。
struct CleanupGuard {
    url: String,
    data: TestData,
}

impl CleanupGuard {
    /// 保存连接字符串和唯一测试键，供异步清理使用。
    fn new(url: &str, data: TestData) -> Self {
        Self {
            url: url.to_owned(),
            data,
        }
    }

    /// 在独立连接中原子删除位置、特征、内容、cursor 和节点。
    async fn cleanup(&self) -> Result<(), String> {
        let (mut client, connection) = tokio_postgres::connect(&self.url, tokio_postgres::NoTls)
            .await
            .map_err(|error| format!("打开清理连接失败: {error}"))?;
        let connection_task = tokio::spawn(async move {
            let _ = connection.await;
        });
        let result = self.cleanup_with_client(&mut client).await;
        drop(client);
        connection_task.abort();
        result
    }

    /// 按外键依赖顺序删除单次测试数据。
    async fn cleanup_with_client(&self, client: &mut Client) -> Result<(), String> {
        let transaction = client
            .transaction()
            .await
            .map_err(|error| format!("开启清理事务失败: {error}"))?;
        transaction
            .execute(
                "DELETE FROM deletion_tombstones WHERE machine_id=$1 AND normalized_path=$2",
                &[&self.data.machine.as_str(), &self.data.normalized_path],
            )
            .await
            .map_err(|error| format!("清理位置墓碑失败: {error}"))?;
        transaction
            .execute(
                "DELETE FROM file_locations WHERE machine_id=$1 AND normalized_path=$2",
                &[&self.data.machine.as_str(), &self.data.normalized_path],
            )
            .await
            .map_err(|error| format!("清理文件位置失败: {error}"))?;

        let feature_deletes = [
            "DELETE FROM video_frame_stage2 WHERE content_id=(SELECT content_id FROM contents WHERE md5=$1 AND file_size=$2)",
            "DELETE FROM video_frame_stage1 WHERE content_id=(SELECT content_id FROM contents WHERE md5=$1 AND file_size=$2)",
            "DELETE FROM image_stage2 WHERE content_id=(SELECT content_id FROM contents WHERE md5=$1 AND file_size=$2)",
            "DELETE FROM image_stage1 WHERE content_id=(SELECT content_id FROM contents WHERE md5=$1 AND file_size=$2)",
            "DELETE FROM video_metadata WHERE content_id=(SELECT content_id FROM contents WHERE md5=$1 AND file_size=$2)",
        ];
        for key in self.data.content_keys() {
            let md5 = key.md5();
            let file_size = i64::try_from(key.file_size())
                .map_err(|error| format!("清理内容大小转换失败: {error}"))?;
            for statement in feature_deletes {
                transaction
                    .execute(statement, &[&md5.as_slice(), &file_size])
                    .await
                    .map_err(|error| format!("清理内容特征失败: {error}"))?;
            }
            transaction
                .execute(
                    "DELETE FROM contents WHERE md5=$1 AND file_size=$2",
                    &[&md5.as_slice(), &file_size],
                )
                .await
                .map_err(|error| format!("清理内容失败: {error}"))?;
        }
        transaction
            .execute(
                "DELETE FROM sync_cursors WHERE machine_id=$1",
                &[&self.data.machine.as_str()],
            )
            .await
            .map_err(|error| format!("清理同步游标失败: {error}"))?;
        transaction
            .execute(
                "DELETE FROM nodes WHERE machine_id=$1",
                &[&self.data.machine.as_str()],
            )
            .await
            .map_err(|error| format!("清理节点失败: {error}"))?;
        transaction
            .commit()
            .await
            .map_err(|error| format!("提交清理事务失败: {error}"))?;
        Ok(())
    }
}

/// 从中心数据库安全读取一行，并在查询失败时先释放连接。
async fn query_one(
    url: &str,
    statement: &str,
    params: &[&(dyn ToSql + Sync)],
) -> Result<Row, String> {
    let (client, connection) = tokio_postgres::connect(url, tokio_postgres::NoTls)
        .await
        .map_err(|error| format!("打开查询连接失败: {error}"))?;
    let connection_task = tokio::spawn(async move {
        let _ = connection.await;
    });
    let result = client
        .query_one(statement, params)
        .await
        .map_err(|error| format!("查询中心数据失败: {error}"));
    drop(client);
    connection_task.abort();
    result
}

/// 读取带类型的 PostgreSQL 字段，避免 Row::get 在断言失败时 panic。
fn column<T: FromSqlOwned>(row: &Row, index: usize, label: &str) -> Result<T, String> {
    row.try_get(index)
        .map_err(|error| format!("读取{label}失败: {error}"))
}

/// 返回可清理的结构化断言错误。
fn require_eq<T>(label: &str, actual: T, expected: T) -> Result<(), String>
where
    T: Debug + PartialEq,
{
    if actual == expected {
        Ok(())
    } else {
        Err(format!("{label}不匹配：期望 {expected:?}，实际 {actual:?}"))
    }
}

/// 组装中心同步批次，保持输入序号由调用方明确指定。
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

/// 生成文件位置变更载荷，验证本次运行的 LocationKey 也被隔离。
fn file_change(
    seq: u64,
    machine: &MachineId,
    normalized_path: &str,
    key: ContentKey,
) -> proto::SyncChange {
    let payload = Payload::new(1)
        .text(machine.as_str())
        .text(normalized_path)
        .text(normalized_path)
        .u64(key.file_size())
        .bytes(&key.md5())
        .u64(key.file_size())
        .u8(1)
        .finish();
    change(seq, "file", payload)
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

    /// 写入 UTF-8 文本。
    fn text(self, value: &str) -> Self {
        self.bytes(value.as_bytes())
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

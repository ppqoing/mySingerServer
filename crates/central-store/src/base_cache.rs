//! Node 基础计算前使用的 PostgreSQL 批量缓存查询。

use std::collections::BTreeMap;

use dedup_core::{ContentKey, MachineId, MediaKind};
use dedup_media::{ImageStage1, PdqHash};
use tokio_postgres::Row;

use crate::{CentralError, CentralStore};

/// 中心库中一份可安全导入 Node SQLite 的基础计算缓存。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralBaseCacheEntry {
    /// MD5 与文件大小组成的跨库内容键。
    pub content_key: ContentKey,
    /// FFmpeg 实际探测后的媒体类型。
    pub media_kind: MediaKind,
    /// Node 已完成基础探测与该媒体类型必需的一筛。
    pub base_complete: bool,
    /// 已缓存的像素宽度。
    pub width: Option<u32>,
    /// 已缓存的像素高度。
    pub height: Option<u32>,
    /// 视频时长，单位毫秒。
    pub duration_ms: Option<u64>,
    /// 字段完整的一筛；部分结果不冒充完整命中。
    pub stage1: Option<CentralBaseStage1>,
}

/// PostgreSQL 中可完整复用的一筛特征。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum CentralBaseStage1 {
    /// 图片完整一筛。
    Image(ImageStage1),
    /// 视频六个固定槽位；失败槽位为 `None`。
    Video(Box<[Option<ImageStage1>; 6]>),
}

impl CentralStore {
    /// 按输入顺序批量查询指定机器的“规范路径 + 大小”缓存。
    pub async fn lookup_base_paths(
        &self,
        machine_id: &MachineId,
        paths: &[(String, u64)],
    ) -> Result<Vec<Option<CentralBaseCacheEntry>>, CentralError> {
        if paths.is_empty() {
            return Ok(Vec::new());
        }
        let normalized = paths
            .iter()
            .map(|(path, _)| path.clone())
            .collect::<Vec<_>>();
        let sizes = paths
            .iter()
            .map(|(_, size)| pg_i64(*size, "文件大小"))
            .collect::<Result<Vec<_>, _>>()?;
        let rows = self
            .client
            .query(
                "WITH requested AS (
                   SELECT normalized_path,file_size,ordinality
                   FROM unnest($2::text[],$3::bigint[])
                        WITH ORDINALITY AS r(normalized_path,file_size,ordinality)
                 )
                 SELECT r.ordinality,c.content_id,c.md5,c.file_size,c.media_kind,c.base_complete
                 FROM requested r
                 LEFT JOIN file_locations f ON f.machine_id=$1
                   AND f.normalized_path=r.normalized_path
                   AND f.file_size=r.file_size AND f.active
                 LEFT JOIN contents c ON c.content_id=f.content_id
                 ORDER BY r.ordinality",
                &[&machine_id.as_str(), &normalized, &sizes],
            )
            .await?;
        self.load_base_rows(paths.len(), rows).await
    }

    /// 按输入顺序批量查询 MD5 与大小完全相同的内容缓存。
    pub async fn lookup_base_contents(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<CentralBaseCacheEntry>>, CentralError> {
        if keys.is_empty() {
            return Ok(Vec::new());
        }
        let md5 = keys
            .iter()
            .map(|key| key.md5().to_vec())
            .collect::<Vec<_>>();
        let sizes = keys
            .iter()
            .map(|key| pg_i64(key.file_size(), "文件大小"))
            .collect::<Result<Vec<_>, _>>()?;
        let rows = self
            .client
            .query(
                "WITH requested AS (
                   SELECT md5,file_size,ordinality
                   FROM unnest($1::bytea[],$2::bigint[])
                        WITH ORDINALITY AS r(md5,file_size,ordinality)
                 )
                 SELECT r.ordinality,c.content_id,c.md5,c.file_size,c.media_kind,c.base_complete
                 FROM requested r
                 LEFT JOIN contents c ON c.md5=r.md5 AND c.file_size=r.file_size
                 ORDER BY r.ordinality",
                &[&md5, &sizes],
            )
            .await?;
        self.load_base_rows(keys.len(), rows).await
    }

    /// 一次性装载批次中命中内容的媒体元数据和一筛，避免逐文件往返。
    async fn load_base_rows(
        &self,
        input_len: usize,
        rows: Vec<Row>,
    ) -> Result<Vec<Option<CentralBaseCacheEntry>>, CentralError> {
        let mut output = vec![None; input_len];
        let mut content_ids = Vec::new();
        let mut positions = BTreeMap::new();
        for row in rows {
            let ordinal = usize::try_from(row.get::<_, i64>(0))
                .map_err(|_| invalid_state("缓存查询序号无效"))?;
            let Some(content_id) = row.get::<_, Option<i64>>(1) else {
                continue;
            };
            let position = ordinal
                .checked_sub(1)
                .filter(|position| *position < input_len)
                .ok_or_else(|| invalid_state("缓存查询序号越界"))?;
            let entry = CentralBaseCacheEntry {
                content_key: ContentKey::new(
                    fixed::<16>(row.get(2), "内容 MD5")?,
                    non_negative(row.get(3), "文件大小")?,
                ),
                media_kind: parse_media_kind(row.get(4))?,
                base_complete: row.get(5),
                width: None,
                height: None,
                duration_ms: None,
                stage1: None,
            };
            output[position] = Some(entry);
            content_ids.push(content_id);
            positions.insert(content_id, position);
        }
        if content_ids.is_empty() {
            return Ok(output);
        }
        load_image_stage1(&self.client, &content_ids, &positions, &mut output).await?;
        load_video_base(&self.client, &content_ids, &positions, &mut output).await?;
        Ok(output)
    }
}

/// 批量装载完整图片一筛，同时保留部分尺寸用于缺失判断。
async fn load_image_stage1(
    client: &tokio_postgres::Client,
    content_ids: &[i64],
    positions: &BTreeMap<i64, usize>,
    output: &mut [Option<CentralBaseCacheEntry>],
) -> Result<(), CentralError> {
    for row in client
        .query(
            "SELECT content_id,width,height,pdq,quality FROM image_stage1
             WHERE content_id=ANY($1)",
            &[&content_ids],
        )
        .await?
    {
        let Some(position) = positions.get(&row.get::<_, i64>(0)).copied() else {
            continue;
        };
        let Some(entry) = output[position].as_mut() else {
            continue;
        };
        entry.width = optional_positive(row.get(1), "图片宽度")?;
        entry.height = optional_positive(row.get(2), "图片高度")?;
        let fields = (
            entry.width,
            entry.height,
            row.get::<_, Option<Vec<u8>>>(3),
            row.get::<_, Option<i16>>(4),
        );
        if let (Some(width), Some(height), Some(pdq), Some(quality)) = fields {
            entry.stage1 = Some(CentralBaseStage1::Image(ImageStage1 {
                width,
                height,
                pdq: PdqHash::from_bytes(fixed::<32>(pdq, "图片 PDQ")?),
                quality: u8::try_from(quality).map_err(|_| invalid_state("图片 Quality 越界"))?,
            }));
        }
    }
    Ok(())
}

/// 批量装载视频元数据和严格完整的六槽一筛。
async fn load_video_base(
    client: &tokio_postgres::Client,
    content_ids: &[i64],
    positions: &BTreeMap<i64, usize>,
    output: &mut [Option<CentralBaseCacheEntry>],
) -> Result<(), CentralError> {
    for row in client
        .query(
            "SELECT content_id,duration_ms,width,height FROM video_metadata
             WHERE content_id=ANY($1)",
            &[&content_ids],
        )
        .await?
    {
        let Some(position) = positions.get(&row.get::<_, i64>(0)).copied() else {
            continue;
        };
        let Some(entry) = output[position].as_mut() else {
            continue;
        };
        entry.duration_ms = optional_non_negative(row.get(1), "视频时长")?;
        entry.width = optional_positive(row.get(2), "视频宽度")?;
        entry.height = optional_positive(row.get(3), "视频高度")?;
    }
    let rows = client
        .query(
            "SELECT content_id,slot,decoded,width,height,pdq,quality
             FROM video_frame_stage1 WHERE content_id=ANY($1)
             ORDER BY content_id,slot",
            &[&content_ids],
        )
        .await?;
    let mut grouped = BTreeMap::<i64, Vec<Row>>::new();
    for row in rows {
        grouped.entry(row.get(0)).or_default().push(row);
    }
    for (content_id, rows) in grouped {
        if rows.len() != 6 {
            continue;
        }
        let mut frames = [None; 6];
        let mut complete = true;
        for (expected, row) in rows.into_iter().enumerate() {
            let slot = usize::try_from(row.get::<_, i16>(1)).unwrap_or(usize::MAX);
            if slot != expected {
                complete = false;
                break;
            }
            if !row.get::<_, bool>(2) {
                continue;
            }
            let fields = (
                row.get::<_, Option<i32>>(3),
                row.get::<_, Option<i32>>(4),
                row.get::<_, Option<Vec<u8>>>(5),
                row.get::<_, Option<i16>>(6),
            );
            let (Some(width), Some(height), Some(pdq), Some(quality)) = fields else {
                complete = false;
                break;
            };
            frames[slot] = Some(ImageStage1 {
                width: positive(width, "视频帧宽度")?,
                height: positive(height, "视频帧高度")?,
                pdq: PdqHash::from_bytes(fixed::<32>(pdq, "视频帧 PDQ")?),
                quality: u8::try_from(quality).map_err(|_| invalid_state("视频帧 Quality 越界"))?,
            });
        }
        if complete
            && frames.iter().flatten().count() >= 4
            && let Some(position) = positions.get(&content_id).copied()
            && let Some(entry) = output[position].as_mut()
        {
            entry.stage1 = Some(CentralBaseStage1::Video(Box::new(frames)));
        }
    }
    Ok(())
}

/// 把 u64 转为 PostgreSQL BIGINT，拒绝静默溢出。
fn pg_i64(value: u64, name: &str) -> Result<i64, CentralError> {
    i64::try_from(value).map_err(|_| invalid_state(format!("{name} 超出 BIGINT 范围")))
}

/// 把非负 BIGINT 转为 u64。
fn non_negative(value: i64, name: &str) -> Result<u64, CentralError> {
    u64::try_from(value).map_err(|_| invalid_state(format!("{name} 不能为负数")))
}

/// 把可空非负 BIGINT 转为 u64。
fn optional_non_negative(value: Option<i64>, name: &str) -> Result<Option<u64>, CentralError> {
    value.map(|value| non_negative(value, name)).transpose()
}

/// 把正整数尺寸转为 u32。
fn positive(value: i32, name: &str) -> Result<u32, CentralError> {
    u32::try_from(value)
        .ok()
        .filter(|value| *value > 0)
        .ok_or_else(|| invalid_state(format!("{name} 必须为正数")))
}

/// 把可空正整数尺寸转为 u32。
fn optional_positive(value: Option<i32>, name: &str) -> Result<Option<u32>, CentralError> {
    value.map(|value| positive(value, name)).transpose()
}

/// 解码固定长度二进制字段。
fn fixed<const N: usize>(value: Vec<u8>, name: &str) -> Result<[u8; N], CentralError> {
    value
        .try_into()
        .map_err(|_| invalid_state(format!("{name} 长度错误")))
}

/// 解码中心 schema 中的媒体类型名称。
fn parse_media_kind(value: &str) -> Result<MediaKind, CentralError> {
    match value {
        "image" => Ok(MediaKind::Image),
        "video" => Ok(MediaKind::Video),
        "other" => Ok(MediaKind::Other),
        _ => Err(invalid_state(format!("未知媒体类型: {value}"))),
    }
}

/// 创建中心库数据不一致错误。
fn invalid_state(message: impl Into<String>) -> CentralError {
    CentralError::InvalidState(message.into())
}

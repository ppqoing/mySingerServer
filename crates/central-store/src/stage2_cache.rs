//! Node 二次特征任务使用的 PostgreSQL 批量缓存查询。

use std::collections::BTreeMap;

use dedup_core::{ContentKey, MediaKind};
use dedup_media::ImageStage2;

use crate::{CentralError, CentralStore};

/// 中心库中一份字段完整、可导入 Node SQLite 的二次特征。
#[derive(Clone, Debug, PartialEq)]
pub enum CentralStage2CacheEntry {
    /// 图片的九分块 pHash 与 Sobel。
    Image(Box<ImageStage2>),
    /// 覆盖请求成功槽位的视频二次特征。
    Video(Box<[Option<ImageStage2>; 6]>),
}

impl CentralStore {
    /// 按输入顺序批量查询二次特征；视频仅在全部请求槽位命中时返回。
    pub async fn lookup_stage2_contents(
        &self,
        requests: &[(ContentKey, MediaKind, Vec<u8>)],
    ) -> Result<Vec<Option<CentralStage2CacheEntry>>, CentralError> {
        if requests.is_empty() {
            return Ok(Vec::new());
        }
        let md5 = requests
            .iter()
            .map(|(content, _, _)| content.md5().to_vec())
            .collect::<Vec<_>>();
        let sizes = requests
            .iter()
            .map(|(content, _, _)| {
                i64::try_from(content.file_size())
                    .map_err(|_| CentralError::InvalidState("文件大小超过 BIGINT".into()))
            })
            .collect::<Result<Vec<_>, _>>()?;
        let rows = self
            .client
            .query(
                "WITH requested AS (
                   SELECT md5,file_size,ordinality
                   FROM unnest($1::bytea[],$2::bigint[])
                        WITH ORDINALITY AS r(md5,file_size,ordinality)
                 )
                 SELECT r.ordinality,c.content_id
                 FROM requested r
                 LEFT JOIN contents c ON c.md5=r.md5 AND c.file_size=r.file_size
                 ORDER BY r.ordinality",
                &[&md5, &sizes],
            )
            .await?;
        let mut content_positions = BTreeMap::new();
        for row in rows {
            let Some(content_id) = row.get::<_, Option<i64>>(1) else {
                continue;
            };
            let ordinal = usize::try_from(row.get::<_, i64>(0)).map_err(|error| {
                CentralError::InvalidState(format!("二筛缓存序号无法转换: {error}"))
            })?;
            let position = ordinal
                .checked_sub(1)
                .filter(|value| *value < requests.len())
                .ok_or_else(|| CentralError::InvalidState("二筛缓存序号无效".into()))?;
            content_positions.insert(content_id, position);
        }
        let content_ids = content_positions.keys().copied().collect::<Vec<_>>();
        let mut output = vec![None; requests.len()];
        if content_ids.is_empty() {
            return Ok(output);
        }
        for row in self
            .client
            .query(
                "SELECT content_id,phash_parts,sobel FROM image_stage2
                 WHERE content_id=ANY($1) AND phash_parts IS NOT NULL AND sobel IS NOT NULL",
                &[&content_ids],
            )
            .await?
        {
            let content_id = row.get::<_, i64>(0);
            let Some(position) = content_positions.get(&content_id).copied() else {
                continue;
            };
            if requests[position].1 == MediaKind::Image {
                output[position] = Some(CentralStage2CacheEntry::Image(Box::new(decode_stage2(
                    row.get(1),
                    row.get(2),
                )?)));
            }
        }
        let rows = self
            .client
            .query(
                "SELECT content_id,slot,phash_parts,sobel FROM video_frame_stage2
                 WHERE content_id=ANY($1) AND phash_parts IS NOT NULL AND sobel IS NOT NULL
                 ORDER BY content_id,slot",
                &[&content_ids],
            )
            .await?;
        let mut frames = BTreeMap::<i64, [Option<ImageStage2>; 6]>::new();
        for row in rows {
            let content_id = row.get::<_, i64>(0);
            let slot = usize::try_from(row.get::<_, i16>(1)).unwrap_or(usize::MAX);
            if slot < 6 {
                frames.entry(content_id).or_insert([None; 6])[slot] =
                    Some(decode_stage2(row.get(2), row.get(3))?);
            }
        }
        for (content_id, position) in content_positions {
            let (_, media_kind, required_slots) = &requests[position];
            if *media_kind != MediaKind::Video {
                continue;
            }
            let available = frames.remove(&content_id).unwrap_or([None; 6]);
            if required_slots.iter().all(|slot| {
                available
                    .get(usize::from(*slot))
                    .is_some_and(Option::is_some)
            }) {
                output[position] = Some(CentralStage2CacheEntry::Video(Box::new(available)));
            }
        }
        Ok(output)
    }
}

/// 解码中心 schema 中固定长度的联合二筛数组。
fn decode_stage2(phash: Vec<u8>, sobel: Vec<u8>) -> Result<ImageStage2, CentralError> {
    let phash: [u8; 72] = phash
        .try_into()
        .map_err(|_| CentralError::InvalidState("二筛 pHash 长度错误".into()))?;
    let sobel: [u8; 512] = sobel
        .try_into()
        .map_err(|_| CentralError::InvalidState("二筛 Sobel 长度错误".into()))?;
    let mut phash_parts = [0_u64; 9];
    for (index, bytes) in phash.chunks_exact(8).enumerate() {
        phash_parts[index] = u64::from_le_bytes(bytes.try_into().expect("固定八字节"));
    }
    let mut histogram = [0.0_f32; 128];
    for (index, bytes) in sobel.chunks_exact(4).enumerate() {
        histogram[index] = f32::from_le_bytes(bytes.try_into().expect("固定四字节"));
    }
    if histogram.iter().any(|value| !value.is_finite()) {
        return Err(CentralError::InvalidState("中心 Sobel 包含非有限数".into()));
    }
    Ok(ImageStage2 {
        phash_parts,
        sobel: histogram,
    })
}

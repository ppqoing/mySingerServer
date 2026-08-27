//! 批量路径缓存查询，以及 MD5 索引后按文件大小确认的内容/位置事务。

use std::time::Duration;

use dedup_core::{ContentKey, LocationKey, MediaKind};
use dedup_media::sample_positions;
use rusqlite::{OptionalExtension, Transaction, params};

use crate::{
    ActiveFile, BaseCacheRecord, CacheLookup, CompleteStage1, ContentId, ContentRecord,
    FeatureWrite, ImageStage1Fields, NodeStore, ScannedPath, StoreError, VideoFrameStage1Fields,
    VideoMetadataFields,
    open::{fixed_bytes, sqlite_integer},
    outbox::append_sync_change,
    rows::RowEncoder,
};

impl NodeStore {
    /// 按输入顺序批量查询“当前机器 + 规范路径 + 文件大小”的可复用内容。
    pub fn lookup_scanned_paths(
        &self,
        scanned_paths: &[ScannedPath],
    ) -> Result<Vec<CacheLookup>, StoreError> {
        let mut statement = self.connection.prepare_cached(
            "SELECT f.content_id,c.md5,c.file_size
             FROM files f JOIN contents c ON c.content_id=f.content_id
             WHERE f.machine_id=?1 AND f.normalized_path=?2
               AND f.file_size=?3 AND f.active=1",
        )?;
        scanned_paths
            .iter()
            .map(|scanned| {
                let hit: Option<(i64, Vec<u8>, i64)> = statement
                    .query_row(
                        params![
                            self.machine_id().as_str(),
                            scanned.normalized_path.as_str(),
                            sqlite_integer(scanned.file_size)?
                        ],
                        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
                    )
                    .optional()?;
                let (content_id, content_key) = if let Some((id, md5, file_size)) = hit {
                    (
                        Some(ContentId::from_i64(id)),
                        Some(ContentKey::new(
                            fixed_bytes(md5, "contents.md5")?,
                            file_size as u64,
                        )),
                    )
                } else {
                    (None, None)
                };
                Ok(CacheLookup {
                    scanned: scanned.clone(),
                    content_id,
                    content_key,
                })
            })
            .collect()
    }

    /// 先用 MD5 索引读取候选并比较大小，再原子写入内容、位置和同步 outbox。
    pub fn upsert_content_and_location(
        &mut self,
        scanned: &ScannedPath,
        md5: [u8; 16],
        media_kind: MediaKind,
    ) -> Result<ContentRecord, StoreError> {
        let machine_id = self.machine_id().clone();
        let file_size = sqlite_integer(scanned.file_size)?;
        let transaction = self.connection.transaction()?;
        let matching_id = {
            let mut statement = transaction
                .prepare_cached("SELECT content_id,file_size FROM contents WHERE md5=?1")?;
            let candidates = statement.query_map([md5.as_slice()], |row| {
                Ok((row.get::<_, i64>(0)?, row.get::<_, i64>(1)?))
            })?;
            let mut matching_id = None;
            for candidate in candidates {
                let (content_id, candidate_size) = candidate?;
                if candidate_size == file_size {
                    matching_id = Some(content_id);
                    break;
                }
            }
            matching_id
        };

        let (content_id, reused) = if let Some(content_id) = matching_id {
            (content_id, true)
        } else {
            transaction.execute(
                "INSERT INTO contents(md5,file_size,media_kind) VALUES(?1,?2,?3)",
                params![md5.as_slice(), file_size, media_kind_name(media_kind)],
            )?;
            let content_id = transaction.last_insert_rowid();
            append_sync_change(
                &transaction,
                "content",
                encode_content(ContentKey::new(md5, scanned.file_size), media_kind, false),
            )?;
            (content_id, false)
        };

        transaction.execute(
            "INSERT INTO files(machine_id,normalized_path,display_path,file_size,content_id,active)
             VALUES(?1,?2,?3,?4,?5,1)
             ON CONFLICT(machine_id,normalized_path) DO UPDATE SET
               display_path=excluded.display_path,
               file_size=excluded.file_size,
               content_id=excluded.content_id,
               active=1",
            params![
                machine_id.as_str(),
                scanned.normalized_path.as_str(),
                scanned.display_path.as_path().to_string_lossy().as_ref(),
                file_size,
                content_id
            ],
        )?;
        append_sync_change(
            &transaction,
            "file",
            encode_file(
                machine_id.as_str(),
                scanned,
                ContentKey::new(md5, scanned.file_size),
                true,
            ),
        )?;
        transaction.commit()?;

        Ok(ContentRecord {
            id: ContentId::from_i64(content_id),
            key: ContentKey::new(md5, scanned.file_size),
            reused,
        })
    }

    /// 返回内容当前由实际媒体探测确认的类别。
    pub fn content_media_kind(&self, content_id: ContentId) -> Result<MediaKind, StoreError> {
        let value: String = self.connection.query_row(
            "SELECT media_kind FROM contents WHERE content_id=?1",
            [content_id.as_i64()],
            |row| row.get(0),
        )?;
        match value.as_str() {
            "image" => Ok(MediaKind::Image),
            "video" => Ok(MediaKind::Video),
            "other" => Ok(MediaKind::Other),
            _ => Err(StoreError::InvalidState(format!(
                "未知内容媒体类型: {value}"
            ))),
        }
    }

    /// 用 FFmpeg 实际探测结果替换新内容的暂存类型。
    pub fn set_content_media_kind(
        &mut self,
        content_id: ContentId,
        media_kind: MediaKind,
    ) -> Result<(), StoreError> {
        let transaction = self.connection.transaction()?;
        let changed = transaction.execute(
            "UPDATE contents SET media_kind=?2 WHERE content_id=?1",
            params![content_id.as_i64(), media_kind_name(media_kind)],
        )?;
        if changed != 1 {
            return Err(StoreError::InvalidState("要更新的内容不存在".into()));
        }
        let key = content_key_in_transaction(&transaction, content_id)?;
        let base_complete: bool = transaction.query_row(
            "SELECT base_complete FROM contents WHERE content_id=?1",
            [content_id.as_i64()],
            |row| row.get(0),
        )?;
        append_sync_change(
            &transaction,
            "content",
            encode_content(key, media_kind, base_complete),
        )?;
        transaction.commit()?;
        Ok(())
    }

    /// 在基础探测和必需一筛均已写入后设置完成标记并同步中心库。
    pub fn mark_base_complete(&mut self, content_id: ContentId) -> Result<(), StoreError> {
        let transaction = self.connection.transaction()?;
        let changed = transaction.execute(
            "UPDATE contents SET base_complete=1 WHERE content_id=?1",
            [content_id.as_i64()],
        )?;
        if changed != 1 {
            return Err(StoreError::InvalidState("要完成的内容不存在".into()));
        }
        let key = content_key_in_transaction(&transaction, content_id)?;
        let media_kind: String = transaction.query_row(
            "SELECT media_kind FROM contents WHERE content_id=?1",
            [content_id.as_i64()],
            |row| row.get(0),
        )?;
        append_sync_change(
            &transaction,
            "content",
            encode_content(key, parse_media_kind(&media_kind)?, true),
        )?;
        transaction.commit()?;
        Ok(())
    }

    /// 查询本机规范路径当前是否仍为活动位置。
    pub fn is_location_active(
        &self,
        normalized_path: &dedup_core::NormalizedPath,
    ) -> Result<bool, StoreError> {
        Ok(self
            .connection
            .query_row(
                "SELECT active FROM files WHERE machine_id=?1 AND normalized_path=?2",
                params![self.machine_id().as_str(), normalized_path.as_str()],
                |row| row.get::<_, bool>(0),
            )
            .optional()?
            .unwrap_or(false))
    }

    /// 用跨边界 ContentKey 查找本机 SQLite 内容 ID。
    pub fn content_id_by_key(&self, key: ContentKey) -> Result<Option<ContentId>, StoreError> {
        Ok(self
            .connection
            .query_row(
                "SELECT content_id FROM contents WHERE md5=?1 AND file_size=?2",
                params![key.md5().as_slice(), sqlite_integer(key.file_size())?],
                |row| row.get::<_, i64>(0).map(ContentId::from_i64),
            )
            .optional()?)
    }

    /// 返回拥有该内容的第一个活动位置和实际显示路径，顺序固定为机器、规范路径。
    pub fn active_location_for_content(
        &self,
        content_id: ContentId,
    ) -> Result<Option<(dedup_core::LocationKey, dedup_core::DisplayPath)>, StoreError> {
        let row = self
            .connection
            .query_row(
                "SELECT machine_id,normalized_path,display_path FROM files
                 WHERE content_id=?1 AND active=1 ORDER BY machine_id,normalized_path LIMIT 1",
                [content_id.as_i64()],
                |row| {
                    Ok((
                        row.get::<_, String>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, String>(2)?,
                    ))
                },
            )
            .optional()?;
        row.map(|(machine, normalized, display)| {
            Ok((
                dedup_core::LocationKey::new(
                    dedup_core::MachineId::parse(&machine)?,
                    dedup_core::NormalizedPath::new(normalized)?,
                ),
                dedup_core::DisplayPath::new(display)?,
            ))
        })
        .transpose()
    }

    /// 按完整位置键返回当前活动文件；预览和删除不会使用历史位置。
    pub fn active_file(&self, location: &LocationKey) -> Result<Option<ActiveFile>, StoreError> {
        let row = self
            .connection
            .query_row(
                "SELECT f.content_id,c.md5,c.file_size,f.display_path,c.media_kind
                 FROM files f JOIN contents c ON c.content_id=f.content_id
                 WHERE f.machine_id=?1 AND f.normalized_path=?2 AND f.active=1",
                params![
                    location.machine_id().as_str(),
                    location.normalized_path().as_str()
                ],
                |row| {
                    Ok((
                        row.get::<_, i64>(0)?,
                        row.get::<_, Vec<u8>>(1)?,
                        row.get::<_, i64>(2)?,
                        row.get::<_, String>(3)?,
                        row.get::<_, String>(4)?,
                    ))
                },
            )
            .optional()?;
        row.map(|(content_id, md5, file_size, display_path, media_kind)| {
            Ok(ActiveFile {
                content_id: ContentId::from_i64(content_id),
                content_key: ContentKey::new(fixed_bytes(md5, "contents.md5")?, file_size as u64),
                display_path: dedup_core::DisplayPath::new(display_path)?,
                media_kind: parse_media_kind(&media_kind)?,
            })
        })
        .transpose()
    }

    /// 返回视频内容已生成的 JPEG 联系表相对缓存路径。
    pub fn contact_sheet_path(&self, content_id: ContentId) -> Result<Option<String>, StoreError> {
        Ok(self
            .connection
            .query_row(
                "SELECT relative_path FROM contact_sheets WHERE content_id=?1",
                [content_id.as_i64()],
                |row| row.get(0),
            )
            .optional()?)
    }

    /// 加载内容键、探测字段和严格完整的一筛，供 Node 计算缺失掩码。
    pub fn load_base_cache_record(
        &self,
        content_id: ContentId,
    ) -> Result<BaseCacheRecord, StoreError> {
        let (md5, file_size, media_kind, base_complete): (Vec<u8>, i64, String, bool) =
            self.connection.query_row(
                "SELECT md5,file_size,media_kind,base_complete FROM contents WHERE content_id=?1",
                [content_id.as_i64()],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
            )?;
        let media_kind = parse_media_kind(&media_kind)?;
        let (width, height, duration_ms) = match media_kind {
            MediaKind::Image => self
                .connection
                .query_row(
                    "SELECT width,height FROM image_stage1 WHERE content_id=?1",
                    [content_id.as_i64()],
                    |row| Ok((row.get(0)?, row.get(1)?)),
                )
                .optional()?
                .map_or((None, None, None), |(width, height)| (width, height, None)),
            MediaKind::Video => self
                .connection
                .query_row(
                    "SELECT width,height,duration_ms FROM video_metadata WHERE content_id=?1",
                    [content_id.as_i64()],
                    |row| Ok((row.get(0)?, row.get(1)?, row.get::<_, Option<i64>>(2)?)),
                )
                .optional()?
                .map_or((None, None, None), |(width, height, duration)| {
                    (width, height, duration.map(|value| value as u64))
                }),
            MediaKind::Other => (None, None, None),
        };
        Ok(BaseCacheRecord {
            content_id: Some(content_id),
            content_key: ContentKey::new(
                fixed_bytes(md5, "contents.md5")?,
                u64::try_from(file_size)
                    .map_err(|_| StoreError::InvalidState("内容文件大小不能为负数".into()))?,
            ),
            media_kind,
            base_complete,
            width,
            height,
            duration_ms,
            stage1: self.load_complete_stage1(content_id)?,
        })
    }

    /// 把 PostgreSQL 缓存导入本地内容、位置和一筛；不修改任何任务项终态。
    pub fn import_base_cache_record(
        &mut self,
        scanned: &ScannedPath,
        cached: &BaseCacheRecord,
    ) -> Result<ContentRecord, StoreError> {
        if cached.content_key.file_size() != scanned.file_size {
            return Err(StoreError::InvalidState(
                "中心缓存文件大小与枚举结果不一致".into(),
            ));
        }
        let content =
            self.upsert_content_and_location(scanned, cached.content_key.md5(), cached.media_kind)?;
        self.set_content_media_kind(content.id, cached.media_kind)?;
        match &cached.stage1 {
            Some(CompleteStage1::Image(feature)) => {
                self.commit_feature_result(
                    content.id,
                    None,
                    FeatureWrite::ImageStage1(ImageStage1Fields::from(*feature)),
                )?;
            }
            Some(CompleteStage1::Video(frames)) => {
                self.commit_feature_result(
                    content.id,
                    None,
                    FeatureWrite::VideoMetadata(VideoMetadataFields {
                        duration_ms: cached.duration_ms,
                        width: cached.width,
                        height: cached.height,
                    }),
                )?;
                let positions = sample_positions(Duration::from_millis(
                    cached.duration_ms.unwrap_or_default(),
                ));
                for (slot, feature) in frames.iter().enumerate() {
                    self.commit_feature_result(
                        content.id,
                        None,
                        FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                            slot: slot as u8,
                            time_ms: positions[slot].as_millis() as u64,
                            decoded: feature.is_some(),
                            width: feature.map(|value| value.width),
                            height: feature.map(|value| value.height),
                            pdq: feature.map(|value| value.pdq),
                            quality: feature.map(|value| value.quality),
                        }),
                    )?;
                }
            }
            None if cached.media_kind == MediaKind::Video => {
                self.commit_feature_result(
                    content.id,
                    None,
                    FeatureWrite::VideoMetadata(VideoMetadataFields {
                        duration_ms: cached.duration_ms,
                        width: cached.width,
                        height: cached.height,
                    }),
                )?;
            }
            None => {}
        }
        if cached.base_complete {
            self.mark_base_complete(content.id)?;
        }
        Ok(content)
    }
}

fn parse_media_kind(value: &str) -> Result<MediaKind, StoreError> {
    match value {
        "image" => Ok(MediaKind::Image),
        "video" => Ok(MediaKind::Video),
        "other" => Ok(MediaKind::Other),
        _ => Err(StoreError::InvalidState(format!(
            "未知内容媒体类型: {value}"
        ))),
    }
}

pub(crate) fn media_kind_name(kind: MediaKind) -> &'static str {
    match kind {
        MediaKind::Image => "image",
        MediaKind::Video => "video",
        MediaKind::Other => "other",
    }
}

pub(crate) fn content_key_in_transaction(
    transaction: &Transaction<'_>,
    content_id: ContentId,
) -> Result<ContentKey, StoreError> {
    let (md5, file_size): (Vec<u8>, i64) = transaction.query_row(
        "SELECT md5,file_size FROM contents WHERE content_id=?1",
        [content_id.as_i64()],
        |row| Ok((row.get(0)?, row.get(1)?)),
    )?;
    Ok(ContentKey::new(
        fixed_bytes(md5, "contents.md5")?,
        file_size as u64,
    ))
}

pub(crate) fn encode_content(key: ContentKey, kind: MediaKind, base_complete: bool) -> Vec<u8> {
    RowEncoder::new(2)
        .bytes(&key.md5())
        .u64(key.file_size())
        .u8(match kind {
            MediaKind::Image => 1,
            MediaKind::Video => 2,
            MediaKind::Other => 3,
        })
        .u8(u8::from(base_complete))
        .finish()
}

pub(crate) fn encode_file(
    machine_id: &str,
    scanned: &ScannedPath,
    key: ContentKey,
    active: bool,
) -> Vec<u8> {
    RowEncoder::new(1)
        .text(machine_id)
        .text(scanned.normalized_path.as_str())
        .text(scanned.display_path.as_path().to_string_lossy().as_ref())
        .u64(scanned.file_size)
        .bytes(&key.md5())
        .u64(key.file_size())
        .u8(u8::from(active))
        .finish()
}

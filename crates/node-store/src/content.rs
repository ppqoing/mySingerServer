//! 批量路径缓存查询，以及 MD5 索引后按文件大小确认的内容/位置事务。

use std::{fmt::Write as _, time::Duration};

use dedup_core::{ContentKey, LocationKey, MediaKind};
use dedup_media::sample_positions;
use rusqlite::types::Value;
use rusqlite::{OptionalExtension, Transaction, limits::Limit, params, params_from_iter};

use crate::{
    ActiveFile, BaseCacheRecord, CacheLookup, CompleteStage1, ContentId, ContentRecord,
    FeatureWrite, ImageStage1Fields, NodeStore, ScannedPath, StoreError, VideoFrameStage1Fields,
    VideoMetadataFields,
    features::{decode_stage1_fields, decode_stage2_if_valid},
    open::{fixed_bytes, sqlite_integer},
    outbox::append_sync_change,
    rows::RowEncoder,
};

impl NodeStore {
    /// 基础缓存一次逻辑批次最多提交的请求数，超过 SQLite 变量上限时再细分。
    const BASE_CACHE_BATCH_LIMIT: usize = 1_000;

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

    /// 按输入顺序批量加载路径对应的基础缓存记录；缺失项保留为 `None`。
    pub fn lookup_base_cache_by_paths(
        &self,
        scanned_paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, StoreError> {
        if scanned_paths.is_empty() {
            return Ok(Vec::new());
        }
        let batch_limit = self.base_cache_batch_limit(true)?;
        let mut records = Vec::with_capacity(scanned_paths.len());
        for batch in scanned_paths.chunks(batch_limit) {
            records.extend(self.lookup_path_cache_batch(batch)?);
        }
        Ok(records)
    }

    /// 按输入顺序批量加载内容键对应的基础缓存记录；缺失项保留为 `None`。
    pub fn lookup_base_cache_by_keys(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, StoreError> {
        if keys.is_empty() {
            return Ok(Vec::new());
        }
        let batch_limit = self.base_cache_batch_limit(false)?;
        let mut records = Vec::with_capacity(keys.len());
        for batch in keys.chunks(batch_limit) {
            records.extend(self.lookup_key_cache_batch(batch)?);
        }
        Ok(records)
    }

    /// 根据连接的变量上限计算每个子批容量，变量不足时明确拒绝而不退回逐项查询。
    fn base_cache_batch_limit(&self, has_machine_parameter: bool) -> Result<usize, StoreError> {
        let variable_limit =
            usize::try_from(self.connection.limit(Limit::SQLITE_LIMIT_VARIABLE_NUMBER)?)
                .map_err(|_| StoreError::InvalidState("SQLite 变量参数上限无效".into()))?;
        let reserved = usize::from(has_machine_parameter);
        let available = variable_limit.saturating_sub(reserved);
        let batch_limit = (available / 3).min(Self::BASE_CACHE_BATCH_LIMIT);
        if batch_limit == 0 {
            return Err(StoreError::InvalidState(
                "SQLite 变量参数上限不足以查询一个基础缓存请求".into(),
            ));
        }
        Ok(batch_limit)
    }

    /// 执行一个路径子批的固定三条查询，并按请求序号组装基础缓存记录。
    fn lookup_path_cache_batch(
        &self,
        scanned_paths: &[ScannedPath],
    ) -> Result<Vec<Option<BaseCacheRecord>>, StoreError> {
        let values = values_rows(scanned_paths.len(), 2);
        let first_sql = format!(
            "WITH request_rows(ordinal, normalized_path, file_size) AS (VALUES {values})
             SELECT r.ordinal,c.content_id,c.md5,c.file_size,c.media_kind,c.base_complete,
                    i.width,i.height,i.pdq,i.quality,
                    vm.width,vm.height,vm.duration_ms,
                    i2.phash_parts,i2.sobel,cs.relative_path
             FROM request_rows r
             LEFT JOIN files f ON f.machine_id=?1 AND f.normalized_path=r.normalized_path
                              AND f.file_size=r.file_size AND f.active=1
             LEFT JOIN contents c ON c.content_id=f.content_id
             LEFT JOIN image_stage1 i ON i.content_id=c.content_id
             LEFT JOIN video_metadata vm ON vm.content_id=c.content_id
             LEFT JOIN image_stage2 i2 ON i2.content_id=c.content_id
             LEFT JOIN contact_sheets cs ON cs.content_id=c.content_id
             ORDER BY r.ordinal"
        );
        let mut first_params = Vec::with_capacity(1 + scanned_paths.len() * 3);
        first_params.push(Value::Text(self.machine_id().as_str().to_owned()));
        for (ordinal, scanned) in scanned_paths.iter().enumerate() {
            first_params.push(Value::Integer(i64::try_from(ordinal).map_err(|_| {
                StoreError::InvalidState("基础缓存请求序号超出 SQLite 范围".into())
            })?));
            first_params.push(Value::Text(scanned.normalized_path.as_str().to_owned()));
            first_params.push(Value::Integer(sqlite_integer(scanned.file_size)?));
        }
        let mut statement = self.connection.prepare(&first_sql)?;
        let raw_rows = statement
            .query_map(params_from_iter(first_params), read_base_cache_row)?
            .collect::<Result<Vec<_>, _>>()?;
        let mut records = decode_base_cache_rows(raw_rows, scanned_paths.len())?;

        let frame_values = values_rows(scanned_paths.len(), 2);
        let frame_sql = format!(
            "WITH request_rows(ordinal, normalized_path, file_size) AS (VALUES {frame_values})
             SELECT r.ordinal,vf.slot,vf.decoded,vf.width,vf.height,vf.pdq,vf.quality
             FROM request_rows r
             JOIN files f ON f.machine_id=?1 AND f.normalized_path=r.normalized_path
                          AND f.file_size=r.file_size AND f.active=1
             JOIN contents c ON c.content_id=f.content_id AND c.media_kind='video'
             JOIN video_frame_stage1 vf ON vf.content_id=c.content_id
             ORDER BY r.ordinal,vf.slot"
        );
        let mut frame_params = Vec::with_capacity(1 + scanned_paths.len() * 3);
        frame_params.push(Value::Text(self.machine_id().as_str().to_owned()));
        for (ordinal, scanned) in scanned_paths.iter().enumerate() {
            frame_params.push(Value::Integer(i64::try_from(ordinal).map_err(|_| {
                StoreError::InvalidState("基础缓存请求序号超出 SQLite 范围".into())
            })?));
            frame_params.push(Value::Text(scanned.normalized_path.as_str().to_owned()));
            frame_params.push(Value::Integer(sqlite_integer(scanned.file_size)?));
        }
        let mut statement = self.connection.prepare(&frame_sql)?;
        let frame_rows = statement
            .query_map(params_from_iter(frame_params), read_video_frame_row)?
            .collect::<Result<Vec<_>, _>>()?;
        apply_video_frame_rows(&mut records, frame_rows)?;

        let stage2_values = values_rows(scanned_paths.len(), 2);
        let stage2_sql = format!(
            "WITH request_rows(ordinal, normalized_path, file_size) AS (VALUES {stage2_values})
             SELECT r.ordinal,vf.slot,vf.phash_parts,vf.sobel
             FROM request_rows r
             JOIN files f ON f.machine_id=?1 AND f.normalized_path=r.normalized_path
                          AND f.file_size=r.file_size AND f.active=1
             JOIN contents c ON c.content_id=f.content_id AND c.media_kind='video'
             JOIN video_frame_stage2 vf ON vf.content_id=c.content_id
             ORDER BY r.ordinal,vf.slot"
        );
        let mut stage2_params = Vec::with_capacity(1 + scanned_paths.len() * 3);
        stage2_params.push(Value::Text(self.machine_id().as_str().to_owned()));
        for (ordinal, scanned) in scanned_paths.iter().enumerate() {
            stage2_params.push(Value::Integer(i64::try_from(ordinal).map_err(|_| {
                StoreError::InvalidState("基础缓存请求序号超出 SQLite 范围".into())
            })?));
            stage2_params.push(Value::Text(scanned.normalized_path.as_str().to_owned()));
            stage2_params.push(Value::Integer(sqlite_integer(scanned.file_size)?));
        }
        let mut statement = self.connection.prepare(&stage2_sql)?;
        let stage2_rows = statement
            .query_map(params_from_iter(stage2_params), read_video_stage2_row)?
            .collect::<Result<Vec<_>, _>>()?;
        apply_video_stage2_rows(&mut records, stage2_rows)?;
        Ok(records)
    }

    /// 执行一个内容键子批的固定三条查询，并按请求序号组装基础缓存记录。
    fn lookup_key_cache_batch(
        &self,
        keys: &[ContentKey],
    ) -> Result<Vec<Option<BaseCacheRecord>>, StoreError> {
        let values = values_rows(keys.len(), 1);
        let first_sql = format!(
            "WITH request_rows(ordinal, md5, file_size) AS (VALUES {values})
             SELECT r.ordinal,c.content_id,c.md5,c.file_size,c.media_kind,c.base_complete,
                    i.width,i.height,i.pdq,i.quality,
                    vm.width,vm.height,vm.duration_ms,
                    i2.phash_parts,i2.sobel,cs.relative_path
             FROM request_rows r
             LEFT JOIN contents c ON c.md5=r.md5 AND c.file_size=r.file_size
             LEFT JOIN image_stage1 i ON i.content_id=c.content_id
             LEFT JOIN video_metadata vm ON vm.content_id=c.content_id
             LEFT JOIN image_stage2 i2 ON i2.content_id=c.content_id
             LEFT JOIN contact_sheets cs ON cs.content_id=c.content_id
             ORDER BY r.ordinal"
        );
        let mut first_params = Vec::with_capacity(keys.len() * 3);
        for (ordinal, key) in keys.iter().enumerate() {
            first_params.push(Value::Integer(i64::try_from(ordinal).map_err(|_| {
                StoreError::InvalidState("基础缓存请求序号超出 SQLite 范围".into())
            })?));
            first_params.push(Value::Blob(key.md5().to_vec()));
            first_params.push(Value::Integer(sqlite_integer(key.file_size())?));
        }
        let mut statement = self.connection.prepare(&first_sql)?;
        let raw_rows = statement
            .query_map(params_from_iter(first_params), read_base_cache_row)?
            .collect::<Result<Vec<_>, _>>()?;
        let mut records = decode_base_cache_rows(raw_rows, keys.len())?;

        let frame_values = values_rows(keys.len(), 1);
        let frame_sql = format!(
            "WITH request_rows(ordinal, md5, file_size) AS (VALUES {frame_values})
             SELECT r.ordinal,vf.slot,vf.decoded,vf.width,vf.height,vf.pdq,vf.quality
             FROM request_rows r
             JOIN contents c ON c.md5=r.md5 AND c.file_size=r.file_size
                            AND c.media_kind='video'
             JOIN video_frame_stage1 vf ON vf.content_id=c.content_id
             ORDER BY r.ordinal,vf.slot"
        );
        let mut frame_params = Vec::with_capacity(keys.len() * 3);
        for (ordinal, key) in keys.iter().enumerate() {
            frame_params.push(Value::Integer(i64::try_from(ordinal).map_err(|_| {
                StoreError::InvalidState("基础缓存请求序号超出 SQLite 范围".into())
            })?));
            frame_params.push(Value::Blob(key.md5().to_vec()));
            frame_params.push(Value::Integer(sqlite_integer(key.file_size())?));
        }
        let mut statement = self.connection.prepare(&frame_sql)?;
        let frame_rows = statement
            .query_map(params_from_iter(frame_params), read_video_frame_row)?
            .collect::<Result<Vec<_>, _>>()?;
        apply_video_frame_rows(&mut records, frame_rows)?;

        let stage2_values = values_rows(keys.len(), 1);
        let stage2_sql = format!(
            "WITH request_rows(ordinal, md5, file_size) AS (VALUES {stage2_values})
             SELECT r.ordinal,vf.slot,vf.phash_parts,vf.sobel
             FROM request_rows r
             JOIN contents c ON c.md5=r.md5 AND c.file_size=r.file_size
                            AND c.media_kind='video'
             JOIN video_frame_stage2 vf ON vf.content_id=c.content_id
             ORDER BY r.ordinal,vf.slot"
        );
        let mut stage2_params = Vec::with_capacity(keys.len() * 3);
        for (ordinal, key) in keys.iter().enumerate() {
            stage2_params.push(Value::Integer(i64::try_from(ordinal).map_err(|_| {
                StoreError::InvalidState("基础缓存请求序号超出 SQLite 范围".into())
            })?));
            stage2_params.push(Value::Blob(key.md5().to_vec()));
            stage2_params.push(Value::Integer(sqlite_integer(key.file_size())?));
        }
        let mut statement = self.connection.prepare(&stage2_sql)?;
        let stage2_rows = statement
            .query_map(params_from_iter(stage2_params), read_video_stage2_row)?
            .collect::<Result<Vec<_>, _>>()?;
        apply_video_stage2_rows(&mut records, stage2_rows)?;
        Ok(records)
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
                    (
                        width,
                        height,
                        duration.and_then(|value| u64::try_from(value).ok()),
                    )
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
            image_stage2: self.load_image_stage2_for_cache(content_id)?,
            video_stage2: Box::new(self.load_video_stage2_for_cache(content_id)?),
            contact_sheet_relative_path: self.contact_sheet_path(content_id)?,
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

/// 基础缓存首条查询的原始行；特征完整性要等第二条视频查询结束后统一判断。
struct RawBaseCacheRow {
    ordinal: i64,
    content_id: Option<i64>,
    md5: Option<Vec<u8>>,
    file_size: Option<i64>,
    media_kind: Option<String>,
    base_complete: Option<i64>,
    image_width: Option<i64>,
    image_height: Option<i64>,
    image_pdq: Option<Vec<u8>>,
    image_quality: Option<i64>,
    video_width: Option<i64>,
    video_height: Option<i64>,
    video_duration_ms: Option<i64>,
    image_stage2_phash: Option<Vec<u8>>,
    image_stage2_sobel: Option<Vec<u8>>,
    contact_sheet_relative_path: Option<String>,
}

/// 视频第二条查询返回的单个槽位，按请求序号暂存后还原固定六槽位数组。
struct RawVideoFrameRow {
    ordinal: i64,
    slot: Option<i64>,
    decoded: Option<i64>,
    width: Option<i64>,
    height: Option<i64>,
    pdq: Option<Vec<u8>>,
    quality: Option<i64>,
}

/// 视频二筛批量查询返回的单个槽位原始行。
struct RawVideoStage2Row {
    ordinal: i64,
    slot: Option<i64>,
    phash: Option<Vec<u8>>,
    sobel: Option<Vec<u8>>,
}

/// 生成带连续绑定参数的 VALUES 行，避免把输入数据拼接进 SQL。
fn values_rows(count: usize, first_parameter: usize) -> String {
    let mut values = String::new();
    for ordinal in 0..count {
        if ordinal > 0 {
            values.push_str(", ");
        }
        let parameter = first_parameter + ordinal * 3;
        write!(
            values,
            "(?{parameter},?{},?{})",
            parameter + 1,
            parameter + 2
        )
        .expect("写入内存中的 VALUES 占位符不会失败");
    }
    values
}

/// 把 SQLite 首条查询的列解码为可继续补充视频特征的基础记录。
fn read_base_cache_row(row: &rusqlite::Row<'_>) -> rusqlite::Result<RawBaseCacheRow> {
    Ok(RawBaseCacheRow {
        ordinal: row.get(0)?,
        content_id: row.get(1)?,
        md5: row.get(2)?,
        file_size: row.get(3)?,
        media_kind: row.get(4)?,
        base_complete: row.get(5)?,
        image_width: row.get(6)?,
        image_height: row.get(7)?,
        image_pdq: row.get(8)?,
        image_quality: row.get(9)?,
        video_width: row.get(10)?,
        video_height: row.get(11)?,
        video_duration_ms: row.get(12)?,
        image_stage2_phash: row.get(13)?,
        image_stage2_sobel: row.get(14)?,
        contact_sheet_relative_path: row.get(15)?,
    })
}

/// 把 SQLite 第二条查询的列解码为视频槽位暂存行。
fn read_video_frame_row(row: &rusqlite::Row<'_>) -> rusqlite::Result<RawVideoFrameRow> {
    Ok(RawVideoFrameRow {
        ordinal: row.get(0)?,
        slot: row.get(1)?,
        decoded: row.get(2)?,
        width: row.get(3)?,
        height: row.get(4)?,
        pdq: row.get(5)?,
        quality: row.get(6)?,
    })
}

/// 解码视频二筛原始行，保留非法字段供统一缺失分类器处理。
fn read_video_stage2_row(row: &rusqlite::Row<'_>) -> rusqlite::Result<RawVideoStage2Row> {
    Ok(RawVideoStage2Row {
        ordinal: row.get(0)?,
        slot: row.get(1)?,
        phash: row.get(2)?,
        sobel: row.get(3)?,
    })
}

/// 检查批量结果的请求序号并保持输入严格等长同序。
fn decode_base_cache_rows(
    rows: Vec<RawBaseCacheRow>,
    expected_len: usize,
) -> Result<Vec<Option<BaseCacheRecord>>, StoreError> {
    if rows.len() != expected_len {
        return Err(StoreError::InvalidState(format!(
            "基础缓存批量查询返回长度不匹配: 期望 {expected_len}，实际 {}",
            rows.len()
        )));
    }
    rows.into_iter()
        .enumerate()
        .map(|(expected_ordinal, row)| {
            let ordinal = usize::try_from(row.ordinal)
                .map_err(|_| StoreError::InvalidState("基础缓存查询返回了无效请求序号".into()))?;
            if ordinal != expected_ordinal {
                return Err(StoreError::InvalidState(format!(
                    "基础缓存查询返回顺序错误: 期望 {expected_ordinal}，实际 {ordinal}"
                )));
            }
            decode_base_cache_row(row)
        })
        .collect()
}

/// 解码一条内容行；左连接未命中时只返回缺失，不伪造缓存记录。
fn decode_base_cache_row(row: RawBaseCacheRow) -> Result<Option<BaseCacheRecord>, StoreError> {
    let Some(content_id) = row.content_id else {
        return Ok(None);
    };
    let (Some(md5), Some(file_size), Some(media_kind), Some(base_complete)) =
        (row.md5, row.file_size, row.media_kind, row.base_complete)
    else {
        return Ok(None);
    };
    let Ok(md5) = md5.try_into() else {
        return Ok(None);
    };
    let Ok(file_size) = u64::try_from(file_size) else {
        return Ok(None);
    };
    let Ok(media_kind) = parse_media_kind(&media_kind) else {
        return Ok(None);
    };
    let content_key = ContentKey::new(md5, file_size);
    let base_complete = base_complete == 1;
    let (width, height, duration_ms, stage1) = match media_kind {
        MediaKind::Image => {
            let width = row.image_width.and_then(|value| u32::try_from(value).ok());
            let height = row.image_height.and_then(|value| u32::try_from(value).ok());
            let stage1 = decode_stage1_fields(
                row.image_width,
                row.image_height,
                row.image_pdq,
                row.image_quality,
            )
            .map(CompleteStage1::Image);
            (width, height, None, stage1)
        }
        MediaKind::Video => {
            let width = row.video_width.and_then(|value| u32::try_from(value).ok());
            let height = row.video_height.and_then(|value| u32::try_from(value).ok());
            let duration_ms = row
                .video_duration_ms
                .and_then(|value| u64::try_from(value).ok());
            (width, height, duration_ms, None)
        }
        MediaKind::Other => (None, None, None, None),
    };
    Ok(Some(BaseCacheRecord {
        content_id: Some(ContentId::from_i64(content_id)),
        content_key,
        media_kind,
        base_complete,
        width,
        height,
        duration_ms,
        stage1,
        image_stage2: decode_stage2_if_valid(
            row.image_stage2_phash.as_deref(),
            row.image_stage2_sobel.as_deref(),
        ),
        video_stage2: Box::new([None; 6]),
        contact_sheet_relative_path: row.contact_sheet_relative_path,
    }))
}

/// 把视频槽位按请求序号合并，并沿用单项加载的六槽位/四成功帧完整性规则。
fn apply_video_frame_rows(
    records: &mut [Option<BaseCacheRecord>],
    rows: Vec<RawVideoFrameRow>,
) -> Result<(), StoreError> {
    let mut grouped: Vec<Vec<RawVideoFrameRow>> = (0..records.len()).map(|_| Vec::new()).collect();
    for row in rows {
        let ordinal = usize::try_from(row.ordinal)
            .map_err(|_| StoreError::InvalidState("视频特征查询返回了无效请求序号".into()))?;
        let Some(group) = grouped.get_mut(ordinal) else {
            return Err(StoreError::InvalidState(
                "视频特征查询返回了越界请求序号".into(),
            ));
        };
        group.push(row);
    }

    for (ordinal, record) in records.iter_mut().enumerate() {
        let Some(record) = record else {
            continue;
        };
        if record.media_kind != MediaKind::Video {
            continue;
        }
        let frame_rows = std::mem::take(&mut grouped[ordinal]);
        if frame_rows.len() != 6
            || frame_rows.iter().any(|row| {
                row.slot
                    .and_then(|slot| usize::try_from(slot).ok())
                    .is_none_or(|slot| slot >= 6)
            })
        {
            continue;
        }
        let mut frames = [None; 6];
        let mut seen = [false; 6];
        let mut complete = true;
        for row in frame_rows {
            let Some(slot) = row.slot.and_then(|slot| usize::try_from(slot).ok()) else {
                complete = false;
                break;
            };
            if seen[slot] {
                complete = false;
                break;
            }
            seen[slot] = true;
            match row.decoded {
                Some(0) => {}
                Some(1) => {
                    let Some(feature) =
                        decode_stage1_fields(row.width, row.height, row.pdq, row.quality)
                    else {
                        complete = false;
                        break;
                    };
                    frames[slot] = Some(feature);
                }
                _ => {
                    complete = false;
                    break;
                }
            }
        }
        if complete && seen.iter().all(|seen| *seen) && frames.iter().flatten().count() >= 4 {
            record.stage1 = Some(CompleteStage1::Video(Box::new(frames)));
        }
    }
    Ok(())
}

/// 把视频二筛批量原始行按请求序号写回六槽位数组，非法项只保留为缺失。
fn apply_video_stage2_rows(
    records: &mut [Option<BaseCacheRecord>],
    rows: Vec<RawVideoStage2Row>,
) -> Result<(), StoreError> {
    for row in rows {
        let Ok(ordinal) = usize::try_from(row.ordinal) else {
            continue;
        };
        let Some(record) = records.get_mut(ordinal).and_then(Option::as_mut) else {
            continue;
        };
        if record.media_kind != MediaKind::Video {
            continue;
        }
        let Some(slot) = row.slot.and_then(|slot| usize::try_from(slot).ok()) else {
            continue;
        };
        if slot >= record.video_stage2.len() {
            continue;
        }
        record.video_stage2[slot] =
            decode_stage2_if_valid(row.phash.as_deref(), row.sobel.as_deref());
    }
    Ok(())
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

#[cfg(test)]
mod tests {
    use std::sync::{LazyLock, Mutex};

    use dedup_core::MediaKind;
    use rusqlite::{
        limits::Limit,
        trace::{TraceEvent, TraceEventCodes},
    };

    use super::*;

    static TRACED_STATEMENTS: LazyLock<Mutex<Vec<String>>> =
        LazyLock::new(|| Mutex::new(Vec::new()));
    static TRACE_TEST_LOCK: LazyLock<Mutex<()>> = LazyLock::new(|| Mutex::new(()));

    /// 收集当前连接执行的 SQL，验证基础缓存批次不会退化为逐项查询。
    fn trace_statement(event: TraceEvent<'_>) {
        if let TraceEvent::Stmt(_, sql) = event {
            TRACED_STATEMENTS.lock().unwrap().push(sql.to_owned());
        }
    }

    /// 路径缓存查询对一千个输入只执行固定三条业务 SELECT，不随项数逐项增长。
    #[test]
    fn base_cache_batch_uses_three_business_selects() {
        let _trace_guard = TRACE_TEST_LOCK.lock().unwrap();
        let mut store = NodeStore::open_in_memory(
            dedup_core::MachineId::parse(
                "73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae",
            )
            .unwrap(),
        )
        .unwrap();
        let inputs: Vec<ScannedPath> = (0..1_000)
            .map(|index| {
                let mut md5 = [0; 16];
                md5[..2].copy_from_slice(&(index as u16).to_be_bytes());
                let scanned = ScannedPath::new(
                    dedup_core::NormalizedPath::new(&format!(r"D:\batch-{index}.bin")).unwrap(),
                    dedup_core::DisplayPath::new(&format!(r"D:\batch-{index}.bin")).unwrap(),
                    index as u64 + 1,
                );
                store
                    .upsert_content_and_location(&scanned, md5, MediaKind::Other)
                    .unwrap();
                scanned
            })
            .collect();

        TRACED_STATEMENTS.lock().unwrap().clear();
        store
            .connection
            .trace_v2(TraceEventCodes::SQLITE_TRACE_STMT, Some(trace_statement));
        let _ = store.lookup_base_cache_by_paths(&inputs).unwrap();
        store.connection.trace_v2(TraceEventCodes::empty(), None);

        let traced = TRACED_STATEMENTS.lock().unwrap();
        let lookup_count = traced
            .iter()
            .filter(|sql| sql.contains("WITH request_rows"))
            .count();
        assert_eq!(
            lookup_count, 3,
            "批量查询实际执行了 {lookup_count} 条业务 SELECT"
        );
        assert!(
            traced.iter().all(|sql| !sql.starts_with("INSERT")
                && !sql.starts_with("UPDATE")
                && !sql.starts_with("DELETE")),
            "批量查询不应写任务或其他业务表"
        );
    }

    /// 内容键批量查询同样必须对一千个输入只执行固定三条业务 SELECT。
    #[test]
    fn base_cache_key_batch_uses_three_business_selects() {
        let _trace_guard = TRACE_TEST_LOCK.lock().unwrap();
        let mut store = NodeStore::open_in_memory(
            dedup_core::MachineId::parse(
                "73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae",
            )
            .unwrap(),
        )
        .unwrap();
        let keys: Vec<ContentKey> = (0..1_000)
            .map(|index| {
                let mut md5 = [0; 16];
                md5[..2].copy_from_slice(&(index as u16).to_be_bytes());
                let scanned = ScannedPath::new(
                    dedup_core::NormalizedPath::new(&format!(r"D:\key-batch-{index}.bin")).unwrap(),
                    dedup_core::DisplayPath::new(&format!(r"D:\key-batch-{index}.bin")).unwrap(),
                    index as u64 + 1,
                );
                let content = store
                    .upsert_content_and_location(&scanned, md5, MediaKind::Other)
                    .unwrap();
                content.key
            })
            .collect();

        TRACED_STATEMENTS.lock().unwrap().clear();
        store
            .connection
            .trace_v2(TraceEventCodes::SQLITE_TRACE_STMT, Some(trace_statement));
        let _ = store.lookup_base_cache_by_keys(&keys).unwrap();
        store.connection.trace_v2(TraceEventCodes::empty(), None);

        let traced = TRACED_STATEMENTS.lock().unwrap();
        let lookup_count = traced
            .iter()
            .filter(|sql| sql.contains("WITH request_rows"))
            .count();
        assert_eq!(
            lookup_count, 3,
            "内容键批量查询实际执行了 {lookup_count} 条业务 SELECT"
        );
        assert!(
            traced.iter().all(|sql| !sql.starts_with("INSERT")
                && !sql.starts_with("UPDATE")
                && !sql.starts_with("DELETE")),
            "批量查询不应写任务或其他业务表"
        );
    }

    /// 子批容量必须随运行时 SQLite 变量上限切块，并拒绝连一个路径请求都容不下的上限。
    #[test]
    fn base_cache_batch_uses_runtime_variable_limit() {
        let _trace_guard = TRACE_TEST_LOCK.lock().unwrap();
        let store = NodeStore::open_in_memory(
            dedup_core::MachineId::parse(
                "73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae",
            )
            .unwrap(),
        )
        .unwrap();
        let inputs: Vec<_> = (0..5)
            .map(|index| {
                ScannedPath::new(
                    dedup_core::NormalizedPath::new(&format!(r"D:\limited-{index}.bin")).unwrap(),
                    dedup_core::DisplayPath::new(&format!(r"D:\limited-{index}.bin")).unwrap(),
                    index as u64 + 1,
                )
            })
            .collect();
        store
            .connection
            .set_limit(Limit::SQLITE_LIMIT_VARIABLE_NUMBER, 7)
            .unwrap();

        TRACED_STATEMENTS.lock().unwrap().clear();
        store
            .connection
            .trace_v2(TraceEventCodes::SQLITE_TRACE_STMT, Some(trace_statement));
        assert_eq!(store.lookup_base_cache_by_paths(&inputs).unwrap().len(), 5);
        store.connection.trace_v2(TraceEventCodes::empty(), None);
        let traced = TRACED_STATEMENTS.lock().unwrap();
        assert_eq!(
            traced
                .iter()
                .filter(|sql| sql.contains("WITH request_rows"))
                .count(),
            9
        );

        store
            .connection
            .set_limit(Limit::SQLITE_LIMIT_VARIABLE_NUMBER, 3)
            .unwrap();
        assert!(matches!(
            store.lookup_base_cache_by_paths(&inputs),
            Err(StoreError::InvalidState(message)) if message.contains("变量参数上限不足")
        ));
    }
}

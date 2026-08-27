//! SQLite 只读事务中的固定高水位和按表/主键分页快照。

use dedup_core::{ContentKey, MediaKind};
use dedup_media::{ImageStage2, PdqHash};
use rusqlite::{Connection, OpenFlags, Transaction, TransactionBehavior, params};

use crate::{
    ImageStage1Fields, NodeStore, SnapshotPage, SnapshotRow, StoreError, VideoFrameStage1Fields,
    VideoFrameStage2Fields, VideoMetadataFields,
    content::encode_content,
    delete::{DeleteOutcome, encode_deletion_tombstone},
    features::{
        encode_image_stage1, encode_image_stage2, encode_video_frame_stage1,
        encode_video_frame_stage2, encode_video_metadata,
    },
    open::fixed_bytes,
    outbox::outbox_high_seq_from,
    rows::RowEncoder,
};

/// 一次连接内持有的 SQLite 只读快照；Drop 时结束事务，断线后必须重新开始。
pub struct Snapshot<'store> {
    transaction: Transaction<'store>,
    high_seq: u64,
}

/// 节点 actor 跨多个网络请求持有的独立 SQLite 只读事务。
pub struct OwnedSnapshot {
    connection: Connection,
    high_seq: u64,
}

impl NodeStore {
    /// 开启只读快照并冻结开始时已经提交的 outbox 高水位。
    pub fn begin_snapshot(&mut self) -> Result<Snapshot<'_>, StoreError> {
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Deferred)?;
        let high_seq = outbox_high_seq_from(&transaction)?;
        Ok(Snapshot {
            transaction,
            high_seq,
        })
    }

    /// 为节点 actor 打开独立只读连接，并在同一 SQLite 事务中保持整个网络快照。
    pub fn begin_owned_snapshot(&self) -> Result<OwnedSnapshot, StoreError> {
        let path = self
            .database_path
            .as_ref()
            .ok_or_else(|| StoreError::InvalidState("内存数据库不能创建跨请求只读快照".into()))?;
        let connection = Connection::open_with_flags(path, OpenFlags::SQLITE_OPEN_READ_ONLY)?;
        connection.busy_timeout(std::time::Duration::from_secs(5))?;
        connection.execute_batch("BEGIN DEFERRED TRANSACTION")?;
        let high_seq = outbox_high_seq_from(&connection)?;
        Ok(OwnedSnapshot {
            connection,
            high_seq,
        })
    }
}

impl Snapshot<'_> {
    /// 返回快照开始时的本地 outbox 高水位。
    pub const fn high_seq(&self) -> u64 {
        self.high_seq
    }

    /// 按固定表名和稳定主键读取一页；空 cursor 表示从该表开头读取。
    pub fn read_page(
        &self,
        table_name: &str,
        cursor: &str,
        limit: usize,
    ) -> Result<SnapshotPage, StoreError> {
        SnapshotReader(&self.transaction).read_page(table_name, cursor, limit)
    }
}

impl OwnedSnapshot {
    /// 返回独立只读事务开始时的本地 outbox 高水位。
    pub const fn high_seq(&self) -> u64 {
        self.high_seq
    }

    /// 从同一个独立只读事务按固定表和稳定主键读取一页。
    pub fn read_page(
        &self,
        table_name: &str,
        cursor: &str,
        limit: usize,
    ) -> Result<SnapshotPage, StoreError> {
        SnapshotReader(&self.connection).read_page(table_name, cursor, limit)
    }
}

struct SnapshotReader<'connection>(&'connection Connection);

impl SnapshotReader<'_> {
    /// 按固定表名和稳定主键读取一页；空 cursor 表示从该表开头读取。
    pub fn read_page(
        &self,
        table_name: &str,
        cursor: &str,
        limit: usize,
    ) -> Result<SnapshotPage, StoreError> {
        let fetch_limit = limit.saturating_add(1);
        let rows = match table_name {
            "contents" => self.read_contents(cursor, fetch_limit)?,
            "files" => self.read_files(cursor, fetch_limit)?,
            "image_stage1" => self.read_image_stage1(cursor, fetch_limit)?,
            "image_stage2" => self.read_image_stage2(cursor, fetch_limit)?,
            "video_metadata" => self.read_video_metadata(cursor, fetch_limit)?,
            "video_frame_stage1" => self.read_video_frame_stage1(cursor, fetch_limit)?,
            "video_frame_stage2" => self.read_video_frame_stage2(cursor, fetch_limit)?,
            "deletion_tombstones" => self.read_deletion_tombstones(cursor, fetch_limit)?,
            _ => return Err(StoreError::InvalidSnapshotTable(table_name.to_owned())),
        };
        Ok(finish_page(table_name, rows, limit))
    }

    fn read_contents(&self, cursor: &str, limit: usize) -> Result<Vec<SnapshotRow>, StoreError> {
        let mut statement = self.0.prepare(
            "SELECT hex(md5)||':'||printf('%020d',file_size),md5,file_size,media_kind,base_complete
             FROM contents
             WHERE hex(md5)||':'||printf('%020d',file_size)>?1
             ORDER BY md5,file_size LIMIT ?2",
        )?;
        let raw = statement
            .query_map(params![cursor, limit as i64], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, Vec<u8>>(1)?,
                    row.get::<_, i64>(2)?,
                    row.get::<_, String>(3)?,
                    row.get::<_, bool>(4)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|(key, md5, size, kind, base_complete)| {
                let kind = parse_media_kind(&kind)?;
                let content_key = ContentKey::new(fixed_bytes(md5, "contents.md5")?, size as u64);
                Ok(SnapshotRow {
                    key,
                    payload: encode_content(content_key, kind, base_complete),
                })
            })
            .collect()
    }

    fn read_files(&self, cursor: &str, limit: usize) -> Result<Vec<SnapshotRow>, StoreError> {
        let mut statement = self.0.prepare(
            "SELECT f.machine_id||':'||f.normalized_path,
                    f.machine_id,f.normalized_path,f.display_path,f.file_size,
                    c.md5,c.file_size,f.active
             FROM files f JOIN contents c ON c.content_id=f.content_id
             WHERE f.machine_id||':'||f.normalized_path>?1
             ORDER BY f.machine_id,f.normalized_path LIMIT ?2",
        )?;
        let raw = statement
            .query_map(params![cursor, limit as i64], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, String>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, String>(3)?,
                    row.get::<_, i64>(4)?,
                    row.get::<_, Vec<u8>>(5)?,
                    row.get::<_, i64>(6)?,
                    row.get::<_, bool>(7)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(
                |(key, machine, normalized, display, size, md5, content_size, active)| {
                    let payload = RowEncoder::new(1)
                        .text(&machine)
                        .text(&normalized)
                        .text(&display)
                        .u64(size as u64)
                        .bytes(&fixed_bytes::<16>(md5, "contents.md5")?)
                        .u64(content_size as u64)
                        .u8(u8::from(active))
                        .finish();
                    Ok(SnapshotRow { key, payload })
                },
            )
            .collect()
    }

    fn read_image_stage1(
        &self,
        cursor: &str,
        limit: usize,
    ) -> Result<Vec<SnapshotRow>, StoreError> {
        let mut statement = self.0.prepare(
            "SELECT hex(c.md5)||':'||printf('%020d',c.file_size),c.md5,c.file_size,
                    f.width,f.height,f.pdq,f.quality
             FROM image_stage1 f JOIN contents c ON c.content_id=f.content_id
             WHERE hex(c.md5)||':'||printf('%020d',c.file_size)>?1
             ORDER BY c.md5,c.file_size LIMIT ?2",
        )?;
        let raw = statement
            .query_map(params![cursor, limit as i64], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, Vec<u8>>(1)?,
                    row.get::<_, i64>(2)?,
                    row.get::<_, Option<u32>>(3)?,
                    row.get::<_, Option<u32>>(4)?,
                    row.get::<_, Option<Vec<u8>>>(5)?,
                    row.get::<_, Option<u8>>(6)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|(key, md5, size, width, height, pdq, quality)| {
                let content_key = ContentKey::new(fixed_bytes(md5, "contents.md5")?, size as u64);
                let pdq = pdq
                    .map(|value| fixed_bytes(value, "image_stage1.pdq").map(PdqHash::from_bytes))
                    .transpose()?;
                Ok(SnapshotRow {
                    key,
                    payload: encode_image_stage1(
                        content_key,
                        ImageStage1Fields {
                            width,
                            height,
                            pdq,
                            quality,
                        },
                    ),
                })
            })
            .collect()
    }

    fn read_image_stage2(
        &self,
        cursor: &str,
        limit: usize,
    ) -> Result<Vec<SnapshotRow>, StoreError> {
        let mut statement = self.0.prepare(
            "SELECT hex(c.md5)||':'||printf('%020d',c.file_size),c.md5,c.file_size,
                    f.phash_parts,f.sobel
             FROM image_stage2 f JOIN contents c ON c.content_id=f.content_id
             WHERE f.phash_parts IS NOT NULL AND f.sobel IS NOT NULL
               AND hex(c.md5)||':'||printf('%020d',c.file_size)>?1
             ORDER BY c.md5,c.file_size LIMIT ?2",
        )?;
        let raw = statement
            .query_map(params![cursor, limit as i64], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, Vec<u8>>(1)?,
                    row.get::<_, i64>(2)?,
                    row.get::<_, Vec<u8>>(3)?,
                    row.get::<_, Vec<u8>>(4)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|(key, md5, size, phash, sobel)| {
                let content_key = ContentKey::new(fixed_bytes(md5, "contents.md5")?, size as u64);
                let features = decode_stage2_for_snapshot(phash, sobel)?;
                Ok(SnapshotRow {
                    key,
                    payload: encode_image_stage2(content_key, &features),
                })
            })
            .collect()
    }

    fn read_video_metadata(
        &self,
        cursor: &str,
        limit: usize,
    ) -> Result<Vec<SnapshotRow>, StoreError> {
        let mut statement = self.0.prepare(
            "SELECT hex(c.md5)||':'||printf('%020d',c.file_size),c.md5,c.file_size,
                    f.duration_ms,f.width,f.height
             FROM video_metadata f JOIN contents c ON c.content_id=f.content_id
             WHERE hex(c.md5)||':'||printf('%020d',c.file_size)>?1
             ORDER BY c.md5,c.file_size LIMIT ?2",
        )?;
        let raw = statement
            .query_map(params![cursor, limit as i64], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, Vec<u8>>(1)?,
                    row.get::<_, i64>(2)?,
                    row.get::<_, Option<i64>>(3)?,
                    row.get::<_, Option<u32>>(4)?,
                    row.get::<_, Option<u32>>(5)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|(key, md5, size, duration, width, height)| {
                let content_key = ContentKey::new(fixed_bytes(md5, "contents.md5")?, size as u64);
                Ok(SnapshotRow {
                    key,
                    payload: encode_video_metadata(
                        content_key,
                        VideoMetadataFields {
                            duration_ms: duration.map(|value| value as u64),
                            width,
                            height,
                        },
                    ),
                })
            })
            .collect()
    }

    fn read_video_frame_stage1(
        &self,
        cursor: &str,
        limit: usize,
    ) -> Result<Vec<SnapshotRow>, StoreError> {
        let mut statement = self.0.prepare(
            "SELECT hex(c.md5)||':'||printf('%020d',c.file_size)||':'||printf('%01d',f.slot),
                    c.md5,c.file_size,f.slot,f.time_ms,f.decoded,f.width,f.height,f.pdq,f.quality
             FROM video_frame_stage1 f JOIN contents c ON c.content_id=f.content_id
             WHERE hex(c.md5)||':'||printf('%020d',c.file_size)||':'||printf('%01d',f.slot)>?1
             ORDER BY c.md5,c.file_size,f.slot LIMIT ?2",
        )?;
        let raw = statement
            .query_map(params![cursor, limit as i64], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, Vec<u8>>(1)?,
                    row.get::<_, i64>(2)?,
                    row.get::<_, u8>(3)?,
                    row.get::<_, i64>(4)?,
                    row.get::<_, bool>(5)?,
                    row.get::<_, Option<u32>>(6)?,
                    row.get::<_, Option<u32>>(7)?,
                    row.get::<_, Option<Vec<u8>>>(8)?,
                    row.get::<_, Option<u8>>(9)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(
                |(key, md5, size, slot, time, decoded, width, height, pdq, quality)| {
                    let content_key =
                        ContentKey::new(fixed_bytes(md5, "contents.md5")?, size as u64);
                    let pdq = pdq
                        .map(|value| {
                            fixed_bytes(value, "video_frame_stage1.pdq").map(PdqHash::from_bytes)
                        })
                        .transpose()?;
                    Ok(SnapshotRow {
                        key,
                        payload: encode_video_frame_stage1(
                            content_key,
                            VideoFrameStage1Fields {
                                slot,
                                time_ms: time as u64,
                                decoded,
                                width,
                                height,
                                pdq,
                                quality,
                            },
                        ),
                    })
                },
            )
            .collect()
    }

    fn read_video_frame_stage2(
        &self,
        cursor: &str,
        limit: usize,
    ) -> Result<Vec<SnapshotRow>, StoreError> {
        let mut statement = self.0.prepare(
            "SELECT hex(c.md5)||':'||printf('%020d',c.file_size)||':'||printf('%01d',f.slot),
                    c.md5,c.file_size,f.slot,f.phash_parts,f.sobel
             FROM video_frame_stage2 f JOIN contents c ON c.content_id=f.content_id
             WHERE f.phash_parts IS NOT NULL AND f.sobel IS NOT NULL
               AND hex(c.md5)||':'||printf('%020d',c.file_size)||':'||printf('%01d',f.slot)>?1
             ORDER BY c.md5,c.file_size,f.slot LIMIT ?2",
        )?;
        let raw = statement
            .query_map(params![cursor, limit as i64], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, Vec<u8>>(1)?,
                    row.get::<_, i64>(2)?,
                    row.get::<_, u8>(3)?,
                    row.get::<_, Vec<u8>>(4)?,
                    row.get::<_, Vec<u8>>(5)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|(key, md5, size, slot, phash, sobel)| {
                let content_key = ContentKey::new(fixed_bytes(md5, "contents.md5")?, size as u64);
                let features = decode_stage2_for_snapshot(phash, sobel)?;
                Ok(SnapshotRow {
                    key,
                    payload: encode_video_frame_stage2(
                        content_key,
                        VideoFrameStage2Fields { slot, features },
                    ),
                })
            })
            .collect()
    }

    fn read_deletion_tombstones(
        &self,
        cursor: &str,
        limit: usize,
    ) -> Result<Vec<SnapshotRow>, StoreError> {
        let mut statement = self.0.prepare(
            "SELECT machine_id||':'||normalized_path,machine_id,normalized_path,md5,file_size,outcome
             FROM deletion_tombstones
             WHERE machine_id||':'||normalized_path>?1
             ORDER BY machine_id,normalized_path LIMIT ?2",
        )?;
        let raw = statement
            .query_map(params![cursor, limit as i64], |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, String>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, Vec<u8>>(3)?,
                    row.get::<_, i64>(4)?,
                    row.get::<_, String>(5)?,
                ))
            })?
            .collect::<Result<Vec<_>, _>>()?;
        raw.into_iter()
            .map(|(key, machine, path, md5, size, outcome)| {
                let content_key = ContentKey::new(fixed_bytes(md5, "contents.md5")?, size as u64);
                let outcome = match outcome.as_str() {
                    "recycled" => DeleteOutcome::Recycled,
                    "deleted" => DeleteOutcome::Deleted,
                    _ => return Err(StoreError::InvalidState("未知删除墓碑结果".into())),
                };
                Ok(SnapshotRow {
                    key,
                    payload: encode_deletion_tombstone(&machine, &path, content_key, outcome),
                })
            })
            .collect()
    }
}

fn finish_page(table_name: &str, mut rows: Vec<SnapshotRow>, limit: usize) -> SnapshotPage {
    let done = rows.len() <= limit;
    if !done {
        rows.truncate(limit);
    }
    let next_cursor = (!done)
        .then(|| rows.last().map(|row| row.key.clone()))
        .flatten();
    SnapshotPage {
        table_name: table_name.to_owned(),
        rows,
        next_cursor,
        done,
    }
}

fn parse_media_kind(value: &str) -> Result<MediaKind, StoreError> {
    match value {
        "image" => Ok(MediaKind::Image),
        "video" => Ok(MediaKind::Video),
        "other" => Ok(MediaKind::Other),
        _ => Err(StoreError::InvalidFeature("未知 media_kind")),
    }
}

fn decode_stage2_for_snapshot(phash: Vec<u8>, sobel: Vec<u8>) -> Result<ImageStage2, StoreError> {
    let phash = fixed_bytes::<72>(phash, "stage2.phash_parts")?;
    let mut phash_parts = [0_u64; 9];
    for (index, value) in phash.chunks_exact(8).enumerate() {
        phash_parts[index] = u64::from_le_bytes(value.try_into().expect("固定八字节分块"));
    }
    let sobel = fixed_bytes::<512>(sobel, "stage2.sobel")?;
    let mut histogram = [0.0_f32; 128];
    for (index, value) in sobel.chunks_exact(4).enumerate() {
        histogram[index] = f32::from_le_bytes(value.try_into().expect("固定四字节浮点"));
    }
    if histogram.iter().any(|value| !value.is_finite()) {
        return Err(StoreError::NonFiniteSobel);
    }
    Ok(ImageStage2 {
        phash_parts,
        sobel: histogram,
    })
}

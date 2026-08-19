//! 节点 outbox 的版本化解码，以及内容键优先的中心同步事务。

use dedup_core::{ContentKey, MachineId};
use dedup_protocol::proto;
use tokio_postgres::Transaction;

use super::{CentralError, CentralStore, invalid_payload, pg_i64};

/// 一次全量节点快照对应的未提交 PostgreSQL 事务。
///
/// 类型离开作用域而未调用 `commit` 时，`tokio-postgres` 会回滚所有页面，下一连接必须
/// 重新向节点请求新的 snapshot token。
pub struct CentralSnapshot<'a> {
    transaction: Transaction<'a>,
    machine_id: MachineId,
    high_seq: u64,
}

impl CentralStore {
    /// 在一个事务中按依赖顺序写入一批节点变更并推进该机器唯一游标。
    pub async fn apply_sync_batch(
        &mut self,
        machine_id: &MachineId,
        batch: &proto::SyncChangeBatch,
    ) -> Result<u64, CentralError> {
        let decoded = batch
            .changes
            .iter()
            .map(|change| decode_change(change).map(|decoded| (change.seq, decoded)))
            .collect::<Result<Vec<_>, _>>()?;
        let transaction = self.client.transaction().await?;
        transaction
            .execute(
                "INSERT INTO nodes(machine_id) VALUES($1)
                 ON CONFLICT(machine_id) DO UPDATE SET last_seen_at=now()",
                &[&machine_id.as_str()],
            )
            .await?;
        transaction
            .execute(
                "INSERT INTO sync_cursors(machine_id,committed_seq) VALUES($1,0)
                 ON CONFLICT(machine_id) DO NOTHING",
                &[&machine_id.as_str()],
            )
            .await?;

        for (sequence, change) in decoded
            .iter()
            .filter(|(_, change)| matches!(change, DecodedChange::Content { .. }))
        {
            apply_change(&transaction, machine_id, *sequence, change).await?;
        }
        for (sequence, change) in decoded
            .iter()
            .filter(|(_, change)| !matches!(change, DecodedChange::Content { .. }))
        {
            apply_change(&transaction, machine_id, *sequence, change).await?;
        }
        let committed_seq = batch.changes.last().map_or_else(|| 0, |change| change.seq);
        transaction
            .execute(
                "UPDATE sync_cursors SET committed_seq=GREATEST(committed_seq,$2),updated_at=now()
                 WHERE machine_id=$1",
                &[&machine_id.as_str(), &pg_i64(committed_seq, "同步序号")?],
            )
            .await?;
        transaction.commit().await?;
        Ok(committed_seq)
    }

    /// 返回指定 MD5 在中心按文件大小区分后的内容数量，主要用于诊断与测试。
    pub async fn content_count(&self, md5: [u8; 16]) -> Result<u64, CentralError> {
        let count: i64 = self
            .client
            .query_one(
                "SELECT COUNT(*) FROM contents WHERE md5=$1",
                &[&md5.as_slice()],
            )
            .await?
            .get(0);
        Ok(count as u64)
    }

    /// 返回一个 ContentKey 当前在多少机器路径上有位置记录。
    pub async fn location_count(&self, md5: [u8; 16], file_size: u64) -> Result<u64, CentralError> {
        let count: i64 = self
            .client
            .query_one(
                "SELECT COUNT(*) FROM file_locations f JOIN contents c ON c.content_id=f.content_id
                 WHERE c.md5=$1 AND c.file_size=$2",
                &[&md5.as_slice(), &pg_i64(file_size, "文件大小")?],
            )
            .await?
            .get(0);
        Ok(count as u64)
    }

    /// 返回 PostgreSQL 已提交的节点同步游标；未知节点为 0。
    pub async fn sync_cursor(&self, machine_id: &MachineId) -> Result<u64, CentralError> {
        let row = self
            .client
            .query_opt(
                "SELECT committed_seq FROM sync_cursors WHERE machine_id=$1",
                &[&machine_id.as_str()],
            )
            .await?;
        Ok(row.map_or(0, |row| row.get::<_, i64>(0) as u64))
    }

    /// 开启整次快照事务；旧位置先统一失效，页面写完前中心对外不可见半份数据。
    pub async fn begin_snapshot_replace(
        &mut self,
        machine_id: &MachineId,
        high_seq: u64,
    ) -> Result<CentralSnapshot<'_>, CentralError> {
        let transaction = self.client.transaction().await?;
        transaction
            .execute(
                "INSERT INTO nodes(machine_id) VALUES($1)
                 ON CONFLICT(machine_id) DO UPDATE SET last_seen_at=now()",
                &[&machine_id.as_str()],
            )
            .await?;
        transaction
            .execute(
                "INSERT INTO sync_cursors(machine_id,committed_seq) VALUES($1,0)
                 ON CONFLICT(machine_id) DO NOTHING",
                &[&machine_id.as_str()],
            )
            .await?;
        transaction
            .execute(
                "UPDATE file_locations SET active=FALSE,updated_seq=$2 WHERE machine_id=$1",
                &[&machine_id.as_str(), &pg_i64(high_seq, "快照高水位")?],
            )
            .await?;
        transaction
            .execute(
                "DELETE FROM deletion_tombstones WHERE machine_id=$1",
                &[&machine_id.as_str()],
            )
            .await?;
        Ok(CentralSnapshot {
            transaction,
            machine_id: machine_id.clone(),
            high_seq,
        })
    }
}

impl CentralSnapshot<'_> {
    /// 按固定表顺序把一页版本化载荷写入当前快照事务。
    pub async fn apply_page(
        &mut self,
        table_name: &str,
        rows: &[Vec<u8>],
    ) -> Result<(), CentralError> {
        let entity_kind = snapshot_entity_kind(table_name)?;
        for payload in rows {
            let change = proto::SyncChange {
                seq: self.high_seq,
                entity_kind: entity_kind.into(),
                payload: payload.clone(),
            };
            let decoded = decode_change(&change)?;
            apply_change(&self.transaction, &self.machine_id, self.high_seq, &decoded).await?;
        }
        Ok(())
    }

    /// 原子提交全部快照页面，并把中心游标推进到快照起始高水位。
    pub async fn commit(self) -> Result<u64, CentralError> {
        self.transaction
            .execute(
                "UPDATE sync_cursors
                 SET committed_seq=GREATEST(committed_seq,$2),updated_at=now()
                 WHERE machine_id=$1",
                &[
                    &self.machine_id.as_str(),
                    &pg_i64(self.high_seq, "快照高水位")?,
                ],
            )
            .await?;
        self.transaction.commit().await?;
        Ok(self.high_seq)
    }
}

#[derive(Debug)]
enum DecodedChange {
    Content {
        key: ContentKey,
        media_kind: &'static str,
    },
    File {
        machine_id: String,
        normalized_path: String,
        display_path: String,
        file_size: u64,
        key: ContentKey,
        active: bool,
    },
    ImageStage1 {
        key: ContentKey,
        width: Option<u32>,
        height: Option<u32>,
        pdq: Option<Vec<u8>>,
        quality: Option<u8>,
    },
    ImageStage2 {
        key: ContentKey,
        phash_parts: Vec<u8>,
        sobel: Vec<u8>,
    },
    VideoMetadata {
        key: ContentKey,
        duration_ms: Option<u64>,
        width: Option<u32>,
        height: Option<u32>,
    },
    VideoFrameStage1 {
        key: ContentKey,
        slot: u8,
        time_ms: u64,
        decoded: bool,
        width: Option<u32>,
        height: Option<u32>,
        pdq: Option<Vec<u8>>,
        quality: Option<u8>,
    },
    VideoFrameStage2 {
        key: ContentKey,
        slot: u8,
        phash_parts: Vec<u8>,
        sobel: Vec<u8>,
    },
    DeletionTombstone {
        machine_id: String,
        normalized_path: String,
        key: ContentKey,
        outcome: &'static str,
    },
    ContactSheet,
}

async fn apply_change(
    transaction: &Transaction<'_>,
    source_machine: &MachineId,
    sequence: u64,
    change: &DecodedChange,
) -> Result<(), CentralError> {
    match change {
        DecodedChange::Content { key, media_kind } => {
            transaction
                .execute(
                    "INSERT INTO contents(md5,file_size,media_kind) VALUES($1,$2,$3)
                     ON CONFLICT(md5,file_size) DO UPDATE SET media_kind=excluded.media_kind",
                    &[
                        &key.md5().as_slice(),
                        &pg_i64(key.file_size(), "文件大小")?,
                        media_kind,
                    ],
                )
                .await?;
        }
        DecodedChange::File {
            machine_id,
            normalized_path,
            display_path,
            file_size,
            key,
            active,
        } => {
            if machine_id != source_machine.as_str() {
                return Err(invalid_payload("文件变更 machine_id 与同步来源不一致"));
            }
            let content_id = content_id(transaction, *key).await?;
            transaction
                .execute(
                    "INSERT INTO file_locations(
                       machine_id,normalized_path,display_path,file_size,content_id,active,updated_seq)
                     VALUES($1,$2,$3,$4,$5,$6,$7)
                     ON CONFLICT(machine_id,normalized_path) DO UPDATE SET
                       display_path=excluded.display_path,file_size=excluded.file_size,
                       content_id=excluded.content_id,active=excluded.active,updated_seq=excluded.updated_seq",
                    &[
                        &machine_id,
                        &normalized_path,
                        &display_path,
                        &pg_i64(*file_size, "文件大小")?,
                        &content_id,
                        active,
                        &pg_i64(sequence, "同步序号")?,
                    ],
                )
                .await?;
        }
        DecodedChange::ImageStage1 {
            key,
            width,
            height,
            pdq,
            quality,
        } => {
            let content_id = content_id(transaction, *key).await?;
            transaction
                .execute(
                    "INSERT INTO image_stage1(content_id,width,height,pdq,quality)
                     VALUES($1,$2,$3,$4,$5) ON CONFLICT(content_id) DO UPDATE SET
                       width=excluded.width,height=excluded.height,pdq=excluded.pdq,quality=excluded.quality",
                    &[
                        &content_id,
                        &width.map(|value| value as i32),
                        &height.map(|value| value as i32),
                        pdq,
                        &quality.map(i16::from),
                    ],
                )
                .await?;
        }
        DecodedChange::ImageStage2 {
            key,
            phash_parts,
            sobel,
        } => {
            let content_id = content_id(transaction, *key).await?;
            transaction
                .execute(
                    "INSERT INTO image_stage2(content_id,phash_parts,sobel) VALUES($1,$2,$3)
                     ON CONFLICT(content_id) DO UPDATE SET
                       phash_parts=excluded.phash_parts,sobel=excluded.sobel",
                    &[&content_id, phash_parts, sobel],
                )
                .await?;
        }
        DecodedChange::VideoMetadata {
            key,
            duration_ms,
            width,
            height,
        } => {
            let content_id = content_id(transaction, *key).await?;
            transaction
                .execute(
                    "INSERT INTO video_metadata(content_id,duration_ms,width,height)
                     VALUES($1,$2,$3,$4) ON CONFLICT(content_id) DO UPDATE SET
                       duration_ms=excluded.duration_ms,width=excluded.width,height=excluded.height",
                    &[
                        &content_id,
                        &duration_ms
                            .map(|value| pg_i64(value, "视频时长"))
                            .transpose()?,
                        &width.map(|value| value as i32),
                        &height.map(|value| value as i32),
                    ],
                )
                .await?;
        }
        DecodedChange::VideoFrameStage1 {
            key,
            slot,
            time_ms,
            decoded,
            width,
            height,
            pdq,
            quality,
        } => {
            let content_id = content_id(transaction, *key).await?;
            transaction
                .execute(
                    "INSERT INTO video_frame_stage1(
                       content_id,slot,time_ms,decoded,width,height,pdq,quality)
                     VALUES($1,$2,$3,$4,$5,$6,$7,$8)
                     ON CONFLICT(content_id,slot) DO UPDATE SET
                       time_ms=excluded.time_ms,decoded=excluded.decoded,width=excluded.width,
                       height=excluded.height,pdq=excluded.pdq,quality=excluded.quality",
                    &[
                        &content_id,
                        &i16::from(*slot),
                        &pg_i64(*time_ms, "帧时间")?,
                        decoded,
                        &width.map(|value| value as i32),
                        &height.map(|value| value as i32),
                        pdq,
                        &quality.map(i16::from),
                    ],
                )
                .await?;
        }
        DecodedChange::VideoFrameStage2 {
            key,
            slot,
            phash_parts,
            sobel,
        } => {
            let content_id = content_id(transaction, *key).await?;
            transaction
                .execute(
                    "INSERT INTO video_frame_stage2(content_id,slot,phash_parts,sobel)
                     VALUES($1,$2,$3,$4) ON CONFLICT(content_id,slot) DO UPDATE SET
                       phash_parts=excluded.phash_parts,sobel=excluded.sobel",
                    &[&content_id, &i16::from(*slot), phash_parts, sobel],
                )
                .await?;
        }
        DecodedChange::DeletionTombstone {
            machine_id,
            normalized_path,
            key,
            outcome,
        } => {
            if machine_id != source_machine.as_str() {
                return Err(invalid_payload("删除墓碑 machine_id 与同步来源不一致"));
            }
            transaction
                .execute(
                    "INSERT INTO deletion_tombstones(
                       machine_id,normalized_path,md5,file_size,outcome,updated_seq)
                     VALUES($1,$2,$3,$4,$5,$6)
                     ON CONFLICT(machine_id,normalized_path) DO UPDATE SET
                       md5=excluded.md5,file_size=excluded.file_size,outcome=excluded.outcome,
                       updated_seq=excluded.updated_seq",
                    &[
                        machine_id,
                        normalized_path,
                        &key.md5().as_slice(),
                        &pg_i64(key.file_size(), "删除墓碑文件大小")?,
                        outcome,
                        &pg_i64(sequence, "删除墓碑同步序号")?,
                    ],
                )
                .await?;
        }
        DecodedChange::ContactSheet => {}
    }
    Ok(())
}

async fn content_id(transaction: &Transaction<'_>, key: ContentKey) -> Result<i64, CentralError> {
    Ok(transaction
        .query_one(
            "SELECT content_id FROM contents WHERE md5=$1 AND file_size=$2",
            &[&key.md5().as_slice(), &pg_i64(key.file_size(), "文件大小")?],
        )
        .await?
        .get(0))
}

fn decode_change(change: &proto::SyncChange) -> Result<DecodedChange, CentralError> {
    if change.entity_kind == "contact_sheet" {
        return Ok(DecodedChange::ContactSheet);
    }
    let mut reader = PayloadReader::new(&change.payload)?;
    let decoded = match change.entity_kind.as_str() {
        "content" => DecodedChange::Content {
            key: reader.content_key()?,
            media_kind: match reader.u8()? {
                1 => "image",
                2 => "video",
                3 => "other",
                _ => return Err(invalid_payload("content.media_kind 无效")),
            },
        },
        "file" => DecodedChange::File {
            machine_id: reader.text()?,
            normalized_path: reader.text()?,
            display_path: reader.text()?,
            file_size: reader.u64()?,
            key: reader.content_key()?,
            active: reader.boolean()?,
        },
        "image_stage1" => DecodedChange::ImageStage1 {
            key: reader.content_key()?,
            width: reader.optional_u32()?,
            height: reader.optional_u32()?,
            pdq: reader.optional_bytes()?,
            quality: reader.optional_u8()?,
        },
        "image_stage2" => DecodedChange::ImageStage2 {
            key: reader.content_key()?,
            phash_parts: reader.bytes()?,
            sobel: reader.bytes()?,
        },
        "video_metadata" => DecodedChange::VideoMetadata {
            key: reader.content_key()?,
            duration_ms: reader.optional_u64()?,
            width: reader.optional_u32()?,
            height: reader.optional_u32()?,
        },
        "video_frame_stage1" => DecodedChange::VideoFrameStage1 {
            key: reader.content_key()?,
            slot: reader.u8()?,
            time_ms: reader.u64()?,
            decoded: reader.boolean()?,
            width: reader.optional_u32()?,
            height: reader.optional_u32()?,
            pdq: reader.optional_bytes()?,
            quality: reader.optional_u8()?,
        },
        "video_frame_stage2" => DecodedChange::VideoFrameStage2 {
            key: reader.content_key()?,
            slot: reader.u8()?,
            phash_parts: reader.bytes()?,
            sobel: reader.bytes()?,
        },
        "deletion_tombstone" => DecodedChange::DeletionTombstone {
            machine_id: reader.text()?,
            normalized_path: reader.text()?,
            key: reader.content_key()?,
            outcome: match reader.text()?.as_str() {
                "recycled" => "recycled",
                "deleted" => "deleted",
                _ => return Err(invalid_payload("删除墓碑 outcome 无效")),
            },
        },
        other => return Err(invalid_payload(format!("未知 entity_kind: {other}"))),
    };
    reader.finish()?;
    Ok(decoded)
}

fn snapshot_entity_kind(table_name: &str) -> Result<&'static str, CentralError> {
    match table_name {
        "contents" => Ok("content"),
        "files" => Ok("file"),
        "image_stage1" => Ok("image_stage1"),
        "image_stage2" => Ok("image_stage2"),
        "video_metadata" => Ok("video_metadata"),
        "video_frame_stage1" => Ok("video_frame_stage1"),
        "video_frame_stage2" => Ok("video_frame_stage2"),
        "deletion_tombstones" => Ok("deletion_tombstone"),
        other => Err(invalid_payload(format!("未知快照表: {other}"))),
    }
}

struct PayloadReader<'a> {
    bytes: &'a [u8],
    at: usize,
}

impl<'a> PayloadReader<'a> {
    fn new(bytes: &'a [u8]) -> Result<Self, CentralError> {
        if bytes.first() != Some(&1) {
            return Err(invalid_payload("载荷版本不是 1"));
        }
        Ok(Self { bytes, at: 1 })
    }

    fn content_key(&mut self) -> Result<ContentKey, CentralError> {
        let md5 = self
            .bytes()?
            .try_into()
            .map_err(|_| invalid_payload("MD5 长度不是 16"))?;
        Ok(ContentKey::new(md5, self.u64()?))
    }

    fn bytes(&mut self) -> Result<Vec<u8>, CentralError> {
        let length = u32::from_be_bytes(self.take_array()?) as usize;
        Ok(self.take_dynamic(length)?.to_vec())
    }

    fn text(&mut self) -> Result<String, CentralError> {
        String::from_utf8(self.bytes()?).map_err(|_| invalid_payload("文本不是 UTF-8"))
    }

    fn u64(&mut self) -> Result<u64, CentralError> {
        Ok(u64::from_be_bytes(self.take_array()?))
    }

    fn u32(&mut self) -> Result<u32, CentralError> {
        Ok(u32::from_be_bytes(self.take_array()?))
    }

    fn u8(&mut self) -> Result<u8, CentralError> {
        Ok(self.take_dynamic(1)?[0])
    }

    fn boolean(&mut self) -> Result<bool, CentralError> {
        match self.u8()? {
            0 => Ok(false),
            1 => Ok(true),
            _ => Err(invalid_payload("布尔字段无效")),
        }
    }

    fn optional_u64(&mut self) -> Result<Option<u64>, CentralError> {
        self.optional(|reader| reader.u64())
    }

    fn optional_u32(&mut self) -> Result<Option<u32>, CentralError> {
        self.optional(|reader| reader.u32())
    }

    fn optional_u8(&mut self) -> Result<Option<u8>, CentralError> {
        self.optional(|reader| reader.u8())
    }

    fn optional_bytes(&mut self) -> Result<Option<Vec<u8>>, CentralError> {
        self.optional(|reader| reader.bytes())
    }

    fn optional<T>(
        &mut self,
        read: impl FnOnce(&mut Self) -> Result<T, CentralError>,
    ) -> Result<Option<T>, CentralError> {
        match self.u8()? {
            0 => Ok(None),
            1 => read(self).map(Some),
            _ => Err(invalid_payload("可选字段标记无效")),
        }
    }

    fn take_array<const N: usize>(&mut self) -> Result<[u8; N], CentralError> {
        self.take_dynamic(N)?
            .try_into()
            .map_err(|_| invalid_payload("固定字段被截断"))
    }

    fn take_dynamic(&mut self, length: usize) -> Result<&'a [u8], CentralError> {
        let end = self
            .at
            .checked_add(length)
            .ok_or_else(|| invalid_payload("载荷长度溢出"))?;
        let value = self
            .bytes
            .get(self.at..end)
            .ok_or_else(|| invalid_payload("载荷被截断"))?;
        self.at = end;
        Ok(value)
    }

    fn finish(&self) -> Result<(), CentralError> {
        if self.at == self.bytes.len() {
            Ok(())
        } else {
            Err(invalid_payload("载荷尾部有多余字节"))
        }
    }
}

//! 批量路径缓存查询，以及 MD5 索引后按文件大小确认的内容/位置事务。

use dedup_core::{ContentKey, MediaKind};
use rusqlite::{OptionalExtension, Transaction, params};

use crate::{
    CacheLookup, ContentId, ContentRecord, NodeStore, ScannedPath, StoreError,
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
                encode_content(ContentKey::new(md5, scanned.file_size), media_kind),
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

pub(crate) fn encode_content(key: ContentKey, kind: MediaKind) -> Vec<u8> {
    RowEncoder::new(1)
        .bytes(&key.md5())
        .u64(key.file_size())
        .u8(match kind {
            MediaKind::Image => 1,
            MediaKind::Video => 2,
            MediaKind::Other => 3,
        })
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

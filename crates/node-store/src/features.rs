//! 图片/视频一筛与联合二筛的事务写入和完整结果加载。

use dedup_core::MediaKind;
use dedup_media::{ImageStage1, ImageStage2, PdqHash, phash_parts_to_blob};
use rusqlite::{OptionalExtension, Transaction, TransactionBehavior, params};

use crate::{
    BaseCacheRecord, CompleteStage1, CompleteStage2, ContentId, FeatureWrite, ImageStage1Fields,
    NodeStore, StoreError, TaskItemApplyResult, TaskItemCompletion, TaskItemIdentity,
    VideoFrameStage1Fields, VideoFrameStage2Fields, VideoMetadataFields,
    cache_integrity::classify_cache_completeness,
    content::{content_key_in_transaction, encode_content},
    open::sqlite_integer,
    outbox::append_sync_change,
    rows::RowEncoder,
    tasks::{TaskItemIdentityState, classify_task_item_identity, complete_item_in_transaction},
};

impl NodeStore {
    /// 删除已经被磁盘满清理器移除的联系表本地引用。
    pub fn clear_contact_sheet_references(
        &mut self,
        relative_paths: &[String],
    ) -> Result<usize, StoreError> {
        let transaction = self.connection.transaction()?;
        let mut removed = 0;
        for relative_path in relative_paths {
            removed += transaction.execute(
                "DELETE FROM contact_sheets WHERE relative_path=?1",
                [relative_path],
            )?;
        }
        transaction.commit()?;
        Ok(removed)
    }

    /// 在同一事务写特征、可选任务项成功状态和对应 outbox，返回新 outbox 序号。
    pub fn commit_feature_result(
        &mut self,
        content_id: ContentId,
        task_item_id: Option<&str>,
        result: FeatureWrite,
    ) -> Result<u64, StoreError> {
        let transaction = self.connection.transaction()?;
        let key = content_key_in_transaction(&transaction, content_id)?;
        let (entity_kind, payload) = match result {
            FeatureWrite::ImageStage1(fields) => {
                transaction.execute(
                    "INSERT INTO image_stage1(content_id,width,height,pdq,quality)
                     VALUES(?1,?2,?3,?4,?5)
                     ON CONFLICT(content_id) DO UPDATE SET
                       width=CASE WHEN excluded.width > 0 THEN excluded.width ELSE image_stage1.width END,
                       height=CASE WHEN excluded.height > 0 THEN excluded.height ELSE image_stage1.height END,
                       pdq=COALESCE(excluded.pdq,image_stage1.pdq),
                       quality=CASE WHEN excluded.quality BETWEEN 0 AND 100
                                    THEN excluded.quality ELSE image_stage1.quality END",
                    params![
                        content_id.as_i64(),
                        fields.width,
                        fields.height,
                        fields.pdq.map(|hash| hash.as_bytes().to_vec()),
                        fields.quality.filter(|quality| *quality <= 100)
                    ],
                )?;
                let merged = read_image_stage1_fields(&transaction, content_id)?;
                ("image_stage1", encode_image_stage1(key, merged))
            }
            FeatureWrite::ImageStage2(features) => {
                ensure_finite(&features.sobel)?;
                let phash = phash_parts_to_blob(&features.phash_parts);
                let sobel = encode_sobel(&features.sobel);
                transaction.execute(
                    "INSERT INTO image_stage2(content_id,phash_parts,sobel) VALUES(?1,?2,?3)
                     ON CONFLICT(content_id) DO UPDATE SET
                       phash_parts=excluded.phash_parts,sobel=excluded.sobel",
                    params![content_id.as_i64(), phash.as_slice(), sobel.as_slice()],
                )?;
                ("image_stage2", encode_image_stage2(key, &features))
            }
            FeatureWrite::VideoMetadata(fields) => {
                transaction.execute(
                    "INSERT INTO video_metadata(content_id,duration_ms,width,height)
                     VALUES(?1,?2,?3,?4)
                     ON CONFLICT(content_id) DO UPDATE SET
                       duration_ms=CASE WHEN excluded.duration_ms > 0 THEN excluded.duration_ms ELSE video_metadata.duration_ms END,
                       width=CASE WHEN excluded.width > 0 THEN excluded.width ELSE video_metadata.width END,
                       height=CASE WHEN excluded.height > 0 THEN excluded.height ELSE video_metadata.height END",
                    params![
                        content_id.as_i64(),
                        fields.duration_ms.map(sqlite_integer).transpose()?,
                        fields.width,
                        fields.height
                    ],
                )?;
                let merged = read_video_metadata_fields(&transaction, content_id)?;
                ("video_metadata", encode_video_metadata(key, merged))
            }
            FeatureWrite::VideoFrameStage1(frame) => {
                validate_slot(frame.slot)?;
                transaction.execute(
                    "INSERT INTO video_frame_stage1(
                       content_id,slot,time_ms,decoded,width,height,pdq,quality)
                     VALUES(?1,?2,?3,?4,?5,?6,?7,?8)
                     ON CONFLICT(content_id,slot) DO UPDATE SET
                       time_ms=COALESCE(excluded.time_ms,video_frame_stage1.time_ms),
                       decoded=CASE WHEN excluded.decoded=1 THEN 1 ELSE video_frame_stage1.decoded END,
                       width=CASE WHEN excluded.width > 0 THEN excluded.width ELSE video_frame_stage1.width END,
                       height=CASE WHEN excluded.height > 0 THEN excluded.height ELSE video_frame_stage1.height END,
                       pdq=COALESCE(excluded.pdq,video_frame_stage1.pdq),
                       quality=CASE WHEN excluded.quality BETWEEN 0 AND 100
                                    THEN excluded.quality ELSE video_frame_stage1.quality END",
                    params![
                        content_id.as_i64(),
                        frame.slot,
                        sqlite_integer(frame.time_ms)?,
                        frame.decoded,
                        frame.width,
                        frame.height,
                        frame.pdq.map(|hash| hash.as_bytes().to_vec()),
                        frame.quality.filter(|quality| *quality <= 100)
                    ],
                )?;
                let merged = read_video_frame_stage1_fields(&transaction, content_id, frame.slot)?;
                ("video_frame_stage1", encode_video_frame_stage1(key, merged))
            }
            FeatureWrite::VideoFrameStage2(frame) => {
                validate_slot(frame.slot)?;
                ensure_finite(&frame.features.sobel)?;
                let phash = phash_parts_to_blob(&frame.features.phash_parts);
                let sobel = encode_sobel(&frame.features.sobel);
                transaction.execute(
                    "INSERT INTO video_frame_stage2(content_id,slot,phash_parts,sobel)
                     VALUES(?1,?2,?3,?4)
                     ON CONFLICT(content_id,slot) DO UPDATE SET
                       phash_parts=excluded.phash_parts,sobel=excluded.sobel",
                    params![
                        content_id.as_i64(),
                        frame.slot,
                        phash.as_slice(),
                        sobel.as_slice()
                    ],
                )?;
                ("video_frame_stage2", encode_video_frame_stage2(key, frame))
            }
            FeatureWrite::ContactSheet(relative_path) => {
                transaction.execute(
                    "INSERT INTO contact_sheets(content_id,relative_path) VALUES(?1,?2)
                     ON CONFLICT(content_id) DO UPDATE SET relative_path=excluded.relative_path",
                    params![content_id.as_i64(), &relative_path],
                )?;
                contact_sheet_payload(key, &relative_path)
            }
        };
        if let Some(task_item_id) = task_item_id {
            transaction.execute(
                "UPDATE task_items SET status='succeeded',error=NULL WHERE item_id=?1",
                [task_item_id],
            )?;
        }
        let sequence = append_sync_change(&transaction, entity_kind, payload)?;
        transaction.commit()?;
        Ok(sequence)
    }

    /// 仅当扫描项仍为 running 且任务未取消时，在一个事务提交全部一筛、outbox 与项成功。
    ///
    /// 返回 `false` 表示取消或晚到结果被忽略；此时内容类型、特征和联系表引用均不改变。
    pub fn commit_scan_stage1_if_running(
        &mut self,
        item_id: &str,
        content_id: ContentId,
        media_kind: MediaKind,
        writes: Vec<FeatureWrite>,
        now_ms: i64,
    ) -> Result<bool, StoreError> {
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let (item_status, task_status): (String, String) = transaction.query_row(
            "SELECT ti.status,t.status
             FROM task_items ti JOIN tasks t ON t.task_id=ti.task_id
             WHERE ti.item_id=?1",
            [item_id],
            |row| Ok((row.get(0)?, row.get(1)?)),
        )?;
        if item_status != "running" || !matches!(task_status.as_str(), "queued" | "running") {
            transaction.commit()?;
            return Ok(false);
        }
        commit_scan_stage1_in_transaction(
            &transaction,
            item_id,
            content_id,
            media_kind,
            writes,
            now_ms,
        )?;
        transaction.commit()?;
        Ok(true)
    }

    /// 仅在 task/item/content 身份匹配且仍活动时原子提交全部一筛、outbox 和项成功。
    pub fn commit_scan_stage1_guarded(
        &mut self,
        identity: &TaskItemIdentity,
        media_kind: MediaKind,
        writes: Vec<FeatureWrite>,
        now_ms: i64,
    ) -> Result<TaskItemApplyResult, StoreError> {
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let state = classify_task_item_identity(&transaction, identity)?;
        let result = match state {
            TaskItemIdentityState::Active => {
                let Some(content_id) = identity.content_id else {
                    return Err(StoreError::InvalidState(
                        "一筛提交身份必须携带 content_id".into(),
                    ));
                };
                TaskItemApplyResult::Applied(commit_scan_stage1_in_transaction(
                    &transaction,
                    &identity.item_id,
                    content_id,
                    media_kind,
                    writes,
                    now_ms,
                )?)
            }
            TaskItemIdentityState::Inactive => TaskItemApplyResult::IgnoredInactive,
            TaskItemIdentityState::Mismatch => TaskItemApplyResult::IdentityMismatch,
        };
        transaction.commit()?;
        Ok(result)
    }

    /// 在不创建任务表记录的情况下原子提交一筛、内容状态和同步 outbox。
    ///
    /// 该入口供瞬时 TSV 任务使用；任务状态只由内存运行时维护，SQLite 只保存最终缓存。
    /// 返回本次事务最后写入的 outbox 序号。
    pub fn commit_scan_stage1_taskless(
        &mut self,
        content_id: ContentId,
        media_kind: MediaKind,
        writes: Vec<FeatureWrite>,
    ) -> Result<u64, StoreError> {
        let transaction = self
            .connection
            .transaction_with_behavior(TransactionBehavior::Immediate)?;
        let sequence = commit_scan_stage1_features_in_transaction(
            &transaction,
            content_id,
            media_kind,
            writes,
        )?;
        transaction.commit()?;
        Ok(sequence)
    }

    /// 只返回图片完整四字段，或六槽位记录且至少四个成功帧字段完整的视频一筛。
    pub fn load_complete_stage1(
        &self,
        content_id: ContentId,
    ) -> Result<Option<CompleteStage1>, StoreError> {
        match self.content_media_kind(content_id)? {
            MediaKind::Image => self
                .load_image_stage1(content_id)
                .map(|value| value.map(CompleteStage1::Image)),
            MediaKind::Video => self.load_video_stage1(content_id),
            MediaKind::Other => Ok(None),
        }
    }

    /// 只返回 pHash 与有限 Sobel 同时存在的图片或视频槽位联合二筛。
    pub fn load_complete_stage2(
        &self,
        content_id: ContentId,
    ) -> Result<Option<CompleteStage2>, StoreError> {
        self.load_complete_stage2_from_cache(content_id)
    }

    /// 把本机已经完整保存的联合二筛特征重新写入 outbox，供中心缓存缺口按需补同步。
    ///
    /// 返回 `false` 表示本机缓存也不完整，调用方随后才需要启动 Worker；已有结果不会
    /// 重新解码媒体。视频会重新发布每个一筛成功槽位，保持中心完整性判定不变。
    pub fn republish_complete_stage2(&mut self, content_id: ContentId) -> Result<bool, StoreError> {
        let cached = self.load_base_cache_record(content_id)?;
        self.republish_complete_stage2_from_cache(&cached)
    }

    /// 直接使用批量查询得到的二筛原始结构重发 outbox，不再逐内容读取 SQLite。
    pub fn republish_complete_stage2_from_cache(
        &mut self,
        cached: &BaseCacheRecord,
    ) -> Result<bool, StoreError> {
        let Some(content_id) = cached.content_id else {
            return Ok(false);
        };
        if !classify_cache_completeness(cached, true).is_complete() {
            return Ok(false);
        }
        match cached.media_kind {
            MediaKind::Image => {
                let Some(feature) = cached.image_stage2 else {
                    return Ok(false);
                };
                self.commit_feature_result(content_id, None, FeatureWrite::ImageStage2(feature))?;
            }
            MediaKind::Video => {
                let Some(CompleteStage1::Video(stage1)) = cached.stage1.as_ref() else {
                    return Ok(false);
                };
                let slots = stage1
                    .iter()
                    .enumerate()
                    .filter_map(|(slot, feature)| feature.as_ref().map(|_| slot as u8))
                    .collect::<Vec<_>>();
                return self.republish_stage2_slots_from_cache(cached, &slots);
            }
            MediaKind::Other => return Ok(false),
        }
        Ok(true)
    }

    /// 只重发指定的完整视频槽位，供中心只请求本机已有槽位时闭合缓存同步。
    pub fn republish_stage2_slots_from_cache(
        &mut self,
        cached: &BaseCacheRecord,
        slots: &[u8],
    ) -> Result<bool, StoreError> {
        let Some(content_id) = cached.content_id else {
            return Ok(false);
        };
        if slots.is_empty() {
            return Ok(false);
        }
        let completeness = classify_cache_completeness(cached, true);
        if completeness.base_missing_parts != 0 || cached.media_kind != MediaKind::Video {
            return Ok(false);
        }
        let Some(CompleteStage1::Video(stage1)) = cached.stage1.as_ref() else {
            return Ok(false);
        };
        let mut selected = [false; 6];
        for &slot in slots {
            validate_slot(slot)?;
            let index = usize::from(slot);
            if selected[index] {
                continue;
            }
            selected[index] = true;
            if stage1[index].is_none()
                || completeness.video_stage2_missing_slots & (1_u8 << index) != 0
            {
                return Ok(false);
            }
        }
        for (index, selected) in selected.into_iter().enumerate() {
            if selected {
                let Some(features) = cached.video_stage2[index] else {
                    return Ok(false);
                };
                self.commit_feature_result(
                    content_id,
                    None,
                    FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                        slot: index as u8,
                        features,
                    }),
                )?;
            }
        }
        Ok(true)
    }

    fn load_image_stage1(&self, content_id: ContentId) -> Result<Option<ImageStage1>, StoreError> {
        let row: Option<(Option<i64>, Option<i64>, Option<Vec<u8>>, Option<i64>)> = self
            .connection
            .query_row(
                "SELECT width,height,pdq,quality FROM image_stage1
                 WHERE content_id=?1",
                [content_id.as_i64()],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
            )
            .optional()?;
        Ok(row.and_then(|(width, height, pdq, quality)| {
            decode_stage1_fields(width, height, pdq, quality)
        }))
    }

    fn load_video_stage1(
        &self,
        content_id: ContentId,
    ) -> Result<Option<CompleteStage1>, StoreError> {
        type FrameRow = (
            Option<i64>,
            Option<i64>,
            Option<i64>,
            Option<i64>,
            Option<Vec<u8>>,
            Option<i64>,
        );
        let mut statement = self.connection.prepare_cached(
            "SELECT slot,decoded,width,height,pdq,quality FROM video_frame_stage1
             WHERE content_id=?1 ORDER BY slot",
        )?;
        let rows = statement
            .query_map([content_id.as_i64()], |row| {
                Ok(FrameRow::from((
                    row.get(0)?,
                    row.get(1)?,
                    row.get(2)?,
                    row.get(3)?,
                    row.get(4)?,
                    row.get(5)?,
                )))
            })?
            .collect::<Result<Vec<FrameRow>, _>>()?;
        if rows.len() != 6
            || rows
                .iter()
                .enumerate()
                .any(|(slot, row)| row.0 != Some(slot as i64))
        {
            return Ok(None);
        }
        let mut frames = [None; 6];
        for (slot, decoded, width, height, pdq, quality) in rows {
            let (Some(slot), Some(decoded)) = (slot, decoded) else {
                return Ok(None);
            };
            let Ok(slot) = usize::try_from(slot) else {
                return Ok(None);
            };
            match decoded {
                0 => continue,
                1 => {}
                _ => return Ok(None),
            }
            let Some(feature) = decode_stage1_fields(width, height, pdq, quality) else {
                return Ok(None);
            };
            frames[slot] = Some(feature);
        }
        if frames.iter().flatten().count() < 4 {
            return Ok(None);
        }
        Ok(Some(CompleteStage1::Video(Box::new(frames))))
    }

    /// 读取图片二筛的逐字段原始缓存；任一长度或浮点非法时只返回缺失。
    pub(crate) fn load_image_stage2_for_cache(
        &self,
        content_id: ContentId,
    ) -> Result<Option<ImageStage2>, StoreError> {
        let row: Option<(Option<Vec<u8>>, Option<Vec<u8>>)> = self
            .connection
            .query_row(
                "SELECT phash_parts,sobel FROM image_stage2 WHERE content_id=?1",
                [content_id.as_i64()],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .optional()?;
        Ok(row
            .and_then(|(phash, sobel)| decode_stage2_if_valid(phash.as_deref(), sobel.as_deref())))
    }

    /// 读取视频六个二筛槽位，缺失或非法槽位保留为 `None`。
    pub(crate) fn load_video_stage2_for_cache(
        &self,
        content_id: ContentId,
    ) -> Result<[Option<ImageStage2>; 6], StoreError> {
        let mut output = [None; 6];
        let mut statement = self.connection.prepare_cached(
            "SELECT slot,phash_parts,sobel FROM video_frame_stage2
             WHERE content_id=?1 ORDER BY slot",
        )?;
        let rows = statement.query_map([content_id.as_i64()], |row| {
            Ok((
                row.get::<_, Option<i64>>(0)?,
                row.get::<_, Option<Vec<u8>>>(1)?,
                row.get::<_, Option<Vec<u8>>>(2)?,
            ))
        })?;
        for row in rows {
            let (Some(slot), phash, sobel) = row? else {
                continue;
            };
            let Ok(slot) = usize::try_from(slot) else {
                continue;
            };
            if slot >= output.len() {
                continue;
            }
            output[slot] = decode_stage2_if_valid(phash.as_deref(), sobel.as_deref());
        }
        Ok(output)
    }

    /// 只返回 pHash 与有限 Sobel 同时存在的图片或视频槽位联合二筛。
    pub(crate) fn load_complete_stage2_from_cache(
        &self,
        content_id: ContentId,
    ) -> Result<Option<CompleteStage2>, StoreError> {
        match self.content_media_kind(content_id)? {
            MediaKind::Image => self
                .load_image_stage2_for_cache(content_id)
                .map(|value| value.map(|value| CompleteStage2::Image(Box::new(value)))),
            MediaKind::Video => {
                let Some(CompleteStage1::Video(stage1)) = self.load_complete_stage1(content_id)?
                else {
                    return Ok(None);
                };
                let frames = self.load_video_stage2_for_cache(content_id)?;
                if stage1
                    .iter()
                    .enumerate()
                    .filter_map(|(slot, feature)| feature.as_ref().map(|_| slot))
                    .all(|slot| frames[slot].is_some())
                {
                    Ok(Some(CompleteStage2::Video(Box::new(frames))))
                } else {
                    Ok(None)
                }
            }
            MediaKind::Other => Ok(None),
        }
    }
}

/// 在既有事务中按固定顺序写入内容、一筛、outbox 和任务项终态。
fn commit_scan_stage1_in_transaction(
    transaction: &Transaction<'_>,
    item_id: &str,
    content_id: ContentId,
    media_kind: MediaKind,
    writes: Vec<FeatureWrite>,
    now_ms: i64,
) -> Result<crate::TaskEvent, StoreError> {
    commit_scan_stage1_features_in_transaction(transaction, content_id, media_kind, writes)?;
    complete_item_in_transaction(
        transaction,
        item_id,
        TaskItemCompletion::Succeeded {
            content_id: Some(content_id),
        },
        now_ms,
    )
}

/// 在既有事务中按固定顺序写入内容、一筛和 outbox，不触碰任务表。
fn commit_scan_stage1_features_in_transaction(
    transaction: &Transaction<'_>,
    content_id: ContentId,
    media_kind: MediaKind,
    writes: Vec<FeatureWrite>,
) -> Result<u64, StoreError> {
    transaction.execute(
        "UPDATE contents SET media_kind=?2,base_complete=1 WHERE content_id=?1",
        params![
            content_id.as_i64(),
            match media_kind {
                MediaKind::Image => "image",
                MediaKind::Video => "video",
                MediaKind::Other => "other",
            }
        ],
    )?;
    let key = content_key_in_transaction(transaction, content_id)?;
    let mut sequence = append_sync_change(
        transaction,
        "content",
        encode_content(key, media_kind, true),
    )?;
    for write in writes {
        let (entity_kind, payload) = match write {
            FeatureWrite::ImageStage1(fields) => {
                transaction.execute(
                    "INSERT INTO image_stage1(content_id,width,height,pdq,quality)
                     VALUES(?1,?2,?3,?4,?5)
                     ON CONFLICT(content_id) DO UPDATE SET
                       width=CASE WHEN excluded.width > 0 THEN excluded.width ELSE image_stage1.width END,
                       height=CASE WHEN excluded.height > 0 THEN excluded.height ELSE image_stage1.height END,
                       pdq=COALESCE(excluded.pdq,image_stage1.pdq),
                       quality=CASE WHEN excluded.quality BETWEEN 0 AND 100
                                    THEN excluded.quality ELSE image_stage1.quality END",
                    params![
                        content_id.as_i64(),
                        fields.width,
                        fields.height,
                        fields.pdq.map(|hash| hash.as_bytes().to_vec()),
                        fields.quality.filter(|quality| *quality <= 100)
                    ],
                )?;
                let merged = read_image_stage1_fields(transaction, content_id)?;
                ("image_stage1", encode_image_stage1(key, merged))
            }
            FeatureWrite::VideoMetadata(fields) => {
                transaction.execute(
                    "INSERT INTO video_metadata(content_id,duration_ms,width,height)
                     VALUES(?1,?2,?3,?4)
                     ON CONFLICT(content_id) DO UPDATE SET
                       duration_ms=CASE WHEN excluded.duration_ms > 0 THEN excluded.duration_ms ELSE video_metadata.duration_ms END,
                       width=CASE WHEN excluded.width > 0 THEN excluded.width ELSE video_metadata.width END,
                       height=CASE WHEN excluded.height > 0 THEN excluded.height ELSE video_metadata.height END",
                    params![
                        content_id.as_i64(),
                        fields.duration_ms.map(sqlite_integer).transpose()?,
                        fields.width,
                        fields.height
                    ],
                )?;
                let merged = read_video_metadata_fields(transaction, content_id)?;
                ("video_metadata", encode_video_metadata(key, merged))
            }
            FeatureWrite::VideoFrameStage1(frame) => {
                validate_slot(frame.slot)?;
                transaction.execute(
                    "INSERT INTO video_frame_stage1(
                       content_id,slot,time_ms,decoded,width,height,pdq,quality)
                     VALUES(?1,?2,?3,?4,?5,?6,?7,?8)
                     ON CONFLICT(content_id,slot) DO UPDATE SET
                       time_ms=COALESCE(excluded.time_ms,video_frame_stage1.time_ms),
                       decoded=CASE WHEN excluded.decoded=1 THEN 1 ELSE video_frame_stage1.decoded END,
                       width=CASE WHEN excluded.width > 0 THEN excluded.width ELSE video_frame_stage1.width END,
                       height=CASE WHEN excluded.height > 0 THEN excluded.height ELSE video_frame_stage1.height END,
                       pdq=COALESCE(excluded.pdq,video_frame_stage1.pdq),
                       quality=CASE WHEN excluded.quality BETWEEN 0 AND 100
                                    THEN excluded.quality ELSE video_frame_stage1.quality END",
                    params![
                        content_id.as_i64(),
                        frame.slot,
                        sqlite_integer(frame.time_ms)?,
                        frame.decoded,
                        frame.width,
                        frame.height,
                        frame.pdq.map(|hash| hash.as_bytes().to_vec()),
                        frame.quality.filter(|quality| *quality <= 100)
                    ],
                )?;
                let merged = read_video_frame_stage1_fields(transaction, content_id, frame.slot)?;
                ("video_frame_stage1", encode_video_frame_stage1(key, merged))
            }
            FeatureWrite::ContactSheet(relative_path) => {
                transaction.execute(
                    "INSERT INTO contact_sheets(content_id,relative_path) VALUES(?1,?2)
                     ON CONFLICT(content_id) DO UPDATE SET relative_path=excluded.relative_path",
                    params![content_id.as_i64(), &relative_path],
                )?;
                contact_sheet_payload(key, &relative_path)
            }
            FeatureWrite::ImageStage2(_) | FeatureWrite::VideoFrameStage2(_) => {
                return Err(StoreError::InvalidFeature("扫描一筛事务不能写入二筛结果"));
            }
        };
        sequence = append_sync_change(transaction, entity_kind, payload)?;
    }
    Ok(sequence)
}

fn validate_slot(slot: u8) -> Result<(), StoreError> {
    if slot > 5 {
        return Err(StoreError::InvalidFeature("视频槽位必须位于 0..=5"));
    }
    Ok(())
}

/// 从当前事务读取图片一筛的合并后字段，保证 outbox 与 SQLite 同值。
fn read_image_stage1_fields(
    transaction: &Transaction<'_>,
    content_id: ContentId,
) -> Result<ImageStage1Fields, StoreError> {
    let row: (Option<i64>, Option<i64>, Option<Vec<u8>>, Option<i64>) = transaction.query_row(
        "SELECT width,height,pdq,quality FROM image_stage1 WHERE content_id=?1",
        [content_id.as_i64()],
        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
    )?;
    Ok(ImageStage1Fields {
        width: row.0.and_then(|value| u32::try_from(value).ok()),
        height: row.1.and_then(|value| u32::try_from(value).ok()),
        pdq: decode_pdq(row.2),
        quality: row.3.and_then(|value| u8::try_from(value).ok()),
    })
}

/// 从当前事务读取视频元数据的合并后字段，保留既有有效列。
fn read_video_metadata_fields(
    transaction: &Transaction<'_>,
    content_id: ContentId,
) -> Result<VideoMetadataFields, StoreError> {
    let row: (Option<i64>, Option<i64>, Option<i64>) = transaction.query_row(
        "SELECT duration_ms,width,height FROM video_metadata WHERE content_id=?1",
        [content_id.as_i64()],
        |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
    )?;
    Ok(VideoMetadataFields {
        duration_ms: row.0.and_then(|value| u64::try_from(value).ok()),
        width: row.1.and_then(|value| u32::try_from(value).ok()),
        height: row.2.and_then(|value| u32::try_from(value).ok()),
    })
}

/// 从当前事务读取视频槽位的一筛合并后字段，保持失败槽位的旧时间和状态。
fn read_video_frame_stage1_fields(
    transaction: &Transaction<'_>,
    content_id: ContentId,
    slot: u8,
) -> Result<VideoFrameStage1Fields, StoreError> {
    let row: (
        Option<i64>,
        Option<i64>,
        Option<i64>,
        Option<i64>,
        Option<Vec<u8>>,
        Option<i64>,
    ) = transaction.query_row(
        "SELECT time_ms,decoded,width,height,pdq,quality
         FROM video_frame_stage1 WHERE content_id=?1 AND slot=?2",
        params![content_id.as_i64(), slot],
        |row| {
            Ok((
                row.get(0)?,
                row.get(1)?,
                row.get(2)?,
                row.get(3)?,
                row.get(4)?,
                row.get(5)?,
            ))
        },
    )?;
    Ok(VideoFrameStage1Fields {
        slot,
        time_ms: row
            .0
            .and_then(|value| u64::try_from(value).ok())
            .unwrap_or_default(),
        decoded: row.1 == Some(1),
        width: row.2.and_then(|value| u32::try_from(value).ok()),
        height: row.3.and_then(|value| u32::try_from(value).ok()),
        pdq: decode_pdq(row.4),
        quality: row.5.and_then(|value| u8::try_from(value).ok()),
    })
}

/// 仅将固定长度的 SQLite PDQ 字节转换为领域哈希。
fn decode_pdq(bytes: Option<Vec<u8>>) -> Option<PdqHash> {
    bytes
        .and_then(|value| <[u8; 32]>::try_from(value).ok())
        .map(PdqHash::from_bytes)
}

fn ensure_finite(sobel: &[f32; 128]) -> Result<(), StoreError> {
    if sobel.iter().any(|value| !value.is_finite()) {
        return Err(StoreError::NonFiniteSobel);
    }
    Ok(())
}

/// 从 SQLite 原始列解码一筛字段；非法尺寸、长度或 Quality 只返回结构缺失。
pub(crate) fn decode_stage1_fields(
    width: Option<i64>,
    height: Option<i64>,
    pdq: Option<Vec<u8>>,
    quality: Option<i64>,
) -> Option<ImageStage1> {
    let width = u32::try_from(width?).ok()?;
    let height = u32::try_from(height?).ok()?;
    let quality = u8::try_from(quality?).ok()?;
    if width == 0 || height == 0 || quality > 100 {
        return None;
    }
    Some(ImageStage1 {
        width,
        height,
        pdq: PdqHash::from_bytes(pdq?.try_into().ok()?),
        quality,
    })
}

fn encode_sobel(sobel: &[f32; 128]) -> [u8; 512] {
    let mut bytes = [0_u8; 512];
    for (index, value) in sobel.iter().enumerate() {
        bytes[index * 4..index * 4 + 4].copy_from_slice(&value.to_le_bytes());
    }
    bytes
}

/// 从 SQLite 原始列解码二筛字段；长度错误或非有限 Sobel 只返回结构缺失。
pub(crate) fn decode_stage2_if_valid(
    phash: Option<&[u8]>,
    sobel: Option<&[u8]>,
) -> Option<ImageStage2> {
    let phash: [u8; 72] = phash?.try_into().ok()?;
    let mut phash_parts = [0_u64; 9];
    for (index, part) in phash.chunks_exact(8).enumerate() {
        phash_parts[index] = u64::from_le_bytes(part.try_into().expect("固定八字节分块"));
    }
    let sobel: [u8; 512] = sobel?.try_into().ok()?;
    let mut histogram = [0.0_f32; 128];
    for (index, value) in sobel.chunks_exact(4).enumerate() {
        histogram[index] = f32::from_le_bytes(value.try_into().expect("固定四字节浮点"));
    }
    histogram
        .iter()
        .all(|value| value.is_finite())
        .then_some(ImageStage2 {
            phash_parts,
            sobel: histogram,
        })
}

fn encode_key(encoder: RowEncoder, key: dedup_core::ContentKey) -> RowEncoder {
    encoder.bytes(&key.md5()).u64(key.file_size())
}

pub(crate) fn encode_image_stage1(
    key: dedup_core::ContentKey,
    fields: ImageStage1Fields,
) -> Vec<u8> {
    encode_key(RowEncoder::new(1), key)
        .optional_u32(fields.width)
        .optional_u32(fields.height)
        .optional_bytes(fields.pdq.as_ref().map(|hash| hash.as_bytes().as_slice()))
        .optional_u8(fields.quality)
        .finish()
}

pub(crate) fn encode_image_stage2(key: dedup_core::ContentKey, features: &ImageStage2) -> Vec<u8> {
    let phash = phash_parts_to_blob(&features.phash_parts);
    let sobel = encode_sobel(&features.sobel);
    encode_key(RowEncoder::new(1), key)
        .bytes(&phash)
        .bytes(&sobel)
        .finish()
}

pub(crate) fn encode_video_metadata(
    key: dedup_core::ContentKey,
    fields: VideoMetadataFields,
) -> Vec<u8> {
    let mut encoder = encode_key(RowEncoder::new(1), key);
    encoder = match fields.duration_ms {
        Some(value) => encoder.u8(1).u64(value),
        None => encoder.u8(0),
    };
    encoder
        .optional_u32(fields.width)
        .optional_u32(fields.height)
        .finish()
}

pub(crate) fn encode_video_frame_stage1(
    key: dedup_core::ContentKey,
    frame: VideoFrameStage1Fields,
) -> Vec<u8> {
    encode_key(RowEncoder::new(1), key)
        .u8(frame.slot)
        .u64(frame.time_ms)
        .u8(u8::from(frame.decoded))
        .optional_u32(frame.width)
        .optional_u32(frame.height)
        .optional_bytes(frame.pdq.as_ref().map(|hash| hash.as_bytes().as_slice()))
        .optional_u8(frame.quality)
        .finish()
}

pub(crate) fn encode_video_frame_stage2(
    key: dedup_core::ContentKey,
    frame: VideoFrameStage2Fields,
) -> Vec<u8> {
    let phash = phash_parts_to_blob(&frame.features.phash_parts);
    let sobel = encode_sobel(&frame.features.sobel);
    encode_key(RowEncoder::new(1), key)
        .u8(frame.slot)
        .bytes(&phash)
        .bytes(&sobel)
        .finish()
}

fn contact_sheet_payload(
    key: dedup_core::ContentKey,
    relative_path: &str,
) -> (&'static str, Vec<u8>) {
    (
        "contact_sheet",
        encode_key(RowEncoder::new(1), key)
            .text(relative_path)
            .finish(),
    )
}

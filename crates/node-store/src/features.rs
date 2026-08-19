//! 图片/视频一筛与联合二筛的事务写入和完整结果加载。

use dedup_core::MediaKind;
use dedup_media::{ImageStage1, ImageStage2, PdqHash, phash_parts_to_blob};
use rusqlite::{OptionalExtension, params};

use crate::{
    CompleteStage1, CompleteStage2, ContentId, FeatureWrite, ImageStage1Fields, NodeStore,
    StoreError, VideoFrameStage1Fields, VideoFrameStage2Fields, VideoMetadataFields,
    content::content_key_in_transaction,
    open::{fixed_bytes, sqlite_integer},
    outbox::append_sync_change,
    rows::RowEncoder,
};

impl NodeStore {
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
                       width=excluded.width,height=excluded.height,
                       pdq=excluded.pdq,quality=excluded.quality",
                    params![
                        content_id.as_i64(),
                        fields.width,
                        fields.height,
                        fields.pdq.map(|hash| hash.as_bytes().to_vec()),
                        fields.quality
                    ],
                )?;
                ("image_stage1", encode_image_stage1(key, fields))
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
                       duration_ms=excluded.duration_ms,width=excluded.width,height=excluded.height",
                    params![
                        content_id.as_i64(),
                        fields.duration_ms.map(sqlite_integer).transpose()?,
                        fields.width,
                        fields.height
                    ],
                )?;
                ("video_metadata", encode_video_metadata(key, fields))
            }
            FeatureWrite::VideoFrameStage1(frame) => {
                validate_slot(frame.slot)?;
                transaction.execute(
                    "INSERT INTO video_frame_stage1(
                       content_id,slot,time_ms,decoded,width,height,pdq,quality)
                     VALUES(?1,?2,?3,?4,?5,?6,?7,?8)
                     ON CONFLICT(content_id,slot) DO UPDATE SET
                       time_ms=excluded.time_ms,decoded=excluded.decoded,
                       width=excluded.width,height=excluded.height,
                       pdq=excluded.pdq,quality=excluded.quality",
                    params![
                        content_id.as_i64(),
                        frame.slot,
                        sqlite_integer(frame.time_ms)?,
                        frame.decoded,
                        frame.width,
                        frame.height,
                        frame.pdq.map(|hash| hash.as_bytes().to_vec()),
                        frame.quality
                    ],
                )?;
                ("video_frame_stage1", encode_video_frame_stage1(key, frame))
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
        match self.content_media_kind(content_id)? {
            MediaKind::Image => self
                .load_image_stage2(content_id)
                .map(|value| value.map(Box::new).map(CompleteStage2::Image)),
            MediaKind::Video => self.load_video_stage2(content_id),
            MediaKind::Other => Ok(None),
        }
    }

    /// 把本机已经完整保存的联合二筛特征重新写入 outbox，供中心缓存缺口按需补同步。
    ///
    /// 返回 `false` 表示本机缓存也不完整，调用方随后才需要启动 Worker；已有结果不会
    /// 重新解码媒体。视频会重新发布每个一筛成功槽位，保持中心完整性判定不变。
    pub fn republish_complete_stage2(&mut self, content_id: ContentId) -> Result<bool, StoreError> {
        let Some(features) = self.load_complete_stage2(content_id)? else {
            return Ok(false);
        };
        match features {
            CompleteStage2::Image(feature) => {
                self.commit_feature_result(content_id, None, FeatureWrite::ImageStage2(*feature))?;
            }
            CompleteStage2::Video(frames) => {
                for (slot, feature) in frames.iter().enumerate() {
                    if let Some(feature) = feature {
                        self.commit_feature_result(
                            content_id,
                            None,
                            FeatureWrite::VideoFrameStage2(VideoFrameStage2Fields {
                                slot: slot as u8,
                                features: *feature,
                            }),
                        )?;
                    }
                }
            }
        }
        Ok(true)
    }

    fn load_image_stage1(&self, content_id: ContentId) -> Result<Option<ImageStage1>, StoreError> {
        let row: Option<(u32, u32, Vec<u8>, u8)> = self
            .connection
            .query_row(
                "SELECT width,height,pdq,quality FROM image_stage1
                 WHERE content_id=?1 AND width IS NOT NULL AND height IS NOT NULL
                   AND pdq IS NOT NULL AND quality IS NOT NULL",
                [content_id.as_i64()],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
            )
            .optional()?;
        row.map(|(width, height, pdq, quality)| {
            Ok(ImageStage1 {
                width,
                height,
                pdq: PdqHash::from_bytes(fixed_bytes(pdq, "image_stage1.pdq")?),
                quality,
            })
        })
        .transpose()
    }

    fn load_video_stage1(
        &self,
        content_id: ContentId,
    ) -> Result<Option<CompleteStage1>, StoreError> {
        type FrameRow = (
            u8,
            bool,
            Option<u32>,
            Option<u32>,
            Option<Vec<u8>>,
            Option<u8>,
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
                .any(|(slot, row)| row.0 as usize != slot)
        {
            return Ok(None);
        }
        let mut frames = [None; 6];
        for (slot, decoded, width, height, pdq, quality) in rows {
            if !decoded {
                continue;
            }
            let (Some(width), Some(height), Some(pdq), Some(quality)) =
                (width, height, pdq, quality)
            else {
                return Ok(None);
            };
            frames[slot as usize] = Some(ImageStage1 {
                width,
                height,
                pdq: PdqHash::from_bytes(fixed_bytes(pdq, "video_frame_stage1.pdq")?),
                quality,
            });
        }
        if frames.iter().flatten().count() < 4 {
            return Ok(None);
        }
        Ok(Some(CompleteStage1::Video(Box::new(frames))))
    }

    fn load_image_stage2(&self, content_id: ContentId) -> Result<Option<ImageStage2>, StoreError> {
        let row: Option<(Vec<u8>, Vec<u8>)> = self
            .connection
            .query_row(
                "SELECT phash_parts,sobel FROM image_stage2
                 WHERE content_id=?1 AND phash_parts IS NOT NULL AND sobel IS NOT NULL",
                [content_id.as_i64()],
                |row| Ok((row.get(0)?, row.get(1)?)),
            )
            .optional()?;
        row.map(|(phash, sobel)| decode_stage2(phash, sobel))
            .transpose()
    }

    fn load_video_stage2(
        &self,
        content_id: ContentId,
    ) -> Result<Option<CompleteStage2>, StoreError> {
        let Some(CompleteStage1::Video(stage1)) = self.load_complete_stage1(content_id)? else {
            return Ok(None);
        };
        let mut frames = [None; 6];
        let mut statement = self.connection.prepare_cached(
            "SELECT phash_parts,sobel FROM video_frame_stage2
             WHERE content_id=?1 AND slot=?2
               AND phash_parts IS NOT NULL AND sobel IS NOT NULL",
        )?;
        for slot in 0..6 {
            if stage1[slot].is_none() {
                continue;
            }
            let row: Option<(Vec<u8>, Vec<u8>)> = statement
                .query_row(params![content_id.as_i64(), slot as i64], |row| {
                    Ok((row.get(0)?, row.get(1)?))
                })
                .optional()?;
            let Some((phash, sobel)) = row else {
                return Ok(None);
            };
            frames[slot] = Some(decode_stage2(phash, sobel)?);
        }
        Ok(Some(CompleteStage2::Video(Box::new(frames))))
    }
}

fn validate_slot(slot: u8) -> Result<(), StoreError> {
    if slot > 5 {
        return Err(StoreError::InvalidFeature("视频槽位必须位于 0..=5"));
    }
    Ok(())
}

fn ensure_finite(sobel: &[f32; 128]) -> Result<(), StoreError> {
    if sobel.iter().any(|value| !value.is_finite()) {
        return Err(StoreError::NonFiniteSobel);
    }
    Ok(())
}

fn encode_sobel(sobel: &[f32; 128]) -> [u8; 512] {
    let mut bytes = [0_u8; 512];
    for (index, value) in sobel.iter().enumerate() {
        bytes[index * 4..index * 4 + 4].copy_from_slice(&value.to_le_bytes());
    }
    bytes
}

fn decode_stage2(phash: Vec<u8>, sobel: Vec<u8>) -> Result<ImageStage2, StoreError> {
    let phash = fixed_bytes::<72>(phash, "stage2.phash_parts")?;
    let mut phash_parts = [0_u64; 9];
    for (index, part) in phash.chunks_exact(8).enumerate() {
        phash_parts[index] = u64::from_le_bytes(part.try_into().expect("固定八字节分块"));
    }
    let sobel = fixed_bytes::<512>(sobel, "stage2.sobel")?;
    let mut histogram = [0.0_f32; 128];
    for (index, value) in sobel.chunks_exact(4).enumerate() {
        histogram[index] = f32::from_le_bytes(value.try_into().expect("固定四字节浮点"));
    }
    ensure_finite(&histogram)?;
    Ok(ImageStage2 {
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

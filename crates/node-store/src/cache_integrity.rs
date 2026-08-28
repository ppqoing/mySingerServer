//! 集中验证 SQLite 基础和二筛缓存的结构完整性，并生成稳定缺失掩码。

use dedup_core::MediaKind;
use dedup_media::{ImageStage1, ImageStage2};
use dedup_protocol::{BASE_MISSING_CONTACT_SHEET, BASE_MISSING_PROBE, BASE_MISSING_STAGE1};

use crate::{BaseCacheRecord, CompleteStage1};

/// SQLite 缓存字段通过结构校验后得到的缺失描述。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CacheCompleteness {
    /// 基础计算仍缺少的协议位。
    pub base_missing_parts: u32,
    /// 图片二筛是否缺少任一必需字段。
    pub image_stage2_missing: bool,
    /// 视频二筛缺失槽位的六位掩码。
    pub video_stage2_missing_slots: u8,
}

impl CacheCompleteness {
    /// 返回没有任何缺失字段的结果。
    pub const fn complete() -> Self {
        Self {
            base_missing_parts: 0,
            image_stage2_missing: false,
            video_stage2_missing_slots: 0,
        }
    }

    /// 判断基础计算与二筛是否均已完整。
    pub const fn is_complete(self) -> bool {
        self.base_missing_parts == 0
            && !self.image_stage2_missing
            && self.video_stage2_missing_slots == 0
    }
}

/// 依据结构而不是字段内容数值判断缓存是否完整。
pub fn classify_cache_completeness(
    record: &BaseCacheRecord,
    contact_sheet_valid: bool,
) -> CacheCompleteness {
    let mut base_missing_parts = 0;
    if !record.base_complete || !probe_complete(record) {
        base_missing_parts |= BASE_MISSING_PROBE;
    }
    if !record.base_complete || !stage1_complete(record) {
        base_missing_parts |= BASE_MISSING_STAGE1;
    }
    if record.media_kind == MediaKind::Video && !contact_sheet_valid {
        base_missing_parts |= BASE_MISSING_CONTACT_SHEET;
    }

    let image_stage2_missing = record.media_kind == MediaKind::Image
        && !record.image_stage2.as_ref().is_some_and(valid_stage2);
    let video_stage2_missing_slots = if record.media_kind == MediaKind::Video {
        video_stage2_missing_slots(record)
    } else {
        0
    };

    CacheCompleteness {
        base_missing_parts,
        image_stage2_missing,
        video_stage2_missing_slots,
    }
}

/// 基础探测字段必须是非零尺寸和视频非零时长；Other 仅依赖完成标记。
fn probe_complete(record: &BaseCacheRecord) -> bool {
    match record.media_kind {
        MediaKind::Image => valid_dimension(record.width) && valid_dimension(record.height),
        MediaKind::Video => {
            valid_dimension(record.width)
                && valid_dimension(record.height)
                && record.duration_ms.is_some_and(|value| value > 0)
        }
        MediaKind::Other => true,
    }
}

/// 基础一筛必须满足媒体类型对应的固定字段和最少有效视频帧数。
fn stage1_complete(record: &BaseCacheRecord) -> bool {
    match (record.media_kind, record.stage1.as_ref()) {
        (MediaKind::Image, Some(CompleteStage1::Image(feature))) => valid_stage1(feature),
        (MediaKind::Video, Some(CompleteStage1::Video(frames))) => {
            frames.iter().flatten().all(valid_stage1) && frames.iter().flatten().count() >= 4
        }
        (MediaKind::Other, None) => true,
        _ => false,
    }
}

/// 只接受正整数宽高；数据库中的 NULL、零和越界值均在解码层视为缺失。
fn valid_dimension(value: Option<u32>) -> bool {
    value.is_some_and(|value| value > 0)
}

/// 校验一筛图片的尺寸和 Quality；PDQ 使用固定数组类型天然保证长度。
fn valid_stage1(feature: &ImageStage1) -> bool {
    feature.width > 0 && feature.height > 0 && feature.quality <= 100
}

/// 校验二筛 Sobel 有限；全零 pHash/Sobel 是合法计算结果。
fn valid_stage2(feature: &ImageStage2) -> bool {
    feature.sobel.iter().all(|value| value.is_finite())
}

/// 仅对已经成功解码的一筛槽位要求对应二筛，失败槽位不进入缺失掩码。
fn video_stage2_missing_slots(record: &BaseCacheRecord) -> u8 {
    let Some(CompleteStage1::Video(stage1)) = record.stage1.as_ref() else {
        return 0;
    };
    stage1
        .iter()
        .enumerate()
        .filter_map(|(slot, feature)| {
            feature.as_ref().and_then(|_| {
                (!record.video_stage2[slot].as_ref().is_some_and(valid_stage2))
                    .then_some(1_u8 << slot)
            })
        })
        .fold(0, |mask, bit| mask | bit)
}

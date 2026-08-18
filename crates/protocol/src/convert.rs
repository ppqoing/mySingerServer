//! Protobuf 外部键与共享领域值对象之间的显式转换。

use dedup_core::{ContentKey, LocationKey, MachineId, NormalizedPath, Thresholds};

use crate::{ProtocolError, proto};

/// 单个 `FileChunk.data` 允许的最大字节数。
pub const MAX_FILE_CHUNK_DATA: usize = 1_048_576;

impl From<&ContentKey> for proto::ContentKey {
    fn from(value: &ContentKey) -> Self {
        Self {
            md5: value.md5().to_vec(),
            file_size: value.file_size(),
        }
    }
}

impl TryFrom<proto::ContentKey> for ContentKey {
    type Error = ProtocolError;

    fn try_from(value: proto::ContentKey) -> Result<Self, Self::Error> {
        let actual = value.md5.len();
        let md5 = value
            .md5
            .try_into()
            .map_err(|_| ProtocolError::InvalidMd5Length { actual })?;
        Ok(Self::new(md5, value.file_size))
    }
}

impl From<&LocationKey> for proto::LocationKey {
    fn from(value: &LocationKey) -> Self {
        Self {
            machine_id: value.machine_id().as_str().to_owned(),
            normalized_path: value.normalized_path().as_str().to_owned(),
        }
    }
}

impl TryFrom<proto::LocationKey> for LocationKey {
    type Error = ProtocolError;

    fn try_from(value: proto::LocationKey) -> Result<Self, Self::Error> {
        Ok(Self::new(
            MachineId::parse(&value.machine_id)?,
            NormalizedPath::new(value.normalized_path)?,
        ))
    }
}

impl From<&Thresholds> for proto::Thresholds {
    fn from(value: &Thresholds) -> Self {
        Self {
            pdq_quality_min: u32::from(value.pdq_quality_min),
            aspect_tolerance: value.aspect_tolerance,
            pdq_hamming_max: u32::from(value.pdq_hamming_max),
            phash_part_hamming_max: u32::from(value.phash_part_hamming_max),
            phash_min_passed_parts: u32::from(value.phash_min_passed_parts),
            sobel_min: value.sobel_min,
            video_min_valid_frames: u32::from(value.video_min_valid_frames),
            video_stage1_min: value.video_stage1_min,
            video_stage2_min: value.video_stage2_min,
        }
    }
}

impl TryFrom<proto::Thresholds> for Thresholds {
    type Error = ProtocolError;

    fn try_from(value: proto::Thresholds) -> Result<Self, Self::Error> {
        let thresholds = Self {
            pdq_quality_min: narrow(value.pdq_quality_min, "pdq_quality_min")?,
            aspect_tolerance: value.aspect_tolerance,
            pdq_hamming_max: value.pdq_hamming_max.try_into().map_err(|_| {
                dedup_core::CoreError::InvalidThreshold {
                    field: "pdq_hamming_max",
                    reason: "整数超出 u16",
                }
            })?,
            phash_part_hamming_max: narrow(value.phash_part_hamming_max, "phash_part_hamming_max")?,
            phash_min_passed_parts: narrow(value.phash_min_passed_parts, "phash_min_passed_parts")?,
            sobel_min: value.sobel_min,
            video_min_valid_frames: narrow(value.video_min_valid_frames, "video_min_valid_frames")?,
            video_stage1_min: value.video_stage1_min,
            video_stage2_min: value.video_stage2_min,
        };
        thresholds.validate()?;
        Ok(thresholds)
    }
}

/// 在协议边界验证单个文件块的数据上限。
pub fn validate_file_chunk(chunk: &proto::FileChunk) -> Result<(), ProtocolError> {
    if chunk.data.len() > MAX_FILE_CHUNK_DATA {
        return Err(ProtocolError::FileChunkTooLarge {
            actual: chunk.data.len(),
        });
    }
    Ok(())
}

fn narrow(value: u32, field: &'static str) -> Result<u8, dedup_core::CoreError> {
    value
        .try_into()
        .map_err(|_| dedup_core::CoreError::InvalidThreshold {
            field,
            reason: "整数超出 u8",
        })
}

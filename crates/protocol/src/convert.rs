//! Protobuf 外部键与共享领域值对象之间的显式转换。

use std::net::IpAddr;

use dedup_core::{
    ContentKey, DiskReadConfig, EnumeratorKind, LocationKey, MachineId, NodeConfig,
    NodePathsConfig, NodePostgresConfig, NormalizedPath, Thresholds, WorkerConfig, WorkerMode,
};

use crate::{ProtocolError, proto};

/// 单个 `FileChunk.data` 允许的最大字节数。
pub const MAX_FILE_CHUNK_DATA: usize = 1_048_576;

impl TryFrom<&NodeConfig> for proto::NodeConfigValue {
    type Error = ProtocolError;

    fn try_from(value: &NodeConfig) -> Result<Self, Self::Error> {
        let value = value.clone().normalized()?;
        Ok(Self {
            listen_ip: value.listen_ip.to_string(),
            port: u32::from(value.port),
            enumerator: match value.enumerator {
                EnumeratorKind::WindowsWalker => proto::NodeEnumerator::NodeWindowsWalker as i32,
                EnumeratorKind::Everything => proto::NodeEnumerator::NodeEverything as i32,
            },
            data_path: value.paths.data_path.clone(),
            config_path: value.paths.config_path.clone(),
            log_path: value.paths.log_path.clone(),
            cache_path: value.paths.cache_path.clone(),
            hdd_threads_per_disk: encode_u32(
                value.read.hdd_threads_per_disk,
                "read.hdd_threads_per_disk",
            )?,
            ssd_threads_per_disk: encode_u32(
                value.read.ssd_threads_per_disk,
                "read.ssd_threads_per_disk",
            )?,
            unknown_threads_per_disk: encode_u32(
                value.read.unknown_threads_per_disk,
                "read.unknown_threads_per_disk",
            )?,
            total_threads: encode_u32(value.read.total_threads, "read.total_threads")?,
            block_size_bytes: encode_u64(value.read.block_size_bytes, "read.block_size_bytes")?,
            block_timeout_seconds: value.read.block_timeout_seconds,
            block_retries: value.read.block_retries,
            legacy_worker_count: encode_u32(value.worker_count, "worker_count")?,
            worker_mode: match value.worker.mode {
                WorkerMode::Automatic => proto::NodeWorkerMode::NodeWorkerAutomatic as i32,
                WorkerMode::Manual => proto::NodeWorkerMode::NodeWorkerManual as i32,
            },
            reserved_cores: encode_u32(value.worker.reserved_cores, "worker.reserved_cores")?,
            manual_worker_count: encode_u32(
                value.worker.manual_worker_count,
                "worker.manual_worker_count",
            )?,
            image_extensions: value.image_extensions.clone(),
            video_extensions: value.video_extensions.clone(),
            postgres: Some(proto::NodePostgresConfigValue {
                enabled: value.postgres.enabled,
                host: value.postgres.host.clone(),
                port: u32::from(value.postgres.port),
                database: value.postgres.database.clone(),
                username: value.postgres.username.clone(),
                password: value.postgres.password.clone(),
                connect_timeout_seconds: value.postgres.connect_timeout_seconds,
            }),
        })
    }
}

impl TryFrom<proto::NodeConfigValue> for NodeConfig {
    type Error = ProtocolError;

    fn try_from(value: proto::NodeConfigValue) -> Result<Self, Self::Error> {
        let listen_ip = value
            .listen_ip
            .parse::<IpAddr>()
            .map_err(|_| invalid_config("listen_ip", "不是有效 IP 地址"))?;
        let enumerator = match proto::NodeEnumerator::try_from(value.enumerator) {
            Ok(proto::NodeEnumerator::NodeWindowsWalker) => EnumeratorKind::WindowsWalker,
            Ok(proto::NodeEnumerator::NodeEverything) => EnumeratorKind::Everything,
            Ok(proto::NodeEnumerator::Unspecified) => {
                return Err(invalid_config("enumerator", "未知枚举值"));
            }
            Err(_unknown_value) => return Err(invalid_config("enumerator", "未知枚举值")),
        };
        let mode = match proto::NodeWorkerMode::try_from(value.worker_mode) {
            Ok(proto::NodeWorkerMode::NodeWorkerAutomatic) => WorkerMode::Automatic,
            Ok(proto::NodeWorkerMode::NodeWorkerManual) => WorkerMode::Manual,
            Ok(proto::NodeWorkerMode::Unspecified) => {
                return Err(invalid_config("worker.mode", "未知枚举值"));
            }
            Err(_unknown_value) => return Err(invalid_config("worker.mode", "未知枚举值")),
        };
        let postgres = value.postgres.unwrap_or_default();
        let config = Self {
            listen_ip,
            port: value
                .port
                .try_into()
                .map_err(|_| invalid_config("port", "超出 u16"))?,
            worker_count: narrow_usize(value.legacy_worker_count, "worker_count")?,
            enumerator,
            paths: NodePathsConfig {
                data_path: value.data_path,
                config_path: value.config_path,
                log_path: value.log_path,
                cache_path: value.cache_path,
            },
            read: DiskReadConfig {
                hdd_threads_per_disk: narrow_usize(
                    value.hdd_threads_per_disk,
                    "read.hdd_threads_per_disk",
                )?,
                ssd_threads_per_disk: narrow_usize(
                    value.ssd_threads_per_disk,
                    "read.ssd_threads_per_disk",
                )?,
                unknown_threads_per_disk: narrow_usize(
                    value.unknown_threads_per_disk,
                    "read.unknown_threads_per_disk",
                )?,
                total_threads: narrow_usize(value.total_threads, "read.total_threads")?,
                block_size_bytes: usize::try_from(value.block_size_bytes)
                    .map_err(|_| invalid_config("read.block_size_bytes", "超出 usize"))?,
                block_timeout_seconds: value.block_timeout_seconds,
                block_retries: value.block_retries,
            },
            worker: WorkerConfig {
                mode,
                reserved_cores: narrow_usize(value.reserved_cores, "worker.reserved_cores")?,
                manual_worker_count: narrow_usize(
                    value.manual_worker_count,
                    "worker.manual_worker_count",
                )?,
            },
            image_extensions: value.image_extensions,
            video_extensions: value.video_extensions,
            postgres: NodePostgresConfig {
                enabled: postgres.enabled,
                host: postgres.host,
                port: postgres
                    .port
                    .try_into()
                    .map_err(|_| invalid_config("postgres.port", "超出 u16"))?,
                database: postgres.database,
                username: postgres.username,
                password: postgres.password,
                connect_timeout_seconds: postgres.connect_timeout_seconds,
            },
        };
        Ok(config.normalized()?)
    }
}

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

fn narrow_usize(value: u32, field: &'static str) -> Result<usize, ProtocolError> {
    value
        .try_into()
        .map_err(|_| invalid_config(field, "超出 usize"))
}

fn encode_u32(value: usize, field: &'static str) -> Result<u32, ProtocolError> {
    value
        .try_into()
        .map_err(|_| invalid_config(field, "超出 u32"))
}

fn encode_u64(value: usize, field: &'static str) -> Result<u64, ProtocolError> {
    value
        .try_into()
        .map_err(|_| invalid_config(field, "超出 u64"))
}

fn invalid_config(field: &'static str, reason: &'static str) -> ProtocolError {
    dedup_core::CoreError::InvalidConfig { field, reason }.into()
}

//! 节点 TCP Protobuf 消息、生成代码与领域类型转换。
#![warn(missing_docs)]

mod convert;
mod error;
mod generated;

pub use convert::{MAX_FILE_CHUNK_DATA, validate_file_chunk};
pub use error::ProtocolError;
pub use generated::{FILE_DESCRIPTOR_SET, proto};

/// 当前 Rust V2 节点与管理端握手使用的固定协议版本。
pub const PROTOCOL_VERSION: u32 = 3;

/// 每个运行时任务详情最多保留并通过 wire 返回的最近失败数量。
pub const MAX_RUNTIME_FAILURES: usize = 20;

#[cfg(test)]
mod tests {
    use dedup_core::ContentKey;

    use super::{FILE_DESCRIPTOR_SET, proto, validate_file_chunk};

    /// 防止协议把节点 SQLite 的本地 content_id 当成跨边界身份。
    #[test]
    fn content_key_round_trips_without_local_content_id() {
        let key = ContentKey::new([0x5a; 16], 1234);
        let wire = proto::ContentKey::from(&key);
        assert_eq!(ContentKey::try_from(wire).unwrap(), key);
    }

    /// 防止未来消息误加入只在单个 SQLite 内有效的 content_id 字段。
    #[test]
    fn descriptor_has_no_content_id_field() {
        assert!(!String::from_utf8_lossy(FILE_DESCRIPTOR_SET).contains("content_id"));
    }

    /// 防止预览或快照把超过 1 MiB 的原始文件块放进单个协议消息。
    #[test]
    fn file_chunk_rejects_data_above_one_mib() {
        let chunk = proto::FileChunk {
            data: vec![0; 1_048_577],
            ..Default::default()
        };
        assert!(validate_file_chunk(&chunk).is_err());
    }
}

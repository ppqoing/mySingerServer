//! Protobuf 与领域值对象之间转换时的边界错误。

use thiserror::Error;

/// 无效协议字段在进入业务 crate 前返回的错误。
#[derive(Debug, Error)]
pub enum ProtocolError {
    /// `ContentKey.md5` 不是固定 16 字节。
    #[error("ContentKey.md5 必须是 16 字节，实际为 {actual}")]
    InvalidMd5Length {
        /// 收到的字节数。
        actual: usize,
    },
    /// 领域路径、机器 ID 或阈值校验失败。
    #[error(transparent)]
    InvalidDomain(#[from] dedup_core::CoreError),
    /// 文件块数据超过固定 1 MiB 上限。
    #[error("FileChunk.data 超过 1 MiB: {actual}")]
    FileChunkTooLarge {
        /// 收到的块数据字节数。
        actual: usize,
    },
}

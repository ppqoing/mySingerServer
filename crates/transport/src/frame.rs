//! TCP 上固定四字节大端长度头的读取与写入边界。

use std::io;

use thiserror::Error;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

/// 普通 Envelope 的最大编码长度：8 MiB。
pub const MAX_ORDINARY_FRAME: usize = 8 * 1024 * 1024;

/// 调用方声明的帧用途，用于选择唯一尺寸边界。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FrameClass {
    /// 任务、状态、同步和包含文件块的普通 Envelope。
    Ordinary,
}

impl FrameClass {
    const fn limit(self) -> usize {
        match self {
            Self::Ordinary => MAX_ORDINARY_FRAME,
        }
    }
}

/// 分帧层在读取 Protobuf 之前返回的错误。
#[derive(Debug, Error)]
pub enum FrameError {
    /// 长度头声明了不允许的零字节正文。
    #[error("不允许零长度协议帧")]
    Empty,
    /// 正文超过当前帧类别的固定限制。
    #[error("协议帧过大: {actual} > {limit}")]
    TooLarge {
        /// 收到或准备发送的正文长度。
        actual: usize,
        /// 当前帧类别允许的最大长度。
        limit: usize,
    },
    /// 长度头或正文在声明长度前结束。
    #[error("协议帧被截断")]
    Truncated,
    /// 读写系统错误。
    #[error(transparent)]
    Io(#[from] io::Error),
}

/// 为一段已经编码的 Protobuf 正文添加四字节大端长度头。
pub fn encode_frame(payload: &[u8], class: FrameClass) -> Result<Vec<u8>, FrameError> {
    validate_length(payload.len(), class)?;
    let length = u32::try_from(payload.len()).map_err(|_| FrameError::TooLarge {
        actual: payload.len(),
        limit: class.limit(),
    })?;
    let mut frame = Vec::with_capacity(4 + payload.len());
    frame.extend_from_slice(&length.to_be_bytes());
    frame.extend_from_slice(payload);
    Ok(frame)
}

/// 从异步字节流逐帧读取 Protobuf 正文。
pub struct FrameReader<R> {
    reader: R,
}

impl<R> FrameReader<R>
where
    R: AsyncRead + Unpin,
{
    /// 包装一个 TCP 读取半端或匿名管道读取端。
    pub const fn new(reader: R) -> Self {
        Self { reader }
    }

    /// 读取并验证一个普通 Envelope 正文。
    pub async fn read_frame(&mut self) -> Result<Vec<u8>, FrameError> {
        let mut header = [0_u8; 4];
        read_exact(&mut self.reader, &mut header).await?;
        let length = u32::from_be_bytes(header) as usize;
        validate_length(length, FrameClass::Ordinary)?;
        let mut payload = vec![0_u8; length];
        read_exact(&mut self.reader, &mut payload).await?;
        Ok(payload)
    }
}

/// 向异步字节流写入完整长度头和 Protobuf 正文。
pub struct FrameWriter<W> {
    writer: W,
}

impl<W> FrameWriter<W>
where
    W: AsyncWrite + Unpin,
{
    /// 包装一个 TCP 写入半端或匿名管道写入端。
    pub const fn new(writer: W) -> Self {
        Self { writer }
    }

    /// 验证并写入一个完整帧；返回前刷新底层流。
    pub async fn write_frame(
        &mut self,
        payload: &[u8],
        class: FrameClass,
    ) -> Result<(), FrameError> {
        let frame = encode_frame(payload, class)?;
        self.writer.write_all(&frame).await?;
        self.writer.flush().await?;
        Ok(())
    }
}

fn validate_length(length: usize, class: FrameClass) -> Result<(), FrameError> {
    if length == 0 {
        return Err(FrameError::Empty);
    }
    let limit = class.limit();
    if length > limit {
        return Err(FrameError::TooLarge {
            actual: length,
            limit,
        });
    }
    Ok(())
}

async fn read_exact<R>(reader: &mut R, buffer: &mut [u8]) -> Result<(), FrameError>
where
    R: AsyncRead + Unpin,
{
    match reader.read_exact(buffer).await {
        Ok(_) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::UnexpectedEof => Err(FrameError::Truncated),
        Err(error) => Err(FrameError::Io(error)),
    }
}

//! 已验证 NodeConfig 驱动的分块读取、超时重试和流式 MD5。

use std::{
    io,
    path::{Path, PathBuf},
    time::Duration,
};

use dedup_core::{CoreError, NodeConfig};
use dedup_windows::{OverlappedFileReader, ReadCancellationToken};
use md5::{Digest, Md5};
use thiserror::Error;

/// 单次底层块读取的可分类结果。
#[derive(Debug, Error)]
pub enum BlockReadError {
    /// 当前块在配置超时内没有完成。
    #[error("块读取超时")]
    Timeout {
        /// Windows 边界提供的可选原始错误码。
        raw_os_error: Option<i32>,
    },
    /// 当前任务收到取消请求。
    #[error("读取已取消")]
    Cancelled,
    /// 非超时、非取消的普通文件 I/O 错误。
    #[error(transparent)]
    Io(#[from] io::Error),
}

/// 可注入的定点块读取边界。
pub trait BlockReader {
    /// 从路径指定偏移读取，返回实际字节数、EOF 或分类错误。
    fn read_at(
        &self,
        path: &Path,
        offset: u64,
        buffer: &mut [u8],
        timeout: Duration,
        cancellation: &ReadCancellationToken,
    ) -> Result<usize, BlockReadError>;
}

impl BlockReader for OverlappedFileReader {
    fn read_at(
        &self,
        path: &Path,
        offset: u64,
        buffer: &mut [u8],
        timeout: Duration,
        cancellation: &ReadCancellationToken,
    ) -> Result<usize, BlockReadError> {
        OverlappedFileReader::read_at(self, path, offset, buffer, timeout, cancellation).map_err(
            |error| match error.raw_os_error() {
                Some(258) => BlockReadError::Timeout {
                    raw_os_error: Some(258),
                },
                Some(995) => BlockReadError::Cancelled,
                _ if error.kind() == io::ErrorKind::TimedOut => BlockReadError::Timeout {
                    raw_os_error: error.raw_os_error(),
                },
                _ if cancellation.is_cancelled() => BlockReadError::Cancelled,
                _ => BlockReadError::Io(error),
            },
        )
    }
}

/// 文件级读取失败；疑似物理故障保留精确块身份供后续落库投影。
#[derive(Debug, Error)]
pub enum ReadFailure {
    /// 用户取消，不能继续读取下一块。
    #[error("读取已取消")]
    Cancelled,
    /// 同一块超过配置的全部读取尝试。
    #[error(
        "疑似物理读取故障: {path:?}，文件大小 {file_size}，块偏移 {block_offset}，块长度 {block_len}"
    )]
    SuspectedPhysical {
        /// 实际读取路径。
        path: PathBuf,
        /// 打开任务时观察到的完整文件大小。
        file_size: u64,
        /// 当前配置块的起始偏移。
        block_offset: u64,
        /// 当前配置块的期望长度。
        block_len: usize,
        /// 最后一次超时提供的可选 Windows 原始错误码。
        raw_os_error: Option<i32>,
    },
    /// 非超时文件错误或文件在读取期间缩短。
    #[error("文件读取失败: {path:?}，块偏移 {block_offset}: {source}")]
    Io {
        /// 实际读取路径。
        path: PathBuf,
        /// 发生错误的块内绝对偏移。
        block_offset: u64,
        /// 底层文件错误。
        #[source]
        source: io::Error,
    },
}

/// 按 NodeConfig 固定块大小、超时和重试次数读取文件。
pub struct RetryingFileReader<R> {
    reader: R,
    block_size: usize,
    block_timeout: Duration,
    block_retries: u32,
}

impl<R> RetryingFileReader<R> {
    /// 验证完整 NodeConfig 后冻结本次读取策略。
    pub fn new(reader: R, config: &NodeConfig) -> Result<Self, CoreError> {
        config.validate()?;
        Ok(Self {
            reader,
            block_size: config.read.block_size_bytes,
            block_timeout: Duration::from_secs(config.read.block_timeout_seconds),
            block_retries: config.read.block_retries,
        })
    }
}

impl RetryingFileReader<OverlappedFileReader> {
    /// 创建使用生产 Windows OVERLAPPED I/O 的读取器。
    pub fn system(config: &NodeConfig) -> Result<Self, CoreError> {
        Self::new(OverlappedFileReader, config)
    }
}

impl<R> RetryingFileReader<R>
where
    R: BlockReader,
{
    /// 分块读取完整文件并返回 MD5；取消或单文件故障不会读取后续块。
    pub fn read_file_md5(
        &self,
        path: &Path,
        cancellation: &ReadCancellationToken,
    ) -> Result<[u8; 16], ReadFailure> {
        self.read_file_md5_with_progress(path, cancellation, |_| Ok(()))
    }

    /// 分块读取并只在每个真实成功块写入 MD5 后报告实际字节数。
    pub fn read_file_md5_with_progress(
        &self,
        path: &Path,
        cancellation: &ReadCancellationToken,
        mut progress: impl FnMut(usize) -> io::Result<()>,
    ) -> Result<[u8; 16], ReadFailure> {
        if cancellation.is_cancelled() {
            return Err(ReadFailure::Cancelled);
        }
        let file_size = std::fs::metadata(path)
            .map_err(|source| io_failure(path, 0, source))?
            .len();
        let mut digest = Md5::new();
        let mut block_offset = 0u64;
        let mut buffer = vec![0u8; self.block_size];
        while block_offset < file_size {
            if cancellation.is_cancelled() {
                return Err(ReadFailure::Cancelled);
            }
            let block_len = usize::try_from((file_size - block_offset).min(self.block_size as u64))
                .expect("块长度不超过已经验证的 usize block_size");
            let mut filled = 0usize;
            while filled < block_len {
                if cancellation.is_cancelled() {
                    return Err(ReadFailure::Cancelled);
                }
                let read_offset = block_offset + filled as u64;
                let target = &mut buffer[filled..block_len];
                let mut completed = false;
                for attempt in 0..=self.block_retries {
                    if cancellation.is_cancelled() {
                        return Err(ReadFailure::Cancelled);
                    }
                    match self.reader.read_at(
                        path,
                        read_offset,
                        target,
                        self.block_timeout,
                        cancellation,
                    ) {
                        Ok(0) => {
                            return Err(io_failure(
                                path,
                                read_offset,
                                io::Error::new(
                                    io::ErrorKind::UnexpectedEof,
                                    "文件在分块读取期间提前结束",
                                ),
                            ));
                        }
                        Ok(read) if read <= target.len() => {
                            filled += read;
                            completed = true;
                            break;
                        }
                        Ok(_) => {
                            return Err(io_failure(
                                path,
                                read_offset,
                                io::Error::new(
                                    io::ErrorKind::InvalidData,
                                    "块读取器返回了超过缓冲区的长度",
                                ),
                            ));
                        }
                        Err(BlockReadError::Timeout { .. }) if attempt < self.block_retries => {}
                        Err(BlockReadError::Timeout { raw_os_error }) => {
                            return Err(ReadFailure::SuspectedPhysical {
                                path: path.to_path_buf(),
                                file_size,
                                block_offset,
                                block_len,
                                raw_os_error,
                            });
                        }
                        Err(BlockReadError::Cancelled) => return Err(ReadFailure::Cancelled),
                        Err(BlockReadError::Io(source)) => {
                            return Err(io_failure(path, read_offset, source));
                        }
                    }
                }
                debug_assert!(completed);
            }
            digest.update(&buffer[..block_len]);
            progress(block_len).map_err(|source| io_failure(path, block_offset, source))?;
            block_offset += block_len as u64;
        }
        Ok(digest.finalize().into())
    }
}

fn io_failure(path: &Path, block_offset: u64, source: io::Error) -> ReadFailure {
    ReadFailure::Io {
        path: path.to_path_buf(),
        block_offset,
        source,
    }
}

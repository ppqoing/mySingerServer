//! Worker 内单文件、单句柄的可重试读取会话。

use std::{
    io::{self, SeekFrom},
    path::{Path, PathBuf},
    time::Duration,
};

use dedup_media_ffmpeg::SeekableMediaSource;
use dedup_windows::{ReadCancellationToken, ReusableOverlappedFile};
use thiserror::Error;

const WAIT_TIMEOUT_RAW_CODE: i32 = 258;
const OPERATION_ABORTED_RAW_CODE: i32 = 995;

/// 一次 Worker 文件会话冻结的块读取限制。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct WorkerReadLimits {
    /// 单次读取块大小。
    block_size: usize,
    /// 单块读取超时。
    block_timeout: Duration,
    /// 单块首次读取失败后的额外重试次数。
    block_retries: u32,
}

impl WorkerReadLimits {
    /// 验证并创建 Worker 读取限制；`timeout_ms` 必须大于零。
    pub fn new(
        block_size: usize,
        timeout_ms: u64,
        block_retries: u32,
    ) -> Result<Self, WorkerFileSessionError> {
        if block_size == 0 || timeout_ms == 0 {
            return Err(WorkerFileSessionError::InvalidLimits);
        }
        Ok(Self {
            block_size,
            block_timeout: Duration::from_millis(timeout_ms),
            block_retries,
        })
    }
}

/// Worker 文件会话的取消、I/O 与疑似物理损坏错误。
#[derive(Debug, Error)]
pub enum WorkerFileSessionError {
    /// 块大小或超时为零。
    #[error("Worker 文件读取参数无效")]
    InvalidLimits,
    /// 文件读取已取消。
    #[error("Worker 文件读取已取消")]
    Cancelled,
    /// 同一块在全部重试后仍然超时，应跳过文件并写入故障记录。
    #[error("疑似物理读取损坏: 偏移 {offset}，块大小 {size}")]
    SuspectedPhysicalRead {
        /// 发生超时的绝对文件偏移。
        offset: u64,
        /// 本次期望读取的块大小。
        size: usize,
        /// Windows 提供的原始错误码。
        raw_os_error: Option<i32>,
    },
    /// 非超时的普通文件 I/O 错误。
    #[error("文件读取失败: {path:?}，偏移 {offset}: {source}")]
    Io {
        /// 已打开文件的原始路径，仅用于诊断和持久化记录。
        path: PathBuf,
        /// 发生错误的绝对文件偏移。
        offset: u64,
        /// 底层文件系统错误。
        #[source]
        source: io::Error,
    },
}

/// Worker 在一次基础计算内复用的文件句柄和 AVIO 游标。
pub struct WorkerFileSession {
    /// 已打开且不随原路径重命名失效的 OVERLAPPED 文件。
    file: ReusableOverlappedFile,
    /// 打开文件时使用的路径，用于故障追溯。
    path: PathBuf,
    /// 自定义 AVIO 当前读取位置。
    cursor: u64,
    /// 本次会话统一使用的读取参数。
    limits: WorkerReadLimits,
    /// 媒体回调沿用最近一次基础计算传入的取消标记。
    cancellation: ReadCancellationToken,
}

impl WorkerFileSession {
    /// 打开文件一次，并冻结长度、读取限制和可复用 Windows 句柄。
    pub fn open(path: &Path, limits: WorkerReadLimits) -> Result<Self, WorkerFileSessionError> {
        let file =
            ReusableOverlappedFile::open(path).map_err(|source| WorkerFileSessionError::Io {
                path: path.to_path_buf(),
                offset: 0,
                source,
            })?;
        Ok(Self {
            file,
            path: path.to_path_buf(),
            cursor: 0,
            limits,
            cancellation: ReadCancellationToken::new(),
        })
    }

    /// 返回实现 FFmpeg 自定义 AVIO 的同一文件会话。
    pub fn media_source(&mut self) -> &mut dyn SeekableMediaSource {
        self
    }

    /// 返回打开时冻结的文件长度，供 Worker 校验枚举身份没有变化。
    pub fn len(&self) -> u64 {
        self.file.len()
    }

    /// 返回文件是否为空。
    pub fn is_empty(&self) -> bool {
        self.file.is_empty()
    }

    /// 使用配置的超时和重试次数完成一次定点读取。
    fn read_at_with_retry(
        &mut self,
        offset: u64,
        buffer: &mut [u8],
    ) -> Result<usize, WorkerFileSessionError> {
        if self.cancellation.is_cancelled() {
            return Err(WorkerFileSessionError::Cancelled);
        }
        for attempt in 0..=self.limits.block_retries {
            match self.file.read_at(
                offset,
                buffer,
                self.limits.block_timeout,
                &self.cancellation,
            ) {
                Ok(read) => return Ok(read),
                Err(error) if is_cancelled(&error, &self.cancellation) => {
                    return Err(WorkerFileSessionError::Cancelled);
                }
                Err(error) if is_timeout(&error) && attempt < self.limits.block_retries => {}
                Err(error) if is_timeout(&error) => {
                    return Err(WorkerFileSessionError::SuspectedPhysicalRead {
                        offset,
                        size: buffer.len(),
                        raw_os_error: error.raw_os_error(),
                    });
                }
                Err(source) => return Err(self.io_error(offset, source)),
            }
        }
        unreachable!("读取循环总会返回成功或最后一次失败")
    }

    /// 把底层错误附加到当前文件身份和偏移。
    fn io_error(&self, offset: u64, source: io::Error) -> WorkerFileSessionError {
        WorkerFileSessionError::Io {
            path: self.path.clone(),
            offset,
            source,
        }
    }
}

impl SeekableMediaSource for WorkerFileSession {
    fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
        let read = self
            .read_at_with_retry(self.cursor, buffer)
            .map_err(io::Error::other)?;
        self.cursor = self.cursor.saturating_add(read as u64);
        Ok(read)
    }

    fn seek(&mut self, position: SeekFrom) -> io::Result<u64> {
        let next = match position {
            SeekFrom::Start(offset) => i128::from(offset),
            SeekFrom::Current(offset) => i128::from(self.cursor) + i128::from(offset),
            SeekFrom::End(offset) => i128::from(self.file.len()) + i128::from(offset),
        };
        self.cursor = u64::try_from(next)
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "媒体读取位置不能小于零"))?;
        Ok(self.cursor)
    }

    fn len(&self) -> u64 {
        self.file.len()
    }
}

/// 判断底层错误是否为 Windows 块读取超时。
fn is_timeout(error: &io::Error) -> bool {
    error.kind() == io::ErrorKind::TimedOut || error.raw_os_error() == Some(WAIT_TIMEOUT_RAW_CODE)
}

/// 判断底层错误是否为显式取消。
fn is_cancelled(error: &io::Error, cancellation: &ReadCancellationToken) -> bool {
    cancellation.is_cancelled() || error.raw_os_error() == Some(OPERATION_ABORTED_RAW_CODE)
}

//! 使用 Windows OVERLAPPED 的可取消定点文件读取边界。

use std::{
    io,
    os::windows::ffi::OsStrExt,
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use windows::{
    Win32::{
        Foundation::{
            CloseHandle, ERROR_HANDLE_EOF, ERROR_IO_PENDING, ERROR_OPERATION_ABORTED, GENERIC_READ,
            HANDLE, WAIT_OBJECT_0, WAIT_TIMEOUT,
        },
        Storage::FileSystem::{
            CreateFileW, FILE_ATTRIBUTE_NORMAL, FILE_FLAG_OVERLAPPED, FILE_SHARE_DELETE,
            FILE_SHARE_READ, FILE_SHARE_WRITE, GetFileSizeEx, OPEN_EXISTING, ReadFile,
        },
        System::{
            IO::{CancelIoEx, GetOverlappedResult, OVERLAPPED, OVERLAPPED_0_0},
            Threading::{CreateEventW, ResetEvent, WaitForSingleObject},
        },
    },
    core::{HRESULT, PCWSTR},
};

const CANCELLATION_POLL_MILLISECONDS: u32 = 10;

/// 跨线程共享的文件读取取消标记。
#[derive(Clone, Debug, Default)]
pub struct ReadCancellationToken(Arc<AtomicBool>);

impl ReadCancellationToken {
    /// 创建尚未取消的标记。
    pub fn new() -> Self {
        Self::default()
    }

    /// 请求当前读取尽快执行 `CancelIoEx` 并停止后续块。
    pub fn cancel(&self) {
        self.0.store(true, Ordering::Release);
    }

    /// 当前读取任务是否已经收到取消请求。
    pub fn is_cancelled(&self) -> bool {
        self.0.load(Ordering::Acquire)
    }
}

/// 生产 Windows OVERLAPPED 块读取器。
#[derive(Clone, Copy, Debug, Default)]
pub struct OverlappedFileReader;

impl OverlappedFileReader {
    /// 从绝对文件路径的指定偏移读取一个块。
    ///
    /// 超时返回带 `WAIT_TIMEOUT` 原始码的 `TimedOut` 错误；取消返回带
    /// `ERROR_OPERATION_ABORTED` 原始码的错误。返回前总会等待取消的 I/O 收束，调用者的
    /// buffer 和 OVERLAPPED 不会在内核仍使用时离开作用域。
    pub fn read_at(
        &self,
        path: &Path,
        offset: u64,
        buffer: &mut [u8],
        timeout: Duration,
        cancellation: &ReadCancellationToken,
    ) -> io::Result<usize> {
        let mut file = ReusableOverlappedFile::open(path)?;
        file.read_at(offset, buffer, timeout, cancellation)
    }
}

/// Worker 内一次打开并支持重复定点读取的 OVERLAPPED 文件。
///
/// 类型以可变借用串行复用同一个文件和事件句柄，避免同一事件同时服务多个内核 I/O。
pub struct ReusableOverlappedFile {
    file: OwnedHandle,
    event: OwnedHandle,
    len: u64,
}

impl ReusableOverlappedFile {
    /// 打开一个允许共享读写和删除的本地文件，并冻结打开时的文件长度。
    pub fn open(path: &Path) -> io::Result<Self> {
        let file = open_overlapped_file(path)?;
        let event = create_event()?;
        let mut len = 0_i64;
        // SAFETY: file 是当前值独占的有效文件句柄，输出指针在调用期间有效。
        unsafe { GetFileSizeEx(file.0, &mut len) }.map_err(io_error)?;
        let len = u64::try_from(len)
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "文件长度不能为负数"))?;
        Ok(Self { file, event, len })
    }

    /// 返回打开文件时冻结的字节长度。
    pub const fn len(&self) -> u64 {
        self.len
    }

    /// 返回文件在打开时是否为空。
    pub const fn is_empty(&self) -> bool {
        self.len == 0
    }

    /// 使用同一文件和事件句柄从指定偏移读取一个块。
    pub fn read_at(
        &mut self,
        offset: u64,
        buffer: &mut [u8],
        timeout: Duration,
        cancellation: &ReadCancellationToken,
    ) -> io::Result<usize> {
        if cancellation.is_cancelled() {
            return Err(cancelled_error());
        }
        if buffer.is_empty() || offset >= self.len {
            return Ok(0);
        }
        let maximum = usize::try_from((self.len - offset).min(buffer.len() as u64))
            .expect("读取长度不超过缓冲区 usize");
        let buffer = &mut buffer[..maximum];
        let _length = u32::try_from(buffer.len())
            .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "读取块超过 u32"))?;
        // SAFETY: event 是当前文件会话独占且没有未收束 I/O 的手动复位事件。
        unsafe { ResetEvent(self.event.0) }.map_err(io_error)?;
        let mut overlapped = OVERLAPPED {
            hEvent: self.event.0,
            ..Default::default()
        };
        overlapped.Anonymous.Anonymous = OVERLAPPED_0_0 {
            Offset: offset as u32,
            OffsetHigh: (offset >> 32) as u32,
        };

        // SAFETY: 文件以 OVERLAPPED 打开；buffer 与 OVERLAPPED 保持到完成或取消收束后。
        let start = unsafe { ReadFile(self.file.0, Some(buffer), None, Some(&mut overlapped)) };
        match start {
            Ok(()) => {}
            Err(error) if error.code() == HRESULT::from_win32(ERROR_IO_PENDING.0) => {
                wait_for_completion(
                    self.file.0,
                    self.event.0,
                    &mut overlapped,
                    timeout,
                    cancellation,
                )?;
            }
            Err(error) if error.code() == HRESULT::from_win32(ERROR_HANDLE_EOF.0) => return Ok(0),
            Err(error) => return Err(io_error(error)),
        }

        if cancellation.is_cancelled() {
            cancel_and_drain(self.file.0, &mut overlapped);
            return Err(cancelled_error());
        }
        let mut transferred = 0u32;
        // SAFETY: OVERLAPPED 已同步完成；输出指针在调用期间有效。
        match unsafe { GetOverlappedResult(self.file.0, &overlapped, &mut transferred, false) } {
            Ok(()) => Ok(transferred as usize),
            Err(error) if error.code() == HRESULT::from_win32(ERROR_HANDLE_EOF.0) => Ok(0),
            Err(error) => Err(io_error(error)),
        }
    }
}

struct OwnedHandle(HANDLE);

impl Drop for OwnedHandle {
    fn drop(&mut self) {
        // SAFETY: 句柄来自成功的 CreateFileW/CreateEventW，并只由此值关闭一次。
        let _ = unsafe { CloseHandle(self.0) };
    }
}

fn open_overlapped_file(path: &Path) -> io::Result<OwnedHandle> {
    let path = path
        .as_os_str()
        .encode_wide()
        .chain([0])
        .collect::<Vec<_>>();
    // SAFETY: path 是调用期间有效的 NUL 结尾 UTF-16，返回句柄交给 OwnedHandle。
    let handle = unsafe {
        CreateFileW(
            PCWSTR(path.as_ptr()),
            GENERIC_READ.0,
            FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
            None,
            OPEN_EXISTING,
            FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OVERLAPPED,
            None,
        )
    }
    .map_err(io_error)?;
    Ok(OwnedHandle(handle))
}

fn create_event() -> io::Result<OwnedHandle> {
    // SAFETY: 创建无名称、手动复位、初始未触发事件；句柄交给 OwnedHandle。
    let handle = unsafe { CreateEventW(None, true, false, PCWSTR::null()) }.map_err(io_error)?;
    Ok(OwnedHandle(handle))
}

fn wait_for_completion(
    file: HANDLE,
    event: HANDLE,
    overlapped: &mut OVERLAPPED,
    timeout: Duration,
    cancellation: &ReadCancellationToken,
) -> io::Result<()> {
    let deadline = Instant::now()
        .checked_add(timeout)
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "读取超时范围无效"))?;
    loop {
        if cancellation.is_cancelled() {
            cancel_and_drain(file, overlapped);
            return Err(cancelled_error());
        }
        let now = Instant::now();
        if now >= deadline {
            cancel_and_drain(file, overlapped);
            return Err(timeout_error());
        }
        let wait_milliseconds = deadline
            .saturating_duration_since(now)
            .as_millis()
            .clamp(1, CANCELLATION_POLL_MILLISECONDS as u128)
            as u32;
        // SAFETY: event 是当前读取专用的有效事件句柄。
        match unsafe { WaitForSingleObject(event, wait_milliseconds) } {
            WAIT_OBJECT_0 => return Ok(()),
            WAIT_TIMEOUT => {}
            _ => {
                let error = windows::core::Error::from_thread();
                cancel_and_drain(file, overlapped);
                return Err(io_error(error));
            }
        }
    }
}

fn cancel_and_drain(file: HANDLE, overlapped: &mut OVERLAPPED) {
    // SAFETY: 文件句柄和 OVERLAPPED 属于当前尚未收束的操作。
    let _ = unsafe { CancelIoEx(file, Some(overlapped)) };
    let mut transferred = 0u32;
    // SAFETY: 等待内核不再访问 OVERLAPPED/buffer；取消错误本身不覆盖调用者的原因。
    let _ = unsafe { GetOverlappedResult(file, overlapped, &mut transferred, true) };
}

fn timeout_error() -> io::Error {
    io::Error::from_raw_os_error(WAIT_TIMEOUT.0 as i32)
}

fn cancelled_error() -> io::Error {
    io::Error::from_raw_os_error(ERROR_OPERATION_ABORTED.0 as i32)
}

fn io_error(error: windows::core::Error) -> io::Error {
    let hresult = error.code().0 as u32;
    let raw = if hresult & 0xffff_0000 == 0x8007_0000 {
        hresult & 0xffff
    } else {
        hresult
    };
    io::Error::from_raw_os_error(raw as i32)
}

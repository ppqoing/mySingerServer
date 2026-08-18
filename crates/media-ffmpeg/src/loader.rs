//! Windows DLL 搜索隔离与固定 FFmpeg 函数表加载。

use std::{
    ffi::{OsStr, c_void},
    fmt,
    path::{Path, PathBuf},
    ptr,
};

use thiserror::Error;
use windows::{
    Win32::{
        Foundation::{FreeLibrary, HMODULE},
        System::LibraryLoader::{
            AddDllDirectory, GetProcAddress, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR,
            LOAD_LIBRARY_SEARCH_SYSTEM32, LOAD_LIBRARY_SEARCH_USER_DIRS, LoadLibraryExW,
            RemoveDllDirectory, SetDefaultDllDirectories,
        },
    },
    core::{PCSTR, PCWSTR},
};

use crate::ffi::FfmpegApi;

const REQUIRED_DLLS: [&str; 5] = [
    "avutil-60.dll",
    "swresample-6.dll",
    "swscale-9.dll",
    "avcodec-62.dll",
    "avformat-62.dll",
];

/// 返回固定 FFmpeg 8.0.1 运行时 DLL 的加载顺序。
///
/// 顺序先放基础库再放依赖它们的编解码与封装库，和发布清单保持一致。
pub fn required_dlls() -> &'static [&'static str; 5] {
    &REQUIRED_DLLS
}

/// 根据 `worker.exe` 的实际路径计算唯一允许的 FFmpeg DLL 目录。
pub fn dll_directory(worker_executable: &Path) -> Result<PathBuf, FfmpegError> {
    let parent = worker_executable
        .parent()
        .filter(|path| !path.as_os_str().is_empty())
        .ok_or_else(|| FfmpegError::WorkerHasNoDirectory(worker_executable.to_path_buf()))?;
    Ok(parent.join("runtime").join("ffmpeg"))
}

/// 已加载的固定 FFmpeg 8.0.1 运行时。
///
/// 该值持有全部 DLL 句柄；探测或解码期间函数指针不会悬空。释放时按加载逆序
/// 卸载 DLL，并移除本实例添加的 DLL 搜索目录。
pub struct Ffmpeg {
    pub(crate) api: FfmpegApi,
    modules: Vec<HMODULE>,
    directory_cookie: *mut c_void,
    directory: PathBuf,
}

impl Ffmpeg {
    /// 只从 `<worker目录>\runtime\ffmpeg` 和 Windows System32 加载固定 DLL。
    ///
    /// 当前工作目录与 `PATH` 都不参与解析；缺少任何一个清单 DLL 时直接返回包含
    /// 精确文件名的错误。
    pub fn load_from_worker_executable(worker_executable: &Path) -> Result<Self, FfmpegError> {
        let directory = dll_directory(worker_executable)?;
        for name in required_dlls() {
            let path = directory.join(name);
            if !path.is_file() {
                return Err(FfmpegError::MissingDll(path));
            }
        }

        let default_flags = LOAD_LIBRARY_SEARCH_SYSTEM32 | LOAD_LIBRARY_SEARCH_USER_DIRS;
        // SAFETY: 标志只允许 System32 和随后显式添加的用户目录；不传入裸指针。
        unsafe { SetDefaultDllDirectories(default_flags) }.map_err(|source| {
            FfmpegError::Windows {
                operation: "SetDefaultDllDirectories",
                source,
            }
        })?;

        let wide_directory = wide_null(&directory);
        // SAFETY: `wide_directory` 在调用期间保持 NUL 结尾且有效。
        let cookie = unsafe { AddDllDirectory(PCWSTR(wide_directory.as_ptr())) };
        if cookie.is_null() {
            return Err(FfmpegError::Windows {
                operation: "AddDllDirectory",
                source: windows::core::Error::from_thread(),
            });
        }

        let mut pending = PendingLoad {
            modules: Vec::with_capacity(REQUIRED_DLLS.len()),
            directory_cookie: cookie,
        };
        let load_flags = LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR
            | LOAD_LIBRARY_SEARCH_SYSTEM32
            | LOAD_LIBRARY_SEARCH_USER_DIRS;
        for name in required_dlls() {
            let path = directory.join(name);
            let wide_path = wide_null(&path);
            // SAFETY: 使用绝对文件名和受限搜索标志；UTF-16 缓冲区在调用期间有效。
            let module = unsafe { LoadLibraryExW(PCWSTR(wide_path.as_ptr()), None, load_flags) }
                .map_err(|source| FfmpegError::LoadDll {
                    path: path.clone(),
                    source,
                })?;
            pending.modules.push(module);
        }

        // SAFETY: 所有模块句柄都由 `pending` 持有，并覆盖函数表解析的整个过程。
        let api = unsafe { resolve_api(&pending.modules)? };
        let modules = std::mem::take(&mut pending.modules);
        let directory_cookie = std::mem::replace(&mut pending.directory_cookie, ptr::null_mut());
        Ok(Self {
            api,
            modules,
            directory_cookie,
            directory,
        })
    }
}

impl fmt::Debug for Ffmpeg {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Ffmpeg")
            .field("directory", &self.directory)
            .field("module_count", &self.modules.len())
            .finish_non_exhaustive()
    }
}

impl Drop for Ffmpeg {
    fn drop(&mut self) {
        unload_all(&mut self.modules);
        remove_directory(self.directory_cookie);
        self.directory_cookie = ptr::null_mut();
    }
}

/// FFmpeg DLL 加载、媒体打开与解码的统一错误。
#[derive(Debug, Error)]
pub enum FfmpegError {
    /// `worker.exe` 路径没有可用父目录。
    #[error("worker executable has no directory: {0}")]
    WorkerHasNoDirectory(PathBuf),
    /// 固定运行目录缺少一个清单 DLL。
    #[error("required FFmpeg DLL is missing: {0}")]
    MissingDll(PathBuf),
    /// Windows DLL 搜索设置调用失败。
    #[error("{operation} failed: {source}")]
    Windows {
        /// 失败的 Win32 API 名称。
        operation: &'static str,
        /// Win32 返回的错误。
        #[source]
        source: windows::core::Error,
    },
    /// 某个固定 DLL 无法加载。
    #[error("failed to load FFmpeg DLL {path}: {source}")]
    LoadDll {
        /// DLL 的绝对路径。
        path: PathBuf,
        /// Windows 加载错误。
        #[source]
        source: windows::core::Error,
    },
    /// 固定 ABI 所需的导出函数不存在。
    #[error("FFmpeg symbol is missing: {0}")]
    MissingSymbol(&'static str),
    /// 媒体路径包含 FFmpeg URL 接口无法表示的 NUL 字符。
    #[error("media path contains a NUL character: {0}")]
    PathContainsNul(PathBuf),
    /// FFmpeg API 返回负错误码。
    #[error("FFmpeg operation {operation} failed with code {code}")]
    Api {
        /// 失败操作名。
        operation: &'static str,
        /// 原始 FFmpeg 错误码。
        code: i32,
    },
    /// 输入没有可解码的视频流或尺寸无效。
    #[error("invalid or unsupported visual media: {0}")]
    InvalidMedia(&'static str),
    /// 归一化抽帧位置不在 `0.0..=1.0`。
    #[error("frame position must be finite and within 0.0..=1.0: {0}")]
    InvalidPosition(f64),
}

struct PendingLoad {
    modules: Vec<HMODULE>,
    directory_cookie: *mut c_void,
}

impl Drop for PendingLoad {
    fn drop(&mut self) {
        unload_all(&mut self.modules);
        remove_directory(self.directory_cookie);
    }
}

fn unload_all(modules: &mut Vec<HMODULE>) {
    while let Some(module) = modules.pop() {
        // SAFETY: 每个句柄由本加载器成功取得且只在这里释放一次。
        let _ = unsafe { FreeLibrary(module) };
    }
}

fn remove_directory(cookie: *mut c_void) {
    if !cookie.is_null() {
        // SAFETY: cookie 来自本进程成功的 AddDllDirectory，且只移除一次。
        let _ = unsafe { RemoveDllDirectory(cookie.cast_const()) };
    }
}

fn wide_null(path: &Path) -> Vec<u16> {
    use std::os::windows::ffi::OsStrExt;
    OsStr::new(path).encode_wide().chain(Some(0)).collect()
}

unsafe fn resolve_api(modules: &[HMODULE]) -> Result<FfmpegApi, FfmpegError> {
    macro_rules! symbol {
        ($name:literal) => {{
            // SAFETY: 目标类型是与固定 FFmpeg 8.0.1 头文件完全一致的函数指针类型。
            unsafe { resolve_symbol(modules, concat!($name, "\0").as_bytes(), $name)? }
        }};
    }

    Ok(FfmpegApi {
        avformat_open_input: symbol!("avformat_open_input"),
        avformat_find_stream_info: symbol!("avformat_find_stream_info"),
        av_find_best_stream: symbol!("av_find_best_stream"),
        avformat_seek_file: symbol!("avformat_seek_file"),
        av_read_frame: symbol!("av_read_frame"),
        avformat_close_input: symbol!("avformat_close_input"),
        avcodec_find_decoder: symbol!("avcodec_find_decoder"),
        avcodec_alloc_context3: symbol!("avcodec_alloc_context3"),
        avcodec_parameters_to_context: symbol!("avcodec_parameters_to_context"),
        avcodec_open2: symbol!("avcodec_open2"),
        avcodec_send_packet: symbol!("avcodec_send_packet"),
        avcodec_receive_frame: symbol!("avcodec_receive_frame"),
        avcodec_flush_buffers: symbol!("avcodec_flush_buffers"),
        avcodec_free_context: symbol!("avcodec_free_context"),
        av_packet_alloc: symbol!("av_packet_alloc"),
        av_packet_unref: symbol!("av_packet_unref"),
        av_packet_free: symbol!("av_packet_free"),
        av_frame_alloc: symbol!("av_frame_alloc"),
        av_frame_unref: symbol!("av_frame_unref"),
        av_frame_free: symbol!("av_frame_free"),
        sws_get_context: symbol!("sws_getContext"),
        sws_scale: symbol!("sws_scale"),
        sws_free_context: symbol!("sws_freeContext"),
    })
}

unsafe fn resolve_symbol<T: Copy>(
    modules: &[HMODULE],
    nul_name: &'static [u8],
    display_name: &'static str,
) -> Result<T, FfmpegError> {
    for module in modules.iter().rev() {
        // SAFETY: `nul_name` is a static NUL-terminated ASCII symbol and the module is loaded.
        if let Some(procedure) = unsafe { GetProcAddress(*module, PCSTR(nul_name.as_ptr())) } {
            debug_assert_eq!(std::mem::size_of::<T>(), std::mem::size_of_val(&procedure));
            // SAFETY: 调用方为该固定符号提供了由 FFmpeg 8.0.1 头文件定义的函数类型。
            return Ok(unsafe { std::mem::transmute_copy(&procedure) });
        }
    }
    Err(FfmpegError::MissingSymbol(display_name))
}

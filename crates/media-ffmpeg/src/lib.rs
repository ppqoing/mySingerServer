//! FFmpeg 8.0.1 DLL 的受限加载、FFI 与安全探测解码接口。
#![warn(missing_docs)]

mod decode;
mod ffi;
mod loader;

pub use decode::{DecodedFrame, MediaProbe, SeekableMediaSource};
pub use loader::{Ffmpeg, FfmpegError, dll_directory, required_dlls};

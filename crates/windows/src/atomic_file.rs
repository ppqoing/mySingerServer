//! 同目录文件的 Windows 原子替换边界。

use std::{io, os::windows::ffi::OsStrExt, path::Path};

use windows::{
    Win32::Storage::FileSystem::{MOVEFILE_REPLACE_EXISTING, MOVEFILE_WRITE_THROUGH, MoveFileExW},
    core::PCWSTR,
};

/// 用 Windows 原子替换语义把同目录来源文件发布到目标路径。
///
/// 两个路径必须都是绝对路径且拥有同一个父目录；调用成功后来源路径不再存在。
pub fn atomic_replace_file(source: &Path, destination: &Path) -> io::Result<()> {
    validate_siblings(source, destination)?;
    std::fs::metadata(source)?;
    let source_wide = wide_path(source)?;
    let destination_wide = wide_path(destination)?;
    unsafe {
        MoveFileExW(
            PCWSTR(source_wide.as_ptr()),
            PCWSTR(destination_wide.as_ptr()),
            MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH,
        )
        .map_err(|_| io::Error::last_os_error())
    }
}

/// 拒绝跨目录或相对路径，避免 MoveFileExW 退化为不可预期的跨卷操作。
fn validate_siblings(source: &Path, destination: &Path) -> io::Result<()> {
    if !source.is_absolute() || !destination.is_absolute() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "原子替换路径必须是绝对路径",
        ));
    }
    if source == destination || source.parent() != destination.parent() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "原子替换来源和目标必须是不同的同目录文件",
        ));
    }
    Ok(())
}

/// 转换为以 NUL 结尾的 Windows API 路径，拒绝内部 NUL。
fn wide_path(path: &Path) -> io::Result<Vec<u16>> {
    let mut wide = path.as_os_str().encode_wide().collect::<Vec<_>>();
    if wide.contains(&0) {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "路径不能包含 NUL",
        ));
    }
    wide.push(0);
    Ok(wide)
}

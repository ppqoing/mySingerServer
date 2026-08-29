//! 同目录文件的 Windows 原子替换边界。

use std::{
    fs::File,
    io,
    os::windows::{ffi::OsStrExt, io::AsRawHandle},
    path::Path,
};

use windows::{
    Win32::Foundation::HANDLE,
    Win32::Storage::FileSystem::{
        FileRenameInfoEx, MOVEFILE_REPLACE_EXISTING, MOVEFILE_WRITE_THROUGH, MoveFileExW,
        REPLACEFILE_WRITE_THROUGH, ReplaceFileW, SetFileInformationByHandle,
    },
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
    match std::fs::metadata(destination) {
        Ok(_) => {
            // 目标可能被结果 reader 持有；ReplaceFileW 能在共享删除句柄下保持旧身份可读。
            unsafe {
                ReplaceFileW(
                    PCWSTR(destination_wide.as_ptr()),
                    PCWSTR(source_wide.as_ptr()),
                    PCWSTR::null(),
                    REPLACEFILE_WRITE_THROUGH,
                    None,
                    None,
                )
            }
            .map_err(|_| io::Error::last_os_error())
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => unsafe {
            MoveFileExW(
                PCWSTR(source_wide.as_ptr()),
                PCWSTR(destination_wide.as_ptr()),
                MOVEFILE_REPLACE_EXISTING | MOVEFILE_WRITE_THROUGH,
            )
            .map_err(|_| io::Error::last_os_error())
        },
        Err(error) => Err(error),
    }
}

/// 使用已打开的来源句柄原子替换目标，保持新 reader 绑定同一文件身份。
///
/// 来源句柄必须拥有 DELETE 权限并允许共享删除；目标句柄也必须允许共享删除。
pub fn atomic_replace_file_from_handle(
    source_file: &File,
    source: &Path,
    destination: &Path,
) -> io::Result<()> {
    validate_siblings(source, destination)?;
    std::fs::metadata(source)?;
    let destination_wide = wide_path(destination)?;
    let file_name = &destination_wide[..destination_wide.len().saturating_sub(1)];
    let file_name_bytes = file_name
        .len()
        .checked_mul(std::mem::size_of::<u16>())
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "目标路径过长"))?;
    let info_len = 20_usize
        .checked_add(file_name_bytes)
        .and_then(|length| length.checked_add(std::mem::size_of::<u16>()))
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "目标路径过长"))?;
    let info_len_u32 = u32::try_from(info_len)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidInput, "目标路径过长"))?;
    let mut rename_info = vec![0_u8; info_len];
    // FILE_RENAME_INFO_EX 的 Flags：替换已有目标并使用 Windows POSIX 重命名语义。
    let flags = 0x0000_0001_u32 | 0x0000_0002_u32;
    rename_info[..4].copy_from_slice(&flags.to_le_bytes());
    rename_info[16..20].copy_from_slice(&(file_name_bytes as u32).to_le_bytes());
    for (index, value) in file_name.iter().enumerate() {
        let offset = 20 + index * 2;
        rename_info[offset..offset + 2].copy_from_slice(&value.to_le_bytes());
    }

    unsafe {
        SetFileInformationByHandle(
            HANDLE(source_file.as_raw_handle()),
            FileRenameInfoEx,
            rename_info.as_ptr().cast(),
            info_len_u32,
        )
    }
    .map_err(|_| io::Error::last_os_error())
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

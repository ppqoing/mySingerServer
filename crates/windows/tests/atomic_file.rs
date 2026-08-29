//! Windows 原子替换文件的真实文件系统行为测试。

use std::{
    fs,
    io::{Read, Seek, SeekFrom},
    os::windows::{ffi::OsStrExt, fs::OpenOptionsExt},
    path::Path,
};

use dedup_windows::{atomic_replace_file, atomic_replace_file_from_handle};
use tempfile::tempdir;
use windows::{
    Win32::{
        Foundation::{CloseHandle, GENERIC_READ},
        Storage::FileSystem::{CreateFileW, FILE_ATTRIBUTE_NORMAL, FILE_SHARE_MODE, OPEN_EXISTING},
    },
    core::PCWSTR,
};

/// 防止替换退化为删除后复制，导致读者看见旧目标消失。
#[test]
fn replaces_existing_sibling_without_leaving_old_bytes() {
    let directory = tempdir().unwrap();
    let source = directory.path().join("partial.tsv");
    let destination = directory.path().join("result.tsv");
    fs::write(&source, b"new-result").unwrap();
    fs::write(&destination, b"old-result").unwrap();

    atomic_replace_file(&source, &destination).unwrap();

    assert!(!source.exists());
    assert_eq!(fs::read(&destination).unwrap(), b"new-result");
}

/// 防止首次发布因为不存在旧结果而失败。
#[test]
fn publishes_when_destination_does_not_exist() {
    let directory = tempdir().unwrap();
    let source = directory.path().join("partial.tsv");
    let destination = directory.path().join("result.tsv");
    fs::write(&source, b"first-result").unwrap();

    atomic_replace_file(&source, &destination).unwrap();

    assert_eq!(fs::read(&destination).unwrap(), b"first-result");
}

/// 防止无效来源在替换前破坏已发布的成功结果。
#[test]
fn missing_source_keeps_existing_destination_bytes() {
    let directory = tempdir().unwrap();
    let source = directory.path().join("missing.tsv");
    let destination = directory.path().join("result.tsv");
    fs::write(&destination, b"old-result").unwrap();

    assert!(atomic_replace_file(&source, &destination).is_err());

    assert_eq!(fs::read(&destination).unwrap(), b"old-result");
}

/// 防止目标被独占打开时仍误报发布成功或覆盖原结果。
#[test]
fn locked_destination_rejects_replace_and_keeps_old_bytes() {
    let directory = tempdir().unwrap();
    let source = directory.path().join("partial.tsv");
    let destination = directory.path().join("result.tsv");
    fs::write(&source, b"new-result").unwrap();
    fs::write(&destination, b"old-result").unwrap();
    let handle = open_without_sharing(&destination);

    assert!(atomic_replace_file(&source, &destination).is_err());

    unsafe {
        CloseHandle(handle).unwrap();
    }
    assert_eq!(fs::read(&destination).unwrap(), b"old-result");
}

/// 结果 reader 同时持有来源和旧目标时，仍以来源句柄完成原子替换。
#[test]
fn replaces_open_source_and_destination_by_source_handle() {
    let directory = tempdir().unwrap();
    let source = directory.path().join("partial.tsv");
    let destination = directory.path().join("result.tsv");
    fs::write(&source, b"new-result").unwrap();
    fs::write(&destination, b"old-result").unwrap();
    let mut source_reader = open_shared_delete_reader(&source);
    let mut old_reader = open_shared_delete_reader(&destination);

    atomic_replace_file_from_handle(&source_reader, &source, &destination).unwrap();

    assert!(!source.exists());
    source_reader.seek(SeekFrom::Start(0)).unwrap();
    old_reader.seek(SeekFrom::Start(0)).unwrap();
    let mut source_bytes = Vec::new();
    let mut old_bytes = Vec::new();
    source_reader.read_to_end(&mut source_bytes).unwrap();
    old_reader.read_to_end(&mut old_bytes).unwrap();
    assert_eq!(source_bytes, b"new-result");
    assert_eq!(old_bytes, b"old-result");
    drop(source_reader);
    drop(old_reader);
    assert_eq!(fs::read(&destination).unwrap(), b"new-result");
}

/// 使用 share=0 模拟 Windows 中正在被其他进程独占读取的结果文件。
fn open_without_sharing(path: &Path) -> windows::Win32::Foundation::HANDLE {
    let wide = path
        .as_os_str()
        .encode_wide()
        .chain(std::iter::once(0))
        .collect::<Vec<_>>();
    unsafe {
        CreateFileW(
            PCWSTR(wide.as_ptr()),
            GENERIC_READ.0,
            FILE_SHARE_MODE(0),
            None,
            OPEN_EXISTING,
            FILE_ATTRIBUTE_NORMAL,
            None,
        )
        .unwrap()
    }
}

/// 以结果 reader 所需的共享删除和 DELETE 权限打开文件。
fn open_shared_delete_reader(path: &Path) -> fs::File {
    fs::OpenOptions::new()
        .read(true)
        .access_mode(0x8001_0000)
        .share_mode(0x0001 | 0x0002 | 0x0004)
        .open(path)
        .unwrap()
}

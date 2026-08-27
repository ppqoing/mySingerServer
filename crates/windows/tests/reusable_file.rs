//! 可复用 OVERLAPPED 文件句柄的随机读取契约。

#![cfg(windows)]

use std::time::Duration;

use dedup_windows::{ReadCancellationToken, ReusableOverlappedFile};

#[test]
fn reusable_file_keeps_one_handle_for_multiple_offsets() {
    let directory = tempfile::tempdir().unwrap();
    let original = directory.path().join("media.bin");
    let moved = directory.path().join("renamed.bin");
    std::fs::write(&original, b"0123456789abcdef").unwrap();

    let mut file = ReusableOverlappedFile::open(&original).unwrap();
    assert_eq!(file.len(), 16);
    std::fs::rename(&original, &moved).unwrap();

    let cancellation = ReadCancellationToken::new();
    let mut first = [0_u8; 4];
    let mut second = [0_u8; 4];
    assert_eq!(
        file.read_at(0, &mut first, Duration::from_secs(3), &cancellation,)
            .unwrap(),
        4
    );
    assert_eq!(
        file.read_at(8, &mut second, Duration::from_secs(3), &cancellation,)
            .unwrap(),
        4
    );
    assert_eq!(&first, b"0123");
    assert_eq!(&second, b"89ab");
}

//! Worker 文件会话在路径变化后仍复用原句柄完成媒体随机读取。

#![cfg(windows)]

use std::io::SeekFrom;

use dedup_node_engine::worker::{WorkerFileSession, WorkerReadLimits};

#[test]
fn one_worker_file_session_reuses_open_handle_for_media_reads() {
    let directory = tempfile::tempdir().unwrap();
    let original = directory.path().join("media.bin");
    let moved = directory.path().join("renamed.bin");
    let bytes = b"0123456789abcdef";
    std::fs::write(&original, bytes).unwrap();

    let mut session =
        WorkerFileSession::open(&original, WorkerReadLimits::new(4, 3_000, 2).unwrap()).unwrap();
    std::fs::rename(&original, &moved).unwrap();

    let source = session.media_source();
    source.seek(SeekFrom::Start(4)).unwrap();
    let mut buffer = [0_u8; 8];
    assert_eq!(source.read(&mut buffer).unwrap(), 8);
    assert_eq!(&buffer, b"456789ab");
}

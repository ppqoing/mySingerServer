use std::fs;

use dedup_core::DisplayPath;
use dedup_node_engine::scan::{EverythingEnumerator, FileEnumerator, WindowsWalker};
use tempfile::tempdir;

#[test]
fn windows_walker_returns_all_files_in_stable_normalized_order() {
    let directory = tempdir().unwrap();
    fs::create_dir(directory.path().join("nested")).unwrap();
    fs::write(directory.path().join("z.jpg"), b"image").unwrap();
    fs::write(directory.path().join("nested").join("a.mp4"), b"video").unwrap();
    fs::write(directory.path().join("plain.txt"), b"plain").unwrap();
    fs::write(directory.path().join("unknown.nomatch"), b"unknown").unwrap();

    let rows = WindowsWalker
        .enumerate(&[DisplayPath::new(directory.path()).unwrap()])
        .unwrap();
    let keys = rows
        .iter()
        .map(|row| row.normalized_path.as_str().to_owned())
        .collect::<Vec<_>>();

    assert_eq!(rows.len(), 4);
    assert!(keys.windows(2).all(|pair| pair[0] < pair[1]));
    assert!(
        rows.iter()
            .all(|row| row.display_path.as_path().is_absolute())
    );
    assert_eq!(rows.iter().map(|row| row.file_size).sum::<u64>(), 22);
}

#[test]
fn everything_has_the_same_sorted_output_contract_when_ipc_is_available() {
    let directory = tempdir().unwrap();
    fs::write(directory.path().join("indexed.txt"), b"fixture").unwrap();
    let root = DisplayPath::new(directory.path()).unwrap();

    let rows = match EverythingEnumerator.enumerate(&[root]) {
        Ok(rows) => rows,
        Err(error) => {
            // Everything 是显式配置依赖；当前机未运行时返回单一明确错误，任务不回退。
            assert!(error.to_string().contains("Everything IPC 不可用"));
            return;
        }
    };
    assert!(
        rows.windows(2)
            .all(|pair| pair[0].normalized_path < pair[1].normalized_path)
    );
    assert!(
        rows.iter()
            .all(|row| row.display_path.as_path().is_absolute())
    );
}

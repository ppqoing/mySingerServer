use std::{collections::BTreeSet, fs};

use dedup_core::{DisplayPath, NodeConfig};
use dedup_node_engine::scan::{
    EverythingEnumerator, FileEnumerator, FilteredWindowsWalker, MediaExtensionFilter,
    WindowsWalker,
};
use dedup_node_store::ScannedPath;
use tempfile::tempdir;

/// 断言枚举结果保持全局规范排序且每个规范路径仅出现一次。
fn assert_globally_sorted_and_unique(rows: &[ScannedPath]) {
    let keys = rows
        .iter()
        .map(|row| row.normalized_path.as_str())
        .collect::<Vec<_>>();
    assert!(keys.windows(2).all(|pair| pair[0] < pair[1]));
    assert_eq!(
        keys.iter().copied().collect::<BTreeSet<_>>().len(),
        rows.len()
    );
}

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
fn filtered_walker_only_returns_configured_extensions() {
    let directory = tempdir().unwrap();
    fs::write(directory.path().join("photo.JPG"), b"image").unwrap();
    fs::write(directory.path().join("movie.mp4"), b"video").unwrap();
    fs::write(directory.path().join("notes.txt"), b"text").unwrap();
    fs::write(directory.path().join("README"), b"none").unwrap();
    let mut config = NodeConfig::default();
    config.image_extensions = vec!["jpg".into()];
    config.video_extensions = vec!["mp4".into()];
    let walker = FilteredWindowsWalker::new(MediaExtensionFilter::from_config(&config));

    let rows = walker
        .enumerate(&[DisplayPath::new(directory.path()).unwrap()])
        .unwrap();
    let names = rows
        .iter()
        .map(|row| {
            row.display_path
                .as_path()
                .file_name()
                .unwrap()
                .to_string_lossy()
                .into_owned()
        })
        .collect::<Vec<_>>();

    assert_eq!(names, ["movie.mp4", "photo.JPG"]);
}

#[test]
fn empty_filter_returns_no_walker_rows() {
    let directory = tempdir().unwrap();
    fs::write(directory.path().join("photo.jpg"), b"image").unwrap();
    let mut config = NodeConfig::default();
    config.image_extensions.clear();
    config.video_extensions.clear();
    let walker = FilteredWindowsWalker::new(MediaExtensionFilter::from_config(&config));

    assert!(
        walker
            .enumerate(&[DisplayPath::new(directory.path()).unwrap()])
            .unwrap()
            .is_empty()
    );
}

#[test]
fn everything_has_the_same_sorted_output_contract_when_ipc_is_available() {
    let directory = tempdir().unwrap();
    fs::write(directory.path().join("indexed.jpg"), b"fixture").unwrap();
    let root = DisplayPath::new(directory.path()).unwrap();
    let filter = MediaExtensionFilter::from_config(&NodeConfig::default());

    let rows = match EverythingEnumerator::new(filter).enumerate(&[root]) {
        Ok(rows) => rows,
        Err(error) => {
            // 原始 IPC 适配器保持严格错误；Node 上层负责把整次扫描回退到 Walker。
            assert!(error.to_string().contains("Everything IPC 不可用"));
            eprintln!("SKIP_EVERYTHING_IPC_UNAVAILABLE: {error}");
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

#[test]
fn windows_walker_keeps_two_roots_globally_sorted_and_unique() {
    let first_root = tempdir().unwrap();
    let second_root = tempdir().unwrap();
    fs::create_dir(first_root.path().join("nested")).unwrap();
    fs::write(first_root.path().join("z.bin"), b"first-z").unwrap();
    fs::write(first_root.path().join("nested/a.bin"), b"first-a").unwrap();
    fs::write(second_root.path().join("b.bin"), b"second-b").unwrap();
    fs::write(second_root.path().join("a.bin"), b"second-a").unwrap();
    let roots = [
        DisplayPath::new(first_root.path()).unwrap(),
        DisplayPath::new(second_root.path()).unwrap(),
    ];

    let rows = WindowsWalker.enumerate(&roots).unwrap();

    assert_eq!(rows.len(), 4);
    assert_globally_sorted_and_unique(&rows);
}

#[test]
fn everything_keeps_two_roots_globally_sorted_and_unique_when_ipc_is_available() {
    let first_root = tempdir().unwrap();
    let second_root = tempdir().unwrap();
    fs::write(first_root.path().join("indexed-a.jpg"), b"first").unwrap();
    fs::write(second_root.path().join("indexed-b.mp4"), b"second").unwrap();
    let roots = [
        DisplayPath::new(first_root.path()).unwrap(),
        DisplayPath::new(second_root.path()).unwrap(),
    ];

    let filter = MediaExtensionFilter::from_config(&NodeConfig::default());
    let rows = match EverythingEnumerator::new(filter).enumerate(&roots) {
        Ok(rows) => rows,
        Err(error) => {
            // Everything 不可用时沿既有契约跳过，禁止为测试启动全盘索引。
            assert!(error.to_string().contains("Everything IPC 不可用"));
            eprintln!("SKIP_EVERYTHING_IPC_UNAVAILABLE: {error}");
            return;
        }
    };

    assert_globally_sorted_and_unique(&rows);
}

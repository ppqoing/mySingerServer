//! Node 本机路径的原始字符串与实际访问路径边界。

use std::{fs, path::Path};

use dedup_windows::LocalNodePath;

#[test]
fn relative_path_keeps_its_raw_string_and_resolves_from_the_executable_directory() {
    let directory = tempfile::tempdir().unwrap();
    let executable_dir = directory.path().join("portable");
    fs::create_dir_all(&executable_dir).unwrap();

    let path = LocalNodePath::validate(&executable_dir, r"data\node\cache").unwrap();

    assert_eq!(path.raw(), r"data\node\cache");
    assert_eq!(path.resolved(), executable_dir.join(r"data\node\cache"));
}

#[test]
fn absolute_local_path_is_preserved_without_executable_directory_prefixing() {
    let directory = tempfile::tempdir().unwrap();
    let executable_dir = directory.path().join("portable");
    fs::create_dir_all(&executable_dir).unwrap();
    let absolute = directory.path().join("custom-data");

    let path = LocalNodePath::validate(&executable_dir, absolute.to_str().unwrap()).unwrap();

    assert_eq!(path.raw(), absolute.to_str().unwrap());
    assert_eq!(path.resolved(), Path::new(&absolute));
}

#[test]
fn empty_and_unc_paths_are_rejected_before_node_configuration_is_saved() {
    let directory = tempfile::tempdir().unwrap();

    assert!(LocalNodePath::validate(directory.path(), "").is_err());
    assert!(LocalNodePath::validate(directory.path(), "   ").is_err());
    assert!(LocalNodePath::validate(directory.path(), r"\\server\share\node").is_err());
    assert!(LocalNodePath::validate(directory.path(), r"\\?\UNC\server\share\node").is_err());
}

#[test]
fn drive_relative_and_rooted_relative_paths_cannot_bypass_the_executable_directory_base() {
    let directory = tempfile::tempdir().unwrap();

    assert!(LocalNodePath::validate(directory.path(), r"C:cache").is_err());
    assert!(LocalNodePath::validate(directory.path(), r"\cache").is_err());
}

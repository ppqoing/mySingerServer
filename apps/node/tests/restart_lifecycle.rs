//! node.exe 启动时的默认配置初始化与只读 bootstrap 门禁。

#[path = "../src/restart_lifecycle.rs"]
mod restart_lifecycle;

use std::{
    fs,
    path::PathBuf,
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::NodeConfig;
use dedup_windows::AppLayout;
use restart_lifecycle::load_or_initialize_node_config;

#[test]
fn first_start_creates_default_bootstrap_then_returns_resolved_paths() {
    let directory = TestDirectory::new("bootstrap-default");
    let executable = directory.path().join("node.exe");
    let layout = AppLayout::from_executable(&executable).unwrap();

    let loaded = load_or_initialize_node_config(&layout).unwrap();

    assert_eq!(loaded.config.paths.config_path, "data/node/config.toml");
    assert_eq!(
        loaded.resolved.data_path,
        directory.path().join("data/node")
    );
    assert_eq!(
        loaded.resolved.log_path,
        directory.path().join("data/node/logs")
    );
    assert_eq!(
        loaded.resolved.cache_path,
        directory.path().join("data/node/cache")
    );
    assert!(layout.node_bootstrap().exists());
    assert!(layout.node_config().exists());
}

#[test]
fn existing_config_and_bootstrap_are_not_rewritten_on_start() {
    let directory = TestDirectory::new("bootstrap-existing");
    let executable = directory.path().join("node.exe");
    let layout = AppLayout::from_executable(&executable).unwrap();
    fs::create_dir_all(layout.node_root()).unwrap();
    fs::write(
        layout.node_config(),
        NodeConfig::default().to_toml().unwrap(),
    )
    .unwrap();
    fs::write(
        layout.node_bootstrap(),
        b"config_path = \"data/node/config.toml\"\n# preserve this file\n",
    )
    .unwrap();
    let config_bytes = fs::read(layout.node_config()).unwrap();
    let bootstrap_bytes = fs::read(layout.node_bootstrap()).unwrap();

    load_or_initialize_node_config(&layout).unwrap();

    assert_eq!(fs::read(layout.node_config()).unwrap(), config_bytes);
    assert_eq!(fs::read(layout.node_bootstrap()).unwrap(), bootstrap_bytes);
}

struct TestDirectory(PathBuf);

impl TestDirectory {
    /// 创建测试专用的临时便携目录。
    fn new(label: &str) -> Self {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let path = std::env::temp_dir().join(format!(
            "mysingerserver-node-config-{label}-{}-{nonce}",
            std::process::id()
        ));
        fs::create_dir_all(&path).unwrap();
        Self(path)
    }

    /// 返回临时目录路径。
    fn path(&self) -> &std::path::Path {
        &self.0
    }
}

impl Drop for TestDirectory {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

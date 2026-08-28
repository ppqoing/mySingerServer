//! node.exe 的默认配置初始化与仓库加载。

use std::fs;

use dedup_core::NodeConfig;
use dedup_node_engine::config_repository::{LoadedNodeConfig, NodeConfigRepository};
use dedup_windows::AppLayout;

/// 首次创建固定默认 bootstrap/config，随后只通过仓库加载配置与解析路径。
pub fn load_or_initialize_node_config(
    layout: &AppLayout,
) -> Result<LoadedNodeConfig, Box<dyn std::error::Error>> {
    if !layout.node_bootstrap().exists() {
        fs::create_dir_all(layout.node_root())?;
        if !layout.node_config().exists() {
            fs::write(layout.node_config(), NodeConfig::default().to_toml()?)?;
        }
        fs::write(
            layout.node_bootstrap(),
            "config_path = \"data/node/config.toml\"\n",
        )?;
    }
    Ok(NodeConfigRepository::from_layout(layout).load()?)
}

//! 桌面端和节点 TOML 配置的强类型边界。

use std::{fmt, net::IpAddr};

use serde::{Deserialize, Serialize};

use crate::{CoreError, DeleteMode, EnumeratorKind, Thresholds};

/// 管理工具手工维护的节点 IP 和端口。
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(default)]
pub struct NodeEndpoint {
    /// 节点监听的 IPv4 或 IPv6 地址。
    pub ip: IpAddr,
    /// 节点 TCP 监听端口。
    pub port: u16,
}

impl Default for NodeEndpoint {
    fn default() -> Self {
        Self {
            ip: IpAddr::from([127, 0, 0, 1]),
            port: 39091,
        }
    }
}

impl fmt::Display for NodeEndpoint {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        std::net::SocketAddr::new(self.ip, self.port).fmt(formatter)
    }
}

/// `desktop.exe` 保存在 `data/desktop/config.toml` 的配置。
#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(default)]
pub struct DesktopConfig {
    /// 手工配置并由管理工具并行连接的节点列表。
    pub nodes: Vec<NodeEndpoint>,
    /// 用户提供的 PostgreSQL NoTls 连接串；为空时只启用本地功能。
    pub postgres_url: Option<String>,
    /// 每次分析创建时完整复制的相似度阈值。
    pub thresholds: Thresholds,
    /// 删除确认框默认使用的文件处理方式。
    pub delete_mode: DeleteMode,
    /// 节点断线后的固定重连间隔秒数。
    pub reconnect_interval_seconds: u64,
}

impl Default for DesktopConfig {
    fn default() -> Self {
        Self {
            nodes: vec![NodeEndpoint::default()],
            postgres_url: None,
            thresholds: Thresholds::default(),
            delete_mode: DeleteMode::default(),
            reconnect_interval_seconds: 5,
        }
    }
}

impl DesktopConfig {
    /// 从 TOML 解码并在唯一配置边界完成字段验证。
    pub fn from_toml(text: &str) -> Result<Self, CoreError> {
        let config: Self = toml::from_str(text)?;
        config.validate()?;
        Ok(config)
    }

    /// 验证端口、重连间隔和完整阈值快照。
    pub fn validate(&self) -> Result<(), CoreError> {
        if self.nodes.iter().any(|node| node.port == 0) {
            return Err(invalid_config("nodes.port", "端口不能为 0"));
        }
        if self.reconnect_interval_seconds == 0 {
            return Err(invalid_config(
                "reconnect_interval_seconds",
                "重连间隔必须大于 0",
            ));
        }
        self.thresholds.validate()
    }

    /// 编码为可直接写入配置文件的 TOML 文本。
    pub fn to_toml(&self) -> Result<String, CoreError> {
        self.validate()?;
        Ok(toml::to_string_pretty(self)?)
    }
}

/// `node.exe` 保存在 `data/node/config.toml` 的配置。
#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(default)]
pub struct NodeConfig {
    /// 节点监听的 IPv4 或 IPv6 地址。
    pub listen_ip: IpAddr,
    /// 节点 TCP 监听端口。
    pub port: u16,
    /// 同时运行的媒体 Worker 数量。
    pub worker_count: usize,
    /// 扫描目录时选择的文件枚举方式。
    pub enumerator: EnumeratorKind,
}

impl Default for NodeConfig {
    fn default() -> Self {
        Self {
            listen_ip: IpAddr::from([127, 0, 0, 1]),
            port: 39091,
            worker_count: std::thread::available_parallelism()
                .map(usize::from)
                .unwrap_or(1),
            enumerator: EnumeratorKind::default(),
        }
    }
}

impl NodeConfig {
    /// 从 TOML 解码并在节点启动边界验证配置。
    pub fn from_toml(text: &str) -> Result<Self, CoreError> {
        let config: Self = toml::from_str(text)?;
        config.validate()?;
        Ok(config)
    }

    /// 验证节点启动必需的端口和 Worker 数量。
    pub fn validate(&self) -> Result<(), CoreError> {
        if self.port == 0 {
            return Err(invalid_config("port", "端口不能为 0"));
        }
        if self.worker_count == 0 {
            return Err(invalid_config("worker_count", "Worker 数量必须大于 0"));
        }
        Ok(())
    }

    /// 编码为可直接写入节点配置文件的 TOML 文本。
    pub fn to_toml(&self) -> Result<String, CoreError> {
        self.validate()?;
        Ok(toml::to_string_pretty(self)?)
    }
}

const fn invalid_config(field: &'static str, reason: &'static str) -> CoreError {
    CoreError::InvalidConfig { field, reason }
}

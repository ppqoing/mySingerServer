//! 桌面端和节点 TOML 配置的强类型边界。

use std::{fmt, net::IpAddr};

use serde::{Deserialize, Serialize};

use crate::{CoreError, DeleteMode, EnumeratorKind, Thresholds};

/// 单块 HDD、SSD 或未知本地盘允许的最大读取线程数。
pub const MAX_READ_THREADS_PER_DISK: usize = 64;
/// 全部本地物理磁盘合计允许的最大读取线程数。
pub const MAX_TOTAL_READ_THREADS: usize = 256;
/// 手动模式允许的最大 Worker 进程数。
pub const MAX_MANUAL_WORKER_COUNT: usize = 256;
/// 自动模式允许保留的最大逻辑核心数。
pub const MAX_RESERVED_CORES: usize = 255;
/// Node 连接中心 PostgreSQL 时允许的最大连接超时秒数。
pub const MAX_POSTGRES_CONNECT_TIMEOUT_SECONDS: u64 = 60;

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
    ///
    /// 此字段保留到 Node 启动路径切换为 `worker` 配置为止，避免本任务提前修改运行时装配。
    pub worker_count: usize,
    /// 扫描目录时选择的文件枚举方式。
    pub enumerator: EnumeratorKind,
    /// 扫描允许的图片扩展名；值不含前导点并使用小写。
    pub image_extensions: Vec<String>,
    /// 扫描允许的视频扩展名；值不含前导点并使用小写。
    pub video_extensions: Vec<String>,
    /// 节点运行数据、配置、日志和缓存的原始路径字符串。
    pub paths: NodePathsConfig,
    /// Node 控制的磁盘读取并发和块读取参数。
    pub read: DiskReadConfig,
    /// Worker 自动或手动模式参数。
    pub worker: WorkerConfig,
    /// Node 可选的中心 PostgreSQL 基础连接参数。
    pub postgres: NodePostgresConfig,
}

impl Default for NodeConfig {
    fn default() -> Self {
        Self {
            listen_ip: IpAddr::from([127, 0, 0, 1]),
            port: 39091,
            worker_count: std::thread::available_parallelism()
                .map(usize::from)
                .unwrap_or(1),
            enumerator: EnumeratorKind::Everything,
            image_extensions: owned_extensions(DEFAULT_IMAGE_EXTENSIONS),
            video_extensions: owned_extensions(DEFAULT_VIDEO_EXTENSIONS),
            paths: NodePathsConfig::default(),
            read: DiskReadConfig::default(),
            worker: WorkerConfig::default(),
            postgres: NodePostgresConfig::default(),
        }
    }
}

impl NodeConfig {
    /// 从 TOML 解码并在节点启动边界验证配置。
    pub fn from_toml(text: &str) -> Result<Self, CoreError> {
        let config: Self = toml::from_str(text)?;
        config.normalized()
    }

    /// 规范化扩展名并验证完整 Node 配置，供 TOML、协议和 UI 保存边界复用。
    pub fn normalized(mut self) -> Result<Self, CoreError> {
        normalize_extensions(&mut self.image_extensions, "image_extensions")?;
        normalize_extensions(&mut self.video_extensions, "video_extensions")?;
        self.validate()?;
        Ok(self)
    }

    /// 验证节点启动必需的端口、读取和 Worker 参数。
    pub fn validate(&self) -> Result<(), CoreError> {
        if self.port == 0 {
            return Err(invalid_config("port", "端口不能为 0"));
        }
        if self.worker_count == 0 {
            return Err(invalid_config("worker_count", "Worker 数量必须大于 0"));
        }
        if self.worker_count > MAX_MANUAL_WORKER_COUNT {
            return Err(invalid_config("worker_count", "Worker 数量不能超过 256"));
        }
        validate_extensions(&self.image_extensions, "image_extensions")?;
        validate_extensions(&self.video_extensions, "video_extensions")?;
        self.read.validate()?;
        self.worker.validate()?;
        self.postgres.validate()?;
        Ok(())
    }

    /// 编码为可直接写入节点配置文件的 TOML 文本。
    pub fn to_toml(&self) -> Result<String, CoreError> {
        let normalized = self.clone().normalized()?;
        Ok(toml::to_string_pretty(&normalized)?)
    }
}

/// 当前产品声明支持的图片扩展名全集。
const DEFAULT_IMAGE_EXTENSIONS: &[&str] = &[
    "apng", "avif", "bmp", "cur", "dds", "dib", "dpx", "exr", "fits", "gif", "hdr", "heic", "heif",
    "ico", "j2c", "j2k", "jfif", "jls", "jp2", "jpc", "jpe", "jpeg", "jpg", "jxl", "pam", "pbm",
    "pcd", "pcx", "pfm", "pgm", "pgx", "png", "pnm", "ppm", "psd", "qoi", "ras", "sgi", "svg",
    "tga", "tif", "tiff", "webp", "xbm", "xpm", "xwd",
];

/// 当前产品声明支持的视频扩展名全集。
const DEFAULT_VIDEO_EXTENSIONS: &[&str] = &[
    "264", "265", "266", "3g2", "3gp", "amv", "apv", "asf", "av1", "avc", "avi", "bik", "bink",
    "cdxl", "dav", "dif", "divx", "dv", "evc", "evo", "f4v", "flm", "flv", "gxf", "h261", "h263",
    "h264", "h265", "h266", "hevc", "ifv", "ismv", "ivf", "kux", "lvf", "m1v", "m2t", "m2ts",
    "m2v", "m4v", "mj2", "mjpeg", "mjpg", "mk3d", "mkv", "moflex", "mov", "mp4", "mpe", "mpeg",
    "mpg", "mts", "mxf", "nsv", "nut", "nuv", "obu", "ogm", "ogv", "pdv", "qt", "r3d", "rm",
    "rmvb", "roq", "rpl", "ser", "smjpeg", "smk", "str", "swf", "ts", "ty", "usm", "vc1", "viv",
    "vivo", "vob", "vvc", "webm", "wmv", "wtv", "xmv", "y4m", "yop",
];

/// 把只读默认值复制为配置拥有的字符串数组。
fn owned_extensions(defaults: &[&str]) -> Vec<String> {
    defaults
        .iter()
        .map(|extension| (*extension).to_owned())
        .collect()
}

/// 把用户扩展名转换为稳定的小写无点形式，并在一个列表内去重。
fn normalize_extensions(
    extensions: &mut Vec<String>,
    field: &'static str,
) -> Result<(), CoreError> {
    for extension in extensions.iter_mut() {
        *extension = extension
            .trim()
            .trim_start_matches('.')
            .to_ascii_lowercase();
    }
    validate_extensions(extensions, field)?;
    extensions.sort_unstable();
    extensions.dedup();
    Ok(())
}

/// 校验扩展名只包含可安全拼入 Everything `ext:` 查询的字符。
fn validate_extensions(extensions: &[String], field: &'static str) -> Result<(), CoreError> {
    if extensions.iter().any(|extension| {
        extension.is_empty()
            || !extension.bytes().all(|byte| {
                byte.is_ascii_lowercase()
                    || byte.is_ascii_digit()
                    || matches!(byte, b'_' | b'+' | b'-')
            })
    }) {
        return Err(CoreError::InvalidConfig {
            field,
            reason: "扩展名只能包含小写 ASCII 字母、数字、_、+、-",
        });
    }
    Ok(())
}

/// Node 可选的中心 PostgreSQL 基础连接参数。
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(default)]
pub struct NodePostgresConfig {
    /// 是否启用中心 PostgreSQL；关闭时 Node 只使用本地 SQLite。
    pub enabled: bool,
    /// PostgreSQL 主机名或 IP 地址。
    pub host: String,
    /// PostgreSQL TCP 端口。
    pub port: u16,
    /// PostgreSQL 数据库名。
    pub database: String,
    /// PostgreSQL 用户名。
    pub username: String,
    /// PostgreSQL 密码，配置往返时必须保持原值。
    pub password: String,
    /// 建立 PostgreSQL 连接允许等待的秒数。
    pub connect_timeout_seconds: u64,
}

impl Default for NodePostgresConfig {
    fn default() -> Self {
        Self {
            enabled: false,
            host: "127.0.0.1".to_owned(),
            port: 5432,
            database: "media_dedup".to_owned(),
            username: "postgres".to_owned(),
            password: String::new(),
            connect_timeout_seconds: 3,
        }
    }
}

impl NodePostgresConfig {
    /// 仅在启用 PostgreSQL 时验证连接必需字段和超时边界。
    fn validate(&self) -> Result<(), CoreError> {
        if !self.enabled {
            return Ok(());
        }
        if self.host.trim().is_empty() {
            return Err(invalid_config("postgres.host", "主机不能为空"));
        }
        if self.port == 0 {
            return Err(invalid_config("postgres.port", "端口不能为 0"));
        }
        if self.database.trim().is_empty() {
            return Err(invalid_config("postgres.database", "数据库名不能为空"));
        }
        if self.username.trim().is_empty() {
            return Err(invalid_config("postgres.username", "用户名不能为空"));
        }
        if !(1..=MAX_POSTGRES_CONNECT_TIMEOUT_SECONDS).contains(&self.connect_timeout_seconds) {
            return Err(invalid_config(
                "postgres.connect_timeout_seconds",
                "连接超时必须在 1 到 60 秒之间",
            ));
        }
        Ok(())
    }
}

/// Node 本地运行目录的原始路径配置。
///
/// 相对路径和绝对路径都按原样保留；路径解析和本地磁盘验证由后续 Windows 边界负责。
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(default)]
pub struct NodePathsConfig {
    /// Node 数据目录的原始配置字符串。
    pub data_path: String,
    /// Node 配置文件的原始配置字符串。
    pub config_path: String,
    /// Node 日志目录的原始配置字符串。
    pub log_path: String,
    /// Node 缓存目录的原始配置字符串。
    pub cache_path: String,
}

impl Default for NodePathsConfig {
    fn default() -> Self {
        Self {
            data_path: "data/node".to_owned(),
            config_path: "data/node/config.toml".to_owned(),
            log_path: "data/node/logs".to_owned(),
            cache_path: "data/node/cache".to_owned(),
        }
    }
}

/// 按物理磁盘类别限制的 Node 文件读取参数。
#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(default)]
pub struct DiskReadConfig {
    /// 每块 HDD 同时允许的文件级读取数。
    pub hdd_threads_per_disk: usize,
    /// 每块 SSD 同时允许的文件级读取数。
    pub ssd_threads_per_disk: usize,
    /// 每块未知本地物理盘同时允许的文件级读取数。
    pub unknown_threads_per_disk: usize,
    /// 所有物理磁盘合计允许的读取数。
    pub total_threads: usize,
    /// 一次流式读取使用的块大小（字节）。
    pub block_size_bytes: usize,
    /// 单个读取块的超时秒数。
    pub block_timeout_seconds: u64,
    /// 单个读取块超时后的重试次数。
    pub block_retries: u32,
}

impl Default for DiskReadConfig {
    fn default() -> Self {
        Self {
            hdd_threads_per_disk: 1,
            ssd_threads_per_disk: 2,
            unknown_threads_per_disk: 1,
            total_threads: 4,
            block_size_bytes: 4 * 1024 * 1024,
            block_timeout_seconds: 3,
            block_retries: 2,
        }
    }
}

impl DiskReadConfig {
    fn validate(&self) -> Result<(), CoreError> {
        if self.hdd_threads_per_disk == 0 {
            return Err(invalid_config(
                "read.hdd_threads_per_disk",
                "每块 HDD 的读取线程数必须大于 0",
            ));
        }
        if self.hdd_threads_per_disk > MAX_READ_THREADS_PER_DISK {
            return Err(invalid_config(
                "read.hdd_threads_per_disk",
                "每块 HDD 的读取线程数不能超过 64",
            ));
        }
        if self.ssd_threads_per_disk == 0 {
            return Err(invalid_config(
                "read.ssd_threads_per_disk",
                "每块 SSD 的读取线程数必须大于 0",
            ));
        }
        if self.ssd_threads_per_disk > MAX_READ_THREADS_PER_DISK {
            return Err(invalid_config(
                "read.ssd_threads_per_disk",
                "每块 SSD 的读取线程数不能超过 64",
            ));
        }
        if self.unknown_threads_per_disk == 0 {
            return Err(invalid_config(
                "read.unknown_threads_per_disk",
                "未知盘的读取线程数必须大于 0",
            ));
        }
        if self.unknown_threads_per_disk > MAX_READ_THREADS_PER_DISK {
            return Err(invalid_config(
                "read.unknown_threads_per_disk",
                "未知盘的读取线程数不能超过 64",
            ));
        }
        if self.total_threads == 0 {
            return Err(invalid_config(
                "read.total_threads",
                "总读取线程数必须大于 0",
            ));
        }
        if self.total_threads > MAX_TOTAL_READ_THREADS {
            return Err(invalid_config(
                "read.total_threads",
                "总读取线程数不能超过 256",
            ));
        }
        if !(64 * 1024..=64 * 1024 * 1024).contains(&self.block_size_bytes) {
            return Err(invalid_config(
                "read.block_size_bytes",
                "读取块大小必须在 64 KiB 到 64 MiB 之间",
            ));
        }
        if !(1..=60).contains(&self.block_timeout_seconds) {
            return Err(invalid_config(
                "read.block_timeout_seconds",
                "读取块超时必须在 1 到 60 秒之间",
            ));
        }
        if self.block_retries > 10 {
            return Err(invalid_config(
                "read.block_retries",
                "读取块重试次数不能超过 10",
            ));
        }
        Ok(())
    }
}

/// Worker 进程数量的计算方式。
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerMode {
    /// 按逻辑 CPU 数扣除保留核心后自动计算。
    Automatic,
    /// 使用用户明确提供的 Worker 数量。
    Manual,
}

/// Node Worker 的自动或手动数量配置。
#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(default)]
pub struct WorkerConfig {
    /// Worker 数量的计算方式。
    pub mode: WorkerMode,
    /// 自动模式下不用于 Worker 的逻辑核心数。
    pub reserved_cores: usize,
    /// 手动模式下明确启动的 Worker 数量。
    pub manual_worker_count: usize,
}

impl Default for WorkerConfig {
    fn default() -> Self {
        Self {
            mode: WorkerMode::Automatic,
            reserved_cores: 1,
            manual_worker_count: 1,
        }
    }
}

impl WorkerConfig {
    /// 根据模式和检测到的逻辑 CPU 数返回至少为一的有效 Worker 数。
    pub fn effective_worker_count(&self, logical_cpus: usize) -> usize {
        match self.mode {
            WorkerMode::Automatic => logical_cpus.saturating_sub(self.reserved_cores).max(1),
            WorkerMode::Manual => self.manual_worker_count,
        }
    }

    /// 根据逻辑 CPU 和保留核心计算统一 CPU 权重预算；手动 Worker 数不会扩大该预算。
    pub fn effective_cpu_budget(&self, logical_cpus: usize) -> usize {
        logical_cpus.saturating_sub(self.reserved_cores).max(1)
    }

    fn validate(&self) -> Result<(), CoreError> {
        if self.reserved_cores > MAX_RESERVED_CORES {
            return Err(invalid_config(
                "worker.reserved_cores",
                "自动模式保留核心数不能超过 255",
            ));
        }
        if self.manual_worker_count > MAX_MANUAL_WORKER_COUNT {
            return Err(invalid_config(
                "worker.manual_worker_count",
                "手动 Worker 数量不能超过 256",
            ));
        }
        if self.mode == WorkerMode::Manual && self.manual_worker_count == 0 {
            return Err(invalid_config(
                "worker.manual_worker_count",
                "手动 Worker 数量必须大于 0",
            ));
        }
        Ok(())
    }
}

const fn invalid_config(field: &'static str, reason: &'static str) -> CoreError {
    CoreError::InvalidConfig { field, reason }
}

#[cfg(test)]
mod tests {
    use crate::{EnumeratorKind, NodeConfig};

    #[test]
    fn new_node_config_defaults_to_everything() {
        assert_eq!(NodeConfig::default().enumerator, EnumeratorKind::Everything,);
    }
}

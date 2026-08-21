//! Node 配置的 bootstrap 定位、路径验证、摘要和双文件原子替换边界。

use std::{
    fs::{self, File, OpenOptions},
    io::{self, Write},
    path::{Path, PathBuf},
};

use dedup_core::{CoreError, NodeConfig};
use dedup_windows::{AppLayout, LocalNodePath};
use sha2::{Digest, Sha256};
use thiserror::Error;
use uuid::Uuid;

/// Node 配置仓库读写失败的分类。
#[derive(Debug, Error)]
pub enum ConfigRepositoryError {
    /// 配置字段或本机路径未通过既有强类型边界。
    #[error(transparent)]
    Core(#[from] CoreError),
    /// 某个配置文件的读、写、刷新或替换失败。
    #[error("配置文件操作失败 {path}: {source}")]
    Io {
        /// 发生 IO 错误的精确文件路径。
        path: PathBuf,
        /// 底层文件系统错误。
        #[source]
        source: io::Error,
    },
    /// bootstrap 不是只含原始 `config_path` 的 TOML 表。
    #[error("bootstrap.toml 无效: {0}")]
    InvalidBootstrap(&'static str),
    /// bootstrap TOML 无法被解析。
    #[error("bootstrap.toml 解析失败: {0}")]
    BootstrapToml(#[from] toml::de::Error),
    /// bootstrap TOML 无法编码。
    #[error("bootstrap.toml 编码失败: {0}")]
    BootstrapEncode(#[from] toml::ser::Error),
    /// 保存请求基于已过期的完整配置摘要。
    #[error("配置版本冲突，期望 {expected}，当前 {actual}")]
    VersionConflict {
        /// 客户端加载快照时得到的摘要。
        expected: String,
        /// 当前配置文件内容的摘要。
        actual: String,
    },
    /// bootstrap 替换失败后，当前配置也无法恢复到已同步的旧副本。
    #[error("bootstrap 替换失败且配置回滚失败: {rollback}")]
    RollbackFailed {
        /// 回滚配置时的底层错误。
        rollback: io::Error,
    },
}

/// 已加载的原始 Node 配置、版本摘要与仅供本机访问的解析路径。
#[derive(Clone, Debug, PartialEq)]
pub struct LoadedNodeConfig {
    /// 原样解码的 Node 配置。
    pub config: NodeConfig,
    /// 不改变原始字符串的本机解析结果。
    pub resolved: ResolvedNodePaths,
    /// 完整配置 TOML 文件内容的 SHA-256 小写十六进制摘要。
    pub version_sha256: String,
    config_toml: String,
}

/// Node 配置中四个路径的本机访问解析结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResolvedNodePaths {
    /// 数据目录的解析路径。
    pub data_path: PathBuf,
    /// 实际完整配置文件的解析路径。
    pub config_path: PathBuf,
    /// 日志目录的解析路径。
    pub log_path: PathBuf,
    /// 缓存目录的解析路径。
    pub cache_path: PathBuf,
}

/// 固定在 `node.exe` 目录的 bootstrap 与实际 Node 配置文件仓库。
#[derive(Clone, Debug)]
pub struct NodeConfigRepository {
    executable_dir: PathBuf,
}

impl NodeConfigRepository {
    /// 从 `AppLayout` 创建仓库；bootstrap 始终相对同一可执行文件目录固定定位。
    pub fn from_layout(layout: &AppLayout) -> Self {
        Self::new(layout.executable_dir())
    }

    /// 从 `node.exe` 所在目录创建仓库。
    pub fn new(executable_dir: &Path) -> Self {
        Self {
            executable_dir: executable_dir.to_path_buf(),
        }
    }

    /// 读取 bootstrap 所指向的完整配置，并在本机路径边界完成验证。
    pub fn load(&self) -> Result<LoadedNodeConfig, ConfigRepositoryError> {
        let bootstrap = fs::read_to_string(self.bootstrap_path())
            .map_err(|source| self.io_error(self.bootstrap_path(), source))?;
        let config_path_raw = parse_bootstrap(&bootstrap)?;
        let config_path = LocalNodePath::validate(&self.executable_dir, &config_path_raw)?;
        let config_toml = fs::read_to_string(config_path.resolved())
            .map_err(|source| self.io_error(config_path.resolved(), source))?;
        let config = NodeConfig::from_toml(&config_toml)?;
        let resolved = self.resolve_paths(&config)?;

        Ok(LoadedNodeConfig {
            version_sha256: config_sha256(&config_toml),
            config,
            resolved,
            config_toml,
        })
    }

    /// 返回当前可供远程管理端读取的完整配置快照。
    pub fn snapshot(&self) -> Result<LoadedNodeConfig, ConfigRepositoryError> {
        self.load()
    }

    /// 仅在摘要仍匹配时保存完整配置，并在 bootstrap 替换失败时恢复同路径旧配置。
    pub fn save_if_version(
        &self,
        expected_version_sha256: &str,
        config: &NodeConfig,
    ) -> Result<LoadedNodeConfig, ConfigRepositoryError> {
        config.validate()?;
        let resolved = self.resolve_paths(config)?;
        let current = self.load()?;
        if expected_version_sha256 != current.version_sha256 {
            return Err(ConfigRepositoryError::VersionConflict {
                expected: expected_version_sha256.to_owned(),
                actual: current.version_sha256,
            });
        }

        let config_toml = config.to_toml()?;
        let bootstrap_toml = bootstrap_toml(&config.paths.config_path)?;
        let target_config_path = &resolved.config_path;
        let config_temp = write_synced_temp(target_config_path, config_toml.as_bytes())?;
        let bootstrap_temp = match write_synced_temp(&self.bootstrap_path(), bootstrap_toml.as_bytes()) {
            Ok(path) => path,
            Err(error) => {
                remove_if_exists(&config_temp);
                return Err(error);
            }
        };
        let same_config_path = target_config_path == &current.resolved.config_path;
        let rollback_temp = if same_config_path {
            match write_synced_temp(target_config_path, current.config_toml.as_bytes()) {
                Ok(path) => Some(path),
                Err(error) => {
                    remove_if_exists(&config_temp);
                    remove_if_exists(&bootstrap_temp);
                    return Err(error);
                }
            }
        } else {
            None
        };

        if let Err(error) = replace_file(&config_temp, target_config_path) {
            remove_if_exists(&config_temp);
            remove_if_exists(&bootstrap_temp);
            if let Some(rollback_temp) = rollback_temp {
                remove_if_exists(&rollback_temp);
            }
            return Err(self.io_error(target_config_path, error));
        }

        if let Err(bootstrap_error) = replace_file(&bootstrap_temp, &self.bootstrap_path()) {
            remove_if_exists(&bootstrap_temp);
            if let Some(rollback_temp) = rollback_temp {
                if let Err(rollback) = replace_file(&rollback_temp, target_config_path) {
                    return Err(ConfigRepositoryError::RollbackFailed { rollback });
                }
            }
            return Err(self.io_error(self.bootstrap_path(), bootstrap_error));
        }

        if let Some(rollback_temp) = rollback_temp {
            remove_if_exists(&rollback_temp);
        }
        self.load()
    }

    fn bootstrap_path(&self) -> PathBuf {
        self.executable_dir.join("bootstrap.toml")
    }

    fn resolve_paths(&self, config: &NodeConfig) -> Result<ResolvedNodePaths, ConfigRepositoryError> {
        let data_path = LocalNodePath::validate(&self.executable_dir, &config.paths.data_path)?;
        let config_path = LocalNodePath::validate(&self.executable_dir, &config.paths.config_path)?;
        let log_path = LocalNodePath::validate(&self.executable_dir, &config.paths.log_path)?;
        let cache_path = LocalNodePath::validate(&self.executable_dir, &config.paths.cache_path)?;
        Ok(ResolvedNodePaths {
            data_path: data_path.resolved().to_path_buf(),
            config_path: config_path.resolved().to_path_buf(),
            log_path: log_path.resolved().to_path_buf(),
            cache_path: cache_path.resolved().to_path_buf(),
        })
    }

    fn io_error(&self, path: impl AsRef<Path>, source: io::Error) -> ConfigRepositoryError {
        ConfigRepositoryError::Io {
            path: path.as_ref().to_path_buf(),
            source,
        }
    }
}

fn parse_bootstrap(text: &str) -> Result<String, ConfigRepositoryError> {
    let value: toml::Value = toml::from_str(text)?;
    let table = value
        .as_table()
        .ok_or(ConfigRepositoryError::InvalidBootstrap("根节点必须是表"))?;
    if table.len() != 1 {
        return Err(ConfigRepositoryError::InvalidBootstrap(
            "只能包含 config_path",
        ));
    }
    table
        .get("config_path")
        .and_then(toml::Value::as_str)
        .map(str::to_owned)
        .ok_or(ConfigRepositoryError::InvalidBootstrap(
            "config_path 必须是字符串",
        ))
}

fn bootstrap_toml(config_path: &str) -> Result<String, ConfigRepositoryError> {
    let mut table = toml::map::Map::new();
    table.insert(
        "config_path".to_owned(),
        toml::Value::String(config_path.to_owned()),
    );
    Ok(toml::to_string_pretty(&toml::Value::Table(table))?)
}

fn config_sha256(text: &str) -> String {
    format!("{:x}", Sha256::digest(text.as_bytes()))
}

fn write_synced_temp(target: &Path, content: &[u8]) -> Result<PathBuf, ConfigRepositoryError> {
    let parent = target.parent().ok_or_else(|| ConfigRepositoryError::Io {
        path: target.to_path_buf(),
        source: io::Error::new(io::ErrorKind::InvalidInput, "目标路径没有父目录"),
    })?;
    fs::create_dir_all(parent).map_err(|source| ConfigRepositoryError::Io {
        path: parent.to_path_buf(),
        source,
    })?;
    let file_name = target.file_name().ok_or_else(|| ConfigRepositoryError::Io {
        path: target.to_path_buf(),
        source: io::Error::new(io::ErrorKind::InvalidInput, "目标路径没有文件名"),
    })?;
    let temporary = parent.join(format!(
        ".{}.{}.tmp",
        file_name.to_string_lossy(),
        Uuid::new_v4()
    ));
    let mut file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&temporary)
        .map_err(|source| ConfigRepositoryError::Io {
            path: temporary.clone(),
            source,
        })?;
    if let Err(source) = write_and_sync(&mut file, content) {
        drop(file);
        remove_if_exists(&temporary);
        return Err(ConfigRepositoryError::Io {
            path: temporary,
            source,
        });
    }
    Ok(temporary)
}

fn write_and_sync(file: &mut File, content: &[u8]) -> io::Result<()> {
    file.write_all(content)?;
    file.sync_all()
}

fn replace_file(temporary: &Path, destination: &Path) -> io::Result<()> {
    fs::rename(temporary, destination)
}

fn remove_if_exists(path: &Path) {
    if path.exists() {
        let _ = fs::remove_file(path);
    }
}

//! Node 配置的 bootstrap 定位、路径验证、摘要与可恢复双文件事务边界。

use std::{
    collections::HashMap,
    fs::{self, File, OpenOptions},
    io::{self, Write},
    ffi::OsString,
    path::{Component, Path, PathBuf},
    sync::{Arc, Mutex, MutexGuard, OnceLock, Weak},
};

use dedup_core::{CoreError, NodeConfig};
use dedup_windows::{AppLayout, LocalNodePath};
use sha2::{Digest, Sha256};
use thiserror::Error;
use uuid::Uuid;

const BOOTSTRAP_FILE: &str = "bootstrap.toml";
const JOURNAL_FILE: &str = "config-transaction.toml";
const LOCK_FILE: &str = "config.lock";

static TRANSACTION_LOCKS: OnceLock<Mutex<HashMap<String, Weak<Mutex<()>>>>> = OnceLock::new();

/// 保存流程中供集成测试模拟进程突然终止的固定阶段。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ConfigSaveFailpoint {
    /// 新完整配置替换完成，但 bootstrap 尚未替换。
    AfterConfigReplace,
    /// bootstrap 替换完成，但事务 journal 尚未删除。
    AfterBootstrapReplace,
}

/// Node 配置仓库读写失败的分类。
#[derive(Debug, Error)]
pub enum ConfigRepositoryError {
    /// 配置字段或本机路径未通过既有强类型边界。
    #[error(transparent)]
    Core(#[from] CoreError),
    /// 某个配置文件的读、写、刷新、替换或删除失败。
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
    BootstrapToml(#[source] toml::de::Error),
    /// bootstrap TOML 无法编码。
    #[error("bootstrap.toml 编码失败: {0}")]
    BootstrapEncode(#[source] toml::ser::Error),
    /// 未提交事务 journal 无法被解析。
    #[error("配置事务 journal 无效: {0}")]
    InvalidJournal(&'static str),
    /// journal TOML 无法解析。
    #[error("配置事务 journal 解析失败: {0}")]
    JournalToml(#[source] toml::de::Error),
    /// journal TOML 无法编码。
    #[error("配置事务 journal 编码失败: {0}")]
    JournalEncode(#[source] toml::ser::Error),
    /// bootstrap 指向的原始路径与完整配置内部字段不一致。
    #[error("bootstrap config_path {bootstrap} 与配置 config_path {config} 不一致")]
    ConfigPathMismatch {
        /// bootstrap 中的原始配置路径。
        bootstrap: String,
        /// 完整配置中的原始配置路径。
        config: String,
    },
    /// 完整配置试图占用仓库自己的控制文件。
    #[error("配置路径不能使用仓库控制文件 {path}")]
    RepositoryControlPath {
        /// 被拒绝的解析后路径。
        path: PathBuf,
    },
    /// 保存请求基于已过期的完整配置摘要。
    #[error("配置版本冲突，期望 {expected}，当前 {actual}")]
    VersionConflict {
        /// 客户端加载快照时得到的摘要。
        expected: String,
        /// 当前配置文件内容的摘要。
        actual: String,
    },
    /// 测试固定失败点模拟了替换后尚未提交时的进程中断。
    #[error("保存流程在 {0} 模拟中断")]
    SimulatedInterruption(&'static str),
    /// 运行期保存错误后旧状态恢复也失败；journal 被保留，后续加载不会暴露新配置。
    #[error("配置保存失败 {original}，恢复旧状态失败 {recovery}")]
    RecoveryFailed {
        /// 触发立即恢复的原始错误。
        original: Box<ConfigRepositoryError>,
        /// 恢复旧 bootstrap 与配置时的错误。
        recovery: Box<ConfigRepositoryError>,
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
    bootstrap_toml: String,
    bootstrap_config_path: String,
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

#[derive(Debug)]
struct RepositoryState {
    transaction: Arc<Mutex<()>>,
    failpoint: Mutex<Option<ConfigSaveFailpoint>>,
}

/// 固定在 `node.exe` 目录的 bootstrap 与实际 Node 配置文件仓库。
#[derive(Clone, Debug)]
pub struct NodeConfigRepository {
    executable_dir: PathBuf,
    state: Arc<RepositoryState>,
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
            state: Arc::new(RepositoryState {
                transaction: transaction_lock_for(executable_dir),
                failpoint: Mutex::new(None),
            }),
        }
    }

    /// 创建带一次性固定中断点的仓库，仅供事务恢复行为测试。
    pub fn with_failpoint(executable_dir: &Path, failpoint: ConfigSaveFailpoint) -> Self {
        let repository = Self::new(executable_dir);
        *repository.lock_failpoint() = Some(failpoint);
        repository
    }

    /// 读取 bootstrap 所指向的完整配置，并先恢复任何未提交事务。
    pub fn load(&self) -> Result<LoadedNodeConfig, ConfigRepositoryError> {
        let _transaction = self.lock_transaction();
        self.recover_locked()?;
        self.load_locked()
    }

    /// 返回当前可供远程管理端读取的完整配置快照。
    pub fn snapshot(&self) -> Result<LoadedNodeConfig, ConfigRepositoryError> {
        self.load()
    }

    /// 仅在摘要仍匹配时保存完整配置；journal 删除是唯一提交点。
    pub fn save_if_version(
        &self,
        expected_version_sha256: &str,
        config: &NodeConfig,
    ) -> Result<LoadedNodeConfig, ConfigRepositoryError> {
        let _transaction = self.lock_transaction();
        self.recover_locked()?;
        config.validate()?;
        let resolved = self.resolve_paths(config)?;
        let current = self.load_locked()?;
        if expected_version_sha256 != current.version_sha256 {
            return Err(ConfigRepositoryError::VersionConflict {
                expected: expected_version_sha256.to_owned(),
                actual: current.version_sha256,
            });
        }

        let config_toml = config.to_toml()?;
        let bootstrap_toml = bootstrap_toml(&config.paths.config_path)?;
        let config_temp = write_synced_temp(&resolved.config_path, config_toml.as_bytes())?;
        let bootstrap_temp = match write_synced_temp(&self.bootstrap_path(), bootstrap_toml.as_bytes()) {
            Ok(path) => path,
            Err(error) => {
                discard_repository_temp(&config_temp);
                return Err(error);
            }
        };
        let journal = ConfigTransactionJournal::from_loaded(&current, &config.paths.config_path);
        if let Err(error) = self.write_journal(&journal) {
            discard_repository_temp(&config_temp);
            discard_repository_temp(&bootstrap_temp);
            return Err(error);
        }

        if let Err(error) = replace_file(&config_temp, &resolved.config_path) {
            discard_repository_temp(&config_temp);
            discard_repository_temp(&bootstrap_temp);
            return self.recover_after_error(self.io_error(&resolved.config_path, error));
        }
        if self.consume_failpoint(ConfigSaveFailpoint::AfterConfigReplace) {
            return Err(ConfigRepositoryError::SimulatedInterruption("config rename 后"));
        }

        if let Err(error) = replace_file(&bootstrap_temp, &self.bootstrap_path()) {
            discard_repository_temp(&bootstrap_temp);
            return self.recover_after_error(self.io_error(self.bootstrap_path(), error));
        }
        if self.consume_failpoint(ConfigSaveFailpoint::AfterBootstrapReplace) {
            return Err(ConfigRepositoryError::SimulatedInterruption("bootstrap rename 后"));
        }

        if let Err(error) = fs::remove_file(self.journal_path()) {
            return self.recover_after_error(self.io_error(self.journal_path(), error));
        }
        self.load_locked()
    }

    fn load_locked(&self) -> Result<LoadedNodeConfig, ConfigRepositoryError> {
        let bootstrap_toml = fs::read_to_string(self.bootstrap_path())
            .map_err(|source| self.io_error(self.bootstrap_path(), source))?;
        let bootstrap_config_path = parse_bootstrap(&bootstrap_toml)?;
        let config_path = LocalNodePath::validate(&self.executable_dir, &bootstrap_config_path)?;
        self.reject_control_path(config_path.resolved())?;
        let config_toml = fs::read_to_string(config_path.resolved())
            .map_err(|source| self.io_error(config_path.resolved(), source))?;
        let config = NodeConfig::from_toml(&config_toml)?;
        if config.paths.config_path != bootstrap_config_path {
            return Err(ConfigRepositoryError::ConfigPathMismatch {
                bootstrap: bootstrap_config_path,
                config: config.paths.config_path,
            });
        }
        let resolved = self.resolve_paths(&config)?;

        Ok(LoadedNodeConfig {
            version_sha256: config_sha256(&config_toml),
            config,
            resolved,
            config_toml,
            bootstrap_toml,
            bootstrap_config_path,
        })
    }

    fn recover_locked(&self) -> Result<(), ConfigRepositoryError> {
        let journal_path = self.journal_path();
        if !journal_path.exists() {
            return Ok(());
        }
        let journal_toml = fs::read_to_string(&journal_path)
            .map_err(|source| self.io_error(&journal_path, source))?;
        let journal = ConfigTransactionJournal::parse(&journal_toml)?;
        let old_config = LocalNodePath::validate(&self.executable_dir, &journal.old_config_path)?;
        self.reject_control_path(old_config.resolved())?;

        let config_temp = write_synced_temp(old_config.resolved(), journal.old_config_toml.as_bytes())?;
        if let Err(error) = replace_file(&config_temp, old_config.resolved()) {
            discard_repository_temp(&config_temp);
            return Err(self.io_error(old_config.resolved(), error));
        }
        let bootstrap_temp = write_synced_temp(&self.bootstrap_path(), journal.old_bootstrap_toml.as_bytes())?;
        if let Err(error) = replace_file(&bootstrap_temp, &self.bootstrap_path()) {
            discard_repository_temp(&bootstrap_temp);
            return Err(self.io_error(self.bootstrap_path(), error));
        }
        fs::remove_file(&journal_path).map_err(|source| self.io_error(&journal_path, source))
    }

    fn write_journal(&self, journal: &ConfigTransactionJournal) -> Result<(), ConfigRepositoryError> {
        let journal_path = self.journal_path();
        let temporary = write_synced_temp(&journal_path, journal.to_toml()?.as_bytes())?;
        if let Err(error) = replace_file(&temporary, &journal_path) {
            discard_repository_temp(&temporary);
            return Err(self.io_error(&journal_path, error));
        }
        Ok(())
    }

    fn recover_after_error<T>(
        &self,
        original: ConfigRepositoryError,
    ) -> Result<T, ConfigRepositoryError> {
        match self.recover_locked() {
            Ok(()) => Err(original),
            Err(recovery) => Err(ConfigRepositoryError::RecoveryFailed {
                original: Box::new(original),
                recovery: Box::new(recovery),
            }),
        }
    }

    fn resolve_paths(&self, config: &NodeConfig) -> Result<ResolvedNodePaths, ConfigRepositoryError> {
        let data_path = LocalNodePath::validate(&self.executable_dir, &config.paths.data_path)?;
        let config_path = LocalNodePath::validate(&self.executable_dir, &config.paths.config_path)?;
        self.reject_control_path(config_path.resolved())?;
        let log_path = LocalNodePath::validate(&self.executable_dir, &config.paths.log_path)?;
        let cache_path = LocalNodePath::validate(&self.executable_dir, &config.paths.cache_path)?;
        Ok(ResolvedNodePaths {
            data_path: data_path.resolved().to_path_buf(),
            config_path: config_path.resolved().to_path_buf(),
            log_path: log_path.resolved().to_path_buf(),
            cache_path: cache_path.resolved().to_path_buf(),
        })
    }

    fn reject_control_path(&self, path: &Path) -> Result<(), ConfigRepositoryError> {
        let normalized = lexical_windows_path(path);
        if [self.bootstrap_path(), self.journal_path(), self.lock_path()]
            .iter()
            .map(|control| lexical_windows_path(control))
            .any(|control| paths_equal_ignore_ascii_case(&normalized, &control))
        {
            return Err(ConfigRepositoryError::RepositoryControlPath {
                path: path.to_path_buf(),
            });
        }
        Ok(())
    }

    fn consume_failpoint(&self, expected: ConfigSaveFailpoint) -> bool {
        let mut failpoint = self.lock_failpoint();
        if *failpoint == Some(expected) {
            *failpoint = None;
            true
        } else {
            false
        }
    }

    fn bootstrap_path(&self) -> PathBuf {
        self.executable_dir.join(BOOTSTRAP_FILE)
    }

    fn journal_path(&self) -> PathBuf {
        self.executable_dir.join(JOURNAL_FILE)
    }

    fn lock_path(&self) -> PathBuf {
        self.executable_dir.join(LOCK_FILE)
    }

    fn lock_transaction(&self) -> MutexGuard<'_, ()> {
        self.state
            .transaction
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    fn lock_failpoint(&self) -> MutexGuard<'_, Option<ConfigSaveFailpoint>> {
        self.state
            .failpoint
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    fn io_error(&self, path: impl AsRef<Path>, source: io::Error) -> ConfigRepositoryError {
        ConfigRepositoryError::Io {
            path: path.as_ref().to_path_buf(),
            source,
        }
    }
}

#[derive(Debug)]
struct ConfigTransactionJournal {
    old_bootstrap_toml: String,
    old_config_path: String,
    old_config_toml: String,
    target_config_path: String,
}

impl ConfigTransactionJournal {
    fn from_loaded(current: &LoadedNodeConfig, target_config_path: &str) -> Self {
        Self {
            old_bootstrap_toml: current.bootstrap_toml.clone(),
            old_config_path: current.bootstrap_config_path.clone(),
            old_config_toml: current.config_toml.clone(),
            target_config_path: target_config_path.to_owned(),
        }
    }

    fn parse(text: &str) -> Result<Self, ConfigRepositoryError> {
        let value: toml::Value = toml::from_str(text).map_err(ConfigRepositoryError::JournalToml)?;
        let table = value
            .as_table()
            .ok_or(ConfigRepositoryError::InvalidJournal("根节点必须是表"))?;
        if table.len() != 4 {
            return Err(ConfigRepositoryError::InvalidJournal("字段必须完整且唯一"));
        }
        Ok(Self {
            old_bootstrap_toml: journal_string(table, "old_bootstrap_toml")?,
            old_config_path: journal_string(table, "old_config_path")?,
            old_config_toml: journal_string(table, "old_config_toml")?,
            target_config_path: journal_string(table, "target_config_path")?,
        })
    }

    fn to_toml(&self) -> Result<String, ConfigRepositoryError> {
        let mut table = toml::map::Map::new();
        table.insert(
            "old_bootstrap_toml".to_owned(),
            toml::Value::String(self.old_bootstrap_toml.clone()),
        );
        table.insert(
            "old_config_path".to_owned(),
            toml::Value::String(self.old_config_path.clone()),
        );
        table.insert(
            "old_config_toml".to_owned(),
            toml::Value::String(self.old_config_toml.clone()),
        );
        table.insert(
            "target_config_path".to_owned(),
            toml::Value::String(self.target_config_path.clone()),
        );
        toml::to_string_pretty(&toml::Value::Table(table)).map_err(ConfigRepositoryError::JournalEncode)
    }
}

fn journal_string(
    table: &toml::map::Map<String, toml::Value>,
    field: &'static str,
) -> Result<String, ConfigRepositoryError> {
    table
        .get(field)
        .and_then(toml::Value::as_str)
        .map(str::to_owned)
        .ok_or(ConfigRepositoryError::InvalidJournal(field))
}

fn parse_bootstrap(text: &str) -> Result<String, ConfigRepositoryError> {
    let value: toml::Value = toml::from_str(text).map_err(ConfigRepositoryError::BootstrapToml)?;
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
    toml::to_string_pretty(&toml::Value::Table(table)).map_err(ConfigRepositoryError::BootstrapEncode)
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
        discard_repository_temp(&temporary);
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

fn discard_repository_temp(path: &Path) {
    let Some(name) = path.file_name().and_then(|name| name.to_str()) else {
        return;
    };
    let mut parts = name.split('.').filter(|part| !part.is_empty()).rev();
    if parts.next() != Some("tmp") {
        return;
    }
    let Some(identifier) = parts.next() else {
        return;
    };
    if Uuid::parse_str(identifier).is_ok() {
        let _ = fs::remove_file(path);
    }
}

fn paths_equal_ignore_ascii_case(left: &Path, right: &Path) -> bool {
    left.as_os_str()
        .to_string_lossy()
        .eq_ignore_ascii_case(&right.as_os_str().to_string_lossy())
}

fn transaction_lock_for(executable_dir: &Path) -> Arc<Mutex<()>> {
    let key = lexical_windows_path(executable_dir)
        .as_os_str()
        .to_string_lossy()
        .to_ascii_lowercase();
    let registry = TRANSACTION_LOCKS.get_or_init(|| Mutex::new(HashMap::new()));
    let mut registry = registry
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner());
    registry.retain(|_, lock| lock.strong_count() > 0);
    if let Some(lock) = registry.get(&key).and_then(Weak::upgrade) {
        return lock;
    }
    let lock = Arc::new(Mutex::new(()));
    registry.insert(key, Arc::downgrade(&lock));
    lock
}

fn lexical_windows_path(path: &Path) -> PathBuf {
    let mut prefix = None;
    let mut rooted = false;
    let mut segments = Vec::<OsString>::new();
    for component in path.components() {
        match component {
            Component::Prefix(value) => prefix = Some(value.as_os_str().to_owned()),
            Component::RootDir => rooted = true,
            Component::CurDir => {}
            Component::ParentDir => {
                if segments.last().is_some_and(|segment| segment != "..") {
                    segments.pop();
                } else if !rooted {
                    segments.push(OsString::from(".."));
                }
            }
            Component::Normal(value) => segments.push(value.to_owned()),
        }
    }
    let mut normalized = PathBuf::new();
    if let Some(prefix) = prefix {
        normalized.push(prefix);
    }
    if rooted {
        normalized.push(Path::new(r"\"));
    }
    for segment in segments {
        normalized.push(segment);
    }
    normalized
}

//! Node bootstrap 只读定位与完整配置单文件原子保存边界。

use std::{
    fs,
    path::PathBuf,
    sync::{Arc, Barrier},
};

use dedup_core::NodeConfig;
use dedup_node_engine::config_repository::{ConfigRepositoryError, NodeConfigRepository};

#[test]
fn load_keeps_relative_paths_raw_and_resolves_them_from_node_executable_directory() {
    let fixture = RepositoryFixture::new();
    let config = fixture.initial_config();
    fixture.write_active_config(&config);

    let loaded = fixture.repository().load().unwrap();

    assert_eq!(loaded.config.paths.cache_path, r"data\node\cache");
    assert_eq!(
        loaded.resolved.cache_path,
        fixture.executable_dir.join(r"data\node\cache")
    );
    assert_eq!(loaded.version_sha256.len(), 64);
}

#[test]
fn save_replaces_existing_config_without_writing_bootstrap_or_journal() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_bootstrap = fs::read_to_string(fixture.bootstrap_path()).unwrap();
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.port = 39092;

    let saved = repository
        .save_if_version(&repository.snapshot().unwrap().version_sha256, &changed)
        .unwrap();

    assert_eq!(saved.config, changed);
    assert_eq!(repository.load().unwrap().config, changed);
    assert_eq!(
        fs::read_to_string(fixture.bootstrap_path()).unwrap(),
        old_bootstrap
    );
    assert!(
        !fixture
            .executable_dir
            .join("config-transaction.toml")
            .exists()
    );
}

#[test]
fn save_rejects_a_new_config_path_and_keeps_both_files_unchanged() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let old_bootstrap = fs::read_to_string(fixture.bootstrap_path()).unwrap();
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.paths.config_path = r"settings\node\config.toml".into();

    assert!(matches!(
        repository.save_if_version(&repository.snapshot().unwrap().version_sha256, &changed),
        Err(ConfigRepositoryError::ConfigPathMismatch { .. })
    ));
    assert_eq!(
        fs::read_to_string(fixture.initial_config_path()).unwrap(),
        old_config
    );
    assert_eq!(
        fs::read_to_string(fixture.bootstrap_path()).unwrap(),
        old_bootstrap
    );
    assert!(
        !fixture
            .executable_dir
            .join(r"settings\node\config.toml")
            .exists()
    );
}

#[test]
fn stale_version_refuses_to_write_the_active_config() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.port = 39092;

    assert!(matches!(
        repository.save_if_version("0".repeat(64).as_str(), &changed),
        Err(ConfigRepositoryError::VersionConflict { .. })
    ));
    assert_eq!(
        fs::read_to_string(fixture.initial_config_path()).unwrap(),
        old_config
    );
}

#[test]
fn failed_replacement_keeps_old_config_and_cleans_temporary_file() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.port = 39094;

    let mut read_only = fs::metadata(fixture.initial_config_path())
        .unwrap()
        .permissions();
    read_only.set_readonly(true);
    fs::set_permissions(fixture.initial_config_path(), read_only).unwrap();
    let result =
        repository.save_if_version(&repository.snapshot().unwrap().version_sha256, &changed);

    let mut writable = fs::metadata(fixture.initial_config_path())
        .unwrap()
        .permissions();
    writable.set_readonly(false);
    fs::set_permissions(fixture.initial_config_path(), writable).unwrap();

    assert!(matches!(result, Err(ConfigRepositoryError::Io { .. })));
    assert_eq!(
        fs::read_to_string(fixture.initial_config_path()).unwrap(),
        old_config
    );
    let temporary_files = fs::read_dir(fixture.initial_config_path().parent().unwrap())
        .unwrap()
        .filter_map(Result::ok)
        .filter(|entry| entry.file_name().to_string_lossy().ends_with(".tmp"))
        .count();
    assert_eq!(temporary_files, 0, "替换失败后不得遗留临时配置文件");
}

#[test]
fn clones_with_the_same_old_version_allow_only_one_save() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let repository = fixture.repository();
    let version = repository.snapshot().unwrap().version_sha256;
    let barrier = Arc::new(Barrier::new(2));
    let first = repository.clone();
    let second = repository.clone();
    let first_barrier = barrier.clone();
    let second_barrier = barrier;
    let first_version = version.clone();
    let mut first_config = initial.clone();
    first_config.port = 39092;
    let mut second_config = initial;
    second_config.port = 39093;

    let first_result = std::thread::spawn(move || {
        first_barrier.wait();
        first.save_if_version(&first_version, &first_config)
    });
    let second_result = std::thread::spawn(move || {
        second_barrier.wait();
        second.save_if_version(&version, &second_config)
    });
    let results = [first_result.join().unwrap(), second_result.join().unwrap()];

    assert_eq!(results.iter().filter(|result| result.is_ok()).count(), 1);
    assert_eq!(
        results
            .iter()
            .filter(|result| matches!(result, Err(ConfigRepositoryError::VersionConflict { .. })))
            .count(),
        1
    );
}

#[test]
fn bootstrap_and_config_path_must_match_exactly() {
    let fixture = RepositoryFixture::new();
    let mut config = fixture.initial_config();
    config.paths.config_path = r"other\node.toml".into();
    fixture.write_config_at_initial_path(&config);
    fixture.write_bootstrap(r"data\node\config.toml");

    assert!(matches!(
        fixture.repository().load(),
        Err(ConfigRepositoryError::ConfigPathMismatch { .. })
    ));
}

#[test]
fn save_rejects_lexical_aliases_of_repository_control_files() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let repository = fixture.repository();
    let version = repository.snapshot().unwrap().version_sha256;

    for alias in [
        r".\bootstrap.toml",
        r"data\..\bootstrap.toml",
        r".\config.lock",
        r"data\..\config.lock",
    ] {
        let mut changed = initial.clone();
        changed.paths.config_path = alias.into();
        assert!(matches!(
            repository.save_if_version(&version, &changed),
            Err(ConfigRepositoryError::RepositoryControlPath { .. })
                | Err(ConfigRepositoryError::ConfigPathMismatch { .. })
        ));
    }
}

struct RepositoryFixture {
    _directory: tempfile::TempDir,
    executable_dir: PathBuf,
}

impl RepositoryFixture {
    /// 创建与 node.exe 目录布局相同的临时仓库。
    fn new() -> Self {
        let directory = tempfile::tempdir().unwrap();
        let executable_dir = directory.path().join("portable");
        fs::create_dir_all(&executable_dir).unwrap();
        Self {
            _directory: directory,
            executable_dir,
        }
    }

    /// 创建使用当前临时 node.exe 目录的仓库。
    fn repository(&self) -> NodeConfigRepository {
        NodeConfigRepository::new(&self.executable_dir)
    }

    fn bootstrap_path(&self) -> PathBuf {
        self.executable_dir.join("bootstrap.toml")
    }

    fn initial_config_path(&self) -> PathBuf {
        self.executable_dir.join(r"data\node\config.toml")
    }

    fn initial_config(&self) -> NodeConfig {
        let mut config = NodeConfig::default();
        config.paths.data_path = r"data\node".into();
        config.paths.config_path = r"data\node\config.toml".into();
        config.paths.log_path = r"data\node\logs".into();
        config.paths.cache_path = r"data\node\cache".into();
        config
    }

    fn write_active_config(&self, config: &NodeConfig) {
        self.write_config_at_initial_path(config);
        self.write_bootstrap(r"data\node\config.toml");
    }

    fn write_config_at_initial_path(&self, config: &NodeConfig) {
        let config_path = self.initial_config_path();
        fs::create_dir_all(config_path.parent().unwrap()).unwrap();
        fs::write(&config_path, config.to_toml().unwrap()).unwrap();
    }

    fn write_bootstrap(&self, config_path: &str) {
        fs::write(
            self.bootstrap_path(),
            format!("config_path = {config_path:?}\n"),
        )
        .unwrap();
    }
}

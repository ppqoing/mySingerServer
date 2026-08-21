//! Node bootstrap 与完整配置的原子持久化边界。

use std::{fs, path::PathBuf};

use dedup_core::NodeConfig;
use dedup_node_engine::config_repository::{ConfigRepositoryError, NodeConfigRepository};

#[test]
fn load_keeps_relative_paths_raw_and_resolves_them_from_node_executable_directory() {
    let fixture = RepositoryFixture::new();
    let config = fixture.initial_config();
    fixture.write_active_config(&config);
    let repository = fixture.repository();

    let loaded = repository.load().unwrap();

    assert_eq!(loaded.config.paths.cache_path, r"data\node\cache");
    assert_eq!(
        loaded.resolved.cache_path,
        fixture.executable_dir.join(r"data\node\cache")
    );
    assert_eq!(loaded.version_sha256.len(), 64);
    assert!(loaded.version_sha256.bytes().all(|byte| byte.is_ascii_hexdigit()));
}

#[test]
fn save_to_a_new_config_path_keeps_the_old_config_file() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config_text = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.paths.config_path = r"settings\node\config.toml".into();
    changed.port = 39092;

    let saved = repository
        .save_if_version(&repository.snapshot().unwrap().version_sha256, &changed)
        .unwrap();

    assert_eq!(saved.config, changed);
    assert_eq!(fs::read_to_string(fixture.initial_config_path()).unwrap(), old_config_text);
    let bootstrap: toml::Value =
        toml::from_str(&fs::read_to_string(fixture.bootstrap_path()).unwrap()).unwrap();
    let bootstrap = bootstrap.as_table().unwrap();
    assert_eq!(bootstrap.len(), 1);
    assert_eq!(
        bootstrap.get("config_path").and_then(toml::Value::as_str),
        Some(r"settings\node\config.toml")
    );
    assert!(fixture.executable_dir.join(r"settings\node\config.toml").exists());
}

#[test]
fn stale_version_refuses_to_write_either_current_file() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let old_bootstrap = fs::read_to_string(fixture.bootstrap_path()).unwrap();

    let error = fixture
        .repository()
        .save_if_version("0".repeat(64).as_str(), &initial)
        .unwrap_err();

    assert!(matches!(error, ConfigRepositoryError::VersionConflict { .. }));
    assert_eq!(fs::read_to_string(fixture.initial_config_path()).unwrap(), old_config);
    assert_eq!(fs::read_to_string(fixture.bootstrap_path()).unwrap(), old_bootstrap);
}

#[test]
fn target_config_write_failure_keeps_the_active_bootstrap_and_config() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let old_bootstrap = fs::read_to_string(fixture.bootstrap_path()).unwrap();
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.paths.config_path = r"data\node\config.toml\blocked.toml".into();

    assert!(repository
        .save_if_version(&repository.snapshot().unwrap().version_sha256, &changed)
        .is_err());

    assert_eq!(fs::read_to_string(fixture.initial_config_path()).unwrap(), old_config);
    assert_eq!(fs::read_to_string(fixture.bootstrap_path()).unwrap(), old_bootstrap);
    assert_eq!(repository.load().unwrap().config, initial);
}

#[test]
fn bootstrap_replace_failure_rolls_back_an_overwrite_of_the_current_config_path() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let old_bootstrap = fs::read_to_string(fixture.bootstrap_path()).unwrap();
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.port = 39092;
    fixture.make_bootstrap_read_only();

    assert!(repository
        .save_if_version(&repository.snapshot().unwrap().version_sha256, &changed)
        .is_err());
    fixture.make_bootstrap_writable();

    assert_eq!(fs::read_to_string(fixture.initial_config_path()).unwrap(), old_config);
    assert_eq!(fs::read_to_string(fixture.bootstrap_path()).unwrap(), old_bootstrap);
    assert_eq!(repository.load().unwrap().config, initial);
}

#[test]
fn bootstrap_replace_failure_keeps_the_old_config_active_when_the_path_changes() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let old_bootstrap = fs::read_to_string(fixture.bootstrap_path()).unwrap();
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.paths.config_path = r"next\node\config.toml".into();
    changed.port = 39092;
    fixture.make_bootstrap_read_only();

    assert!(repository
        .save_if_version(&repository.snapshot().unwrap().version_sha256, &changed)
        .is_err());
    fixture.make_bootstrap_writable();

    assert_eq!(fs::read_to_string(fixture.initial_config_path()).unwrap(), old_config);
    assert_eq!(fs::read_to_string(fixture.bootstrap_path()).unwrap(), old_bootstrap);
    assert_eq!(repository.load().unwrap().config, initial);
    assert!(fixture.executable_dir.join(r"next\node\config.toml").exists());
}

struct RepositoryFixture {
    _directory: tempfile::TempDir,
    executable_dir: PathBuf,
}

impl RepositoryFixture {
    fn new() -> Self {
        let directory = tempfile::tempdir().unwrap();
        let executable_dir = directory.path().join("portable");
        fs::create_dir_all(&executable_dir).unwrap();
        Self {
            _directory: directory,
            executable_dir,
        }
    }

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
        let config_path = self.initial_config_path();
        fs::create_dir_all(config_path.parent().unwrap()).unwrap();
        fs::write(&config_path, config.to_toml().unwrap()).unwrap();
        fs::write(
            self.bootstrap_path(),
            "config_path = \"data\\\\node\\\\config.toml\"\n",
        )
        .unwrap();
    }

    fn make_bootstrap_read_only(&self) {
        let mut permissions = fs::metadata(self.bootstrap_path()).unwrap().permissions();
        permissions.set_readonly(true);
        fs::set_permissions(self.bootstrap_path(), permissions).unwrap();
    }

    fn make_bootstrap_writable(&self) {
        let mut permissions = fs::metadata(self.bootstrap_path()).unwrap().permissions();
        permissions.set_readonly(false);
        fs::set_permissions(self.bootstrap_path(), permissions).unwrap();
    }
}

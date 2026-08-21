//! Node bootstrap 与完整配置的原子持久化边界。

use std::{
    fs,
    path::PathBuf,
    sync::{Arc, Barrier},
};

use dedup_core::NodeConfig;
use dedup_node_engine::config_repository::{
    ConfigRepositoryError, ConfigSaveFailpoint, NodeConfigRepository,
};

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
fn restore_if_version_uses_cas_and_restores_exact_original_text_and_sha() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    let original_config = format!(
        "# preserve original bytes\r\n{}",
        initial
            .to_toml()
            .unwrap()
            .replace("port = 39091", "port=39091 # original spacing")
    );
    let original_bootstrap = "config_path='data\\node\\config.toml' # original spacing\r\n";
    fs::create_dir_all(fixture.initial_config_path().parent().unwrap()).unwrap();
    fs::write(fixture.initial_config_path(), &original_config).unwrap();
    fs::write(fixture.bootstrap_path(), original_bootstrap).unwrap();
    let repository = fixture.repository();
    let previous = repository.snapshot().unwrap();
    let mut changed = initial.clone();
    changed.port = 39092;

    let saved = repository
        .save_if_version(&previous.version_sha256, &changed)
        .unwrap();
    assert_ne!(saved.version_sha256, previous.version_sha256);
    assert_ne!(
        fs::read_to_string(fixture.initial_config_path()).unwrap(),
        original_config
    );

    let conflict = repository
        .restore_if_version("wrong-new-version", &previous)
        .unwrap_err();
    assert!(matches!(
        conflict,
        ConfigRepositoryError::VersionConflict { .. }
    ));
    assert_eq!(
        repository.snapshot().unwrap().version_sha256,
        saved.version_sha256
    );

    let restored = repository
        .restore_if_version(&saved.version_sha256, &previous)
        .unwrap();
    assert_eq!(restored.version_sha256, previous.version_sha256);
    assert_eq!(restored.config, initial);
    assert_eq!(
        fs::read_to_string(fixture.initial_config_path()).unwrap(),
        original_config
    );
    assert_eq!(
        fs::read_to_string(fixture.bootstrap_path()).unwrap(),
        original_bootstrap
    );
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
    let second_barrier = barrier.clone();
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
fn independent_repositories_with_the_same_old_version_allow_only_one_save() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let first = fixture.repository();
    let second = fixture.repository();
    let version = first.snapshot().unwrap().version_sha256;
    let barrier = Arc::new(Barrier::new(2));
    let first_barrier = barrier.clone();
    let second_barrier = barrier.clone();
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
fn load_recovers_a_persisted_journal_after_config_replace_interruption() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_config = fs::read_to_string(fixture.initial_config_path()).unwrap();
    let repository = fixture.repository_with_failpoint(ConfigSaveFailpoint::AfterConfigReplace);
    let mut changed = initial.clone();
    changed.port = 39092;

    assert!(matches!(
        repository.save_if_version(&repository.snapshot().unwrap().version_sha256, &changed),
        Err(ConfigRepositoryError::SimulatedInterruption(_))
    ));
    assert!(fixture.journal_path().exists());
    assert_ne!(fs::read_to_string(fixture.initial_config_path()).unwrap(), old_config);

    assert_eq!(repository.load().unwrap().config, initial);
    assert_eq!(fs::read_to_string(fixture.initial_config_path()).unwrap(), old_config);
    assert!(!fixture.journal_path().exists());
}

#[test]
fn load_recovers_a_persisted_journal_after_bootstrap_replace_interruption() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let old_bootstrap = fs::read_to_string(fixture.bootstrap_path()).unwrap();
    let repository = fixture.repository_with_failpoint(ConfigSaveFailpoint::AfterBootstrapReplace);
    let mut changed = initial.clone();
    changed.paths.config_path = r"next\node\config.toml".into();
    changed.port = 39092;

    assert!(matches!(
        repository.save_if_version(&repository.snapshot().unwrap().version_sha256, &changed),
        Err(ConfigRepositoryError::SimulatedInterruption(_))
    ));
    assert!(fixture.journal_path().exists());
    assert_ne!(fs::read_to_string(fixture.bootstrap_path()).unwrap(), old_bootstrap);

    assert_eq!(repository.load().unwrap().config, initial);
    assert_eq!(fs::read_to_string(fixture.bootstrap_path()).unwrap(), old_bootstrap);
    assert!(!fixture.journal_path().exists());
}

#[test]
fn failed_recovery_keeps_the_journal_and_does_not_expose_the_new_config() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let repository = fixture.repository_with_failpoint(ConfigSaveFailpoint::AfterConfigReplace);
    let mut changed = initial.clone();
    changed.port = 39092;

    assert!(repository
        .save_if_version(&repository.snapshot().unwrap().version_sha256, &changed)
        .is_err());
    fixture.make_initial_config_read_only();
    assert!(repository.load().is_err());
    assert!(fixture.journal_path().exists());
    fixture.make_initial_config_writable();

    assert_eq!(repository.load().unwrap().config, initial);
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
fn save_rejects_repository_control_files_as_config_targets() {
    let fixture = RepositoryFixture::new();
    let initial = fixture.initial_config();
    fixture.write_active_config(&initial);
    let repository = fixture.repository();
    let mut changed = initial.clone();
    changed.paths.config_path = "bootstrap.toml".into();

    assert!(matches!(
        repository.save_if_version(&repository.snapshot().unwrap().version_sha256, &changed),
        Err(ConfigRepositoryError::RepositoryControlPath { .. })
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
        r".\config-transaction.toml",
        r"data\..\config.lock",
    ] {
        let mut changed = initial.clone();
        changed.paths.config_path = alias.into();
        assert!(matches!(
            repository.save_if_version(&version, &changed),
            Err(ConfigRepositoryError::RepositoryControlPath { .. })
        ));
    }
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

    fn repository_with_failpoint(&self, failpoint: ConfigSaveFailpoint) -> NodeConfigRepository {
        NodeConfigRepository::with_failpoint(&self.executable_dir, failpoint)
    }

    fn bootstrap_path(&self) -> PathBuf {
        self.executable_dir.join("bootstrap.toml")
    }

    fn initial_config_path(&self) -> PathBuf {
        self.executable_dir.join(r"data\node\config.toml")
    }

    fn journal_path(&self) -> PathBuf {
        self.executable_dir.join("config-transaction.toml")
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

    fn make_initial_config_read_only(&self) {
        self.set_initial_config_read_only(true);
    }

    fn make_initial_config_writable(&self) {
        self.set_initial_config_read_only(false);
    }

    fn set_initial_config_read_only(&self, read_only: bool) {
        let mut permissions = fs::metadata(self.initial_config_path()).unwrap().permissions();
        permissions.set_readonly(read_only);
        fs::set_permissions(self.initial_config_path(), permissions).unwrap();
    }
}

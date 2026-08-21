//! Node 运行配置的默认值与拒绝边界。

use dedup_core::{CoreError, EnumeratorKind, NodeConfig, WorkerMode};

#[test]
fn defaults_match_the_approved_node_runtime_contract() {
    let config = NodeConfig::default();

    assert_eq!(config.enumerator, EnumeratorKind::Everything);
    assert_eq!(config.paths.data_path, "data/node");
    assert_eq!(config.paths.config_path, "data/node/config.toml");
    assert_eq!(config.paths.log_path, "data/node/logs");
    assert_eq!(config.paths.cache_path, "data/node/cache");
    assert_eq!(config.read.hdd_threads_per_disk, 1);
    assert_eq!(config.read.ssd_threads_per_disk, 2);
    assert_eq!(config.read.unknown_threads_per_disk, 1);
    assert_eq!(config.read.total_threads, 4);
    assert_eq!(config.read.block_size_bytes, 4 * 1024 * 1024);
    assert_eq!(config.read.block_timeout_seconds, 3);
    assert_eq!(config.read.block_retries, 2);
    assert_eq!(config.worker.mode, WorkerMode::Automatic);
    assert_eq!(config.worker.reserved_cores, 1);
}

#[test]
fn validate_rejects_each_node_runtime_boundary() {
    let cases = [
        ("port = 0", "port"),
        ("[read]\nhdd_threads_per_disk = 0", "read.hdd_threads_per_disk"),
        ("[read]\nssd_threads_per_disk = 0", "read.ssd_threads_per_disk"),
        (
            "[read]\nunknown_threads_per_disk = 0",
            "read.unknown_threads_per_disk",
        ),
        ("[read]\ntotal_threads = 0", "read.total_threads"),
        (
            "[read]\nhdd_threads_per_disk = 65",
            "read.hdd_threads_per_disk",
        ),
        (
            "[read]\nssd_threads_per_disk = 65",
            "read.ssd_threads_per_disk",
        ),
        (
            "[read]\nunknown_threads_per_disk = 65",
            "read.unknown_threads_per_disk",
        ),
        ("[read]\ntotal_threads = 257", "read.total_threads"),
        ("[read]\nblock_size_bytes = 65535", "read.block_size_bytes"),
        (
            "[read]\nblock_size_bytes = 67108865",
            "read.block_size_bytes",
        ),
        (
            "[read]\nblock_timeout_seconds = 0",
            "read.block_timeout_seconds",
        ),
        (
            "[read]\nblock_timeout_seconds = 61",
            "read.block_timeout_seconds",
        ),
        ("[read]\nblock_retries = 11", "read.block_retries"),
        (
            "[worker]\nmode = \"manual\"\nmanual_worker_count = 0",
            "worker.manual_worker_count",
        ),
        (
            "[worker]\nmode = \"automatic\"\nreserved_cores = 256",
            "worker.reserved_cores",
        ),
        (
            "[worker]\nmode = \"manual\"\nmanual_worker_count = 257",
            "worker.manual_worker_count",
        ),
        ("worker_count = 257", "worker_count"),
    ];

    for (toml, expected_field) in cases {
        let error = NodeConfig::from_toml(toml).expect_err("配置应在 Node 边界被拒绝");
        assert!(matches!(
            error,
            CoreError::InvalidConfig { field, .. } if field == expected_field
        ));
    }
}

#[test]
fn automatic_mode_rejects_an_out_of_range_manual_worker_count() {
    let error = NodeConfig::from_toml(
        r#"
[worker]
mode = "automatic"
manual_worker_count = 257
"#,
    )
    .expect_err("未生效的手动 Worker 字段也必须通过配置边界验证");

    assert!(matches!(
        error,
        CoreError::InvalidConfig {
            field: "worker.manual_worker_count",
            ..
        }
    ));
}

#[test]
fn manual_mode_rejects_an_out_of_range_reserved_core_count() {
    let error = NodeConfig::from_toml(
        r#"
[worker]
mode = "manual"
reserved_cores = 256
"#,
    )
    .expect_err("未生效的保留核心字段也必须通过配置边界验证");

    assert!(matches!(
        error,
        CoreError::InvalidConfig {
            field: "worker.reserved_cores",
            ..
        }
    ));
}

#[test]
fn automatic_worker_count_reserves_cores_without_falling_below_one() {
    let config = NodeConfig::default();

    assert_eq!(config.worker.effective_worker_count(8), 7);
    assert_eq!(config.worker.effective_worker_count(1), 1);
    assert_eq!(config.worker.effective_worker_count(0), 1);
}

#[test]
fn manual_worker_count_is_used_after_validation() {
    let config = NodeConfig::from_toml(
        r#"
[worker]
mode = "manual"
manual_worker_count = 3
"#,
    )
    .expect("手动 Worker 数量大于零应通过验证");

    assert_eq!(config.worker.effective_worker_count(64), 3);
}

#[test]
fn node_paths_round_trip_as_raw_strings() {
    let config = NodeConfig::from_toml(
        r#"
[paths]
data_path = "relative/data"
config_path = "D:\\node-config.toml"
log_path = "logs"
cache_path = "D:\\cache"
"#,
    )
    .expect("路径原始字符串应由后续本机路径边界验证");

    assert_eq!(config.paths.data_path, "relative/data");
    assert_eq!(config.paths.config_path, "D:\\node-config.toml");
    assert_eq!(config.paths.log_path, "logs");
    assert_eq!(config.paths.cache_path, "D:\\cache");
}

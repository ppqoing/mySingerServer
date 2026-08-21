//! node.exe 父进程门禁与替代进程宿主生命周期。

#[path = "../src/restart_lifecycle.rs"]
mod restart_lifecycle;

use std::{
    ffi::OsString,
    fs,
    net::TcpListener,
    path::PathBuf,
    process::{Command, Stdio},
    sync::mpsc,
    thread,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use dedup_node_engine::host_control::{NodeHostControl, NodeHostControlError};
use dedup_windows::AppLayout;
use restart_lifecycle::{
    NodeRestartHost, load_or_initialize_node_config, wait_for_requested_parent,
};
use tokio::sync::mpsc as tokio_mpsc;

#[test]
fn wait_for_parent_gate_prevents_database_and_listener_startup() {
    let directory = TestDirectory::new("wait-gate");
    let database = directory.path().join("node.db");
    let mut parent = spawn_long_running_process();
    let parent_pid = parent.id();
    let (started, startup) = mpsc::sync_channel(1);
    let database_for_start = database.clone();
    let replacement = thread::spawn(move || {
        wait_for_requested_parent([
            OsString::from("--wait-for-parent"),
            OsString::from(parent_pid.to_string()),
        ])
        .unwrap();
        fs::write(&database_for_start, b"opened-after-parent-exit").unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        started.send(listener).unwrap();
    });

    assert!(startup.recv_timeout(Duration::from_millis(150)).is_err());
    assert!(!database.exists());
    parent.kill().unwrap();
    parent.wait().unwrap();

    let listener = startup.recv_timeout(Duration::from_secs(2)).unwrap();
    assert!(database.exists());
    assert_ne!(listener.local_addr().unwrap().port(), 0);
    drop(listener);
    replacement.join().unwrap();
}

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

#[tokio::test]
async fn replacement_spawn_failure_keeps_the_old_runtime_unsignalled() {
    let (shutdown, mut shutdown_receiver) = tokio_mpsc::channel(1);
    let host = NodeRestartHost::new(
        PathBuf::from(r"Z:\missing\node.exe"),
        std::process::id(),
        shutdown,
    );

    let error = host.prepare_replacement("saved-sha").unwrap_err();
    assert!(matches!(error, NodeHostControlError::Failed(_)));
    assert!(shutdown_receiver.try_recv().is_err());
}

#[tokio::test]
async fn prepared_host_commits_orderly_shutdown_only_once() {
    let directory = TestDirectory::new("host-commit");
    let script = directory.path().join("replacement.cmd");
    let arguments = directory.path().join("arguments.txt");
    fs::write(
        &script,
        format!(
            "@echo off\r\n>\"{}\" echo %*\r\n",
            arguments.display()
        ),
    )
    .unwrap();
    let (shutdown, mut shutdown_receiver) = tokio_mpsc::channel(1);
    let host = NodeRestartHost::new(script, 515_151, shutdown);

    host.prepare_replacement("saved-sha").unwrap();
    host.commit_exit_after_response().unwrap();
    host.commit_exit_after_response().unwrap();

    assert_eq!(shutdown_receiver.recv().await, Some(()));
    assert!(shutdown_receiver.try_recv().is_err());
    for _ in 0..40 {
        if arguments.exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    assert_eq!(
        fs::read_to_string(arguments).unwrap().trim(),
        "--wait-for-parent 515151"
    );
}

#[test]
fn wait_for_parent_rejects_malformed_command_lines() {
    assert!(wait_for_requested_parent([OsString::from("--wait-for-parent")]).is_err());
    assert!(wait_for_requested_parent([OsString::from("--unknown")]).is_err());
}

fn spawn_long_running_process() -> std::process::Child {
    Command::new("ping.exe")
        .args(["-t", "127.0.0.1"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap()
}

struct TestDirectory(PathBuf);

impl TestDirectory {
    fn new(label: &str) -> Self {
        let nonce = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap()
            .as_nanos();
        let path = std::env::temp_dir().join(format!(
            "mysingerserver-node-restart-{label}-{}-{nonce}",
            std::process::id()
        ));
        fs::create_dir_all(&path).unwrap();
        Self(path)
    }

    fn path(&self) -> &std::path::Path {
        &self.0
    }
}

impl Drop for TestDirectory {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

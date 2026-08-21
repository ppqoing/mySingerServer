use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

use dedup_core::{
    ContentKey, DisplayPath, GroupId, LocationKey, MachineId, MediaKind, NodeConfig,
    NormalizedPath, Thresholds, WorkerMode,
};
use dedup_node_engine::{
    actor::{NodeConfigRepositoryAccess, NodeConfigState, NodeEngine, NodeEngineHandle},
    config_repository::ConfigRepositoryError,
    host_control::{NodeHostControl, NodeHostControlError},
    server::NodeRequestHandler,
};
use dedup_node_store::{
    AnalysisMode, GroupKind, GroupMemberWrite, GroupWrite, NodeStore, ScannedPath,
};
use dedup_protocol::proto;

#[tokio::test]
async fn protocol_requests_cross_the_actor_and_ack_persisted_outbox() {
    let machine = MachineId::parse(&"88".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    store.record_sync_change("fixture", vec![1, 2, 3]).unwrap();
    let (handle, actor) = NodeEngine::spawn_for_test(
        store,
        "127.0.0.1:39091".parse().unwrap(),
        Path::new(r"C:\fixture\cache"),
    );

    let status = handle
        .handle(envelope(
            1,
            proto::envelope::Payload::NodeStatus(Default::default()),
        ))
        .await;
    let Some(proto::envelope::Payload::NodeStatus(status)) = status.payload else {
        panic!("expected node status");
    };
    assert_eq!(status.machine_id, machine.as_str());
    assert_eq!(status.listen_address, "127.0.0.1:39091");
    assert_eq!(status.outbox_high_seq, 1);

    let ping = handle
        .handle(envelope(
            2,
            proto::envelope::Payload::Ping(proto::Ping { nonce: 42 }),
        ))
        .await;
    assert!(matches!(
        ping.payload,
        Some(proto::envelope::Payload::Ping(proto::Ping { nonce: 42 }))
    ));

    let pulled = handle
        .handle(envelope(
            3,
            proto::envelope::Payload::PullChanges(proto::PullChanges {
                after_seq: 0,
                limit: 1000,
            }),
        ))
        .await;
    let Some(proto::envelope::Payload::SyncChangeBatch(batch)) = pulled.payload else {
        panic!("expected sync batch");
    };
    assert_eq!(batch.changes.len(), 1);
    assert_eq!(batch.high_seq, 1);

    let ack = handle
        .handle(envelope(
            4,
            proto::envelope::Payload::SyncAck(proto::SyncAck { committed_seq: 1 }),
        ))
        .await;
    assert!(matches!(
        ack.payload,
        Some(proto::envelope::Payload::SyncAck(_))
    ));

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn actor_holds_snapshot_until_last_table_or_connection_close() {
    let directory = tempfile::tempdir().unwrap();
    let machine = MachineId::parse(&"89".repeat(32)).unwrap();
    let mut store = NodeStore::open(&directory.path().join("node.db"), machine).unwrap();
    store
        .upsert_content_and_location(
            &ScannedPath::new(
                NormalizedPath::new(r"D:\snapshot.bin").unwrap(),
                DisplayPath::new(r"D:\snapshot.bin").unwrap(),
                7,
            ),
            [7; 16],
            MediaKind::Other,
        )
        .unwrap();
    let (handle, actor) =
        NodeEngine::spawn_for_test(store, "127.0.0.1:39091".parse().unwrap(), directory.path());

    let begin = handle
        .handle(envelope(
            1,
            proto::envelope::Payload::BeginSnapshot(Default::default()),
        ))
        .await;
    let Some(proto::envelope::Payload::BeginSnapshot(begin)) = begin.payload else {
        panic!("expected snapshot token");
    };
    assert!(!begin.snapshot_token.is_empty());
    let page = handle
        .handle(envelope(
            2,
            proto::envelope::Payload::ReadSnapshotPage(proto::ReadSnapshotPage {
                snapshot_token: begin.snapshot_token.clone(),
                table_name: "contents".into(),
                cursor: String::new(),
                limit: 1000,
                rows: Vec::new(),
                next_cursor: String::new(),
                done: false,
            }),
        ))
        .await;
    let Some(proto::envelope::Payload::ReadSnapshotPage(page)) = page.payload else {
        panic!("expected snapshot page");
    };
    assert_eq!(page.rows.len(), 1);

    handle.connection_closed().await;
    let stale = handle
        .handle(envelope(
            3,
            proto::envelope::Payload::ReadSnapshotPage(proto::ReadSnapshotPage {
                snapshot_token: begin.snapshot_token,
                table_name: "files".into(),
                cursor: String::new(),
                limit: 1000,
                rows: Vec::new(),
                next_cursor: String::new(),
                done: false,
            }),
        ))
        .await;
    assert!(matches!(
        stale.payload,
        Some(proto::envelope::Payload::Error(proto::Error { code, .. }))
            if code == proto::ErrorCode::NotFound as i32
    ));

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn actor_reports_current_member_activity_in_result_pages() {
    let machine = MachineId::parse(&"8a".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine.clone()).unwrap();
    let paths = [
        r"D:\ActorResults\a.bin",
        r"D:\ActorResults\b.bin",
        r"D:\ActorResults\c.bin",
    ];
    let contents = [
        ContentKey::new([0xa1; 16], 101),
        ContentKey::new([0xa2; 16], 102),
        ContentKey::new([0xa3; 16], 103),
    ];
    for (path, content) in paths.iter().zip(contents) {
        store
            .upsert_content_and_location(
                &ScannedPath::new(
                    NormalizedPath::new(path).unwrap(),
                    DisplayPath::new(path).unwrap(),
                    content.file_size(),
                ),
                content.md5(),
                MediaKind::Other,
            )
            .unwrap();
    }
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let group_id = GroupId::new().as_uuid().to_string();
    store
        .replace_groups(
            run,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Exact,
                representative: contents[0],
                members: paths
                    .iter()
                    .zip(contents)
                    .enumerate()
                    .map(|(index, (path, content))| {
                        GroupMemberWrite::new(
                            LocationKey::new(machine.clone(), NormalizedPath::new(path).unwrap()),
                            content,
                            index == 0,
                        )
                    })
                    .collect(),
            }],
        )
        .unwrap();
    let scan = store
        .create_scan_task(&[NormalizedPath::new(r"D:\ActorResults").unwrap()], 2)
        .unwrap();
    store
        .finalize_scan_task(
            scan,
            &[
                NormalizedPath::new(paths[0]).unwrap(),
                NormalizedPath::new(paths[2]).unwrap(),
            ],
            3,
        )
        .unwrap();
    let (handle, actor) = NodeEngine::spawn_for_test(
        store,
        "127.0.0.1:39091".parse().unwrap(),
        Path::new(r"C:\fixture\cache"),
    );

    let groups = handle
        .handle(envelope(
            1,
            proto::envelope::Payload::ListGroups(proto::ListGroups {
                analysis_run_id: run.as_uuid().to_string(),
                group_kind: proto::GroupKind::GroupExact as i32,
                cursor: String::new(),
                limit: 10,
                groups: Vec::new(),
                next_cursor: String::new(),
            }),
        ))
        .await;
    let Some(proto::envelope::Payload::ListGroups(groups)) = groups.payload else {
        panic!("expected group page");
    };
    assert_eq!(groups.groups[0].member_count, 2);
    assert_eq!(groups.groups[0].reclaimable_bytes, 103);

    let members = handle
        .handle(envelope(
            2,
            proto::envelope::Payload::ListGroupMembers(proto::ListGroupMembers {
                analysis_run_id: run.as_uuid().to_string(),
                group_id,
                cursor: String::new(),
                limit: 10,
                members: Vec::new(),
                next_cursor: String::new(),
            }),
        ))
        .await;
    let Some(proto::envelope::Payload::ListGroupMembers(members)) = members.payload else {
        panic!("expected member page");
    };
    assert_eq!(
        members
            .members
            .iter()
            .map(|member| member.active)
            .collect::<Vec<_>>(),
        [true, false, true]
    );

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn remote_config_get_returns_original_snapshot_and_effective_workers() {
    let repository = FakeConfigRepository::new(SaveBehavior::Success);
    let repository_state = Arc::clone(&repository.state);
    let host = FakeHostControl::new(false);
    let host_state = Arc::clone(&host.state);
    let (handle, actor) = spawn_remote_config_actor(repository, Some(host));

    let response = handle
        .handle(envelope(
            31,
            proto::envelope::Payload::GetNodeConfig(proto::GetNodeConfig {}),
        ))
        .await;
    let Some(proto::envelope::Payload::NodeConfigSnapshot(snapshot)) = response.payload else {
        panic!("expected node config snapshot");
    };
    assert_eq!(snapshot.machine_id, "ab".repeat(32));
    assert_eq!(snapshot.version_sha256, "current-sha");
    assert!(snapshot.logical_cpu_count >= 1);
    assert_eq!(snapshot.effective_worker_count, 7);
    assert_eq!(
        snapshot.config,
        Some(proto::NodeConfigValue {
            listen_ip: "127.0.0.9".into(),
            port: 39123,
            enumerator: proto::NodeEnumerator::NodeWindowsWalker as i32,
            data_path: "relative/data".into(),
            config_path: "relative/config.toml".into(),
            log_path: r"D:\Node Logs".into(),
            cache_path: "relative/cache".into(),
            hdd_threads_per_disk: 3,
            ssd_threads_per_disk: 5,
            unknown_threads_per_disk: 2,
            total_threads: 9,
            block_size_bytes: 2 * 1024 * 1024,
            block_timeout_seconds: 11,
            block_retries: 4,
            legacy_worker_count: 6,
            worker_mode: proto::NodeWorkerMode::NodeWorkerManual as i32,
            reserved_cores: 2,
            manual_worker_count: 7,
        })
    );
    assert_eq!(repository_state.lock().unwrap().load_calls, 1);
    assert_eq!(repository_state.lock().unwrap().save_calls, 0);
    assert!(host_state.lock().unwrap().prepared_versions.is_empty());

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn remote_config_save_without_host_fails_before_repository_write() {
    let repository = FakeConfigRepository::new(SaveBehavior::Success);
    let repository_state = Arc::clone(&repository.state);
    let (handle, actor) = spawn_remote_config_actor::<FakeHostControl>(repository, None);
    let mut request = save_request(32, "current-sha");
    let Some(proto::envelope::Payload::SaveNodeConfigAndRestart(save)) = request.payload.as_mut()
    else {
        unreachable!();
    };
    save.config.as_mut().unwrap().port = 0;

    let response = handle.handle(request).await;
    assert_error_code(response, proto::ErrorCode::Internal);
    assert_eq!(repository_state.lock().unwrap().save_calls, 0);

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn remote_config_invalid_field_skips_repository_and_host() {
    let repository = FakeConfigRepository::new(SaveBehavior::Success);
    let repository_state = Arc::clone(&repository.state);
    let host = FakeHostControl::new(false);
    let host_state = Arc::clone(&host.state);
    let (handle, actor) = spawn_remote_config_actor(repository, Some(host));
    let mut request = save_request(33, "current-sha");
    let Some(proto::envelope::Payload::SaveNodeConfigAndRestart(save)) = request.payload.as_mut()
    else {
        unreachable!();
    };
    save.config.as_mut().unwrap().port = 0;

    let response = handle.handle(request).await;
    assert_error_code(response, proto::ErrorCode::InvalidRequest);
    assert_eq!(repository_state.lock().unwrap().save_calls, 0);
    assert!(host_state.lock().unwrap().prepared_versions.is_empty());

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn remote_config_version_conflict_and_path_failure_do_not_prepare_replacement() {
    for (behavior, expected_code) in [
        (SaveBehavior::Conflict, proto::ErrorCode::Conflict),
        (SaveBehavior::PathFailure, proto::ErrorCode::InvalidRequest),
    ] {
        let repository = FakeConfigRepository::new(behavior);
        let repository_state = Arc::clone(&repository.state);
        let host = FakeHostControl::new(false);
        let host_state = Arc::clone(&host.state);
        let (handle, actor) = spawn_remote_config_actor(repository, Some(host));

        let response = handle.handle(save_request(34, "stale-sha")).await;
        assert_error_code(response, expected_code);
        assert_eq!(repository_state.lock().unwrap().save_calls, 1);
        assert!(host_state.lock().unwrap().prepared_versions.is_empty());

        handle.shutdown().await.unwrap();
        actor.await.unwrap();
    }
}

#[tokio::test]
async fn remote_config_prepare_failure_keeps_old_node_running() {
    let events = Arc::new(Mutex::new(Vec::new()));
    let repository = FakeConfigRepository::with_control(
        SaveBehavior::Success,
        false,
        Arc::clone(&events),
    );
    let repository_state = Arc::clone(&repository.state);
    let host = FakeHostControl::with_control(true, 0, Arc::clone(&events));
    let host_state = Arc::clone(&host.state);
    let (handle, actor) = spawn_remote_config_actor(repository, Some(host));

    let response = handle.handle(save_request(35, "current-sha")).await;
    assert_error_code(response, proto::ErrorCode::Internal);
    let repository_state = repository_state.lock().unwrap();
    assert_eq!(repository_state.save_calls, 1);
    assert_eq!(repository_state.restore_calls, 1);
    assert_eq!(repository_state.current.version_sha256, "current-sha");
    assert_eq!(repository_state.current.config, remote_config_fixture());
    drop(repository_state);
    assert_eq!(host_state.lock().unwrap().prepared_versions, ["saved-sha"]);
    assert_eq!(
        events.lock().unwrap().as_slice(),
        ["snapshot", "save", "prepare", "restore"]
    );
    handle.response_flushed(35).await.unwrap();
    assert_eq!(host_state.lock().unwrap().commit_calls, 0);
    let ping = handle
        .handle(envelope(
            36,
            proto::envelope::Payload::Ping(proto::Ping { nonce: 99 }),
        ))
        .await;
    assert!(matches!(
        ping.payload,
        Some(proto::envelope::Payload::Ping(proto::Ping { nonce: 99 }))
    ));

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn remote_config_prepare_and_restore_failure_reports_partial_save() {
    let events = Arc::new(Mutex::new(Vec::new()));
    let repository = FakeConfigRepository::with_control(
        SaveBehavior::Success,
        true,
        Arc::clone(&events),
    );
    let repository_state = Arc::clone(&repository.state);
    let host = FakeHostControl::with_control(true, 0, Arc::clone(&events));
    let (handle, actor) = spawn_remote_config_actor(repository, Some(host));

    let response = handle.handle(save_request(37, "current-sha")).await;
    let Some(proto::envelope::Payload::Error(error)) = response.payload else {
        panic!("expected combined internal error");
    };
    assert_eq!(error.code, proto::ErrorCode::Internal as i32);
    assert!(error.message.contains("fixture prepare failure"));
    assert!(error.message.contains("fixture restore failure"));
    assert!(error.message.contains("已保存"));
    let repository_state = repository_state.lock().unwrap();
    assert_eq!(repository_state.save_calls, 1);
    assert_eq!(repository_state.restore_calls, 1);
    assert_eq!(repository_state.current.version_sha256, "saved-sha");
    drop(repository_state);
    assert_eq!(
        events.lock().unwrap().as_slice(),
        ["snapshot", "save", "prepare", "restore"]
    );

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[tokio::test]
async fn remote_config_commit_failure_keeps_pending_for_same_request_retry() {
    let events = Arc::new(Mutex::new(Vec::new()));
    let repository = FakeConfigRepository::with_control(
        SaveBehavior::Success,
        false,
        Arc::clone(&events),
    );
    let repository_state = Arc::clone(&repository.state);
    let host = FakeHostControl::with_control(false, 2, Arc::clone(&events));
    let host_state = Arc::clone(&host.state);
    let (handle, actor) = spawn_remote_config_actor(repository, Some(host));

    let response = handle.handle(save_request(41, "current-sha")).await;
    let Some(proto::envelope::Payload::NodeRestartAccepted(accepted)) = response.payload else {
        panic!("expected restart acceptance");
    };
    assert_eq!(accepted.machine_id, "ab".repeat(32));
    assert_eq!(accepted.saved_version_sha256, "saved-sha");
    assert_eq!(repository_state.lock().unwrap().save_calls, 1);
    assert_eq!(host_state.lock().unwrap().prepared_versions, ["saved-sha"]);

    let busy = handle.handle(save_request(42, "saved-sha")).await;
    assert_error_code(busy, proto::ErrorCode::Conflict);
    assert_eq!(repository_state.lock().unwrap().save_calls, 1);
    assert_eq!(host_state.lock().unwrap().prepared_versions.len(), 1);

    handle.response_flushed(999).await.unwrap();
    assert_eq!(host_state.lock().unwrap().commit_calls, 0);
    let first_commit = handle.response_flushed(41).await.unwrap_err();
    assert!(first_commit.contains("fixture commit failure"));
    assert_eq!(host_state.lock().unwrap().commit_calls, 1);
    let still_busy = handle.handle(save_request(43, "saved-sha")).await;
    assert_error_code(still_busy, proto::ErrorCode::Conflict);
    assert_eq!(repository_state.lock().unwrap().save_calls, 1);
    assert_eq!(host_state.lock().unwrap().prepared_versions.len(), 1);
    let second_commit = handle.response_flushed(41).await.unwrap_err();
    assert!(second_commit.contains("fixture commit failure"));
    assert_eq!(host_state.lock().unwrap().commit_calls, 2);
    let still_busy_after_retry = handle.handle(save_request(44, "saved-sha")).await;
    assert_error_code(still_busy_after_retry, proto::ErrorCode::Conflict);
    handle.response_flushed(41).await.unwrap();
    assert_eq!(host_state.lock().unwrap().commit_calls, 3);
    handle.response_flushed(41).await.unwrap();
    assert_eq!(host_state.lock().unwrap().commit_calls, 3);
    assert_eq!(
        events.lock().unwrap().as_slice(),
        [
            "snapshot", "save", "prepare", "commit", "commit", "commit"
        ]
    );

    handle.shutdown().await.unwrap();
    actor.await.unwrap();
}

#[derive(Clone, Copy)]
enum SaveBehavior {
    Success,
    Conflict,
    PathFailure,
}

struct FakeRepositoryState {
    current: NodeConfigState,
    behavior: SaveBehavior,
    load_calls: usize,
    save_calls: usize,
    restore_calls: usize,
    fail_restore: bool,
}

struct FakeConfigRepository {
    state: Arc<Mutex<FakeRepositoryState>>,
    events: Arc<Mutex<Vec<&'static str>>>,
}

impl FakeConfigRepository {
    fn new(behavior: SaveBehavior) -> Self {
        Self::with_control(behavior, false, Arc::new(Mutex::new(Vec::new())))
    }

    fn with_control(
        behavior: SaveBehavior,
        fail_restore: bool,
        events: Arc<Mutex<Vec<&'static str>>>,
    ) -> Self {
        Self {
            state: Arc::new(Mutex::new(FakeRepositoryState {
                current: NodeConfigState::for_test(remote_config_fixture(), "current-sha"),
                behavior,
                load_calls: 0,
                save_calls: 0,
                restore_calls: 0,
                fail_restore,
            })),
            events,
        }
    }
}

impl NodeConfigRepositoryAccess for FakeConfigRepository {
    fn snapshot(&self) -> Result<NodeConfigState, ConfigRepositoryError> {
        self.events.lock().unwrap().push("snapshot");
        let mut state = self.state.lock().unwrap();
        state.load_calls += 1;
        Ok(state.current.clone())
    }

    fn save_if_version(
        &self,
        expected_version_sha256: &str,
        config: &NodeConfig,
    ) -> Result<NodeConfigState, ConfigRepositoryError> {
        self.events.lock().unwrap().push("save");
        let mut state = self.state.lock().unwrap();
        state.save_calls += 1;
        match state.behavior {
            SaveBehavior::Success => {
                state.current = NodeConfigState::for_test(config.clone(), "saved-sha");
                Ok(state.current.clone())
            }
            SaveBehavior::Conflict => Err(ConfigRepositoryError::VersionConflict {
                expected: expected_version_sha256.into(),
                actual: "current-sha".into(),
            }),
            SaveBehavior::PathFailure => Err(ConfigRepositoryError::RepositoryControlPath {
                path: PathBuf::from(r"C:\node\bootstrap.toml"),
            }),
        }
    }

    fn restore_if_version(
        &self,
        expected_new_version_sha256: &str,
        previous: &NodeConfigState,
    ) -> Result<NodeConfigState, ConfigRepositoryError> {
        self.events.lock().unwrap().push("restore");
        let mut state = self.state.lock().unwrap();
        state.restore_calls += 1;
        if state.fail_restore {
            return Err(ConfigRepositoryError::InvalidJournal(
                "fixture restore failure",
            ));
        }
        if state.current.version_sha256 != expected_new_version_sha256 {
            return Err(ConfigRepositoryError::VersionConflict {
                expected: expected_new_version_sha256.into(),
                actual: state.current.version_sha256.clone(),
            });
        }
        state.current = previous.clone();
        Ok(state.current.clone())
    }
}

#[derive(Default)]
struct FakeHostState {
    prepared_versions: Vec<String>,
    commit_calls: usize,
    commit_failures_remaining: usize,
}

struct FakeHostControl {
    state: Arc<Mutex<FakeHostState>>,
    fail_prepare: bool,
    events: Arc<Mutex<Vec<&'static str>>>,
}

impl FakeHostControl {
    fn new(fail_prepare: bool) -> Self {
        Self::with_control(fail_prepare, 0, Arc::new(Mutex::new(Vec::new())))
    }

    fn with_control(
        fail_prepare: bool,
        commit_failures_remaining: usize,
        events: Arc<Mutex<Vec<&'static str>>>,
    ) -> Self {
        Self {
            state: Arc::new(Mutex::new(FakeHostState {
                prepared_versions: Vec::new(),
                commit_calls: 0,
                commit_failures_remaining,
            })),
            fail_prepare,
            events,
        }
    }
}

impl NodeHostControl for FakeHostControl {
    fn prepare_replacement(&self, saved_version: &str) -> Result<(), NodeHostControlError> {
        self.events.lock().unwrap().push("prepare");
        self.state
            .lock()
            .unwrap()
            .prepared_versions
            .push(saved_version.into());
        if self.fail_prepare {
            Err(NodeHostControlError::Failed(
                "fixture prepare failure".into(),
            ))
        } else {
            Ok(())
        }
    }

    fn commit_exit_after_response(&self) -> Result<(), NodeHostControlError> {
        self.events.lock().unwrap().push("commit");
        let mut state = self.state.lock().unwrap();
        state.commit_calls += 1;
        if state.commit_failures_remaining > 0 {
            state.commit_failures_remaining -= 1;
            Err(NodeHostControlError::Failed(
                "fixture commit failure".into(),
            ))
        } else {
            Ok(())
        }
    }
}

fn spawn_remote_config_actor<H: NodeHostControl + 'static>(
    repository: FakeConfigRepository,
    host: Option<H>,
) -> (NodeEngineHandle, tokio::task::JoinHandle<()>) {
    let machine = MachineId::parse(&"ab".repeat(32)).unwrap();
    let store = NodeStore::open_in_memory(machine).unwrap();
    NodeEngine::spawn_with_remote_config_for_test(
        store,
        "127.0.0.1:39091".parse().unwrap(),
        Path::new(r"C:\fixture\cache"),
        Box::new(repository),
        host.map(|host| Box::new(host) as Box<dyn NodeHostControl>),
    )
}

fn remote_config_fixture() -> NodeConfig {
    let mut config = NodeConfig::default();
    config.listen_ip = "127.0.0.9".parse().unwrap();
    config.port = 39123;
    config.worker_count = 6;
    config.enumerator = dedup_core::EnumeratorKind::WindowsWalker;
    config.paths.data_path = "relative/data".into();
    config.paths.config_path = "relative/config.toml".into();
    config.paths.log_path = r"D:\Node Logs".into();
    config.paths.cache_path = "relative/cache".into();
    config.read.hdd_threads_per_disk = 3;
    config.read.ssd_threads_per_disk = 5;
    config.read.unknown_threads_per_disk = 2;
    config.read.total_threads = 9;
    config.read.block_size_bytes = 2 * 1024 * 1024;
    config.read.block_timeout_seconds = 11;
    config.read.block_retries = 4;
    config.worker.mode = WorkerMode::Manual;
    config.worker.reserved_cores = 2;
    config.worker.manual_worker_count = 7;
    config
}

fn save_request(request_id: u64, expected_version: &str) -> proto::Envelope {
    envelope(
        request_id,
        proto::envelope::Payload::SaveNodeConfigAndRestart(proto::SaveNodeConfigAndRestart {
            expected_version_sha256: expected_version.into(),
            config: Some(proto::NodeConfigValue {
                listen_ip: "127.0.0.9".into(),
                port: 39123,
                enumerator: proto::NodeEnumerator::NodeWindowsWalker as i32,
                data_path: "relative/data".into(),
                config_path: "relative/config.toml".into(),
                log_path: r"D:\Node Logs".into(),
                cache_path: "relative/cache".into(),
                hdd_threads_per_disk: 3,
                ssd_threads_per_disk: 5,
                unknown_threads_per_disk: 2,
                total_threads: 9,
                block_size_bytes: 2 * 1024 * 1024,
                block_timeout_seconds: 11,
                block_retries: 4,
                legacy_worker_count: 6,
                worker_mode: proto::NodeWorkerMode::NodeWorkerManual as i32,
                reserved_cores: 2,
                manual_worker_count: 7,
            }),
        }),
    )
}

fn assert_error_code(response: proto::Envelope, expected: proto::ErrorCode) {
    assert!(matches!(
        response.payload,
        Some(proto::envelope::Payload::Error(proto::Error { code, .. })) if code == expected as i32
    ));
}

fn envelope(request_id: u64, payload: proto::envelope::Payload) -> proto::Envelope {
    proto::Envelope {
        request_id,
        payload: Some(payload),
    }
}

use std::{
    net::IpAddr,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::Duration,
};

use dedup_core::{AnalysisRunId, DesktopConfig, MachineId, NodeEndpoint};
use dedup_desktop_core::{
    analysis::CrossPollReport,
    app::{DesktopApp, UiCommand, UiEvent},
    central::CentralAnalysisStatus,
    runtime_tasks::{
        DesktopRuntimeTaskRegistry, DesktopRuntimeTaskState, RuntimeStageState, RuntimeTaskKey,
        RuntimeTaskOwner,
    },
    sync::{SyncPhase, SyncProgress, SyncTrigger, sync_trigger_channel},
    view_state::DesktopPaths,
};
use dedup_node_engine::server::{NodeRequestHandler, NodeServer};
use dedup_protocol::proto;
use tempfile::TempDir;
use tokio::sync::{broadcast, oneshot};

#[test]
fn cross_analysis_real_poll_shape_updates_seven_fixed_stages() {
    let registry = DesktopRuntimeTaskRegistry::new();
    let machines = [
        MachineId::from_sha256([0xd1; 32]),
        MachineId::from_sha256([0xd2; 32]),
    ];
    let reporter = registry.begin_cross_analysis("cross", &machines, "跨机器分析");
    reporter.update_cross_poll(
        &CrossPollReport {
            run_id: AnalysisRunId::new(),
            status: CentralAnalysisStatus::CollectingStage1,
            skipped_incomplete: 0,
            candidate_count: 0,
            unresolved_candidates: 0,
            phase2_task_count: 0,
        },
        2,
    );
    reporter.update_cross_poll(
        &CrossPollReport {
            run_id: AnalysisRunId::new(),
            status: CentralAnalysisStatus::Phase2Dispatched,
            skipped_incomplete: 0,
            candidate_count: 12,
            unresolved_candidates: 5,
            phase2_task_count: 2,
        },
        2,
    );
    reporter.update_cross_poll(
        &CrossPollReport {
            run_id: AnalysisRunId::new(),
            status: CentralAnalysisStatus::Completed,
            skipped_incomplete: 0,
            candidate_count: 12,
            unresolved_candidates: 0,
            phase2_task_count: 2,
        },
        2,
    );
    reporter.finish(DesktopRuntimeTaskState::Completed).unwrap();

    let details = registry.details(reporter.key()).unwrap();
    assert_eq!(details.stages.len(), 7);
    assert!(details.stages.iter().all(|stage| stage.state.is_terminal()));
    let candidates = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "stage1_screening")
        .unwrap();
    assert_eq!(candidates.unit, "candidate_pairs");
    assert_eq!(candidates.completed, 12);
    let wait = details
        .stages
        .iter()
        .find(|stage| stage.stage_id == "wait_nodes")
        .unwrap();
    assert_eq!(wait.unit, "nodes");
    assert_eq!(wait.total, Some(2));
}

#[test]
fn sync_progress_merges_active_machine_and_maps_ack_incremental_snapshot_caught_up() {
    let registry = DesktopRuntimeTaskRegistry::new();
    let machine = MachineId::from_sha256([0xd3; 32]);
    let reporter = registry.begin_or_merge_sync(&machine, "自动同步");
    let duplicate = registry.begin_or_merge_sync(&machine, "手动同步");
    assert_eq!(reporter.key(), duplicate.key());
    for (phase, committed, changes, pages) in [
        (SyncPhase::Acknowledging, 0, 0, 0),
        (SyncPhase::Incremental, 0, 4, 0),
        (SyncPhase::Snapshot, 0, 4, 3),
        (SyncPhase::CaughtUp, 9, 4, 3),
    ] {
        reporter.update_sync_progress(SyncProgress {
            trigger: SyncTrigger::Automatic,
            phase,
            committed_seq: committed,
            node_high_seq: 9,
            batch_count: 1,
            change_count: changes,
            snapshot_page_count: pages,
        });
    }
    reporter.finish(DesktopRuntimeTaskState::Completed).unwrap();

    let details = registry.details(reporter.key()).unwrap();
    assert_eq!(details.stages.len(), 4);
    assert!(
        details
            .stages
            .iter()
            .all(|stage| stage.state == RuntimeStageState::Completed)
    );
    assert_eq!(
        details
            .stages
            .iter()
            .find(|stage| stage.stage_id == "incremental")
            .unwrap()
            .completed,
        4
    );
    assert_eq!(
        details
            .stages
            .iter()
            .find(|stage| stage.stage_id == "snapshot")
            .unwrap()
            .completed,
        3
    );
    let next = registry.begin_or_merge_sync(&machine, "下一轮");
    assert_ne!(next.key(), reporter.key());
}

#[test]
fn delete_runtime_observes_confirmed_results_without_creating_or_expanding_commands() {
    let registry = DesktopRuntimeTaskRegistry::new();
    let machine_a = MachineId::from_sha256([0xd4; 32]);
    let machine_b = MachineId::from_sha256([0xd5; 32]);
    let reporter = registry.begin_delete("confirmed-delete", &[machine_a, machine_b], "删除", 2);
    reporter.mark_delete_prepared();
    let confirmed_results = vec![
        proto::DeleteItem {
            delete_item_id: "a".into(),
            outcome: "deleted".into(),
            ..Default::default()
        },
        proto::DeleteItem {
            delete_item_id: "b".into(),
            outcome: "failed".into(),
            message: "sharing violation".into(),
            ..Default::default()
        },
    ];
    reporter.finish_delete_results(&confirmed_results);
    reporter.finish(DesktopRuntimeTaskState::Failed).unwrap();

    let details = registry.details(reporter.key()).unwrap();
    assert_eq!(details.overall_total, Some(2));
    assert_eq!(details.overall_completed, 1);
    assert_eq!(details.overall_failed, 1);
    assert_eq!(details.failures.len(), 1);
    assert!(details.failures[0].message.contains("sharing violation"));
    assert_eq!(
        details
            .stages
            .iter()
            .find(|stage| stage.stage_id == "delete_items")
            .unwrap()
            .failed,
        1
    );
}

#[tokio::test]
async fn queued_sync_triggers_are_drained_into_the_active_runtime_row() {
    let (sender, mut receiver) = sync_trigger_channel(4);
    sender.connected().await.unwrap();
    assert_eq!(receiver.next().await, Some(SyncTrigger::Automatic));

    sender.manual().await.unwrap();
    sender.catch_up_tick().await.unwrap();
    assert_eq!(receiver.drain_pending(), 2);
}

/// 控制器启动时先发布统一运行任务快照，保证 UI 从第一条事件起使用唯一数据源。
#[tokio::test(start_paused = true)]
async fn controller_publishes_initial_runtime_tasks_snapshot_before_view_snapshot() {
    let temp = TempDir::new().unwrap();
    let mut config = DesktopConfig::default();
    config.nodes.clear();
    let (app, mut events) = DesktopApp::start(config, desktop_paths(&temp));

    let first = events.recv().await.expect("启动应发布初始运行任务事件");
    match first {
        UiEvent::RuntimeTasksChanged(state) => {
            assert!(state.summaries().is_empty(), "空启动快照不应伪造运行任务");
        }
        other => panic!("启动第一条事件必须是 RuntimeTasksChanged，实际为 {other:?}"),
    }
    assert!(
        matches!(events.recv().await, Some(UiEvent::ViewChanged(_))),
        "统一运行任务快照之后才发布普通视图快照"
    );

    app.send(UiCommand::Shutdown).await.unwrap();
}

/// 可控 Node handler 记录列表与详情请求，并向真实 TCP 会话推送终态事件。
#[derive(Clone)]
struct RuntimeTaskHandler {
    /// 握手后状态接口报告的物理机器身份。
    machine_id: MachineId,
    /// 列表请求次数，用于验证固定两秒刷新节奏。
    list_calls: Arc<AtomicUsize>,
    /// 详情请求次数，用于验证只拉取当前选中任务。
    detail_calls: Arc<AtomicUsize>,
    /// 每条管理连接各自订阅的运行任务终态广播。
    changes: broadcast::Sender<proto::RuntimeTaskChanged>,
    /// 测试运行任务列表传输失败后不回填旧摘要。
    fail_runtime_list: Arc<AtomicBool>,
    /// 测试当前选中详情失败时保留详情并标记 stale。
    fail_runtime_details: Arc<AtomicBool>,
}

impl NodeRequestHandler for RuntimeTaskHandler {
    async fn handle(&self, request: proto::Envelope) -> proto::Envelope {
        let payload = match request.payload {
            Some(proto::envelope::Payload::NodeStatus(_)) => {
                proto::envelope::Payload::NodeStatus(proto::NodeStatus {
                    machine_id: self.machine_id.as_str().into(),
                    listen_address: "127.0.0.1".into(),
                    ..Default::default()
                })
            }
            Some(proto::envelope::Payload::ListTasks(mut page)) => {
                page.tasks.clear();
                page.next_cursor.clear();
                proto::envelope::Payload::ListTasks(page)
            }
            Some(proto::envelope::Payload::ListRuntimeTasks(mut page)) => {
                self.list_calls.fetch_add(1, Ordering::SeqCst);
                if self.fail_runtime_list.load(Ordering::SeqCst) {
                    proto::envelope::Payload::Error(proto::Error {
                        code: proto::ErrorCode::Internal as i32,
                        message: "模拟运行任务列表传输失败".into(),
                    })
                } else {
                    page.tasks = vec![runtime_summary(self.machine_id.as_str())];
                    page.next_cursor.clear();
                    proto::envelope::Payload::ListRuntimeTasks(page)
                }
            }
            Some(proto::envelope::Payload::GetRuntimeTaskDetails(mut response)) => {
                self.detail_calls.fetch_add(1, Ordering::SeqCst);
                if self.fail_runtime_details.load(Ordering::SeqCst) {
                    proto::envelope::Payload::Error(proto::Error {
                        code: proto::ErrorCode::Internal as i32,
                        message: "模拟运行任务详情传输失败".into(),
                    })
                } else {
                    response.details = Some(proto::RuntimeTaskDetails {
                        summary: Some(runtime_summary(self.machine_id.as_str())),
                        stages: Vec::new(),
                        workers: Vec::new(),
                        failures: Vec::new(),
                        execution_config: None,
                        pipeline_metrics: None,
                    });
                    proto::envelope::Payload::GetRuntimeTaskDetails(response)
                }
            }
            _ => proto::envelope::Payload::Error(proto::Error {
                code: proto::ErrorCode::InvalidRequest as i32,
                message: "测试节点只提供运行任务查询".into(),
            }),
        };
        proto::Envelope {
            request_id: request.request_id,
            payload: Some(payload),
        }
    }

    fn subscribe_runtime_events(&self) -> Option<broadcast::Receiver<proto::RuntimeTaskChanged>> {
        Some(self.changes.subscribe())
    }
}

/// 暂停时钟精确验证 2 秒 tick、按需详情和终态事件立即刷新。
#[tokio::test(start_paused = true)]
async fn controller_refreshes_runtime_tasks_on_two_second_tick_and_terminal_event() {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let machine_id = MachineId::from_sha256([0xe1; 32]);
    let list_calls = Arc::new(AtomicUsize::new(0));
    let detail_calls = Arc::new(AtomicUsize::new(0));
    let fail_runtime_list = Arc::new(AtomicBool::new(false));
    let fail_runtime_details = Arc::new(AtomicBool::new(false));
    let (changes, _) = broadcast::channel(8);
    let (shutdown_sender, shutdown) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        RuntimeTaskHandler {
            machine_id: machine_id.clone(),
            list_calls: Arc::clone(&list_calls),
            detail_calls: Arc::clone(&detail_calls),
            changes: changes.clone(),
            fail_runtime_list,
            fail_runtime_details,
        },
        shutdown,
    ));
    let temp = TempDir::new().unwrap();
    let config = DesktopConfig {
        nodes: vec![NodeEndpoint {
            ip: IpAddr::from([127, 0, 0, 1]),
            port: address.port(),
        }],
        reconnect_interval_seconds: 30,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(config, desktop_paths(&temp));

    wait_for_count(&list_calls, 1).await;
    let baseline = list_calls.load(Ordering::SeqCst);
    assert_eq!(detail_calls.load(Ordering::SeqCst), 0);

    tokio::time::advance(Duration::from_millis(1_999)).await;
    tokio::task::yield_now().await;
    assert_eq!(list_calls.load(Ordering::SeqCst), baseline);
    tokio::time::advance(Duration::from_millis(1)).await;
    wait_for_count(&list_calls, baseline + 1).await;

    app.send(UiCommand::SelectRuntimeTask {
        key: RuntimeTaskKey {
            owner: RuntimeTaskOwner::Node { node_index: 0 },
            id: "node-runtime".into(),
        },
    })
    .await
    .unwrap();
    wait_for_count(&detail_calls, 1).await;
    let before_event = list_calls.load(Ordering::SeqCst);
    changes
        .send(proto::RuntimeTaskChanged {
            runtime_task_id: "node-runtime".into(),
            state: "completed".into(),
        })
        .unwrap();
    wait_for_count(&list_calls, before_event + 1).await;

    let mut observed_selected_details = false;
    while let Ok(event) = events.try_recv() {
        if let UiEvent::RuntimeTasksChanged(state) = event {
            observed_selected_details |= state.selected().is_some() && state.details().is_some();
        }
    }
    assert!(observed_selected_details, "选中任务应立即发布详情状态");

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
}

/// 列表传输失败不能把旧摘要当作当前任务；只有已选详情允许保留并 stale。
#[tokio::test(start_paused = true)]
async fn runtime_list_failure_removes_old_summary_and_only_selected_details_stale() {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let machine_id = MachineId::from_sha256([0xe2; 32]);
    let list_calls = Arc::new(AtomicUsize::new(0));
    let detail_calls = Arc::new(AtomicUsize::new(0));
    let fail_runtime_list = Arc::new(AtomicBool::new(false));
    let fail_runtime_details = Arc::new(AtomicBool::new(false));
    let (changes, _) = broadcast::channel(8);
    let (shutdown_sender, shutdown) = oneshot::channel();
    let server = tokio::spawn(NodeServer::serve_until(
        listener,
        RuntimeTaskHandler {
            machine_id,
            list_calls: Arc::clone(&list_calls),
            detail_calls: Arc::clone(&detail_calls),
            changes,
            fail_runtime_list: Arc::clone(&fail_runtime_list),
            fail_runtime_details: Arc::clone(&fail_runtime_details),
        },
        shutdown,
    ));
    let temp = TempDir::new().unwrap();
    let config = DesktopConfig {
        nodes: vec![NodeEndpoint {
            ip: IpAddr::from([127, 0, 0, 1]),
            port: address.port(),
        }],
        reconnect_interval_seconds: 30,
        ..DesktopConfig::default()
    };
    let (app, mut events) = DesktopApp::start(config, desktop_paths(&temp));
    wait_for_count(&list_calls, 1).await;
    app.send(UiCommand::SelectRuntimeTask {
        key: RuntimeTaskKey {
            owner: RuntimeTaskOwner::Node { node_index: 0 },
            id: "node-runtime".into(),
        },
    })
    .await
    .unwrap();
    wait_for_count(&detail_calls, 1).await;

    fail_runtime_list.store(true, Ordering::SeqCst);
    fail_runtime_details.store(true, Ordering::SeqCst);
    let expected_list_calls = list_calls.load(Ordering::SeqCst) + 1;
    tokio::time::advance(Duration::from_secs(2)).await;
    wait_for_count(&list_calls, expected_list_calls).await;
    wait_for_count(&detail_calls, 2).await;

    let failed = tokio::time::timeout(Duration::from_secs(1), async {
        loop {
            if let Some(UiEvent::RuntimeTasksChanged(state)) = events.recv().await
                && state.summaries().is_empty()
                && state.selected().is_some()
                && state.details().is_some()
                && state.is_stale()
            {
                return state;
            }
        }
    })
    .await
    .expect("列表失败必须发布空摘要和 stale 的旧详情");
    assert!(
        failed
            .error()
            .is_some_and(|error| error.contains("详情失败")),
        "stale 只能来自当前选中详情的独立失败"
    );

    app.send(UiCommand::Shutdown).await.unwrap();
    shutdown_sender.send(()).unwrap();
    server.await.unwrap().unwrap();
}

/// 返回一个稳定 Node 运行任务摘要。
fn runtime_summary(machine_id: &str) -> proto::RuntimeTaskSummary {
    proto::RuntimeTaskSummary {
        runtime_task_id: "node-runtime".into(),
        machine_id: machine_id.into(),
        task_kind: "scan".into(),
        title: "节点扫描".into(),
        state: "running".into(),
        stage_summary: "读取与 MD5".into(),
        overall_completed: 1,
        overall_total: 2,
        overall_total_known: true,
        overall_failed: 0,
        overall_skipped: 0,
    }
}

/// 在暂停时钟下只让出调度权，避免等待逻辑偷偷推进固定 tick。
async fn wait_for_count(counter: &AtomicUsize, expected: usize) {
    for _ in 0..1_000 {
        if counter.load(Ordering::SeqCst) >= expected {
            return;
        }
        tokio::task::yield_now().await;
    }
    panic!("计数未达到 {expected}");
}

/// 构造隔离的 Desktop 路径，不读写用户目录。
fn desktop_paths(temp: &TempDir) -> DesktopPaths {
    DesktopPaths {
        data: temp.path().to_path_buf(),
        logs: temp.path().join("logs"),
        cache: temp.path().join("cache"),
        config: temp.path().join("config.toml"),
    }
}

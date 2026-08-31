//! GUI 退出后的双 Node 只读终态观察器协议契约。

#[path = "../examples/physical_two_host_observer.rs"]
#[allow(dead_code)]
mod observer;

use std::{path::Path, time::Duration};

use dedup_core::{NodeEndpoint, product_id};
use dedup_protocol::{PROTOCOL_VERSION, proto};
use dedup_transport::{FrameClass, FrameReader, FrameWriter};
use prost::Message;
use serde_json::Value;
use tempfile::TempDir;
use tokio::{net::TcpListener, sync::oneshot, time::timeout};

/// 防止观察器并发占用两个节点或混入任何会改变节点状态的协议请求。
#[tokio::test(flavor = "current_thread")]
async fn observer_reads_two_nodes_in_order_with_only_read_frames() {
    let first = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let second = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let first_endpoint = endpoint(first.local_addr().unwrap());
    let second_endpoint = endpoint(second.local_addr().unwrap());
    let (first_finished, first_done) = oneshot::channel();
    let first_server = tokio::spawn(serve_snapshot(first, "a1", true, Some(first_finished)));
    let second_server = tokio::spawn(serve_snapshot_after_first(second, "b2", first_done));
    let evidence = TempDir::new().unwrap();
    let config = observer::ObserverConfig::new(
        first_endpoint,
        second_endpoint,
        evidence.path(),
        "terminal.ndjson",
    )
    .unwrap();

    let result = observer::run_observer(config).await.unwrap();

    assert_eq!(result.status, "completed");
    first_server.await.unwrap();
    second_server.await.unwrap();

    let records = read_records(&evidence.path().join("terminal.ndjson"));
    assert_eq!(
        record_types(&records),
        [
            "observer_start",
            "node_snapshot",
            "node_snapshot",
            "observer_result"
        ]
    );
    assert_eq!(records[1]["machine_id"], "a1".repeat(32));
    assert_eq!(records[2]["machine_id"], "b2".repeat(32));
    assert_eq!(records[1]["latest_persistent_task"]["available"], false);
    assert_eq!(
        records[1]["latest_persistent_task"]["reason"],
        "协议未提供任务创建时间或最新排序语义"
    );
    assert_eq!(
        records[1]["runtime_tasks"][0]["pipeline_metrics"]["disk_reads"][0]["physical_disk_id"],
        "disk-a"
    );
}

/// 防止 GUI 仍持有第一个节点管理连接时继续连接第二节点，掩盖唯一连接诊断。
#[tokio::test(flavor = "current_thread")]
async fn observer_stops_after_node_busy_and_writes_stable_diagnosis() {
    let busy = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let untouched = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let busy_endpoint = endpoint(busy.local_addr().unwrap());
    let untouched_endpoint = endpoint(untouched.local_addr().unwrap());
    let busy_server = tokio::spawn(serve_busy(busy));
    let untouched_server =
        tokio::spawn(async move { timeout(Duration::from_millis(200), untouched.accept()).await });
    let evidence = TempDir::new().unwrap();
    let config = observer::ObserverConfig::new(
        busy_endpoint,
        untouched_endpoint,
        evidence.path(),
        "busy.ndjson",
    )
    .unwrap();

    let error = observer::run_observer(config).await.unwrap_err();

    assert_eq!(error.code(), "node_busy");
    busy_server.await.unwrap();
    assert!(
        untouched_server.await.unwrap().is_err(),
        "NodeBusy 后不得连接第二节点"
    );
    let records = read_records(&evidence.path().join("busy.ndjson"));
    assert_eq!(
        record_types(&records),
        ["observer_start", "observer_error", "observer_result"]
    );
    assert_eq!(records[1]["code"], "node_busy");
    assert_eq!(
        records[1]["message"],
        "节点正被 GUI 的唯一管理连接占用；请完全退出 GUI 后再观察"
    );
    assert_eq!(records[2]["status"], "failed");
}

/// 启动一个完整回应只读观察请求的 loopback 节点，并断言接收帧的严格白名单和顺序。
async fn serve_snapshot(
    listener: TcpListener,
    machine_seed: &str,
    include_runtime: bool,
    finished: Option<oneshot::Sender<()>>,
) {
    let (stream, _) = listener.accept().await.unwrap();
    serve_snapshot_stream(stream, machine_seed, include_runtime).await;
    if let Some(finished) = finished {
        finished.send(()).unwrap();
    }
}

/// 仅在第一节点完整关闭会话后才接受第二连接，从而捕获并发连接回归。
async fn serve_snapshot_after_first(
    listener: TcpListener,
    machine_seed: &str,
    first_done: oneshot::Receiver<()>,
) {
    tokio::select! {
        biased;
        connection = listener.accept() => {
            let _ = connection.unwrap();
            panic!("第一节点未完成只读快照前观察器连接了第二节点");
        }
        result = first_done => result.unwrap(),
    }
    serve_snapshot(listener, machine_seed, true, None).await;
}

/// 在已接收的 loopback 流上回应只读观察协议帧。
async fn serve_snapshot_stream(
    stream: tokio::net::TcpStream,
    machine_seed: &str,
    include_runtime: bool,
) {
    let (read, write) = stream.into_split();
    let mut reader = FrameReader::new(read);
    let mut writer = FrameWriter::new(write);
    let mut received = Vec::new();
    loop {
        let Ok(frame) = reader.read_frame().await else {
            break;
        };
        let request = proto::Envelope::decode(frame.as_slice()).unwrap();
        let payload = request.payload.unwrap();
        received.push(payload_name(&payload));
        let response = match payload {
            proto::envelope::Payload::Hello(hello) => {
                assert_eq!(hello.protocol_version, PROTOCOL_VERSION);
                assert_eq!(hello.product_id, product_id());
                proto::envelope::Payload::Hello(proto::Hello {
                    protocol_version: PROTOCOL_VERSION,
                    product_id: product_id().into(),
                    peer_name: "node-fixture".into(),
                })
            }
            proto::envelope::Payload::NodeStatus(_) => {
                proto::envelope::Payload::NodeStatus(proto::NodeStatus {
                    machine_id: machine_seed.repeat(32),
                    listen_address: "127.0.0.1:0".into(),
                    worker_count: 4,
                    busy_workers: 1,
                    queued_items: 2,
                    running_items: 1,
                    outbox_high_seq: 99,
                    engine_restarting: false,
                })
            }
            proto::envelope::Payload::ListTasks(_) => {
                proto::envelope::Payload::ListTasks(proto::ListTasks {
                    tasks: vec![proto::TaskSummary {
                        task_id: "task-1".into(),
                        task_kind: "scan".into(),
                        state: proto::TaskState::TaskCompleted as i32,
                        total_items: 3,
                        completed_items: 3,
                        failed_items: 0,
                        skipped_items: 0,
                        outbox_high_seq: 98,
                    }],
                    ..Default::default()
                })
            }
            proto::envelope::Payload::ListRuntimeTasks(_) if include_runtime => {
                proto::envelope::Payload::ListRuntimeTasks(proto::ListRuntimeTasks {
                    tasks: vec![proto::RuntimeTaskSummary {
                        runtime_task_id: "runtime-1".into(),
                        machine_id: machine_seed.repeat(32),
                        task_kind: "base_compute".into(),
                        title: "扫描".into(),
                        state: "completed".into(),
                        stage_summary: "收尾".into(),
                        overall_completed: 3,
                        overall_total: 3,
                        overall_total_known: true,
                        overall_failed: 0,
                        overall_skipped: 0,
                        outbox_high_seq: Some(99),
                    }],
                    ..Default::default()
                })
            }
            proto::envelope::Payload::GetRuntimeTaskDetails(request) if include_runtime => {
                assert_eq!(request.runtime_task_id, "runtime-1");
                proto::envelope::Payload::GetRuntimeTaskDetails(proto::GetRuntimeTaskDetails {
                    runtime_task_id: request.runtime_task_id,
                    details: Some(runtime_details(machine_seed)),
                })
            }
            _ => panic!("观察器发送了非只读或不符合顺序的帧"),
        };
        writer
            .write_frame(
                &proto::Envelope {
                    request_id: request.request_id,
                    payload: Some(response),
                }
                .encode_to_vec(),
                FrameClass::Ordinary,
            )
            .await
            .unwrap();
    }
    assert_eq!(
        received,
        [
            "hello",
            "node_status",
            "node_status",
            "list_tasks",
            "list_runtime_tasks",
            "get_runtime_task_details"
        ]
    );
}

/// 启动返回 `NodeBusy` 的节点替身。
async fn serve_busy(listener: TcpListener) {
    let (stream, _) = listener.accept().await.unwrap();
    let (read, write) = stream.into_split();
    let mut reader = FrameReader::new(read);
    let mut writer = FrameWriter::new(write);
    let request = proto::Envelope::decode(reader.read_frame().await.unwrap().as_slice()).unwrap();
    assert!(matches!(
        request.payload,
        Some(proto::envelope::Payload::Hello(_))
    ));
    writer
        .write_frame(
            &proto::Envelope {
                request_id: request.request_id,
                payload: Some(proto::envelope::Payload::Error(proto::Error {
                    code: proto::ErrorCode::NodeBusy as i32,
                    message: "occupied".into(),
                })),
            }
            .encode_to_vec(),
            FrameClass::Ordinary,
        )
        .await
        .unwrap();
}

/// 构造运行详情中的真实资源和逐盘指标夹具。
fn runtime_details(machine_seed: &str) -> proto::RuntimeTaskDetails {
    proto::RuntimeTaskDetails {
        summary: Some(proto::RuntimeTaskSummary {
            runtime_task_id: "runtime-1".into(),
            machine_id: machine_seed.repeat(32),
            task_kind: "base_compute".into(),
            title: "扫描".into(),
            state: "completed".into(),
            stage_summary: "收尾".into(),
            overall_completed: 3,
            overall_total: 3,
            overall_total_known: true,
            overall_failed: 0,
            overall_skipped: 0,
            outbox_high_seq: Some(99),
        }),
        stages: vec![proto::RuntimeStageDetails {
            stage_id: "finalize".into(),
            display_name: "收尾".into(),
            state: proto::RuntimeStageState::RuntimeStageCompleted as i32,
            unit: "files".into(),
            completed: 3,
            total: 3,
            total_known: true,
            failed: 0,
            skipped: 0,
            speed_per_second: 1.0,
            elapsed_ms: 20,
            eta_ms: None,
        }],
        pipeline_metrics: Some(proto::RuntimePipelineMetrics {
            hash_io: Some(proto::RuntimeResourceMetrics {
                current: Some(1),
                peak: Some(2),
                capacity: Some(4),
                ..Default::default()
            }),
            disk_reads: vec![proto::RuntimeDiskReadMetrics {
                physical_disk_id: "disk-a".into(),
                capacity: Some(2),
                hash_waiting: Some(0),
                media_waiting: Some(0),
                hash_active: Some(1),
                media_active: Some(0),
                hash_granted_total: Some(3),
                media_granted_total: Some(0),
                hash_released_total: Some(3),
                media_released_total: Some(0),
            }],
            ..Default::default()
        }),
        ..Default::default()
    }
}

/// 读取观察器实际写出的 NDJSON 记录。
fn read_records(path: &Path) -> Vec<Value> {
    std::fs::read_to_string(path)
        .unwrap()
        .lines()
        .map(|line| serde_json::from_str(line).unwrap())
        .collect()
}

/// 提取记录种类，避免测试依赖记录的内部字段布局。
fn record_types(records: &[Value]) -> Vec<&str> {
    records
        .iter()
        .map(|record| record["record_type"].as_str().unwrap())
        .collect()
}

/// 把 loopback 端点转换成与生产入口一致的手工节点配置。
fn endpoint(address: std::net::SocketAddr) -> NodeEndpoint {
    NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    }
}

/// 将协议 oneof 映射为行为断言使用的稳定名称。
fn payload_name(payload: &proto::envelope::Payload) -> &'static str {
    match payload {
        proto::envelope::Payload::Hello(_) => "hello",
        proto::envelope::Payload::NodeStatus(_) => "node_status",
        proto::envelope::Payload::ListTasks(_) => "list_tasks",
        proto::envelope::Payload::ListRuntimeTasks(_) => "list_runtime_tasks",
        proto::envelope::Payload::GetRuntimeTaskDetails(_) => "get_runtime_task_details",
        _ => "forbidden",
    }
}

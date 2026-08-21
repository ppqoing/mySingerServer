//! 运行时任务详情与持久 TaskSummary 严格分离的 descriptor/wire 契约。

use dedup_protocol::{FILE_DESCRIPTOR_SET, MAX_RUNTIME_FAILURES, proto};
use prost::Message;
use prost_types::{DescriptorProto, FileDescriptorSet};

#[test]
fn runtime_task_messages_round_trip_parallel_stages_workers_and_failures() {
    let summary = proto::RuntimeTaskSummary {
        runtime_task_id: "runtime-1".into(),
        machine_id: "ab".repeat(32),
        task_kind: "scan".into(),
        title: "扫描".into(),
        state: "running".into(),
        stage_summary: "读取与 MD5 / 媒体探测与一筛并行".into(),
        overall_completed: 58,
        overall_total: 100,
        overall_total_known: true,
        overall_failed: 1,
        overall_skipped: 2,
    };
    let details = proto::RuntimeTaskDetails {
        summary: Some(summary.clone()),
        stages: vec![proto::RuntimeStageDetails {
            stage_id: "read_md5".into(),
            display_name: "读取与 MD5".into(),
            state: proto::RuntimeStageState::RuntimeStageRunning as i32,
            unit: "bytes".into(),
            completed: 4096,
            total: 8192,
            total_known: true,
            failed: 1,
            skipped: 0,
            speed_per_second: 2048.0,
            elapsed_ms: 2500,
            eta_ms: Some(2000),
        }],
        workers: vec![proto::RuntimeWorkerDetails {
            slot: 2,
            process_id: Some(4321),
            stage_id: "probe_stage1".into(),
            display_path: r"D:\Media\clip.mp4".into(),
            physical_disk_id: "PhysicalDisk7".into(),
            completed_files: 18,
            speed_per_second: 3.5,
        }],
        failures: vec![proto::RuntimeFailureDetails {
            stage_id: "read_md5".into(),
            display_path: r"D:\Media\broken.mp4".into(),
            message: "疑似物理读取故障".into(),
        }],
    };
    let envelope = proto::Envelope {
        request_id: 9,
        payload: Some(proto::envelope::Payload::GetRuntimeTaskDetails(
            proto::GetRuntimeTaskDetails {
                runtime_task_id: "runtime-1".into(),
                details: Some(details.clone()),
            },
        )),
    };
    let decoded = proto::Envelope::decode(envelope.encode_to_vec().as_slice()).unwrap();
    let Some(proto::envelope::Payload::GetRuntimeTaskDetails(decoded)) = decoded.payload else {
        panic!("应解码运行时详情");
    };
    assert_eq!(decoded.details, Some(details));

    let list = proto::ListRuntimeTasks {
        cursor: String::new(),
        limit: 100,
        tasks: vec![summary],
        next_cursor: "next".into(),
    };
    assert_eq!(
        proto::ListRuntimeTasks::decode(list.encode_to_vec().as_slice()).unwrap(),
        list
    );
    assert_eq!(MAX_RUNTIME_FAILURES, 20);
}

#[test]
fn descriptor_exposes_runtime_details_after_fault_tags_without_polluting_task_summary() {
    let descriptor = FileDescriptorSet::decode(FILE_DESCRIPTOR_SET).unwrap();
    let file = descriptor
        .file
        .iter()
        .find(|file| file.package.as_deref() == Some("mysingerserver.v2"))
        .unwrap();
    let messages = &file.message_type;
    for name in [
        "ListRuntimeTasks",
        "GetRuntimeTaskDetails",
        "RuntimeTaskSummary",
        "RuntimeTaskDetails",
        "RuntimeStageDetails",
        "RuntimeWorkerDetails",
        "RuntimeFailureDetails",
        "RuntimeTaskChanged",
    ] {
        assert!(message(messages, name).is_some(), "descriptor 缺少 {name}");
    }

    let envelope = message(messages, "Envelope").unwrap();
    for (name, number) in [
        ("list_runtime_tasks", 43),
        ("get_runtime_task_details", 44),
        ("runtime_task_changed", 45),
    ] {
        assert!(
            envelope
                .field
                .iter()
                .any(|field| field.name.as_deref() == Some(name) && field.number == Some(number)),
            "Envelope 缺少 {name}={number}"
        );
    }

    assert_fields(
        message(messages, "RuntimeTaskSummary").unwrap(),
        &[
            "runtime_task_id",
            "machine_id",
            "task_kind",
            "title",
            "state",
            "stage_summary",
            "overall_completed",
            "overall_total",
            "overall_total_known",
            "overall_failed",
            "overall_skipped",
        ],
    );
    assert_fields(
        message(messages, "RuntimeStageDetails").unwrap(),
        &[
            "stage_id",
            "display_name",
            "state",
            "unit",
            "completed",
            "total",
            "total_known",
            "failed",
            "skipped",
            "speed_per_second",
            "elapsed_ms",
            "eta_ms",
        ],
    );
    assert_fields(
        message(messages, "RuntimeWorkerDetails").unwrap(),
        &[
            "slot",
            "process_id",
            "stage_id",
            "display_path",
            "physical_disk_id",
            "completed_files",
            "speed_per_second",
        ],
    );

    let persistent = message(messages, "TaskSummary").unwrap();
    assert_fields(
        persistent,
        &[
            "task_id",
            "task_kind",
            "state",
            "total_items",
            "completed_items",
            "failed_items",
            "skipped_items",
            "outbox_high_seq",
        ],
    );
    for forbidden in ["path", "worker", "speed", "physical_disk", "stage_summary"] {
        assert!(
            persistent
                .field
                .iter()
                .all(|field| !field.name.as_deref().unwrap_or_default().contains(forbidden)),
            "持久 TaskSummary 不得加入运行时字段 {forbidden}"
        );
    }
    assert!(message(messages, "TaskEvent").is_some(), "旧 TaskEvent 必须保留");
}

fn assert_fields(message: &DescriptorProto, expected: &[&str]) {
    let actual = message
        .field
        .iter()
        .filter_map(|field| field.name.as_deref())
        .collect::<Vec<_>>();
    assert_eq!(actual, expected, "{} 字段集合漂移", message.name());
}

fn message<'a>(messages: &'a [DescriptorProto], name: &str) -> Option<&'a DescriptorProto> {
    messages
        .iter()
        .find(|message| message.name.as_deref() == Some(name))
}

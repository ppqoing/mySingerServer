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
        outbox_high_seq: Some(42),
        ..Default::default()
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
            current_step: "生成缩略图".into(),
            cache_detail: "复用本地缩略图".into(),
            phase: Some(proto::RuntimeWorkerPhase::RuntimeWorkerFeature as i32),
            cpu_weight: Some(3),
            decoder_threads: Some(3),
        }],
        failures: vec![proto::RuntimeFailureDetails {
            stage_id: "read_md5".into(),
            display_path: r"D:\Media\broken.mp4".into(),
            message: "疑似物理读取故障".into(),
        }],
        execution_config: Some(proto::RuntimeExecutionConfig {
            hash_tasks: Some(4),
            path_cache_queue_capacity: Some(2),
            content_cache_queue_capacity: Some(64),
            decode_queue_capacity: Some(64),
            persist_queue_capacity: Some(8),
            worker_slots: Some(4),
            cpu_budget: Some(12),
            global_disk_permits: Some(4),
            hdd_per_disk_permits: Some(1),
            ssd_per_disk_permits: Some(2),
            unknown_per_disk_permits: Some(1),
        }),
        pipeline_metrics: Some(proto::RuntimePipelineMetrics {
            hash_queue: Some(queue_metrics(2, 4, 4)),
            path_cache_queue: Some(queue_metrics(0, 2, 2)),
            content_cache_queue: Some(queue_metrics(3, 7, 64)),
            decode_queue: Some(queue_metrics(1, 5, 64)),
            persist_queue: Some(queue_metrics(0, 4, 8)),
            hash_io: Some(resource_metrics(2, 4, 4)),
            media_io: Some(resource_metrics(1, 3, 4)),
            cpu_weight: Some(resource_metrics(3, 9, 12)),
            worker_slots: Some(resource_metrics(1, 4, 4)),
            hash_bytes: Some(8_192),
            media_throughput: vec![proto::RuntimeMediaThroughput {
                media_kind: proto::MediaKind::MediaVideo as i32,
                size_bucket: "medium".into(),
                files: 2,
                bytes: 512 * 1024 * 1024,
            }],
            ..Default::default()
        }),
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
        "RuntimeExecutionConfig",
        "RuntimePipelineMetrics",
        "RuntimeDiskReadMetrics",
        "RuntimeOwnershipMetrics",
        "RuntimeQueueMetrics",
        "RuntimeResourceMetrics",
        "RuntimeLatencyHistogram",
        "RuntimeLatencyBucket",
        "RuntimeMediaThroughput",
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
            "outbox_high_seq",
        ],
    );
    let changed = message(messages, "RuntimeTaskChanged").unwrap();
    assert_fields(changed, &["runtime_task_id", "state", "outbox_high_seq"]);
    let changed_highwater = changed
        .field
        .iter()
        .find(|field| field.name.as_deref() == Some("outbox_high_seq"))
        .unwrap();
    assert_eq!(changed_highwater.number, Some(3));
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
            "current_step",
            "cache_detail",
            "phase",
            "cpu_weight",
            "decoder_threads",
        ],
    );

    let task_details = message(messages, "RuntimeTaskDetails").unwrap();
    let execution_config = task_details
        .field
        .iter()
        .find(|field| field.name.as_deref() == Some("execution_config"))
        .unwrap();
    let pipeline_metrics = task_details
        .field
        .iter()
        .find(|field| field.name.as_deref() == Some("pipeline_metrics"))
        .unwrap();
    assert_eq!(execution_config.number, Some(5));
    assert_eq!(pipeline_metrics.number, Some(6));

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
            persistent.field.iter().all(|field| !field
                .name
                .as_deref()
                .unwrap_or_default()
                .contains(forbidden)),
            "持久 TaskSummary 不得加入运行时字段 {forbidden}"
        );
    }
    assert!(
        message(messages, "TaskEvent").is_some(),
        "旧 TaskEvent 必须保留"
    );

    let ownership = message(messages, "RuntimeOwnershipMetrics").unwrap();
    assert_fields(ownership, &["current", "peak", "capacity"]);
    let pipeline = message(messages, "RuntimePipelineMetrics").unwrap();
    for (name, number) in [
        ("hash_waiting_permit", 12),
        ("hash_reading", 13),
        ("hash_completed_unjoined", 14),
        ("media_permit_waiting", 15),
        ("media_acquire_ready", 16),
        ("media_permit_ready", 17),
        ("worker_dispatching", 18),
        ("worker_start_pending", 19),
        ("worker_decode", 20),
        ("worker_feature", 21),
        ("worker_result_wait", 22),
        ("worker_phase_unknown", 23),
        ("content_output_credit_owned", 24),
        ("hash_refill_token_available", 25),
        ("decode_credit_owned", 26),
        ("item_completion_latency", 27),
    ] {
        let field = pipeline
            .field
            .iter()
            .find(|field| field.name.as_deref() == Some(name))
            .unwrap_or_else(|| panic!("RuntimePipelineMetrics 缺少 {name}"));
        assert!(
            field.number == Some(number),
            "RuntimePipelineMetrics 缺少 {name}={number}"
        );
        let expected_type = if number == 27 {
            ".mysingerserver.v2.RuntimeLatencyHistogram"
        } else {
            ".mysingerserver.v2.RuntimeOwnershipMetrics"
        };
        assert_eq!(
            field.type_name.as_deref(),
            Some(expected_type),
            "{name} wire shape 漂移"
        );
    }
    let disk_reads = pipeline
        .field
        .iter()
        .find(|field| field.name.as_deref() == Some("disk_reads"))
        .expect("RuntimePipelineMetrics 缺少 disk_reads");
    assert_eq!(disk_reads.number, Some(28));
    assert_eq!(
        disk_reads.type_name.as_deref(),
        Some(".mysingerserver.v2.RuntimeDiskReadMetrics")
    );
    assert!(disk_reads.label == Some(prost_types::field_descriptor_proto::Label::Repeated as i32));
    let disk_read = message(messages, "RuntimeDiskReadMetrics").unwrap();
    assert_fields(
        disk_read,
        &[
            "physical_disk_id",
            "capacity",
            "hash_waiting",
            "media_waiting",
            "hash_active",
            "media_active",
            "hash_granted_total",
            "media_granted_total",
            "hash_released_total",
            "media_released_total",
        ],
    );
    for (name, number) in [
        ("physical_disk_id", 1),
        ("capacity", 2),
        ("hash_waiting", 3),
        ("media_waiting", 4),
        ("hash_active", 5),
        ("media_active", 6),
        ("hash_granted_total", 7),
        ("media_granted_total", 8),
        ("hash_released_total", 9),
        ("media_released_total", 10),
    ] {
        assert!(
            disk_read.field.iter().any(|field| {
                field.name.as_deref() == Some(name) && field.number == Some(number)
            }),
            "RuntimeDiskReadMetrics 缺少 {name}={number}"
        );
    }
}

#[test]
fn legacy_runtime_details_leave_new_telemetry_absent() {
    let worker = proto::RuntimeWorkerDetails::decode([0x08, 0x02].as_slice()).unwrap();
    assert_eq!(worker.slot, 2);
    assert_eq!(worker.phase, None);
    assert_eq!(worker.cpu_weight, None);
    assert_eq!(worker.decoder_threads, None);

    let details = proto::RuntimeTaskDetails::decode(&[][..]).unwrap();
    assert_eq!(details.execution_config, None);
    assert_eq!(details.pipeline_metrics, None);
}

#[test]
fn runtime_pipeline_ownership_round_trip_preserves_tags_and_zero_presence() {
    let metrics = proto::RuntimePipelineMetrics {
        hash_queue: None,
        path_cache_queue: None,
        content_cache_queue: None,
        decode_queue: None,
        persist_queue: None,
        hash_io: None,
        media_io: None,
        cpu_weight: None,
        worker_slots: None,
        hash_bytes: None,
        media_throughput: Vec::new(),
        hash_waiting_permit: Some(ownership(1, 3, 4)),
        hash_reading: Some(ownership(2, 4, 4)),
        hash_completed_unjoined: Some(ownership(3, 5, 6)),
        media_permit_waiting: Some(ownership(4, 6, 7)),
        media_acquire_ready: Some(ownership(5, 7, 8)),
        media_permit_ready: Some(ownership(6, 8, 9)),
        worker_dispatching: Some(ownership(7, 9, 10)),
        worker_start_pending: Some(ownership(8, 10, 11)),
        worker_decode: Some(ownership(9, 11, 12)),
        worker_feature: Some(ownership(10, 12, 13)),
        worker_result_wait: Some(ownership(11, 13, 14)),
        worker_phase_unknown: Some(ownership(12, 14, 15)),
        content_output_credit_owned: Some(ownership(13, 15, 16)),
        hash_refill_token_available: Some(ownership(14, 16, 17)),
        decode_credit_owned: Some(ownership(15, 17, 18)),
        item_completion_latency: Some(histogram()),
        disk_reads: vec![
            proto::RuntimeDiskReadMetrics {
                physical_disk_id: "PhysicalDisk1".into(),
                capacity: Some(1),
                hash_waiting: Some(0),
                media_waiting: Some(1),
                hash_active: Some(1),
                media_active: Some(0),
                hash_granted_total: Some(2),
                media_granted_total: Some(3),
                hash_released_total: Some(1),
                media_released_total: Some(3),
            },
            proto::RuntimeDiskReadMetrics {
                physical_disk_id: "PhysicalDisk2".into(),
                capacity: Some(2),
                hash_waiting: Some(1),
                media_waiting: Some(0),
                hash_active: Some(0),
                media_active: Some(1),
                hash_granted_total: Some(4),
                media_granted_total: Some(5),
                hash_released_total: Some(4),
                media_released_total: Some(4),
            },
        ],
    };
    let decoded = proto::RuntimePipelineMetrics::decode(metrics.encode_to_vec().as_slice())
        .expect("ownership 指标必须可 round-trip");
    assert_eq!(decoded, metrics);

    let absent = proto::RuntimePipelineMetrics::default();
    assert_eq!(absent.hash_waiting_permit, None);
    assert_eq!(absent.hash_refill_token_available, None);
    assert_eq!(absent.item_completion_latency, None);
    assert!(absent.disk_reads.is_empty());

    let zero = proto::RuntimePipelineMetrics {
        hash_waiting_permit: Some(ownership(0, 0, 0)),
        ..Default::default()
    };
    assert_eq!(zero.hash_waiting_permit.as_ref().unwrap().current, Some(0));
    assert_ne!(zero.hash_waiting_permit, absent.hash_waiting_permit);

    let legacy = proto::RuntimePipelineMetrics {
        hash_queue: Some(queue_metrics(1, 1, 1)),
        ..Default::default()
    };
    let decoded_legacy =
        proto::RuntimePipelineMetrics::decode(legacy.encode_to_vec().as_slice()).unwrap();
    let new_field_presence = [
        (
            "hash_waiting_permit",
            decoded_legacy.hash_waiting_permit.as_ref().map(|_| ()),
        ),
        (
            "hash_reading",
            decoded_legacy.hash_reading.as_ref().map(|_| ()),
        ),
        (
            "hash_completed_unjoined",
            decoded_legacy.hash_completed_unjoined.as_ref().map(|_| ()),
        ),
        (
            "media_permit_waiting",
            decoded_legacy.media_permit_waiting.as_ref().map(|_| ()),
        ),
        (
            "media_acquire_ready",
            decoded_legacy.media_acquire_ready.as_ref().map(|_| ()),
        ),
        (
            "media_permit_ready",
            decoded_legacy.media_permit_ready.as_ref().map(|_| ()),
        ),
        (
            "worker_dispatching",
            decoded_legacy.worker_dispatching.as_ref().map(|_| ()),
        ),
        (
            "worker_start_pending",
            decoded_legacy.worker_start_pending.as_ref().map(|_| ()),
        ),
        (
            "worker_decode",
            decoded_legacy.worker_decode.as_ref().map(|_| ()),
        ),
        (
            "worker_feature",
            decoded_legacy.worker_feature.as_ref().map(|_| ()),
        ),
        (
            "worker_result_wait",
            decoded_legacy.worker_result_wait.as_ref().map(|_| ()),
        ),
        (
            "worker_phase_unknown",
            decoded_legacy.worker_phase_unknown.as_ref().map(|_| ()),
        ),
        (
            "content_output_credit_owned",
            decoded_legacy
                .content_output_credit_owned
                .as_ref()
                .map(|_| ()),
        ),
        (
            "hash_refill_token_available",
            decoded_legacy
                .hash_refill_token_available
                .as_ref()
                .map(|_| ()),
        ),
        (
            "decode_credit_owned",
            decoded_legacy.decode_credit_owned.as_ref().map(|_| ()),
        ),
        (
            "item_completion_latency",
            decoded_legacy.item_completion_latency.as_ref().map(|_| ()),
        ),
    ];
    for (field, presence) in new_field_presence {
        assert!(presence.is_none(), "旧字节流不得填充新字段 {field}");
    }
    assert!(
        decoded_legacy.disk_reads.is_empty(),
        "旧字节流不得填充 disk_reads"
    );
}

/// 构造一组可区分 None 与 Some(0) 的 ownership 指标。
fn ownership(current: u64, peak: u64, capacity: u64) -> proto::RuntimeOwnershipMetrics {
    proto::RuntimeOwnershipMetrics {
        current: Some(current),
        peak: Some(peak),
        capacity: Some(capacity),
    }
}

fn queue_metrics(current: u64, peak: u64, capacity: u64) -> proto::RuntimeQueueMetrics {
    proto::RuntimeQueueMetrics {
        current: Some(current),
        peak: Some(peak),
        capacity: Some(capacity),
        wait_latency: Some(histogram()),
        service_latency: Some(histogram()),
    }
}

fn resource_metrics(current: u64, peak: u64, capacity: u64) -> proto::RuntimeResourceMetrics {
    proto::RuntimeResourceMetrics {
        current: Some(current),
        peak: Some(peak),
        capacity: Some(capacity),
        wait_latency: Some(histogram()),
        service_latency: Some(histogram()),
    }
}

fn histogram() -> proto::RuntimeLatencyHistogram {
    proto::RuntimeLatencyHistogram {
        buckets: vec![
            proto::RuntimeLatencyBucket {
                upper_bound_ms: Some(1),
                count: 1,
            },
            proto::RuntimeLatencyBucket {
                upper_bound_ms: None,
                count: 0,
            },
        ],
        count: 1,
        p50_ms: Some(1),
        p95_ms: Some(1),
        p99_ms: Some(1),
        max_ms: Some(1),
    }
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

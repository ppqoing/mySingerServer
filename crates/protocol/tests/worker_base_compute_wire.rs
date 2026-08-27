//! Worker 一次性基础计算 V5 消息与旧两步标签的废弃 wire 契约。

use dedup_protocol::{
    BASE_MISSING_CONTACT_SHEET, BASE_MISSING_PROBE, BASE_MISSING_STAGE1, FILE_DESCRIPTOR_SET,
    PROTOCOL_VERSION, proto,
};
use prost::Message;
use prost_types::FileDescriptorSet;

#[test]
fn one_shot_base_compute_preserves_identity_hash_limits_and_decoder_budget() {
    let request = proto::ComputeBaseFeatures {
        task_id: "task-1".into(),
        item_id: "item-1".into(),
        machine_id: "73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae".into(),
        normalized_path: r"i:\\media\\video.mp4".into(),
        display_path: r"I:\\Media\\video.mp4".into(),
        file_size: 123_456,
        physical_disk_id: "disk-0".into(),
        block_size_bytes: 4 * 1024 * 1024,
        block_timeout_ms: 3_000,
        block_retries: 2,
        md5: vec![0x5a; 16],
        media_kind: proto::MediaKind::MediaVideo as i32,
        missing_parts: BASE_MISSING_PROBE | BASE_MISSING_STAGE1 | BASE_MISSING_CONTACT_SHEET,
        decoder_threads: 3,
    };
    let envelope = proto::WorkerEnvelope {
        payload: Some(proto::worker_envelope::Payload::ComputeBaseFeatures(
            request,
        )),
    };

    let decoded = proto::WorkerEnvelope::decode(envelope.encode_to_vec().as_slice()).unwrap();
    assert_eq!(decoded, envelope);
    assert_eq!(PROTOCOL_VERSION, 5);
}

#[test]
fn source_read_complete_and_terminal_result_preserve_item_identity() {
    let source_read_complete = proto::WorkerEnvelope {
        payload: Some(proto::worker_envelope::Payload::BaseSourceReadComplete(
            proto::BaseSourceReadComplete {
                task_id: "task-1".into(),
                item_id: "item-1".into(),
                request_elapsed_us: Some(8_000),
            },
        )),
    };
    let result = proto::WorkerEnvelope {
        payload: Some(proto::worker_envelope::Payload::BaseComputeResult(
            proto::BaseComputeResult {
                task_id: "task-1".into(),
                item_id: "item-1".into(),
                md5: vec![0x5a; 16],
                payload: vec![1, 2, 3],
            },
        )),
    };

    for envelope in [source_read_complete, result] {
        let decoded = proto::WorkerEnvelope::decode(envelope.encode_to_vec().as_slice()).unwrap();
        assert_eq!(decoded, envelope);
    }
}

#[test]
fn worker_phase_events_use_v5_additive_tag_28_and_never_define_hash_wait() {
    let phases = [
        proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
        proto::RuntimeWorkerPhase::RuntimeWorkerFeature,
        proto::RuntimeWorkerPhase::RuntimeWorkerResultWait,
        proto::RuntimeWorkerPhase::RuntimeWorkerIdle,
    ];
    for phase in phases {
        let envelope = proto::WorkerEnvelope {
            payload: Some(proto::worker_envelope::Payload::WorkerPhaseChanged(
                proto::WorkerPhaseChanged {
                    task_id: "task-1".into(),
                    item_id: "item-1".into(),
                    phase: phase as i32,
                    request_elapsed_us: Some(12_345),
                },
            )),
        };
        assert_eq!(
            proto::WorkerEnvelope::decode(envelope.encode_to_vec().as_slice()).unwrap(),
            envelope
        );
    }
    assert_eq!(PROTOCOL_VERSION, 5, "新增运行遥测不得提升协议主版本");

    let descriptors = FileDescriptorSet::decode(FILE_DESCRIPTOR_SET).unwrap();
    let file = descriptors
        .file
        .iter()
        .find(|file| file.package.as_deref() == Some("mysingerserver.v2"))
        .unwrap();
    let worker = file
        .message_type
        .iter()
        .find(|message| message.name.as_deref() == Some("WorkerEnvelope"))
        .unwrap();
    let phase_field = worker
        .field
        .iter()
        .find(|field| field.name.as_deref() == Some("worker_phase_changed"))
        .unwrap();
    assert_eq!(phase_field.number, Some(28));
    assert_eq!(worker.reserved_range.len(), 3);
    assert_eq!(
        worker.reserved_name,
        [
            "begin_base_compute",
            "continue_base_compute",
            "base_hash_ready",
        ]
    );
    let phase = file
        .enum_type
        .iter()
        .find(|value| value.name.as_deref() == Some("RuntimeWorkerPhase"))
        .unwrap();
    assert!(phase.value.iter().all(|value| {
        !value
            .name
            .as_deref()
            .unwrap_or_default()
            .to_ascii_lowercase()
            .contains("hash")
    }));
}

#[test]
fn legacy_source_complete_without_elapsed_stays_decodable() {
    let legacy = proto::BaseSourceReadComplete::decode(
        [
            0x0A, 0x06, b't', b'a', b's', b'k', b'-', b'1', 0x12, 0x06, b'i', b't', b'e', b'm',
            b'-', b'1',
        ]
        .as_slice(),
    )
    .unwrap();
    assert_eq!(legacy.task_id, "task-1");
    assert_eq!(legacy.item_id, "item-1");
    assert_eq!(legacy.request_elapsed_us, None);
}

#[test]
fn legacy_two_step_base_tags_are_ignored_as_unknown_fields() {
    let legacy_payloads = [
        [0x6A, 0x00].as_slice(),
        [0x72, 0x00].as_slice(),
        [0xCA, 0x01, 0x00].as_slice(),
    ];

    for encoded in legacy_payloads {
        let decoded = proto::WorkerEnvelope::decode(encoded).unwrap();
        assert_eq!(decoded.payload, None);
        assert!(decoded.encode_to_vec().is_empty());
    }
}

#[test]
fn stage2_message_preserves_contact_sheet_reuse_contract() {
    let request = proto::ComputeStage2 {
        task_id: "task-2".into(),
        item_id: "item-2".into(),
        display_path: r"I:\\Media\\video.mp4".into(),
        frame_slots: vec![0, 1, 2, 3, 4, 5],
        contact_sheet_path: r"data\\node\\cache\\contact-sheets\\5a\\5a5a.jpg".into(),
        generate_contact_sheet_if_missing: true,
    };

    let decoded = proto::ComputeStage2::decode(request.encode_to_vec().as_slice()).unwrap();
    assert_eq!(decoded, request);
}

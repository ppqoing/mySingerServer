//! 远程 Node 配置 V5 的 wire、转换与 descriptor 契约。

use std::net::{IpAddr, Ipv4Addr};

use dedup_core::{EnumeratorKind, NodeConfig, WorkerMode};
use dedup_protocol::{FILE_DESCRIPTOR_SET, PROTOCOL_VERSION, ProtocolError, proto};
use prost::Message;
use prost_types::{DescriptorProto, FileDescriptorSet};

#[test]
fn node_config_value_round_trips_every_approved_field() {
    let mut config = NodeConfig::default();
    config.listen_ip = IpAddr::V4(Ipv4Addr::new(10, 2, 3, 4));
    config.port = 39123;
    config.worker_count = 17;
    config.enumerator = EnumeratorKind::WindowsWalker;
    config.paths.data_path = r"data\custom".into();
    config.paths.config_path = r"D:\\config\node.toml".into();
    config.paths.log_path = r"logs\node".into();
    config.paths.cache_path = r"D:\\cache".into();
    config.read.hdd_threads_per_disk = 3;
    config.read.ssd_threads_per_disk = 4;
    config.read.unknown_threads_per_disk = 5;
    config.read.total_threads = 6;
    config.read.block_size_bytes = 8 * 1024 * 1024;
    config.read.block_timeout_seconds = 9;
    config.read.block_retries = 10;
    config.worker.mode = WorkerMode::Manual;
    config.worker.reserved_cores = 11;
    config.worker.manual_worker_count = 12;
    config.postgres.enabled = true;
    config.postgres.host = "192.168.1.8".into();
    config.postgres.port = 15432;
    config.postgres.database = "media".into();
    config.postgres.username = "dedup".into();
    config.postgres.password = "secret".into();
    config.postgres.connect_timeout_seconds = 7;

    let wire = proto::NodeConfigValue::try_from(&config).unwrap();
    let decoded = NodeConfig::try_from(wire.clone()).unwrap();

    assert_eq!(decoded, config);
    assert_eq!(wire.legacy_worker_count, 17);
    assert_eq!(
        wire.worker_mode,
        proto::NodeWorkerMode::NodeWorkerManual as i32
    );
    assert_eq!(wire.postgres.as_ref().unwrap().username, "dedup");
    assert_eq!(wire.postgres.as_ref().unwrap().password, "secret");
}

#[test]
fn unknown_config_enum_is_rejected_instead_of_silently_defaulting() {
    let mut wire = proto::NodeConfigValue::try_from(&NodeConfig::default()).unwrap();
    wire.enumerator = 99;

    assert!(matches!(
        NodeConfig::try_from(wire),
        Err(ProtocolError::InvalidDomain(
            dedup_core::CoreError::InvalidConfig {
                field: "enumerator",
                ..
            }
        ))
    ));
}

#[test]
fn config_messages_round_trip_snapshot_and_save_values() {
    let config = proto::NodeConfigValue::try_from(&NodeConfig::default()).unwrap();
    let snapshot = proto::NodeConfigSnapshot {
        machine_id: "73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae".into(),
        version_sha256: "a".repeat(64),
        config: Some(config.clone()),
        logical_cpu_count: 24,
        effective_worker_count: 23,
    };
    let bytes = snapshot.encode_to_vec();
    let decoded = proto::NodeConfigSnapshot::decode(bytes.as_slice()).unwrap();
    assert_eq!(decoded, snapshot);

    let save = proto::SaveNodeConfig {
        expected_version_sha256: "b".repeat(64),
        config: Some(config),
    };
    let accepted = proto::NodeConfigSaved {
        machine_id: snapshot.machine_id,
        saved_version_sha256: "c".repeat(64),
    };
    assert_eq!(
        proto::SaveNodeConfig::decode(save.encode_to_vec().as_slice()).unwrap(),
        save
    );
    assert_eq!(
        proto::NodeConfigSaved::decode(accepted.encode_to_vec().as_slice()).unwrap(),
        accepted
    );
}

#[test]
fn invalid_node_config_is_rejected_before_encoding() {
    let mut config = NodeConfig::default();
    config.worker_count = usize::MAX;

    assert!(matches!(
        proto::NodeConfigValue::try_from(&config),
        Err(ProtocolError::InvalidDomain(
            dedup_core::CoreError::InvalidConfig {
                field: "worker_count",
                ..
            }
        ))
    ));
}

#[test]
fn invalid_worker_enum_and_oversized_block_size_are_rejected_while_decoding() {
    let mut unknown_worker = proto::NodeConfigValue::try_from(&NodeConfig::default()).unwrap();
    unknown_worker.worker_mode = 99;
    assert!(matches!(
        NodeConfig::try_from(unknown_worker),
        Err(ProtocolError::InvalidDomain(
            dedup_core::CoreError::InvalidConfig {
                field: "worker.mode",
                ..
            }
        ))
    ));

    let mut oversized_block = proto::NodeConfigValue::try_from(&NodeConfig::default()).unwrap();
    oversized_block.block_size_bytes = u64::MAX;
    assert!(matches!(
        NodeConfig::try_from(oversized_block),
        Err(ProtocolError::InvalidDomain(
            dedup_core::CoreError::InvalidConfig {
                field: "read.block_size_bytes",
                ..
            }
        ))
    ));
}

#[test]
fn descriptor_contains_only_the_approved_config_messages_and_envelope_tags() {
    let descriptor = FileDescriptorSet::decode(FILE_DESCRIPTOR_SET).unwrap();
    let file = descriptor
        .file
        .iter()
        .find(|file| file.package.as_deref() == Some("mysingerserver.v2"))
        .unwrap();
    let messages = &file.message_type;
    for name in [
        "GetNodeConfig",
        "NodeConfigSnapshot",
        "SaveNodeConfig",
        "NodeConfigSaved",
    ] {
        assert!(message(messages, name).is_some(), "descriptor 缺少 {name}");
    }
    let envelope = message(messages, "Envelope").unwrap();
    for (name, number) in [
        ("get_node_config", 37),
        ("node_config_snapshot", 38),
        ("save_node_config", 39),
        ("node_config_saved", 40),
    ] {
        assert!(
            envelope
                .field
                .iter()
                .any(|field| field.name.as_deref() == Some(name) && field.number == Some(number)),
            "Envelope 缺少 {name}={number}"
        );
    }
    for message in [
        "GetNodeConfig",
        "NodeConfigValue",
        "NodeConfigSnapshot",
        "SaveNodeConfig",
        "NodeConfigSaved",
    ]
    .into_iter()
    .map(|name| message(messages, name).unwrap())
    {
        for field in &message.field {
            let name = field
                .name
                .as_deref()
                .unwrap_or_default()
                .to_ascii_lowercase();
            assert!(
                !["auth", "key", "tls", "token", "certificate"]
                    .iter()
                    .any(|forbidden| name.contains(forbidden)),
                "配置协议不得含安全扩展字段: {name}"
            );
        }
    }
}

#[test]
fn descriptor_removes_legacy_management_payloads_without_changing_protocol_version() {
    let descriptor = FileDescriptorSet::decode(FILE_DESCRIPTOR_SET).unwrap();
    let file = descriptor
        .file
        .iter()
        .find(|file| file.package.as_deref() == Some("mysingerserver.v2"))
        .unwrap();
    let messages = &file.message_type;
    for name in [
        "RetryDeleteItems",
        "ListFileFaults",
        "ClearFileFault",
        "SaveNodeConfigAndRestart",
        "NodeRestartAccepted",
    ] {
        assert!(
            message(messages, name).is_none(),
            "V5 descriptor 不得暴露已删除管理消息 {name}"
        );
    }
    let envelope = message(messages, "Envelope").unwrap();
    for field in envelope.field.iter() {
        assert!(
            !matches!(
                field.name.as_deref(),
                Some(
                    "retry_delete_items"
                        | "list_file_faults"
                        | "clear_file_fault"
                        | "save_node_config_and_restart"
                        | "node_restart_accepted"
                )
            ),
            "V5 Envelope 不得暴露已删除管理字段 {:?}",
            field.name
        );
    }
    assert_eq!(PROTOCOL_VERSION, 5);
}

#[test]
fn get_node_config_uses_protocol_v5_envelope_payload() {
    assert_eq!(PROTOCOL_VERSION, 5);
    let envelope = proto::Envelope {
        request_id: 7,
        payload: Some(proto::envelope::Payload::GetNodeConfig(
            proto::GetNodeConfig {},
        )),
    };
    let decoded = proto::Envelope::decode(envelope.encode_to_vec().as_slice()).unwrap();
    assert!(matches!(
        decoded.payload,
        Some(proto::envelope::Payload::GetNodeConfig(_))
    ));
}

fn message<'a>(messages: &'a [DescriptorProto], name: &str) -> Option<&'a DescriptorProto> {
    messages
        .iter()
        .find(|message| message.name.as_deref() == Some(name))
}

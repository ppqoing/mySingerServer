//! Node 本地最近结果滑动窗口的 V5 wire 契约。

use dedup_protocol::{FILE_DESCRIPTOR_SET, PROTOCOL_VERSION, proto};
use prost::Message;
use prost_types::{DescriptorProto, FileDescriptorSet};

#[test]
fn local_result_window_round_trips_with_group_kind_and_display_path() {
    let request = proto::ReadLocalResultWindow {
        analysis_run_id: "run-1".into(),
        kind: proto::LocalResultWindowKind::LocalResultWindowGroups as i32,
        group_id: String::new(),
        start_index: 5,
        visible_count: 100,
        total_rows: 1,
        stale: true,
        result_revision: 10,
        current_revision: 11,
        groups: vec![proto::DuplicateGroup {
            group_id: "g-1".into(),
            kind: proto::GroupKind::GroupExact as i32,
            member_count: 2,
            ..Default::default()
        }],
        members: Vec::new(),
        group_kind: proto::GroupKind::GroupExact as i32,
    };
    let envelope = proto::Envelope {
        request_id: 7,
        payload: Some(proto::envelope::Payload::ReadLocalResultWindow(
            request.clone(),
        )),
    };
    let decoded = proto::Envelope::decode(envelope.encode_to_vec().as_slice()).unwrap();
    assert_eq!(decoded, envelope);

    let member = proto::GroupMember {
        display_path: r"D:\Media\one.jpg".into(),
        ..Default::default()
    };
    let member_window = proto::ReadLocalResultWindow {
        analysis_run_id: "run-1".into(),
        kind: proto::LocalResultWindowKind::LocalResultWindowMembers as i32,
        group_id: "g-1".into(),
        visible_count: 1,
        members: vec![member.clone()],
        ..Default::default()
    };
    let member_envelope = proto::Envelope {
        request_id: 8,
        payload: Some(proto::envelope::Payload::ReadLocalResultWindow(
            member_window,
        )),
    };
    assert_eq!(
        proto::Envelope::decode(member_envelope.encode_to_vec().as_slice()).unwrap(),
        member_envelope
    );
    assert_eq!(
        proto::GroupMember::decode(member.encode_to_vec().as_slice()).unwrap(),
        member
    );
    assert_eq!(PROTOCOL_VERSION, 5);
}

#[test]
fn descriptor_keeps_new_window_at_tag_46_and_appends_display_path_at_12() {
    let descriptors = FileDescriptorSet::decode(FILE_DESCRIPTOR_SET).unwrap();
    let file = descriptors
        .file
        .iter()
        .find(|file| file.name.as_deref() == Some("node.proto"))
        .unwrap();
    let message = |name: &str| -> &DescriptorProto {
        file.message_type
            .iter()
            .find(|message| message.name.as_deref() == Some(name))
            .unwrap()
    };
    let envelope = message("Envelope");
    let window = envelope
        .oneof_decl
        .iter()
        .find(|_| true)
        .and_then(|_| {
            envelope
                .field
                .iter()
                .find(|field| field.name.as_deref() == Some("read_local_result_window"))
        })
        .unwrap();
    assert_eq!(window.number, Some(46));

    let member = message("GroupMember");
    let display_path = member
        .field
        .iter()
        .find(|field| field.name.as_deref() == Some("display_path"))
        .unwrap();
    assert_eq!(display_path.number, Some(12));
}

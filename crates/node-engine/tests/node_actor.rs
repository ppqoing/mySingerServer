use std::path::Path;

use dedup_core::{
    ContentKey, DisplayPath, GroupId, LocationKey, MachineId, MediaKind, NormalizedPath, Thresholds,
};
use dedup_node_engine::{actor::NodeEngine, server::NodeRequestHandler};
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

fn envelope(request_id: u64, payload: proto::envelope::Payload) -> proto::Envelope {
    proto::Envelope {
        request_id,
        payload: Some(payload),
    }
}

use std::path::Path;

use dedup_core::MachineId;
use dedup_node_engine::{actor::NodeEngine, server::NodeRequestHandler};
use dedup_node_store::NodeStore;
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

fn envelope(request_id: u64, payload: proto::envelope::Payload) -> proto::Envelope {
    proto::Envelope {
        request_id,
        payload: Some(payload),
    }
}

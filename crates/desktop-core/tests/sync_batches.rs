use std::sync::Mutex;

use dedup_core::MachineId;
use dedup_desktop_core::sync::{
    SyncEngine, SyncError, SyncNodeClient, SyncRepository, SyncSnapshot, SyncTrigger,
    sync_trigger_channel,
};
use dedup_protocol::proto;

#[tokio::test]
async fn automatic_and_manual_sync_use_exact_1000_row_batches() {
    for trigger in [SyncTrigger::Automatic, SyncTrigger::Manual] {
        let node = FakeNode::with_changes(2501);
        let mut repository = FakeRepository::default();
        let report = SyncEngine::new()
            .sync_node(&node, &mut repository, trigger)
            .await
            .unwrap();

        assert_eq!(repository.batch_sizes, [1000, 1000, 501]);
        assert_eq!(node.nonzero_acks(), [1000, 2000, 2501]);
        assert_eq!(report.committed_seq, 2501);
        assert_eq!(report.change_count, 2501);
        assert_eq!(report.trigger, trigger);
    }
}

#[tokio::test]
async fn commit_failure_does_not_ack_and_reconnect_replays_the_batch() {
    let node = FakeNode::with_changes(3);
    let mut repository = FakeRepository {
        fail_next_apply: true,
        ..Default::default()
    };
    let first = SyncEngine::new()
        .sync_node(&node, &mut repository, SyncTrigger::Automatic)
        .await;
    assert!(matches!(first, Err(SyncError::Backend(_))));
    assert_eq!(repository.cursor, 0);
    assert!(node.nonzero_acks().is_empty());

    SyncEngine::new()
        .sync_node(&node, &mut repository, SyncTrigger::Automatic)
        .await
        .unwrap();
    assert_eq!(repository.cursor, 3);
    assert_eq!(repository.applied_sequences, [1, 2, 3]);
    assert_eq!(node.nonzero_acks(), [3]);
}

#[tokio::test]
async fn ack_loss_after_commit_is_closed_by_the_next_initial_ack() {
    let node = FakeNode::with_changes(2);
    node.fail_ack_once(2);
    let mut repository = FakeRepository::default();
    let first = SyncEngine::new()
        .sync_node(&node, &mut repository, SyncTrigger::Manual)
        .await;
    assert!(matches!(first, Err(SyncError::Backend(_))));
    assert_eq!(repository.cursor, 2, "PG 提交不能因 ACK 丢失而回滚");
    assert_eq!(repository.apply_calls, 1);

    SyncEngine::new()
        .sync_node(&node, &mut repository, SyncTrigger::Manual)
        .await
        .unwrap();
    assert_eq!(repository.apply_calls, 1, "重连不能重复写已提交业务行");
    assert_eq!(node.nonzero_acks(), [2, 2]);
}

#[tokio::test]
async fn fixed_automatic_sources_and_manual_action_share_one_trigger_channel() {
    let (sender, mut receiver) = sync_trigger_channel(4);
    sender.connected().await.unwrap();
    sender.task_completed().await.unwrap();
    sender.catch_up_tick().await.unwrap();
    sender.manual().await.unwrap();

    assert_eq!(receiver.next().await, Some(SyncTrigger::Automatic));
    assert_eq!(receiver.next().await, Some(SyncTrigger::Automatic));
    assert_eq!(receiver.next().await, Some(SyncTrigger::Automatic));
    assert_eq!(receiver.next().await, Some(SyncTrigger::Manual));
}

#[tokio::test]
async fn full_trigger_channel_coalesces_without_blocking_the_controller() {
    let (sender, mut receiver) = sync_trigger_channel(1);
    sender.connected().await.unwrap();
    tokio::time::timeout(std::time::Duration::from_millis(50), sender.manual())
        .await
        .expect("已满触发通道不得阻塞 UI 控制循环")
        .unwrap();
    assert_eq!(receiver.next().await, Some(SyncTrigger::Automatic));
}

struct FakeNode {
    machine_id: MachineId,
    state: Mutex<FakeNodeState>,
}

struct FakeNodeState {
    changes: Vec<proto::SyncChange>,
    acknowledgements: Vec<u64>,
    fail_ack_once: Option<u64>,
}

impl FakeNode {
    fn with_changes(count: u64) -> Self {
        Self {
            machine_id: MachineId::parse(&"a5".repeat(32)).unwrap(),
            state: Mutex::new(FakeNodeState {
                changes: (1..=count)
                    .map(|seq| proto::SyncChange {
                        seq,
                        entity_kind: "fixture".into(),
                        payload: vec![seq as u8],
                    })
                    .collect(),
                acknowledgements: Vec::new(),
                fail_ack_once: None,
            }),
        }
    }

    fn fail_ack_once(&self, sequence: u64) {
        self.state.lock().unwrap().fail_ack_once = Some(sequence);
    }

    fn nonzero_acks(&self) -> Vec<u64> {
        self.state
            .lock()
            .unwrap()
            .acknowledgements
            .iter()
            .copied()
            .filter(|value| *value != 0)
            .collect()
    }
}

#[allow(async_fn_in_trait)]
impl SyncNodeClient for FakeNode {
    fn machine_id(&self) -> &MachineId {
        &self.machine_id
    }

    async fn acknowledge(&self, committed_seq: u64) -> Result<(), SyncError> {
        let mut state = self.state.lock().unwrap();
        state.acknowledgements.push(committed_seq);
        if state.fail_ack_once == Some(committed_seq) {
            state.fail_ack_once = None;
            return Err(SyncError::Backend("fixture ACK lost".into()));
        }
        Ok(())
    }

    async fn pull_changes(
        &self,
        after_seq: u64,
        limit: u32,
    ) -> Result<proto::SyncChangeBatch, SyncError> {
        let state = self.state.lock().unwrap();
        let changes = state
            .changes
            .iter()
            .filter(|change| change.seq > after_seq)
            .take(limit as usize)
            .cloned()
            .collect();
        Ok(proto::SyncChangeBatch {
            changes,
            high_seq: state.changes.last().map_or(0, |change| change.seq),
            pruned_through_seq: 0,
        })
    }

    async fn begin_snapshot(&self) -> Result<proto::BeginSnapshot, SyncError> {
        Err(SyncError::Backend("snapshot not expected".into()))
    }

    async fn read_snapshot_page(
        &self,
        _request: proto::ReadSnapshotPage,
    ) -> Result<proto::ReadSnapshotPage, SyncError> {
        Err(SyncError::Backend("snapshot not expected".into()))
    }
}

#[derive(Default)]
struct FakeRepository {
    cursor: u64,
    batch_sizes: Vec<usize>,
    applied_sequences: Vec<u64>,
    apply_calls: usize,
    fail_next_apply: bool,
}

struct FakeSnapshot<'a> {
    repository: &'a mut FakeRepository,
    high_seq: u64,
}

#[allow(async_fn_in_trait)]
impl SyncRepository for FakeRepository {
    type Snapshot<'a> = FakeSnapshot<'a>;

    async fn cursor(&self, _machine_id: &MachineId) -> Result<u64, SyncError> {
        Ok(self.cursor)
    }

    async fn apply_batch(
        &mut self,
        _machine_id: &MachineId,
        batch: &proto::SyncChangeBatch,
    ) -> Result<u64, SyncError> {
        self.apply_calls += 1;
        if std::mem::take(&mut self.fail_next_apply) {
            return Err(SyncError::Backend("fixture commit failed".into()));
        }
        self.batch_sizes.push(batch.changes.len());
        self.applied_sequences
            .extend(batch.changes.iter().map(|change| change.seq));
        self.cursor = batch
            .changes
            .last()
            .map_or(self.cursor, |change| change.seq);
        Ok(self.cursor)
    }

    async fn begin_snapshot(
        &mut self,
        _machine_id: &MachineId,
        snapshot_high_seq: u64,
    ) -> Result<Self::Snapshot<'_>, SyncError> {
        Ok(FakeSnapshot {
            repository: self,
            high_seq: snapshot_high_seq,
        })
    }
}

#[allow(async_fn_in_trait)]
impl SyncSnapshot for FakeSnapshot<'_> {
    async fn apply_page(&mut self, _table_name: &str, _rows: &[Vec<u8>]) -> Result<(), SyncError> {
        Ok(())
    }

    async fn commit(self) -> Result<u64, SyncError> {
        self.repository.cursor = self.high_seq;
        Ok(self.high_seq)
    }
}

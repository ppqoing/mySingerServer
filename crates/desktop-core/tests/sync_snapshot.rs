use std::{collections::BTreeMap, sync::Mutex};

use dedup_core::MachineId;
use dedup_core::{DisplayPath, MediaKind, NormalizedPath};
use dedup_desktop_core::central::CentralStore;
use dedup_desktop_core::sync::{
    SNAPSHOT_TABLES, SyncEngine, SyncError, SyncNodeClient, SyncRepository, SyncSnapshot,
    SyncTrigger,
};
use dedup_node_store::{NodeStore, ScannedPath};
use dedup_protocol::proto;

#[tokio::test]
async fn pruned_increment_uses_one_atomic_snapshot_then_continues_after_highwater() {
    let node = SnapshotNode::new(false);
    let mut repository = SnapshotRepository::new(5);
    let report = SyncEngine::new()
        .sync_node(&node, &mut repository, SyncTrigger::Manual)
        .await
        .unwrap();

    assert_eq!(repository.snapshot_commits, 1);
    assert_eq!(repository.tables, SNAPSHOT_TABLES);
    assert!(!repository.tables.contains(&"contact_sheets".to_owned()));
    assert!(
        repository
            .tables
            .contains(&"deletion_tombstones".to_owned())
    );
    assert_eq!(repository.incremental_sequences, [11]);
    assert_eq!(node.nonzero_acks(), [5, 10, 11]);
    assert_eq!(report.committed_seq, 11);
}

#[tokio::test]
async fn interrupted_snapshot_rolls_back_and_next_connection_restarts_from_first_table() {
    let node = SnapshotNode::new(true);
    let mut repository = SnapshotRepository::new(5);
    let first = SyncEngine::new()
        .sync_node(&node, &mut repository, SyncTrigger::Automatic)
        .await;
    assert!(matches!(first, Err(SyncError::Backend(_))));
    assert_eq!(repository.cursor, 5);
    assert_eq!(repository.snapshot_commits, 0);

    let report = SyncEngine::new()
        .sync_node(&node, &mut repository, SyncTrigger::Automatic)
        .await
        .unwrap();
    assert_eq!(node.begin_count(), 2);
    assert_eq!(node.first_table_reads(), 2);
    assert_eq!(repository.snapshot_commits, 1);
    assert_eq!(report.committed_seq, 11);
}

#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn postgres_snapshot_replaces_locations_atomically_and_drop_rolls_back() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let machine = MachineId::parse(&"9a".repeat(32)).unwrap();
    let mut central = CentralStore::connect(&url).await.unwrap();
    let mut stale = NodeStore::open_in_memory(machine.clone()).unwrap();
    add_node_file(&mut stale, r"C:\SnapshotPg\stale.bin", 41, [0x71; 16]);
    let stale_batch = stale.pull_changes(0, 1000).unwrap();
    central
        .apply_sync_batch(
            &machine,
            &proto::SyncChangeBatch {
                changes: stale_batch.changes,
                high_seq: stale_batch.high_seq,
                pruned_through_seq: stale_batch.pruned_through_seq,
            },
        )
        .await
        .unwrap();

    let mut fresh = NodeStore::open_in_memory(machine.clone()).unwrap();
    add_node_file(&mut fresh, r"C:\SnapshotPg\fresh.bin", 42, [0x72; 16]);
    let snapshot = fresh.begin_snapshot().unwrap();
    let highwater = snapshot.high_seq();
    let mut writer = central
        .begin_snapshot_replace(&machine, highwater)
        .await
        .unwrap();
    for table in SNAPSHOT_TABLES {
        let page = snapshot.read_page(table, "", 1000).unwrap();
        let rows = page
            .rows
            .into_iter()
            .map(|row| row.payload)
            .collect::<Vec<_>>();
        writer.apply_page(table, &rows).await.unwrap();
    }
    writer.commit().await.unwrap();

    let (client, connection) = tokio_postgres::connect(&url, tokio_postgres::NoTls)
        .await
        .unwrap();
    tokio::spawn(async move { connection.await.unwrap() });
    let active_paths: Vec<String> = client
        .query(
            "SELECT normalized_path FROM file_locations
             WHERE machine_id=$1 AND active=TRUE ORDER BY normalized_path",
            &[&machine.as_str()],
        )
        .await
        .unwrap()
        .into_iter()
        .map(|row| row.get(0))
        .collect();
    assert_eq!(active_paths, [r"C:\SNAPSHOTPG\FRESH.BIN"]);

    let mut rollback = NodeStore::open_in_memory(machine.clone()).unwrap();
    add_node_file(&mut rollback, r"C:\SnapshotPg\rollback.bin", 43, [0x73; 16]);
    let rollback_snapshot = rollback.begin_snapshot().unwrap();
    let mut uncommitted = central
        .begin_snapshot_replace(&machine, rollback_snapshot.high_seq())
        .await
        .unwrap();
    let content_page = rollback_snapshot.read_page("contents", "", 1000).unwrap();
    let rows = content_page
        .rows
        .into_iter()
        .map(|row| row.payload)
        .collect::<Vec<_>>();
    uncommitted.apply_page("contents", &rows).await.unwrap();
    drop(uncommitted);

    let still_active: Vec<String> = client
        .query(
            "SELECT normalized_path FROM file_locations
             WHERE machine_id=$1 AND active=TRUE ORDER BY normalized_path",
            &[&machine.as_str()],
        )
        .await
        .unwrap()
        .into_iter()
        .map(|row| row.get(0))
        .collect();
    assert_eq!(still_active, active_paths);
    assert_eq!(central.content_count([0x73; 16]).await.unwrap(), 0);
}

struct SnapshotNode {
    machine_id: MachineId,
    state: Mutex<SnapshotNodeState>,
}

struct SnapshotNodeState {
    acknowledgements: Vec<u64>,
    begin_count: usize,
    table_reads: BTreeMap<String, usize>,
    fail_once: bool,
}

impl SnapshotNode {
    fn new(fail_once: bool) -> Self {
        Self {
            machine_id: MachineId::parse(&"b6".repeat(32)).unwrap(),
            state: Mutex::new(SnapshotNodeState {
                acknowledgements: Vec::new(),
                begin_count: 0,
                table_reads: BTreeMap::new(),
                fail_once,
            }),
        }
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

    fn begin_count(&self) -> usize {
        self.state.lock().unwrap().begin_count
    }

    fn first_table_reads(&self) -> usize {
        *self
            .state
            .lock()
            .unwrap()
            .table_reads
            .get(SNAPSHOT_TABLES[0])
            .unwrap_or(&0)
    }
}

#[allow(async_fn_in_trait)]
impl SyncNodeClient for SnapshotNode {
    fn machine_id(&self) -> &MachineId {
        &self.machine_id
    }

    async fn acknowledge(&self, committed_seq: u64) -> Result<(), SyncError> {
        self.state
            .lock()
            .unwrap()
            .acknowledgements
            .push(committed_seq);
        Ok(())
    }

    async fn pull_changes(
        &self,
        after_seq: u64,
        _limit: u32,
    ) -> Result<proto::SyncChangeBatch, SyncError> {
        if after_seq < 8 {
            return Err(SyncError::SnapshotRequired);
        }
        let changes = (after_seq < 11)
            .then(|| proto::SyncChange {
                seq: 11,
                entity_kind: "fixture".into(),
                payload: vec![11],
            })
            .into_iter()
            .collect();
        Ok(proto::SyncChangeBatch {
            changes,
            high_seq: 11,
            pruned_through_seq: 8,
        })
    }

    async fn begin_snapshot(&self) -> Result<proto::BeginSnapshot, SyncError> {
        let mut state = self.state.lock().unwrap();
        state.begin_count += 1;
        Ok(proto::BeginSnapshot {
            snapshot_token: format!("snapshot-{}", state.begin_count),
            snapshot_high_seq: 10,
        })
    }

    async fn read_snapshot_page(
        &self,
        request: proto::ReadSnapshotPage,
    ) -> Result<proto::ReadSnapshotPage, SyncError> {
        let mut state = self.state.lock().unwrap();
        *state
            .table_reads
            .entry(request.table_name.clone())
            .or_default() += 1;
        if state.fail_once && request.table_name == SNAPSHOT_TABLES[1] {
            state.fail_once = false;
            return Err(SyncError::Backend("fixture disconnected".into()));
        }
        Ok(proto::ReadSnapshotPage {
            snapshot_token: request.snapshot_token,
            table_name: request.table_name,
            cursor: request.cursor,
            limit: request.limit,
            rows: vec![vec![1]],
            next_cursor: String::new(),
            done: true,
        })
    }
}

struct SnapshotRepository {
    cursor: u64,
    snapshot_commits: usize,
    tables: Vec<String>,
    incremental_sequences: Vec<u64>,
}

impl SnapshotRepository {
    fn new(cursor: u64) -> Self {
        Self {
            cursor,
            snapshot_commits: 0,
            tables: Vec::new(),
            incremental_sequences: Vec::new(),
        }
    }
}

struct PendingSnapshot<'a> {
    repository: &'a mut SnapshotRepository,
    high_seq: u64,
    tables: Vec<String>,
}

#[allow(async_fn_in_trait)]
impl SyncRepository for SnapshotRepository {
    type Snapshot<'a> = PendingSnapshot<'a>;

    async fn cursor(&self, _machine_id: &MachineId) -> Result<u64, SyncError> {
        Ok(self.cursor)
    }

    async fn apply_batch(
        &mut self,
        _machine_id: &MachineId,
        batch: &proto::SyncChangeBatch,
    ) -> Result<u64, SyncError> {
        self.incremental_sequences
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
        Ok(PendingSnapshot {
            repository: self,
            high_seq: snapshot_high_seq,
            tables: Vec::new(),
        })
    }
}

#[allow(async_fn_in_trait)]
impl SyncSnapshot for PendingSnapshot<'_> {
    async fn apply_page(&mut self, table_name: &str, _rows: &[Vec<u8>]) -> Result<(), SyncError> {
        if self.tables.last().is_none_or(|last| last != table_name) {
            self.tables.push(table_name.to_owned());
        }
        Ok(())
    }

    async fn commit(self) -> Result<u64, SyncError> {
        self.repository.cursor = self.high_seq;
        self.repository.snapshot_commits += 1;
        self.repository.tables = self.tables;
        Ok(self.high_seq)
    }
}

fn add_node_file(store: &mut NodeStore, path: &str, size: u64, md5: [u8; 16]) {
    store
        .upsert_content_and_location(
            &ScannedPath::new(
                NormalizedPath::new(path).unwrap(),
                DisplayPath::new(path).unwrap(),
                size,
            ),
            md5,
            MediaKind::Other,
        )
        .unwrap();
}

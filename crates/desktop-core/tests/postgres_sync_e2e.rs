//! 真实 PostgreSQL 与节点 SQLite outbox 的同步故障窗口端到端验收。
//!
//! 本文件故意默认忽略：管理员必须先在专用空库手工执行 `deploy/central-v2.sql`，再通过
//! `DEDUP_TEST_POSTGRES_URL` 指向该 V2 schema。测试不执行 DDL、不清空数据库，也不删除共享
//! schema；每个场景使用随机 `MachineId`、独立临时 `NodeStore` 和唯一内容键隔离本轮数据。

use std::sync::Mutex;

use dedup_core::{
    DeleteMode, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath, Thresholds,
};
use dedup_desktop_core::{
    central::{CentralSnapshot, CentralStore},
    sync::{SNAPSHOT_TABLES, SyncEngine, SyncError, SyncNodeClient, SyncRepository, SyncTrigger},
};
use dedup_media::{ImageStage2, PdqHash};
use dedup_node_store::{
    AnalysisMode, ConfirmedDeleteItem, DeleteOutcome, DeleteResult, FeatureWrite, GroupKind,
    GroupMemberWrite, GroupWrite, ImageStage1Fields, NodeStore, OwnedSnapshot, ReviewDecision,
    ScannedPath, StoreError, SyncBatch, SyncState,
};
use dedup_protocol::proto;
use tempfile::TempDir;
use uuid::Uuid;

/// 用真实 `SyncEngine` 和 `CentralStore` 验证 2501 行固定分页，并继续验证二筛 outbox
/// 高水位只有在 PostgreSQL 提交和节点 ACK 都完成后才收敛。
#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn postgres_sync_uses_1000_1000_501_batches_and_reaches_stage2_highwater() {
    let url = postgres_url();
    let machine = unique_machine_id();
    let node = StoreNode::new(machine.clone());

    // 每个新内容稳定产生 content + file 两条 outbox；1250 个内容为 2500 条，随后一条
    // image_stage1 恰好把首轮高水位推进到 2501，避免用伪造协议载荷冒充真实 SQLite。
    let image_id = node.with_store_mut(|store| {
        let mut first_image = None;
        for ordinal in 0..1250_u64 {
            let kind = if ordinal == 0 {
                MediaKind::Image
            } else {
                MediaKind::Other
            };
            let record = store
                .upsert_content_and_location(
                    &scanned(&machine, ordinal, "bin"),
                    content_md5(&machine, ordinal),
                    kind,
                )
                .unwrap();
            if ordinal == 0 {
                first_image = Some(record.id);
            }
        }
        assert_eq!(store.outbox_high_seq().unwrap(), 2500);
        let image_id = first_image.expect("首个图片内容必须存在");
        let stage1_seq = store
            .commit_feature_result(
                image_id,
                None,
                FeatureWrite::ImageStage1(ImageStage1Fields {
                    width: Some(640),
                    height: Some(480),
                    pdq: Some(PdqHash::from_bytes([0x51; 32])),
                    quality: Some(90),
                }),
            )
            .unwrap();
        assert_eq!(stage1_seq, 2501);
        image_id
    });

    let mut central = CentralStore::connect(&url).await.unwrap();
    let report = SyncEngine::new()
        .sync_node(&node, &mut central, SyncTrigger::Automatic)
        .await
        .unwrap();
    assert_eq!(node.nonempty_pull_sizes(), [1000, 1000, 501]);
    assert_eq!(node.nonzero_ack_attempts(), [1000, 2000, 2501]);
    assert_eq!(report.batch_count, 3);
    assert_eq!(report.change_count, 2501);
    assert_eq!(report.committed_seq, 2501);
    assert_eq!(central.sync_cursor(&machine).await.unwrap(), 2501);

    // 二筛写入发生在首轮 ACK 裁剪之后，因此它必须形成新的、可独立观察的 outbox 高水位。
    let stage2_seq = node.with_store_mut(|store| {
        store
            .commit_feature_result(
                image_id,
                None,
                FeatureWrite::ImageStage2(image_stage2(0x52)),
            )
            .unwrap()
    });
    assert_eq!(stage2_seq, 2502);
    let stage2_report = SyncEngine::new()
        .sync_node(&node, &mut central, SyncTrigger::Automatic)
        .await
        .unwrap();
    assert_eq!(stage2_report.change_count, 1);
    assert_eq!(stage2_report.committed_seq, stage2_seq);
    assert_eq!(central.sync_cursor(&machine).await.unwrap(), stage2_seq);

    let client = postgres_client(&url).await;
    let stage2_rows: i64 = client
        .query_one(
            "SELECT COUNT(*) FROM image_stage2 s
             JOIN contents c ON c.content_id=s.content_id
             WHERE c.md5=$1 AND c.file_size=$2",
            &[&content_md5(&machine, 0).as_slice(), &file_size_for(0)],
        )
        .await
        .unwrap()
        .get(0);
    assert_eq!(stage2_rows, 1, "stage2 高水位必须对应已提交特征正文");
}

/// 同一真实数据库连接分别覆盖“事务调用前失败”和“事务已提交但 ACK 响应丢失”。前者不得
/// 推进中心游标；后者由下一轮开头重复 ACK 已提交游标收敛，且不重放业务批次。
#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn postgres_commit_failure_and_ack_loss_keep_their_respective_boundaries() {
    let url = postgres_url();

    let commit_machine = unique_machine_id();
    let commit_node = StoreNode::new(commit_machine.clone());
    commit_node.with_store_mut(|store| {
        store
            .upsert_content_and_location(
                &scanned(&commit_machine, 0, "dat"),
                content_md5(&commit_machine, 0),
                MediaKind::Other,
            )
            .unwrap();
    });
    let mut commit_central = CentralStore::connect(&url).await.unwrap();
    let mut fail_once = FailBeforeCommit::new(&mut commit_central);
    let first = SyncEngine::new()
        .sync_node(&commit_node, &mut fail_once, SyncTrigger::Automatic)
        .await;
    assert!(matches!(first, Err(SyncError::Backend(_))));
    assert_eq!(
        fail_once
            .central
            .sync_cursor(&commit_machine)
            .await
            .unwrap(),
        0
    );
    assert!(commit_node.nonzero_ack_attempts().is_empty());
    assert_eq!(commit_node.sync_state().acked_seq, 0);

    let retry = SyncEngine::new()
        .sync_node(&commit_node, &mut fail_once, SyncTrigger::Automatic)
        .await
        .unwrap();
    assert_eq!(retry.committed_seq, 2);
    assert_eq!(
        fail_once
            .central
            .sync_cursor(&commit_machine)
            .await
            .unwrap(),
        2
    );
    assert_eq!(fail_once.apply_attempts, 2);

    let ack_machine = unique_machine_id();
    let ack_node = StoreNode::new(ack_machine.clone());
    ack_node.with_store_mut(|store| {
        store
            .upsert_content_and_location(
                &scanned(&ack_machine, 0, "dat"),
                content_md5(&ack_machine, 0),
                MediaKind::Other,
            )
            .unwrap();
    });
    ack_node.fail_ack_once(2);
    let mut ack_central = CentralStore::connect(&url).await.unwrap();
    let lost = SyncEngine::new()
        .sync_node(&ack_node, &mut ack_central, SyncTrigger::Manual)
        .await;
    assert!(matches!(lost, Err(SyncError::Backend(_))));
    assert_eq!(ack_central.sync_cursor(&ack_machine).await.unwrap(), 2);
    assert_eq!(ack_node.sync_state().acked_seq, 0);

    let converged = SyncEngine::new()
        .sync_node(&ack_node, &mut ack_central, SyncTrigger::Manual)
        .await
        .unwrap();
    assert_eq!(
        converged.batch_count, 0,
        "下一轮首 ACK 后不应重写已提交批次"
    );
    assert_eq!(ack_node.nonzero_ack_attempts(), [2, 2]);
    assert_eq!(
        ack_node.sync_state(),
        SyncState {
            acked_seq: 2,
            pruned_through_seq: 2,
        }
    );
}

/// 中心游标落后于 SQLite 裁剪边界时必须改走一次完整快照；同一删除 outbox 批次被重复提交
/// 时，PostgreSQL tombstone 仍由唯一键 UPSERT 为一行，并保持文件失活。
#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn postgres_pruned_outbox_uses_snapshot_and_tombstone_replay_is_idempotent() {
    let url = postgres_url();
    let snapshot_machine = unique_machine_id();
    let snapshot_node = StoreNode::new(snapshot_machine.clone());
    snapshot_node.with_store_mut(|store| {
        store
            .upsert_content_and_location(
                &scanned(&snapshot_machine, 0, "snapshot"),
                content_md5(&snapshot_machine, 0),
                MediaKind::Other,
            )
            .unwrap();
        let highwater = store.outbox_high_seq().unwrap();
        assert_eq!(highwater, 2);
        store.ack_changes(highwater).unwrap();
    });

    let mut central = CentralStore::connect(&url).await.unwrap();
    let snapshot_report = SyncEngine::new()
        .sync_node(&snapshot_node, &mut central, SyncTrigger::Manual)
        .await
        .unwrap();
    assert_eq!(snapshot_node.snapshot_begin_count(), 1);
    assert_eq!(
        snapshot_report.snapshot_page_count,
        SNAPSHOT_TABLES.len() as u64
    );
    assert_eq!(snapshot_report.committed_seq, 2);
    assert_eq!(central.sync_cursor(&snapshot_machine).await.unwrap(), 2);
    assert_eq!(
        central
            .location_count(content_md5(&snapshot_machine, 0), file_size_for(0) as u64,)
            .await
            .unwrap(),
        1
    );

    let delete_machine = unique_machine_id();
    let delete_node = StoreNode::new(delete_machine.clone());
    let deleted_location = seed_successful_delete(&delete_node, &delete_machine);
    let delete_batch = delete_node.proto_batch_after(0, 1000).unwrap();
    assert!(
        delete_batch
            .changes
            .iter()
            .any(|change| change.entity_kind == "deletion_tombstone")
    );

    central
        .apply_sync_batch(&delete_machine, &delete_batch)
        .await
        .unwrap();
    central
        .apply_sync_batch(&delete_machine, &delete_batch)
        .await
        .unwrap();

    let client = postgres_client(&url).await;
    let tombstone_rows: i64 = client
        .query_one(
            "SELECT COUNT(*) FROM deletion_tombstones
             WHERE machine_id=$1 AND normalized_path=$2",
            &[
                &delete_machine.as_str(),
                &deleted_location.normalized_path().as_str(),
            ],
        )
        .await
        .unwrap()
        .get(0);
    assert_eq!(tombstone_rows, 1, "重复同步同一墓碑不得产生第二行");
    let active: bool = client
        .query_one(
            "SELECT active FROM file_locations
             WHERE machine_id=$1 AND normalized_path=$2",
            &[
                &delete_machine.as_str(),
                &deleted_location.normalized_path().as_str(),
            ],
        )
        .await
        .unwrap()
        .get(0);
    assert!(!active, "墓碑重放后删除位置仍必须保持失活");
}

/// 一个最薄的真实节点适配器：所有业务载荷、裁剪和快照都来自 `NodeStore`；这里只负责把
/// 同步 trait 的协议结构映射到公开 SQLite API，并记录批大小/ACK 供端到端断言。
struct StoreNode {
    machine_id: MachineId,
    state: Mutex<StoreNodeState>,
    _database_directory: TempDir,
}

struct StoreNodeState {
    store: NodeStore,
    active_snapshot: Option<ActiveSnapshot>,
    snapshot_begin_count: usize,
    nonempty_pull_sizes: Vec<usize>,
    ack_attempts: Vec<u64>,
    fail_ack_once: Option<u64>,
}

struct ActiveSnapshot {
    token: String,
    snapshot: OwnedSnapshot,
}

impl StoreNode {
    fn new(machine_id: MachineId) -> Self {
        let directory = tempfile::tempdir().unwrap();
        let store = NodeStore::open(&directory.path().join("node.db"), machine_id.clone()).unwrap();
        Self {
            machine_id,
            state: Mutex::new(StoreNodeState {
                store,
                active_snapshot: None,
                snapshot_begin_count: 0,
                nonempty_pull_sizes: Vec::new(),
                ack_attempts: Vec::new(),
                fail_ack_once: None,
            }),
            _database_directory: directory,
        }
    }

    fn with_store_mut<T>(&self, action: impl FnOnce(&mut NodeStore) -> T) -> T {
        action(&mut self.state.lock().unwrap().store)
    }

    fn proto_batch_after(
        &self,
        after_seq: u64,
        limit: usize,
    ) -> Result<proto::SyncChangeBatch, StoreError> {
        self.state
            .lock()
            .unwrap()
            .store
            .pull_changes(after_seq, limit)
            .map(proto_batch)
    }

    fn fail_ack_once(&self, sequence: u64) {
        self.state.lock().unwrap().fail_ack_once = Some(sequence);
    }

    fn nonempty_pull_sizes(&self) -> Vec<usize> {
        self.state.lock().unwrap().nonempty_pull_sizes.clone()
    }

    fn nonzero_ack_attempts(&self) -> Vec<u64> {
        self.state
            .lock()
            .unwrap()
            .ack_attempts
            .iter()
            .copied()
            .filter(|sequence| *sequence != 0)
            .collect()
    }

    fn sync_state(&self) -> SyncState {
        self.state.lock().unwrap().store.sync_state().unwrap()
    }

    fn snapshot_begin_count(&self) -> usize {
        self.state.lock().unwrap().snapshot_begin_count
    }
}

#[allow(async_fn_in_trait)]
impl SyncNodeClient for StoreNode {
    fn machine_id(&self) -> &MachineId {
        &self.machine_id
    }

    async fn acknowledge(&self, committed_seq: u64) -> Result<(), SyncError> {
        let mut state = self.state.lock().unwrap();
        state.ack_attempts.push(committed_seq);
        if state.fail_ack_once == Some(committed_seq) {
            state.fail_ack_once = None;
            return Err(SyncError::Backend("fixture ACK response lost".into()));
        }
        state.store.ack_changes(committed_seq).map_err(sync_error)
    }

    async fn pull_changes(
        &self,
        after_seq: u64,
        limit: u32,
    ) -> Result<proto::SyncChangeBatch, SyncError> {
        let mut state = self.state.lock().unwrap();
        let batch = state
            .store
            .pull_changes(after_seq, limit as usize)
            .map_err(sync_error)?;
        if !batch.changes.is_empty() {
            state.nonempty_pull_sizes.push(batch.changes.len());
        }
        Ok(proto_batch(batch))
    }

    async fn begin_snapshot(&self) -> Result<proto::BeginSnapshot, SyncError> {
        let mut state = self.state.lock().unwrap();
        let snapshot = state.store.begin_owned_snapshot().map_err(sync_error)?;
        state.snapshot_begin_count += 1;
        let token = format!("snapshot-{}", state.snapshot_begin_count);
        let highwater = snapshot.high_seq();
        state.active_snapshot = Some(ActiveSnapshot {
            token: token.clone(),
            snapshot,
        });
        Ok(proto::BeginSnapshot {
            snapshot_token: token,
            snapshot_high_seq: highwater,
        })
    }

    async fn read_snapshot_page(
        &self,
        request: proto::ReadSnapshotPage,
    ) -> Result<proto::ReadSnapshotPage, SyncError> {
        let mut state = self.state.lock().unwrap();
        let active = state
            .active_snapshot
            .as_ref()
            .ok_or_else(|| SyncError::Backend("fixture snapshot missing".into()))?;
        if active.token != request.snapshot_token {
            return Err(SyncError::Backend("fixture snapshot token mismatch".into()));
        }
        let page = active
            .snapshot
            .read_page(&request.table_name, &request.cursor, request.limit as usize)
            .map_err(sync_error)?;
        let is_final_page =
            page.done && page.table_name == *SNAPSHOT_TABLES.last().expect("固定快照表非空");
        let response = proto::ReadSnapshotPage {
            snapshot_token: request.snapshot_token,
            table_name: page.table_name,
            cursor: request.cursor,
            limit: request.limit,
            rows: page.rows.into_iter().map(|row| row.payload).collect(),
            next_cursor: page.next_cursor.unwrap_or_default(),
            done: page.done,
        };
        if is_final_page {
            state.active_snapshot = None;
        }
        Ok(response)
    }
}

/// 只在调用真实 PostgreSQL 事务之前失败一次；第二次调用完全转交 `CentralStore`，用于证明
/// SyncEngine 不会把“准备提交”误当成“已经提交”。
struct FailBeforeCommit<'a> {
    central: &'a mut CentralStore,
    fail_next: bool,
    apply_attempts: usize,
}

impl<'a> FailBeforeCommit<'a> {
    fn new(central: &'a mut CentralStore) -> Self {
        Self {
            central,
            fail_next: true,
            apply_attempts: 0,
        }
    }
}

#[allow(async_fn_in_trait)]
impl SyncRepository for FailBeforeCommit<'_> {
    type Snapshot<'a>
        = CentralSnapshot<'a>
    where
        Self: 'a;

    async fn cursor(&self, machine_id: &MachineId) -> Result<u64, SyncError> {
        Ok(self.central.sync_cursor(machine_id).await?)
    }

    async fn apply_batch(
        &mut self,
        machine_id: &MachineId,
        batch: &proto::SyncChangeBatch,
    ) -> Result<u64, SyncError> {
        self.apply_attempts += 1;
        if std::mem::take(&mut self.fail_next) {
            return Err(SyncError::Backend("fixture failed before PG commit".into()));
        }
        Ok(self.central.apply_sync_batch(machine_id, batch).await?)
    }

    async fn begin_snapshot(
        &mut self,
        machine_id: &MachineId,
        snapshot_high_seq: u64,
    ) -> Result<Self::Snapshot<'_>, SyncError> {
        Ok(self
            .central
            .begin_snapshot_replace(machine_id, snapshot_high_seq)
            .await?)
    }
}

fn postgres_url() -> String {
    std::env::var("DEDUP_TEST_POSTGRES_URL").expect("ignored test requires DEDUP_TEST_POSTGRES_URL")
}

async fn postgres_client(url: &str) -> tokio_postgres::Client {
    let (client, connection) = tokio_postgres::connect(url, tokio_postgres::NoTls)
        .await
        .unwrap();
    tokio::spawn(async move {
        connection.await.unwrap();
    });
    client
}

fn unique_machine_id() -> MachineId {
    let left = Uuid::now_v7().as_u128();
    let right = Uuid::now_v7().as_u128();
    MachineId::parse(&format!("{left:032x}{right:032x}")).unwrap()
}

fn content_md5(machine: &MachineId, ordinal: u64) -> [u8; 16] {
    let mut md5 = [0_u8; 16];
    md5[..8].copy_from_slice(&ordinal.to_be_bytes());
    let machine_prefix = u64::from_str_radix(&machine.as_str()[..16], 16).unwrap();
    md5[8..].copy_from_slice(&machine_prefix.to_be_bytes());
    md5
}

const fn file_size_for(ordinal: u64) -> i64 {
    10_000 + ordinal as i64
}

fn scanned(machine: &MachineId, ordinal: u64, extension: &str) -> ScannedPath {
    let path = format!(
        r"C:\PostgresSyncE2E\{}\file-{ordinal:04}.{extension}",
        &machine.as_str()[..16]
    );
    ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        file_size_for(ordinal) as u64,
    )
}

fn image_stage2(seed: u8) -> ImageStage2 {
    let mut sobel = [0.0_f32; 128];
    sobel[usize::from(seed) % sobel.len()] = 1.0;
    ImageStage2 {
        phash_parts: [u64::from(seed); 9],
        sobel,
    }
}

fn proto_batch(batch: SyncBatch) -> proto::SyncChangeBatch {
    proto::SyncChangeBatch {
        changes: batch.changes,
        high_seq: batch.high_seq,
        pruned_through_seq: batch.pruned_through_seq,
    }
}

fn sync_error(error: StoreError) -> SyncError {
    match error {
        StoreError::SnapshotRequired { .. } => SyncError::SnapshotRequired,
        other => SyncError::Backend(other.to_string()),
    }
}

fn seed_successful_delete(node: &StoreNode, machine: &MachineId) -> LocationKey {
    node.with_store_mut(|store| {
        let deleted = store
            .upsert_content_and_location(
                &scanned(machine, 20_000, "deleted"),
                content_md5(machine, 20_000),
                MediaKind::Other,
            )
            .unwrap();
        let kept = store
            .upsert_content_and_location(
                &scanned(machine, 20_001, "kept"),
                content_md5(machine, 20_001),
                MediaKind::Other,
            )
            .unwrap();
        let deleted_location = LocationKey::new(
            machine.clone(),
            scanned(machine, 20_000, "deleted").normalized_path,
        );
        let kept_location = LocationKey::new(
            machine.clone(),
            scanned(machine, 20_001, "kept").normalized_path,
        );
        let run = store
            .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
            .unwrap();
        let group_id = format!("postgres-sync-delete-{}", run.as_uuid());
        store
            .replace_groups(
                run,
                &[GroupWrite {
                    group_id: group_id.clone(),
                    kind: GroupKind::Exact,
                    representative: kept.key,
                    members: vec![
                        GroupMemberWrite::new(kept_location.clone(), kept.key, true),
                        GroupMemberWrite::new(deleted_location.clone(), deleted.key, false),
                    ],
                }],
            )
            .unwrap();
        store
            .save_review_mark(run, &group_id, &kept_location, ReviewDecision::Keep)
            .unwrap();
        store
            .save_review_mark(run, &group_id, &deleted_location, ReviewDecision::Delete)
            .unwrap();
        let batch = store
            .create_delete_batch(
                run,
                &[ConfirmedDeleteItem::new(
                    group_id,
                    deleted_location.clone(),
                    deleted.key,
                )],
                DeleteMode::RecycleBin,
                2,
            )
            .unwrap();
        store
            .apply_delete_results(
                &batch.batch_id,
                &[DeleteResult::new(
                    batch.items[0].item_id.clone(),
                    DeleteOutcome::Recycled,
                    None,
                )],
            )
            .unwrap();
        deleted_location
    })
}

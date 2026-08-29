//! 中心当前事实与瞬态复核边界的 PostgreSQL 行为测试。

use dedup_central_store::{
    CentralAnalysisInput, CentralAnalysisNode, CentralGroupKind, CentralGroupMember,
    CentralGroupWrite, CentralReviewDecision, CentralStore,
};
use dedup_core::{AnalysisRunId, ContentKey, LocationKey, MachineId, NormalizedPath, TaskId};
use dedup_protocol::proto;
use tokio_postgres::{Client, NoTls};
use uuid::Uuid;

/// 旧复核和墓碑载荷只用于兼容输入；中心当前窗口只返回当前事实。
#[tokio::test]
#[ignore = "requires DEDUP_TEST_POSTGRES_URL"]
async fn current_windows_ignore_review_history_and_tombstone_sync() {
    let url = std::env::var("DEDUP_TEST_POSTGRES_URL").unwrap();
    let machine = unique_machine();
    let content = ContentKey::new([0x51; 16], 510);
    let keep = location(machine.clone(), r"C:\TransientHistory\keep.bin");
    let delete = location(machine.clone(), r"C:\TransientHistory\delete.bin");

    let mut store = CentralStore::connect(&url).await.unwrap();
    store
        .apply_sync_batch(
            &machine,
            &batch(vec![
                content_change(1, content),
                file_change(2, &keep, content, true),
                file_change(3, &delete, content, true),
            ]),
        )
        .await
        .unwrap();

    let run_id = store
        .create_analysis_run(
            &dedup_core::Thresholds::default(),
            &[CentralAnalysisNode {
                machine_id: machine.clone(),
                task_id: TaskId::new(),
                task_highwater: 0,
                sync_highwater: 3,
                task_status: "completed".into(),
            }],
        )
        .await
        .unwrap();
    store
        .insert_analysis_inputs(
            run_id,
            &[
                CentralAnalysisInput {
                    content,
                    location: keep.clone(),
                },
                CentralAnalysisInput {
                    content,
                    location: delete.clone(),
                },
            ],
        )
        .await
        .unwrap();
    let group_id = format!("transient-history-{}", run_id.as_uuid());
    store
        .replace_groups(
            run_id,
            &[CentralGroupWrite {
                group_id: group_id.clone(),
                kind: CentralGroupKind::Exact,
                representative: content,
                members: vec![
                    member(keep.clone(), content, true),
                    member(delete.clone(), content, false),
                ],
            }],
        )
        .await
        .unwrap();
    store
        .save_review_mark(run_id, &group_id, &keep, CentralReviewDecision::Keep)
        .await
        .unwrap();
    store
        .save_review_mark(run_id, &group_id, &delete, CentralReviewDecision::Delete)
        .await
        .unwrap();

    let members = store
        .page_group_members(run_id, &group_id, None, 10)
        .await
        .unwrap()
        .items;
    assert_eq!(members.len(), 2);
    assert!(
        members
            .iter()
            .all(|member| member.review == CentralReviewDecision::Undecided)
    );

    store
        .apply_sync_batch(
            &machine,
            &batch(vec![
                file_change(4, &delete, content, false),
                tombstone_change(5, &delete, content),
            ]),
        )
        .await
        .unwrap();
    drop(store);

    let (mut client, connection) = tokio_postgres::connect(&url, NoTls).await.unwrap();
    let connection_task = tokio::spawn(async move {
        let _ = connection.await;
    });
    let active: bool = client
        .query_one(
            "SELECT active FROM file_locations WHERE machine_id=$1 AND normalized_path=$2",
            &[&machine.as_str(), &delete.normalized_path().as_str()],
        )
        .await
        .unwrap()
        .get(0);
    let tombstones: i64 = client
        .query_one(
            "SELECT COUNT(*) FROM deletion_tombstones WHERE machine_id=$1 AND normalized_path=$2",
            &[&machine.as_str(), &delete.normalized_path().as_str()],
        )
        .await
        .unwrap()
        .get(0);
    assert!(!active, "文件 active=false 才是删除后的当前事实");
    assert_eq!(tombstones, 0, "旧墓碑载荷不得写入中心历史表");
    cleanup(&mut client, machine, content, run_id, &[keep, delete]).await;
    drop(client);
    connection_task.abort();
}

/// 生成本次测试独占的机器 ID。
fn unique_machine() -> MachineId {
    let uuid = Uuid::new_v4();
    let mut bytes = [0_u8; 32];
    bytes[..16].copy_from_slice(uuid.as_bytes());
    bytes[16..].copy_from_slice(uuid.as_bytes());
    MachineId::from_sha256(bytes)
}

/// 创建标准位置键。
fn location(machine: MachineId, path: &str) -> LocationKey {
    LocationKey::new(machine, NormalizedPath::new(path).unwrap())
}

/// 创建同步批次。
fn batch(changes: Vec<proto::SyncChange>) -> proto::SyncChangeBatch {
    proto::SyncChangeBatch {
        changes,
        high_seq: 0,
        pruned_through_seq: 0,
    }
}

/// 编码内容当前事实。
fn content_change(seq: u64, key: ContentKey) -> proto::SyncChange {
    change(
        seq,
        "content",
        Payload::new(2)
            .bytes(&key.md5())
            .u64(key.file_size())
            .u8(1)
            .u8(1)
            .finish(),
    )
}

/// 编码文件活动状态当前事实。
fn file_change(
    seq: u64,
    location: &LocationKey,
    key: ContentKey,
    active: bool,
) -> proto::SyncChange {
    change(
        seq,
        "file",
        Payload::new(1)
            .text(location.machine_id().as_str())
            .text(location.normalized_path().as_str())
            .text(location.normalized_path().as_str())
            .u64(key.file_size())
            .bytes(&key.md5())
            .u64(key.file_size())
            .u8(u8::from(active))
            .finish(),
    )
}

/// 编码旧版本墓碑载荷，确认其仍可被接收但不产生历史写入。
fn tombstone_change(seq: u64, location: &LocationKey, key: ContentKey) -> proto::SyncChange {
    change(
        seq,
        "deletion_tombstone",
        Payload::new(1)
            .text(location.machine_id().as_str())
            .text(location.normalized_path().as_str())
            .bytes(&key.md5())
            .u64(key.file_size())
            .text("deleted")
            .finish(),
    )
}

/// 组装协议变更。
fn change(seq: u64, entity_kind: &str, payload: Vec<u8>) -> proto::SyncChange {
    proto::SyncChange {
        seq,
        entity_kind: entity_kind.into(),
        payload,
    }
}

/// 创建中心分组成员。
fn member(location: LocationKey, content: ContentKey, representative: bool) -> CentralGroupMember {
    CentralGroupMember {
        location,
        content,
        representative,
        stage1_score: 1.0,
        phash_passed_parts: None,
        stage2_score: None,
        review: CentralReviewDecision::Undecided,
        width: None,
        height: None,
        quality: None,
        active: true,
    }
}

/// 删除测试产生的所有关联行，不改变正式中心库其他机器的数据。
async fn cleanup(
    client: &mut Client,
    machine: MachineId,
    content: ContentKey,
    run_id: AnalysisRunId,
    locations: &[LocationKey],
) {
    let transaction = client.transaction().await.unwrap();
    let run = run_id.as_uuid().to_string();
    transaction
        .execute("DELETE FROM review_marks WHERE analysis_run_id=$1", &[&run])
        .await
        .unwrap();
    transaction
        .execute(
            "DELETE FROM group_members WHERE analysis_run_id=$1",
            &[&run],
        )
        .await
        .unwrap();
    transaction
        .execute(
            "DELETE FROM duplicate_groups WHERE analysis_run_id=$1",
            &[&run],
        )
        .await
        .unwrap();
    transaction
        .execute(
            "DELETE FROM analysis_run_inputs WHERE analysis_run_id=$1",
            &[&run],
        )
        .await
        .unwrap();
    transaction
        .execute(
            "DELETE FROM analysis_run_nodes WHERE analysis_run_id=$1",
            &[&run],
        )
        .await
        .unwrap();
    transaction
        .execute(
            "DELETE FROM analysis_runs WHERE analysis_run_id=$1",
            &[&run],
        )
        .await
        .unwrap();
    for location in locations {
        transaction
            .execute(
                "DELETE FROM file_locations WHERE machine_id=$1 AND normalized_path=$2",
                &[
                    &location.machine_id().as_str(),
                    &location.normalized_path().as_str(),
                ],
            )
            .await
            .unwrap();
    }
    transaction
        .execute(
            "DELETE FROM contents WHERE md5=$1 AND file_size=$2",
            &[&content.md5().as_slice(), &(content.file_size() as i64)],
        )
        .await
        .unwrap();
    transaction
        .execute(
            "DELETE FROM sync_cursors WHERE machine_id=$1",
            &[&machine.as_str()],
        )
        .await
        .unwrap();
    transaction
        .execute(
            "DELETE FROM nodes WHERE machine_id=$1",
            &[&machine.as_str()],
        )
        .await
        .unwrap();
    transaction.commit().await.unwrap();
}

/// 测试用的大端长度前缀载荷编码器。
struct Payload(Vec<u8>);

impl Payload {
    /// 创建指定版本载荷。
    fn new(version: u8) -> Self {
        Self(vec![version])
    }

    /// 写入字节串。
    fn bytes(mut self, value: &[u8]) -> Self {
        self.0
            .extend_from_slice(&(value.len() as u32).to_be_bytes());
        self.0.extend_from_slice(value);
        self
    }

    /// 写入 UTF-8 文本。
    fn text(self, value: &str) -> Self {
        self.bytes(value.as_bytes())
    }

    /// 写入无符号 64 位整数。
    fn u64(mut self, value: u64) -> Self {
        self.0.extend_from_slice(&value.to_be_bytes());
        self
    }

    /// 写入无符号 8 位整数。
    fn u8(mut self, value: u8) -> Self {
        self.0.push(value);
        self
    }

    /// 取出载荷。
    fn finish(self) -> Vec<u8> {
        self.0
    }
}

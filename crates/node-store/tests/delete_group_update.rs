//! 删除结果、重复组收缩、代表重选和成员游标的事务契约。

use dedup_core::{
    ContentKey, DeleteMode, DisplayPath, GroupId, LocationKey, MachineId, MediaKind,
    NormalizedPath, Thresholds,
};
use dedup_node_store::{
    AnalysisMode, DeleteOutcome, DeleteResult, GroupKind, GroupMemberWrite, GroupWrite, NodeStore,
    ReviewDecision, ScannedPath,
};

fn machine() -> MachineId {
    MachineId::from_sha256([0x73; 32])
}

fn location(path: &str) -> LocationKey {
    LocationKey::new(machine(), NormalizedPath::new(path).unwrap())
}

fn add_file(store: &mut NodeStore, path: &str, byte: u8) -> (LocationKey, ContentKey) {
    let size = u64::from(byte) + 100;
    let scanned = ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        size,
    );
    let record = store
        .upsert_content_and_location(&scanned, [byte; 16], MediaKind::Other)
        .unwrap();
    (location(path), record.key)
}

/// 两成员组成功回收一个文件后不再构成重复组，组和复核标记一并删除。
#[test]
fn successful_delete_removes_member_and_small_group() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let (delete_location, delete_key) = add_file(&mut store, r"D:\Two\delete.bin", 1);
    let (keep_location, keep_key) = add_file(&mut store, r"D:\Two\keep.bin", 2);
    let group_id = GroupId::new().as_uuid().to_string();
    store
        .replace_groups(
            run,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Exact,
                representative: delete_key,
                members: vec![
                    GroupMemberWrite::new(delete_location.clone(), delete_key, true),
                    GroupMemberWrite::new(keep_location.clone(), keep_key, false),
                ],
            }],
        )
        .unwrap();
    store
        .save_review_mark(run, &group_id, &delete_location, ReviewDecision::Delete)
        .unwrap();
    store
        .save_review_mark(run, &group_id, &keep_location, ReviewDecision::Keep)
        .unwrap();
    let batch = store
        .create_delete_batch(run, &[group_id], DeleteMode::RecycleBin, 2)
        .unwrap();

    let result = DeleteResult::new(
        batch.items[0].item_id.clone(),
        DeleteOutcome::Recycled,
        None,
    );
    store
        .apply_delete_results(&batch.batch_id, std::slice::from_ref(&result))
        .unwrap();
    store
        .apply_delete_results(&batch.batch_id, std::slice::from_ref(&result))
        .unwrap();

    assert!(store.page_groups(run, None, 20).unwrap().items.is_empty());
    assert!(!store.location_is_active(&delete_location).unwrap());
    assert!(store.location_is_active(&keep_location).unwrap());
    let changes = store.pull_changes(0, 1000).unwrap().changes;
    assert_eq!(
        changes
            .iter()
            .filter(|change| change.entity_kind == "deletion_tombstone")
            .count(),
        1,
        "重复提交成功结果只能产生一个墓碑 outbox"
    );
    let snapshot = store.begin_snapshot().unwrap();
    assert_eq!(
        snapshot
            .read_page("deletion_tombstones", "", 1000)
            .unwrap()
            .rows
            .len(),
        1
    );
}

/// failed/skipped 不改变位置或成员；创建批次前必须存在至少一个活动 Keep。
#[test]
fn failed_or_unprotected_delete_keeps_group() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let (first_location, first_key) = add_file(&mut store, r"D:\Fail\first.bin", 3);
    let (second_location, second_key) = add_file(&mut store, r"D:\Fail\second.bin", 4);
    let group_id = GroupId::new().as_uuid().to_string();
    store
        .replace_groups(
            run,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Exact,
                representative: first_key,
                members: vec![
                    GroupMemberWrite::new(first_location.clone(), first_key, true),
                    GroupMemberWrite::new(second_location.clone(), second_key, false),
                ],
            }],
        )
        .unwrap();
    store
        .save_review_mark(run, &group_id, &first_location, ReviewDecision::Delete)
        .unwrap();
    assert!(
        store
            .create_delete_batch(
                run,
                std::slice::from_ref(&group_id),
                DeleteMode::Permanent,
                2
            )
            .is_err()
    );

    store
        .save_review_mark(run, &group_id, &second_location, ReviewDecision::Keep)
        .unwrap();
    let batch = store
        .create_delete_batch(
            run,
            std::slice::from_ref(&group_id),
            DeleteMode::Permanent,
            3,
        )
        .unwrap();
    store
        .apply_delete_results(
            &batch.batch_id,
            &[DeleteResult::new(
                batch.items[0].item_id.clone(),
                DeleteOutcome::Failed,
                Some("sharing violation".into()),
            )],
        )
        .unwrap();
    assert_eq!(store.page_groups(run, None, 20).unwrap().items.len(), 1);
    assert!(store.location_is_active(&first_location).unwrap());
}

/// 删除代表后从明确 Keep 中按位置稳定顺序选新代表；旧成员游标仍可继续。
#[test]
fn representative_delete_selects_first_keep_and_cursor_continues() {
    let mut store = NodeStore::open_in_memory(machine()).unwrap();
    let run = store
        .create_analysis_run(AnalysisMode::Local, Thresholds::default(), 1)
        .unwrap();
    let (a_location, a_key) = add_file(&mut store, r"D:\Three\A.bin", 5);
    let (b_location, b_key) = add_file(&mut store, r"D:\Three\B.bin", 6);
    let (c_location, c_key) = add_file(&mut store, r"D:\Three\C.bin", 7);
    let group_id = GroupId::new().as_uuid().to_string();
    store
        .replace_groups(
            run,
            &[GroupWrite {
                group_id: group_id.clone(),
                kind: GroupKind::Exact,
                representative: a_key,
                members: vec![
                    GroupMemberWrite::new(a_location.clone(), a_key, true),
                    GroupMemberWrite::new(b_location.clone(), b_key, false),
                    GroupMemberWrite::new(c_location.clone(), c_key, false),
                ],
            }],
        )
        .unwrap();
    store
        .save_review_mark(run, &group_id, &a_location, ReviewDecision::Delete)
        .unwrap();
    store
        .save_review_mark(run, &group_id, &b_location, ReviewDecision::Keep)
        .unwrap();
    store
        .save_review_mark(run, &group_id, &c_location, ReviewDecision::Keep)
        .unwrap();
    let first_page = store.page_group_members(run, &group_id, None, 1).unwrap();
    assert_eq!(first_page.items[0].location, a_location);

    let batch = store
        .create_delete_batch(
            run,
            std::slice::from_ref(&group_id),
            DeleteMode::Permanent,
            2,
        )
        .unwrap();
    store
        .apply_delete_results(
            &batch.batch_id,
            &[DeleteResult::new(
                batch.items[0].item_id.clone(),
                DeleteOutcome::Deleted,
                None,
            )],
        )
        .unwrap();

    let group = store.page_groups(run, None, 20).unwrap().items.remove(0);
    assert_eq!(group.representative, b_key);
    let continued = store
        .page_group_members(run, &group_id, first_page.next_cursor.as_deref(), 10)
        .unwrap();
    assert_eq!(continued.items.len(), 2);
    assert_eq!(continued.items[0].location, b_location);
    assert_eq!(continued.items[1].location, c_location);
}

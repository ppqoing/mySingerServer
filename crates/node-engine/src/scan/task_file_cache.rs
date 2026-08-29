//! 瞬态任务文件的本地/远端基础缓存接缝。

use std::path::Path;

use dedup_core::MachineId;
use dedup_node_store::{BaseCacheRecord, StoreError};
use thiserror::Error;

use super::{
    BaseComputeDecision, BaseTaskInput, PlannedScannedPath, base_compute::cache_rank,
    base_persistence::BaseStoreHandle,
};
use crate::{RemoteCacheError, RemoteFeatureCache, contact_sheet_cache::ContactSheetCacheEntry};

/// 瞬态任务路径缓存解析的最大批量，和任务文件生产上限保持一致。
const MAX_PATH_CACHE_BATCH: usize = 1_000;

/// 路径缓存解析阶段返回的有序输入与远端可用性。
#[derive(Debug)]
pub(crate) struct TaskFileCacheResult {
    /// 按扫描输入顺序返回的任务文件候选；完整命中也保留给 Producer 分类。
    pub(crate) inputs: Vec<BaseTaskInput>,
    /// 本轮第一次远端失败或启动降级时的单条告警。
    pub(crate) warning: Option<String>,
    /// 本轮是否仍可使用远端缓存。
    pub(crate) remote_available: bool,
}

/// 路径缓存解析失败；远端错误本身不升级为该错误，而是降级为本地查询。
#[derive(Debug, Error)]
pub(crate) enum TaskFileCacheError {
    /// 输入批次超过固定上限或远端返回破坏顺序/大小契约。
    #[error("任务文件缓存输入无效: {0}")]
    InvalidInput(String),
    /// 本地 SQLite 批量查询或缓存导入失败。
    #[error(transparent)]
    Store(#[from] StoreError),
}

/// 查询一批任务路径的本地缓存，并在需要时用一次远端批量结果补齐本地缓存。
///
/// 远端结果只在完整度严格高于本地结果时导入；所有导入完成后再做一次路径批查，
/// 以取得本机 `content_id` 和数据库合并后的最终字段。该函数不创建任务文件，
/// 完整命中和缺失项统一交给既有 `BaseTaskProducer` 分类。
pub(crate) async fn resolve_task_file_cache<R: RemoteFeatureCache>(
    store: &BaseStoreHandle,
    remote: &R,
    remote_available: bool,
    machine_id: &MachineId,
    planned: &[PlannedScannedPath],
    contact_sheet_root: &Path,
    force_recompute: bool,
) -> Result<TaskFileCacheResult, TaskFileCacheError> {
    if planned.len() > MAX_PATH_CACHE_BATCH {
        return Err(TaskFileCacheError::InvalidInput(format!(
            "路径缓存批次不能超过 {MAX_PATH_CACHE_BATCH} 行"
        )));
    }
    if planned.is_empty() {
        return Ok(TaskFileCacheResult {
            inputs: Vec::new(),
            warning: remote.startup_warning().map(str::to_owned),
            remote_available: remote_available && remote.startup_warning().is_none(),
        });
    }

    let scanned = planned
        .iter()
        .map(|row| row.scanned.clone())
        .collect::<Vec<_>>();
    // 一次 actor call 完成全部路径查询；不得退回逐文件 SELECT。
    let mut local = store.lookup_base_cache_by_paths(&scanned)?;
    if local.len() != planned.len() {
        return Err(TaskFileCacheError::InvalidInput(
            "本地路径缓存批量查询返回数量不一致".into(),
        ));
    }

    let mut warning = remote.startup_warning().map(str::to_owned);
    let mut available = remote_available && warning.is_none();
    let mut remote_indexes = Vec::new();
    if available && !force_recompute {
        for (index, cached) in local.iter().enumerate() {
            let contact_valid = contact_sheet_valid_for_record(contact_sheet_root, cached.as_ref());
            if BaseComputeDecision::for_cache(cached.as_ref(), contact_valid, false).missing_parts()
                != 0
            {
                remote_indexes.push(index);
            }
        }
    }

    if available && !remote_indexes.is_empty() {
        let remote_paths = remote_indexes
            .iter()
            .map(|&index| scanned[index].clone())
            .collect::<Vec<_>>();
        match remote.lookup_paths(machine_id, &remote_paths).await {
            Ok(remote_records) if remote_records.len() == remote_paths.len() => {
                if remote_records
                    .iter()
                    .zip(&remote_paths)
                    .all(|(record, path)| {
                        record
                            .as_ref()
                            .is_none_or(|record| record.content_key.file_size() == path.file_size)
                    })
                {
                    let mut imported = false;
                    for (&index, remote_record) in remote_indexes.iter().zip(remote_records) {
                        if remote_is_more_complete(remote_record.as_ref(), local[index].as_ref()) {
                            store.import_base_cache_record(
                                &scanned[index],
                                remote_record.as_ref().expect("上方已检查 Some"),
                            )?;
                            imported = true;
                        }
                    }
                    if imported {
                        // 所有远端导入完成后只做一次校准查询，取得最终本机 ID/合并字段。
                        local = store.lookup_base_cache_by_paths(&scanned)?;
                        if local.len() != planned.len() {
                            return Err(TaskFileCacheError::InvalidInput(
                                "导入后的路径缓存批量查询返回数量不一致".into(),
                            ));
                        }
                    }
                } else {
                    available = false;
                    warning =
                        Some("远端路径缓存返回了不匹配的文件大小，本轮降级为 SQLite-only".into());
                }
            }
            Ok(_) => {
                available = false;
                warning = Some("远端路径缓存返回数量不一致，本轮降级为 SQLite-only".into());
            }
            Err(error) => {
                available = false;
                warning = Some(remote_warning(error));
            }
        }
    }

    let inputs = planned
        .iter()
        .zip(local)
        .map(|(planned, cached)| BaseTaskInput {
            contact_sheet_valid: contact_sheet_valid_for_record(
                contact_sheet_root,
                cached.as_ref(),
            ),
            planned: planned.clone(),
            cached,
            force_recompute,
        })
        .collect();
    Ok(TaskFileCacheResult {
        inputs,
        warning,
        remote_available: available,
    })
}

/// 远端缓存只有在基础完整度严格更高时才覆盖本地记录。
fn remote_is_more_complete(
    remote: Option<&BaseCacheRecord>,
    local: Option<&BaseCacheRecord>,
) -> bool {
    cache_rank(remote) > cache_rank(local)
}

/// 只把本机 MD5 派生位置下的可解码联系表视为有效。
fn contact_sheet_valid_for_record(root: &Path, cached: Option<&BaseCacheRecord>) -> bool {
    cached.is_some_and(|record| {
        ContactSheetCacheEntry::from_md5(root, record.content_key.md5())
            .is_valid(record.contact_sheet_relative_path.as_deref())
    })
}

/// 把远端错误转换成一条可观察的本轮降级告警。
fn remote_warning(error: RemoteCacheError) -> String {
    format!("远端路径缓存查询失败，本轮降级为 SQLite-only: {error}")
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};

    use dedup_core::{ContentKey, DisplayPath, MachineId, MediaKind, NormalizedPath};
    use dedup_node_store::{BaseCacheRecord, NodeStore, ScannedPath};
    use dedup_protocol::proto;
    use dedup_windows::{LocalDiskKind, PhysicalDiskId};

    use crate::{
        RemoteCacheError, RemoteFeatureCache,
        scan::{PlannedScannedPath, TaskDiskLane, base_persistence::BaseStoreActor},
    };

    use super::resolve_task_file_cache;

    #[derive(Clone)]
    struct CountingRemote {
        path_calls: Arc<Mutex<Vec<usize>>>,
        hit: Option<BaseCacheRecord>,
        fail: bool,
    }

    impl RemoteFeatureCache for CountingRemote {
        async fn lookup_paths(
            &self,
            _machine_id: &MachineId,
            paths: &[ScannedPath],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            self.path_calls.lock().unwrap().push(paths.len());
            if self.fail {
                return Err(RemoteCacheError::ConnectTimeout);
            }
            Ok(paths.iter().map(|_| self.hit.clone()).collect::<Vec<_>>())
        }

        async fn lookup_contents(
            &self,
            keys: &[ContentKey],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            Ok(vec![None; keys.len()])
        }

        async fn publish_outbox(
            &mut self,
            _machine_id: &MachineId,
            _batch: &proto::SyncChangeBatch,
        ) -> Result<u64, RemoteCacheError> {
            Ok(0)
        }
    }

    fn lane() -> TaskDiskLane {
        TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([7]).unwrap(),
            physical_disk_numbers: vec![7],
            disk_kind: LocalDiskKind::Hdd,
            configured_weight: 1,
            per_disk_limit: 1,
        }
    }

    fn planned(path: &str, size: u64) -> PlannedScannedPath {
        PlannedScannedPath {
            scanned: ScannedPath::new(
                NormalizedPath::new(path).unwrap(),
                DisplayPath::new(path).unwrap(),
                size,
            ),
            lane: lane(),
        }
    }

    fn remote_record(md5: [u8; 16], size: u64) -> BaseCacheRecord {
        BaseCacheRecord {
            content_id: None,
            content_key: ContentKey::new(md5, size),
            media_kind: MediaKind::Other,
            base_complete: true,
            width: None,
            height: None,
            duration_ms: None,
            stage1: None,
            image_stage2: None,
            video_stage2: Box::new([None; 6]),
            contact_sheet_relative_path: None,
        }
    }

    #[tokio::test]
    async fn mixed_path_lookup_imports_only_higher_remote_cache_in_input_order() {
        let machine = MachineId::from_sha256([0xD4; 32]);
        let mut node_store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let local_path = ScannedPath::new(
            NormalizedPath::new(r"C:\local.bin").unwrap(),
            DisplayPath::new(r"C:\local.bin").unwrap(),
            10,
        );
        node_store
            .upsert_content_and_location(&local_path, [1; 16], MediaKind::Other)
            .unwrap();
        node_store
            .mark_base_complete(
                node_store
                    .lookup_base_cache_by_paths(std::slice::from_ref(&local_path))
                    .unwrap()[0]
                    .as_ref()
                    .unwrap()
                    .content_id
                    .unwrap(),
            )
            .unwrap();
        let (actor, store, acknowledgements) = BaseStoreActor::spawn(node_store, 2);
        let calls = Arc::new(Mutex::new(Vec::new()));
        let remote = CountingRemote {
            path_calls: Arc::clone(&calls),
            hit: Some(remote_record([2; 16], 11)),
            fail: false,
        };
        let rows = vec![planned(r"C:\local.bin", 10), planned(r"C:\remote.bin", 11)];

        let result = resolve_task_file_cache(
            &store,
            &remote,
            true,
            &machine,
            &rows,
            std::path::Path::new(r"C:\contacts"),
            false,
        )
        .await
        .unwrap();

        assert_eq!(*calls.lock().unwrap(), vec![1]);
        assert_eq!(result.inputs.len(), 2);
        assert_eq!(
            result.inputs[0].planned.scanned.normalized_path.as_str(),
            r"C:\LOCAL.BIN"
        );
        assert_eq!(
            result.inputs[1].planned.scanned.normalized_path.as_str(),
            r"C:\REMOTE.BIN"
        );
        assert_eq!(
            result.inputs[0].cached.as_ref().unwrap().content_key.md5(),
            [1; 16]
        );
        assert_eq!(
            result.inputs[1].cached.as_ref().unwrap().content_key.md5(),
            [2; 16]
        );
        assert!(
            result.inputs[0]
                .cached
                .as_ref()
                .unwrap()
                .content_id
                .is_some()
        );
        assert!(
            result.inputs[1]
                .cached
                .as_ref()
                .unwrap()
                .content_id
                .is_some()
        );
        assert!(result.remote_available);
        assert!(result.warning.is_none());

        drop(store);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn remote_path_failure_falls_back_to_local_once() {
        let machine = MachineId::from_sha256([0xD5; 32]);
        let node_store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let (actor, store, acknowledgements) = BaseStoreActor::spawn(node_store, 2);
        let calls = Arc::new(Mutex::new(Vec::new()));
        let remote = CountingRemote {
            path_calls: Arc::clone(&calls),
            hit: None,
            fail: true,
        };
        let result = resolve_task_file_cache(
            &store,
            &remote,
            true,
            &machine,
            &[planned(r"C:\fallback.bin", 12)],
            std::path::Path::new(r"C:\contacts"),
            false,
        )
        .await
        .unwrap();

        assert_eq!(*calls.lock().unwrap(), vec![1]);
        assert!(!result.remote_available);
        assert!(result.warning.is_some());
        assert!(result.inputs[0].cached.is_none());
        drop(store);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn path_cache_rejects_more_than_one_thousand_rows_before_remote_call() {
        let machine = MachineId::from_sha256([0xD6; 32]);
        let node_store = NodeStore::open_in_memory(machine.clone()).unwrap();
        let (actor, store, acknowledgements) = BaseStoreActor::spawn(node_store, 2);
        let calls = Arc::new(Mutex::new(Vec::new()));
        let remote = CountingRemote {
            path_calls: Arc::clone(&calls),
            hit: None,
            fail: false,
        };
        let rows = (0..1_001)
            .map(|index| planned(&format!(r"C:\bulk-{index}.bin"), index as u64))
            .collect::<Vec<_>>();

        let error = resolve_task_file_cache(
            &store,
            &remote,
            true,
            &machine,
            &rows,
            std::path::Path::new(r"C:\contacts"),
            false,
        )
        .await
        .expect_err("超出固定批量上限必须在查询前拒绝");
        assert!(error.to_string().contains("1000"));
        assert!(calls.lock().unwrap().is_empty());
        drop(store);
        drop(acknowledgements);
        actor.finish().await.unwrap();
    }
}

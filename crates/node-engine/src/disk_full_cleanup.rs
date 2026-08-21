//! Windows 磁盘满时冻结并清空同物理盘的显式可再生产物集合。

use std::{
    fs, io,
    path::Path,
    sync::{Arc, Mutex},
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_node_store::NodeStore;
use dedup_windows::resolve_storage_location;

use crate::artifact_registry::{ArtifactKind, RegenerableArtifactRegistry};

const ERROR_HANDLE_DISK_FULL: i32 = 39;
const ERROR_DISK_FULL: i32 = 112;

/// 把待写路径和 registry 文件映射到物理盘交集。
pub trait ArtifactDiskResolver: Send + Sync + 'static {
    /// 两条本机路径是否至少共享一个底层物理盘。
    fn shares_physical_disk(&self, artifact: &Path, write_target: &Path) -> io::Result<bool>;
}

/// 使用真实 Windows 卷 extent 查询的生产 resolver。
#[derive(Clone, Copy, Debug, Default)]
pub struct SystemArtifactDiskResolver;

impl ArtifactDiskResolver for SystemArtifactDiskResolver {
    fn shares_physical_disk(&self, artifact: &Path, write_target: &Path) -> io::Result<bool> {
        let artifact = resolve_storage_location(artifact)?;
        let target = resolve_storage_location(write_target)?;
        Ok(artifact.shares_physical_disk(&target))
    }
}

/// 最近一次磁盘满清理的进程内摘要；不会写入文件故障表。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CleanupSummary {
    /// 本次触发的 Unix 毫秒时间。
    pub triggered_at_unix_ms: u64,
    /// 实际删除的文件数量。
    pub deleted_files: usize,
    /// 实际删除前统计的总字节数。
    pub deleted_bytes: u64,
    /// 冻结时因活动租约跳过的数量。
    pub skipped_active: usize,
    /// 因不共享待写物理盘跳过的数量。
    pub skipped_other_disk: usize,
    /// 查询、读取元数据或删除失败的文件数量。
    pub failed_files: usize,
}

/// 持有显式 registry、物理盘 resolver 和最近一次运行摘要的进程级清理器。
#[derive(Clone)]
pub struct DiskFullCleaner {
    registry: Arc<RegenerableArtifactRegistry>,
    resolver: Arc<dyn ArtifactDiskResolver>,
    recent: Arc<Mutex<Option<CleanupSummary>>>,
}

impl DiskFullCleaner {
    /// 组合一个共享 registry 和生产或测试 resolver。
    pub fn new<R>(registry: Arc<RegenerableArtifactRegistry>, resolver: R) -> Self
    where
        R: ArtifactDiskResolver,
    {
        Self {
            registry,
            resolver: Arc::new(resolver),
            recent: Arc::new(Mutex::new(None)),
        }
    }

    /// 返回最近一次磁盘满触发的清理摘要。
    pub fn recent_summary(&self) -> Option<CleanupSummary> {
        self.recent.lock().ok().and_then(|summary| summary.clone())
    }

    fn cleanup(&self, store: &mut NodeStore, write_target: &Path) -> io::Result<CleanupSummary> {
        let frozen = self.registry.freeze_inactive()?;
        let mut summary = CleanupSummary {
            triggered_at_unix_ms: SystemTime::now()
                .duration_since(UNIX_EPOCH)
                .unwrap_or_default()
                .as_millis()
                .try_into()
                .unwrap_or(u64::MAX),
            deleted_files: 0,
            deleted_bytes: 0,
            skipped_active: frozen.skipped_active,
            skipped_other_disk: 0,
            failed_files: 0,
        };
        let mut removed_contact_references = Vec::new();
        let mut deleted_claims = Vec::new();
        for claim in frozen.claims {
            match self
                .resolver
                .shares_physical_disk(claim.path(), write_target)
            {
                Ok(true) => {}
                Ok(false) => {
                    summary.skipped_other_disk += 1;
                    continue;
                }
                Err(_) => {
                    summary.failed_files += 1;
                    continue;
                }
            }
            let file_size = match fs::metadata(claim.path()) {
                Ok(metadata) => Some(metadata.len()),
                Err(error) if error.kind() == io::ErrorKind::NotFound => None,
                Err(_) => {
                    summary.failed_files += 1;
                    continue;
                }
            };
            match fs::remove_file(claim.path()) {
                Ok(()) => {
                    summary.deleted_files += 1;
                    summary.deleted_bytes = summary
                        .deleted_bytes
                        .saturating_add(file_size.unwrap_or_default());
                }
                Err(error) if error.kind() == io::ErrorKind::NotFound => {}
                Err(_) => {
                    summary.failed_files += 1;
                    continue;
                }
            }
            if claim.kind() == ArtifactKind::ContactSheet
                && let Some(reference) = claim.contact_sheet_reference()
            {
                removed_contact_references.push(reference.to_owned());
            }
            deleted_claims.push(claim);
        }
        let clear_result = store
            .clear_contact_sheet_references(&removed_contact_references)
            .map_err(|error| io::Error::other(error.to_string()));
        if clear_result.is_ok() {
            for claim in deleted_claims {
                claim.mark_deleted();
            }
        }
        if let Ok(mut recent) = self.recent.lock() {
            *recent = Some(summary.clone());
        }
        clear_result?;
        Ok(summary)
    }
}

/// 执行一次写入；仅首个 Windows 112/39 触发完整清理并重试一次。
pub fn write_with_disk_full_cleanup<T>(
    cleaner: &DiskFullCleaner,
    store: &mut NodeStore,
    write_target: &Path,
    mut write: impl FnMut() -> io::Result<T>,
) -> io::Result<T> {
    match write() {
        Ok(value) => Ok(value),
        Err(error) if is_disk_full(&error) => {
            cleaner.cleanup(store, write_target)?;
            write()
        }
        Err(error) => Err(error),
    }
}

fn is_disk_full(error: &io::Error) -> bool {
    matches!(
        error.raw_os_error(),
        Some(ERROR_DISK_FULL | ERROR_HANDLE_DISK_FULL)
    )
}

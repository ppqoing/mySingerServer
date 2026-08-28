//! 扫描枚举前冻结根目录对应的物理磁盘 lane。

use std::{cmp::Ordering, collections::BTreeMap, io, path::Path};

use dedup_core::{DiskReadConfig, DisplayPath, NormalizedPath};
use dedup_node_store::ScannedPath;
use dedup_windows::{LocalDiskKind, PhysicalDiskId, resolve_storage_location};

use super::ScanError;

/// 一个扫描根在枚举前解析出的稳定存储位置。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResolvedScanRootStorage {
    /// 已规范化的扫描根。
    pub normalized_root: NormalizedPath,
    /// 一个或多个底层物理盘组成的稳定身份。
    pub physical_disk_id: PhysicalDiskId,
    /// Windows 边界保守判断出的介质类型。
    pub disk_kind: LocalDiskKind,
}

/// 只在建立扫描根计划时调用的存储位置解析器。
pub trait ScanRootStorageResolver: Send + Sync {
    /// 解析一个扫描根的物理盘集合和介质类型。
    fn resolve(&self, root: &Path) -> io::Result<ResolvedScanRootStorage>;
}

/// 生产环境使用的 Windows 根存储解析器。
#[derive(Clone, Copy, Debug, Default)]
pub struct SystemScanRootStorageResolver;

impl ScanRootStorageResolver for SystemScanRootStorageResolver {
    fn resolve(&self, root: &Path) -> io::Result<ResolvedScanRootStorage> {
        let storage = resolve_storage_location(root)?;
        let normalized_root = NormalizedPath::new(root).map_err(io::Error::other)?;
        Ok(ResolvedScanRootStorage {
            normalized_root,
            physical_disk_id: storage.physical_disk_id().clone(),
            disk_kind: storage.disk_kind(),
        })
    }
}

/// 本轮任务项使用的冻结物理盘 lane。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskDiskLane {
    /// 一个或多个底层物理盘组成的稳定身份。
    pub physical_disk_id: PhysicalDiskId,
    /// 排序去重后的全部底层物理盘编号。
    pub physical_disk_numbers: Vec<u32>,
    /// 本轮根解析确定的 HDD、SSD 或保守 Unknown。
    pub disk_kind: LocalDiskKind,
    /// 后续 dispatcher 使用的本轮配置权重。
    pub configured_weight: usize,
    /// 当前 scheduler 使用的本轮逐盘许可上限。
    pub per_disk_limit: usize,
}

/// 已枚举路径及其唯一冻结 lane。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PlannedScannedPath {
    /// 枚举器返回的规范路径、显示路径和文件大小。
    pub scanned: ScannedPath,
    /// 该路径在本轮扫描中消费的冻结 lane。
    pub lane: TaskDiskLane,
}

/// 扫描开始时一次建立、随后只读的根与 lane 计划。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ScanDiskPlan {
    roots: Vec<RootLane>,
    lanes: Vec<TaskDiskLane>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct RootLane {
    normalized_root: NormalizedPath,
    lane_index: usize,
}

#[derive(Clone, Debug)]
struct LaneAccumulator {
    physical_disk_id: PhysicalDiskId,
    disk_kinds: Vec<LocalDiskKind>,
}

impl ScanDiskPlan {
    /// 在首次调用文件枚举器前解析、合并并冻结全部扫描根。
    pub fn build(
        roots: &[DisplayPath],
        read_config: &DiskReadConfig,
        resolver: &dyn ScanRootStorageResolver,
    ) -> Result<Self, ScanError> {
        let mut normalized_roots = BTreeMap::<NormalizedPath, DisplayPath>::new();
        for root in roots {
            let normalized = NormalizedPath::new(root.as_path())
                .map_err(|error| ScanError::InvalidResult(error.to_string()))?;
            normalized_roots
                .entry(normalized)
                .or_insert_with(|| root.clone());
        }

        let mut resolved = Vec::with_capacity(normalized_roots.len());
        for (normalized_root, display_root) in normalized_roots {
            let storage = resolver.resolve(display_root.as_path()).map_err(|source| {
                ScanError::ScanRootStorageResolveFailed {
                    root: display_root.as_path().display().to_string(),
                    message: source.to_string(),
                }
            })?;
            resolved.push((normalized_root, storage));
        }

        let mut lane_by_disk = BTreeMap::<PhysicalDiskId, usize>::new();
        let mut accumulators = Vec::<LaneAccumulator>::new();
        let mut root_lanes = Vec::with_capacity(resolved.len());
        for (normalized_root, storage) in resolved {
            let lane_index = if let Some(&index) = lane_by_disk.get(&storage.physical_disk_id) {
                let kinds = &mut accumulators[index].disk_kinds;
                if !kinds.contains(&storage.disk_kind) {
                    kinds.push(storage.disk_kind);
                }
                index
            } else {
                let index = accumulators.len();
                lane_by_disk.insert(storage.physical_disk_id.clone(), index);
                accumulators.push(LaneAccumulator {
                    physical_disk_id: storage.physical_disk_id,
                    disk_kinds: vec![storage.disk_kind],
                });
                index
            };
            root_lanes.push(RootLane {
                normalized_root,
                lane_index,
            });
        }

        let mut order = (0..accumulators.len()).collect::<Vec<_>>();
        order.sort_by(|left, right| {
            accumulators[*left]
                .physical_disk_id
                .cmp(&accumulators[*right].physical_disk_id)
        });
        let mut remap = vec![0; accumulators.len()];
        let lanes = order
            .iter()
            .enumerate()
            .map(|(new_index, old_index)| {
                remap[*old_index] = new_index;
                let accumulator = &accumulators[*old_index];
                let disk_kind = if accumulator.disk_kinds.len() == 1 {
                    accumulator.disk_kinds[0]
                } else {
                    LocalDiskKind::Unknown
                };
                let per_disk_limit = merged_limit(read_config, &accumulator.disk_kinds);
                TaskDiskLane {
                    physical_disk_numbers: accumulator.physical_disk_id.disk_numbers().to_vec(),
                    physical_disk_id: accumulator.physical_disk_id.clone(),
                    disk_kind,
                    configured_weight: per_disk_limit,
                    per_disk_limit,
                }
            })
            .collect::<Vec<_>>();
        for root in &mut root_lanes {
            root.lane_index = remap[root.lane_index];
        }

        Ok(Self {
            roots: root_lanes,
            lanes,
        })
    }

    /// 为一条枚举行选择组件最深的扫描根并附加冻结 lane。
    pub fn assign(&self, scanned: ScannedPath) -> Result<PlannedScannedPath, ScanError> {
        let root = self
            .roots
            .iter()
            .filter(|root| scanned.normalized_path.is_within(&root.normalized_root))
            .min_by(|left, right| compare_root_specificity(left, right))
            .ok_or_else(|| {
                ScanError::InvalidResult(format!(
                    "枚举结果不属于扫描根: {}",
                    scanned.normalized_path
                ))
            })?;
        let lane = self
            .lanes
            .get(root.lane_index)
            .cloned()
            .ok_or_else(|| ScanError::InvalidResult("扫描根 lane 索引无效".into()))?;
        Ok(PlannedScannedPath { scanned, lane })
    }

    /// 批量为完整枚举清单附加冻结 lane，保持输入顺序和重复项。
    pub fn assign_all(
        &self,
        rows: impl IntoIterator<Item = ScannedPath>,
    ) -> Result<Vec<PlannedScannedPath>, ScanError> {
        rows.into_iter().map(|row| self.assign(row)).collect()
    }

    /// 返回本计划内冻结的所有 lane，供调度器建立只读视图。
    pub fn lanes(&self) -> &[TaskDiskLane] {
        &self.lanes
    }
}

fn compare_root_specificity(left: &RootLane, right: &RootLane) -> Ordering {
    let left_depth = path_depth(&left.normalized_root);
    let right_depth = path_depth(&right.normalized_root);
    right_depth
        .cmp(&left_depth)
        .then_with(|| left.normalized_root.cmp(&right.normalized_root))
}

fn path_depth(path: &NormalizedPath) -> usize {
    Path::new(path.as_str()).components().count()
}

fn merged_limit(config: &DiskReadConfig, observed: &[LocalDiskKind]) -> usize {
    let limit = observed
        .iter()
        .map(|kind| limit_for_kind(config, *kind))
        .min()
        .unwrap_or_else(|| limit_for_kind(config, LocalDiskKind::Unknown));
    if observed.len() > 1 {
        limit.min(limit_for_kind(config, LocalDiskKind::Unknown))
    } else {
        limit
    }
}

fn limit_for_kind(config: &DiskReadConfig, kind: LocalDiskKind) -> usize {
    match kind {
        LocalDiskKind::Hdd => config.hdd_threads_per_disk,
        LocalDiskKind::Ssd => config.ssd_threads_per_disk,
        LocalDiskKind::Unknown => config.unknown_threads_per_disk,
    }
}

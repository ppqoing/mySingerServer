//! 基础缓存分类和瞬态 TSV 任务生产边界。
//!
//! 本模块只接收已经完成本地缓存导入的批量查询结果，按冻结物理盘 lane 生成真正缺失的
//! 基础任务行。调度状态仍由 [`TaskFileDispatcher`] 独占，生产者只保留当前扫描的清单。

use std::{collections::BTreeMap, io};

use dedup_core::{ContentKey, NormalizedPath};
use dedup_node_store::{BaseCacheRecord, ContentId, ResolvedScanFile};
use thiserror::Error;
use uuid::Uuid;

use super::{BaseComputeDecision, PlannedScannedPath, TaskDiskLane};
use crate::{
    task_dispatch::{TaskFileDispatcher, TaskLanePermitProvider},
    task_files::{
        TaskFileIdentity, TaskFileRecord, TaskWorkKind, TaskWorkMask, validate_task_file_record,
    },
};

/// 单次缓存分类允许接收的最大输入行数。
pub const MAX_BASE_TASK_BATCH: usize = 1_000;

/// 一行已经完成本地/远端缓存选择的基础任务输入。
#[derive(Clone, Debug)]
pub struct BaseTaskInput {
    /// 枚举得到的路径和本轮首次解析的物理盘 lane。
    pub planned: PlannedScannedPath,
    /// 已经导入本地 SQLite 的基础缓存；路径未命中时为 `None`。
    pub cached: Option<BaseCacheRecord>,
    /// 本机联系表是否已经通过 artifact 校验。
    pub contact_sheet_valid: bool,
    /// 是否要求忽略已有缓存并重新计算基础结果。
    pub force_recompute: bool,
}

/// 基础任务生产器的当前扫描清单。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BaseTaskManifest {
    /// 本轮成功枚举过的规范路径，按路径稳定排序且不重复。
    pub seen_paths: Vec<NormalizedPath>,
    /// 已有完整缓存或后续 ACK 成功后可用于收尾的文件，按路径稳定排序。
    pub resolved_files: Vec<ResolvedScanFile>,
    /// 本轮直接复用完整基础缓存的文件数。
    pub cache_hits: usize,
}

/// 一条瞬态任务文件行对应的基础计算上下文。
///
/// 任务文件只保存需要计算的路径和缺失掩码；缓存快照、联系表校验和强制重算标记
/// 由生产端按完整 `TaskFileIdentity` 保存，Hash→Media 续算时继续使用同一上下文。
#[derive(Clone, Debug, PartialEq)]
pub struct TaskFileBaseContext {
    /// 路径批量查询得到的本地内容 ID；未命中或尚未导入时为空。
    pub content_id: Option<ContentId>,
    /// 路径批量查询得到的基础缓存快照；未知内容时为空。
    pub cached: Option<BaseCacheRecord>,
    /// 现有联系表是否已通过校验。
    pub contact_sheet_valid: bool,
    /// 是否忽略已有缓存并强制重新计算。
    pub force_recompute: bool,
}

/// 生产端封闭后交给主循环的 dispatcher 和清单。
pub struct BaseTaskProduction<Provider: TaskLanePermitProvider> {
    /// 继续从已封闭任务文件取得唯一读取许可的 dispatcher。
    pub dispatcher: TaskFileDispatcher<Provider>,
    /// 当前扫描的稳定清单统计。
    pub manifest: BaseTaskManifest,
    /// 任务文件行到缓存判定上下文的稳定映射，续算不得改用模糊 item_id。
    pub contexts: BTreeMap<TaskFileIdentity, TaskFileBaseContext>,
}

/// 缓存分类或任务文件发布失败。
#[derive(Debug, Error)]
pub enum BaseTaskProducerError {
    /// 输入没有满足本地缓存和冻结 lane 契约。
    #[error("基础任务输入无效: {0}")]
    InvalidInput(String),
    /// 任务文件追加或封闭失败。
    #[error("基础任务文件发布失败: {0}")]
    Io(#[from] io::Error),
}

/// 一个路径在本轮已经接受过的分类结果，用于跨批次去重和冲突检测。
#[derive(Clone, Debug, Eq, PartialEq)]
struct SeenPathState {
    file_size: u64,
    lane: TaskDiskLane,
    outcome: ClassifiedOutcome,
}

/// 缓存分类后该路径的唯一后续动作。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ClassifiedOutcome {
    /// 完整缓存命中，只进入 resolved 清单。
    Hit(ContentKey),
    /// 需要由 Worker 计算，携带已知 MD5 和真实缺失位。
    Work {
        known_md5: Option<[u8; 16]>,
        missing: TaskWorkMask,
    },
}

/// 同一 lane 在一个批次内的待追加行，保证 lane 内仍使用输入顺序。
struct LaneBatch {
    lane: TaskDiskLane,
    rows: Vec<TaskFileRecord>,
    contexts: Vec<TaskFileBaseContext>,
}

/// 基础缓存分类到瞬态任务文件的唯一生产者。
pub struct BaseTaskProducer<Provider: TaskLanePermitProvider> {
    dispatcher: Option<TaskFileDispatcher<Provider>>,
    seen_paths: BTreeMap<NormalizedPath, SeenPathState>,
    resolved_files: BTreeMap<NormalizedPath, ResolvedScanFile>,
    pending_contexts: BTreeMap<TaskFileIdentity, TaskFileBaseContext>,
    cache_hits: usize,
    /// 已发布 lane 的完整配置，防止同一任务文件名对应两份配置。
    lane_configs: BTreeMap<String, TaskDiskLane>,
}

impl<Provider: TaskLanePermitProvider> BaseTaskProducer<Provider> {
    /// 创建一个拥有任务文件 dispatcher 的基础任务生产者。
    pub fn new(dispatcher: TaskFileDispatcher<Provider>) -> Self {
        Self {
            dispatcher: Some(dispatcher),
            seen_paths: BTreeMap::new(),
            resolved_files: BTreeMap::new(),
            pending_contexts: BTreeMap::new(),
            cache_hits: 0,
            lane_configs: BTreeMap::new(),
        }
    }

    /// 对一批已经完成缓存查询和本地导入的路径分类，并按 lane 各追加一次 TSV。
    ///
    /// 输入校验和去重在任何文件发布前完成，因此未导入缓存、路径冲突和超大批次都
    /// 不会产生部分任务行。只有真正需要计算的行才会创建对应 lane 文件。
    pub fn append_batch(&mut self, inputs: &[BaseTaskInput]) -> Result<(), BaseTaskProducerError> {
        if self.dispatcher.is_none() {
            return Err(BaseTaskProducerError::InvalidInput(
                "基础任务生产器已经结束或 discard".into(),
            ));
        }
        if inputs.len() > MAX_BASE_TASK_BATCH {
            return Err(BaseTaskProducerError::InvalidInput(format!(
                "基础缓存批次不能超过 {MAX_BASE_TASK_BATCH} 行"
            )));
        }

        let mut staged_seen = BTreeMap::<NormalizedPath, SeenPathState>::new();
        let mut staged_resolved = BTreeMap::<NormalizedPath, ResolvedScanFile>::new();
        let mut staged_batches = BTreeMap::<String, LaneBatch>::new();
        let mut staged_contexts = BTreeMap::new();
        let mut staged_lane_configs = BTreeMap::<String, TaskDiskLane>::new();
        let mut staged_hits = 0_usize;

        for input in inputs {
            let scanned = &input.planned.scanned;
            let path = scanned.normalized_path.clone();
            let outcome = classify_input(input)?;
            let state = SeenPathState {
                file_size: scanned.file_size,
                lane: input.planned.lane.clone(),
                outcome,
            };

            if let Some(previous) = self
                .seen_paths
                .get(&path)
                .or_else(|| staged_seen.get(&path))
            {
                validate_duplicate(&path, previous, &state)?;
                continue;
            }

            let lane_key = lane_file_key(&input.planned.lane)?;
            if let Some(previous_lane) = self
                .lane_configs
                .get(&lane_key)
                .or_else(|| staged_lane_configs.get(&lane_key))
            {
                if previous_lane != &input.planned.lane {
                    return Err(BaseTaskProducerError::InvalidInput(format!(
                        "任务文件 lane 配置冲突: {lane_key}"
                    )));
                }
            } else {
                staged_lane_configs.insert(lane_key.clone(), input.planned.lane.clone());
            }

            staged_seen.insert(path.clone(), state);
            match outcome {
                ClassifiedOutcome::Hit(content) => {
                    staged_hits += 1;
                    staged_resolved.insert(
                        path,
                        ResolvedScanFile {
                            scanned: scanned.clone(),
                            content,
                        },
                    );
                }
                ClassifiedOutcome::Work { known_md5, missing } => {
                    let batch = staged_batches.entry(lane_key).or_insert_with(|| LaneBatch {
                        lane: input.planned.lane.clone(),
                        rows: Vec::new(),
                        contexts: Vec::new(),
                    });
                    batch.rows.push(TaskFileRecord {
                        item_id: Uuid::now_v7(),
                        work_kind: TaskWorkKind::Base,
                        scanned: scanned.clone(),
                        known_md5,
                        missing,
                    });
                    batch.contexts.push(TaskFileBaseContext {
                        content_id: input.cached.as_ref().and_then(|cached| cached.content_id),
                        cached: input.cached.clone(),
                        contact_sheet_valid: input.contact_sheet_valid,
                        force_recompute: input.force_recompute,
                    });
                }
            }
        }

        // 先校验全部 lane 的行，避免前一个 lane 已发布后才发现后一个 lane 的路径字段非法。
        for batch in staged_batches.values() {
            for row in &batch.rows {
                validate_task_file_record(row).map_err(|error| {
                    BaseTaskProducerError::InvalidInput(format!("任务行字段无效: {error}"))
                })?;
            }
        }

        // BTreeMap 保证 lane 文件名顺序稳定；每个非空 lane 只调用一次 append_batch。
        for (lane_key, batch) in staged_batches {
            let identities = self
                .dispatcher
                .as_mut()
                .expect("生产器在 append_batch 期间保持 dispatcher 所有权")
                .append_batch(&batch.lane, &batch.rows)?;
            if identities.len() != batch.contexts.len() {
                return Err(BaseTaskProducerError::InvalidInput(
                    "任务文件身份与基础计算上下文数量不一致".into(),
                ));
            }
            for (identity, context) in identities.into_iter().zip(batch.contexts) {
                staged_contexts.insert(identity, context);
            }
            self.lane_configs.entry(lane_key).or_insert(batch.lane);
        }

        for (path, state) in staged_seen {
            self.seen_paths.insert(path, state);
        }
        for (path, resolved) in staged_resolved {
            self.resolved_files.insert(path, resolved);
        }
        self.cache_hits += staged_hits;
        // 所有 lane 的追加都成功后再提交身份上下文，避免留下无法定位的半成品。
        // 这里不能使用路径作为 key，因为同一路径只是一种显示属性，续算需要完整行身份。
        self.pending_contexts.extend(staged_contexts);
        Ok(())
    }

    /// 封闭所有任务文件，并把 dispatcher 和稳定清单一起移交给后续主循环。
    pub fn seal(&mut self) -> Result<BaseTaskProduction<Provider>, BaseTaskProducerError> {
        self.dispatcher
            .as_mut()
            .ok_or_else(|| {
                BaseTaskProducerError::InvalidInput("基础任务生产器已经结束或 discard".into())
            })?
            .seal()?;
        let dispatcher = self
            .dispatcher
            .take()
            .expect("seal 成功后 dispatcher 必须仍由生产器拥有");
        let manifest = BaseTaskManifest {
            seen_paths: std::mem::take(&mut self.seen_paths).into_keys().collect(),
            resolved_files: std::mem::take(&mut self.resolved_files)
                .into_values()
                .collect(),
            cache_hits: self.cache_hits,
        };
        Ok(BaseTaskProduction {
            dispatcher,
            manifest,
            contexts: std::mem::take(&mut self.pending_contexts),
        })
    }

    /// 在追加或 seal 失败后删除本次运行的精确任务目录，保留 owner 直到删除完成。
    pub fn discard(&mut self) -> Result<(), BaseTaskProducerError> {
        let dispatcher = self.dispatcher.as_mut().ok_or_else(|| {
            BaseTaskProducerError::InvalidInput("基础任务生产器已经结束或 discard".into())
        })?;
        dispatcher.discard()?;
        self.dispatcher = None;
        Ok(())
    }
}

/// 校验一行缓存快照并转换为唯一任务动作。
fn classify_input(input: &BaseTaskInput) -> Result<ClassifiedOutcome, BaseTaskProducerError> {
    let scanned = &input.planned.scanned;
    if let Some(cached) = input.cached.as_ref() {
        if cached.content_id.is_none() {
            return Err(BaseTaskProducerError::InvalidInput(format!(
                "路径 {} 的缓存尚未导入本地 SQLite",
                scanned.normalized_path
            )));
        }
        if cached.content_key.file_size() != scanned.file_size {
            return Err(BaseTaskProducerError::InvalidInput(format!(
                "路径 {} 的缓存文件大小与枚举结果不一致",
                scanned.normalized_path
            )));
        }
    }

    let decision = BaseComputeDecision::for_cache(
        input.cached.as_ref(),
        input.contact_sheet_valid,
        input.force_recompute,
    );
    if decision.missing_parts() == 0 {
        let cached = input
            .cached
            .as_ref()
            .ok_or_else(|| BaseTaskProducerError::InvalidInput("完整命中缺少缓存记录".into()))?;
        return Ok(ClassifiedOutcome::Hit(cached.content_key));
    }

    let (known_md5, missing) = match input.cached.as_ref() {
        Some(cached) => {
            let missing =
                TaskWorkMask::for_base(false, decision.missing_parts()).ok_or_else(|| {
                    BaseTaskProducerError::InvalidInput(format!(
                        "路径 {} 的基础缺失掩码无效",
                        scanned.normalized_path
                    ))
                })?;
            (Some(cached.content_key.md5()), missing)
        }
        None => (
            None,
            TaskWorkMask::for_base(true, 0).expect("needs_md5 基础任务掩码固定有效"),
        ),
    };
    Ok(ClassifiedOutcome::Work { known_md5, missing })
}

/// 检查重复规范路径的文件大小、lane 和分类结果是否完全一致。
fn validate_duplicate(
    path: &NormalizedPath,
    previous: &SeenPathState,
    current: &SeenPathState,
) -> Result<(), BaseTaskProducerError> {
    if previous.file_size != current.file_size {
        return Err(BaseTaskProducerError::InvalidInput(format!(
            "重复路径 {} 的文件大小冲突",
            path
        )));
    }
    if previous.lane != current.lane {
        return Err(BaseTaskProducerError::InvalidInput(format!(
            "重复路径 {} 的物理盘 lane 冲突",
            path
        )));
    }
    if previous.outcome != current.outcome {
        let message = match (previous.outcome, current.outcome) {
            (ClassifiedOutcome::Hit(left), ClassifiedOutcome::Hit(right)) if left != right => {
                "ContentKey 冲突"
            }
            _ => "缓存分类冲突",
        };
        return Err(BaseTaskProducerError::InvalidInput(format!(
            "重复路径 {} 的{message}",
            path
        )));
    }
    Ok(())
}

/// 生成与任务文件集合一致的 lane 文件名排序键，并提前拒绝畸形 lane。
fn lane_file_key(lane: &TaskDiskLane) -> Result<String, BaseTaskProducerError> {
    let numbers = lane.physical_disk_numbers.as_slice();
    if numbers.is_empty()
        || numbers != lane.physical_disk_id.disk_numbers()
        || numbers.windows(2).any(|pair| pair[0] >= pair[1])
    {
        return Err(BaseTaskProducerError::InvalidInput(
            "冻结 lane 的物理盘编号必须按升序去重且匹配物理盘身份".into(),
        ));
    }
    let kind = match lane.disk_kind {
        dedup_windows::LocalDiskKind::Hdd => "hdd",
        dedup_windows::LocalDiskKind::Ssd => "ssd",
        dedup_windows::LocalDiskKind::Unknown => "unknown",
    };
    let numbers = numbers
        .iter()
        .map(u32::to_string)
        .collect::<Vec<_>>()
        .join("+");
    Ok(format!("PhysicalDisk{numbers}-{kind}.tasks.tsv"))
}

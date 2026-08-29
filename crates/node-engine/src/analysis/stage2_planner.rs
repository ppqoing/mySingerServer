//! 本地和跨机二筛的瞬态计划分类。
//!
//! 计划器只消费已经批量读取的 SQLite/中心缓存快照，不在分类过程中访问数据库，
//! 也不启动 Worker。调用方必须先冻结活动来源及物理盘 lane，再把同序缓存结果交给
//! [`Stage2TransientPlanner::plan`]；只有返回的 `Compute` 项才允许追加二筛任务文件。

use std::collections::BTreeSet;

use dedup_core::{ContentKey, LocationKey, MediaKind};
use dedup_node_store::{
    BaseCacheRecord, CompleteStage1, CompleteStage2, ContentId, ScannedPath,
    classify_cache_completeness,
};
use dedup_protocol::{BASE_MISSING_PROBE, BASE_MISSING_STAGE1};
use thiserror::Error;
use uuid::Uuid;

use crate::{
    scan::TaskDiskLane,
    task_files::{TaskFileRecord, TaskWorkKind, TaskWorkMask},
};

/// 二筛规划输入中由缓存查询前解析出的当前活动源位置。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Stage2ActiveSource {
    /// 本次候选使用的内容键。
    pub content: ContentKey,
    /// SQLite 中对应的本地内容 ID。
    pub content_id: ContentId,
    /// 当前活动位置键；必须与 `scanned.normalized_path` 相同。
    pub location: LocationKey,
    /// 真实访问路径、规范路径和扫描时文件大小。
    pub scanned: ScannedPath,
    /// 一筛确认的实际媒体类型。
    pub media_kind: MediaKind,
    /// 视频需要参与二筛的槽位；图片必须为空。
    pub frame_slots: Vec<u8>,
    /// 枚举前冻结的物理盘编号、介质类型和读取额度。
    pub lane: TaskDiskLane,
}

impl Stage2ActiveSource {
    /// 从已解析的活动位置创建规划输入；不会重新解析物理盘。
    pub fn new(
        content: ContentKey,
        content_id: ContentId,
        location: LocationKey,
        scanned: ScannedPath,
        media_kind: MediaKind,
        frame_slots: Vec<u8>,
        lane: TaskDiskLane,
    ) -> Self {
        Self {
            content,
            content_id,
            location,
            scanned,
            media_kind,
            frame_slots,
            lane,
        }
    }
}

/// 缓存查询前交给计划器的内容和活动源声明。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Stage2PlanningInput {
    /// 请求的跨边界内容键。
    pub requested_content: ContentKey,
    /// 请求时冻结的活动位置键。
    pub requested_source: LocationKey,
    /// 同一时刻解析出的活动源和物理盘 lane。
    pub active: Stage2ActiveSource,
}

impl Stage2PlanningInput {
    /// 用一个已经确认的活动源生成不带歧义的输入。
    pub fn from_active(active: Stage2ActiveSource) -> Self {
        Self {
            requested_content: active.content,
            requested_source: active.location.clone(),
            active,
        }
    }
}

/// 已在缓存查询前完成校验且只读的二筛来源集合。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FrozenStage2Batch {
    sources: Vec<Stage2ActiveSource>,
}

impl FrozenStage2Batch {
    /// 返回按输入顺序冻结的活动源；缓存结果必须使用同一顺序。
    pub fn sources(&self) -> &[Stage2ActiveSource] {
        &self.sources
    }
}

/// 二筛特征的最小选择集合。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Stage2Selection {
    /// 一张图片的完整联合特征。
    Image,
    /// 视频需要处理的槽位掩码，bit 0 对应槽位 0。
    VideoSlots(u8),
}

impl Stage2Selection {
    /// 返回选择是否为图片二筛。
    pub const fn is_image(self) -> bool {
        matches!(self, Self::Image)
    }

    /// 返回视频槽位掩码；图片返回 0。
    pub const fn video_slots(self) -> u8 {
        match self {
            Self::Image => 0,
            Self::VideoSlots(slots) => slots,
        }
    }
}

/// 只包含真正缺失字段的二筛 Worker 工作项。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Stage2WorkItem {
    /// 供任务文件和 Worker 身份使用的本次运行项 ID。
    item_id: Uuid,
    /// 任务项使用的冻结活动源和物理盘 lane。
    source: Stage2ActiveSource,
    /// 仅包含需要计算的二筛字段。
    selection: Stage2Selection,
    /// 与任务文件格式一致的缺失掩码。
    missing: TaskWorkMask,
}

impl Stage2WorkItem {
    /// 返回本次工作项的 UUID。
    pub const fn item_id(&self) -> Uuid {
        self.item_id
    }

    /// 返回冻结来源，调用方不得替换其 lane。
    pub const fn source(&self) -> &Stage2ActiveSource {
        &self.source
    }

    /// 返回图片或视频槽位选择。
    pub const fn selection(&self) -> Stage2Selection {
        self.selection
    }

    /// 返回任务文件应写入的缺失掩码。
    pub const fn missing(&self) -> TaskWorkMask {
        self.missing
    }

    /// 把规划项转换为固定 TSV 行；不会创建或写入文件。
    pub fn task_record(&self) -> TaskFileRecord {
        TaskFileRecord {
            item_id: self.item_id,
            work_kind: match self.selection {
                Stage2Selection::Image => TaskWorkKind::ImageStage2,
                Stage2Selection::VideoSlots(_) => TaskWorkKind::VideoStage2,
            },
            scanned: self.source.scanned.clone(),
            known_md5: Some(self.source.content.md5()),
            missing: self.missing,
        }
    }
}

/// 一个来源的二筛动作；计划器只分类，不执行这些动作。
#[derive(Clone, Debug, PartialEq)]
pub enum Stage2PlanAction {
    /// 复用本机已完整或部分存在的字段并重新发布必要 outbox。
    RepublishLocal {
        /// 只重新发布当前本机已经具备的字段。
        selection: Stage2Selection,
    },
    /// 将中心完整或部分结果导入本机，仅覆盖当前缺失字段。
    ImportRemote {
        /// 中心返回的联合特征；调用方按 `selection` 取值。
        features: CompleteStage2,
        /// 本次实际导入的图片或视频槽位。
        selection: Stage2Selection,
    },
    /// 由 Worker 计算本机和中心均缺失的字段。
    Compute(Stage2WorkItem),
    /// 基础探测或一筛不完整，保持分析候选 Incomplete。
    IncompleteBase,
}

/// 一个候选内容及其按顺序执行的瞬态动作。
#[derive(Clone, Debug, PartialEq)]
pub struct Stage2PlanItem {
    /// 已冻结的活动源和物理盘 lane。
    source: Stage2ActiveSource,
    /// 复用、导入和计算动作；顺序固定为本地复用、远端导入、Worker 计算。
    actions: Vec<Stage2PlanAction>,
}

impl Stage2PlanItem {
    /// 返回冻结来源。
    pub const fn source(&self) -> &Stage2ActiveSource {
        &self.source
    }

    /// 返回本项全部动作的只读视图。
    pub fn actions(&self) -> &[Stage2PlanAction] {
        &self.actions
    }
}

/// 一次批量二筛缓存分类的结果。
#[derive(Clone, Debug, PartialEq)]
pub struct Stage2TransientPlan {
    items: Vec<Stage2PlanItem>,
}

impl Stage2TransientPlan {
    /// 返回按冻结输入顺序排列的计划项。
    pub fn items(&self) -> &[Stage2PlanItem] {
        &self.items
    }

    /// 返回所有需要追加 TSV/启动 Worker 的工作项；完整命中不会出现在这里。
    pub fn worker_items(&self) -> Vec<&Stage2WorkItem> {
        self.items
            .iter()
            .flat_map(|item| item.actions.iter())
            .filter_map(|action| match action {
                Stage2PlanAction::Compute(work) => Some(work),
                _ => None,
            })
            .collect()
    }
}

/// 二筛瞬态计划输入或缓存批次的错误。
#[derive(Clone, Debug, Error, PartialEq)]
pub enum Stage2PlanError {
    /// 计划批次不能为空。
    #[error("二筛计划不能为空")]
    EmptyBatch,
    /// 同一内容在一个批次中重复出现。
    #[error("二筛批次包含重复内容: {content:?}")]
    DuplicateContent {
        /// 重复内容键。
        content: ContentKey,
    },
    /// 请求来源不是当前活动位置。
    #[error("二筛来源位置不是当前活动位置")]
    SourceIsNotActive,
    /// 来源快照的路径、大小或媒体槽位不合法。
    #[error("二筛来源快照无效: {0}")]
    InvalidSource(String),
    /// 本地或远端批量结果没有与冻结输入保持一一对应。
    #[error("{kind}二筛缓存批次长度不匹配: 期望 {expected}，实际 {actual}")]
    CacheBatchLength {
        /// 发生长度错误的缓存类型。
        kind: &'static str,
        /// 冻结来源数。
        expected: usize,
        /// 缓存结果数。
        actual: usize,
    },
    /// 本地缓存结果的内容键与冻结来源不一致。
    #[error("本地二筛缓存内容键与冻结来源不一致")]
    CacheContentMismatch,
    /// 本地缓存结果的媒体类型与冻结来源不一致。
    #[error("本地二筛缓存媒体类型与冻结来源不一致")]
    CacheMediaKindMismatch,
    /// 本地缓存结果缺少应有的 content_id。
    #[error("本地二筛缓存缺少内容 ID")]
    CacheContentIdMismatch,
}

/// 无副作用的二筛瞬态计划器。
pub struct Stage2TransientPlanner;

impl Stage2TransientPlanner {
    /// 在任何本地或远端缓存查询前校验并冻结活动来源、内容去重和物理盘 lane。
    pub fn freeze(inputs: &[Stage2PlanningInput]) -> Result<FrozenStage2Batch, Stage2PlanError> {
        if inputs.is_empty() {
            return Err(Stage2PlanError::EmptyBatch);
        }
        let mut seen = BTreeSet::new();
        let mut sources = Vec::with_capacity(inputs.len());
        for input in inputs {
            let active = &input.active;
            if !seen.insert(input.requested_content) {
                return Err(Stage2PlanError::DuplicateContent {
                    content: input.requested_content,
                });
            }
            if input.requested_content != active.content
                || input.requested_source != active.location
            {
                return Err(Stage2PlanError::SourceIsNotActive);
            }
            if active.location.normalized_path() != &active.scanned.normalized_path {
                return Err(Stage2PlanError::SourceIsNotActive);
            }
            if active.scanned.file_size != active.content.file_size() {
                return Err(Stage2PlanError::InvalidSource(
                    "活动源大小与内容键不一致".into(),
                ));
            }
            validate_slots(active)?;
            sources.push(active.clone());
        }
        Ok(FrozenStage2Batch { sources })
    }

    /// 消费与冻结输入同序的批量缓存结果，生成复用、导入和最小 Worker 工作项。
    ///
    /// `local` 和 `remote` 都必须由调用方一次批量查询后传入；本方法没有 `NodeStore`
    /// 或远端连接参数，因此不可能退化为逐项 SQLite 查询。
    pub fn plan(
        batch: &FrozenStage2Batch,
        local: &[Option<BaseCacheRecord>],
        remote: Option<&[Option<CompleteStage2>]>,
    ) -> Result<Stage2TransientPlan, Stage2PlanError> {
        if local.len() != batch.sources.len() {
            return Err(Stage2PlanError::CacheBatchLength {
                kind: "本地",
                expected: batch.sources.len(),
                actual: local.len(),
            });
        }
        if let Some(remote) = remote
            && remote.len() != batch.sources.len()
        {
            return Err(Stage2PlanError::CacheBatchLength {
                kind: "远端",
                expected: batch.sources.len(),
                actual: remote.len(),
            });
        }

        let mut items = Vec::with_capacity(batch.sources.len());
        for (index, source) in batch.sources.iter().enumerate() {
            let actions = plan_source(
                source,
                local[index].as_ref(),
                remote.and_then(|rows| rows[index].as_ref()),
            )?;
            items.push(Stage2PlanItem {
                source: source.clone(),
                actions,
            });
        }
        Ok(Stage2TransientPlan { items })
    }
}

/// 校验视频槽位和图片的空槽位约束，避免生成无法消费的任务行。
fn validate_slots(source: &Stage2ActiveSource) -> Result<(), Stage2PlanError> {
    let mut seen = BTreeSet::new();
    if source.media_kind == MediaKind::Image && !source.frame_slots.is_empty() {
        return Err(Stage2PlanError::InvalidSource(
            "图片二筛不能携带视频槽位".into(),
        ));
    }
    if source.media_kind == MediaKind::Video && source.frame_slots.is_empty() {
        return Err(Stage2PlanError::InvalidSource(
            "视频二筛至少需要一个候选槽位".into(),
        ));
    }
    for slot in &source.frame_slots {
        if *slot > 5 || !seen.insert(*slot) {
            return Err(Stage2PlanError::InvalidSource(
                "视频二筛槽位必须为 0..=5 且不能重复".into(),
            ));
        }
    }
    Ok(())
}

/// 依次分类一个已经对齐的本地/远端缓存结果。
fn plan_source(
    source: &Stage2ActiveSource,
    local: Option<&BaseCacheRecord>,
    remote: Option<&CompleteStage2>,
) -> Result<Vec<Stage2PlanAction>, Stage2PlanError> {
    let Some(local) = local else {
        return Ok(vec![Stage2PlanAction::IncompleteBase]);
    };
    validate_local_identity(source, local)?;
    let completeness = classify_cache_completeness(local, true);
    if completeness.base_missing_parts & (BASE_MISSING_PROBE | BASE_MISSING_STAGE1) != 0 {
        return Ok(vec![Stage2PlanAction::IncompleteBase]);
    }

    let (missing_selection, existing_selection) = match source.media_kind {
        MediaKind::Image => {
            let Some(CompleteStage1::Image(_)) = local.stage1.as_ref() else {
                return Ok(vec![Stage2PlanAction::IncompleteBase]);
            };
            if completeness.image_stage2_missing {
                (Some(Stage2Selection::Image), None)
            } else {
                return Ok(vec![Stage2PlanAction::RepublishLocal {
                    selection: Stage2Selection::Image,
                }]);
            }
        }
        MediaKind::Video => {
            let Some(CompleteStage1::Video(stage1)) = local.stage1.as_ref() else {
                return Ok(vec![Stage2PlanAction::IncompleteBase]);
            };
            let eligible = source
                .frame_slots
                .iter()
                .copied()
                .filter(|slot| stage1[usize::from(*slot)].is_some())
                .collect::<Vec<_>>();
            let missing_mask = completeness.video_stage2_missing_slots;
            let missing = eligible
                .iter()
                .copied()
                .filter(|slot| missing_mask & (1_u8 << slot) != 0)
                .fold(0_u8, |mask, slot| mask | (1_u8 << slot));
            let existing = eligible
                .iter()
                .copied()
                .filter(|slot| missing_mask & (1_u8 << slot) == 0)
                .fold(0_u8, |mask, slot| mask | (1_u8 << slot));
            if eligible.is_empty() {
                return Ok(vec![Stage2PlanAction::IncompleteBase]);
            }
            let missing = (missing != 0).then_some(Stage2Selection::VideoSlots(missing));
            let existing = (existing != 0).then_some(Stage2Selection::VideoSlots(existing));
            (missing, existing)
        }
        MediaKind::Other => return Ok(vec![Stage2PlanAction::IncompleteBase]),
    };

    let Some(mut remaining) = missing_selection else {
        return Ok(existing_selection
            .into_iter()
            .map(|selection| Stage2PlanAction::RepublishLocal { selection })
            .collect());
    };
    let mut actions = Vec::new();
    if let Some(existing_selection) = existing_selection {
        actions.push(Stage2PlanAction::RepublishLocal {
            selection: existing_selection,
        });
    }

    if let Some(remote) = remote {
        if let Some((remote_selection, remote_features)) =
            remote_selection(source, remaining, remote)
        {
            actions.push(Stage2PlanAction::ImportRemote {
                features: remote_features.clone(),
                selection: remote_selection,
            });
            remaining = subtract_selection(remaining, remote_selection);
        }
    }

    if selection_is_missing(remaining) {
        let work = Stage2WorkItem {
            item_id: Uuid::now_v7(),
            source: source.clone(),
            selection: remaining,
            missing: task_mask(remaining),
        };
        actions.push(Stage2PlanAction::Compute(work));
    }
    Ok(actions)
}

/// 确认本地批量缓存结果仍属于冻结内容和媒体类型。
fn validate_local_identity(
    source: &Stage2ActiveSource,
    local: &BaseCacheRecord,
) -> Result<(), Stage2PlanError> {
    if local.content_key != source.content {
        return Err(Stage2PlanError::CacheContentMismatch);
    }
    if local.media_kind != source.media_kind {
        return Err(Stage2PlanError::CacheMediaKindMismatch);
    }
    if local.content_id != Some(source.content_id) {
        return Err(Stage2PlanError::CacheContentIdMismatch);
    }
    Ok(())
}

/// 从中心结果筛选当前缺失且结构有效的字段。
fn remote_selection<'a>(
    source: &Stage2ActiveSource,
    remaining: Stage2Selection,
    remote: &'a CompleteStage2,
) -> Option<(Stage2Selection, &'a CompleteStage2)> {
    match (source.media_kind, remaining, remote) {
        (MediaKind::Image, Stage2Selection::Image, CompleteStage2::Image(feature))
            if feature.sobel.iter().all(|value| value.is_finite()) =>
        {
            Some((Stage2Selection::Image, remote))
        }
        (
            MediaKind::Video,
            Stage2Selection::VideoSlots(requested_slots),
            CompleteStage2::Video(features),
        ) => {
            let available = (0..6)
                .filter(|slot| {
                    features[*slot]
                        .as_ref()
                        .is_some_and(|feature| feature.sobel.iter().all(|value| value.is_finite()))
                })
                .fold(0_u8, |mask, slot| mask | (1_u8 << slot));
            let selected = requested_slots & available;
            (selected != 0).then_some((Stage2Selection::VideoSlots(selected), remote))
        }
        _ => None,
    }
}

/// 从待处理选择中扣除已经导入的选择。
fn subtract_selection(left: Stage2Selection, imported: Stage2Selection) -> Stage2Selection {
    match (left, imported) {
        (Stage2Selection::Image, Stage2Selection::Image) => Stage2Selection::VideoSlots(0),
        (Stage2Selection::VideoSlots(left), Stage2Selection::VideoSlots(imported)) => {
            Stage2Selection::VideoSlots(left & !imported)
        }
        (left, _) => left,
    }
}

/// 判断选择中是否仍有待计算字段。
fn selection_is_missing(selection: Stage2Selection) -> bool {
    match selection {
        Stage2Selection::Image => true,
        Stage2Selection::VideoSlots(slots) => slots != 0,
    }
}

/// 生成与任务文件相同的 Stage2 缺失掩码。
fn task_mask(selection: Stage2Selection) -> TaskWorkMask {
    match selection {
        Stage2Selection::Image => TaskWorkMask::for_image_stage2(),
        Stage2Selection::VideoSlots(slots) => {
            TaskWorkMask::for_video_stage2(slots).expect("计划器只会为非零的合法视频槽位创建掩码")
        }
    }
}

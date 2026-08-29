//! 中心 PostgreSQL 结果的有限窗口、成员视图和按需预览。

use std::collections::BTreeMap;

use dedup_core::{AnalysisRunId, ContentKey, LocationKey, MachineId};
use thiserror::Error;

use crate::{
    central::{CentralError, CentralGroupKind, CentralStore},
    node_session::{NodeSession, SessionError},
    review::ReviewDecision,
};

/// 预览协议每块固定最多 1 MiB。
pub const PREVIEW_CHUNK_BYTES: u32 = 1_048_576;

/// 当前结果只来自一个 PostgreSQL 中心分析运行。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ResultScope {
    /// 跨机器中心分析运行。
    Central {
        /// PostgreSQL 分析运行。
        run_id: AnalysisRunId,
    },
}

impl ResultScope {
    /// 返回中心分析运行 ID。
    pub const fn run_id(self) -> AnalysisRunId {
        match self {
            Self::Central { run_id } => run_id,
        }
    }
}

/// 管理端统一展示的三类重复结果。
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum GroupKind {
    /// MD5 与文件大小完全相同。
    Exact,
    /// 图片通过 PDQ 以及 pHash/Sobel 联合二筛。
    SimilarImage,
    /// 视频六个槽位平均结果通过两层筛选。
    SimilarVideo,
}

/// 结果窗口一次最多向 Slint 暴露的行数，避免用户输入造成大批量物化。
pub const MAX_RESULT_WINDOW_ROWS: u32 = 200;

/// 整个中心结果缓存最多保留的游标检查点数。
pub const MAX_RESULT_WINDOW_CHECKPOINTS: usize = 8;

/// 结果窗口请求；游标只在中心缓存内部使用，UI 只提交稳定运行身份与行范围。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResultWindowRequest {
    /// 只允许读取已完成中心分析的 UUID 文本。
    pub analysis_run_id: String,
    /// 结果组类别。
    pub kind: GroupKind,
    /// 期望窗口第一行的零基索引。
    pub start_index: u64,
    /// 期望窗口行数，会在 Core 边界裁剪到固定上限。
    pub visible_count: u32,
}

impl ResultWindowRequest {
    /// 创建结果窗口请求，并把可见行数限制在安全范围内。
    pub fn new(
        analysis_run_id: impl Into<String>,
        kind: GroupKind,
        start_index: u64,
        visible_count: u32,
    ) -> Self {
        Self {
            analysis_run_id: analysis_run_id.into(),
            kind,
            start_index,
            visible_count: visible_count.clamp(1, MAX_RESULT_WINDOW_ROWS),
        }
    }

    /// 返回请求实际使用的可见行数，兼容反序列化或旧调用方的零值。
    pub const fn normalized_visible_count(&self) -> u32 {
        if self.visible_count == 0 {
            1
        } else if self.visible_count > MAX_RESULT_WINDOW_ROWS {
            MAX_RESULT_WINDOW_ROWS
        } else {
            self.visible_count
        }
    }
}

/// 结果窗口的不可变 UI 状态；每次响应整体替换，不累加历史页。
#[derive(Clone, Debug, PartialEq)]
pub struct ResultWindowState<T> {
    /// 当前窗口第一行的零基索引。
    pub start_index: u64,
    /// 中心当前类别的完整结果数量。
    pub total_rows: u64,
    /// 当前窗口行，长度不超过请求可见行数。
    pub items: Vec<T>,
    /// 正在等待中心响应时为真。
    pub loading: bool,
    /// 中心断线或运行身份改变后保留的只读旧窗口。
    pub stale: bool,
}

impl<T> ResultWindowState<T> {
    /// 创建尚未发起请求的空状态。
    pub const fn empty() -> Self {
        Self {
            start_index: 0,
            total_rows: 0,
            items: Vec::new(),
            loading: false,
            stale: false,
        }
    }

    /// 复制当前行并只切换加载标记，保留 stale 窗口内容。
    pub fn with_loading(&self, loading: bool) -> Self
    where
        T: Clone,
    {
        Self {
            start_index: self.start_index,
            total_rows: self.total_rows,
            items: self.items.clone(),
            loading,
            stale: self.stale,
        }
    }

    /// 复制当前行并标记为 stale；stale 结果仍可滚动和预览。
    pub fn as_stale(&self) -> Self
    where
        T: Clone,
    {
        Self {
            start_index: self.start_index,
            total_rows: self.total_rows,
            items: self.items.clone(),
            loading: false,
            stale: true,
        }
    }

    /// 以新的窗口整体替换旧窗口，确保向前、向后滚动都不会拼接历史行。
    pub fn replace(&mut self, next: Self) {
        *self = next;
    }
}

/// 中心分页的少量检查点；缓存只保留游标和匹配行位置，不持有全量结果。
#[derive(Clone, Debug, Eq, PartialEq)]
struct WindowCheckpoint {
    /// 从该游标之后开始读取时，类别内已消费的行数。
    matched_index: u64,
    /// 中心不透明游标；None 表示从头开始。
    cursor: Option<String>,
}

/// PostgreSQL 结果的有限滑动窗口缓存，组与成员分别按运行身份保存检查点。
#[derive(Clone, Debug)]
pub struct CentralResultWindowCache {
    group_checkpoints: BTreeMap<(AnalysisRunId, GroupKind), Vec<WindowCheckpoint>>,
    member_checkpoints: BTreeMap<(AnalysisRunId, String), Vec<WindowCheckpoint>>,
    checkpoint_limit: usize,
}

impl Default for CentralResultWindowCache {
    fn default() -> Self {
        Self::new()
    }
}

impl CentralResultWindowCache {
    /// 创建只保留少量检查点的中心窗口缓存。
    pub fn new() -> Self {
        Self {
            group_checkpoints: BTreeMap::new(),
            member_checkpoints: BTreeMap::new(),
            checkpoint_limit: MAX_RESULT_WINDOW_CHECKPOINTS,
        }
    }

    /// 返回当前缓存持有的检查点总数；窗口行本身不在此处累积。
    pub fn checkpoint_count(&self) -> usize {
        self.group_checkpoints
            .values()
            .map(Vec::len)
            .chain(self.member_checkpoints.values().map(Vec::len))
            .sum()
    }

    /// 从中心按稳定游标读取一个类别窗口，并在内存中整体替换旧行。
    pub async fn load_groups(
        &mut self,
        central: &CentralStore,
        run_id: AnalysisRunId,
        kind: GroupKind,
        start_index: u64,
        visible_count: u32,
    ) -> Result<ResultWindowState<GroupView>, CentralError> {
        let visible_count = visible_count.clamp(1, MAX_RESULT_WINDOW_ROWS) as usize;
        let key = (run_id, kind);
        let checkpoint = self.nearest_group_checkpoint(key, start_index);
        let mut cursor = checkpoint.cursor;
        let mut matched_index = checkpoint.matched_index;
        let mut items = Vec::with_capacity(visible_count);
        let mut total_rows = matched_index;
        loop {
            let page = central
                .page_groups(run_id, cursor.as_deref(), CENTRAL_WINDOW_PAGE_SIZE)
                .await?;
            let next_cursor = page.next_cursor.clone();
            if next_cursor.is_some() && next_cursor == cursor {
                return Err(CentralError::InvalidCursor);
            }
            for group in page.items {
                if central_kind(group.kind) != kind {
                    continue;
                }
                if matched_index >= start_index
                    && matched_index < start_index.saturating_add(visible_count as u64)
                {
                    items.push(group_view(group));
                }
                matched_index = matched_index.saturating_add(1);
                total_rows = matched_index;
            }
            if let Some(next) = next_cursor.clone() {
                self.remember_group_checkpoint(
                    key,
                    WindowCheckpoint {
                        matched_index,
                        cursor: Some(next.clone()),
                    },
                );
                cursor = Some(next);
            } else {
                break;
            }
        }
        Ok(ResultWindowState {
            start_index,
            total_rows,
            items,
            loading: false,
            stale: false,
        })
    }

    /// 从中心按位置游标读取一个成员窗口，并只返回当前有限行。
    pub async fn load_members<F>(
        &mut self,
        central: &CentralStore,
        run_id: AnalysisRunId,
        group_id: &str,
        start_index: u64,
        visible_count: u32,
        online: F,
    ) -> Result<ResultWindowState<MemberView>, CentralError>
    where
        F: FnMut(&MachineId) -> bool + Copy,
    {
        let visible_count = visible_count.clamp(1, MAX_RESULT_WINDOW_ROWS) as usize;
        let key = (run_id, group_id.to_owned());
        let checkpoint = self.nearest_member_checkpoint(key.clone(), start_index);
        let mut cursor = checkpoint.cursor;
        let mut matched_index = checkpoint.matched_index;
        let mut items = Vec::with_capacity(visible_count);
        let mut total_rows = matched_index;
        loop {
            let page = central
                .page_group_members(
                    run_id,
                    group_id,
                    cursor.as_deref(),
                    CENTRAL_WINDOW_PAGE_SIZE,
                )
                .await?;
            let next_cursor = page.next_cursor.clone();
            if next_cursor.is_some() && next_cursor == cursor {
                return Err(CentralError::InvalidCursor);
            }
            for member in page.items {
                if matched_index >= start_index
                    && matched_index < start_index.saturating_add(visible_count as u64)
                {
                    items.push(member_view(member, online));
                }
                matched_index = matched_index.saturating_add(1);
                total_rows = matched_index;
            }
            if let Some(next) = next_cursor.clone() {
                self.remember_member_checkpoint(
                    key.clone(),
                    WindowCheckpoint {
                        matched_index,
                        cursor: Some(next.clone()),
                    },
                );
                cursor = Some(next);
            } else {
                break;
            }
        }
        Ok(ResultWindowState {
            start_index,
            total_rows,
            items,
            loading: false,
            stale: false,
        })
    }

    /// 读取完整中心成员集，供仍需兼容的删除确认边界使用；不暴露给 UI。
    pub async fn load_all_members<F>(
        &mut self,
        central: &CentralStore,
        run_id: AnalysisRunId,
        group_id: &str,
        online: F,
    ) -> Result<Vec<MemberView>, CentralError>
    where
        F: FnMut(&MachineId) -> bool + Copy,
    {
        let mut cursor = None;
        let mut members = Vec::new();
        loop {
            let page = central
                .page_group_members(
                    run_id,
                    group_id,
                    cursor.as_deref(),
                    CENTRAL_WINDOW_PAGE_SIZE,
                )
                .await?;
            if page.next_cursor.is_some() && page.next_cursor == cursor {
                return Err(CentralError::InvalidCursor);
            }
            cursor = page.next_cursor.clone();
            members.extend(
                page.items
                    .into_iter()
                    .map(|member| member_view(member, online)),
            );
            if cursor.is_none() {
                return Ok(members);
            }
        }
    }

    /// 使断线或删除后的下一次读取从稳定起点重新建立检查点。
    pub fn clear_run(&mut self, run_id: AnalysisRunId) {
        self.group_checkpoints.retain(|(id, _), _| *id != run_id);
        self.member_checkpoints.retain(|(id, _), _| *id != run_id);
    }

    fn nearest_group_checkpoint(
        &self,
        key: (AnalysisRunId, GroupKind),
        start_index: u64,
    ) -> WindowCheckpoint {
        self.group_checkpoints
            .get(&key)
            .and_then(|checkpoints| {
                checkpoints
                    .iter()
                    .filter(|checkpoint| checkpoint.matched_index <= start_index)
                    .max_by_key(|checkpoint| checkpoint.matched_index)
                    .cloned()
            })
            .unwrap_or(WindowCheckpoint {
                matched_index: 0,
                cursor: None,
            })
    }

    fn nearest_member_checkpoint(
        &self,
        key: (AnalysisRunId, String),
        start_index: u64,
    ) -> WindowCheckpoint {
        self.member_checkpoints
            .get(&key)
            .and_then(|checkpoints| {
                checkpoints
                    .iter()
                    .filter(|checkpoint| checkpoint.matched_index <= start_index)
                    .max_by_key(|checkpoint| checkpoint.matched_index)
                    .cloned()
            })
            .unwrap_or(WindowCheckpoint {
                matched_index: 0,
                cursor: None,
            })
    }

    fn remember_group_checkpoint(
        &mut self,
        key: (AnalysisRunId, GroupKind),
        checkpoint: WindowCheckpoint,
    ) {
        remember_checkpoint(
            &mut self.group_checkpoints,
            key,
            checkpoint,
            self.checkpoint_limit,
        );
        self.trim_checkpoints();
    }

    fn remember_member_checkpoint(
        &mut self,
        key: (AnalysisRunId, String),
        checkpoint: WindowCheckpoint,
    ) {
        remember_checkpoint(
            &mut self.member_checkpoints,
            key,
            checkpoint,
            self.checkpoint_limit,
        );
        self.trim_checkpoints();
    }

    /// 在多个运行或组之间仍保持全局固定上限，优先丢弃排序最早的旧检查点。
    fn trim_checkpoints(&mut self) {
        while self.checkpoint_count() > self.checkpoint_limit {
            if let Some(key) = self.group_checkpoints.keys().next().copied() {
                let remove_key = self
                    .group_checkpoints
                    .get_mut(&key)
                    .map(|entries| {
                        entries.remove(0);
                        entries.is_empty()
                    })
                    .unwrap_or(false);
                if remove_key {
                    self.group_checkpoints.remove(&key);
                }
                continue;
            }
            let Some(key) = self.member_checkpoints.keys().next().cloned() else {
                break;
            };
            let remove_key = self
                .member_checkpoints
                .get_mut(&key)
                .map(|entries| {
                    entries.remove(0);
                    entries.is_empty()
                })
                .unwrap_or(false);
            if remove_key {
                self.member_checkpoints.remove(&key);
            }
        }
    }
}

/// 中心单次查询页的内部上限，窗口算法不会把更大的页暴露给 UI。
const CENTRAL_WINDOW_PAGE_SIZE: usize = 100;

/// 中心页只转换为当前行摘要，避免缓存保留数据库页面。
fn group_view(group: crate::central::CentralGroup) -> GroupView {
    GroupView {
        group_id: group.group_id,
        kind: central_kind(group.kind),
        representative: group.representative,
        member_count: group.member_count,
        reclaimable_bytes: group.reclaimable_bytes,
    }
}

/// 中心成员只转换为当前行模型，并在转换边界计算在线动作门禁。
fn member_view<F>(member: crate::central::CentralGroupMember, mut online: F) -> MemberView
where
    F: FnMut(&MachineId) -> bool,
{
    let is_online = online(member.location.machine_id());
    let mut view = MemberView::new(
        member.location,
        member.content,
        member.representative,
        is_online,
    );
    view.stage1_score = member.stage1_score;
    view.phash_passed_parts = member.phash_passed_parts;
    view.stage2_score = member.stage2_score;
    // 中心 review_marks 只在本阶段作为兼容写入，结果窗口必须从瞬时板面开始。
    view.review = ReviewDecision::Undecided;
    view.dimensions = member.width.zip(member.height);
    view.quality = member.quality;
    view.set_availability(member.active, is_online);
    view
}

/// 把检查点插入有序列表并限制数量，避免中心滚动积累无限游标。
fn remember_checkpoint<K: Ord>(
    checkpoints: &mut BTreeMap<K, Vec<WindowCheckpoint>>,
    key: K,
    checkpoint: WindowCheckpoint,
    limit: usize,
) {
    let entries = checkpoints.entry(key).or_default();
    entries.retain(|item| item.matched_index != checkpoint.matched_index);
    entries.push(checkpoint);
    entries.sort_by_key(|item| item.matched_index);
    if entries.len() > limit {
        let remove = entries.len() - limit;
        entries.drain(..remove);
    }
}

/// 中心重复组列表中的统一摘要。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GroupView {
    /// 持久组 ID。
    pub group_id: String,
    /// 精确、相似图片或相似视频。
    pub kind: GroupKind,
    /// 当前代表内容的稳定外部键。
    pub representative: ContentKey,
    /// 当前活动位置数。
    pub member_count: u32,
    /// 除代表位置外预计可以释放的字节数。
    pub reclaimable_bytes: u64,
}

/// 离线状态直接派生出的成员动作能力。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct MemberActions {
    /// 是否可以从节点按需读取预览。
    pub preview: bool,
    /// 是否可以请求节点打开文件位置。
    pub open: bool,
    /// 是否可以把成员纳入实际删除计划。
    pub delete: bool,
}

/// 中心组成员的展示和交互模型。
#[derive(Clone, Debug, PartialEq)]
pub struct MemberView {
    /// 机器 ID 与规范路径。
    pub location: LocationKey,
    /// UI 优先展示的路径文本。
    pub display_path: String,
    /// MD5 与文件大小。
    pub content: ContentKey,
    /// 是否为当前代表位置。
    pub representative: bool,
    /// 与代表直接比较的一筛分数。
    pub stage1_score: f64,
    /// 图片分块通过数；精确结果为 `None`。
    pub phash_passed_parts: Option<u8>,
    /// Sobel 或视频平均二筛分数。
    pub stage2_score: Option<f64>,
    /// 当前 Desktop 进程内的复核标记；中心持久字段不会恢复到新会话。
    pub review: ReviewDecision,
    /// 位置是否仍活动。
    pub active: bool,
    /// 当前是否存在对应在线 NodeSession。
    pub online: bool,
    /// 图片或视频像素尺寸；其他文件为 `None`。
    pub dimensions: Option<(u32, u32)>,
    /// 图片 PDQ Quality；视频和其他文件为 `None`。
    pub quality: Option<u8>,
    /// 根据 active/online 一次派生的动作能力。
    pub actions: MemberActions,
}

impl MemberView {
    /// 从已验证外部键创建成员，并一次计算离线动作门禁。
    pub fn new(
        location: LocationKey,
        content: ContentKey,
        representative: bool,
        online: bool,
    ) -> Self {
        let display_path = location.normalized_path().as_str().to_owned();
        Self {
            location,
            display_path,
            content,
            representative,
            stage1_score: 1.0,
            phash_passed_parts: None,
            stage2_score: None,
            review: ReviewDecision::Undecided,
            active: true,
            online,
            dimensions: None,
            quality: None,
            actions: actions(true, online),
        }
    }

    /// 附加媒体元数据，供分辨率与 Quality 快捷复核使用。
    pub const fn with_metadata(
        mut self,
        dimensions: Option<(u32, u32)>,
        quality: Option<u8>,
    ) -> Self {
        self.dimensions = dimensions;
        self.quality = quality;
        self
    }

    /// 覆盖展示路径但不改变协议身份键。
    pub fn with_display_path(mut self, path: impl Into<String>) -> Self {
        self.display_path = path.into();
        self
    }

    /// 更新活动/在线状态并重新计算三个动作门禁。
    pub fn set_availability(&mut self, active: bool, online: bool) {
        self.active = active;
        self.online = online;
        self.actions = actions(active, online);
    }
}

/// 图片原文件或视频联系表在内存中的完整载荷。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PreviewData {
    /// 节点使用的 `original` 或 `contact_sheet` 类别。
    pub file_kind: &'static str,
    /// 仅保存在管理端内存中，不落盘。
    pub bytes: Vec<u8>,
}

/// 按需预览读取的会话或分块连续性错误。
#[derive(Debug, Error)]
pub enum PreviewError {
    /// 节点会话或协议失败。
    #[error(transparent)]
    Session(#[from] SessionError),
    /// 节点返回的 offset 未与请求连续。
    #[error("预览分块偏移不连续")]
    OffsetMismatch,
    /// 非末块为空会导致分页不前进。
    #[error("预览分块没有前进")]
    EmptyChunk,
}

/// 只在用户选择在线成员时把原图或视频 JPG 联系表读入内存。
pub async fn load_preview(
    session: &NodeSession,
    location: &LocationKey,
    kind: GroupKind,
) -> Result<PreviewData, PreviewError> {
    let file_kind = match kind {
        GroupKind::SimilarVideo => "contact_sheet",
        GroupKind::SimilarImage | GroupKind::Exact => "original",
    };
    let mut bytes = Vec::new();
    loop {
        let offset = bytes.len() as u64;
        let chunk = session
            .read_file_chunk(location, file_kind, offset, PREVIEW_CHUNK_BYTES)
            .await?;
        if chunk.offset != offset {
            return Err(PreviewError::OffsetMismatch);
        }
        if chunk.data.is_empty() && !chunk.eof {
            return Err(PreviewError::EmptyChunk);
        }
        bytes.extend_from_slice(&chunk.data);
        if chunk.eof {
            return Ok(PreviewData { file_kind, bytes });
        }
    }
}

const fn actions(active: bool, online: bool) -> MemberActions {
    let enabled = active && online;
    MemberActions {
        preview: enabled,
        open: enabled,
        delete: enabled,
    }
}

const fn central_kind(value: CentralGroupKind) -> GroupKind {
    match value {
        CentralGroupKind::Exact => GroupKind::Exact,
        CentralGroupKind::Image => GroupKind::SimilarImage,
        CentralGroupKind::Video => GroupKind::SimilarVideo,
    }
}

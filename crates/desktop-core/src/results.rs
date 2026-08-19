//! 节点 SQLite 与中心 PostgreSQL 结果到同一有限窗口视图模型的转换。

use dedup_core::{AnalysisRunId, ContentKey, LocationKey};
use dedup_protocol::{ProtocolError, proto};
use thiserror::Error;

use crate::{
    central::{CentralGroupKind, CentralGroupMemberPage, CentralGroupPage, CentralReviewDecision},
    node_session::{NodeSession, SessionError},
    review::ReviewDecision,
};

/// 预览协议每块固定最多 1 MiB。
pub const PREVIEW_CHUNK_BYTES: u32 = 1_048_576;

/// 当前结果来自一个节点 SQLite，或来自 PostgreSQL 中心运行。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ResultScope {
    /// 单节点本地分析；索引定位唯一 NodeSession。
    Local {
        /// 手工节点列表索引。
        node_index: usize,
        /// 节点 SQLite 分析运行。
        run_id: AnalysisRunId,
    },
    /// 跨机器中心分析运行。
    Central {
        /// PostgreSQL 分析运行。
        run_id: AnalysisRunId,
    },
}

impl ResultScope {
    /// 返回本地或中心共同使用的分析运行 ID。
    pub const fn run_id(self) -> AnalysisRunId {
        match self {
            Self::Local { run_id, .. } | Self::Central { run_id } => run_id,
        }
    }
}

/// 管理端统一展示的三类重复结果。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GroupKind {
    /// MD5 与文件大小完全相同。
    Exact,
    /// 图片通过 PDQ 以及 pHash/Sobel 联合二筛。
    SimilarImage,
    /// 视频六个槽位平均结果通过两层筛选。
    SimilarVideo,
}

/// 本地或中心重复组列表中的统一摘要。
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

/// 一页使用稳定不透明游标的统一重复组。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GroupPage {
    /// 当前页结果。
    pub items: Vec<GroupView>,
    /// `None` 表示已经到末页。
    pub next_cursor: Option<String>,
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

/// 本地与中心组成员共用的展示和交互模型。
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
    /// 已从 SQLite 或 PostgreSQL 恢复的复核标记。
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

/// 一页组成员及稳定下一页游标。
#[derive(Clone, Debug, PartialEq)]
pub struct MemberPage {
    /// 当前页成员。
    pub items: Vec<MemberView>,
    /// `None` 表示已经到末页。
    pub next_cursor: Option<String>,
}

/// 只保留最后 `capacity` 行的分页窗口，避免滚动浏览时无限物化结果。
#[derive(Clone, Debug)]
pub struct PagedWindow<T> {
    capacity: usize,
    items: Vec<T>,
    next_cursor: Option<String>,
}

impl<T> PagedWindow<T> {
    /// 创建固定容量窗口；零容量会按一行处理。
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity: capacity.max(1),
            items: Vec::new(),
            next_cursor: None,
        }
    }

    /// 返回当前有限窗口。
    pub fn items(&self) -> &[T] {
        &self.items
    }

    /// 返回服务端提供的不透明下一页游标。
    pub fn next_cursor(&self) -> Option<&str> {
        self.next_cursor.as_deref()
    }
}

impl PagedWindow<GroupView> {
    /// 追加一页并从头丢弃超出容量的旧行。
    pub fn append(&mut self, page: GroupPage) {
        self.items.extend(page.items);
        if self.items.len() > self.capacity {
            self.items.drain(..self.items.len() - self.capacity);
        }
        self.next_cursor = page.next_cursor;
    }
}

/// 节点结果字段缺失或枚举无效。
#[derive(Debug, Error)]
pub enum ResultModelError {
    /// Protobuf 外部键无效。
    #[error(transparent)]
    Protocol(#[from] ProtocolError),
    /// 必需字段缺失。
    #[error("结果字段缺失: {0}")]
    Missing(&'static str),
    /// 组或复核枚举值不属于 V2 定义。
    #[error("结果枚举无效: {0}")]
    InvalidEnum(&'static str),
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

/// 把节点 `ListGroups` 响应转换为统一页，不解释或重写节点游标。
pub fn group_page_from_node(page: proto::ListGroups) -> Result<GroupPage, ResultModelError> {
    let items = page
        .groups
        .into_iter()
        .map(|group| {
            Ok(GroupView {
                group_id: group.group_id,
                kind: node_kind(group.kind)?,
                representative: group
                    .representative
                    .ok_or(ResultModelError::Missing("DuplicateGroup.representative"))?
                    .try_into()?,
                member_count: group.member_count,
                reclaimable_bytes: group.reclaimable_bytes,
            })
        })
        .collect::<Result<Vec<_>, ResultModelError>>()?;
    Ok(GroupPage {
        items,
        next_cursor: non_empty(page.next_cursor),
    })
}

/// 把 PostgreSQL 结果页转换为与节点完全相同的视图类型。
pub fn group_page_from_central(page: CentralGroupPage) -> GroupPage {
    GroupPage {
        items: page
            .items
            .into_iter()
            .map(|group| GroupView {
                group_id: group.group_id,
                kind: central_kind(group.kind),
                representative: group.representative,
                member_count: group.member_count,
                reclaimable_bytes: group.reclaimable_bytes,
            })
            .collect(),
        next_cursor: page.next_cursor,
    }
}

/// 把节点成员页转换为统一模型；`online` 来自当前唯一 session 状态。
pub fn member_page_from_node(
    page: proto::ListGroupMembers,
    online: bool,
) -> Result<MemberPage, ResultModelError> {
    let items = page
        .members
        .into_iter()
        .map(|member| {
            let location: LocationKey = member
                .location
                .ok_or(ResultModelError::Missing("GroupMember.location"))?
                .try_into()?;
            let content = member
                .content
                .ok_or(ResultModelError::Missing("GroupMember.content"))?
                .try_into()?;
            let review = node_review(member.review)?;
            let mut view = MemberView::new(location, content, member.representative, online);
            view.stage1_score = f64::from(member.stage1_score);
            view.phash_passed_parts =
                (member.phash_passed_parts > 0).then_some(member.phash_passed_parts as u8);
            view.stage2_score =
                (member.stage2_score > 0.0).then_some(f64::from(member.stage2_score));
            view.review = review;
            view.dimensions =
                (member.width > 0 && member.height > 0).then_some((member.width, member.height));
            view.quality = (member.quality > 0).then_some(member.quality as u8);
            view.set_availability(member.active, online);
            Ok(view)
        })
        .collect::<Result<Vec<_>, ResultModelError>>()?;
    Ok(MemberPage {
        items,
        next_cursor: non_empty(page.next_cursor),
    })
}

/// 把中心成员页转换为统一模型，并按机器在线集合计算动作门禁。
pub fn member_page_from_central<F>(page: CentralGroupMemberPage, mut is_online: F) -> MemberPage
where
    F: FnMut(&dedup_core::MachineId) -> bool,
{
    MemberPage {
        items: page
            .items
            .into_iter()
            .map(|member| {
                let online = is_online(member.location.machine_id());
                let mut view = MemberView::new(
                    member.location,
                    member.content,
                    member.representative,
                    online,
                );
                view.stage1_score = member.stage1_score;
                view.phash_passed_parts = member.phash_passed_parts;
                view.stage2_score = member.stage2_score;
                view.review = review_from_central(member.review);
                view.dimensions = member.width.zip(member.height);
                view.quality = member.quality;
                view.set_availability(member.active, online);
                view
            })
            .collect(),
        next_cursor: page.next_cursor,
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

fn non_empty(value: String) -> Option<String> {
    (!value.is_empty()).then_some(value)
}

fn node_kind(value: i32) -> Result<GroupKind, ResultModelError> {
    match proto::GroupKind::try_from(value) {
        Ok(proto::GroupKind::GroupExact) => Ok(GroupKind::Exact),
        Ok(proto::GroupKind::GroupSimilarImage) => Ok(GroupKind::SimilarImage),
        Ok(proto::GroupKind::GroupSimilarVideo) => Ok(GroupKind::SimilarVideo),
        _ => Err(ResultModelError::InvalidEnum("GroupKind")),
    }
}

const fn central_kind(value: CentralGroupKind) -> GroupKind {
    match value {
        CentralGroupKind::Exact => GroupKind::Exact,
        CentralGroupKind::Image => GroupKind::SimilarImage,
        CentralGroupKind::Video => GroupKind::SimilarVideo,
    }
}

fn node_review(value: i32) -> Result<ReviewDecision, ResultModelError> {
    match proto::ReviewDecision::try_from(value) {
        Ok(proto::ReviewDecision::ReviewUndecided) => Ok(ReviewDecision::Undecided),
        Ok(proto::ReviewDecision::ReviewKeep) => Ok(ReviewDecision::Keep),
        Ok(proto::ReviewDecision::ReviewDelete) => Ok(ReviewDecision::Delete),
        _ => Err(ResultModelError::InvalidEnum("ReviewDecision")),
    }
}

const fn review_from_central(value: CentralReviewDecision) -> ReviewDecision {
    match value {
        CentralReviewDecision::Undecided => ReviewDecision::Undecided,
        CentralReviewDecision::Keep => ReviewDecision::Keep,
        CentralReviewDecision::Delete => ReviewDecision::Delete,
    }
}

//! 本地分析在当前进程内使用的输入、候选、分组和成员运行模型。

use dedup_core::{AnalysisRunId, ContentKey, DisplayPath, LocationKey, MediaKind, Thresholds};
use dedup_node_store::{
    AnalysisInput, CandidateStatus, CandidateWrite, GroupKind, GroupMemberWrite, GroupWrite,
    PairKind,
};

/// 一条成功扫描位置的内容和原始显示路径快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ScanAnalysisInput {
    /// MD5 与文件大小共同组成的内容键。
    pub content: ContentKey,
    /// 物理机器与规范路径组成的位置键。
    pub location: LocationKey,
    /// 保留原始大小写、供界面和文件访问使用的路径。
    pub display_path: DisplayPath,
    /// Worker 实际探测出的媒体类型。
    pub media_kind: MediaKind,
}

/// 当前扫描快照对应的进程内本地分析运行；候选和输入只在本次进程中持有。
#[derive(Clone, Debug, PartialEq)]
pub(crate) struct LocalAnalysisRun {
    /// 本次运行的 UUID v7 标识，仅用于结果头和进程内关联。
    pub(crate) run_id: AnalysisRunId,
    /// 创建运行时确认的文件库版本。
    pub(crate) library_revision: u64,
    /// 创建运行时的 Unix 毫秒时间。
    pub(crate) created_at_ms: u64,
    /// 本次运行冻结的完整筛选阈值。
    pub(crate) thresholds: Thresholds,
    /// 当前扫描成功项按内容和位置排序去重后的输入。
    pub(crate) inputs: Vec<ScanAnalysisInput>,
    /// 当前进程内的一筛候选和后续二筛结果。
    pub(crate) candidates: Vec<AnalysisCandidate>,
    /// 因基础缓存不完整而跳过的唯一内容数。
    pub(crate) skipped_incomplete: usize,
}

/// 本地相似候选的媒体种类。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisPairKind {
    /// 图片两层筛选候选。
    Image,
    /// 视频六槽两层筛选候选。
    Video,
}

/// 本地候选当前所处的两层筛选状态。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisCandidateStatus {
    /// 一筛已通过，等待联合二筛。
    Stage1Passed,
    /// 联合二筛通过。
    Passed,
    /// 联合二筛拒绝。
    Rejected,
    /// 所需二筛数据不完整，不按零分处理。
    Incomplete,
}

/// 一对内容级候选及其一筛、二筛证据。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct AnalysisCandidate {
    /// 图片或视频候选种类。
    pub kind: AnalysisPairKind,
    /// 按内容键排序后的左侧内容。
    pub left: ContentKey,
    /// 按内容键排序后的右侧内容。
    pub right: ContentKey,
    /// 一筛得分。
    pub stage1_score: f64,
    /// 二筛通过的 pHash 分块数。
    pub phash_passed_parts: Option<u8>,
    /// 联合二筛得分；Incomplete 时为空。
    pub stage2_score: Option<f64>,
    /// 当前候选状态。
    pub status: AnalysisCandidateStatus,
}

/// 最终本地分析组的媒体种类。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisGroupKind {
    /// 内容键和文件大小完全相同。
    Exact,
    /// 图片联合筛选通过。
    Image,
    /// 视频六槽联合筛选通过。
    Video,
}

/// 一个最终组中的位置成员及其相对代表的直接证据。
#[derive(Clone, Debug, PartialEq)]
pub struct AnalysisGroupMember {
    /// 物理机器与规范路径位置。
    pub location: LocationKey,
    /// 位置原始显示路径，不能从规范路径重新推导。
    pub display_path: DisplayPath,
    /// 成员内容键。
    pub content: ContentKey,
    /// 是否为组代表位置。
    pub representative: bool,
    /// 与代表直接比较的一筛得分。
    pub stage1_score: f64,
    /// 与代表直接比较时通过的 pHash 分块数。
    pub phash_passed_parts: Option<u8>,
    /// 与代表直接比较的联合二筛得分。
    pub stage2_score: Option<f64>,
}

impl AnalysisGroupMember {
    /// 创建精确组使用的默认满分成员或相似组的基础成员。
    pub fn new(
        location: LocationKey,
        display_path: DisplayPath,
        content: ContentKey,
        representative: bool,
    ) -> Self {
        Self {
            location,
            display_path,
            content,
            representative,
            stage1_score: 1.0,
            phash_passed_parts: None,
            stage2_score: None,
        }
    }
}

/// 一个最终重复组及其按位置展开的成员。
#[derive(Clone, Debug, PartialEq)]
pub struct AnalysisGroup {
    /// UUID v7 字符串形式的组 ID。
    pub group_id: String,
    /// 精确、图片或视频组种类。
    pub kind: AnalysisGroupKind,
    /// 代表成员的内容键。
    pub representative: ContentKey,
    /// 至少两个位置成员，代表位置恰有一个。
    pub members: Vec<AnalysisGroupMember>,
}

/// 将旧 Store 输入包裹为运行输入；旧表没有显示路径时使用规范绝对路径兜底。
pub(crate) fn from_store_input(input: &AnalysisInput, media_kind: MediaKind) -> ScanAnalysisInput {
    let display_path = DisplayPath::new(input.location.normalized_path().as_str())
        .expect("规范绝对路径必须可作为显示路径");
    ScanAnalysisInput {
        content: input.content,
        location: input.location.clone(),
        display_path,
        media_kind,
    }
}

/// 将 Store 候选在算法入口转换为运行候选，保持每个评分字段原值。
pub(crate) fn from_store_candidate(candidate: CandidateWrite) -> AnalysisCandidate {
    AnalysisCandidate {
        kind: candidate.kind.into(),
        left: candidate.left,
        right: candidate.right,
        stage1_score: candidate.stage1_score,
        phash_passed_parts: candidate.phash_passed_parts,
        stage2_score: candidate.stage2_score,
        status: candidate.status.into(),
    }
}

/// 在暂时保留的旧 Store 写边界一次性转换候选集合。
pub(crate) fn to_store_candidates(candidates: &[AnalysisCandidate]) -> Vec<CandidateWrite> {
    candidates
        .iter()
        .copied()
        .map(|candidate| CandidateWrite {
            kind: candidate.kind.into(),
            left: candidate.left,
            right: candidate.right,
            stage1_score: candidate.stage1_score,
            phash_passed_parts: candidate.phash_passed_parts,
            stage2_score: candidate.stage2_score,
            status: candidate.status.into(),
        })
        .collect()
}

/// 在暂时保留的旧 Store 写边界一次性转换分组，显示路径字段只在该兼容边界丢弃。
pub(crate) fn to_store_groups(groups: &[AnalysisGroup]) -> Vec<GroupWrite> {
    groups
        .iter()
        .map(|group| GroupWrite {
            group_id: group.group_id.clone(),
            kind: group.kind.into(),
            representative: group.representative,
            members: group
                .members
                .iter()
                .map(|member| GroupMemberWrite {
                    location: member.location.clone(),
                    content: member.content,
                    representative: member.representative,
                    stage1_score: member.stage1_score,
                    phash_passed_parts: member.phash_passed_parts,
                    stage2_score: member.stage2_score,
                })
                .collect(),
        })
        .collect()
}

impl From<PairKind> for AnalysisPairKind {
    fn from(value: PairKind) -> Self {
        match value {
            PairKind::Image => Self::Image,
            PairKind::Video => Self::Video,
        }
    }
}

impl From<AnalysisPairKind> for PairKind {
    fn from(value: AnalysisPairKind) -> Self {
        match value {
            AnalysisPairKind::Image => Self::Image,
            AnalysisPairKind::Video => Self::Video,
        }
    }
}

impl From<CandidateStatus> for AnalysisCandidateStatus {
    fn from(value: CandidateStatus) -> Self {
        match value {
            CandidateStatus::Stage1Passed => Self::Stage1Passed,
            CandidateStatus::Passed => Self::Passed,
            CandidateStatus::Rejected => Self::Rejected,
            CandidateStatus::Incomplete => Self::Incomplete,
        }
    }
}

impl From<AnalysisCandidateStatus> for CandidateStatus {
    fn from(value: AnalysisCandidateStatus) -> Self {
        match value {
            AnalysisCandidateStatus::Stage1Passed => Self::Stage1Passed,
            AnalysisCandidateStatus::Passed => Self::Passed,
            AnalysisCandidateStatus::Rejected => Self::Rejected,
            AnalysisCandidateStatus::Incomplete => Self::Incomplete,
        }
    }
}

impl From<GroupKind> for AnalysisGroupKind {
    fn from(value: GroupKind) -> Self {
        match value {
            GroupKind::Exact => Self::Exact,
            GroupKind::Image => Self::Image,
            GroupKind::Video => Self::Video,
        }
    }
}

impl From<AnalysisGroupKind> for GroupKind {
    fn from(value: AnalysisGroupKind) -> Self {
        match value {
            AnalysisGroupKind::Exact => Self::Exact,
            AnalysisGroupKind::Image => Self::Image,
            AnalysisGroupKind::Video => Self::Video,
        }
    }
}

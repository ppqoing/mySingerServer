//! SQLite 边界使用的行值对象、完整特征结果和同步分页结构。

use dedup_core::{ContentKey, DisplayPath, MediaKind, NormalizedPath};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};
use dedup_protocol::proto;

/// 单个 SQLite 内部使用的内容自增标识，不通过协议或同步传播。
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct ContentId(i64);

impl ContentId {
    /// 返回仅供节点内部 SQL 参数使用的整数。
    pub const fn as_i64(self) -> i64 {
        self.0
    }

    pub(crate) const fn from_i64(value: i64) -> Self {
        Self(value)
    }
}

/// 文件枚举器交给缓存批量查询的稳定输入。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ScannedPath {
    /// 用于键和比较的 Windows 规范路径。
    pub normalized_path: NormalizedPath,
    /// 用于显示和真实访问的原始路径。
    pub display_path: DisplayPath,
    /// 枚举时取得的文件大小。
    pub file_size: u64,
}

impl ScannedPath {
    /// 创建一个已经由 core 类型验证过路径的扫描行。
    pub const fn new(
        normalized_path: NormalizedPath,
        display_path: DisplayPath,
        file_size: u64,
    ) -> Self {
        Self {
            normalized_path,
            display_path,
            file_size,
        }
    }
}

/// 一个扫描路径的 SQLite 缓存查询结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CacheLookup {
    /// 查询输入在批次中的原始顺序。
    pub scanned: ScannedPath,
    /// 路径和大小均命中时的本地内容 ID。
    pub content_id: Option<ContentId>,
    /// 命中时可直接复用的 MD5 与大小。
    pub content_key: Option<ContentKey>,
}

impl CacheLookup {
    /// 是否满足“机器 ID + 规范路径 + 文件大小”完整跳过条件。
    pub const fn is_reusable(&self) -> bool {
        self.content_key.is_some()
    }

    /// 返回命中时的跨边界内容键。
    pub const fn content_key(&self) -> Option<ContentKey> {
        self.content_key
    }
}

/// 内容及当前位置写入后的复用结果。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ContentRecord {
    /// SQLite 本地内容 ID。
    pub id: ContentId,
    /// MD5 与大小组成的稳定内容键。
    pub key: ContentKey,
    /// 内容行是否在本次调用前已经存在。
    pub reused: bool,
}

/// SQLite 或中心缓存提供的一份基础媒体计算快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BaseCacheRecord {
    /// SQLite 本地内容 ID；中心导入前为 `None`。
    pub content_id: Option<ContentId>,
    /// 跨 SQLite/PostgreSQL 使用的内容键。
    pub content_key: ContentKey,
    /// 实际媒体类型。
    pub media_kind: MediaKind,
    /// 基础探测与该媒体类型必需的一筛已经在单事务内完成。
    pub base_complete: bool,
    /// 已缓存的像素宽度。
    pub width: Option<u32>,
    /// 已缓存的像素高度。
    pub height: Option<u32>,
    /// 视频时长；图片为空。
    pub duration_ms: Option<u64>,
    /// 完整一筛；部分特征不会伪装成完整命中。
    pub stage1: Option<CompleteStage1>,
}

/// 预览和删除边界读取的当前活动文件身份。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ActiveFile {
    /// SQLite 内部内容 ID，只用于继续查询节点本地特征。
    pub content_id: ContentId,
    /// 扫描时确认的 MD5 与大小。
    pub content_key: ContentKey,
    /// 保留原始拼写并用于实际文件访问的绝对路径。
    pub display_path: DisplayPath,
    /// FFmpeg 实际探测后保存的媒体类别。
    pub media_kind: MediaKind,
}

/// 允许保存部分图片一筛字段，以便失败项在数据库中保持可诊断状态。
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ImageStage1Fields {
    /// 原图宽度。
    pub width: Option<u32>,
    /// 原图高度。
    pub height: Option<u32>,
    /// PDQ-256。
    pub pdq: Option<PdqHash>,
    /// PDQ Quality。
    pub quality: Option<u8>,
}

impl From<ImageStage1> for ImageStage1Fields {
    fn from(value: ImageStage1) -> Self {
        Self {
            width: Some(value.width),
            height: Some(value.height),
            pdq: Some(value.pdq),
            quality: Some(value.quality),
        }
    }
}

/// 视频整体元数据写入值。
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct VideoMetadataFields {
    /// 毫秒时长。
    pub duration_ms: Option<u64>,
    /// 视频显示宽度。
    pub width: Option<u32>,
    /// 视频显示高度。
    pub height: Option<u32>,
}

/// 视频单个槽位的一筛结果；失败槽位仍保存槽位和时间。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct VideoFrameStage1Fields {
    /// 固定 `0..=5` 槽位。
    pub slot: u8,
    /// 固定采样时间，单位毫秒。
    pub time_ms: u64,
    /// 该槽位是否成功解码。
    pub decoded: bool,
    /// 成功帧宽度。
    pub width: Option<u32>,
    /// 成功帧高度。
    pub height: Option<u32>,
    /// 成功帧 PDQ。
    pub pdq: Option<PdqHash>,
    /// 成功帧 Quality。
    pub quality: Option<u8>,
}

/// 视频单个槽位的联合二筛结果。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct VideoFrameStage2Fields {
    /// 固定 `0..=5` 槽位。
    pub slot: u8,
    /// 九分块 pHash 与 Sobel 联合结果。
    pub features: ImageStage2,
}

/// 在一个事务中提交的一类特征结果。
#[derive(Clone, Debug, PartialEq)]
pub enum FeatureWrite {
    /// 图片一筛，可包含部分字段。
    ImageStage1(ImageStage1Fields),
    /// 图片完整联合二筛。
    ImageStage2(ImageStage2),
    /// 视频整体元数据。
    VideoMetadata(VideoMetadataFields),
    /// 视频一个槽位的一筛结果。
    VideoFrameStage1(VideoFrameStage1Fields),
    /// 视频一个槽位的联合二筛结果。
    VideoFrameStage2(VideoFrameStage2Fields),
    /// 视频联系表在节点缓存根内的相对路径。
    ContactSheet(String),
}

/// 只有完整字段才会从 Store 返回的一筛结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum CompleteStage1 {
    /// 一张图片的完整一筛。
    Image(ImageStage1),
    /// 六个槽位记录中的成功帧；失败槽位为 `None`。装箱避免放大图片分支。
    Video(Box<[Option<ImageStage1>; 6]>),
}

/// 只有联合字段完整时才会从 Store 返回的二筛结果。
#[derive(Clone, Debug, PartialEq)]
pub enum CompleteStage2 {
    /// 图片联合二筛。Sobel 向量较大，因此结果也装箱。
    Image(Box<ImageStage2>),
    /// 对应成功一筛槽位的联合二筛；解码失败槽位为 `None`。装箱避免大栈枚举。
    Video(Box<[Option<ImageStage2>; 6]>),
}

/// 节点 outbox 的已确认和已裁剪边界。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SyncState {
    /// PostgreSQL 已提交并由节点接受的最高序号。
    pub acked_seq: u64,
    /// 节点已经实际清理的最高序号。
    pub pruned_through_seq: u64,
}

/// 一批按序号递增的 Protobuf 同步变更。
#[derive(Clone, Debug, PartialEq)]
pub struct SyncBatch {
    /// 最多由调用方限制数量的变更。
    pub changes: Vec<proto::SyncChange>,
    /// 拉取时节点已经产生的最高序号。
    pub high_seq: u64,
    /// 拉取时节点的裁剪边界。
    pub pruned_through_seq: u64,
}

impl SyncBatch {
    /// 便于游标推进和测试读取当前批次序号。
    pub fn sequences(&self) -> Vec<u64> {
        self.changes.iter().map(|change| change.seq).collect()
    }
}

/// 快照中一个带稳定主键游标的基础行。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SnapshotRow {
    /// 表内稳定排序键。
    pub key: String,
    /// 与该实体 outbox 相同的版本化二进制载荷。
    pub payload: Vec<u8>,
}

/// SQLite 只读快照的一页。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SnapshotPage {
    /// 当前固定表名。
    pub table_name: String,
    /// 按主键递增的行。
    pub rows: Vec<SnapshotRow>,
    /// 非末页时用于继续的最后主键。
    pub next_cursor: Option<String>,
    /// 当前表是否已经读完。
    pub done: bool,
}

/// 简单版本化字段编码器；中心解码只依赖字段顺序，不携带 SQLite content_id。
pub(crate) struct RowEncoder(Vec<u8>);

impl RowEncoder {
    pub(crate) fn new(version: u8) -> Self {
        Self(vec![version])
    }

    pub(crate) fn bytes(mut self, value: &[u8]) -> Self {
        self.0
            .extend_from_slice(&(value.len() as u32).to_be_bytes());
        self.0.extend_from_slice(value);
        self
    }

    pub(crate) fn text(self, value: &str) -> Self {
        self.bytes(value.as_bytes())
    }

    pub(crate) fn u64(mut self, value: u64) -> Self {
        self.0.extend_from_slice(&value.to_be_bytes());
        self
    }

    pub(crate) fn u32(mut self, value: u32) -> Self {
        self.0.extend_from_slice(&value.to_be_bytes());
        self
    }

    pub(crate) fn u8(mut self, value: u8) -> Self {
        self.0.push(value);
        self
    }

    pub(crate) fn finish(self) -> Vec<u8> {
        self.0
    }

    pub(crate) fn optional_u32(self, value: Option<u32>) -> Self {
        match value {
            Some(value) => self.u8(1).u32(value),
            None => self.u8(0),
        }
    }

    pub(crate) fn optional_u8(self, value: Option<u8>) -> Self {
        match value {
            Some(value) => self.u8(1).u8(value),
            None => self.u8(0),
        }
    }

    pub(crate) fn optional_bytes(self, value: Option<&[u8]>) -> Self {
        match value {
            Some(value) => self.u8(1).bytes(value),
            None => self.u8(0),
        }
    }
}

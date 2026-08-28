//! 按物理磁盘保存瞬态计算任务的固定 TSV 文件，并管理原位状态确认。
//!
//! 本模块只拥有任务文件的句柄、行偏移和有限预读窗口。调用方不能取得文件句柄，
//! 只有在结果已经由上层事务确认后，才能通过身份值把行首从 `P` 改成 `C/F`。

use std::{
    collections::{BTreeMap, BTreeSet, VecDeque},
    fs::{self, File, OpenOptions},
    io::{self, BufWriter, Seek, SeekFrom, Write},
    path::{Path, PathBuf},
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
};

#[cfg(not(windows))]
use std::io::Read;

use dedup_node_store::ScannedPath;
use dedup_windows::LocalDiskKind;
use uuid::Uuid;

use crate::scan::TaskDiskLane;

const TASK_NEEDS_MD5: u64 = 1 << 3;
const TASK_IMAGE_STAGE2: u64 = 1 << 4;
const TASK_VIDEO_STAGE2: u64 = 0b11_1111 << 5;
const TASK_BASE_PARTS: u64 = 0b111;
const TASK_KNOWN_BITS: u64 =
    TASK_BASE_PARTS | TASK_NEEDS_MD5 | TASK_IMAGE_STAGE2 | TASK_VIDEO_STAGE2;

/// TSV 行首的唯一运行状态。
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum TaskLineStatus {
    /// 尚未完成 SQLite ACK 的任务行。
    Pending,
    /// 所需结果已经提交 SQLite 的任务行。
    Completed,
    /// 单文件读取或 Worker 失败的任务行。
    Failed,
}

impl TaskLineStatus {
    const fn as_byte(self) -> u8 {
        match self {
            Self::Pending => b'P',
            Self::Completed => b'C',
            Self::Failed => b'F',
        }
    }

    fn parse(byte: u8) -> io::Result<Self> {
        match byte {
            b'P' => Ok(Self::Pending),
            b'C' => Ok(Self::Completed),
            b'F' => Ok(Self::Failed),
            _ => Err(invalid_data("任务文件状态不是 P/C/F")),
        }
    }
}

/// 一行任务需要进入的计算入口。
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum TaskWorkKind {
    /// 基础 MD5、媒体探测和一筛任务。
    Base,
    /// 图片二筛任务。
    ImageStage2,
    /// 视频指定槽位的二筛任务。
    VideoStage2,
}

impl TaskWorkKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Base => "base",
            Self::ImageStage2 => "image_stage2",
            Self::VideoStage2 => "video_stage2",
        }
    }

    fn parse(value: &str) -> io::Result<Self> {
        match value {
            "base" => Ok(Self::Base),
            "image_stage2" => Ok(Self::ImageStage2),
            "video_stage2" => Ok(Self::VideoStage2),
            _ => Err(invalid_data("任务文件工作类型无效")),
        }
    }
}

/// 固定 64 位任务缺失掩码。
///
/// bits 0..=2 保留基础缺失字段，bit 3 表示需要 MD5，bit 4 表示图片二筛，
/// bits 5..=10 表示视频二筛槽位 0..5。未知位和零值不能写入任务文件。
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct TaskWorkMask(u64);

impl TaskWorkMask {
    /// 从固定掩码解析非空且没有未知位的值。
    pub const fn from_bits(bits: u64) -> Option<Self> {
        if bits == 0 || bits & !TASK_KNOWN_BITS != 0 {
            None
        } else {
            Some(Self(bits))
        }
    }

    /// 构造供错误测试或校验分支使用的空掩码；追加时必定拒绝。
    pub const fn empty() -> Self {
        Self(0)
    }

    /// 返回原始 64 位掩码。
    pub const fn bits(self) -> u64 {
        self.0
    }

    /// 返回是否需要先计算 MD5。
    pub const fn needs_md5(self) -> bool {
        self.0 & TASK_NEEDS_MD5 != 0
    }

    /// 返回 bits 0..=2 中的基础缺失字段。
    pub const fn base_missing_parts(self) -> u32 {
        (self.0 & TASK_BASE_PARTS) as u32
    }

    /// 返回是否缺少图片二筛。
    pub const fn image_stage2_missing(self) -> bool {
        self.0 & TASK_IMAGE_STAGE2 != 0
    }

    /// 返回视频二筛缺少的槽位位图，返回值 bit 0 对应槽位 0。
    pub const fn video_stage2_slots(self) -> u8 {
        ((self.0 & TASK_VIDEO_STAGE2) >> 5) as u8
    }

    /// 构造基础任务的缺失掩码。
    pub const fn for_base(needs_md5: bool, base_missing_parts: u32) -> Option<Self> {
        let bits = (base_missing_parts as u64 & TASK_BASE_PARTS)
            | if needs_md5 { TASK_NEEDS_MD5 } else { 0 };
        Self::from_bits(bits)
    }

    /// 构造图片二筛缺失掩码。
    pub const fn for_image_stage2() -> Self {
        Self(TASK_IMAGE_STAGE2)
    }

    /// 构造视频二筛槽位缺失掩码。
    pub const fn for_video_stage2(slots: u8) -> Option<Self> {
        if slots == 0 || slots & !0b11_1111 != 0 {
            None
        } else {
            Some(Self((slots as u64) << 5))
        }
    }

    fn validate_for(self, work_kind: TaskWorkKind, known_md5: Option<[u8; 16]>) -> io::Result<()> {
        if self.0 == 0 || self.0 & !TASK_KNOWN_BITS != 0 {
            return Err(invalid_input("缺失字段掩码不能为空且不能包含未知位"));
        }
        match work_kind {
            TaskWorkKind::Base => {
                if self.image_stage2_missing() || self.video_stage2_slots() != 0 {
                    return Err(invalid_input("基础任务不能携带二筛缺失位"));
                }
                if known_md5.is_none() && !self.needs_md5() {
                    return Err(invalid_input("未知 MD5 的基础任务必须携带 needs_md5"));
                }
                if known_md5.is_none() && self.base_missing_parts() != 0 {
                    return Err(invalid_input("未知 MD5 的基础任务不能预先声明内容字段缺失"));
                }
                if known_md5.is_some() == self.needs_md5() {
                    return Err(invalid_input("基础任务的 known_md5 与 needs_md5 不匹配"));
                }
            }
            TaskWorkKind::ImageStage2 => {
                if known_md5.is_none()
                    || !self.image_stage2_missing()
                    || self.base_missing_parts() != 0
                    || self.needs_md5()
                    || self.video_stage2_slots() != 0
                {
                    return Err(invalid_input("图片二筛任务的缺失掩码组合无效"));
                }
            }
            TaskWorkKind::VideoStage2 => {
                if known_md5.is_none()
                    || self.video_stage2_slots() == 0
                    || self.base_missing_parts() != 0
                    || self.needs_md5()
                    || self.image_stage2_missing()
                {
                    return Err(invalid_input("视频二筛任务的缺失掩码组合无效"));
                }
            }
        }
        Ok(())
    }
}

/// 一个真正需要计算的 TSV 项。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskFileRecord {
    /// 任务项的规范 UUID v7。
    pub item_id: Uuid,
    /// 本行的计算入口。
    pub work_kind: TaskWorkKind,
    /// 枚举得到的规范路径、显示路径和文件大小。
    pub scanned: ScannedPath,
    /// 已经由缓存确认的 MD5；未知时为空且必须带 needs_md5 位。
    pub known_md5: Option<[u8; 16]>,
    /// 只包含本行真实缺失字段的掩码。
    pub missing: TaskWorkMask,
}

/// 结果提交时必须原样回传的文件身份。
///
/// 字段保持私有，调用方只能保存并回传由任务文件管理器产生的身份；状态更新时会
/// 重新读取完整行，核对 run、lane、偏移、长度、item 和掩码后才允许原位写入。
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct TaskFileIdentity {
    run_id: String,
    lane_file_name: String,
    item_id: Uuid,
    line_offset: u64,
    line_length: u64,
    missing: TaskWorkMask,
}

impl TaskFileIdentity {
    /// 用已验证的任务文件身份构造一个可回传的行身份。
    pub fn new(
        run_id: impl ToString,
        lane: &TaskDiskLane,
        item_id: Uuid,
        line_offset: u64,
        line_length: u64,
        missing: TaskWorkMask,
    ) -> io::Result<Self> {
        let run_id = canonical_run_id(&run_id.to_string())?;
        let lane_file_name = lane_file_name(lane)?;
        validate_item_id(item_id)?;
        if line_length == 0 {
            return Err(invalid_input("任务行长度不能为空"));
        }
        if missing.0 == 0 || missing.0 & !TASK_KNOWN_BITS != 0 {
            return Err(invalid_input("任务身份缺失掩码无效"));
        }
        Ok(Self {
            run_id,
            lane_file_name,
            item_id,
            line_offset,
            line_length,
            missing,
        })
    }

    /// 返回规范运行 UUID 字符串。
    pub fn run_id(&self) -> &str {
        &self.run_id
    }

    /// 返回任务项 UUID v7。
    pub const fn item_id(&self) -> Uuid {
        self.item_id
    }

    /// 返回只由物理盘身份生成的任务文件名。
    pub fn lane_file_name(&self) -> &str {
        &self.lane_file_name
    }

    /// 返回任务行的绝对字节偏移。
    pub const fn line_offset(&self) -> u64 {
        self.line_offset
    }

    /// 返回任务行的完整字节长度。
    pub const fn line_length(&self) -> u64 {
        self.line_length
    }

    /// 返回任务行的缺失掩码。
    pub const fn missing(&self) -> TaskWorkMask {
        self.missing
    }
}

/// 一个可交给后续 dispatcher 的拥有型队首快照。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TaskLaneHead {
    /// 队首行身份。
    pub identity: TaskFileIdentity,
    /// 队首任务内容。
    pub record: TaskFileRecord,
}

/// 任务文件发布边界的拥有型通知句柄。
///
/// 句柄只携带原子序号、通知对象和所有权令牌，不暴露任何文件句柄或读取游标。
#[derive(Clone)]
pub struct TaskFilePublication {
    epoch: Arc<AtomicU64>,
    notify: Arc<tokio::sync::Notify>,
    _owner_token: Arc<()>,
}

impl TaskFilePublication {
    /// 返回当前发布变更序号。
    pub fn epoch(&self) -> u64 {
        self.epoch.load(Ordering::Acquire)
    }

    /// 等待序号超过 `observed`；注册通知后再次检查，避免丢失唤醒。
    pub async fn wait_for_change(&self, observed: u64) -> u64 {
        loop {
            let notified = self.notify.notified();
            tokio::pin!(notified);
            notified.as_mut().enable();
            let current = self.epoch.load(Ordering::Acquire);
            if current != observed {
                return current;
            }
            notified.await;
        }
    }
}

/// 隐藏全部文件句柄、追加游标、状态偏移和有限预读的任务文件集合。
pub struct TransientTaskFileSet {
    run_id: String,
    runtime_root: PathBuf,
    run_dir: PathBuf,
    lanes: BTreeMap<String, LaneState>,
    item_lanes: BTreeMap<Uuid, String>,
    physical_lanes: BTreeMap<dedup_windows::PhysicalDiskId, String>,
    sealed: bool,
    discarded: bool,
    poisoned: bool,
    poison_reason: Option<String>,
    change_epoch: Arc<AtomicU64>,
    publication_notify: Arc<tokio::sync::Notify>,
    owner_token: Arc<()>,
    fail_next_append_flush: bool,
    fail_next_status_sync: bool,
}

struct LaneState {
    lane: TaskDiskLane,
    writer: BufWriter<File>,
    reader: File,
    status_writer: File,
    metadata: Vec<RowMeta>,
    prefetched: VecDeque<TaskLaneHead>,
    cursor: usize,
    published_len: u64,
    sealed: bool,
    in_flight: Option<TaskFileIdentity>,
    poisoned: bool,
}

#[derive(Clone, Copy)]
struct RowMeta {
    item_id: Uuid,
    missing: TaskWorkMask,
    offset: u64,
    length: u64,
    status: TaskLineStatus,
}

impl TransientTaskFileSet {
    /// 在全新的运行目录中创建任务文件集合；目录存在时拒绝复用。
    pub fn create(runtime_root: &Path, run_id: impl ToString) -> io::Result<Self> {
        let run_id = canonical_run_id(&run_id.to_string())?;
        fs::create_dir_all(runtime_root)?;
        let run_dir = runtime_root.join(&run_id);
        fs::create_dir(&run_dir)?;
        Ok(Self {
            run_id,
            runtime_root: runtime_root.to_path_buf(),
            run_dir,
            lanes: BTreeMap::new(),
            item_lanes: BTreeMap::new(),
            physical_lanes: BTreeMap::new(),
            sealed: false,
            discarded: false,
            poisoned: false,
            poison_reason: None,
            change_epoch: Arc::new(AtomicU64::new(0)),
            publication_notify: Arc::new(tokio::sync::Notify::new()),
            owner_token: Arc::new(()),
            fail_next_append_flush: false,
            fail_next_status_sync: false,
        })
    }

    /// 返回本次运行的规范 UUID。
    pub fn run_id(&self) -> &str {
        &self.run_id
    }

    /// 返回任务文件发布通知句柄；句柄保持运行目录的活动所有权。
    pub fn publication(&self) -> TaskFilePublication {
        TaskFilePublication {
            epoch: Arc::clone(&self.change_epoch),
            notify: Arc::clone(&self.publication_notify),
            _owner_token: Arc::clone(&self.owner_token),
        }
    }

    /// 返回运行是否已经进入不可恢复的毒化状态。
    pub fn is_poisoned(&self) -> bool {
        self.poisoned
    }

    /// 返回最近一次导致运行毒化的错误说明。
    pub fn poison_reason(&self) -> Option<&str> {
        self.poison_reason.as_deref()
    }

    /// 检查运行仍可继续使用；毒化或丢弃后所有生产操作都失败关闭。
    pub fn health(&self) -> io::Result<()> {
        self.ensure_active()
    }

    /// 注入下一次追加 flush 失败，仅供真实 IO seam 测试验证失败关闭语义。
    pub fn fail_next_append_flush_for_test(&mut self) {
        self.fail_next_append_flush = true;
    }

    /// 注入下一次状态同步失败，写入状态字节后再返回错误，仅供行为测试使用。
    pub fn fail_next_status_sync_for_test(&mut self) {
        self.fail_next_status_sync = true;
    }

    /// 关闭本集合句柄并删除创建时固定的唯一运行目录。
    ///
    /// 任何发布句柄仍存活时拒绝删除；目录校验通过后只删除 runtime 的直接子目录，
    /// 不接受外部传入的任意路径或运行 ID。
    pub fn discard(&mut self) -> io::Result<()> {
        self.ensure_not_discarded()?;
        if Arc::strong_count(&self.owner_token) != 1 {
            return Err(invalid_input("任务文件仍有活动所有者，不能 discard"));
        }
        validate_exact_run_directory(&self.runtime_root, &self.run_dir, &self.run_id)?;
        self.lanes.clear();
        fs::remove_dir_all(&self.run_dir)?;
        self.discarded = true;
        self.advance_epoch();
        Ok(())
    }

    /// 返回由 lane 身份计算出的文件路径；尚未追加时文件可能尚不存在。
    pub fn lane_path(&self, lane: &TaskDiskLane) -> io::Result<PathBuf> {
        Ok(self.run_dir.join(lane_file_name(lane)?))
    }

    /// 注册一个空 lane 文件，便于 dispatcher 观察尚未有生产者写入的 lane。
    pub fn register_lane(&mut self, lane: &TaskDiskLane) -> io::Result<()> {
        self.ensure_active()?;
        if self.sealed {
            return Err(invalid_input("任务文件集合已经 seal，不能注册 lane"));
        }
        let key = lane_file_name(lane)?;
        self.ensure_lane(lane, key)?;
        self.advance_epoch();
        Ok(())
    }

    /// 追加一整批真实缺失任务；只有整批 flush 成功后才扩大 published 边界。
    pub fn append_batch(
        &mut self,
        lane: &TaskDiskLane,
        rows: &[TaskFileRecord],
    ) -> io::Result<Vec<TaskFileIdentity>> {
        self.ensure_active()?;
        if self.sealed {
            return Err(invalid_input("任务文件集合已经 seal，不能追加"));
        }
        let key = lane_file_name(lane)?;
        if rows.is_empty() {
            return Ok(Vec::new());
        }

        let mut batch_ids = BTreeSet::new();
        let mut serialized = Vec::with_capacity(rows.len());
        for row in rows {
            validate_record(row)?;
            if !batch_ids.insert(row.item_id) || self.item_lanes.contains_key(&row.item_id) {
                return Err(invalid_input("任务项 UUID 在运行中重复"));
            }
            serialized.push(serialize_record(row)?);
        }

        let run_id = self.run_id.clone();
        let starting_offset = self
            .lanes
            .get(&key)
            .map_or(0, |lane_state| lane_state.published_len);
        let mut identities = Vec::with_capacity(rows.len());
        let mut offset = starting_offset;
        for (row, bytes) in rows.iter().zip(serialized.iter()) {
            let length = u64::try_from(bytes.len()).map_err(|_| invalid_input("任务行过长"))?;
            identities.push(TaskFileIdentity {
                run_id: run_id.clone(),
                lane_file_name: key.clone(),
                item_id: row.item_id,
                line_offset: offset,
                line_length: length,
                missing: row.missing,
            });
            offset = offset
                .checked_add(length)
                .ok_or_else(|| invalid_input("任务文件长度溢出"))?;
        }

        let inject_flush_failure = self.fail_next_append_flush;
        self.fail_next_append_flush = false;
        let write_error = {
            let lane_state = self.ensure_lane(lane, key.clone())?;
            if lane_state.sealed || lane_state.poisoned {
                return Err(invalid_input("任务 lane 已封闭或发生不可恢复的写入错误"));
            }
            if lane_state.published_len != starting_offset {
                return Err(invalid_data("任务 lane 发布长度发生变化"));
            }
            let write_result = (|| {
                for bytes in &serialized {
                    lane_state.writer.write_all(bytes)?;
                }
                if inject_flush_failure {
                    return Err(io::Error::other("注入任务文件 flush 失败"));
                }
                lane_state.writer.flush()
            })();
            if let Err(error) = write_result {
                let _ = lane_state.writer.get_mut().set_len(starting_offset);
                let _ = lane_state
                    .writer
                    .get_mut()
                    .seek(SeekFrom::Start(starting_offset));
                lane_state.poisoned = true;
                Some(error)
            } else {
                for (row, identity) in rows.iter().zip(identities.iter()) {
                    lane_state.metadata.push(RowMeta {
                        item_id: row.item_id,
                        missing: row.missing,
                        offset: identity.line_offset,
                        length: identity.line_length,
                        status: TaskLineStatus::Pending,
                    });
                }
                lane_state.published_len = offset;
                None
            }
        };
        if let Some(error) = write_error {
            self.poison_run(format!("任务文件追加失败: {error}"));
            return Err(error);
        }
        for row in rows {
            self.item_lanes.insert(row.item_id, key.clone());
        }
        self.advance_epoch();
        Ok(identities)
    }

    /// 刷新全部 lane 并封闭生产边界；seal 后不再接受追加。
    pub fn seal(&mut self) -> io::Result<()> {
        self.ensure_active()?;
        if self.sealed {
            return Ok(());
        }
        let mut flush_error = None;
        for lane in self.lanes.values_mut() {
            if let Err(error) = lane.writer.flush() {
                lane.poisoned = true;
                flush_error = Some(error);
                break;
            }
        }
        if let Some(error) = flush_error {
            self.poison_run(format!("任务文件 seal flush 失败: {error}"));
            return Err(error);
        }
        for lane in self.lanes.values_mut() {
            lane.sealed = true;
        }
        self.sealed = true;
        self.advance_epoch();
        Ok(())
    }

    /// 返回 lane 当前队首的拥有型快照；只解析有限预读窗口。
    pub fn peek_lane(&mut self, lane: &TaskDiskLane) -> io::Result<Option<TaskLaneHead>> {
        self.ensure_active()?;
        let key = lane_file_name(lane)?;
        let set_run_id = self.run_id.clone();
        let lane_state = match self.lanes.get_mut(&key) {
            Some(value) => value,
            None => return Ok(None),
        };
        refill_lane(&set_run_id, lane_state)?;
        Ok(lane_state.prefetched.front().cloned())
    }

    /// 返回 lane 当前队首的拥有型快照，供异步 dispatcher 保存身份。
    pub fn owned_lane_head(&mut self, lane: &TaskDiskLane) -> io::Result<Option<TaskLaneHead>> {
        self.ensure_active()?;
        let key = lane_file_name(lane)?;
        let set_run_id = self.run_id.clone();
        let lane_state = match self.lanes.get_mut(&key) {
            Some(value) => value,
            None => return Ok(None),
        };
        refill_lane(&set_run_id, lane_state)?;
        Ok(lane_state.prefetched.front().cloned())
    }

    /// 返回指定 lane 当前队首身份，便于 dispatcher 先观察再精确领取。
    pub fn head_identity(&mut self, lane: &TaskDiskLane) -> io::Result<Option<TaskFileIdentity>> {
        Ok(self.owned_lane_head(lane)?.map(|head| head.identity))
    }

    /// 返回所有已注册 lane 的拥有型队首；每个 lane 最多返回一行。
    pub fn lane_heads(&mut self) -> io::Result<Vec<(TaskDiskLane, TaskLaneHead)>> {
        let lanes = self
            .lanes
            .values()
            .map(|lane_state| lane_state.lane.clone())
            .collect::<Vec<_>>();
        let mut heads = Vec::new();
        for lane in lanes {
            if let Some(head) = self.owned_lane_head(&lane)? {
                heads.push((lane, head));
            }
        }
        Ok(heads)
    }

    /// 返回所有已注册 lane 的只读快照，供 dispatcher 构造固定观察集合。
    pub fn lanes(&self) -> Vec<TaskDiskLane> {
        self.lanes
            .values()
            .map(|lane_state| lane_state.lane.clone())
            .collect()
    }

    /// 领取观察到的队首任务；只能使用完整身份，文件首字节仍保持 `P`。
    pub fn take_lane(
        &mut self,
        expected_identity: &TaskFileIdentity,
    ) -> io::Result<Option<(TaskFileIdentity, TaskFileRecord)>> {
        self.ensure_active()?;
        if expected_identity.run_id != self.run_id {
            return Err(invalid_input("任务身份 run_id 不匹配"));
        }
        let key = expected_identity.lane_file_name.clone();
        let set_run_id = self.run_id.clone();
        let lane_state = match self.lanes.get_mut(&key) {
            Some(value) => value,
            None => return Ok(None),
        };
        if lane_state.in_flight.is_some() {
            return Ok(None);
        }
        refill_lane(&set_run_id, lane_state)?;
        let head = match lane_state.prefetched.front() {
            Some(value) => value,
            None => return Ok(None),
        };
        if expected_identity != &head.identity {
            return Err(invalid_input("任务队首身份已经变化"));
        }
        let head = lane_state.prefetched.pop_front().expect("已确认队首存在");
        lane_state.in_flight = Some(head.identity.clone());
        Ok(Some((head.identity, head.record)))
    }

    /// 在 SQLite 事务 ACK 成功后，把指定任务行从 `P` 原位改为 `C`。
    pub fn mark_completed(&mut self, identity: &TaskFileIdentity) -> io::Result<()> {
        self.mark_terminal(identity, TaskLineStatus::Completed)
    }

    /// 在单文件读取或 Worker 失败后，把指定任务行从 `P` 原位改为 `F`。
    pub fn mark_failed(&mut self, identity: &TaskFileIdentity) -> io::Result<()> {
        self.mark_terminal(identity, TaskLineStatus::Failed)
    }

    /// 返回只包含已发布边界的预读对象数量，便于验证内存上限。
    pub fn prefetched_len(&mut self, lane: &TaskDiskLane) -> io::Result<usize> {
        let key = lane_file_name(lane)?;
        let set_run_id = self.run_id.clone();
        let lane_state = self
            .lanes
            .get_mut(&key)
            .ok_or_else(|| invalid_input("任务 lane 尚未注册"))?;
        refill_lane(&set_run_id, lane_state)?;
        Ok(lane_state.prefetched.len())
    }

    /// 返回当前已 flush 且对读取器可见的 lane 字节数。
    pub fn published_len(&self, lane: &TaskDiskLane) -> io::Result<u64> {
        let key = lane_file_name(lane)?;
        Ok(self
            .lanes
            .get(&key)
            .map_or(0, |lane_state| lane_state.published_len))
    }

    /// 返回指定 lane 是否已经封闭生产。
    pub fn is_sealed(&self, lane: &TaskDiskLane) -> io::Result<bool> {
        let key = lane_file_name(lane)?;
        Ok(self
            .lanes
            .get(&key)
            .is_some_and(|lane_state| lane_state.sealed))
    }

    /// 返回发布或状态变化的单调通知序号，dispatcher 可用它避免忙等。
    pub fn change_epoch(&self) -> u64 {
        self.change_epoch.load(Ordering::Acquire)
    }

    /// 返回发布边界或行状态变化的通知序号。
    pub fn publication_epoch(&self) -> u64 {
        self.change_epoch()
    }

    /// 返回运行目录是否已 seal、全部行已进入终态且没有在途 ACK。
    pub fn all_terminal(&self) -> bool {
        !self.discarded
            && !self.poisoned
            && self.sealed
            && self.lanes.values().all(|lane| {
                lane.sealed
                    && !lane.poisoned
                    && lane.in_flight.is_none()
                    && lane.prefetched.is_empty()
                    && lane
                        .metadata
                        .iter()
                        .all(|row| row.status != TaskLineStatus::Pending)
            })
    }

    fn ensure_not_discarded(&self) -> io::Result<()> {
        if self.discarded {
            Err(invalid_input("任务文件集合已经 discard"))
        } else {
            Ok(())
        }
    }

    fn ensure_active(&self) -> io::Result<()> {
        self.ensure_not_discarded()?;
        if self.poisoned {
            Err(io::Error::other(
                self.poison_reason
                    .as_deref()
                    .unwrap_or("任务文件集合已经毒化"),
            ))
        } else {
            Ok(())
        }
    }

    fn poison_run(&mut self, reason: impl Into<String>) {
        self.poisoned = true;
        if self.poison_reason.is_none() {
            self.poison_reason = Some(reason.into());
        }
        self.advance_epoch();
    }

    fn advance_epoch(&self) {
        self.change_epoch.fetch_add(1, Ordering::AcqRel);
        self.publication_notify.notify_waiters();
    }

    fn ensure_lane(&mut self, lane: &TaskDiskLane, key: String) -> io::Result<&mut LaneState> {
        if let Some(existing) = self.lanes.get(&key) {
            if !same_lane_configuration(&existing.lane, lane) {
                return Err(invalid_input("同名任务 lane 的物理盘配置不一致"));
            }
            return self
                .lanes
                .get_mut(&key)
                .ok_or_else(|| invalid_input("任务 lane 不存在"));
        }
        if let Some(existing_key) = self.physical_lanes.get(&lane.physical_disk_id) {
            let existing = self
                .lanes
                .get(existing_key)
                .ok_or_else(|| invalid_input("物理盘 lane 索引损坏"))?;
            if !same_lane_configuration(&existing.lane, lane) {
                return Err(invalid_input("同一物理盘的 lane 配置已经冻结"));
            }
            return Err(invalid_input("同一物理盘 lane 文件名不一致"));
        }
        let path = self.run_dir.join(&key);
        let writer_file = OpenOptions::new()
            .create_new(true)
            .read(true)
            .write(true)
            .open(&path)?;
        let reader = writer_file.try_clone()?;
        let status_writer = writer_file.try_clone()?;
        self.lanes.insert(
            key.clone(),
            LaneState {
                lane: lane.clone(),
                writer: BufWriter::new(writer_file),
                reader,
                status_writer,
                metadata: Vec::new(),
                prefetched: VecDeque::new(),
                cursor: 0,
                published_len: 0,
                sealed: false,
                in_flight: None,
                poisoned: false,
            },
        );
        self.physical_lanes
            .insert(lane.physical_disk_id.clone(), key.clone());
        self.lanes
            .get_mut(&key)
            .ok_or_else(|| invalid_input("任务 lane 创建失败"))
    }

    fn mark_terminal(
        &mut self,
        identity: &TaskFileIdentity,
        target: TaskLineStatus,
    ) -> io::Result<()> {
        self.ensure_active()?;
        if identity.run_id != self.run_id {
            return Err(invalid_input("任务身份 run_id 不匹配"));
        }
        let inject_sync_failure = self.fail_next_status_sync;
        self.fail_next_status_sync = false;
        let lane_state = self
            .lanes
            .get_mut(&identity.lane_file_name)
            .ok_or_else(|| invalid_input("任务身份 lane 不存在"))?;
        if let Err(error) = lane_state.writer.flush() {
            lane_state.poisoned = true;
            self.poison_run(format!("任务状态写入前 flush 失败: {error}"));
            return Err(error);
        }
        let line_end = identity
            .line_offset
            .checked_add(identity.line_length)
            .ok_or_else(|| invalid_input("任务身份越过 published 边界"))?;
        if line_end > lane_state.published_len {
            return Err(invalid_input("任务身份越过 published 边界"));
        }
        let bytes = read_at(
            &lane_state.reader,
            identity.line_offset,
            identity.line_length,
        )?;
        let parsed = parse_record(&bytes)?;
        if parsed.status != lane_status(lane_state, identity)?
            || parsed.record.item_id != identity.item_id
            || parsed.record.missing != identity.missing
        {
            return Err(invalid_data("任务身份与文件行内容不一致"));
        }
        let meta = lane_state
            .metadata
            .iter_mut()
            .find(|row| row.offset == identity.line_offset);
        let meta = meta.ok_or_else(|| invalid_data("任务身份偏移不在已发布边界内"))?;
        if meta.length != identity.line_length
            || meta.item_id != identity.item_id
            || meta.missing != identity.missing
        {
            return Err(invalid_data("任务身份行长度或内容不一致"));
        }
        if parsed.status == target {
            return Ok(());
        }
        if parsed.status != TaskLineStatus::Pending || meta.status != TaskLineStatus::Pending {
            return Err(invalid_input("任务行只允许从 P 转换为目标终态"));
        }
        if lane_state.in_flight.as_ref() != Some(identity) {
            return Err(invalid_input("任务行尚未被当前 dispatcher 领取"));
        }

        let write_result = write_status(
            &lane_state.status_writer,
            identity.line_offset,
            target.as_byte(),
            inject_sync_failure,
        );
        if let Err(error) = write_result {
            lane_state.poisoned = true;
            self.poison_run(format!("任务状态写入或同步失败: {error}"));
            return Err(error);
        }
        meta.status = target;
        lane_state.in_flight = None;
        self.advance_epoch();
        Ok(())
    }
}

fn lane_status(lane: &LaneState, identity: &TaskFileIdentity) -> io::Result<TaskLineStatus> {
    lane.metadata
        .iter()
        .find(|row| row.offset == identity.line_offset)
        .map(|row| row.status)
        .ok_or_else(|| invalid_data("任务身份偏移不在已发布边界内"))
}

fn refill_lane(run_id: &str, lane: &mut LaneState) -> io::Result<()> {
    if lane.in_flight.is_some() {
        return Ok(());
    }
    let window = lane.lane.per_disk_limit.saturating_mul(2).max(2);
    while lane.prefetched.len() < window && lane.cursor < lane.metadata.len() {
        let meta = lane.metadata[lane.cursor];
        if meta.status != TaskLineStatus::Pending {
            lane.cursor += 1;
            continue;
        }
        let bytes = read_at(&lane.reader, meta.offset, meta.length)?;
        let parsed = parse_record(&bytes)?;
        if parsed.status != TaskLineStatus::Pending
            || parsed.record.item_id != meta.item_id
            || parsed.record.missing != meta.missing
        {
            return Err(invalid_data("任务文件预读行与已发布元数据不一致"));
        }
        let identity = TaskFileIdentity {
            run_id: run_id.to_owned(),
            lane_file_name: lane_file_name(&lane.lane)?,
            item_id: meta.item_id,
            line_offset: meta.offset,
            line_length: meta.length,
            missing: meta.missing,
        };
        lane.prefetched.push_back(TaskLaneHead {
            identity,
            record: parsed.record,
        });
        lane.cursor += 1;
    }
    Ok(())
}

struct ParsedRecord {
    status: TaskLineStatus,
    record: TaskFileRecord,
}

fn serialize_record(record: &TaskFileRecord) -> io::Result<Vec<u8>> {
    let normalized_path = record.scanned.normalized_path.as_str();
    let display_path = record
        .scanned
        .display_path
        .as_path()
        .to_str()
        .ok_or_else(|| invalid_input("显示路径不是有效 UTF-8"))?;
    validate_text_field(normalized_path, "规范路径")?;
    validate_text_field(display_path, "显示路径")?;
    let md5 = record
        .known_md5
        .map_or_else(String::new, |bytes| encode_hex(&bytes));
    let line = format!(
        "P\t{}\t{}\t{}\t{}\t{}\t{}\t{:016x}\n",
        record.item_id,
        record.work_kind.as_str(),
        normalized_path,
        display_path,
        record.scanned.file_size,
        md5,
        record.missing.bits()
    );
    Ok(line.into_bytes())
}

fn parse_record(bytes: &[u8]) -> io::Result<ParsedRecord> {
    if bytes.is_empty() || bytes.last() != Some(&b'\n') || bytes.contains(&b'\r') {
        return Err(invalid_data("任务行必须以 LF 结束且不能包含 CR"));
    }
    let text = std::str::from_utf8(bytes).map_err(|_| invalid_data("任务行不是 UTF-8"))?;
    let fields = text[..text.len() - 1].split('\t').collect::<Vec<_>>();
    if fields.len() != 8 || fields.iter().any(|field| field.contains(['\r', '\n'])) {
        return Err(invalid_data("任务行必须严格包含 8 列"));
    }
    if fields[0].len() != 1 {
        return Err(invalid_data("任务行状态列无效"));
    }
    let status = TaskLineStatus::parse(fields[0].as_bytes()[0])?;
    let item_id = parse_canonical_v7(fields[1], "任务项 ID")?;
    let work_kind = TaskWorkKind::parse(fields[2])?;
    let normalized =
        dedup_core::NormalizedPath::new(fields[3]).map_err(|_| invalid_data("规范路径无效"))?;
    let display =
        dedup_core::DisplayPath::new(fields[4]).map_err(|_| invalid_data("显示路径无效"))?;
    let file_size = fields[5]
        .parse::<u64>()
        .map_err(|_| invalid_data("文件大小不是十进制 u64"))?;
    let known_md5 = parse_md5(fields[6])?;
    let missing_bits = parse_fixed_hex_u64(fields[7])?;
    let missing =
        TaskWorkMask::from_bits(missing_bits).ok_or_else(|| invalid_data("任务缺失掩码无效"))?;
    missing.validate_for(work_kind, known_md5)?;
    Ok(ParsedRecord {
        status,
        record: TaskFileRecord {
            item_id,
            work_kind,
            scanned: ScannedPath::new(normalized, display, file_size),
            known_md5,
            missing,
        },
    })
}

fn validate_record(record: &TaskFileRecord) -> io::Result<()> {
    validate_item_id(record.item_id)?;
    record
        .missing
        .validate_for(record.work_kind, record.known_md5)?;
    let normalized = record.scanned.normalized_path.as_str();
    let display = record
        .scanned
        .display_path
        .as_path()
        .to_str()
        .ok_or_else(|| invalid_input("显示路径不是有效 UTF-8"))?;
    validate_text_field(normalized, "规范路径")?;
    validate_text_field(display, "显示路径")?;
    Ok(())
}

fn validate_item_id(item_id: Uuid) -> io::Result<()> {
    if item_id.get_version_num() != 7 {
        return Err(invalid_input("任务项 ID 必须是 UUID v7"));
    }
    Ok(())
}

fn parse_canonical_v7(value: &str, field: &str) -> io::Result<Uuid> {
    let uuid = Uuid::parse_str(value).map_err(|_| invalid_data(field))?;
    if uuid.get_version_num() != 7 || uuid.to_string() != value {
        return Err(invalid_data("UUID 必须是规范小写 v7"));
    }
    Ok(uuid)
}

fn canonical_run_id(value: &str) -> io::Result<String> {
    let uuid = parse_canonical_v7(value, "运行 ID 必须是 UUID v7")?;
    Ok(uuid.to_string())
}

fn validate_text_field(value: &str, field: &str) -> io::Result<()> {
    if value.contains(['\t', '\r', '\n']) {
        return Err(invalid_input(format!("{field}不能包含 tab、CR 或 LF")));
    }
    Ok(())
}

fn parse_md5(value: &str) -> io::Result<Option<[u8; 16]>> {
    if value.is_empty() {
        return Ok(None);
    }
    if value.len() != 32
        || value
            .bytes()
            .any(|byte| !byte.is_ascii_hexdigit() || byte.is_ascii_uppercase())
    {
        return Err(invalid_data("已知 MD5 必须是 32 位小写十六进制"));
    }
    let mut result = [0u8; 16];
    for (index, slot) in result.iter_mut().enumerate() {
        *slot = (hex_nibble(value.as_bytes()[index * 2])? << 4)
            | hex_nibble(value.as_bytes()[index * 2 + 1])?;
    }
    Ok(Some(result))
}

fn parse_fixed_hex_u64(value: &str) -> io::Result<u64> {
    if value.len() != 16
        || value
            .bytes()
            .any(|byte| !byte.is_ascii_hexdigit() || byte.is_ascii_uppercase())
    {
        return Err(invalid_data("缺失掩码必须是 16 位小写十六进制"));
    }
    u64::from_str_radix(value, 16).map_err(|_| invalid_data("缺失掩码不是十六进制"))
}

fn hex_nibble(byte: u8) -> io::Result<u8> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        _ => Err(invalid_data("十六进制字段无效")),
    }
}

fn encode_hex(bytes: &[u8; 16]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut result = String::with_capacity(32);
    for byte in bytes {
        result.push(HEX[(byte >> 4) as usize] as char);
        result.push(HEX[(byte & 0x0f) as usize] as char);
    }
    result
}

fn read_at(file: &File, offset: u64, length: u64) -> io::Result<Vec<u8>> {
    let length = usize::try_from(length).map_err(|_| invalid_data("任务行长度超过内存可读范围"))?;
    let mut bytes = vec![0u8; length];
    #[cfg(windows)]
    {
        use std::os::windows::fs::FileExt;
        let mut read = 0usize;
        while read < bytes.len() {
            let count = file.seek_read(&mut bytes[read..], offset + read as u64)?;
            if count == 0 {
                return Err(invalid_data("任务行在已发布边界内却读取不完整"));
            }
            read += count;
        }
    }
    #[cfg(not(windows))]
    {
        let mut handle = file.try_clone()?;
        handle.seek(SeekFrom::Start(offset))?;
        handle.read_exact(&mut bytes)?;
    }
    Ok(bytes)
}

fn write_status(file: &File, offset: u64, byte: u8, fail_sync: bool) -> io::Result<()> {
    #[cfg(windows)]
    {
        use std::os::windows::fs::FileExt;
        if file.seek_write(&[byte], offset)? != 1 {
            return Err(io::Error::new(
                io::ErrorKind::WriteZero,
                "任务状态字节未完整写入",
            ));
        }
        if fail_sync {
            return Err(io::Error::other("注入任务状态 sync_data 失败"));
        }
        file.sync_data()?;
    }
    #[cfg(not(windows))]
    {
        let mut handle = file.try_clone()?;
        handle.seek(SeekFrom::Start(offset))?;
        handle.write_all(&[byte])?;
        if fail_sync {
            return Err(io::Error::other("注入任务状态 sync_data 失败"));
        }
        handle.sync_data()?;
    }
    Ok(())
}

fn lane_file_name(lane: &TaskDiskLane) -> io::Result<String> {
    let mut numbers = lane.physical_disk_numbers.clone();
    if numbers.is_empty() {
        return Err(invalid_input("物理盘编号不能为空"));
    }
    numbers.sort_unstable();
    numbers.dedup();
    if numbers != lane.physical_disk_id.disk_numbers() {
        return Err(invalid_input("lane 的物理盘编号与物理盘身份不一致"));
    }
    let kind = match lane.disk_kind {
        LocalDiskKind::Hdd => "hdd",
        LocalDiskKind::Ssd => "ssd",
        LocalDiskKind::Unknown => "unknown",
    };
    let disk_numbers = numbers
        .iter()
        .map(u32::to_string)
        .collect::<Vec<_>>()
        .join("+");
    Ok(format!("PhysicalDisk{disk_numbers}-{kind}.tasks.tsv"))
}

fn same_lane_configuration(left: &TaskDiskLane, right: &TaskDiskLane) -> bool {
    left.physical_disk_id == right.physical_disk_id
        && left.physical_disk_numbers == right.physical_disk_numbers
        && left.disk_kind == right.disk_kind
        && left.configured_weight == right.configured_weight
        && left.per_disk_limit == right.per_disk_limit
}

fn validate_exact_run_directory(
    runtime_root: &Path,
    run_dir: &Path,
    run_id: &str,
) -> io::Result<()> {
    validate_directory(runtime_root, "runtime 根目录")?;
    validate_directory(run_dir, "任务运行目录")?;
    if run_dir.parent() != Some(runtime_root)
        || run_dir.file_name().and_then(|name| name.to_str()) != Some(run_id)
    {
        return Err(invalid_input("任务运行目录不是 runtime 根目录的直接子目录"));
    }
    Ok(())
}

fn validate_directory(path: &Path, label: &str) -> io::Result<()> {
    let metadata = fs::symlink_metadata(path)?;
    if !metadata.is_dir() {
        return Err(invalid_input(format!("{label}不是目录")));
    }
    #[cfg(windows)]
    {
        use std::os::windows::fs::MetadataExt;
        const FILE_ATTRIBUTE_REPARSE_POINT: u32 = 0x0400;
        if metadata.file_attributes() & FILE_ATTRIBUTE_REPARSE_POINT != 0 {
            return Err(invalid_input(format!("{label}不能是重解析点")));
        }
    }
    #[cfg(not(windows))]
    if metadata.file_type().is_symlink() {
        return Err(invalid_input(format!("{label}不能是符号链接")));
    }
    Ok(())
}

fn invalid_input(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message.into())
}

fn invalid_data(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}

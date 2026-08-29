//! 最近一次本地分析结果的顺序验真和偏移窗口读取器。

use std::{
    collections::BTreeMap,
    fs::File,
    io::{BufRead, BufReader, Seek, SeekFrom},
    path::Path,
};

use dedup_core::{ContentKey, LocationKey, MachineId, NormalizedPath, Thresholds};
use dedup_node_store::GroupKind;
use sha2::{Digest, Sha256};
use uuid::Uuid;

use super::result_file::{
    AnalysisResultError, AnalysisResultGroupKind, AnalysisResultHeader, AnalysisResultMode,
    AnalysisResultRow, VerifiedAnalysisResult,
};

/// 本地最近结果窗口的读取类别。
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum LocalResultWindowKind {
    /// 读取指定类型的重复组摘要。
    Groups(GroupKind),
    /// 读取指定重复组的成员。
    Members {
        /// 结果文件中的稳定组 ID。
        group_id: String,
    },
}

/// 最近结果中的一个最小重复组摘要。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct LocalResultGroup {
    /// 稳定分组 ID。
    pub group_id: String,
    /// 精确、图片或视频分组。
    pub kind: GroupKind,
    /// 代表内容键。
    pub representative: ContentKey,
    /// 组内成员数。
    pub member_count: u32,
    /// 删除非代表成员可释放的字节数估算。
    pub reclaimable_bytes: u64,
}

/// 一次本地结果窗口读取返回的轻量数据。
#[derive(Clone, Debug, Default, PartialEq)]
pub struct LocalResultWindow {
    /// 请求的窗口起点。
    pub start_index: u64,
    /// 当前类别或组的总行数。
    pub total_rows: u64,
    /// 组窗口内容；成员窗口时为空。
    pub groups: Vec<LocalResultGroup>,
    /// 成员窗口内容；组窗口时为空。
    pub members: Vec<AnalysisResultRow>,
}

/// 对最近一次结果文件建立的进程内偏移索引。
pub struct LatestAnalysisReader {
    /// 已按 H/M/F 顺序验证的结果元数据。
    metadata: VerifiedAnalysisResult,
    /// 已通过验证的结果句柄；发布替换路径后仍指向原文件身份。
    file: BufReader<File>,
    /// 每个首次出现分组的首行偏移。
    #[allow(dead_code)]
    group_offsets: Vec<u64>,
    /// 每个分组成员行的文件偏移，按结果文件顺序保存。
    member_offsets: BTreeMap<String, Vec<u64>>,
    /// 由行扫描累加出的最小组摘要，不保存成员对象。
    groups: Vec<LocalResultGroup>,
}

impl LatestAnalysisReader {
    /// 顺序验证完整 H/M/F 文件，并只建立组摘要与成员字节偏移。
    pub fn open_verified(path: &Path) -> Result<Self, AnalysisResultError> {
        let file = open_result_file(path)?;
        let mut reader = BufReader::new(file);
        let mut byte_offset = 0_u64;
        let (header_offset, header_bytes, header_line) =
            read_record(&mut reader, &mut byte_offset)?
                .ok_or_else(|| AnalysisResultError::InvalidFormat("结果文件为空".into()))?;
        if header_offset != 0 || header_bytes.starts_with(&[0xef, 0xbb, 0xbf]) {
            return Err(AnalysisResultError::InvalidFormat(
                "不允许 UTF-8 BOM 或非首行头记录".into(),
            ));
        }
        let header = parse_header(&header_line)?;
        if header.analysis_mode != AnalysisResultMode::Local {
            return Err(AnalysisResultError::InvalidFormat(
                "最近结果必须是本地分析".into(),
            ));
        }

        let mut hasher = Sha256::new();
        hasher.update(&header_bytes);
        let mut member_count = 0_u64;
        let mut group_indexes = BTreeMap::<String, usize>::new();
        let mut group_offsets = Vec::new();
        let mut member_offsets = BTreeMap::<String, Vec<u64>>::new();
        let mut groups = Vec::new();

        loop {
            let Some((line_offset, line_bytes, line)) = read_record(&mut reader, &mut byte_offset)?
            else {
                return Err(AnalysisResultError::InvalidFormat("缺少尾记录".into()));
            };
            match line.split('\t').next().unwrap_or_default() {
                "M" => {
                    let row = parse_member(&line)?;
                    hasher.update(&line_bytes);
                    member_count = member_count
                        .checked_add(1)
                        .ok_or_else(|| AnalysisResultError::InvalidFormat("成员数量溢出".into()))?;
                    let group_index = if let Some(index) = group_indexes.get(&row.group_id) {
                        *index
                    } else {
                        let index = groups.len();
                        group_indexes.insert(row.group_id.clone(), index);
                        group_offsets.push(line_offset);
                        member_offsets.insert(row.group_id.clone(), Vec::new());
                        groups.push(LocalResultGroup {
                            group_id: row.group_id.clone(),
                            kind: result_kind_to_store(row.group_kind),
                            representative: row.representative_content,
                            member_count: 0,
                            reclaimable_bytes: 0,
                        });
                        index
                    };
                    let group = &mut groups[group_index];
                    if group.kind != result_kind_to_store(row.group_kind)
                        || group.representative != row.representative_content
                    {
                        return Err(AnalysisResultError::InvalidFormat(
                            "同一组的类型或代表内容不一致".into(),
                        ));
                    }
                    group.member_count = group.member_count.checked_add(1).ok_or_else(|| {
                        AnalysisResultError::InvalidFormat("组成员数量溢出".into())
                    })?;
                    if !row.representative {
                        group.reclaimable_bytes = group
                            .reclaimable_bytes
                            .checked_add(row.content.file_size())
                            .ok_or_else(|| {
                                AnalysisResultError::InvalidFormat("可释放字节数溢出".into())
                            })?;
                    }
                    member_offsets
                        .get_mut(&row.group_id)
                        .expect("新建组时同时建立成员偏移表")
                        .push(line_offset);
                }
                "F" => {
                    let footer = parse_footer(&line)?;
                    if footer.member_count != member_count {
                        return Err(AnalysisResultError::InvalidFormat(
                            "尾记录数量与实际记录不匹配".into(),
                        ));
                    }
                    let actual_sha256: [u8; 32] = hasher.finalize().into();
                    if actual_sha256 != footer.sha256 {
                        return Err(AnalysisResultError::InvalidFormat(
                            "尾记录 SHA-256 不匹配".into(),
                        ));
                    }
                    if read_record(&mut reader, &mut byte_offset)?.is_some() {
                        return Err(AnalysisResultError::InvalidFormat(
                            "尾记录必须是文件最后一行".into(),
                        ));
                    }
                    let metadata = VerifiedAnalysisResult {
                        run_id: header.analysis_id,
                        library_revision: header.library_revision,
                        header,
                        member_count,
                        group_count: groups.len() as u64,
                        sha256: actual_sha256,
                    };
                    return Ok(Self {
                        metadata,
                        file: reader,
                        group_offsets,
                        member_offsets,
                        groups,
                    });
                }
                _ => {
                    return Err(AnalysisResultError::InvalidFormat(
                        "记录类型必须是 M 或 F".into(),
                    ));
                }
            }
        }
    }

    /// 返回已验证的结果元数据，不暴露成员索引内部结构。
    pub fn metadata(&self) -> &VerifiedAnalysisResult {
        &self.metadata
    }

    /// 返回原子替换时要保留的稳定来源句柄。
    pub(crate) fn source_file(&self) -> &File {
        self.file.get_ref()
    }

    /// 按类别或组 ID 读取窗口，读取时只解析请求范围内的 M 行。
    pub fn read_window(
        &mut self,
        kind: LocalResultWindowKind,
        start: u64,
        count: u32,
    ) -> Result<LocalResultWindow, AnalysisResultError> {
        match kind {
            LocalResultWindowKind::Groups(group_kind) => {
                let filtered = self
                    .groups
                    .iter()
                    .filter(|group| group.kind == group_kind)
                    .collect::<Vec<_>>();
                let total_rows = filtered.len() as u64;
                let (from, to) = window_bounds(start, u64::from(count), total_rows)?;
                Ok(LocalResultWindow {
                    start_index: start,
                    total_rows,
                    groups: filtered[from..to]
                        .iter()
                        .map(|group| (*group).clone())
                        .collect(),
                    members: Vec::new(),
                })
            }
            LocalResultWindowKind::Members { group_id } => {
                let offsets = self.member_offsets.get(&group_id).ok_or_else(|| {
                    AnalysisResultError::InvalidFormat(format!("结果组不存在: {group_id}"))
                })?;
                let total_rows = offsets.len() as u64;
                let (from, to) = window_bounds(start, u64::from(count), total_rows)?;
                let selected_offsets = offsets[from..to].to_vec();
                let mut members = Vec::with_capacity(to.saturating_sub(from));
                for expected_offset in selected_offsets {
                    self.file.seek(SeekFrom::Start(expected_offset))?;
                    let mut local_offset = expected_offset;
                    let (_, _, line) =
                        read_record(&mut self.file, &mut local_offset)?.ok_or_else(|| {
                            AnalysisResultError::InvalidFormat("成员偏移指向文件末尾".into())
                        })?;
                    let row = parse_member(&line)?;
                    if row.group_id != group_id {
                        return Err(AnalysisResultError::InvalidFormat(
                            "成员偏移与组 ID 不一致".into(),
                        ));
                    }
                    members.push(row);
                }
                Ok(LocalResultWindow {
                    start_index: start,
                    total_rows,
                    groups: Vec::new(),
                    members,
                })
            }
        }
    }

    /// 按组和位置流式查找一个成员，只在命中时保留一个解析后的成员对象。
    ///
    /// 复核和删除计划不能受 UI 窗口上限影响；这里复用进程内偏移表逐行读取，
    /// 不建立 `.idx` 文件，也不把整个大组装入内存。
    pub fn find_member(
        &mut self,
        group_id: &str,
        location: &LocationKey,
    ) -> Result<Option<AnalysisResultRow>, AnalysisResultError> {
        let offset_count = self
            .member_offsets
            .get(group_id)
            .ok_or_else(|| AnalysisResultError::InvalidFormat(format!("结果组不存在: {group_id}")))?
            .len();
        for index in 0..offset_count {
            let offset = self
                .member_offsets
                .get(group_id)
                .and_then(|offsets| offsets.get(index))
                .copied()
                .ok_or_else(|| AnalysisResultError::InvalidFormat("成员偏移索引损坏".into()))?;
            let row = self.read_member_at(offset, group_id)?;
            if &row.location == location {
                return Ok(Some(row));
            }
        }
        Ok(None)
    }

    /// 读取一个已知组成员的固定偏移并再次核对组 ID。
    fn read_member_at(
        &mut self,
        offset: u64,
        group_id: &str,
    ) -> Result<AnalysisResultRow, AnalysisResultError> {
        self.file.seek(SeekFrom::Start(offset))?;
        let mut local_offset = offset;
        let (_, _, line) = read_record(&mut self.file, &mut local_offset)?
            .ok_or_else(|| AnalysisResultError::InvalidFormat("成员偏移指向文件末尾".into()))?;
        let row = parse_member(&line)?;
        if row.group_id != group_id {
            return Err(AnalysisResultError::InvalidFormat(
                "成员偏移与组 ID 不一致".into(),
            ));
        }
        Ok(row)
    }
}

/// 以允许替换的共享模式打开结果文件，同时把 reader 绑定到该文件身份。
fn open_result_file(path: &Path) -> std::io::Result<File> {
    use std::os::windows::fs::OpenOptionsExt;

    // Windows 默认拒绝后续删除共享；结果发布必须能替换路径，但旧 reader 仍保留旧句柄。
    const FILE_SHARE_READ: u32 = 0x0001;
    const FILE_SHARE_WRITE: u32 = 0x0002;
    const FILE_SHARE_DELETE: u32 = 0x0004;
    const GENERIC_READ: u32 = 0x8000_0000;
    const DELETE: u32 = 0x0001_0000;
    std::fs::OpenOptions::new()
        .read(true)
        .access_mode(GENERIC_READ | DELETE)
        .share_mode(FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE)
        .open(path)
}

/// 读取一行并返回其起始偏移、含 LF 原字节和无 LF 文本。
fn read_record<R: BufRead>(
    reader: &mut R,
    byte_offset: &mut u64,
) -> Result<Option<(u64, Vec<u8>, String)>, AnalysisResultError> {
    let start = *byte_offset;
    let mut bytes = Vec::new();
    let read = reader.read_until(b'\n', &mut bytes)?;
    if read == 0 {
        return Ok(None);
    }
    *byte_offset = byte_offset
        .checked_add(read as u64)
        .ok_or_else(|| AnalysisResultError::InvalidFormat("文件偏移溢出".into()))?;
    if !bytes.ends_with(b"\n") {
        return Err(AnalysisResultError::InvalidFormat("记录末尾缺少 LF".into()));
    }
    if bytes[..bytes.len() - 1].contains(&b'\r') {
        return Err(AnalysisResultError::InvalidFormat(
            "记录必须使用 LF 换行".into(),
        ));
    }
    let line = String::from_utf8(bytes[..bytes.len() - 1].to_vec())
        .map_err(|_| AnalysisResultError::InvalidFormat("文件必须是 UTF-8".into()))?;
    Ok(Some((start, bytes, line)))
}

/// 解析固定 H 记录并验证格式版本、模式和阈值。
fn parse_header(line: &str) -> Result<AnalysisResultHeader, AnalysisResultError> {
    let fields = line.split('\t').collect::<Vec<_>>();
    if fields.len() != 15 || fields[0] != "H" {
        return Err(AnalysisResultError::InvalidFormat(
            "头记录列数或类型错误".into(),
        ));
    }
    let format_version = parse_u32(fields[1], "格式版本")?;
    if format_version != 1 {
        return Err(AnalysisResultError::InvalidFormat(
            "不支持的格式版本".into(),
        ));
    }
    let analysis_id = Uuid::parse_str(fields[2])
        .map(dedup_core::AnalysisRunId::from_uuid)
        .map_err(|_| AnalysisResultError::InvalidFormat("分析 ID 无效".into()))?;
    let header = AnalysisResultHeader {
        format_version,
        analysis_id,
        library_revision: parse_u64(fields[3], "文件库版本")?,
        analysis_mode: match fields[4] {
            "local" => AnalysisResultMode::Local,
            "central" => AnalysisResultMode::Central,
            _ => return Err(AnalysisResultError::InvalidFormat("分析模式无效".into())),
        },
        created_at_ms: parse_u64(fields[5], "创建时间")?,
        thresholds: Thresholds {
            pdq_quality_min: parse_u8(fields[6], "PDQ Quality 阈值")?,
            aspect_tolerance: parse_f32(fields[7], "长宽比阈值")?,
            pdq_hamming_max: parse_u16(fields[8], "PDQ 汉明阈值")?,
            phash_part_hamming_max: parse_u8(fields[9], "pHash 汉明阈值")?,
            phash_min_passed_parts: parse_u8(fields[10], "pHash 通过块数阈值")?,
            sobel_min: parse_f32(fields[11], "Sobel 阈值")?,
            video_min_valid_frames: parse_u8(fields[12], "视频有效帧阈值")?,
            video_stage1_min: parse_f32(fields[13], "视频一筛阈值")?,
            video_stage2_min: parse_f32(fields[14], "视频二筛阈值")?,
        },
    };
    header
        .thresholds
        .validate()
        .map_err(|error| AnalysisResultError::InvalidFormat(format!("阈值无效: {error}")))?;
    Ok(header)
}

/// 解析固定 M 记录并构造一次性成员对象。
fn parse_member(line: &str) -> Result<AnalysisResultRow, AnalysisResultError> {
    let fields = line.split('\t').collect::<Vec<_>>();
    if fields.len() != 14 || fields[0] != "M" {
        return Err(AnalysisResultError::InvalidFormat(
            "成员记录列数或类型错误".into(),
        ));
    }
    let group_kind = parse_result_kind(fields[1])?;
    ensure_text(fields[2], "组 ID")?;
    if fields[2].is_empty() {
        return Err(AnalysisResultError::InvalidFormat("组 ID 不能为空".into()));
    }
    let representative = match fields[3] {
        "0" => false,
        "1" => true,
        _ => return Err(AnalysisResultError::InvalidFormat("代表标记无效".into())),
    };
    let representative_content = parse_content_key(fields[4], fields[5])?;
    let machine = MachineId::parse(fields[6])
        .map_err(|_| AnalysisResultError::InvalidFormat("机器 ID 无效".into()))?;
    let normalized = NormalizedPath::new(fields[7])
        .map_err(|_| AnalysisResultError::InvalidFormat("规范路径无效".into()))?;
    ensure_text(fields[8], "显示路径")?;
    let content = parse_content_key(fields[9], fields[10])?;
    let stage1_score = parse_f64(fields[11], "一筛分数")?;
    let phash_passed_parts = if fields[12].is_empty() {
        None
    } else {
        let value = parse_u8(fields[12], "pHash 通过块数")?;
        if !(1..=9).contains(&value) {
            return Err(AnalysisResultError::InvalidFormat(
                "pHash 通过块数必须位于 1..=9".into(),
            ));
        }
        Some(value)
    };
    let stage2_score = (!fields[13].is_empty())
        .then(|| parse_f64(fields[13], "二筛分数"))
        .transpose()?;
    Ok(AnalysisResultRow {
        group_kind,
        group_id: fields[2].into(),
        representative,
        representative_content,
        location: LocationKey::new(machine, normalized),
        display_path: fields[8].into(),
        content,
        stage1_score,
        phash_passed_parts,
        stage2_score,
    })
}

/// 解析固定 F 记录。
fn parse_footer(line: &str) -> Result<Footer, AnalysisResultError> {
    let fields = line.split('\t').collect::<Vec<_>>();
    if fields.len() != 3 || fields[0] != "F" {
        return Err(AnalysisResultError::InvalidFormat(
            "尾记录列数或类型错误".into(),
        ));
    }
    Ok(Footer {
        member_count: parse_u64(fields[1], "尾记录成员数")?,
        sha256: parse_sha256(fields[2])?,
    })
}

struct Footer {
    member_count: u64,
    sha256: [u8; 32],
}

/// 解析结果文件中的精确、图片和视频枚举。
fn parse_result_kind(value: &str) -> Result<AnalysisResultGroupKind, AnalysisResultError> {
    match value {
        "exact" => Ok(AnalysisResultGroupKind::Exact),
        "image" => Ok(AnalysisResultGroupKind::Image),
        "video" => Ok(AnalysisResultGroupKind::Video),
        _ => Err(AnalysisResultError::InvalidFormat("分组类型无效".into())),
    }
}

/// 将结果文件分组枚举转换成 Node Store 使用的分组枚举。
fn result_kind_to_store(kind: AnalysisResultGroupKind) -> GroupKind {
    match kind {
        AnalysisResultGroupKind::Exact => GroupKind::Exact,
        AnalysisResultGroupKind::Image => GroupKind::Image,
        AnalysisResultGroupKind::Video => GroupKind::Video,
    }
}

/// 计算安全的半开窗口边界，允许从总数末尾请求空窗口。
fn window_bounds(
    start: u64,
    count: u64,
    total: u64,
) -> Result<(usize, usize), AnalysisResultError> {
    let end = start.checked_add(count).unwrap_or(u64::MAX).min(total);
    let from = usize::try_from(start.min(total))
        .map_err(|_| AnalysisResultError::InvalidFormat("窗口起点超出平台范围".into()))?;
    let to = usize::try_from(end)
        .map_err(|_| AnalysisResultError::InvalidFormat("窗口终点超出平台范围".into()))?;
    Ok((from, to))
}

/// 文本字段不能含 TSV 分隔符或换行。
fn ensure_text(value: &str, field: &str) -> Result<(), AnalysisResultError> {
    if value.contains(['\t', '\r', '\n']) {
        return Err(AnalysisResultError::InvalidFormat(format!(
            "{field} 包含 TSV 控制字符"
        )));
    }
    Ok(())
}

/// 解析严格的小写 MD5 与文件大小。
fn parse_content_key(md5: &str, size: &str) -> Result<ContentKey, AnalysisResultError> {
    if md5.len() != 32
        || !md5
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(AnalysisResultError::InvalidFormat(
            "MD5 必须是小写十六进制".into(),
        ));
    }
    let mut bytes = [0_u8; 16];
    for (index, pair) in md5.as_bytes().chunks_exact(2).enumerate() {
        bytes[index] = (hex_nibble(pair[0])? << 4) | hex_nibble(pair[1])?;
    }
    Ok(ContentKey::new(bytes, parse_u64(size, "文件大小")?))
}

/// 解析一个十六进制半字节。
fn hex_nibble(byte: u8) -> Result<u8, AnalysisResultError> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        _ => Err(AnalysisResultError::InvalidFormat(
            "十六进制字符无效".into(),
        )),
    }
}

/// 解析无符号整数。
fn parse_u64(value: &str, field: &str) -> Result<u64, AnalysisResultError> {
    value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))
}

/// 解析无符号 32 位整数。
fn parse_u32(value: &str, field: &str) -> Result<u32, AnalysisResultError> {
    value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))
}

/// 解析无符号 16 位整数。
fn parse_u16(value: &str, field: &str) -> Result<u16, AnalysisResultError> {
    value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))
}

/// 解析无符号 8 位整数。
fn parse_u8(value: &str, field: &str) -> Result<u8, AnalysisResultError> {
    value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))
}

/// 解析有限 f32。
fn parse_f32(value: &str, field: &str) -> Result<f32, AnalysisResultError> {
    let parsed: f32 = value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))?;
    parsed
        .is_finite()
        .then_some(parsed)
        .ok_or_else(|| AnalysisResultError::InvalidFormat(format!("{field} 必须是有限数值")))
}

/// 解析有限 f64。
fn parse_f64(value: &str, field: &str) -> Result<f64, AnalysisResultError> {
    let parsed: f64 = value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))?;
    parsed
        .is_finite()
        .then_some(parsed)
        .ok_or_else(|| AnalysisResultError::InvalidFormat(format!("{field} 必须是有限数值")))
}

/// 解析 F 行中的小写 SHA-256。
fn parse_sha256(value: &str) -> Result<[u8; 32], AnalysisResultError> {
    if value.len() != 64
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(AnalysisResultError::InvalidFormat(
            "SHA-256 必须是小写十六进制".into(),
        ));
    }
    let mut output = [0_u8; 32];
    for (index, pair) in value.as_bytes().chunks_exact(2).enumerate() {
        output[index] = (hex_nibble(pair[0])? << 4) | hex_nibble(pair[1])?;
    }
    Ok(output)
}

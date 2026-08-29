//! 最近一次本地分析结果的版本化 TSV 发布与校验边界。

use std::{
    collections::BTreeSet,
    fs::{self, File},
    io::{self, BufWriter, Write},
    path::{Path, PathBuf},
};

use dedup_core::{AnalysisRunId, ContentKey, LocationKey, MachineId, NormalizedPath, Thresholds};
use dedup_windows::atomic_replace_file;
use sha2::{Digest, Sha256};
use thiserror::Error;
use uuid::Uuid;

const FORMAT_VERSION: u32 = 1;
const PARTIAL_FILE_NAME: &str = "latest-analysis.partial.tsv";
const RESULT_FILE_NAME: &str = "latest-analysis.result.tsv";

/// 分析结果 TSV 的可恢复错误。
#[derive(Debug, Error)]
pub enum AnalysisResultError {
    /// 创建、写入、同步或替换结果文件失败。
    #[error("分析结果文件操作失败: {0}")]
    Io(#[from] io::Error),
    /// 头记录字段不符合固定格式。
    #[error("分析结果头无效: {0}")]
    InvalidHeader(String),
    /// 成员记录字段不符合固定格式。
    #[error("分析结果成员无效: {0}")]
    InvalidRow(String),
    /// 已发布文件无法通过固定格式或摘要校验。
    #[error("分析结果文件格式无效: {0}")]
    InvalidFormat(String),
}

/// 当前结果所属的分析模式。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisResultMode {
    /// 节点本机分析。
    Local,
    /// 中心跨机器分析。
    Central,
}

impl AnalysisResultMode {
    /// 返回固定 TSV 枚举文本。
    const fn as_str(self) -> &'static str {
        match self {
            Self::Local => "local",
            Self::Central => "central",
        }
    }

    /// 解析固定 TSV 枚举文本。
    fn parse(value: &str) -> Option<Self> {
        match value {
            "local" => Some(Self::Local),
            "central" => Some(Self::Central),
            _ => None,
        }
    }
}

/// 结果中重复组的判定种类。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AnalysisResultGroupKind {
    /// MD5 与文件大小完全相同。
    Exact,
    /// 图片两层相似判定通过。
    Image,
    /// 视频两层相似判定通过。
    Video,
}

impl AnalysisResultGroupKind {
    /// 返回固定 TSV 枚举文本。
    const fn as_str(self) -> &'static str {
        match self {
            Self::Exact => "exact",
            Self::Image => "image",
            Self::Video => "video",
        }
    }

    /// 解析固定 TSV 枚举文本。
    fn parse(value: &str) -> Option<Self> {
        match value {
            "exact" => Some(Self::Exact),
            "image" => Some(Self::Image),
            "video" => Some(Self::Video),
            _ => None,
        }
    }
}

/// 一份结果文件开头固定保存的分析快照。
#[derive(Clone, Debug, PartialEq)]
pub struct AnalysisResultHeader {
    /// TSV 格式版本；当前固定为 1。
    pub format_version: u32,
    /// 本次分析运行的进程内标识。
    pub analysis_id: AnalysisRunId,
    /// 冻结分析输入时的文件库版本。
    pub library_revision: u64,
    /// 本机或中心分析。
    pub analysis_mode: AnalysisResultMode,
    /// 创建结果的 Unix 毫秒。
    pub created_at_ms: u64,
    /// 完整冻结的九项筛选阈值。
    pub thresholds: Thresholds,
}

/// 一条可独立显示的最终重复组成员记录。
#[derive(Clone, Debug, PartialEq)]
pub struct AnalysisResultRow {
    /// 精确、图片或视频分组。
    pub group_kind: AnalysisResultGroupKind,
    /// 稳定分组 ID。
    pub group_id: String,
    /// 当前成员是否是代表位置。
    pub representative: bool,
    /// 组代表内容键。
    pub representative_content: ContentKey,
    /// 成员位置键。
    pub location: LocationKey,
    /// 原始显示路径；不在读取时反查 SQLite。
    pub display_path: String,
    /// 成员内容键。
    pub content: ContentKey,
    /// 与代表直接比较的一筛得分。
    pub stage1_score: f64,
    /// 可选的通过 pHash 分块数。
    pub phash_passed_parts: Option<u8>,
    /// 可选的联合二筛得分。
    pub stage2_score: Option<f64>,
}

/// 成功原子发布后可供 UI 安装的结果元数据。
#[derive(Clone, Debug, PartialEq)]
pub struct PublishedAnalysisResult {
    /// 唯一的最近成功结果路径。
    pub path: PathBuf,
    /// 本次分析运行的进程内标识。
    pub run_id: AnalysisRunId,
    /// 冻结分析输入时的文件库版本。
    pub library_revision: u64,
    /// 已发布的头记录。
    pub header: AnalysisResultHeader,
    /// 已发布成员行数。
    pub member_count: u64,
    /// 已发布成员所属的唯一分组数。
    pub group_count: u64,
    /// H/M 全部字节的 SHA-256。
    pub sha256: [u8; 32],
}

/// 通过格式和摘要校验后得到的结果元数据。
#[derive(Clone, Debug, PartialEq)]
pub struct VerifiedAnalysisResult {
    /// 本次分析运行的进程内标识。
    pub run_id: AnalysisRunId,
    /// 冻结分析输入时的文件库版本。
    pub library_revision: u64,
    /// 已校验的头记录。
    pub header: AnalysisResultHeader,
    /// 已校验成员行数。
    pub member_count: u64,
    /// 已校验成员所属的唯一分组数。
    pub group_count: u64,
    /// 已校验的 H/M 字节 SHA-256。
    pub sha256: [u8; 32],
}

/// 仅在当前分析成功前持有 partial 文件的写入器。
pub struct AnalysisResultWriter {
    partial_path: PathBuf,
    result_path: PathBuf,
    header: AnalysisResultHeader,
    writer: Option<BufWriter<File>>,
    hasher: Sha256,
    member_count: u64,
    group_ids: BTreeSet<String>,
    finished: bool,
}

impl AnalysisResultWriter {
    /// 创建或截断固定 partial 文件，并写入 H 记录但绝不触碰旧 result 文件。
    pub fn begin(
        results_root: &Path,
        header: &AnalysisResultHeader,
    ) -> Result<Self, AnalysisResultError> {
        validate_header(header).map_err(AnalysisResultError::InvalidHeader)?;
        if !results_root.is_absolute() {
            return Err(AnalysisResultError::InvalidHeader(
                "结果目录必须是绝对路径".into(),
            ));
        }
        fs::create_dir_all(results_root)?;
        let partial_path = results_root.join(PARTIAL_FILE_NAME);
        let result_path = results_root.join(RESULT_FILE_NAME);
        let mut writer = Self {
            partial_path,
            result_path,
            header: header.clone(),
            writer: Some(BufWriter::new(File::create(
                results_root.join(PARTIAL_FILE_NAME),
            )?)),
            hasher: Sha256::new(),
            member_count: 0,
            group_ids: BTreeSet::new(),
            finished: false,
        };
        let line = encode_header(header);
        if let Err(error) = writer.write_hashed_line(&line) {
            writer.close_and_remove_partial();
            return Err(error);
        }
        Ok(writer)
    }

    /// 追加一条 M 记录；字段非法或写入失败会删除当前 exact partial 文件。
    pub fn write_member(&mut self, row: &AnalysisResultRow) -> Result<(), AnalysisResultError> {
        let line = match encode_member(row) {
            Ok(line) => line,
            Err(error) => {
                self.close_and_remove_partial();
                return Err(error);
            }
        };
        if let Err(error) = self.write_hashed_line(&line) {
            self.close_and_remove_partial();
            return Err(error);
        }
        self.member_count += 1;
        self.group_ids.insert(row.group_id.clone());
        Ok(())
    }

    /// 写 F、同步并关闭 partial 后原子替换固定 result 文件。
    pub fn publish(self) -> Result<PublishedAnalysisResult, AnalysisResultError> {
        self.publish_with_verifier(|_| Ok::<(), AnalysisResultError>(()))
            .map(|(published, _)| published)
    }

    /// 在原子替换前验证并返回验证阶段构造的附加值。
    pub fn publish_with_verifier<T>(
        self,
        verifier: impl FnOnce(&Path) -> Result<T, AnalysisResultError>,
    ) -> Result<(PublishedAnalysisResult, T), AnalysisResultError> {
        self.publish_with_verifier_and_replacer(verifier, |source, destination, _| {
            atomic_replace_file(source, destination).map_err(AnalysisResultError::Io)
        })
    }

    /// 在验证后通过指定原子边界发布，并把验证对象直接返回给调用者。
    pub fn publish_with_verifier_and_replacer<T>(
        mut self,
        verifier: impl FnOnce(&Path) -> Result<T, AnalysisResultError>,
        replacer: impl FnOnce(&Path, &Path, &T) -> Result<(), AnalysisResultError>,
    ) -> Result<(PublishedAnalysisResult, T), AnalysisResultError> {
        let sha256: [u8; 32] = self.hasher.clone().finalize().into();
        let footer = format!("F\t{}\t{}\n", self.member_count, hex_bytes(&sha256));
        let verified = (|| {
            let mut writer = self
                .writer
                .take()
                .ok_or_else(|| AnalysisResultError::InvalidFormat("结果写入器已经关闭".into()))?;
            writer.write_all(footer.as_bytes())?;
            writer.flush()?;
            writer.get_ref().sync_all()?;
            drop(writer);
            let verified = verifier(&self.partial_path)?;
            replacer(&self.partial_path, &self.result_path, &verified)?;
            Ok::<T, AnalysisResultError>(verified)
        })();
        let verified = match verified {
            Ok(verified) => verified,
            Err(error) => {
                self.close_and_remove_partial();
                return Err(error);
            }
        };
        self.finished = true;
        Ok((
            PublishedAnalysisResult {
                path: self.result_path.clone(),
                run_id: self.header.analysis_id,
                library_revision: self.header.library_revision,
                header: self.header.clone(),
                member_count: self.member_count,
                group_count: self.group_ids.len() as u64,
                sha256,
            },
            verified,
        ))
    }

    /// 取消或失败时只删除本次固定 partial 文件，保留上一次 result 文件。
    pub fn discard(mut self) -> Result<(), AnalysisResultError> {
        self.writer.take();
        remove_file_if_exists(&self.partial_path)?;
        self.finished = true;
        Ok(())
    }

    /// 关闭当前句柄并删除本次唯一 partial；清理失败不覆盖原始错误。
    fn close_and_remove_partial(&mut self) {
        self.writer.take();
        let _ = remove_file_if_exists(&self.partial_path);
    }

    /// 将 H/M 记录写入文件并纳入 F 的摘要范围。
    fn write_hashed_line(&mut self, line: &str) -> Result<(), AnalysisResultError> {
        let writer = self
            .writer
            .as_mut()
            .ok_or_else(|| AnalysisResultError::InvalidFormat("结果写入器已经关闭".into()))?;
        writer.write_all(line.as_bytes())?;
        self.hasher.update(line.as_bytes());
        Ok(())
    }
}

impl Drop for AnalysisResultWriter {
    /// 调用方提前退出时尽力清理本次 partial，绝不触碰已发布 result。
    fn drop(&mut self) {
        if !self.finished {
            self.close_and_remove_partial();
        }
    }
}

/// 校验已发布 TSV 的 UTF-8、固定列、枚举、阈值、成员数和 F 之前字节摘要。
pub fn verify_result_file(path: &Path) -> Result<VerifiedAnalysisResult, AnalysisResultError> {
    let bytes = fs::read(path)?;
    if bytes.starts_with(&[0xef, 0xbb, 0xbf]) {
        return Err(AnalysisResultError::InvalidFormat(
            "不允许 UTF-8 BOM".into(),
        ));
    }
    let text = std::str::from_utf8(&bytes)
        .map_err(|_| AnalysisResultError::InvalidFormat("文件必须是 UTF-8".into()))?;
    if !text.ends_with('\n') || text.contains('\r') {
        return Err(AnalysisResultError::InvalidFormat(
            "记录必须使用 LF 换行".into(),
        ));
    }
    let lines = text
        .strip_suffix('\n')
        .unwrap_or_default()
        .split('\n')
        .collect::<Vec<_>>();
    if lines.len() < 2 {
        return Err(AnalysisResultError::InvalidFormat("缺少头或尾记录".into()));
    }
    let header = parse_header(lines[0])?;
    let footer = lines
        .last()
        .copied()
        .unwrap_or_default()
        .split('\t')
        .collect::<Vec<_>>();
    if footer.len() != 3 || footer[0] != "F" {
        return Err(AnalysisResultError::InvalidFormat(
            "尾记录列数或类型错误".into(),
        ));
    }
    let member_count = parse_u64(footer[1], "尾记录成员数")?;
    let expected_sha256 = parse_sha256(footer[2])?;
    let actual_members = lines.len().saturating_sub(2) as u64;
    if member_count != actual_members {
        return Err(AnalysisResultError::InvalidFormat(
            "尾记录成员数不匹配".into(),
        ));
    }
    let mut group_ids = BTreeSet::new();
    for line in &lines[1..lines.len() - 1] {
        group_ids.insert(parse_member(line)?.to_owned());
    }
    let footer_start = bytes
        .windows(3)
        .rposition(|window| window == b"\nF\t")
        .map(|index| index + 1)
        .ok_or_else(|| AnalysisResultError::InvalidFormat("找不到尾记录字节边界".into()))?;
    let actual_sha256: [u8; 32] = Sha256::digest(&bytes[..footer_start]).into();
    if actual_sha256 != expected_sha256 {
        return Err(AnalysisResultError::InvalidFormat(
            "尾记录 SHA-256 不匹配".into(),
        ));
    }
    Ok(VerifiedAnalysisResult {
        run_id: header.analysis_id,
        library_revision: header.library_revision,
        header,
        member_count,
        group_count: group_ids.len() as u64,
        sha256: actual_sha256,
    })
}

/// 删除精确文件，文件已经不存在时视为成功。
fn remove_file_if_exists(path: &Path) -> io::Result<()> {
    match fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

/// 以固定列顺序编码头记录。
fn encode_header(header: &AnalysisResultHeader) -> String {
    let thresholds = header.thresholds;
    format!(
        "H\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\n",
        header.format_version,
        header.analysis_id.as_uuid(),
        header.library_revision,
        header.analysis_mode.as_str(),
        header.created_at_ms,
        thresholds.pdq_quality_min,
        thresholds.aspect_tolerance,
        thresholds.pdq_hamming_max,
        thresholds.phash_part_hamming_max,
        thresholds.phash_min_passed_parts,
        thresholds.sobel_min,
        thresholds.video_min_valid_frames,
        thresholds.video_stage1_min,
        thresholds.video_stage2_min,
    )
}

/// 验证并以固定列顺序编码一个成员记录。
fn encode_member(row: &AnalysisResultRow) -> Result<String, AnalysisResultError> {
    ensure_text(&row.group_id, "组 ID")?;
    if row.group_id.is_empty() {
        return Err(AnalysisResultError::InvalidRow("组 ID 不能为空".into()));
    }
    ensure_text(&row.display_path, "显示路径")?;
    ensure_finite(row.stage1_score, "一筛分数")?;
    if let Some(score) = row.stage2_score {
        ensure_finite(score, "二筛分数")?;
    }
    if row
        .phash_passed_parts
        .is_some_and(|value| !(1..=9).contains(&value))
    {
        return Err(AnalysisResultError::InvalidRow(
            "pHash 通过块数必须位于 1..=9".into(),
        ));
    }
    Ok(format!(
        "M\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\t{}\n",
        row.group_kind.as_str(),
        row.group_id,
        u8::from(row.representative),
        md5_hex(row.representative_content),
        row.representative_content.file_size(),
        row.location.machine_id().as_str(),
        row.location.normalized_path().as_str(),
        row.display_path,
        md5_hex(row.content),
        row.content.file_size(),
        row.stage1_score,
        row.phash_passed_parts
            .map_or_else(String::new, |value| value.to_string()),
        row.stage2_score
            .map_or_else(String::new, |value| value.to_string()),
    ))
}

/// 解析并验证 H 固定列。
fn parse_header(line: &str) -> Result<AnalysisResultHeader, AnalysisResultError> {
    let fields = line.split('\t').collect::<Vec<_>>();
    if fields.len() != 15 || fields[0] != "H" {
        return Err(AnalysisResultError::InvalidFormat(
            "头记录列数或类型错误".into(),
        ));
    }
    let format_version = parse_u32(fields[1], "格式版本")?;
    if format_version != FORMAT_VERSION {
        return Err(AnalysisResultError::InvalidFormat(
            "不支持的格式版本".into(),
        ));
    }
    let analysis_id = Uuid::parse_str(fields[2])
        .map(AnalysisRunId::from_uuid)
        .map_err(|_| AnalysisResultError::InvalidFormat("分析 ID 无效".into()))?;
    let header = AnalysisResultHeader {
        format_version,
        analysis_id,
        library_revision: parse_u64(fields[3], "文件库版本")?,
        analysis_mode: AnalysisResultMode::parse(fields[4])
            .ok_or_else(|| AnalysisResultError::InvalidFormat("分析模式无效".into()))?,
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
    validate_header(&header).map_err(AnalysisResultError::InvalidFormat)?;
    Ok(header)
}

/// 验证 M 固定列，结果读取方后续可在不加载全部行时复用该校验。
fn parse_member(line: &str) -> Result<&str, AnalysisResultError> {
    let fields = line.split('\t').collect::<Vec<_>>();
    if fields.len() != 14 || fields[0] != "M" {
        return Err(AnalysisResultError::InvalidFormat(
            "成员记录列数或类型错误".into(),
        ));
    }
    AnalysisResultGroupKind::parse(fields[1])
        .ok_or_else(|| AnalysisResultError::InvalidFormat("分组类型无效".into()))?;
    ensure_format_text(fields[2], "组 ID")?;
    if fields[2].is_empty() {
        return Err(AnalysisResultError::InvalidFormat("组 ID 不能为空".into()));
    }
    if !matches!(fields[3], "0" | "1") {
        return Err(AnalysisResultError::InvalidFormat("代表标记无效".into()));
    }
    parse_content_key(fields[4], fields[5])?;
    let machine = MachineId::parse(fields[6])
        .map_err(|_| AnalysisResultError::InvalidFormat("机器 ID 无效".into()))?;
    let normalized = NormalizedPath::new(fields[7])
        .map_err(|_| AnalysisResultError::InvalidFormat("规范路径无效".into()))?;
    let _location = LocationKey::new(machine, normalized);
    ensure_format_text(fields[8], "显示路径")?;
    parse_content_key(fields[9], fields[10])?;
    parse_f64(fields[11], "一筛分数")?;
    if !fields[12].is_empty() && !(1..=9).contains(&parse_u8(fields[12], "pHash 通过块数")?) {
        return Err(AnalysisResultError::InvalidFormat(
            "pHash 通过块数必须位于 1..=9".into(),
        ));
    }
    if !fields[13].is_empty() {
        parse_f64(fields[13], "二筛分数")?;
    }
    Ok(fields[2])
}

/// 验证头记录的固定格式版本和九项阈值。
fn validate_header(header: &AnalysisResultHeader) -> Result<(), String> {
    if header.format_version != FORMAT_VERSION {
        return Err("格式版本必须为 1".into());
    }
    header
        .thresholds
        .validate()
        .map_err(|error| format!("阈值无效: {error}"))
}

/// 文本字段不能带入 TSV 分隔符或换行。
fn ensure_text(value: &str, field: &str) -> Result<(), AnalysisResultError> {
    if value.contains(['\t', '\r', '\n']) {
        return Err(AnalysisResultError::InvalidRow(format!(
            "{field} 不能包含制表符或换行"
        )));
    }
    Ok(())
}

/// 读取文件时对应的文本字段校验。
fn ensure_format_text(value: &str, field: &str) -> Result<(), AnalysisResultError> {
    if value.contains(['\t', '\r', '\n']) {
        return Err(AnalysisResultError::InvalidFormat(format!(
            "{field} 包含 TSV 控制字符"
        )));
    }
    Ok(())
}

/// 拒绝 NaN 和无穷，保证固定文本能够可靠往返。
fn ensure_finite(value: f64, field: &str) -> Result<(), AnalysisResultError> {
    if value.is_finite() {
        Ok(())
    } else {
        Err(AnalysisResultError::InvalidRow(format!(
            "{field} 必须是有限数值"
        )))
    }
}

/// 把内容键编码为小写 MD5 文本和独立大小列。
fn md5_hex(key: ContentKey) -> String {
    hex_bytes(&key.md5())
}

/// 把字节编码为小写十六进制。
fn hex_bytes(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut value = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        value.push(HEX[(byte >> 4) as usize] as char);
        value.push(HEX[(byte & 0x0f) as usize] as char);
    }
    value
}

/// 解析严格的小写 32 位 MD5 与文件大小。
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

/// 解析一个已经经小写检查的十六进制半字节。
fn hex_nibble(byte: u8) -> Result<u8, AnalysisResultError> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        _ => Err(AnalysisResultError::InvalidFormat(
            "MD5 十六进制无效".into(),
        )),
    }
}

/// 解析无符号 64 位固定文本。
fn parse_u64(value: &str, field: &str) -> Result<u64, AnalysisResultError> {
    value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))
}

/// 解析无符号 32 位固定文本。
fn parse_u32(value: &str, field: &str) -> Result<u32, AnalysisResultError> {
    value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))
}

/// 解析无符号 16 位固定文本。
fn parse_u16(value: &str, field: &str) -> Result<u16, AnalysisResultError> {
    value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))
}

/// 解析无符号 8 位固定文本。
fn parse_u8(value: &str, field: &str) -> Result<u8, AnalysisResultError> {
    value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))
}

/// 解析有限的可往返 f32 阈值文本。
fn parse_f32(value: &str, field: &str) -> Result<f32, AnalysisResultError> {
    let parsed: f32 = value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))?;
    if parsed.is_finite() {
        Ok(parsed)
    } else {
        Err(AnalysisResultError::InvalidFormat(format!(
            "{field} 必须是有限数值"
        )))
    }
}

/// 解析有限的可往返 f64 分数文本。
fn parse_f64(value: &str, field: &str) -> Result<f64, AnalysisResultError> {
    let parsed: f64 = value
        .parse()
        .map_err(|_| AnalysisResultError::InvalidFormat(format!("{field} 无效")))?;
    if parsed.is_finite() {
        Ok(parsed)
    } else {
        Err(AnalysisResultError::InvalidFormat(format!(
            "{field} 必须是有限数值"
        )))
    }
}

/// 解析 F 行固定保存的 32 字节 SHA-256。
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

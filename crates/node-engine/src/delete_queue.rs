//! 当前 Node 进程专用的逐项删除 TSV 队列。
//!
//! 队列只承担本次删除的顺序和原位状态，不承担恢复、历史或数据库事实。
//! 文件系统删除成功后，调用方必须先完成 SQLite ACK，才能把对应行从 `P` 改为 `C`。

use std::{
    collections::BTreeSet,
    fs::{self, File, OpenOptions},
    io::{self, Read, Seek, SeekFrom, Write},
    path::{Path, PathBuf},
};

use dedup_core::{ContentKey, DeleteMode, LocationKey, MachineId, NormalizedPath};
use dedup_node_store::PlannedDeleteItem;

const QUEUE_FILE_NAME: &str = "delete.tasks.tsv";

/// 删除队列一行的原位状态。
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum DeleteQueueStatus {
    /// 文件尚未完成删除和 SQLite ACK。
    Pending,
    /// 文件删除成功且 SQLite 已确认当前事实。
    Completed,
    /// 文件身份变化、缺失或文件系统删除失败。
    Failed,
}

impl DeleteQueueStatus {
    /// 把状态编码为 TSV 行首的单字节标记。
    const fn as_byte(self) -> u8 {
        match self {
            Self::Pending => b'P',
            Self::Completed => b'C',
            Self::Failed => b'F',
        }
    }

    /// 从 TSV 行首读取固定状态标记。
    fn parse(byte: u8) -> io::Result<Self> {
        match byte {
            b'P' => Ok(Self::Pending),
            b'C' => Ok(Self::Completed),
            b'F' => Ok(Self::Failed),
            _ => Err(invalid_data("删除队列状态必须是 P、C 或 F")),
        }
    }
}

/// 一项已从 TSV 解码的删除工作，包含该行冻结的处理模式。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeleteQueueItem {
    /// 已校验的删除身份，供调用方逐项重验和提交。
    pub item: PlannedDeleteItem,
    /// 本行要求使用的回收站或永久删除模式。
    pub mode: DeleteMode,
}

#[derive(Clone, Debug)]
struct QueueRecord {
    item: PlannedDeleteItem,
    mode: DeleteMode,
    status: DeleteQueueStatus,
    status_offset: u64,
}

/// 当前进程内顺序读取和原位确认删除队列。
pub struct TransientDeleteQueue {
    runtime_root: PathBuf,
    run_dir: PathBuf,
    queue_path: PathBuf,
    file: Option<File>,
    records: Vec<QueueRecord>,
    next_index: usize,
    in_flight: Option<usize>,
}

impl TransientDeleteQueue {
    /// 在 `runtime_root/<run_id>` 精确子目录新建 UTF-8、LF、无 BOM 的删除 TSV。
    ///
    /// 传入项会先完成字段与重复身份校验，写入并 `sync_all` 后再从文件重新解析，
    /// 因此队列实际调度来源始终是 TSV，而不是调用方保留的原始 Vec。
    pub fn create_new(
        runtime_root: &Path,
        run_id: &str,
        mode: DeleteMode,
        items: &[PlannedDeleteItem],
    ) -> io::Result<Self> {
        validate_runtime_root(runtime_root)?;
        validate_run_id(run_id)?;
        validate_items(items)?;
        if items.is_empty() {
            return Err(invalid_input("删除队列不能为空"));
        }

        let run_dir = runtime_root.join(run_id);
        if run_dir.exists() {
            return Err(invalid_input("删除队列运行目录已经存在"));
        }
        fs::create_dir(&run_dir)?;
        if let Err(error) = validate_exact_run_directory(runtime_root, &run_dir, run_id)
            .and_then(|()| write_queue_file(&run_dir, mode, items))
        {
            let _ = fs::remove_dir_all(&run_dir);
            return Err(error);
        }

        match Self::open_existing(runtime_root, run_id) {
            Ok(queue) => Ok(queue),
            Err(error) => {
                let _ = fs::remove_dir_all(&run_dir);
                Err(error)
            }
        }
    }

    /// 打开当前 runtime 根下指定运行目录中的队列，并完整校验每一行。
    ///
    /// 该方法只用于当前进程刚创建的队列或测试读取；它不会恢复旧运行，调用方不应在
    /// Node 启动时扫描 runtime 根目录寻找队列。
    pub fn open_existing(runtime_root: &Path, run_id: &str) -> io::Result<Self> {
        validate_runtime_root(runtime_root)?;
        validate_run_id(run_id)?;
        let run_dir = runtime_root.join(run_id);
        validate_exact_run_directory(runtime_root, &run_dir, run_id)?;
        let queue_path = run_dir.join(QUEUE_FILE_NAME);
        validate_regular_file(&queue_path, "删除队列文件")?;

        let mut file = OpenOptions::new()
            .read(true)
            .write(true)
            .open(&queue_path)?;
        let mut bytes = Vec::new();
        file.read_to_end(&mut bytes)?;
        let records = parse_queue(&bytes)?;
        if records.is_empty() {
            return Err(invalid_data("删除队列没有任何项目"));
        }
        file.seek(SeekFrom::Start(0))?;

        Ok(Self {
            runtime_root: runtime_root.to_path_buf(),
            run_dir,
            queue_path,
            file: Some(file),
            records,
            next_index: 0,
            in_flight: None,
        })
    }

    /// 返回删除 TSV 的绝对路径；调用方可据此记录当前进程诊断信息。
    pub fn path(&self) -> &Path {
        &self.queue_path
    }

    /// 返回当前删除运行的精确目录。
    pub fn run_dir(&self) -> &Path {
        &self.run_dir
    }

    /// 返回队列创建时绑定的 runtime 根目录。
    pub fn runtime_root(&self) -> &Path {
        &self.runtime_root
    }

    /// 返回 TSV 中的行数，不代表已完成数量。
    pub fn len(&self) -> usize {
        self.records.len()
    }

    /// 返回队列是否没有任何行。
    pub fn is_empty(&self) -> bool {
        self.records.is_empty()
    }

    /// 把缓冲内容同步到文件后端，供创建或外部诊断显式调用。
    pub fn sync(&mut self) -> io::Result<()> {
        self.file_mut()?.sync_all()
    }

    /// 按冻结顺序领取下一项；未 ACK 的当前 `P` 项会重复返回而不会跳过。
    ///
    /// 这使 SQLite ACK 失败时可以保持 `P`，调用方不会误把未提交结果当成完成。
    pub fn next_pending(&mut self) -> io::Result<Option<PlannedDeleteItem>> {
        Ok(self.next_pending_entry()?.map(|entry| entry.item))
    }

    /// 按冻结顺序领取下一项，并同时返回行内删除模式。
    pub fn next_pending_entry(&mut self) -> io::Result<Option<DeleteQueueItem>> {
        if let Some(index) = self.in_flight {
            let record = self
                .records
                .get(index)
                .ok_or_else(|| invalid_data("删除队列在途索引越界"))?;
            if record.status != DeleteQueueStatus::Pending {
                return Err(invalid_data("删除队列在途行不是 P 状态"));
            }
            return Ok(Some(DeleteQueueItem {
                item: record.item.clone(),
                mode: record.mode,
            }));
        }

        while let Some(record) = self.records.get(self.next_index) {
            if record.status == DeleteQueueStatus::Pending {
                let index = self.next_index;
                self.in_flight = Some(index);
                return Ok(Some(DeleteQueueItem {
                    item: record.item.clone(),
                    mode: record.mode,
                }));
            }
            self.next_index += 1;
        }
        Ok(None)
    }

    /// 返回一项当前的原位状态。
    pub fn status(&self, item_id: &str) -> io::Result<Option<DeleteQueueStatus>> {
        Ok(self
            .records
            .iter()
            .find(|record| record.item.item_id == item_id)
            .map(|record| record.status))
    }

    /// 文件失败或身份复核跳过时立即把当前在途行从 `P` 改成 `F`。
    pub fn mark_failed(&mut self, item_id: &str) -> io::Result<()> {
        self.complete_in_flight(item_id, DeleteQueueStatus::Failed)
    }

    /// `mark_failed` 的语义别名，明确表示这是队列侧的失败 ACK。
    pub fn ack_failed(&mut self, item_id: &str) -> io::Result<()> {
        self.mark_failed(item_id)
    }

    /// 只有调用方完成 SQLite 单项提交后，才能把当前在途行从 `P` 改成 `C`。
    pub fn ack_sqlite(&mut self, item_id: &str) -> io::Result<()> {
        self.complete_in_flight(item_id, DeleteQueueStatus::Completed)
    }

    /// 所有行变成 `C/F` 后删除本批精确 runtime 子目录，不删除 runtime 根或其他运行。
    pub fn cleanup(&mut self) -> io::Result<()> {
        if self
            .records
            .iter()
            .any(|record| record.status == DeleteQueueStatus::Pending)
        {
            return Err(invalid_input("删除队列仍有未 ACK 的 P 行"));
        }
        validate_runtime_root(&self.runtime_root)?;
        validate_exact_run_directory(&self.runtime_root, &self.run_dir, &run_id(&self.run_dir))?;
        self.file_mut()?.sync_all()?;
        let file = self
            .file
            .take()
            .ok_or_else(|| invalid_input("删除队列已经清理"))?;
        drop(file);
        fs::remove_dir_all(&self.run_dir)
    }

    fn complete_in_flight(&mut self, item_id: &str, target: DeleteQueueStatus) -> io::Result<()> {
        let index = self
            .in_flight
            .ok_or_else(|| invalid_input("删除队列没有可确认的在途项"))?;
        let record = self
            .records
            .get(index)
            .ok_or_else(|| invalid_data("删除队列在途索引越界"))?;
        if record.item.item_id != item_id {
            return Err(invalid_input("删除 ACK 不匹配当前在途项"));
        }
        if record.status != DeleteQueueStatus::Pending {
            return Err(invalid_input("只有 P 状态可以 ACK"));
        }
        let offset = record.status_offset;
        self.write_status(offset, target.as_byte())?;
        self.records[index].status = target;
        self.in_flight = None;
        self.next_index = index + 1;
        Ok(())
    }

    fn file_mut(&mut self) -> io::Result<&mut File> {
        self.file
            .as_mut()
            .ok_or_else(|| invalid_input("删除队列已经清理"))
    }

    fn write_status(&mut self, offset: u64, value: u8) -> io::Result<()> {
        let file = self.file_mut()?;
        file.seek(SeekFrom::Start(offset))?;
        file.write_all(&[value])?;
        file.sync_data()
    }
}

fn write_queue_file(
    run_dir: &Path,
    mode: DeleteMode,
    items: &[PlannedDeleteItem],
) -> io::Result<()> {
    let path = run_dir.join(QUEUE_FILE_NAME);
    let mut bytes = Vec::new();
    for item in items {
        append_item_line(&mut bytes, mode, item)?;
    }
    let mut file = OpenOptions::new().write(true).create_new(true).open(path)?;
    file.write_all(&bytes)?;
    file.sync_all()
}

fn append_item_line(
    output: &mut Vec<u8>,
    mode: DeleteMode,
    item: &PlannedDeleteItem,
) -> io::Result<()> {
    validate_item(item)?;
    let machine = item.location.machine_id().as_str();
    let path = item.location.normalized_path().as_str();
    let md5 = encode_hex(&item.expected.md5());
    let mode = mode_name(mode);
    let line = format!(
        "P\t{}\t{}\t{}\t{}\t{}\t{}\t{}\n",
        item.item_id,
        item.group_id,
        machine,
        path,
        md5,
        item.expected.file_size(),
        mode,
    );
    output.extend_from_slice(line.as_bytes());
    Ok(())
}

fn parse_queue(bytes: &[u8]) -> io::Result<Vec<QueueRecord>> {
    if bytes.starts_with(&[0xef, 0xbb, 0xbf]) {
        return Err(invalid_data("删除队列不能包含 UTF-8 BOM"));
    }
    if bytes.is_empty() || bytes.contains(&b'\r') {
        return Err(invalid_data("删除队列必须是非空 UTF-8 LF 文件"));
    }
    let text = std::str::from_utf8(bytes).map_err(|_| invalid_data("删除队列不是 UTF-8"))?;
    let mut records = Vec::new();
    let mut item_ids = BTreeSet::new();
    let mut locations = BTreeSet::new();
    let mut offset = 0u64;
    for raw_line in text.split_inclusive('\n') {
        if !raw_line.ends_with('\n') {
            return Err(invalid_data("删除队列每行必须以 LF 结束"));
        }
        let line = &raw_line[..raw_line.len() - 1];
        let fields = line.split('\t').collect::<Vec<_>>();
        if fields.len() != 8 || fields.iter().any(|field| field.contains(['\r', '\n'])) {
            return Err(invalid_data("删除队列每行必须严格包含 8 列"));
        }
        let status = fields[0].as_bytes();
        if status.len() != 1 {
            return Err(invalid_data("删除队列状态必须是单字节"));
        }
        let status = DeleteQueueStatus::parse(status[0])?;
        let item_id = parse_text_field(fields[1], "item ID")?;
        let group_id = parse_text_field(fields[2], "group ID")?;
        let machine_id =
            MachineId::parse(fields[3]).map_err(|_| invalid_data("删除队列机器 ID 无效"))?;
        let normalized_path =
            NormalizedPath::new(fields[4]).map_err(|_| invalid_data("删除队列规范路径无效"))?;
        if normalized_path.as_str() != fields[4] {
            return Err(invalid_data("删除队列规范路径必须已经规范化"));
        }
        let md5 = parse_md5(fields[5])?;
        let file_size = fields[6]
            .parse::<u64>()
            .map_err(|_| invalid_data("删除队列文件大小无效"))?;
        let mode = parse_mode(fields[7])?;
        if !item_ids.insert(item_id.to_owned()) {
            return Err(invalid_data("删除队列包含重复 item ID"));
        }
        let location = (
            machine_id.as_str().to_owned(),
            normalized_path.as_str().to_owned(),
        );
        if !locations.insert(location) {
            return Err(invalid_data("删除队列包含重复删除身份"));
        }
        records.push(QueueRecord {
            item: PlannedDeleteItem {
                item_id: item_id.to_owned(),
                group_id: group_id.to_owned(),
                location: LocationKey::new(machine_id, normalized_path),
                expected: ContentKey::new(md5, file_size),
            },
            mode,
            status,
            status_offset: offset,
        });
        offset += raw_line.len() as u64;
    }
    if !bytes.ends_with(b"\n") {
        return Err(invalid_data("删除队列最后一行必须以 LF 结束"));
    }
    Ok(records)
}

fn validate_items(items: &[PlannedDeleteItem]) -> io::Result<()> {
    let mut item_ids = BTreeSet::new();
    let mut locations = BTreeSet::new();
    for item in items {
        validate_item(item)?;
        if !item_ids.insert(item.item_id.clone()) {
            return Err(invalid_input("删除队列包含重复 item ID"));
        }
        let location = (
            item.location.machine_id().as_str().to_owned(),
            item.location.normalized_path().as_str().to_owned(),
        );
        if !locations.insert(location) {
            return Err(invalid_input("删除队列包含重复删除身份"));
        }
    }
    Ok(())
}

fn validate_item(item: &PlannedDeleteItem) -> io::Result<()> {
    parse_text_field(&item.item_id, "item ID")?;
    parse_text_field(&item.group_id, "group ID")?;
    if item.location.machine_id().as_str().len() != 64 {
        return Err(invalid_input("删除队列机器 ID 无效"));
    }
    parse_text_field(item.location.normalized_path().as_str(), "规范路径")
        .map_err(|error| invalid_input(error.to_string()))?;
    Ok(())
}

fn validate_runtime_root(path: &Path) -> io::Result<()> {
    validate_directory(path, "runtime 根目录")
}

fn validate_exact_run_directory(
    runtime_root: &Path,
    run_dir: &Path,
    run_id: &str,
) -> io::Result<()> {
    validate_directory(run_dir, "删除运行目录")?;
    if run_dir.parent() != Some(runtime_root)
        || run_dir.file_name().and_then(|name| name.to_str()) != Some(run_id)
    {
        return Err(invalid_input("删除运行目录不是 runtime 根的直接子目录"));
    }
    Ok(())
}

fn validate_run_id(run_id: &str) -> io::Result<()> {
    if run_id.is_empty()
        || run_id == "."
        || run_id == ".."
        || run_id.contains(['/', '\\', ':', '\t', '\r', '\n'])
    {
        return Err(invalid_input("删除运行 ID 不是安全的单级目录名"));
    }
    Ok(())
}

fn run_id(path: &Path) -> String {
    path.file_name()
        .and_then(|name| name.to_str())
        .unwrap_or_default()
        .to_owned()
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

fn validate_regular_file(path: &Path, label: &str) -> io::Result<()> {
    let metadata = fs::symlink_metadata(path)?;
    if !metadata.is_file() {
        return Err(invalid_input(format!("{label}不是普通文件")));
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

fn parse_text_field<'a>(value: &'a str, field: &str) -> io::Result<&'a str> {
    if value.is_empty() || value.contains(['\t', '\r', '\n']) {
        return Err(invalid_data(format!("删除队列 {field} 为空或包含控制字符")));
    }
    Ok(value)
}

fn mode_name(mode: DeleteMode) -> &'static str {
    match mode {
        DeleteMode::RecycleBin => "recycle_bin",
        DeleteMode::Permanent => "permanent",
    }
}

fn parse_mode(value: &str) -> io::Result<DeleteMode> {
    match value {
        "recycle_bin" => Ok(DeleteMode::RecycleBin),
        "permanent" => Ok(DeleteMode::Permanent),
        _ => Err(invalid_data("删除队列删除模式无效")),
    }
}

fn parse_md5(value: &str) -> io::Result<[u8; 16]> {
    if value.len() != 32
        || value
            .bytes()
            .any(|byte| !byte.is_ascii_hexdigit() || byte.is_ascii_uppercase())
    {
        return Err(invalid_data("删除队列 MD5 必须是 32 位小写十六进制"));
    }
    let mut bytes = [0u8; 16];
    for (index, byte) in bytes.iter_mut().enumerate() {
        *byte = (hex_nibble(value.as_bytes()[index * 2])? << 4)
            | hex_nibble(value.as_bytes()[index * 2 + 1])?;
    }
    Ok(bytes)
}

fn hex_nibble(byte: u8) -> io::Result<u8> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        _ => Err(invalid_data("删除队列十六进制字段无效")),
    }
}

fn encode_hex(bytes: &[u8; 16]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(32);
    for byte in bytes {
        output.push(HEX[(byte >> 4) as usize] as char);
        output.push(HEX[(byte & 0x0f) as usize] as char);
    }
    output
}

fn invalid_input(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message.into())
}

fn invalid_data(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}

//! 不依赖后台线程的定长滚动日志 writer，供三个进程复用。

use std::{
    env,
    fmt::Display,
    fs::{self, File, OpenOptions},
    io::{self, Write},
    panic,
    path::{Path, PathBuf},
    process,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    thread,
    time::{SystemTime, UNIX_EPOCH},
};

use thiserror::Error;
use tracing_subscriber::{EnvFilter, filter::ParseError};

/// 生产日志单文件固定上限 20 MiB。
pub const DEFAULT_LOG_FILE_BYTES: u64 = 20 * 1024 * 1024;
/// 生产日志包含当前文件在内固定保留 10 个文件。
pub const DEFAULT_LOG_FILE_COUNT: usize = 10;

/// 仅允许提高日志详细度时产生的过滤器错误。
#[derive(Debug, Error)]
pub enum LogFilterError {
    /// 指令试图关闭默认 INFO，或不符合允许的 target 语法。
    #[error("不允许的 RUST_LOG 指令：{0}")]
    UnsupportedDirective(String),
    /// 白名单指令仍无法由 tracing 解析。
    #[error("无法解析 RUST_LOG 指令 {0}：{1}")]
    InvalidDirective(String, String),
    /// RUST_LOG 存在，但不是 Unicode 文本。
    #[error("无法读取 RUST_LOG：{0}")]
    Environment(#[from] env::VarError),
}

/// 创建至少保留 INFO 的过滤器；环境指令只能提高日志详细度。
pub fn log_filter(value: Option<&str>) -> Result<EnvFilter, LogFilterError> {
    let mut filter = EnvFilter::new("info");
    for raw_directive in value.unwrap_or_default().split(',') {
        let directive = raw_directive.trim();
        if directive.is_empty() {
            continue;
        }
        if !is_allowed_directive(directive) {
            return Err(LogFilterError::UnsupportedDirective(directive.to_owned()));
        }
        let parsed = directive.parse().map_err(|error: ParseError| {
            LogFilterError::InvalidDirective(directive.to_owned(), error.to_string())
        })?;
        filter = filter.add_directive(parsed);
    }
    Ok(filter)
}

/// 从 RUST_LOG 读取临时排障级别；变量缺失时使用固定 INFO。
pub fn log_filter_from_env() -> Result<EnvFilter, LogFilterError> {
    match env::var("RUST_LOG") {
        Ok(value) => log_filter(Some(&value)),
        Err(env::VarError::NotPresent) => log_filter(None),
        Err(error) => Err(LogFilterError::Environment(error)),
    }
}

/// 判断单条指令是否只会保持或提高默认日志详细度。
fn is_allowed_directive(directive: &str) -> bool {
    if matches!(directive, "info" | "debug" | "trace") {
        return true;
    }
    directive.rsplit_once('=').is_some_and(|(target, level)| {
        matches!(level, "debug" | "trace")
            && !target.is_empty()
            && target.chars().all(|character| {
                character.is_ascii_alphanumeric() || matches!(character, '_' | '-' | ':' | '.')
            })
    })
}

/// 保存进程名和独立应急日志位置，覆盖 subscriber 初始化前及 panic 路径。
#[derive(Clone)]
pub struct ProcessDiagnostics {
    process: &'static str,
    emergency_path: Arc<PathBuf>,
    primary_ready: Arc<AtomicBool>,
    panic_hook_installed: Arc<AtomicBool>,
}

impl ProcessDiagnostics {
    /// 为进程创建 `%TEMP%\mySingerServer\logs` 下的固定应急日志位置。
    pub fn new(process: &'static str) -> Self {
        Self::with_emergency_path(
            process,
            env::temp_dir()
                .join("mySingerServer")
                .join("logs")
                .join(format!("{process}-emergency.log")),
        )
    }

    /// 仅供隔离测试把应急日志重定向到临时目录。
    #[doc(hidden)]
    pub fn with_emergency_path(process: &'static str, path: impl Into<PathBuf>) -> Self {
        Self {
            process,
            emergency_path: Arc::new(path.into()),
            primary_ready: Arc::new(AtomicBool::new(false)),
            panic_hook_installed: Arc::new(AtomicBool::new(false)),
        }
    }

    /// 标记正常 subscriber 已就绪，后续普通错误经 tracing 写入主日志。
    pub fn mark_primary_ready(&self) {
        self.primary_ready.store(true, Ordering::Release);
    }

    /// 记录可降级错误；subscriber 尚未就绪时直接写应急日志。
    pub fn record_warning(
        &self,
        event: &'static str,
        operation: &'static str,
        error: &dyn Display,
    ) {
        self.record(DiagnosticLevel::Warn, event, operation, error);
    }

    /// 记录阻止当前操作继续的错误；subscriber 尚未就绪时直接写应急日志。
    pub fn record_error(&self, event: &'static str, operation: &'static str, error: &dyn Display) {
        self.record(DiagnosticLevel::Error, event, operation, error);
    }

    /// 安装一次进程级 panic hook，并在调用原 hook 前直接写应急日志。
    pub fn install_panic_hook(&self) {
        if self.panic_hook_installed.swap(true, Ordering::AcqRel) {
            return;
        }
        let previous_hook = panic::take_hook();
        let diagnostics = self.clone();
        panic::set_hook(Box::new(move |information| {
            if let Err(error) = diagnostics.write_panic(information) {
                eprintln!("写入 panic 应急日志失败：{error}");
            }
            previous_hook(information);
        }));
    }

    /// 根据主日志状态选择 tracing 或应急文件，避免初始化前事件丢失。
    fn record(
        &self,
        level: DiagnosticLevel,
        event: &'static str,
        operation: &'static str,
        error: &dyn Display,
    ) {
        if self.primary_ready.load(Ordering::Acquire) {
            match level {
                DiagnosticLevel::Warn => tracing::warn!(
                    event,
                    process = self.process,
                    pid = process::id(),
                    operation,
                    error = %error,
                    "进程操作已降级"
                ),
                DiagnosticLevel::Error => tracing::error!(
                    event,
                    process = self.process,
                    pid = process::id(),
                    operation,
                    error = %error,
                    "进程操作失败"
                ),
            }
            return;
        }
        if let Err(write_error) = self.write_emergency_event(level, event, operation, error) {
            eprintln!("写入应急日志失败：{write_error}；原始错误：{error}");
        }
    }

    /// 直接追加一条普通应急事件，不经过 tracing 以避免递归。
    fn write_emergency_event(
        &self,
        level: DiagnosticLevel,
        event: &str,
        operation: &str,
        error: &dyn Display,
    ) -> io::Result<()> {
        let line = format!(
            "ts_unix_ms={} level={} event=\"{}\" process=\"{}\" pid={} operation=\"{}\" error=\"{}\"\n",
            unix_timestamp_millis(),
            level.as_str(),
            escape_field(event),
            escape_field(self.process),
            process::id(),
            escape_field(operation),
            escape_field(&error.to_string()),
        );
        append_line(&self.emergency_path, &line)
    }

    /// 直接记录 panic 字段，避免 logger 自身 panic 时再次进入 subscriber。
    fn write_panic(&self, information: &panic::PanicHookInfo<'_>) -> io::Result<()> {
        let current_thread = thread::current();
        let thread_name = current_thread.name().unwrap_or("<unnamed>");
        let (source_file, source_line) =
            information.location().map_or(("<unknown>", 0), |location| {
                (location.file(), location.line())
            });
        let panic_message = information
            .payload()
            .downcast_ref::<&str>()
            .copied()
            .or_else(|| {
                information
                    .payload()
                    .downcast_ref::<String>()
                    .map(String::as_str)
            })
            .unwrap_or("<non-string panic payload>");
        let line = format!(
            "ts_unix_ms={} level=ERROR event=\"process_panicked\" process=\"{}\" pid={} thread=\"{}\" source_file=\"{}\" source_line={} panic_message=\"{}\"\n",
            unix_timestamp_millis(),
            escape_field(self.process),
            process::id(),
            escape_field(thread_name),
            escape_field(source_file),
            source_line,
            escape_field(panic_message),
        );
        append_line(&self.emergency_path, &line)
    }

    /// 主日志 writer 失败时把失败原因和原始日志片段直接写入应急文件。
    fn write_sink_failure(
        &self,
        primary_path: &Path,
        primary_error: &io::Error,
        original: &[u8],
    ) -> io::Result<()> {
        let original_line = String::from_utf8_lossy(original);
        let line = format!(
            "ts_unix_ms={} level=ERROR event=\"diagnostic_sink_failed\" process=\"{}\" pid={} primary_path=\"{}\" fallback_path=\"{}\" error=\"{}\" original_line=\"{}\"\n",
            unix_timestamp_millis(),
            escape_field(self.process),
            process::id(),
            escape_field(&primary_path.display().to_string()),
            escape_field(&self.emergency_path.display().to_string()),
            escape_field(&primary_error.to_string()),
            escape_field(&original_line),
        );
        append_line(&self.emergency_path, &line)
    }
}

/// 应急事件允许的两个错误级别。
#[derive(Clone, Copy)]
enum DiagnosticLevel {
    Warn,
    Error,
}

impl DiagnosticLevel {
    /// 返回稳定的大写日志级别。
    const fn as_str(self) -> &'static str {
        match self {
            Self::Warn => "WARN",
            Self::Error => "ERROR",
        }
    }
}

/// 在主日志 writer 失败时，把原始日志事件转写到进程应急日志。
pub struct FallbackLogWriter<W> {
    primary: W,
    primary_path: PathBuf,
    diagnostics: ProcessDiagnostics,
}

impl<W> FallbackLogWriter<W> {
    /// 包装主 writer，并保存用于故障事件的主日志路径。
    pub fn new(
        primary: W,
        primary_path: impl Into<PathBuf>,
        diagnostics: ProcessDiagnostics,
    ) -> Self {
        Self {
            primary,
            primary_path: primary_path.into(),
            diagnostics,
        }
    }
}

impl<W: Write> Write for FallbackLogWriter<W> {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        match self.primary.write(buffer) {
            Ok(written) => Ok(written),
            Err(primary_error) => {
                match self.diagnostics.write_sink_failure(
                    &self.primary_path,
                    &primary_error,
                    buffer,
                ) {
                    Ok(()) => Ok(buffer.len()),
                    Err(fallback_error) => {
                        eprintln!(
                            "主日志与应急日志均写入失败：primary={primary_error} fallback={fallback_error}"
                        );
                        Err(primary_error)
                    }
                }
            }
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        match self.primary.flush() {
            Ok(()) => Ok(()),
            Err(primary_error) => {
                match self.diagnostics.write_sink_failure(
                    &self.primary_path,
                    &primary_error,
                    b"<flush>",
                ) {
                    Ok(()) => Ok(()),
                    Err(fallback_error) => {
                        eprintln!(
                            "主日志与应急日志均刷新失败：primary={primary_error} fallback={fallback_error}"
                        );
                        Err(primary_error)
                    }
                }
            }
        }
    }
}

/// 追加一个完整 UTF-8 单行，并立即刷新文件缓冲区。
fn append_line(path: &Path, line: &str) -> io::Result<()> {
    if let Some(parent) = path
        .parent()
        .filter(|parent| !parent.as_os_str().is_empty())
    {
        fs::create_dir_all(parent)?;
    }
    let mut file = OpenOptions::new().create(true).append(true).open(path)?;
    file.write_all(line.as_bytes())?;
    file.flush()
}

/// 把字段中的控制字符转义，保证每个事件只占一行。
fn escape_field(value: &str) -> String {
    let mut escaped = String::with_capacity(value.len());
    for character in value.chars() {
        match character {
            '\\' => escaped.push_str("\\\\"),
            '\"' => escaped.push_str("\\\""),
            '\n' => escaped.push_str("\\n"),
            '\r' => escaped.push_str("\\r"),
            '\t' => escaped.push_str("\\t"),
            character if character.is_control() => {
                escaped.push_str(&format!("\\u{{{:x}}}", character as u32));
            }
            character => escaped.push(character),
        }
    }
    escaped
}

/// 返回当前 Unix 毫秒时间；系统时钟早于纪元时使用零。
fn unix_timestamp_millis() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |duration| duration.as_millis())
}

/// 在下一次写入越过边界前轮转的同步文件 writer。
///
/// 文件名为 `<prefix>.log`、`<prefix>.1.log` 至 `<prefix>.(keep-1).log`。
/// 一个大于上限的单条日志仍完整写入当前文件，下一次写入前再轮转。
pub struct SizeRotatingWriter {
    directory: PathBuf,
    prefix: String,
    max_bytes: u64,
    keep_files: usize,
    current: Option<File>,
    current_bytes: u64,
}

impl SizeRotatingWriter {
    /// 创建生产默认的 20 MiB × 10 日志 writer。
    pub fn production(
        directory: impl Into<PathBuf>,
        prefix: impl Into<String>,
    ) -> io::Result<Self> {
        Self::new(
            directory,
            prefix,
            DEFAULT_LOG_FILE_BYTES,
            DEFAULT_LOG_FILE_COUNT,
        )
    }

    /// 创建显式边界的 writer；较小数值只用于单元测试。
    pub fn new(
        directory: impl Into<PathBuf>,
        prefix: impl Into<String>,
        max_bytes: u64,
        keep_files: usize,
    ) -> io::Result<Self> {
        if max_bytes == 0 || keep_files == 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "日志大小和保留数量必须大于零",
            ));
        }
        let directory = directory.into();
        fs::create_dir_all(&directory)?;
        let prefix = prefix.into();
        let path = log_path(&directory, &prefix, 0);
        let current = OpenOptions::new().create(true).append(true).open(path)?;
        let current_bytes = current.metadata()?.len();
        Ok(Self {
            directory,
            prefix,
            max_bytes,
            keep_files,
            current: Some(current),
            current_bytes,
        })
    }

    fn rotate(&mut self) -> io::Result<()> {
        if let Some(mut current) = self.current.take() {
            current.flush()?;
        }
        if self.keep_files > 1 {
            let oldest = log_path(&self.directory, &self.prefix, self.keep_files - 1);
            if oldest.exists() {
                fs::remove_file(oldest)?;
            }
            for index in (1..self.keep_files - 1).rev() {
                let source = log_path(&self.directory, &self.prefix, index);
                if source.exists() {
                    fs::rename(source, log_path(&self.directory, &self.prefix, index + 1))?;
                }
            }
            let current_path = log_path(&self.directory, &self.prefix, 0);
            if current_path.exists() {
                fs::rename(current_path, log_path(&self.directory, &self.prefix, 1))?;
            }
        } else {
            let current_path = log_path(&self.directory, &self.prefix, 0);
            if current_path.exists() {
                fs::remove_file(current_path)?;
            }
        }
        self.current = Some(OpenOptions::new().create(true).append(true).open(log_path(
            &self.directory,
            &self.prefix,
            0,
        ))?);
        self.current_bytes = 0;
        Ok(())
    }
}

impl Write for SizeRotatingWriter {
    fn write(&mut self, buffer: &[u8]) -> io::Result<usize> {
        if self.current_bytes > 0
            && self.current_bytes.saturating_add(buffer.len() as u64) > self.max_bytes
        {
            self.rotate()?;
        }
        let written = self
            .current
            .as_mut()
            .expect("日志 writer 始终持有当前文件")
            .write(buffer)?;
        self.current_bytes = self.current_bytes.saturating_add(written as u64);
        Ok(written)
    }

    fn flush(&mut self) -> io::Result<()> {
        self.current
            .as_mut()
            .expect("日志 writer 始终持有当前文件")
            .flush()
    }
}

fn log_path(directory: &Path, prefix: &str, index: usize) -> PathBuf {
    if index == 0 {
        directory.join(format!("{prefix}.log"))
    } else {
        directory.join(format!("{prefix}.{index}.log"))
    }
}

#[cfg(test)]
mod tests {
    use std::{
        fs,
        io::{self, Write},
    };

    use super::{FallbackLogWriter, ProcessDiagnostics, SizeRotatingWriter, log_filter};

    /// 模拟主日志文件在写入和刷新时都失败。
    struct AlwaysFailWriter;

    impl Write for AlwaysFailWriter {
        fn write(&mut self, _buffer: &[u8]) -> io::Result<usize> {
            Err(io::Error::other("primary write failed"))
        }

        fn flush(&mut self) -> io::Result<()> {
            Err(io::Error::other("primary flush failed"))
        }
    }

    /// 防止轮转后继续增长或保留超过固定文件数量。
    #[test]
    fn rotates_before_crossing_boundary_and_keeps_fixed_count() {
        let directory = tempfile::tempdir().unwrap();
        let mut writer = SizeRotatingWriter::new(directory.path(), "node", 4, 3).unwrap();
        writer.write_all(b"1111").unwrap();
        writer.write_all(b"2222").unwrap();
        writer.write_all(b"3333").unwrap();
        writer.write_all(b"4444").unwrap();
        writer.flush().unwrap();

        assert_eq!(
            fs::read(directory.path().join("node.log")).unwrap(),
            b"4444"
        );
        assert_eq!(
            fs::read(directory.path().join("node.1.log")).unwrap(),
            b"3333"
        );
        assert_eq!(
            fs::read(directory.path().join("node.2.log")).unwrap(),
            b"2222"
        );
        assert!(!directory.path().join("node.3.log").exists());
    }

    /// 防止环境过滤器降低默认级别或静默接受非法指令。
    #[test]
    fn log_filter_defaults_to_info_and_accepts_only_more_verbose_directives() {
        assert_eq!(log_filter(None).unwrap().to_string(), "info");
        assert_eq!(log_filter(Some("")).unwrap().to_string(), "info");
        let explicit = log_filter(Some("dedup_node_engine=debug,info"))
            .unwrap()
            .to_string();
        assert!(explicit.contains("dedup_node_engine=debug"));
        assert!(explicit.contains("info"));
        assert!(log_filter(Some("[invalid")).is_err());
        assert!(log_filter(Some("off")).is_err());
        assert!(log_filter(Some("error")).is_err());
    }

    /// 防止初始化前的错误因换行破坏单行日志契约。
    #[test]
    fn emergency_log_is_single_line_and_contains_process_context() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("node-emergency.log");
        let diagnostics = ProcessDiagnostics::with_emergency_path("node", &path);

        diagnostics.record_error("process_failed", "load_config", &"第一行\n第二行");

        let log = fs::read_to_string(path).unwrap();
        assert_eq!(log.lines().count(), 1);
        assert!(log.contains("event=\"process_failed\""));
        assert!(log.contains("operation=\"load_config\""));
        assert!(log.contains("process=\"node\""));
        assert!(log.contains("第一行\\n第二行"));
    }

    /// 防止主日志写入失败时丢失原始业务事件。
    #[test]
    fn primary_write_failure_replays_original_line_to_emergency_log() {
        let directory = tempfile::tempdir().unwrap();
        let emergency_path = directory.path().join("worker-emergency.log");
        let diagnostics = ProcessDiagnostics::with_emergency_path("worker", &emergency_path);
        let mut writer = FallbackLogWriter::new(AlwaysFailWriter, "primary.log", diagnostics);

        writer
            .write_all(b"event=\"worker_crashed\" error=\"boom\"\n")
            .unwrap();

        let log = fs::read_to_string(emergency_path).unwrap();
        assert!(log.contains("event=\"diagnostic_sink_failed\""));
        assert!(log.contains("worker_crashed"));
        assert!(log.contains("boom"));
    }
}

//! 通过公开 Node TCP 协议执行只读真实媒体半小时运行验收。

use std::{
    collections::BTreeSet,
    env,
    fs::OpenOptions,
    future::Future,
    io::{BufWriter, Write},
    net::SocketAddr,
    path::{Path, PathBuf},
    pin::Pin,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

use dedup_core::{NodeEndpoint, TaskId};
use dedup_desktop_core::node_session::NodeSession;
use dedup_protocol::proto;
use serde::Serialize;

const DEFAULT_DURATION_SECONDS: u64 = 30 * 60;
const SAMPLE_SECONDS: u64 = 2;
const RUNTIME_PAGE_LIMIT: u32 = 1_000;
const TERMINAL_WAIT_TICKS: usize = 15;

/// 不引入 async-trait 的可借用异步结果。
pub type AcceptanceFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T, String>> + Send + 'a>>;

/// 验收客户端经过边界校验后的固定参数。
#[derive(Clone, Debug)]
pub struct AcceptanceConfig {
    endpoint: String,
    media_root: String,
    duration: Duration,
    evidence_root: PathBuf,
    output: PathBuf,
}

impl AcceptanceConfig {
    /// 创建配置；真实计算窗口不得短于半小时，输出必须位于证据根内。
    pub fn new(
        endpoint: &str,
        media_root: &str,
        duration_seconds: u64,
        evidence_root: &Path,
        output: &Path,
    ) -> Result<Self, String> {
        endpoint
            .parse::<SocketAddr>()
            .map_err(|error| format!("验收 endpoint 无效：{error}"))?;
        if media_root.trim().is_empty() {
            return Err("真实媒体根不能为空".into());
        }
        if duration_seconds < DEFAULT_DURATION_SECONDS {
            return Err(format!(
                "真实媒体计算窗口不得少于 {DEFAULT_DURATION_SECONDS} 秒"
            ));
        }
        if !evidence_root.is_absolute() || !output.is_absolute() {
            return Err("证据根和输出路径必须是绝对路径".into());
        }
        if !output.starts_with(evidence_root) || output == evidence_root {
            return Err("运行样本输出必须位于显式证据根内".into());
        }
        Ok(Self {
            endpoint: endpoint.into(),
            media_root: media_root.into(),
            duration: Duration::from_secs(duration_seconds),
            evidence_root: evidence_root.into(),
            output: output.into(),
        })
    }

    /// 从计划约定的四个环境变量创建生产配置。
    pub fn from_env() -> Result<Self, String> {
        let endpoint = required_env("RUST_V2_ACCEPTANCE_ENDPOINT")?;
        let media_root = required_env("RUST_V2_REAL_MEDIA_ROOT")?;
        let output = PathBuf::from(required_env("RUST_V2_ACCEPTANCE_OUTPUT")?);
        let evidence_root = output
            .parent()
            .ok_or_else(|| "RUST_V2_ACCEPTANCE_OUTPUT 缺少父目录".to_string())?;
        let duration_seconds = env::var("RUST_V2_ACCEPTANCE_DURATION_SECONDS")
            .ok()
            .map(|value| {
                value
                    .parse::<u64>()
                    .map_err(|error| format!("验收时长必须是整数秒：{error}"))
            })
            .transpose()?
            .unwrap_or(DEFAULT_DURATION_SECONDS);
        Self::new(
            &endpoint,
            &media_root,
            duration_seconds,
            evidence_root,
            &output,
        )
    }

    /// 返回固定计算窗口。
    pub const fn duration(&self) -> Duration {
        self.duration
    }

    /// 返回固定两秒采样间隔。
    pub const fn sample_interval(&self) -> Duration {
        Duration::from_secs(SAMPLE_SECONDS)
    }

    /// 返回默认枚举器；Node 会负责启动 Everything 或回退 Walker。
    pub const fn enumerator(&self) -> &'static str {
        "everything"
    }

    /// 返回原样传给远端 Node 的媒体根。
    pub fn media_root(&self) -> &str {
        &self.media_root
    }
}

/// 可替换的单调时钟，测试无需真实等待半小时。
pub trait AcceptanceClock: Send + Sync {
    /// 返回验收开始后的单调时长。
    fn elapsed(&self) -> Duration;
    /// 等待一个固定采样间隔。
    fn sleep<'a>(&'a self, duration: Duration) -> Pin<Box<dyn Future<Output = ()> + Send + 'a>>;
}

/// 真实 Tokio 单调时钟。
struct TokioClock(Instant);

impl TokioClock {
    /// 从当前单调时刻开始计时。
    fn start() -> Self {
        Self(Instant::now())
    }
}

impl AcceptanceClock for TokioClock {
    fn elapsed(&self) -> Duration {
        self.0.elapsed()
    }

    fn sleep<'a>(&'a self, duration: Duration) -> Pin<Box<dyn Future<Output = ()> + Send + 'a>> {
        Box::pin(tokio::time::sleep(duration))
    }
}

/// 验收允许调用的最小 Node 公共协议面。
pub trait AcceptanceSession: Send + Sync {
    /// 创建扫描并返回持久任务 ID 字符串。
    fn create_scan<'a>(
        &'a self,
        roots: Vec<String>,
        force_recalculate: bool,
        enumerator: &'a str,
    ) -> AcceptanceFuture<'a, String>;
    /// 返回当前进程全部运行任务摘要。
    fn list_runtime_tasks<'a>(&'a self) -> AcceptanceFuture<'a, Vec<proto::RuntimeTaskSummary>>;
    /// 返回一个运行任务的完整详情。
    fn runtime_task_details<'a>(
        &'a self,
        runtime_task_id: &'a str,
    ) -> AcceptanceFuture<'a, proto::RuntimeTaskDetails>;
    /// 取消到期时仍在运行的持久任务。
    fn cancel_task<'a>(&'a self, persistent_task_id: &'a str) -> AcceptanceFuture<'a, ()>;
}

impl AcceptanceSession for NodeSession {
    fn create_scan<'a>(
        &'a self,
        roots: Vec<String>,
        force_recalculate: bool,
        enumerator: &'a str,
    ) -> AcceptanceFuture<'a, String> {
        Box::pin(async move {
            NodeSession::create_scan(self, roots, force_recalculate, enumerator)
                .await
                .map(|task_id| task_id.as_uuid().to_string())
                .map_err(|error| error.to_string())
        })
    }

    fn list_runtime_tasks<'a>(&'a self) -> AcceptanceFuture<'a, Vec<proto::RuntimeTaskSummary>> {
        Box::pin(async move {
            let mut cursor = String::new();
            let mut tasks = Vec::new();
            loop {
                let page = NodeSession::list_runtime_tasks(self, &cursor, RUNTIME_PAGE_LIMIT)
                    .await
                    .map_err(|error| error.to_string())?;
                tasks.extend(page.tasks);
                if page.next_cursor.is_empty() {
                    return Ok(tasks);
                }
                if page.next_cursor == cursor {
                    return Err("运行任务分页游标没有前进".into());
                }
                cursor = page.next_cursor;
            }
        })
    }

    fn runtime_task_details<'a>(
        &'a self,
        runtime_task_id: &'a str,
    ) -> AcceptanceFuture<'a, proto::RuntimeTaskDetails> {
        Box::pin(async move {
            NodeSession::runtime_task_details(self, runtime_task_id)
                .await
                .map_err(|error| error.to_string())
        })
    }

    fn cancel_task<'a>(&'a self, persistent_task_id: &'a str) -> AcceptanceFuture<'a, ()> {
        Box::pin(async move {
            let uuid = uuid::Uuid::parse_str(persistent_task_id)
                .map_err(|error| format!("持久任务ID无效：{error}"))?;
            NodeSession::cancel_task(self, TaskId::from_uuid(uuid))
                .await
                .map_err(|error| error.to_string())
        })
    }
}

/// 一个阶段的可序列化采样。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeStageSample {
    /// 稳定阶段 ID。
    pub stage_id: String,
    /// 中文阶段名。
    pub display_name: String,
    /// 协议枚举整数，保留未来值。
    pub state: i32,
    /// 计数单位。
    pub unit: String,
    /// 已完成计数。
    pub completed: u64,
    /// 总计数。
    pub total: u64,
    /// 总计数是否已知。
    pub total_known: bool,
    /// 失败计数。
    pub failed: u64,
    /// 跳过计数。
    pub skipped: u64,
    /// 十秒窗口速度。
    pub speed_per_second: f64,
    /// 阶段已运行毫秒数。
    pub elapsed_ms: u64,
    /// 可计算时的剩余毫秒数。
    pub eta_ms: Option<u64>,
}

/// 一个 Worker 槽的可序列化采样。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeWorkerSample {
    /// Worker 槽位。
    pub slot: u32,
    /// 实际进程 ID。
    pub process_id: Option<u32>,
    /// 当前阶段 ID。
    pub stage_id: String,
    /// 当前文件显示路径。
    pub display_path: String,
    /// 当前物理盘标识。
    pub physical_disk_id: String,
    /// 此槽累计完成文件数。
    pub completed_files: u64,
    /// 此槽十秒窗口速度。
    pub speed_per_second: f64,
}

/// 最近一次文件级失败采样。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeFailureSample {
    /// 失败阶段。
    pub stage_id: String,
    /// 文件显示路径。
    pub display_path: String,
    /// 简短错误信息。
    pub message: String,
}

/// 每两秒写入一行的运行任务详情样本。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeAcceptanceSample {
    /// 固定记录类型，便于 PowerShell 解析 NDJSON。
    pub record_type: &'static str,
    /// Unix UTC 毫秒时间戳。
    pub utc_unix_ms: u64,
    /// 计算窗口已经过秒数。
    pub elapsed_seconds: u64,
    /// 运行任务 ID。
    pub runtime_task_id: String,
    /// 物理机器 ID。
    pub machine_id: String,
    /// 运行任务状态。
    pub state: String,
    /// 整体已完成计数。
    pub overall_completed: u64,
    /// 整体总计数。
    pub overall_total: u64,
    /// 整体总计数是否已知。
    pub overall_total_known: bool,
    /// 整体失败计数。
    pub overall_failed: u64,
    /// 整体跳过计数。
    pub overall_skipped: u64,
    /// 节点进程内直连样本不会陈旧。
    pub stale: bool,
    /// 全部阶段。
    pub stages: Vec<RuntimeStageSample>,
    /// 当前 Worker 槽。
    pub workers: Vec<RuntimeWorkerSample>,
    /// 最近失败。
    pub failures: Vec<RuntimeFailureSample>,
}

impl RuntimeAcceptanceSample {
    /// 从协议详情复制为不依赖 protobuf 序列化的证据对象。
    fn from_details(elapsed: Duration, details: proto::RuntimeTaskDetails) -> Result<Self, String> {
        let summary = details
            .summary
            .ok_or_else(|| "运行任务详情缺少摘要".to_string())?;
        Ok(Self {
            record_type: "runtime_sample",
            utc_unix_ms: unix_ms(),
            elapsed_seconds: elapsed.as_secs(),
            runtime_task_id: summary.runtime_task_id,
            machine_id: summary.machine_id,
            state: summary.state,
            overall_completed: summary.overall_completed,
            overall_total: summary.overall_total,
            overall_total_known: summary.overall_total_known,
            overall_failed: summary.overall_failed,
            overall_skipped: summary.overall_skipped,
            stale: false,
            stages: details
                .stages
                .into_iter()
                .map(|stage| RuntimeStageSample {
                    stage_id: stage.stage_id,
                    display_name: stage.display_name,
                    state: stage.state,
                    unit: stage.unit,
                    completed: stage.completed,
                    total: stage.total,
                    total_known: stage.total_known,
                    failed: stage.failed,
                    skipped: stage.skipped,
                    speed_per_second: stage.speed_per_second,
                    elapsed_ms: stage.elapsed_ms,
                    eta_ms: stage.eta_ms,
                })
                .collect(),
            workers: details
                .workers
                .into_iter()
                .map(|worker| RuntimeWorkerSample {
                    slot: worker.slot,
                    process_id: worker.process_id,
                    stage_id: worker.stage_id,
                    display_path: worker.display_path,
                    physical_disk_id: worker.physical_disk_id,
                    completed_files: worker.completed_files,
                    speed_per_second: worker.speed_per_second,
                })
                .collect(),
            failures: details
                .failures
                .into_iter()
                .map(|failure| RuntimeFailureSample {
                    stage_id: failure.stage_id,
                    display_path: failure.display_path,
                    message: failure.message,
                })
                .collect(),
        })
    }
}

/// 计算窗口结束后的汇总记录。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeAcceptanceResult {
    /// 固定记录类型。
    pub record_type: &'static str,
    /// 固定计算窗口秒数。
    pub duration_seconds: u64,
    /// 实际样本数。
    pub sample_count: u64,
    /// 为保持计算创建的扫描数。
    pub scans_started: u64,
    /// 提前失败的扫描数。
    pub failed_scans: u64,
    /// 到期时是否取消了活动任务。
    pub cancelled_at_deadline: bool,
}

/// NDJSON 或内存样本目的地。
pub trait AcceptanceSink {
    /// 写入一个两秒详情样本。
    fn write_sample(&mut self, sample: &RuntimeAcceptanceSample) -> Result<(), String>;
    /// 写入最终汇总；测试 sink 可使用默认空实现。
    fn write_result(&mut self, _result: &RuntimeAcceptanceResult) -> Result<(), String> {
        Ok(())
    }
}

/// 持续运行到固定截止时间；提前终态会创建下一次强制重算扫描。
pub async fn run_acceptance<S, C, W>(
    session: &S,
    clock: &C,
    mut sink: W,
    config: &AcceptanceConfig,
) -> Result<RuntimeAcceptanceResult, String>
where
    S: AcceptanceSession,
    C: AcceptanceClock,
    W: AcceptanceSink,
{
    let mut known_runtime_ids = session
        .list_runtime_tasks()
        .await?
        .into_iter()
        .map(|task| task.runtime_task_id)
        .collect::<BTreeSet<_>>();
    let mut persistent_task_id = session
        .create_scan(vec![config.media_root().into()], false, config.enumerator())
        .await?;
    let mut runtime_task_id = None;
    let mut scans_started = 1_u64;
    let mut failed_scans = 0_u64;
    let mut sample_count = 0_u64;
    let mut current_terminal = false;

    while clock.elapsed() < config.duration() {
        clock.sleep(config.sample_interval()).await;
        if runtime_task_id.is_none() {
            runtime_task_id = newest_scan_runtime(session, &known_runtime_ids).await?;
        }
        let Some(active_runtime_id) = runtime_task_id.as_deref() else {
            return Err("创建扫描后没有观察到新的运行任务".into());
        };
        let sample = RuntimeAcceptanceSample::from_details(
            clock.elapsed(),
            session.runtime_task_details(active_runtime_id).await?,
        )?;
        current_terminal = is_terminal(&sample.state);
        if sample.state == "failed" {
            failed_scans += 1;
        }
        sink.write_sample(&sample)?;
        sample_count += 1;

        if current_terminal && clock.elapsed() < config.duration() {
            known_runtime_ids.insert(active_runtime_id.into());
            persistent_task_id = session
                .create_scan(vec![config.media_root().into()], true, config.enumerator())
                .await?;
            scans_started += 1;
            runtime_task_id = None;
            current_terminal = false;
        }
    }

    let cancelled_at_deadline = !current_terminal;
    if cancelled_at_deadline {
        session.cancel_task(&persistent_task_id).await?;
        if let Some(runtime_task_id) = runtime_task_id.as_deref() {
            wait_for_terminal(session, clock, runtime_task_id, config.sample_interval()).await?;
        }
    }
    let result = RuntimeAcceptanceResult {
        record_type: "runtime_result",
        duration_seconds: config.duration().as_secs(),
        sample_count,
        scans_started,
        failed_scans,
        cancelled_at_deadline,
    };
    sink.write_result(&result)?;
    Ok(result)
}

async fn newest_scan_runtime<S: AcceptanceSession>(
    session: &S,
    known: &BTreeSet<String>,
) -> Result<Option<String>, String> {
    Ok(session
        .list_runtime_tasks()
        .await?
        .into_iter()
        .find(|task| task.task_kind == "scan" && !known.contains(&task.runtime_task_id))
        .map(|task| task.runtime_task_id))
}

async fn wait_for_terminal<S: AcceptanceSession, C: AcceptanceClock>(
    session: &S,
    clock: &C,
    runtime_task_id: &str,
    interval: Duration,
) -> Result<(), String> {
    for _ in 0..TERMINAL_WAIT_TICKS {
        let details = session.runtime_task_details(runtime_task_id).await?;
        let state = details
            .summary
            .ok_or_else(|| "取消后的运行详情缺少摘要".to_string())?
            .state;
        if is_terminal(&state) {
            return Ok(());
        }
        clock.sleep(interval).await;
    }
    Err("到期取消后30秒内没有观察到运行任务终态".into())
}

fn is_terminal(state: &str) -> bool {
    matches!(state, "completed" | "failed" | "cancelled")
}

/// 逐行写入并立即刷新，异常中止时仍保留已采样证据。
struct NdjsonSink {
    writer: BufWriter<std::fs::File>,
}

impl NdjsonSink {
    /// 以 create-new 打开隔离 evidence 文件，拒绝覆盖旧验收。
    fn create(path: &Path) -> Result<Self, String> {
        let file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(path)
            .map_err(|error| format!("无法创建运行证据 {}：{error}", path.display()))?;
        Ok(Self {
            writer: BufWriter::new(file),
        })
    }

    fn write_json<T: Serialize>(&mut self, value: &T) -> Result<(), String> {
        serde_json::to_writer(&mut self.writer, value)
            .map_err(|error| format!("无法序列化运行证据：{error}"))?;
        self.writer
            .write_all(b"\n")
            .and_then(|_| self.writer.flush())
            .map_err(|error| format!("无法刷新运行证据：{error}"))
    }
}

impl AcceptanceSink for NdjsonSink {
    fn write_sample(&mut self, sample: &RuntimeAcceptanceSample) -> Result<(), String> {
        self.write_json(sample)
    }

    fn write_result(&mut self, result: &RuntimeAcceptanceResult) -> Result<(), String> {
        self.write_json(result)
    }
}

fn required_env(name: &str) -> Result<String, String> {
    env::var(name)
        .map_err(|_| format!("缺少必需环境变量 {name}"))
        .and_then(|value| {
            if value.trim().is_empty() {
                Err(format!("环境变量 {name} 不能为空"))
            } else {
                Ok(value)
            }
        })
}

fn endpoint(value: &str) -> Result<NodeEndpoint, String> {
    let address = value
        .parse::<SocketAddr>()
        .map_err(|error| format!("验收 endpoint 无效：{error}"))?;
    Ok(NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    })
}

fn unix_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let config = AcceptanceConfig::from_env().map_err(std::io::Error::other)?;
    std::fs::create_dir_all(&config.evidence_root)?;
    let session =
        NodeSession::connect(endpoint(&config.endpoint).map_err(std::io::Error::other)?).await?;
    let sink = NdjsonSink::create(&config.output).map_err(std::io::Error::other)?;
    let result = run_acceptance(&session, &TokioClock::start(), sink, &config)
        .await
        .map_err(std::io::Error::other)?;
    println!(
        "RUST_V2_RUNTIME_ACCEPTANCE_PASS duration={} samples={} scans={}",
        result.duration_seconds, result.sample_count, result.scans_started
    );
    Ok(())
}

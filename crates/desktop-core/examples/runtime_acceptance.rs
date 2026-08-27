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
const SAMPLE_SECONDS: u64 = 1;
const RUNTIME_PAGE_LIMIT: u32 = 1_000;
const TERMINAL_WAIT_TICKS: usize = 15;
/// 运行任务发现失败的稳定机器可读错误码。
const FATAL_RUNTIME_TASK_DISCOVERY: &str = "runtime_task_discovery_failed";
/// 运行任务详情请求失败的稳定机器可读错误码。
const FATAL_RUNTIME_TASK_DETAILS: &str = "runtime_task_details_failed";
/// 运行任务详情结构不完整的稳定机器可读错误码。
const FATAL_RUNTIME_TASK_DETAILS_INVALID: &str = "runtime_task_details_invalid";
/// 强制重算创建失败的稳定机器可读错误码。
const FATAL_CREATE_SCAN: &str = "create_scan_failed";
/// 到期取消请求失败的稳定机器可读错误码。
const FATAL_CANCEL_TASK: &str = "cancel_task_failed";
/// 到期取消后未观察到终态的稳定机器可读错误码。
const FATAL_TERMINAL_WAIT_TIMEOUT: &str = "runtime_terminal_wait_timeout";
/// 最终 runtime_result 写出失败的稳定机器可读错误码。
const FATAL_RESULT_WRITE: &str = "runtime_result_write_failed";
/// 单条运行样本写出失败的稳定机器可读错误码。
const FATAL_SAMPLE_WRITE: &str = "runtime_sample_write_failed";
/// 多媒体根环境变量配置错误的稳定错误文本。
const INVALID_MEDIA_ROOTS: &str =
    "RUST_V2_REAL_MEDIA_ROOTS_JSON 必须是至少包含一个非空字符串的 JSON 数组";
/// 单轮环境变量配置错误的稳定错误文本。
const INVALID_SINGLE_RUN: &str = "RUST_V2_ACCEPTANCE_SINGLE_RUN 只接受 1 或 true";

/// 不引入 async-trait 的可借用异步结果。
pub type AcceptanceFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T, String>> + Send + 'a>>;

/// 验收客户端经过边界校验后的固定参数。
#[derive(Clone, Debug)]
pub struct AcceptanceConfig {
    /// Node TCP endpoint。
    endpoint: String,
    /// 按输入顺序传给每次扫描的媒体根列表。
    media_roots: Vec<String>,
    /// 是否在首个扫描进入终态后立即结束。
    single_run: bool,
    /// 验收最长运行窗口。
    duration: Duration,
    /// 允许写入证据的根目录。
    evidence_root: PathBuf,
    /// NDJSON 输出路径。
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
        if media_root.trim().is_empty() {
            return Err("真实媒体根不能为空".into());
        }
        Self::new_with_roots(
            endpoint,
            vec![media_root.into()],
            false,
            duration_seconds,
            evidence_root,
            output,
        )
    }

    /// 创建支持多媒体根和单轮模式的配置。
    pub fn new_with_roots(
        endpoint: &str,
        media_roots: Vec<String>,
        single_run: bool,
        duration_seconds: u64,
        evidence_root: &Path,
        output: &Path,
    ) -> Result<Self, String> {
        endpoint
            .parse::<SocketAddr>()
            .map_err(|error| format!("验收 endpoint 无效：{error}"))?;
        if media_roots.is_empty() || media_roots.iter().any(|root| root.trim().is_empty()) {
            return Err(INVALID_MEDIA_ROOTS.into());
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
            media_roots,
            single_run,
            duration: Duration::from_secs(duration_seconds),
            evidence_root: evidence_root.into(),
            output: output.into(),
        })
    }

    /// 从环境变量创建生产配置，兼容旧单根变量。
    pub fn from_env() -> Result<Self, String> {
        let endpoint = required_env("RUST_V2_ACCEPTANCE_ENDPOINT")?;
        let media_roots = match env::var("RUST_V2_REAL_MEDIA_ROOTS_JSON") {
            Ok(value) => parse_media_roots_json(&value)?,
            Err(env::VarError::NotPresent) => {
                vec![required_env("RUST_V2_REAL_MEDIA_ROOT")?]
            }
            Err(env::VarError::NotUnicode(_)) => return Err(INVALID_MEDIA_ROOTS.into()),
        };
        let single_run = match env::var("RUST_V2_ACCEPTANCE_SINGLE_RUN") {
            Ok(value) => parse_single_run(Some(value))?,
            Err(env::VarError::NotPresent) => false,
            Err(env::VarError::NotUnicode(_)) => return Err(INVALID_SINGLE_RUN.into()),
        };
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
        Self::new_with_roots(
            &endpoint,
            media_roots,
            single_run,
            duration_seconds,
            evidence_root,
            &output,
        )
    }

    /// 返回固定计算窗口。
    pub const fn duration(&self) -> Duration {
        self.duration
    }

    /// 返回固定一秒采样间隔。
    pub const fn sample_interval(&self) -> Duration {
        Duration::from_secs(SAMPLE_SECONDS)
    }

    /// 返回验收客户端专用的 Windows Walker 协议值，确保扫描范围只受显式媒体根约束。
    pub const fn enumerator(&self) -> &'static str {
        "windows_walker"
    }

    /// 返回原样传给远端 Node 的媒体根。
    pub fn media_root(&self) -> &str {
        &self.media_roots[0]
    }

    /// 返回按输入顺序传给每次扫描的全部媒体根。
    pub fn media_roots(&self) -> &[String] {
        &self.media_roots
    }

    /// 返回是否在首个扫描终态后立即结束。
    pub const fn single_run(&self) -> bool {
        self.single_run
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
    /// Worker 当前执行子步骤。
    pub current_step: String,
    /// 当前缓存命中、缩略图复用或回退说明。
    pub cache_detail: String,
    /// 当前文件显示路径。
    pub display_path: String,
    /// 当前物理盘标识。
    pub physical_disk_id: String,
    /// 此槽累计完成文件数。
    pub completed_files: u64,
    /// 此槽十秒窗口速度。
    pub speed_per_second: f64,
    /// Worker 显式子阶段；旧节点或未知枚举为 null。
    pub phase: Option<String>,
    /// 本文件实际占用的统一 CPU 权重。
    pub cpu_weight: Option<u32>,
    /// 本文件实际传给解码器的线程数。
    pub decoder_threads: Option<u32>,
}

/// 本次基础计算实际采用的并发与容量上限。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeExecutionConfigSample {
    /// Node Hash 最大并发。
    pub hash_tasks: Option<u32>,
    /// 路径缓存有界队列容量。
    pub path_cache_queue_capacity: Option<u32>,
    /// 内容缓存有界队列容量。
    pub content_cache_queue_capacity: Option<u32>,
    /// 待解码总所有权容量。
    pub decode_queue_capacity: Option<u32>,
    /// 待持久化总所有权容量。
    pub persist_queue_capacity: Option<u32>,
    /// 实际 Worker 槽位数。
    pub worker_slots: Option<u32>,
    /// 统一 CPU 权重预算。
    pub cpu_budget: Option<u32>,
    /// 全局磁盘许可数。
    pub global_disk_permits: Option<u32>,
    /// 每块 HDD 的许可数。
    pub hdd_per_disk_permits: Option<u32>,
    /// 每块 SSD 的许可数。
    pub ssd_per_disk_permits: Option<u32>,
    /// 未识别介质类型每盘许可数。
    pub unknown_per_disk_permits: Option<u32>,
}

/// 固定延迟桶的一个边界及累计样本数。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeLatencyBucketSample {
    /// 上界毫秒；null 表示正无穷桶。
    pub upper_bound_ms: Option<u64>,
    /// 落入本桶的累计样本数。
    pub count: u64,
}

/// 一个队列或资源的累计延迟分布。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeLatencyHistogramSample {
    /// 固定桶明细。
    pub buckets: Vec<RuntimeLatencyBucketSample>,
    /// 累计样本数。
    pub count: u64,
    /// 中位数毫秒。
    pub p50_ms: Option<u64>,
    /// P95 毫秒。
    pub p95_ms: Option<u64>,
    /// P99 毫秒。
    pub p99_ms: Option<u64>,
    /// 最大耗时毫秒。
    pub max_ms: Option<u64>,
}

/// 队列或共享资源的占用、容量与延迟证据。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeOccupancySample {
    /// 当前拥有量。
    pub current: Option<u64>,
    /// 历史峰值。
    pub peak: Option<u64>,
    /// 硬容量。
    pub capacity: Option<u64>,
    /// 等待耗时分布。
    pub wait_latency: Option<RuntimeLatencyHistogramSample>,
    /// 处理或持有耗时分布。
    pub service_latency: Option<RuntimeLatencyHistogramSample>,
}

/// 已由持久化 ACK 确认的一类媒体吞吐。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeMediaThroughputSample {
    /// 协议媒体类型整数，保留未来枚举值。
    pub media_kind: i32,
    /// 文件大小桶。
    pub size_bucket: String,
    /// 已提交文件数。
    pub files: u64,
    /// 已提交字节数。
    pub bytes: u64,
}

/// NDJSON 中一个细分 ownership 指标；旧 Node 缺失时三个字段都保持 null。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeOwnershipSample {
    /// 当前实际持有数量；None 表示该节点没有发布此指标。
    pub current: Option<u64>,
    /// 当前进程生命周期内观察到的峰值；None 表示该节点没有发布此指标。
    pub peak: Option<u64>,
    /// 生产者声明的硬容量；None 表示该节点没有发布此指标。
    pub capacity: Option<u64>,
}

/// 一块物理盘的读取许可计数；optional 字段原样保留 null 与零的差异。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeDiskReadSample {
    /// 与物理盘映射一致的稳定标识。
    pub physical_disk_id: String,
    /// 该盘观察到的读取许可容量。
    pub capacity: Option<u64>,
    /// Hash 请求等待数。
    pub hash_waiting: Option<u64>,
    /// 媒体请求等待数。
    pub media_waiting: Option<u64>,
    /// Hash 活动读取数。
    pub hash_active: Option<u64>,
    /// 媒体活动读取数。
    pub media_active: Option<u64>,
    /// Hash 累计获准数。
    pub hash_granted_total: Option<u64>,
    /// 媒体累计获准数。
    pub media_granted_total: Option<u64>,
    /// Hash 累计释放数。
    pub hash_released_total: Option<u64>,
    /// 媒体累计释放数。
    pub media_released_total: Option<u64>,
}

/// 五条有界队列、四类资源、细分 ownership 和已提交吞吐的实时证据。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimePipelineMetricsSample {
    /// Hash 队列。
    pub hash_queue: Option<RuntimeOccupancySample>,
    /// 路径缓存队列。
    pub path_cache_queue: Option<RuntimeOccupancySample>,
    /// 内容缓存队列。
    pub content_cache_queue: Option<RuntimeOccupancySample>,
    /// 待解码队列。
    pub decode_queue: Option<RuntimeOccupancySample>,
    /// 待持久化队列。
    pub persist_queue: Option<RuntimeOccupancySample>,
    /// Hash 磁盘资源。
    pub hash_io: Option<RuntimeOccupancySample>,
    /// 媒体读取资源。
    pub media_io: Option<RuntimeOccupancySample>,
    /// CPU 权重资源。
    pub cpu_weight: Option<RuntimeOccupancySample>,
    /// Worker 槽资源。
    pub worker_slots: Option<RuntimeOccupancySample>,
    /// Node 已完成 Hash 的累计字节数。
    pub hash_bytes: Option<u64>,
    /// 媒体类型与大小桶吞吐。
    pub media_throughput: Vec<RuntimeMediaThroughputSample>,
    /// Hash 等待磁盘许可的 ownership。
    pub hash_waiting_permit: Option<RuntimeOwnershipSample>,
    /// Hash 正在读取的 ownership。
    pub hash_reading: Option<RuntimeOwnershipSample>,
    /// Hash 已完成但尚未归并的 ownership。
    pub hash_completed_unjoined: Option<RuntimeOwnershipSample>,
    /// 媒体许可等待中的 ownership。
    pub media_permit_waiting: Option<RuntimeOwnershipSample>,
    /// 媒体许可获取完成前的 ready ownership。
    pub media_acquire_ready: Option<RuntimeOwnershipSample>,
    /// 已取得媒体许可的 ownership。
    pub media_permit_ready: Option<RuntimeOwnershipSample>,
    /// Worker 正在派发的 ownership。
    pub worker_dispatching: Option<RuntimeOwnershipSample>,
    /// Worker 已派发但尚未 Started 的 ownership。
    pub worker_start_pending: Option<RuntimeOwnershipSample>,
    /// Worker 解码阶段的 ownership。
    pub worker_decode: Option<RuntimeOwnershipSample>,
    /// Worker 特征阶段的 ownership。
    pub worker_feature: Option<RuntimeOwnershipSample>,
    /// Worker 等待结果发送的 ownership。
    pub worker_result_wait: Option<RuntimeOwnershipSample>,
    /// Worker 未知阶段的 ownership。
    pub worker_phase_unknown: Option<RuntimeOwnershipSample>,
    /// 已占有 content output credit 的 ownership。
    pub content_output_credit_owned: Option<RuntimeOwnershipSample>,
    /// 可用 Hash refill token；这是控制状态，不代表 RAII ownership。
    pub hash_refill_token_available: Option<RuntimeOwnershipSample>,
    /// 已占有 decode credit 的 ownership。
    pub decode_credit_owned: Option<RuntimeOwnershipSample>,
    /// item 从起点到 Applied ACK 的完成时延分布。
    pub item_completion_latency: Option<RuntimeLatencyHistogramSample>,
    /// 按物理盘标识稳定排序的读取许可指标；旧 Node 固定为空数组。
    pub disk_reads: Vec<RuntimeDiskReadSample>,
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

/// 每秒写入一行的运行任务详情样本。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeAcceptanceSample {
    /// 固定记录类型，便于 PowerShell 解析 NDJSON。
    pub record_type: &'static str,
    /// Unix UTC 毫秒时间戳。
    pub utc_unix_ms: u64,
    /// 计算窗口已经过秒数。
    pub elapsed_seconds: u64,
    /// 与上一条详情采样之间的真实单调时间差；首条固定为零。
    pub sample_interval_ms: u64,
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
    /// Node 报告的实际执行配置；旧节点为 null。
    pub execution_config: Option<RuntimeExecutionConfigSample>,
    /// Node 报告的流水线实时指标；旧节点为 null。
    pub pipeline_metrics: Option<RuntimePipelineMetricsSample>,
}

impl RuntimeAcceptanceSample {
    /// 从协议详情复制为不依赖 protobuf 序列化的证据对象。
    pub(crate) fn from_details(
        elapsed: Duration,
        details: proto::RuntimeTaskDetails,
    ) -> Result<Self, String> {
        let proto::RuntimeTaskDetails {
            summary,
            stages,
            workers,
            failures,
            execution_config,
            pipeline_metrics,
        } = details;
        let summary = summary.ok_or_else(|| "运行任务详情缺少摘要".to_string())?;
        Ok(Self {
            record_type: "runtime_sample",
            utc_unix_ms: unix_ms(),
            elapsed_seconds: elapsed.as_secs(),
            sample_interval_ms: 0,
            runtime_task_id: summary.runtime_task_id,
            machine_id: summary.machine_id,
            state: summary.state,
            overall_completed: summary.overall_completed,
            overall_total: summary.overall_total,
            overall_total_known: summary.overall_total_known,
            overall_failed: summary.overall_failed,
            overall_skipped: summary.overall_skipped,
            stale: false,
            stages: stages
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
            workers: workers
                .into_iter()
                .map(|worker| RuntimeWorkerSample {
                    slot: worker.slot,
                    process_id: worker.process_id,
                    stage_id: worker.stage_id,
                    current_step: worker.current_step,
                    cache_detail: worker.cache_detail,
                    display_path: worker.display_path,
                    physical_disk_id: worker.physical_disk_id,
                    completed_files: worker.completed_files,
                    speed_per_second: worker.speed_per_second,
                    phase: worker_phase_name(worker.phase).map(str::to_owned),
                    cpu_weight: worker.cpu_weight,
                    decoder_threads: worker.decoder_threads,
                })
                .collect(),
            failures: failures
                .into_iter()
                .map(|failure| RuntimeFailureSample {
                    stage_id: failure.stage_id,
                    display_path: failure.display_path,
                    message: failure.message,
                })
                .collect(),
            execution_config: execution_config.map(map_execution_config),
            pipeline_metrics: pipeline_metrics.map(map_pipeline_metrics),
        })
    }
}

/// 把协议 Worker 阶段映射为稳定 NDJSON 文本；未知值不作推断。
fn worker_phase_name(value: Option<i32>) -> Option<&'static str> {
    match value.and_then(|value| proto::RuntimeWorkerPhase::try_from(value).ok()) {
        Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle) => Some("idle"),
        Some(proto::RuntimeWorkerPhase::RuntimeWorkerDecode) => Some("decode"),
        Some(proto::RuntimeWorkerPhase::RuntimeWorkerFeature) => Some("feature"),
        Some(proto::RuntimeWorkerPhase::RuntimeWorkerResultWait) => Some("result_wait"),
        _ => None,
    }
}

/// 复制 Node 实际执行配置，不使用本地默认值补空字段。
fn map_execution_config(value: proto::RuntimeExecutionConfig) -> RuntimeExecutionConfigSample {
    RuntimeExecutionConfigSample {
        hash_tasks: value.hash_tasks,
        path_cache_queue_capacity: value.path_cache_queue_capacity,
        content_cache_queue_capacity: value.content_cache_queue_capacity,
        decode_queue_capacity: value.decode_queue_capacity,
        persist_queue_capacity: value.persist_queue_capacity,
        worker_slots: value.worker_slots,
        cpu_budget: value.cpu_budget,
        global_disk_permits: value.global_disk_permits,
        hdd_per_disk_permits: value.hdd_per_disk_permits,
        ssd_per_disk_permits: value.ssd_per_disk_permits,
        unknown_per_disk_permits: value.unknown_per_disk_permits,
    }
}

/// 复制一个延迟直方图，保留固定桶及缺失分位数。
fn map_histogram(value: proto::RuntimeLatencyHistogram) -> RuntimeLatencyHistogramSample {
    RuntimeLatencyHistogramSample {
        buckets: value
            .buckets
            .into_iter()
            .map(|bucket| RuntimeLatencyBucketSample {
                upper_bound_ms: bucket.upper_bound_ms,
                count: bucket.count,
            })
            .collect(),
        count: value.count,
        p50_ms: value.p50_ms,
        p95_ms: value.p95_ms,
        p99_ms: value.p99_ms,
        max_ms: value.max_ms,
    }
}

/// 复制队列指标为统一占用结构。
fn map_queue(value: proto::RuntimeQueueMetrics) -> RuntimeOccupancySample {
    RuntimeOccupancySample {
        current: value.current,
        peak: value.peak,
        capacity: value.capacity,
        wait_latency: value.wait_latency.map(map_histogram),
        service_latency: value.service_latency.map(map_histogram),
    }
}

/// 复制资源指标为统一占用结构。
fn map_resource(value: proto::RuntimeResourceMetrics) -> RuntimeOccupancySample {
    RuntimeOccupancySample {
        current: value.current,
        peak: value.peak,
        capacity: value.capacity,
        wait_latency: value.wait_latency.map(map_histogram),
        service_latency: value.service_latency.map(map_histogram),
    }
}

/// 复制细分 ownership，严格保留协议中的 None 与 Some(0) 差异。
fn map_ownership(value: proto::RuntimeOwnershipMetrics) -> RuntimeOwnershipSample {
    RuntimeOwnershipSample {
        current: value.current,
        peak: value.peak,
        capacity: value.capacity,
    }
}

/// 复制完整流水线指标，保证队列、资源和吞吐字段逐项进入证据。
fn map_pipeline_metrics(value: proto::RuntimePipelineMetrics) -> RuntimePipelineMetricsSample {
    // 生产端当前使用 BTreeMap；证据边界仍显式排序，避免未来实现调整破坏 NDJSON 顺序。
    let mut disk_reads = value
        .disk_reads
        .into_iter()
        .map(|row| RuntimeDiskReadSample {
            physical_disk_id: row.physical_disk_id,
            capacity: row.capacity,
            hash_waiting: row.hash_waiting,
            media_waiting: row.media_waiting,
            hash_active: row.hash_active,
            media_active: row.media_active,
            hash_granted_total: row.hash_granted_total,
            media_granted_total: row.media_granted_total,
            hash_released_total: row.hash_released_total,
            media_released_total: row.media_released_total,
        })
        .collect::<Vec<_>>();
    disk_reads.sort_by(|left, right| left.physical_disk_id.cmp(&right.physical_disk_id));
    RuntimePipelineMetricsSample {
        hash_queue: value.hash_queue.map(map_queue),
        path_cache_queue: value.path_cache_queue.map(map_queue),
        content_cache_queue: value.content_cache_queue.map(map_queue),
        decode_queue: value.decode_queue.map(map_queue),
        persist_queue: value.persist_queue.map(map_queue),
        hash_io: value.hash_io.map(map_resource),
        media_io: value.media_io.map(map_resource),
        cpu_weight: value.cpu_weight.map(map_resource),
        worker_slots: value.worker_slots.map(map_resource),
        hash_bytes: value.hash_bytes,
        media_throughput: value
            .media_throughput
            .into_iter()
            .map(|row| RuntimeMediaThroughputSample {
                media_kind: row.media_kind,
                size_bucket: row.size_bucket,
                files: row.files,
                bytes: row.bytes,
            })
            .collect(),
        hash_waiting_permit: value.hash_waiting_permit.map(map_ownership),
        hash_reading: value.hash_reading.map(map_ownership),
        hash_completed_unjoined: value.hash_completed_unjoined.map(map_ownership),
        media_permit_waiting: value.media_permit_waiting.map(map_ownership),
        media_acquire_ready: value.media_acquire_ready.map(map_ownership),
        media_permit_ready: value.media_permit_ready.map(map_ownership),
        worker_dispatching: value.worker_dispatching.map(map_ownership),
        worker_start_pending: value.worker_start_pending.map(map_ownership),
        worker_decode: value.worker_decode.map(map_ownership),
        worker_feature: value.worker_feature.map(map_ownership),
        worker_result_wait: value.worker_result_wait.map(map_ownership),
        worker_phase_unknown: value.worker_phase_unknown.map(map_ownership),
        content_output_credit_owned: value.content_output_credit_owned.map(map_ownership),
        hash_refill_token_available: value.hash_refill_token_available.map(map_ownership),
        decode_credit_owned: value.decode_credit_owned.map(map_ownership),
        item_completion_latency: value.item_completion_latency.map(map_histogram),
        disk_reads,
    }
}

/// 半小时验收中一次实际创建的持久扫描及其运行终态。
#[derive(Clone, Debug, Serialize)]
pub struct RuntimeAcceptanceScan {
    /// 持久化任务 ID；创建成功后立即记录。
    pub persistent_task_id: String,
    /// 观察到的运行任务 ID；旧 Node 或尚未发现时保持 None。
    pub runtime_task_id: Option<String>,
    /// 最终状态；到期主动取消也记录真实 cancelled，而非覆盖 completed。
    pub terminal_state: Option<String>,
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
    /// 每一次成功创建扫描的持久化、运行时 ID 与终态记录。
    pub scan_tasks: Vec<RuntimeAcceptanceScan>,
    /// 最后一个真实 completed 扫描的持久任务 ID；其他终态不得覆盖它。
    pub latest_completed_persistent_task_id: Option<String>,
    /// 到期主动取消的持久任务 ID；与 completed 证据分开。
    pub deadline_cancelled_persistent_task_id: Option<String>,
    /// 没有任何 completed 扫描时，正确性证据明确为 INCONCLUSIVE。
    pub correctness: &'static str,
    /// 失败时写入稳定机器可读错误码；正常完成时为 null。
    pub fatal_error: Option<String>,
    /// 失败边界的稳定诊断码；不写入动态远端错误文本。
    pub diagnostic: Option<String>,
    /// 本次验收实际使用的媒体根列表。
    pub media_roots: Vec<String>,
    /// 本次验收是否在首个扫描终态后立即结束。
    pub single_run: bool,
}

/// NDJSON 或内存样本目的地。
pub trait AcceptanceSink {
    /// 写入一个一秒详情样本。
    fn write_sample(&mut self, sample: &RuntimeAcceptanceSample) -> Result<(), String>;
    /// 写入最终汇总；测试 sink 可使用默认空实现。
    fn write_result(&mut self, _result: &RuntimeAcceptanceResult) -> Result<(), String> {
        Ok(())
    }
}

/// 组装最终 runtime_result，统一正常完成与失败返回的字段顺序和语义。
fn build_runtime_result(
    config: &AcceptanceConfig,
    sample_count: u64,
    scans_started: u64,
    failed_scans: u64,
    cancelled_at_deadline: bool,
    scan_tasks: Vec<RuntimeAcceptanceScan>,
    latest_completed_persistent_task_id: Option<String>,
    deadline_cancelled_persistent_task_id: Option<String>,
    correctness: &'static str,
    fatal_error: Option<&'static str>,
    diagnostic: Option<&'static str>,
) -> RuntimeAcceptanceResult {
    RuntimeAcceptanceResult {
        record_type: "runtime_result",
        duration_seconds: config.duration().as_secs(),
        sample_count,
        scans_started,
        failed_scans,
        cancelled_at_deadline,
        scan_tasks,
        latest_completed_persistent_task_id,
        deadline_cancelled_persistent_task_id,
        correctness,
        fatal_error: fatal_error.map(str::to_owned),
        diagnostic: diagnostic.map(str::to_owned),
        media_roots: config.media_roots.clone(),
        single_run: config.single_run,
    }
}

/// 最终结果唯一写出边界；任何 sink/序列化/flush 细节都统一折叠为稳定错误码且不重试。
fn finalize_result<W: AcceptanceSink>(
    sink: &mut W,
    result: &RuntimeAcceptanceResult,
) -> Result<(), String> {
    sink.write_result(result)
        .map_err(|_| FATAL_RESULT_WRITE.to_owned())
}

/// 先写失败 runtime_result 再返回稳定 Err；调用者因此可用非零退出码标记验收未完成。
fn write_failure_result<W: AcceptanceSink>(
    sink: &mut W,
    config: &AcceptanceConfig,
    sample_count: u64,
    scans_started: u64,
    failed_scans: u64,
    scan_tasks: Vec<RuntimeAcceptanceScan>,
    latest_completed_persistent_task_id: Option<String>,
    deadline_cancelled_persistent_task_id: Option<String>,
    fatal_error: &'static str,
    diagnostic: &'static str,
) -> Result<RuntimeAcceptanceResult, String> {
    let result = build_runtime_result(
        config,
        sample_count,
        scans_started,
        failed_scans,
        deadline_cancelled_persistent_task_id.is_some(),
        scan_tasks,
        latest_completed_persistent_task_id,
        deadline_cancelled_persistent_task_id,
        "INCONCLUSIVE",
        Some(fatal_error),
        Some(diagnostic),
    );
    if let Err(error) = finalize_result(sink, &result) {
        return Err(error);
    }
    Err(fatal_error.into())
}

/// 持续运行到绝对单调截止时间；提前终态会创建下一次强制重算扫描。
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
    let mut scan_tasks = Vec::new();
    let mut current_scan_index = 0_usize;
    let mut runtime_task_id = None;
    let mut scans_started = 0_u64;
    let mut failed_scans = 0_u64;
    let mut sample_count = 0_u64;
    let mut current_terminal = false;
    let mut latest_completed_persistent_task_id = None;

    let mut known_runtime_ids = match session.list_runtime_tasks().await {
        Ok(tasks) => tasks
            .into_iter()
            .map(|task| task.runtime_task_id)
            .collect::<BTreeSet<_>>(),
        Err(_) => {
            return write_failure_result(
                &mut sink,
                config,
                sample_count,
                scans_started,
                failed_scans,
                scan_tasks,
                latest_completed_persistent_task_id,
                None,
                FATAL_RUNTIME_TASK_DISCOVERY,
                "runtime_task_list_request_failed",
            );
        }
    };
    let mut persistent_task_id = match session
        .create_scan(config.media_roots.clone(), false, config.enumerator())
        .await
    {
        Ok(task_id) => task_id,
        Err(_) => {
            return write_failure_result(
                &mut sink,
                config,
                sample_count,
                scans_started,
                failed_scans,
                scan_tasks,
                latest_completed_persistent_task_id,
                None,
                FATAL_CREATE_SCAN,
                "initial_create_scan_failed",
            );
        }
    };
    // create_scan 成功后立即建立记录，后续任何发现或详情错误都不会丢失持久 ID。
    scan_tasks.push(RuntimeAcceptanceScan {
        persistent_task_id: persistent_task_id.clone(),
        runtime_task_id: None,
        terminal_state: None,
    });
    scans_started = 1;
    // 计划 deadline 以真实单调 Instant 表示；测试时钟的 elapsed 只模拟同一条时间轴。
    let sampling_epoch = Instant::now();
    let sampling_deadline = sampling_epoch + config.duration();
    let sample_interval = config.sample_interval();
    let mut next_tick = sampling_epoch + sample_interval;
    // 仅在样本成功写出后更新，详情失败或 sink 失败不会污染上一成功边界。
    let mut previous_sample_at = None;

    while next_tick <= sampling_deadline {
        let now = sampling_epoch + clock.elapsed();
        if now > sampling_deadline {
            break;
        }
        // 只等待到绝对计划 tick；查询耗时已经消耗的时间不会再额外叠加一秒。
        if now < next_tick {
            clock.sleep(next_tick.duration_since(now)).await;
        }
        let now = sampling_epoch + clock.elapsed();
        if now > sampling_deadline {
            break;
        }

        if runtime_task_id.is_none() {
            runtime_task_id = match newest_base_compute_runtime(session, &known_runtime_ids).await {
                Ok(Some(runtime_task_id)) => Some(runtime_task_id),
                Ok(None) => {
                    return write_failure_result(
                        &mut sink,
                        config,
                        sample_count,
                        scans_started,
                        failed_scans,
                        scan_tasks,
                        latest_completed_persistent_task_id,
                        None,
                        FATAL_RUNTIME_TASK_DISCOVERY,
                        "runtime_task_not_observed",
                    );
                }
                Err(_) => {
                    return write_failure_result(
                        &mut sink,
                        config,
                        sample_count,
                        scans_started,
                        failed_scans,
                        scan_tasks,
                        latest_completed_persistent_task_id,
                        None,
                        FATAL_RUNTIME_TASK_DISCOVERY,
                        "runtime_task_list_request_failed",
                    );
                }
            };
            if let Some(runtime_task_id) = runtime_task_id.as_ref() {
                scan_tasks[current_scan_index].runtime_task_id = Some(runtime_task_id.clone());
            }
        }
        let Some(active_runtime_id) = runtime_task_id.as_deref() else {
            return write_failure_result(
                &mut sink,
                config,
                sample_count,
                scans_started,
                failed_scans,
                scan_tasks,
                latest_completed_persistent_task_id,
                None,
                FATAL_RUNTIME_TASK_DISCOVERY,
                "runtime_task_not_observed",
            );
        };
        let details = match session.runtime_task_details(active_runtime_id).await {
            Ok(details) => details,
            Err(_) => {
                return write_failure_result(
                    &mut sink,
                    config,
                    sample_count,
                    scans_started,
                    failed_scans,
                    scan_tasks,
                    latest_completed_persistent_task_id,
                    None,
                    FATAL_RUNTIME_TASK_DETAILS,
                    "runtime_task_details_request_failed",
                );
            }
        };
        let sample_elapsed = clock.elapsed();
        let sample_at = sampling_epoch + sample_elapsed;
        let sample_interval_ms = previous_sample_at
            .map(|previous| sample_at.saturating_duration_since(previous).as_millis() as u64)
            .unwrap_or(0);
        let mut sample = match RuntimeAcceptanceSample::from_details(sample_elapsed, details) {
            Ok(sample) => sample,
            Err(_) => {
                return write_failure_result(
                    &mut sink,
                    config,
                    sample_count,
                    scans_started,
                    failed_scans,
                    scan_tasks,
                    latest_completed_persistent_task_id,
                    None,
                    FATAL_RUNTIME_TASK_DETAILS_INVALID,
                    "runtime_task_summary_missing",
                );
            }
        };
        sample.sample_interval_ms = sample_interval_ms;
        current_terminal = is_terminal(&sample.state);
        if current_terminal {
            scan_tasks[current_scan_index].terminal_state = Some(sample.state.clone());
            if sample.state == "completed" {
                latest_completed_persistent_task_id = Some(persistent_task_id.clone());
            }
        }
        if sample.state == "failed" {
            failed_scans += 1;
        }
        if sink.write_sample(&sample).is_err() {
            return write_failure_result(
                &mut sink,
                config,
                sample_count,
                scans_started,
                failed_scans,
                scan_tasks,
                latest_completed_persistent_task_id,
                None,
                FATAL_SAMPLE_WRITE,
                "runtime_sample_write_failed",
            );
        }
        // 只有 write_sample 成功后才推进真实相邻输出边界和样本计数。
        previous_sample_at = Some(sample_at);
        sample_count += 1;
        next_tick += sample_interval;

        if current_terminal && config.single_run {
            let correctness = if failed_scans > 0 {
                "FAIL"
            } else if latest_completed_persistent_task_id.is_some() {
                "PASS"
            } else {
                "INCONCLUSIVE"
            };
            let result = build_runtime_result(
                config,
                sample_count,
                scans_started,
                failed_scans,
                false,
                scan_tasks,
                latest_completed_persistent_task_id,
                None,
                correctness,
                None,
                None,
            );
            finalize_result(&mut sink, &result)?;
            return Ok(result);
        }

        if current_terminal && sampling_epoch + clock.elapsed() < sampling_deadline {
            known_runtime_ids.insert(active_runtime_id.into());
            let next_persistent_task_id = match session
                .create_scan(config.media_roots.clone(), true, config.enumerator())
                .await
            {
                Ok(task_id) => task_id,
                Err(_) => {
                    return write_failure_result(
                        &mut sink,
                        config,
                        sample_count,
                        scans_started,
                        failed_scans,
                        scan_tasks,
                        latest_completed_persistent_task_id,
                        None,
                        FATAL_CREATE_SCAN,
                        "forced_recalculate_create_failed",
                    );
                }
            };
            // 新扫描也在 create_scan 成功的同一边界追加，保证错误时保留所有已知 ID。
            scan_tasks.push(RuntimeAcceptanceScan {
                persistent_task_id: next_persistent_task_id.clone(),
                runtime_task_id: None,
                terminal_state: None,
            });
            current_scan_index = scan_tasks.len() - 1;
            persistent_task_id = next_persistent_task_id;
            scans_started += 1;
            runtime_task_id = None;
            current_terminal = false;
        }
    }

    let mut deadline_cancelled_persistent_task_id = None;
    if !current_terminal {
        if session.cancel_task(&persistent_task_id).await.is_err() {
            return write_failure_result(
                &mut sink,
                config,
                sample_count,
                scans_started,
                failed_scans,
                scan_tasks,
                latest_completed_persistent_task_id,
                None,
                FATAL_CANCEL_TASK,
                "deadline_cancel_request_failed",
            );
        }
        // cancel 成功即单独保留主动取消 ID，即使之后竞态观察到 completed 也不覆盖它。
        deadline_cancelled_persistent_task_id = Some(persistent_task_id.clone());
        if let Some(runtime_task_id) = runtime_task_id.as_deref() {
            let terminal_state =
                match wait_for_terminal(session, clock, runtime_task_id, sample_interval).await {
                    Ok(state) => state,
                    Err(_) => {
                        return write_failure_result(
                            &mut sink,
                            config,
                            sample_count,
                            scans_started,
                            failed_scans,
                            scan_tasks,
                            latest_completed_persistent_task_id,
                            deadline_cancelled_persistent_task_id,
                            FATAL_TERMINAL_WAIT_TIMEOUT,
                            "runtime_terminal_state_unobserved",
                        );
                    }
                };
            scan_tasks[current_scan_index].terminal_state = Some(terminal_state.clone());
            // 取消后的最后一次详情也是真实 completed，必须同步进入 PASS 证据。
            if terminal_state == "completed" {
                latest_completed_persistent_task_id = Some(persistent_task_id.clone());
            }
            // deadline 取消后的 failed 仍是业务失败，不能被主动取消 ID 豁免。
            if terminal_state == "failed" {
                failed_scans += 1;
            }
        }
    }
    let result = build_runtime_result(
        config,
        sample_count,
        scans_started,
        failed_scans,
        deadline_cancelled_persistent_task_id.is_some(),
        scan_tasks,
        latest_completed_persistent_task_id.clone(),
        deadline_cancelled_persistent_task_id,
        if failed_scans > 0 {
            "FAIL"
        } else if latest_completed_persistent_task_id.is_some() {
            "PASS"
        } else {
            "INCONCLUSIVE"
        },
        None,
        None,
    );
    finalize_result(&mut sink, &result)?;
    Ok(result)
}

/// 从新出现的运行任务中仅定位基础计算，保持与 CreateScan 的运行契约一致。
async fn newest_base_compute_runtime<S: AcceptanceSession>(
    session: &S,
    known: &BTreeSet<String>,
) -> Result<Option<String>, String> {
    Ok(session
        .list_runtime_tasks()
        .await?
        .into_iter()
        .find(|task| task.task_kind == "base_compute" && !known.contains(&task.runtime_task_id))
        .map(|task| task.runtime_task_id))
}

async fn wait_for_terminal<S: AcceptanceSession, C: AcceptanceClock>(
    session: &S,
    clock: &C,
    runtime_task_id: &str,
    interval: Duration,
) -> Result<String, String> {
    for _ in 0..TERMINAL_WAIT_TICKS {
        let details = session.runtime_task_details(runtime_task_id).await?;
        let state = details
            .summary
            .ok_or_else(|| "取消后的运行详情缺少摘要".to_string())?
            .state;
        if is_terminal(&state) {
            return Ok(state);
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

/// 解析多媒体根 JSON，并拒绝空数组或空白根。
fn parse_media_roots_json(value: &str) -> Result<Vec<String>, String> {
    let roots = serde_json::from_str::<Vec<String>>(value).map_err(|_| INVALID_MEDIA_ROOTS)?;
    if roots.is_empty() || roots.iter().any(|root| root.trim().is_empty()) {
        return Err(INVALID_MEDIA_ROOTS.into());
    }
    Ok(roots)
}

/// 解析单轮模式环境变量；缺失或空白值保持持续运行默认行为。
fn parse_single_run(value: Option<String>) -> Result<bool, String> {
    let Some(value) = value else {
        return Ok(false);
    };
    let value = value.trim();
    if value.is_empty() {
        return Ok(false);
    }
    if value == "1" || value.eq_ignore_ascii_case("true") {
        return Ok(true);
    }
    Err(INVALID_SINGLE_RUN.into())
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

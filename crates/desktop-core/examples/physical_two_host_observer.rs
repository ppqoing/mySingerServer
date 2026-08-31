//! GUI 退出后严格顺序读取两台 Node 终态的外置 NDJSON 观察器。

use std::{
    env,
    fs::{File, OpenOptions},
    io::{BufWriter, Write},
    net::SocketAddr,
    path::{Component, Path, PathBuf},
};

use dedup_core::{CoreError, MachineId, NodeEndpoint, product_id};
use dedup_protocol::{PROTOCOL_VERSION, proto};
use dedup_transport::{FrameClass, FrameReader, FrameWriter};
use prost::Message;
use serde::Serialize;
use serde_json::{Value, json};
use thiserror::Error;
use tokio::{
    net::{
        TcpStream,
        tcp::{OwnedReadHalf, OwnedWriteHalf},
    },
    task::yield_now,
};

const FIRST_ENDPOINT_ENV: &str = "DEDUP_OBSERVER_FIRST_ENDPOINT";
const SECOND_ENDPOINT_ENV: &str = "DEDUP_OBSERVER_SECOND_ENDPOINT";
const EVIDENCE_DIR_ENV: &str = "DEDUP_OBSERVER_EVIDENCE_DIR";
const OUTPUT_FILE_ENV: &str = "DEDUP_OBSERVER_OUTPUT_FILE";
const READ_PAGE_LIMIT: u32 = 100;
const NODE_BUSY_MESSAGE: &str = "节点正被 GUI 的唯一管理连接占用；请完全退出 GUI 后再观察";

/// 观察器的两个手工端点和由调用方预先创建的隔离证据目录。
pub struct ObserverConfig {
    endpoints: [NodeEndpoint; 2],
    output: PathBuf,
}

impl ObserverConfig {
    /// 验证输出文件名，并把输出目录规范化为调用方指定的已存在证据目录。
    pub fn new(
        first: NodeEndpoint,
        second: NodeEndpoint,
        evidence_dir: &Path,
        output_file: &str,
    ) -> Result<Self, ObserverError> {
        let output_file = Path::new(output_file);
        if output_file.as_os_str().is_empty()
            || output_file.components().count() != 1
            || !matches!(output_file.components().next(), Some(Component::Normal(_)))
        {
            return Err(ObserverError::new(
                "invalid_output_file",
                "输出文件必须是证据目录内的单个文件名",
            ));
        }
        let evidence_dir = evidence_dir.canonicalize().map_err(|error| {
            ObserverError::new(
                "invalid_evidence_directory",
                format!("无法规范化证据目录 {}：{error}", evidence_dir.display()),
            )
        })?;
        if !evidence_dir.is_dir() {
            return Err(ObserverError::new(
                "invalid_evidence_directory",
                "证据目录不是目录",
            ));
        }
        Ok(Self {
            endpoints: [first, second],
            output: evidence_dir.join(output_file),
        })
    }

    /// 从四个显式环境变量读取观察器输入，不接触桌面配置或媒体路径。
    pub fn from_env() -> Result<Self, ObserverError> {
        Self::new(
            endpoint_env(FIRST_ENDPOINT_ENV)?,
            endpoint_env(SECOND_ENDPOINT_ENV)?,
            Path::new(&required_env(EVIDENCE_DIR_ENV)?),
            &required_env(OUTPUT_FILE_ENV)?,
        )
    }
}

/// 一次终态观察结束后的稳定结果摘要。
#[derive(Debug, Eq, PartialEq)]
pub struct ObserverResult {
    /// 成功读取两个节点时为 `completed`。
    pub status: String,
    /// 已写入 `node_snapshot` 的节点数。
    pub observed_nodes: usize,
}

/// 观察器在协议、输入或证据写入边界遇到的稳定诊断。
#[derive(Debug, Error)]
#[error("{code}: {message}")]
pub struct ObserverError {
    code: &'static str,
    message: String,
}

impl ObserverError {
    /// 返回机器可读的稳定诊断代码。
    pub const fn code(&self) -> &'static str {
        self.code
    }

    /// 构造一个不依赖远端内部实现的观察器诊断。
    fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}

/// 严格先后连接两个节点，只发送 Hello、状态和任务/详情读取，并逐行写入 NDJSON。
pub async fn run_observer(config: ObserverConfig) -> Result<ObserverResult, ObserverError> {
    let mut sink = NdjsonSink::create(&config.output)?;
    sink.write(&json!({
        "record_type": "observer_start",
        "product_id": product_id(),
        "protocol_version": PROTOCOL_VERSION,
        "endpoints": config.endpoints.iter().map(endpoint_text).collect::<Vec<_>>(),
    }))?;

    let mut observed_nodes = 0;
    for endpoint in &config.endpoints {
        match observe_node(endpoint).await {
            Ok(snapshot) => {
                sink.write(&snapshot)?;
                observed_nodes += 1;
            }
            Err(error) => {
                sink.write(&json!({
                    "record_type": "observer_error",
                    "endpoint": endpoint_text(endpoint),
                    "code": error.code,
                    "message": error.message,
                }))?;
                sink.write(&json!({
                    "record_type": "observer_result",
                    "status": "failed",
                    "observed_nodes": observed_nodes,
                    "failure_code": error.code,
                }))?;
                return Err(error);
            }
        }
    }
    let result = ObserverResult {
        status: "completed".into(),
        observed_nodes,
    };
    sink.write(&json!({
        "record_type": "observer_result",
        "status": result.status,
        "observed_nodes": result.observed_nodes,
    }))?;
    Ok(result)
}

/// 在当前节点完整读取后显式释放会话，才允许调用方连接下一个节点。
async fn observe_node(endpoint: &NodeEndpoint) -> Result<Value, ObserverError> {
    let mut session = ReadonlySession::connect(endpoint.clone()).await?;
    let status = session.status().await?;
    let persistent_tasks = session.list_tasks("", READ_PAGE_LIMIT).await?;
    let runtime_tasks = session.list_runtime_tasks("", READ_PAGE_LIMIT).await?;
    let mut runtime_snapshots = Vec::with_capacity(runtime_tasks.tasks.len());
    for summary in runtime_tasks.tasks {
        let details = session
            .runtime_task_details(&summary.runtime_task_id)
            .await?;
        runtime_snapshots.push(runtime_task_value(&summary, &details));
    }
    let machine_id = session.machine_id()?.as_str().to_owned();
    drop(session);
    // 让本轮已释放的 socket 先交还给运行时，再发起下一端点的 TCP connect。
    yield_now().await;

    Ok(json!({
        "record_type": "node_snapshot",
        "endpoint": endpoint_text(endpoint),
        "machine_id": machine_id,
        "product_id": product_id(),
        "protocol_version": PROTOCOL_VERSION,
        "node_status": status_value(&status),
        "persistent_task_page": {
            "available": true,
            "limit": READ_PAGE_LIMIT,
            "next_cursor": persistent_tasks.next_cursor,
            "tasks": persistent_tasks.tasks.iter().map(task_value).collect::<Vec<_>>(),
        },
        "latest_persistent_task": unavailable("协议未提供任务创建时间或最新排序语义"),
        "runtime_tasks": runtime_snapshots,
    }))
}

/// 单请求单响应的只读 TCP 会话；拥有收发半边以便 Drop 同步释放整个 TCP 连接。
struct ReadonlySession {
    machine_id: Option<MachineId>,
    next_request_id: u64,
    reader: FrameReader<OwnedReadHalf>,
    writer: FrameWriter<OwnedWriteHalf>,
}

impl ReadonlySession {
    /// 建连后完成固定 Hello 与身份状态读取；不启动后台循环或任何业务命令。
    async fn connect(endpoint: NodeEndpoint) -> Result<Self, ObserverError> {
        let stream = TcpStream::connect(SocketAddr::new(endpoint.ip, endpoint.port))
            .await
            .map_err(|error| ObserverError::new("observer_read_failed", error.to_string()))?;
        let (read, write) = stream.into_split();
        let mut session = Self {
            machine_id: None,
            next_request_id: 1,
            reader: FrameReader::new(read),
            writer: FrameWriter::new(write),
        };
        let hello = session
            .request(proto::envelope::Payload::Hello(proto::Hello {
                protocol_version: PROTOCOL_VERSION,
                product_id: product_id().into(),
                peer_name: "physical-two-host-observer".into(),
            }))
            .await?;
        match hello {
            proto::envelope::Payload::Hello(response)
                if response.protocol_version == PROTOCOL_VERSION
                    && response.product_id == product_id() => {}
            proto::envelope::Payload::Hello(_) => {
                return Err(ObserverError::new(
                    "unexpected_response",
                    "节点 Hello 的产品或协议版本不匹配",
                ));
            }
            _ => return Err(ObserverError::new("unexpected_response", "期望 Hello 响应")),
        }
        let status = session.status().await?;
        session.machine_id = Some(MachineId::parse(&status.machine_id).map_err(core_error)?);
        Ok(session)
    }

    /// 返回握手状态中验证过的物理机器身份。
    fn machine_id(&self) -> Result<&MachineId, ObserverError> {
        self.machine_id
            .as_ref()
            .ok_or_else(|| ObserverError::new("invalid_response", "握手后缺少机器身份"))
    }

    /// 读取当前节点状态统计。
    async fn status(&mut self) -> Result<proto::NodeStatus, ObserverError> {
        match self
            .request(proto::envelope::Payload::NodeStatus(Default::default()))
            .await?
        {
            proto::envelope::Payload::NodeStatus(status) => Ok(status),
            _ => Err(ObserverError::new(
                "unexpected_response",
                "期望 NodeStatus 响应",
            )),
        }
    }

    /// 读取一页持久任务，不发送创建、同步、分析、删除或配置命令。
    async fn list_tasks(
        &mut self,
        cursor: &str,
        limit: u32,
    ) -> Result<proto::ListTasks, ObserverError> {
        match self
            .request(proto::envelope::Payload::ListTasks(proto::ListTasks {
                cursor: cursor.into(),
                limit,
                tasks: Vec::new(),
                next_cursor: String::new(),
            }))
            .await?
        {
            proto::envelope::Payload::ListTasks(page) => Ok(page),
            _ => Err(ObserverError::new(
                "unexpected_response",
                "期望 ListTasks 响应",
            )),
        }
    }

    /// 读取一页进程内运行任务摘要。
    async fn list_runtime_tasks(
        &mut self,
        cursor: &str,
        limit: u32,
    ) -> Result<proto::ListRuntimeTasks, ObserverError> {
        match self
            .request(proto::envelope::Payload::ListRuntimeTasks(
                proto::ListRuntimeTasks {
                    cursor: cursor.into(),
                    limit,
                    tasks: Vec::new(),
                    next_cursor: String::new(),
                },
            ))
            .await?
        {
            proto::envelope::Payload::ListRuntimeTasks(page) => Ok(page),
            _ => Err(ObserverError::new(
                "unexpected_response",
                "期望 ListRuntimeTasks 响应",
            )),
        }
    }

    /// 读取一个运行任务的阶段、资源和逐盘详情。
    async fn runtime_task_details(
        &mut self,
        runtime_task_id: &str,
    ) -> Result<proto::RuntimeTaskDetails, ObserverError> {
        match self
            .request(proto::envelope::Payload::GetRuntimeTaskDetails(
                proto::GetRuntimeTaskDetails {
                    runtime_task_id: runtime_task_id.into(),
                    details: None,
                },
            ))
            .await?
        {
            proto::envelope::Payload::GetRuntimeTaskDetails(response) => response
                .details
                .ok_or_else(|| ObserverError::new("invalid_response", "运行任务详情为空")),
            _ => Err(ObserverError::new(
                "unexpected_response",
                "期望 GetRuntimeTaskDetails 响应",
            )),
        }
    }

    /// 严格发送一个请求并读取相同 request ID 的单个响应，主动事件一律拒绝。
    async fn request(
        &mut self,
        payload: proto::envelope::Payload,
    ) -> Result<proto::envelope::Payload, ObserverError> {
        let request_id = self.next_request_id;
        self.next_request_id = self.next_request_id.checked_add(1).unwrap_or(1);
        self.writer
            .write_frame(
                &proto::Envelope {
                    request_id,
                    payload: Some(payload),
                }
                .encode_to_vec(),
                FrameClass::Ordinary,
            )
            .await
            .map_err(frame_error)?;
        let frame = self.reader.read_frame().await.map_err(frame_error)?;
        let response = proto::Envelope::decode(frame.as_slice()).map_err(|error| {
            ObserverError::new("invalid_response", format!("Envelope 解码失败：{error}"))
        })?;
        if response.request_id != request_id {
            return Err(ObserverError::new(
                "invalid_response",
                "只读会话收到了不匹配 request_id 的响应或主动事件",
            ));
        }
        let payload = response
            .payload
            .ok_or_else(|| ObserverError::new("invalid_response", "响应缺少 payload"))?;
        match payload {
            proto::envelope::Payload::Error(error)
                if error.code == proto::ErrorCode::NodeBusy as i32 =>
            {
                Err(ObserverError::new("node_busy", NODE_BUSY_MESSAGE))
            }
            proto::envelope::Payload::Error(error) => Err(ObserverError::new(
                "node_protocol_error",
                format!("节点协议错误 {}: {}", error.code, error.message),
            )),
            payload => Ok(payload),
        }
    }
}

/// 转换核心路径或 MachineId 验证错误，保持外置观察器的稳定代码。
fn core_error(error: CoreError) -> ObserverError {
    ObserverError::new("invalid_response", error.to_string())
}

/// 转换分帧读写错误，避免把运输层类型泄露为观察器 API。
fn frame_error(error: dedup_transport::FrameError) -> ObserverError {
    ObserverError::new("observer_read_failed", error.to_string())
}

/// 转换 NodeStatus 中协议明确给出的节点终态统计。
fn status_value(status: &proto::NodeStatus) -> Value {
    json!({
        "listen_address": status.listen_address,
        "worker_count": status.worker_count,
        "busy_workers": status.busy_workers,
        "queued_items": status.queued_items,
        "running_items": status.running_items,
        "outbox_high_seq": status.outbox_high_seq,
        "engine_restarting": status.engine_restarting,
    })
}

/// 转换持久任务统计；阶段字段不在 TaskSummary 协议内，不能伪造。
fn task_value(task: &proto::TaskSummary) -> Value {
    json!({
        "task_id": task.task_id,
        "task_kind": task.task_kind,
        "state": task_state(task.state),
        "total_items": task.total_items,
        "completed_items": task.completed_items,
        "failed_items": task.failed_items,
        "skipped_items": task.skipped_items,
        "outbox_high_seq": task.outbox_high_seq,
        "stage": unavailable("协议 TaskSummary 未提供阶段"),
    })
}

/// 转换运行时任务摘要、阶段和当前进程遥测资源。
fn runtime_task_value(
    summary: &proto::RuntimeTaskSummary,
    details: &proto::RuntimeTaskDetails,
) -> Value {
    json!({
        "summary": {
            "runtime_task_id": summary.runtime_task_id,
            "machine_id": summary.machine_id,
            "task_kind": summary.task_kind,
            "title": summary.title,
            "state": summary.state,
            "stage_summary": summary.stage_summary,
            "overall_completed": summary.overall_completed,
            "overall_total": summary.overall_total,
            "overall_total_known": summary.overall_total_known,
            "overall_failed": summary.overall_failed,
            "overall_skipped": summary.overall_skipped,
            "outbox_high_seq": summary.outbox_high_seq,
        },
        "stages": details.stages.iter().map(stage_value).collect::<Vec<_>>(),
        "execution_config": execution_config_value(details.execution_config.as_ref()),
        "pipeline_metrics": pipeline_metrics_value(details.pipeline_metrics.as_ref()),
    })
}

/// 转换协议提供的运行阶段，不补写不存在的 ETA 或业务字段。
fn stage_value(stage: &proto::RuntimeStageDetails) -> Value {
    json!({
        "stage_id": stage.stage_id,
        "display_name": stage.display_name,
        "state": runtime_stage_state(stage.state),
        "unit": stage.unit,
        "completed": stage.completed,
        "total": stage.total,
        "total_known": stage.total_known,
        "failed": stage.failed,
        "skipped": stage.skipped,
        "speed_per_second": stage.speed_per_second,
        "elapsed_ms": stage.elapsed_ms,
        "eta_ms": stage.eta_ms,
    })
}

/// 转换基础计算的固定硬上限；无进程遥测时显式标记不可用。
fn execution_config_value(config: Option<&proto::RuntimeExecutionConfig>) -> Value {
    let Some(config) = config else {
        return unavailable("当前协议响应未提供运行配置遥测");
    };
    json!({
        "available": true,
        "hash_tasks": config.hash_tasks,
        "worker_slots": config.worker_slots,
        "cpu_budget": config.cpu_budget,
        "global_disk_permits": config.global_disk_permits,
        "hdd_per_disk_permits": config.hdd_per_disk_permits,
        "ssd_per_disk_permits": config.ssd_per_disk_permits,
        "unknown_per_disk_permits": config.unknown_per_disk_permits,
    })
}

/// 转换资源占用和逐盘读取指标；整个遥测消息缺失时保持缺失原因。
fn pipeline_metrics_value(metrics: Option<&proto::RuntimePipelineMetrics>) -> Value {
    let Some(metrics) = metrics else {
        return unavailable("当前协议响应未提供流水线遥测");
    };
    json!({
        "available": true,
        "resources": {
            "hash_io": resource_value(metrics.hash_io.as_ref()),
            "media_io": resource_value(metrics.media_io.as_ref()),
            "cpu_weight": resource_value(metrics.cpu_weight.as_ref()),
            "worker_slots": resource_value(metrics.worker_slots.as_ref()),
        },
        "hash_bytes": metrics.hash_bytes,
        "disk_reads": metrics.disk_reads.iter().map(disk_read_value).collect::<Vec<_>>(),
    })
}

/// 转换一类共享资源的当前、峰值和容量。
fn resource_value(resource: Option<&proto::RuntimeResourceMetrics>) -> Value {
    let Some(resource) = resource else {
        return unavailable("当前协议响应未提供该资源遥测");
    };
    json!({
        "available": true,
        "current": resource.current,
        "peak": resource.peak,
        "capacity": resource.capacity,
    })
}

/// 转换一个物理盘真实读取许可的瞬时和累计指标。
fn disk_read_value(disk: &proto::RuntimeDiskReadMetrics) -> Value {
    json!({
        "physical_disk_id": disk.physical_disk_id,
        "capacity": disk.capacity,
        "hash_waiting": disk.hash_waiting,
        "media_waiting": disk.media_waiting,
        "hash_active": disk.hash_active,
        "media_active": disk.media_active,
        "hash_granted_total": disk.hash_granted_total,
        "media_granted_total": disk.media_granted_total,
        "hash_released_total": disk.hash_released_total,
        "media_released_total": disk.media_released_total,
    })
}

/// 生成所有协议无法表达字段共同使用的可机读缺失形状。
fn unavailable(reason: &'static str) -> Value {
    json!({"available": false, "reason": reason})
}

/// 将协议任务状态稳定转换为其枚举名称。
fn task_state(value: i32) -> &'static str {
    match proto::TaskState::try_from(value).unwrap_or(proto::TaskState::Unspecified) {
        proto::TaskState::TaskQueued => "queued",
        proto::TaskState::TaskRunning => "running",
        proto::TaskState::TaskCompleted => "completed",
        proto::TaskState::TaskFailed => "failed",
        proto::TaskState::TaskCancelled => "cancelled",
        proto::TaskState::Unspecified => "unspecified",
    }
}

/// 将协议运行阶段状态稳定转换为其枚举名称。
fn runtime_stage_state(value: i32) -> &'static str {
    match proto::RuntimeStageState::try_from(value).unwrap_or(proto::RuntimeStageState::Unspecified)
    {
        proto::RuntimeStageState::RuntimeStageWaiting => "waiting",
        proto::RuntimeStageState::RuntimeStageRunning => "running",
        proto::RuntimeStageState::RuntimeStageCompleted => "completed",
        proto::RuntimeStageState::RuntimeStageFailed => "failed",
        proto::RuntimeStageState::RuntimeStageSkipped => "skipped",
        proto::RuntimeStageState::Unspecified => "unspecified",
    }
}

/// 把手工配置端点格式化为 NDJSON 使用的稳定 IP:port 文本。
fn endpoint_text(endpoint: &NodeEndpoint) -> String {
    SocketAddr::new(endpoint.ip, endpoint.port).to_string()
}

/// 读取一个非空环境变量。
fn required_env(name: &str) -> Result<String, ObserverError> {
    let value = env::var(name)
        .map_err(|_| ObserverError::new("missing_environment", format!("缺少环境变量 {name}")))?;
    if value.trim().is_empty() {
        return Err(ObserverError::new(
            "missing_environment",
            format!("环境变量 {name} 不能为空"),
        ));
    }
    Ok(value)
}

/// 解析并验证一个不可为零端口的手工节点端点。
fn endpoint_env(name: &str) -> Result<NodeEndpoint, ObserverError> {
    let raw = required_env(name)?;
    let address = raw.parse::<SocketAddr>().map_err(|error| {
        ObserverError::new(
            "invalid_endpoint",
            format!("环境变量 {name} 必须是 IP:port：{error}"),
        )
    })?;
    if address.port() == 0 {
        return Err(ObserverError::new(
            "invalid_endpoint",
            format!("环境变量 {name} 端口不能为 0"),
        ));
    }
    Ok(NodeEndpoint {
        ip: address.ip(),
        port: address.port(),
    })
}

/// 逐行创建并刷新 NDJSON，拒绝覆盖已有验收证据。
struct NdjsonSink {
    writer: BufWriter<File>,
}

impl NdjsonSink {
    /// 在已规范化的调用方目录内新建唯一证据文件。
    fn create(path: &Path) -> Result<Self, ObserverError> {
        let file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(path)
            .map_err(|error| {
                ObserverError::new(
                    "evidence_write_failed",
                    format!("无法创建证据文件 {}：{error}", path.display()),
                )
            })?;
        Ok(Self {
            writer: BufWriter::new(file),
        })
    }

    /// 序列化单条记录并立即落盘，保留中途失败前的证据。
    fn write<T: Serialize>(&mut self, record: &T) -> Result<(), ObserverError> {
        serde_json::to_writer(&mut self.writer, record).map_err(|error| {
            ObserverError::new("evidence_write_failed", format!("无法序列化证据：{error}"))
        })?;
        self.writer
            .write_all(b"\n")
            .and_then(|_| self.writer.flush())
            .map_err(|error| {
                ObserverError::new("evidence_write_failed", format!("无法刷新证据：{error}"))
            })
    }
}

/// 从显式环境变量启动观察器，适合在 GUI 已完全退出后单独调用。
#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let result = run_observer(ObserverConfig::from_env()?).await?;
    println!(
        "PHYSICAL_TWO_HOST_OBSERVER_RESULT status={} observed_nodes={}",
        result.status, result.observed_nodes
    );
    Ok(())
}

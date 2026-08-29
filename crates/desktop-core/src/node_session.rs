//! 一个手工配置节点的持久 TCP 会话，以及严格的 V2 Hello 响应检查。

use std::{net::SocketAddr, sync::Arc, time::Duration};

use dedup_core::{
    AnalysisRunId, CoreError, DeleteMode, LocationKey, MachineId, NodeEndpoint, TaskId, product_id,
};
use dedup_protocol::{PROTOCOL_VERSION, proto};
use dedup_transport::{ClientConnection, TransportError};
use thiserror::Error;
use tokio::sync::watch;

/// 建连、握手、协议错误响应或领域值解码失败。
#[derive(Debug, Error)]
pub enum SessionError {
    /// TCP、分帧或 Protobuf 连接已经失败；当前会话不能继续复用。
    #[error(transparent)]
    Transport(#[from] TransportError),
    /// 节点返回了显式协议错误。
    #[error("节点协议错误 {code}: {message}")]
    Protocol {
        /// `ErrorCode` 的整数值，保留未知新值。
        code: i32,
        /// 节点提供的简短原因。
        message: String,
    },
    /// 响应类型与请求约定不一致。
    #[error("节点返回了意外响应，期望 {0}")]
    UnexpectedResponse(&'static str),
    /// 节点状态中的机器 ID 不是规范物理标识。
    #[error(transparent)]
    Core(#[from] CoreError),
    /// 节点响应中的任务 ID 不是 UUID。
    #[error("节点返回无效任务 ID: {0}")]
    InvalidTaskId(String),
    /// 节点返回的结构字段未满足当前协议约定。
    #[error("节点响应无效: {0}")]
    InvalidResponse(String),
}

/// `desktop.exe` 与一个手工 IP:port 之间的已握手 V2 连接。
///
/// 任一 `Transport` 错误都会结束当前调用；上层会话监督器按配置的固定间隔重新调用
/// `connect`。本类型不重建、不重试节点任务，也不维护第二份任务状态。
pub struct NodeSession {
    endpoint: NodeEndpoint,
    machine_id: MachineId,
    connection: Arc<ClientConnection>,
}

impl NodeSession {
    /// 连接节点，发送必须为首帧的 Hello，再读取 NodeStatus 固定物理机器身份。
    pub async fn connect(endpoint: NodeEndpoint) -> Result<Self, SessionError> {
        let address = SocketAddr::new(endpoint.ip, endpoint.port);
        let connection = Arc::new(ClientConnection::connect(address).await?);
        let hello = connection
            .request(proto::envelope::Payload::Hello(proto::Hello {
                protocol_version: PROTOCOL_VERSION,
                product_id: product_id().into(),
                peer_name: "desktop".into(),
            }))
            .await?;
        match payload_or_error(hello)? {
            proto::envelope::Payload::Hello(response)
                if response.protocol_version == PROTOCOL_VERSION
                    && response.product_id == product_id() => {}
            proto::envelope::Payload::Hello(_) => {
                return Err(SessionError::UnexpectedResponse("匹配的 V2 Hello"));
            }
            _ => return Err(SessionError::UnexpectedResponse("Hello")),
        }
        let status = connection
            .request(proto::envelope::Payload::NodeStatus(Default::default()))
            .await?;
        let machine_id = match payload_or_error(status)? {
            proto::envelope::Payload::NodeStatus(status) => MachineId::parse(&status.machine_id)?,
            _ => return Err(SessionError::UnexpectedResponse("NodeStatus")),
        };
        Ok(Self {
            endpoint,
            machine_id,
            connection,
        })
    }

    /// 按配置的固定间隔重复建立同一手工端点会话，关闭信号到达时返回 `None`。
    ///
    /// 每次失败只通过回调更新 UI/日志；本函数不创建、恢复或重试任何远端业务任务。已连接
    /// 会话随后发生 `Transport` 错误时，上层丢弃旧实例并再次调用本函数。
    pub async fn connect_with_retry<F>(
        endpoint: NodeEndpoint,
        retry_interval: Duration,
        shutdown: &mut watch::Receiver<bool>,
        mut on_error: F,
    ) -> Option<Self>
    where
        F: FnMut(&SessionError),
    {
        loop {
            if *shutdown.borrow() {
                return None;
            }
            match Self::connect(endpoint.clone()).await {
                Ok(session) => return Some(session),
                Err(error) => on_error(&error),
            }
            tokio::select! {
                _ = tokio::time::sleep(retry_interval) => {}
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        return None;
                    }
                }
            }
        }
    }

    /// 返回配置中的手工节点地址。
    pub const fn endpoint(&self) -> &NodeEndpoint {
        &self.endpoint
    }

    /// 返回握手后从节点状态取得的物理机器 ID。
    pub const fn machine_id(&self) -> &MachineId {
        &self.machine_id
    }

    /// 刷新节点 Worker、任务和 outbox 统计。
    pub async fn status(&self) -> Result<proto::NodeStatus, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::NodeStatus(Default::default()))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::NodeStatus(status) => Ok(status),
            _ => Err(SessionError::UnexpectedResponse("NodeStatus")),
        }
    }

    /// 读取节点当前原始配置、版本摘要与有效 Worker 快照。
    pub async fn get_node_config(&self) -> Result<proto::NodeConfigSnapshot, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::GetNodeConfig(
                proto::GetNodeConfig {},
            ))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::NodeConfigSnapshot(snapshot) => Ok(snapshot),
            _ => Err(SessionError::UnexpectedResponse("NodeConfigSnapshot")),
        }
    }

    /// 携带加载时摘要请求节点原子保存配置；新配置在 Node 重启后生效。
    pub async fn save_node_config(
        &self,
        expected_version_sha256: &str,
        config: proto::NodeConfigValue,
    ) -> Result<proto::NodeConfigSaved, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::SaveNodeConfig(
                proto::SaveNodeConfig {
                    expected_version_sha256: expected_version_sha256.into(),
                    config: Some(config),
                },
            ))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::NodeConfigSaved(saved) => Ok(saved),
            _ => Err(SessionError::UnexpectedResponse("NodeConfigSaved")),
        }
    }

    /// 在节点创建一个持久扫描任务；媒体计算由节点唯一 WorkerPool 执行。
    pub async fn create_scan(
        &self,
        roots: Vec<String>,
        force_recalculate: bool,
        enumerator: &str,
    ) -> Result<TaskId, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::CreateScan(proto::CreateScan {
                roots,
                force_recalculate,
                enumerator: enumerator.into(),
            }))
            .await?;
        task_accepted(payload_or_error(response)?)
    }

    /// 请求节点取消持久任务及其等待/运行 Worker 项。
    pub async fn cancel_task(&self, task_id: TaskId) -> Result<(), SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::CancelTask(proto::CancelTask {
                task_id: task_id.as_uuid().to_string(),
            }))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::CancelTask(_) => Ok(()),
            _ => Err(SessionError::UnexpectedResponse("CancelTask")),
        }
    }

    /// 分页列出节点持久任务，供任务页刷新而不读取 SQLite。
    pub async fn list_tasks(
        &self,
        cursor: &str,
        limit: u32,
    ) -> Result<proto::ListTasks, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::ListTasks(proto::ListTasks {
                cursor: cursor.into(),
                limit,
                tasks: Vec::new(),
                next_cursor: String::new(),
            }))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::ListTasks(page) => Ok(page),
            _ => Err(SessionError::UnexpectedResponse("ListTasks")),
        }
    }

    /// 分页浏览节点本机目录；空父路径列出可用盘符。
    pub async fn browse_paths(
        &self,
        parent_path: &str,
        cursor: &str,
        limit: u32,
    ) -> Result<proto::BrowsePaths, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::BrowsePaths(proto::BrowsePaths {
                parent_path: parent_path.into(),
                cursor: cursor.into(),
                limit,
                entries: Vec::new(),
                next_cursor: String::new(),
            }))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::BrowsePaths(page) => Ok(page),
            _ => Err(SessionError::UnexpectedResponse("BrowsePaths")),
        }
    }

    /// 按最多 1 MiB 读取原图或视频联系表的一块数据。
    pub async fn read_file_chunk(
        &self,
        location: &LocationKey,
        file_kind: &str,
        offset: u64,
        max_bytes: u32,
    ) -> Result<proto::FileChunk, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::ReadFile(proto::ReadFile {
                location: Some(location.into()),
                file_kind: file_kind.into(),
                offset,
                max_bytes,
            }))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::FileChunk(chunk) => {
                dedup_protocol::validate_file_chunk(&chunk)
                    .map_err(|error| SessionError::InvalidResponse(error.to_string()))?;
                Ok(chunk)
            }
            _ => Err(SessionError::UnexpectedResponse("FileChunk")),
        }
    }

    /// 执行 PostgreSQL 已冻结并按物理机器分配的删除项。
    pub async fn execute_central_delete_batch(
        &self,
        batch_id: &str,
        items: Vec<proto::DeleteItem>,
        mode: DeleteMode,
    ) -> Result<proto::CreateDeleteBatch, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::CreateDeleteBatch(
                proto::CreateDeleteBatch {
                    delete_batch_id: batch_id.into(),
                    mode: match mode {
                        DeleteMode::RecycleBin => proto::DeleteMode::DeleteRecycleBin as i32,
                        DeleteMode::Permanent => proto::DeleteMode::DeletePermanent as i32,
                    },
                    items,
                    analysis_run_id: String::new(),
                    group_ids: Vec::new(),
                },
            ))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::CreateDeleteBatch(batch) => Ok(batch),
            _ => Err(SessionError::UnexpectedResponse("CreateDeleteBatch")),
        }
    }

    /// 幂等确认 PostgreSQL 已提交游标；节点自行限制到本地真实高水位。
    pub async fn acknowledge(&self, committed_seq: u64) -> Result<(), SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::SyncAck(proto::SyncAck {
                committed_seq,
            }))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::SyncAck(_) => Ok(()),
            _ => Err(SessionError::UnexpectedResponse("SyncAck")),
        }
    }

    /// 从指定中心游标之后拉取最多 `limit` 条有序 outbox 变更。
    pub async fn pull_changes(
        &self,
        after_seq: u64,
        limit: u32,
    ) -> Result<proto::SyncChangeBatch, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::PullChanges(proto::PullChanges {
                after_seq,
                limit,
            }))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::SyncChangeBatch(batch) => Ok(batch),
            _ => Err(SessionError::UnexpectedResponse("SyncChangeBatch")),
        }
    }

    /// 请求节点开启一次固定 SQLite 只读快照并返回 token 与起始高水位。
    pub async fn begin_snapshot(&self) -> Result<proto::BeginSnapshot, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::BeginSnapshot(
                proto::BeginSnapshot::default(),
            ))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::BeginSnapshot(snapshot) => Ok(snapshot),
            _ => Err(SessionError::UnexpectedResponse("BeginSnapshot")),
        }
    }

    /// 从同一个快照 token 的指定表读取一页基础行。
    pub async fn read_snapshot_page(
        &self,
        request: proto::ReadSnapshotPage,
    ) -> Result<proto::ReadSnapshotPage, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::ReadSnapshotPage(request))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::ReadSnapshotPage(page) => Ok(page),
            _ => Err(SessionError::UnexpectedResponse("ReadSnapshotPage")),
        }
    }

    /// 查询一个持久节点任务的当前状态和真实 outbox 高水位。
    pub async fn query_task(&self, task_id: TaskId) -> Result<proto::TaskSummary, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::QueryTask(proto::QueryTask {
                task_id: task_id.as_uuid().to_string(),
                task: None,
            }))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::QueryTask(response) => response
                .task
                .ok_or(SessionError::UnexpectedResponse("QueryTask.task")),
            _ => Err(SessionError::UnexpectedResponse("QueryTask")),
        }
    }

    /// 分页读取所选已完成扫描任务对应的当前活动内容位置及本机缓存完整性。
    pub async fn prepare_analysis_input(
        &self,
        run_id: AnalysisRunId,
        scan_task_ids: &[TaskId],
        cursor: &str,
        limit: u32,
    ) -> Result<proto::PrepareAnalysisInput, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::PrepareAnalysisInput(
                proto::PrepareAnalysisInput {
                    analysis_run_id: run_id.as_uuid().to_string(),
                    cursor: cursor.into(),
                    limit,
                    inputs: Vec::new(),
                    next_cursor: String::new(),
                    scan_task_ids: scan_task_ids
                        .iter()
                        .map(|task_id| task_id.as_uuid().to_string())
                        .collect(),
                },
            ))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::PrepareAnalysisInput(page) => Ok(page),
            _ => Err(SessionError::UnexpectedResponse("PrepareAnalysisInput")),
        }
    }

    /// 把一筛完成后汇总的缺失二筛内容作为一个节点批次派发。
    pub async fn dispatch_stage2(
        &self,
        run_id: AnalysisRunId,
        items: Vec<proto::Stage2WorkItem>,
    ) -> Result<TaskId, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::DispatchStage2(
                proto::DispatchStage2 {
                    analysis_run_id: run_id.as_uuid().to_string(),
                    items,
                },
            ))
            .await?;
        let accepted = match payload_or_error(response)? {
            proto::envelope::Payload::TaskAccepted(accepted) => accepted,
            _ => return Err(SessionError::UnexpectedResponse("TaskAccepted")),
        };
        uuid::Uuid::parse_str(&accepted.task_id)
            .map(TaskId::from_uuid)
            .map_err(|_| SessionError::InvalidTaskId(accepted.task_id))
    }

    /// 分页读取当前 Node 进程内运行任务摘要；节点重启后该列表重新为空。
    pub async fn list_runtime_tasks(
        &self,
        cursor: &str,
        limit: u32,
    ) -> Result<proto::ListRuntimeTasks, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::ListRuntimeTasks(
                proto::ListRuntimeTasks {
                    cursor: cursor.into(),
                    limit,
                    tasks: Vec::new(),
                    next_cursor: String::new(),
                },
            ))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::ListRuntimeTasks(page) => Ok(page),
            _ => Err(SessionError::UnexpectedResponse("ListRuntimeTasks")),
        }
    }

    /// 按进程内运行任务 ID 读取完整阶段、Worker 与最近失败详情。
    pub async fn runtime_task_details(
        &self,
        runtime_task_id: &str,
    ) -> Result<proto::RuntimeTaskDetails, SessionError> {
        let response = self
            .connection
            .request(proto::envelope::Payload::GetRuntimeTaskDetails(
                proto::GetRuntimeTaskDetails {
                    runtime_task_id: runtime_task_id.into(),
                    details: None,
                },
            ))
            .await?;
        match payload_or_error(response)? {
            proto::envelope::Payload::GetRuntimeTaskDetails(response) => response
                .details
                .ok_or_else(|| SessionError::InvalidResponse("运行任务详情为空".into())),
            _ => Err(SessionError::UnexpectedResponse("GetRuntimeTaskDetails")),
        }
    }

    /// 在当前同一 TCP 会话等待一个 `request_id=0` 运行任务终态事件。
    pub async fn next_runtime_event(&self) -> Result<proto::RuntimeTaskChanged, SessionError> {
        let envelope = self.connection.next_event().await?;
        if envelope.request_id != 0 {
            return Err(SessionError::InvalidResponse(
                "运行任务事件 request_id 必须为 0".into(),
            ));
        }
        match payload_or_error(envelope)? {
            proto::envelope::Payload::RuntimeTaskChanged(event) => Ok(event),
            _ => Err(SessionError::UnexpectedResponse("RuntimeTaskChanged")),
        }
    }

    /// 等待节点主动推送的任务事件；断线时返回当前会话错误，由上层重新连接。
    pub async fn next_event(&self) -> Result<proto::Envelope, SessionError> {
        Ok(self.connection.next_event().await?)
    }
}

fn payload_or_error(envelope: proto::Envelope) -> Result<proto::envelope::Payload, SessionError> {
    match envelope.payload {
        Some(proto::envelope::Payload::Error(error)) => Err(SessionError::Protocol {
            code: error.code,
            message: error.message,
        }),
        Some(payload) => Ok(payload),
        None => Err(SessionError::UnexpectedResponse("非空 Envelope.payload")),
    }
}

fn task_accepted(payload: proto::envelope::Payload) -> Result<TaskId, SessionError> {
    let accepted = match payload {
        proto::envelope::Payload::TaskAccepted(accepted) => accepted,
        _ => return Err(SessionError::UnexpectedResponse("TaskAccepted")),
    };
    uuid::Uuid::parse_str(&accepted.task_id)
        .map(TaskId::from_uuid)
        .map_err(|_| SessionError::InvalidTaskId(accepted.task_id))
}

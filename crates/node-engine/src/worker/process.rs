//! 单个 worker.exe 的受限创建、Ready 握手和匿名管道收发。

use std::{
    path::{Path, PathBuf},
    process::Stdio,
    time::Duration,
};

use dedup_protocol::proto::{self, worker_envelope};
use dedup_transport::{FrameClass, FrameError, FrameReader, FrameWriter};
use dedup_windows::{CREATE_WORKER_FLAGS, WorkerJob};
use prost::Message;
use thiserror::Error;
use tokio::process::{Child, ChildStdin, ChildStdout, Command};

/// Worker 子进程的可执行文件启动定义。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerLaunch {
    executable: PathBuf,
}

impl WorkerLaunch {
    /// 创建不带命令行参数的生产 Worker 启动定义。
    pub fn new(executable: impl Into<PathBuf>) -> Self {
        Self {
            executable: executable.into(),
        }
    }

    /// 返回将由节点直接启动的 worker.exe 路径。
    pub fn executable(&self) -> &Path {
        &self.executable
    }
}

/// 一个已经加入 Job 且完成 Ready 握手的子进程及其两条匿名管道。
pub(crate) struct WorkerProcess {
    child: Child,
    reader: FrameReader<ChildStdout>,
    writer: FrameWriter<ChildStdin>,
    process_id: u32,
}

impl WorkerProcess {
    /// 以无窗口、管道重定向方式启动进程，加入 Job 后等待匹配 PID 的 Ready。
    pub(crate) async fn spawn(
        launch: &WorkerLaunch,
        job: &WorkerJob,
        startup_timeout: Duration,
    ) -> Result<Self, WorkerProcessError> {
        let mut command = Command::new(launch.executable());
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .creation_flags(CREATE_WORKER_FLAGS);
        let mut child = command.spawn()?;
        let process_id = child.id().ok_or(WorkerProcessError::MissingProcessId)?;
        let raw_handle = child
            .raw_handle()
            .ok_or(WorkerProcessError::MissingProcessHandle)?;
        job.assign_raw_process_handle(raw_handle)
            .map_err(|error| WorkerProcessError::Job(error.to_string()))?;
        let stdin = child.stdin.take().ok_or(WorkerProcessError::MissingStdin)?;
        let stdout = child
            .stdout
            .take()
            .ok_or(WorkerProcessError::MissingStdout)?;
        let mut process = Self {
            child,
            reader: FrameReader::new(stdout),
            writer: FrameWriter::new(stdin),
            process_id,
        };
        let ready = tokio::time::timeout(startup_timeout, process.receive())
            .await
            .map_err(|_| WorkerProcessError::ReadyTimeout)??;
        match ready.payload {
            Some(worker_envelope::Payload::WorkerReady(ready))
                if ready.process_id == process_id =>
            {
                Ok(process)
            }
            _ => Err(WorkerProcessError::ExpectedReady),
        }
    }

    /// 返回 Ready 已确认的 Windows 进程 ID。
    pub(crate) const fn process_id(&self) -> u32 {
        self.process_id
    }

    /// 向 stdin 写出一个完整 WorkerEnvelope 帧。
    pub(crate) async fn send(
        &mut self,
        envelope: &proto::WorkerEnvelope,
    ) -> Result<(), WorkerProcessError> {
        self.writer
            .write_frame(&envelope.encode_to_vec(), FrameClass::Ordinary)
            .await?;
        Ok(())
    }

    /// 从 stdout 读取并解码一个完整 WorkerEnvelope 帧。
    pub(crate) async fn receive(&mut self) -> Result<proto::WorkerEnvelope, WorkerProcessError> {
        let payload = self.reader.read_frame().await?;
        Ok(proto::WorkerEnvelope::decode(payload.as_slice())?)
    }

    /// 请求强制结束并等待进程句柄进入终态。
    pub(crate) async fn terminate(&mut self) -> Result<(), WorkerProcessError> {
        match self.child.start_kill() {
            Ok(()) => {
                self.child.wait().await?;
                Ok(())
            }
            Err(error) if error.kind() == std::io::ErrorKind::InvalidInput => Ok(()),
            Err(error) => Err(error.into()),
        }
    }
}

impl Drop for WorkerProcess {
    /// actor 异常结束时发出最后一次终止请求；Job Object 仍是最终兜底。
    fn drop(&mut self) {
        let _ = self.child.start_kill();
    }
}

#[derive(Debug, Error)]
/// 单 Worker 的创建、握手、分帧和退出错误。
pub(crate) enum WorkerProcessError {
    #[error("启动或等待 Worker 失败: {0}")]
    Io(#[from] std::io::Error),
    #[error("Worker 管道分帧失败: {0}")]
    Frame(#[from] FrameError),
    #[error("Worker Protobuf 解码失败: {0}")]
    Protobuf(#[from] prost::DecodeError),
    #[error("Worker Ready 超时")]
    ReadyTimeout,
    #[error("Worker 启动后首帧不是匹配进程 ID 的 Ready")]
    ExpectedReady,
    #[error("Worker 进程没有 PID")]
    MissingProcessId,
    #[error("Worker 进程没有原生句柄")]
    MissingProcessHandle,
    #[error("Worker stdin 未创建")]
    MissingStdin,
    #[error("Worker stdout 未创建")]
    MissingStdout,
    #[error("Worker 无法加入 Job Object: {0}")]
    Job(String),
}

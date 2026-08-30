//! 瞬态任务文件的 Media Worker 派发和资源事件边界。
//!
//! 本模块只负责把已知 MD5 的基础任务行交给 WorkerPool，并保留磁盘许可到
//! `BaseSourceReadComplete`。结果和失败继续由后续 taskless 持久化阶段处理。

use std::{
    collections::{BTreeMap, BTreeSet},
    time::Duration,
};

use dedup_core::{ContentKey, DiskReadConfig, MediaKind};
use dedup_protocol::{proto, proto::worker_envelope};
use dedup_windows::ReadCancellationToken;

use super::{TaskFileBaseContext, task_file_base_compute::TaskFileBaseComputePending};
use crate::{
    io::DiskReadClass,
    runtime_tasks::{RuntimeFailureUpdate, RuntimeStage, RuntimeTaskReporter, RuntimeWorkerUpdate},
    scan::base_persistence::BaseStoreHandle,
    task_dispatch::TaskLanePermitProvider,
    task_files::{TaskFileIdentity, TaskFileRecord, TaskWorkKind},
    worker::{WorkerEvent, WorkerFileIdentity, WorkerPool},
};

/// 一个已派发媒体项的资源和事件状态；permit 只在源文件读取结束后释放。
struct ActiveMedia<P: TaskLanePermitProvider> {
    /// 任务文件精确身份，终态结果必须原样带回。
    identity: TaskFileIdentity,
    /// 已知 MD5 的媒体任务行。
    record: TaskFileRecord,
    /// 该行在 Hash 阶段冻结的上下文。
    context: TaskFileBaseContext,
    /// dispatcher 交付的唯一磁盘许可；SourceComplete 后变为 None。
    permit: Option<P::Permit>,
    /// WorkerPool Started 确认的逻辑槽位。
    worker_slot: Option<u32>,
    /// Worker 是否已经关闭源文件。
    source_read_complete: bool,
    /// 派发时冻结的完整 Worker 文件身份。
    worker_identity: WorkerFileIdentity,
}

/// Media Worker 正常完成后交给 taskless 持久化阶段的拥有型结果。
pub(crate) struct TaskFileMediaCompleted {
    /// 原始任务文件身份。
    pub(crate) identity: TaskFileIdentity,
    /// 原始媒体任务行。
    pub(crate) record: TaskFileRecord,
    /// Hash/缓存阶段冻结的上下文。
    pub(crate) context: TaskFileBaseContext,
    /// Worker 返回的协议载荷；由后续阶段校验并写入 SQLite。
    pub(crate) response: proto::WorkerEnvelope,
    /// WorkerPool 实际使用的槽位。
    pub(crate) worker_slot: Option<u32>,
}

/// Media Worker 单文件失败；本阶段只返回失败，不把 TSV 改成 F。
pub(crate) struct TaskFileMediaFailure {
    /// 原始任务文件身份。
    pub(crate) identity: TaskFileIdentity,
    /// 原始媒体任务行。
    pub(crate) record: TaskFileRecord,
    /// 面向日志和持久化的失败说明。
    pub(crate) message: String,
    /// WorkerPool 观测到的槽位。
    pub(crate) worker_slot: Option<u32>,
}

/// 一条 Worker 终态；SQLite 写入与 TSV 状态迁移由持久化运行态继续处理。
pub(super) enum TaskFileMediaTerminal {
    /// Worker 返回了基础计算结果。
    Completed(TaskFileMediaCompleted),
    /// Worker 崩溃或违反事件顺序。
    Failed(TaskFileMediaFailure),
}

/// 核心状态机接受事件后生成的 reporter 权威副作用。
enum MediaReporterEffect {
    /// 首条精确 Started 冻结并发布 Worker 槽位。
    Started(RuntimeWorkerUpdate),
    /// 匹配冻结槽位的合法阶段变化。
    Phase {
        /// Worker 槽位。
        slot: u32,
        /// 当前任务项身份。
        item_id: String,
        /// Worker 显式阶段。
        phase: proto::RuntimeWorkerPhase,
        /// Worker 请求耗时。
        request_elapsed: Option<Duration>,
    },
    /// 首条匹配的源读取完成事件。
    SourceReadComplete {
        /// Worker 槽位。
        slot: u32,
        /// 当前任务项身份。
        item_id: String,
        /// Worker 请求耗时。
        request_elapsed: Option<Duration>,
    },
    /// 核心确认成功的 Worker 终态。
    Completed {
        /// Worker 槽位；未收到 Started 时不伪造槽位。
        slot: Option<u32>,
        /// 当前任务项身份。
        item_id: String,
    },
    /// 核心确认失败的 Worker 终态。
    Failed {
        /// Worker 槽位；未收到 Started 时只记录失败。
        slot: Option<u32>,
        /// 当前任务项身份。
        item_id: String,
        /// 运行任务失败投影。
        failure: RuntimeFailureUpdate,
    },
}

/// 一次核心事件判定；Ignored 事件同时没有 terminal 和 reporter 副作用。
struct MediaEventDisposition {
    /// 可交给持久化运行态的唯一终态。
    terminal: Option<TaskFileMediaTerminal>,
    /// 只由核心验证通过后生成的 reporter 副作用。
    reporter_effect: Option<MediaReporterEffect>,
}

impl MediaEventDisposition {
    /// 构造不改变任何权威状态的忽略结果。
    const fn ignored() -> Self {
        Self {
            terminal: None,
            reporter_effect: None,
        }
    }
}

/// Media Worker 的可逐事件推进运行态；读取 permit 仅在源文件读取结束后释放。
pub(super) struct TaskFileMediaRuntime<P: TaskLanePermitProvider> {
    /// 已提交给 WorkerPool、尚未收到终态事件的媒体项。
    active: BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
}

impl<P: TaskLanePermitProvider> TaskFileMediaRuntime<P> {
    /// 创建没有已派发 Worker 的空运行态。
    pub(super) fn new() -> Self {
        Self {
            active: BTreeMap::new(),
        }
    }

    /// 返回当前 Worker 窗口是否还可以派发新的媒体项。
    pub(super) fn has_capacity(&self, worker_capacity: usize) -> bool {
        self.active.len() < worker_capacity
    }

    /// 返回是否仍有等待 Worker 事件的媒体项。
    pub(super) fn has_active(&self) -> bool {
        !self.active.is_empty()
    }

    /// 把一个已领取的 Media 行转换为 Worker 请求并登记为活动项。
    pub(super) async fn dispatch(
        &mut self,
        pending: &mut TaskFileBaseComputePending<P>,
        task: crate::task_dispatch::DispatchedTask<P::Permit>,
        worker_pool: &mut WorkerPool,
        store: &BaseStoreHandle,
        read_config: &DiskReadConfig,
        cancellation: &ReadCancellationToken,
    ) -> Result<(), String> {
        let identity = task.identity.clone();
        let work =
            dispatch_media_task(pending, task, worker_pool, store, read_config, cancellation)
                .await?;
        if self.active.insert(identity, work).is_some() {
            return Err("Media 运行态收到重复活动任务身份".into());
        }
        Ok(())
    }

    /// 消费恰好一个 Worker 事件；Started 和 SourceReadComplete 不产生终态。
    pub(super) async fn handle_event(
        &mut self,
        event: WorkerEvent,
        reporter: Option<&RuntimeTaskReporter>,
    ) -> Result<Option<TaskFileMediaTerminal>, String> {
        let disposition = handle_media_event(event, &mut self.active)?;
        if let (Some(reporter), Some(effect)) = (reporter, disposition.reporter_effect) {
            report_media_effect(effect, reporter).await;
        }
        Ok(disposition.terminal)
    }

    /// 取消 Worker 并等待池内的退出与替换收束，再释放仍由活动项持有的 permit。
    pub(super) async fn cancel_and_drain(
        &mut self,
        worker_pool: &mut WorkerPool,
    ) -> Result<(), String> {
        let run_ids = self
            .active
            .keys()
            .map(|identity| identity.run_id().to_owned())
            .collect::<BTreeSet<_>>();
        let mut diagnostics = Vec::new();
        for run_id in run_ids {
            if let Err(error) = worker_pool.cancel_task(&run_id).await {
                diagnostics.push(format!("取消 Media Worker {run_id} 失败: {error}"));
            }
        }
        // 丢弃 ActiveMedia 时会释放尚未收到 SourceReadComplete 的读取 permit。
        self.active.clear();
        if diagnostics.is_empty() {
            Ok(())
        } else {
            Err(diagnostics.join("; "))
        }
    }
}

/// 把核心已经接受的权威副作用投影到进程内运行任务。
async fn report_media_effect(effect: MediaReporterEffect, reporter: &RuntimeTaskReporter) {
    match effect {
        MediaReporterEffect::Started(worker) => {
            let _ = reporter.worker_started(worker).await;
        }
        MediaReporterEffect::Phase {
            slot,
            item_id,
            phase,
            request_elapsed,
        } => {
            let _ = reporter.worker_phase_nowait(slot, &item_id, phase, request_elapsed);
        }
        MediaReporterEffect::SourceReadComplete {
            slot,
            item_id,
            request_elapsed,
        } => {
            let _ = reporter.worker_source_read_complete_nowait(slot, &item_id, request_elapsed);
        }
        MediaReporterEffect::Completed { slot, item_id } => {
            if let Some(slot) = slot {
                let _ = reporter.worker_completed(slot).await;
                let _ = reporter.worker_released_nowait(slot, &item_id);
            }
        }
        MediaReporterEffect::Failed {
            slot,
            item_id,
            failure,
        } => {
            let _ = reporter.record_failure_nowait(failure);
            if let Some(slot) = slot {
                let _ = reporter.worker_released_nowait(slot, &item_id);
            }
        }
    }
}

/// 把一项 dispatcher 任务转换为 V5 Media 请求并交给 WorkerPool。
async fn dispatch_media_task<P>(
    pending: &mut TaskFileBaseComputePending<P>,
    task: crate::task_dispatch::DispatchedTask<P::Permit>,
    worker_pool: &WorkerPool,
    store: &BaseStoreHandle,
    read_config: &DiskReadConfig,
    cancellation: &ReadCancellationToken,
) -> Result<ActiveMedia<P>, String>
where
    P: TaskLanePermitProvider,
{
    if task.class != DiskReadClass::MediaDecode
        || task.record.work_kind != TaskWorkKind::Base
        || task.record.known_md5.is_none()
        || task.record.missing.needs_md5()
        || task.record.missing.base_missing_parts() == 0
    {
        return Err("Media dispatcher 返回了无效基础任务行".into());
    }
    let identity = task.identity.clone();
    let Some(mut context) = pending.contexts.get(&identity).cloned() else {
        return Err("Media 任务缺少对应内存上下文".into());
    };
    let md5 = task
        .record
        .known_md5
        .expect("上方已验证 Media 任务携带 known_md5");
    let media_kind = context
        .cached
        .as_ref()
        .map_or(MediaKind::Other, |record| record.media_kind);
    let expected_key = ContentKey::new(md5, task.record.scanned.file_size);
    if context
        .cached
        .as_ref()
        .is_some_and(|cached| cached.content_key != expected_key)
    {
        return Err("Media 缓存上下文 ContentKey 与任务行不一致".into());
    }
    // 每个 Media 行都幂等补写当前位置；context 中的 content_id 只是快照，不能代替
    // 本次路径归属写入，否则 Hash 命中旧内容后换路径会漏掉 files 行。
    let content = match store.upsert_content_and_location(&task.record.scanned, md5, media_kind) {
        Ok(content) => content,
        Err(error) => return Err(format!("Media 内容位置写入失败: {error}")),
    };
    if content.key != expected_key {
        return Err("Media Store 返回的 ContentKey 与任务行不一致".into());
    }
    context.content_id = Some(content.id);
    if let Some(cached) = context.cached.as_mut() {
        cached.content_id = Some(content.id);
    }
    pending.contexts.insert(identity.clone(), context.clone());
    let physical_disk_id = physical_disk_display(&context.lane);
    let worker_identity = WorkerFileIdentity {
        machine_id: store.machine_id().clone(),
        normalized_path: task.record.scanned.normalized_path.clone(),
        display_path: task.record.scanned.display_path.clone(),
        file_size: task.record.scanned.file_size,
        stage: "base_compute".into(),
        physical_disk_id: physical_disk_id.clone(),
    };
    let command = proto::WorkerEnvelope {
        payload: Some(worker_envelope::Payload::ComputeBaseFeatures(
            proto::ComputeBaseFeatures {
                task_id: identity.run_id().to_owned(),
                item_id: identity.item_id().to_string(),
                machine_id: store.machine_id().as_str().to_owned(),
                normalized_path: task.record.scanned.normalized_path.as_str().to_owned(),
                display_path: task
                    .record
                    .scanned
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned(),
                file_size: task.record.scanned.file_size,
                physical_disk_id,
                md5: md5.to_vec(),
                media_kind: proto_media_kind(media_kind) as i32,
                missing_parts: task.record.missing.base_missing_parts(),
                block_size_bytes: read_config.block_size_bytes as u64,
                block_timeout_ms: read_config.block_timeout_seconds.saturating_mul(1_000),
                block_retries: read_config.block_retries,
                decoder_threads: worker_pool.decoder_threads_for(media_kind),
            },
        )),
    };
    if let Err(error) = worker_pool
        .dispatch_scan(command, cancellation.clone(), true, worker_identity.clone())
        .await
    {
        return Err(format!("Media Worker 派发失败: {error}"));
    }
    Ok(ActiveMedia {
        identity,
        record: task.record,
        context,
        permit: Some(task.permit),
        worker_slot: None,
        source_read_complete: false,
        worker_identity,
    })
}

/// 处理一条 Worker 事件；外部任务/身份不匹配时完全不触碰当前资源。
fn handle_media_event(
    event: WorkerEvent,
    active: &mut BTreeMap<TaskFileIdentity, ActiveMedia<impl TaskLanePermitProvider>>,
) -> Result<MediaEventDisposition, String> {
    match event {
        WorkerEvent::Started {
            task_id,
            item_id,
            identity,
            slot,
            process_id,
            cpu_weight,
            decoder_threads,
            ..
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(MediaEventDisposition::ignored());
            };
            let Some(work) = active.get_mut(&key) else {
                return Ok(MediaEventDisposition::ignored());
            };
            if !same_worker_identity(&work.worker_identity, &identity, true) {
                return Ok(MediaEventDisposition::ignored());
            }
            // 首条精确 Started 冻结 Worker 槽位；重复事件不能把后续源读取归属改写到别槽。
            if work.worker_slot.is_some() {
                return Ok(MediaEventDisposition::ignored());
            }
            work.worker_slot = Some(slot);
            return Ok(MediaEventDisposition {
                terminal: None,
                reporter_effect: Some(MediaReporterEffect::Started(RuntimeWorkerUpdate {
                    slot,
                    process_id,
                    item_id,
                    stage: RuntimeStage::ComputeBaseFeatures,
                    display_path: identity
                        .display_path
                        .as_path()
                        .to_string_lossy()
                        .into_owned(),
                    physical_disk_id: identity.physical_disk_id,
                    completed_files: 0,
                    speed_per_second: 0.0,
                    current_step: "等待 Worker 阶段事件".into(),
                    cache_detail: String::new(),
                    phase: None,
                    cpu_weight: Some(cpu_weight),
                    decoder_threads,
                })),
            });
        }
        WorkerEvent::PhaseChanged {
            task_id,
            item_id,
            slot,
            phase,
            request_elapsed_us,
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(MediaEventDisposition::ignored());
            };
            if !active
                .get(&key)
                .is_some_and(|work| work.worker_slot == Some(slot))
            {
                return Ok(MediaEventDisposition::ignored());
            }
            return Ok(MediaEventDisposition {
                terminal: None,
                reporter_effect: Some(MediaReporterEffect::Phase {
                    slot,
                    item_id,
                    phase,
                    request_elapsed: request_elapsed_us.map(Duration::from_micros),
                }),
            });
        }
        WorkerEvent::BaseSourceReadComplete {
            task_id,
            item_id,
            slot,
            request_elapsed_us,
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(MediaEventDisposition::ignored());
            };
            let Some(work) = active.get_mut(&key) else {
                return Ok(MediaEventDisposition::ignored());
            };
            if work.worker_slot != Some(slot) || work.source_read_complete {
                return Ok(MediaEventDisposition::ignored());
            }
            work.source_read_complete = true;
            drop(work.permit.take());
            return Ok(MediaEventDisposition {
                terminal: None,
                reporter_effect: Some(MediaReporterEffect::SourceReadComplete {
                    slot,
                    item_id,
                    request_elapsed: request_elapsed_us.map(Duration::from_micros),
                }),
            });
        }
        WorkerEvent::Completed {
            task_id,
            item_id,
            response,
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(MediaEventDisposition::ignored());
            };
            let Some(work) = active.remove(&key) else {
                return Ok(MediaEventDisposition::ignored());
            };
            let worker_slot = work.worker_slot;
            if !work.source_read_complete {
                let message = "Worker Completed 前未收到 BaseSourceReadComplete".to_owned();
                let display_path = work
                    .record
                    .scanned
                    .display_path
                    .as_path()
                    .to_string_lossy()
                    .into_owned();
                return Ok(MediaEventDisposition {
                    terminal: Some(TaskFileMediaTerminal::Failed(TaskFileMediaFailure {
                        identity: work.identity,
                        record: work.record,
                        message: message.clone(),
                        worker_slot,
                    })),
                    reporter_effect: Some(MediaReporterEffect::Failed {
                        slot: worker_slot,
                        item_id,
                        failure: RuntimeFailureUpdate {
                            stage: RuntimeStage::ComputeBaseFeatures,
                            display_path,
                            message,
                        },
                    }),
                });
            } else {
                return Ok(MediaEventDisposition {
                    terminal: Some(TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
                        identity: work.identity,
                        record: work.record,
                        context: work.context,
                        response,
                        worker_slot,
                    })),
                    reporter_effect: Some(MediaReporterEffect::Completed {
                        slot: worker_slot,
                        item_id,
                    }),
                });
            }
        }
        WorkerEvent::Crashed {
            task_id,
            item_id,
            identity,
            process_id,
            exit_code,
            message,
        } => {
            let Some(key) = find_active_identity(active, &task_id, &item_id) else {
                return Ok(MediaEventDisposition::ignored());
            };
            let Some(work) = active.get(&key) else {
                return Ok(MediaEventDisposition::ignored());
            };
            if !same_worker_identity(&work.worker_identity, &identity, false) {
                return Ok(MediaEventDisposition::ignored());
            }
            let work = active.remove(&key).expect("上方已确认活动项存在");
            let worker_slot = work.worker_slot;
            let reporter_message =
                format!("Worker 崩溃: pid={process_id:?}, exit_code={exit_code:?}: {message}");
            return Ok(MediaEventDisposition {
                terminal: Some(TaskFileMediaTerminal::Failed(TaskFileMediaFailure {
                    identity: work.identity,
                    record: work.record,
                    message,
                    worker_slot,
                })),
                reporter_effect: Some(MediaReporterEffect::Failed {
                    slot: worker_slot,
                    item_id,
                    failure: RuntimeFailureUpdate {
                        stage: RuntimeStage::ComputeBaseFeatures,
                        display_path: identity
                            .display_path
                            .as_path()
                            .to_string_lossy()
                            .into_owned(),
                        message: reporter_message,
                    },
                }),
            });
        }
        WorkerEvent::InfrastructureFailure { message } => {
            return Err(format!("Worker 基础设施失败: {message}"));
        }
        WorkerEvent::Cancelled { task_id, item_id } => {
            if find_active_identity(active, &task_id, &item_id).is_some() {
                return Err("Media Worker 在非取消流程返回 Cancelled".into());
            }
        }
        // 当前基础媒体状态机不消费二筛源读取事件，事件本身已由 Pool 保持非终态所有权。
        WorkerEvent::Stage2SourceReadComplete { .. } => {}
    }
    Ok(MediaEventDisposition::ignored())
}

/// 按 Worker 事件的 run/item 找到当前活动身份。
fn find_active_identity<P: TaskLanePermitProvider>(
    active: &BTreeMap<TaskFileIdentity, ActiveMedia<P>>,
    task_id: &str,
    item_id: &str,
) -> Option<TaskFileIdentity> {
    active
        .keys()
        .find(|identity| identity.run_id() == task_id && identity.item_id().to_string() == item_id)
        .cloned()
}

/// 比较 Worker 文件身份；Started 要求阶段也精确一致，崩溃允许阶段已更新。
fn same_worker_identity(
    expected: &WorkerFileIdentity,
    actual: &WorkerFileIdentity,
    include_stage: bool,
) -> bool {
    expected.machine_id == actual.machine_id
        && expected.normalized_path == actual.normalized_path
        && expected.display_path == actual.display_path
        && expected.file_size == actual.file_size
        && expected.physical_disk_id == actual.physical_disk_id
        && (!include_stage || expected.stage == actual.stage)
}

/// 把冻结的物理盘 lane 映射成 Worker 协议显示值。
fn physical_disk_display(lane: &super::TaskDiskLane) -> String {
    format!(
        "PhysicalDisk{}",
        lane.physical_disk_numbers
            .iter()
            .map(u32::to_string)
            .collect::<Vec<_>>()
            .join("+")
    )
}

/// 领域媒体类型转换为 Worker 协议枚举。
const fn proto_media_kind(media_kind: MediaKind) -> proto::MediaKind {
    match media_kind {
        MediaKind::Image => proto::MediaKind::MediaImage,
        MediaKind::Video => proto::MediaKind::MediaVideo,
        MediaKind::Other => proto::MediaKind::MediaOther,
    }
}

#[cfg(test)]
mod tests {
    use std::sync::{
        Arc,
        atomic::{AtomicIsize, Ordering},
    };

    use dedup_core::{DisplayPath, MachineId, NormalizedPath};
    use dedup_node_store::ScannedPath;
    use dedup_protocol::proto;
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};

    use super::{
        ActiveMedia, TaskFileMediaRuntime, TaskFileMediaTerminal, WorkerEvent, WorkerFileIdentity,
        physical_disk_display,
    };
    use crate::{
        io::{DiskReadClass, ReadFailure},
        runtime_tasks::{RuntimeTaskKind, RuntimeTaskRegistry},
        scan::TaskDiskLane,
        task_dispatch::{TaskLanePermitFuture, TaskLanePermitProvider},
        task_files::{TaskFileIdentity, TaskFileRecord, TaskWorkKind, TaskWorkMask},
    };

    const RUN_ID: &str = "01900000-0000-7000-8000-000000000202";

    /// 能观察 Media 源读取许可释放时机的测试许可。
    struct TrackedPermit {
        /// 当前仍存活的许可计数。
        live: Arc<AtomicIsize>,
    }

    /// reporter 必须只投影核心接受的 Started，并把核心判定的协议失败记录为失败。
    #[tokio::test]
    async fn media_reporter_projects_only_authoritative_started_and_failed_terminal() {
        let live = Arc::new(AtomicIsize::new(1));
        let (mut runtime, identity, worker_identity) = active_runtime(Arc::clone(&live));
        let registry = RuntimeTaskRegistry::new();
        let runtime_task_id = "media-authoritative-failure";
        let reporter = registry
            .begin_with_id(
                runtime_task_id,
                RuntimeTaskKind::BaseCompute,
                MachineId::from_sha256([0x53; 32]),
                "Media 权威事件",
            )
            .await;
        let mut wrong_identity = worker_identity.clone();
        wrong_identity.stage = "wrong-stage".into();

        for (identity_value, slot) in [
            (wrong_identity, 9),
            (worker_identity.clone(), 3),
            (worker_identity, 4),
        ] {
            runtime
                .handle_event(
                    WorkerEvent::Started {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        identity: identity_value,
                        slot,
                        process_id: Some(42),
                        cpu_weight: 1,
                        decoder_threads: Some(1),
                        queue_wait_us: 0,
                    },
                    Some(&reporter),
                )
                .await
                .unwrap();
        }
        runtime
            .handle_event(
                WorkerEvent::BaseSourceReadComplete {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    slot: 4,
                    request_elapsed_us: Some(10),
                },
                Some(&reporter),
            )
            .await
            .unwrap();
        let terminal = runtime
            .handle_event(
                WorkerEvent::Completed {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    response: proto::WorkerEnvelope { payload: None },
                },
                Some(&reporter),
            )
            .await
            .unwrap();
        assert!(matches!(terminal, Some(TaskFileMediaTerminal::Failed(_))));

        let details = registry.details(runtime_task_id).await.unwrap();
        assert_eq!(details.workers.len(), 1, "错误或重复 Started 不得新增 slot");
        assert_eq!(details.workers[0].slot, 3, "首次权威 Started 必须冻结 slot");
        assert_eq!(details.workers[0].completed_files, 0);
        assert_eq!(details.failures.len(), 1, "核心判失败必须投影为失败");
        assert!(
            details.failures[0]
                .message
                .contains("Worker Completed 前未收到 BaseSourceReadComplete")
        );

        assert!(
            runtime
                .handle_event(
                    WorkerEvent::Completed {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        response: proto::WorkerEnvelope { payload: None },
                    },
                    Some(&reporter),
                )
                .await
                .unwrap()
                .is_none()
        );
        let duplicate = registry.details(runtime_task_id).await.unwrap();
        assert_eq!(duplicate.workers.len(), 1);
        assert_eq!(duplicate.workers[0].completed_files, 0);
        assert_eq!(duplicate.failures.len(), 1, "重复 terminal 不得重复统计");
    }

    /// 错误/重复源读取和 terminal 不得污染 reporter，合法 Phase 与完成只投影一次。
    #[tokio::test]
    async fn media_reporter_ignores_invalid_source_and_duplicate_terminal_events() {
        let live = Arc::new(AtomicIsize::new(1));
        let (mut runtime, identity, worker_identity) = active_runtime(Arc::clone(&live));
        let registry = RuntimeTaskRegistry::new();
        let runtime_task_id = "media-authoritative-success";
        let reporter = registry
            .begin_with_id(
                runtime_task_id,
                RuntimeTaskKind::BaseCompute,
                MachineId::from_sha256([0x54; 32]),
                "Media 合法事件",
            )
            .await;
        runtime
            .handle_event(
                WorkerEvent::Started {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    identity: worker_identity.clone(),
                    slot: 3,
                    process_id: Some(43),
                    cpu_weight: 1,
                    decoder_threads: Some(1),
                    queue_wait_us: 0,
                },
                Some(&reporter),
            )
            .await
            .unwrap();
        for slot in [4, 3, 3] {
            runtime
                .handle_event(
                    WorkerEvent::BaseSourceReadComplete {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        slot,
                        request_elapsed_us: Some(10),
                    },
                    Some(&reporter),
                )
                .await
                .unwrap();
        }
        for slot in [4, 3] {
            runtime
                .handle_event(
                    WorkerEvent::PhaseChanged {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        slot,
                        phase: proto::RuntimeWorkerPhase::RuntimeWorkerDecode,
                        request_elapsed_us: Some(20),
                    },
                    Some(&reporter),
                )
                .await
                .unwrap();
        }
        let terminal = runtime
            .handle_event(
                WorkerEvent::Completed {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    response: proto::WorkerEnvelope { payload: None },
                },
                Some(&reporter),
            )
            .await
            .unwrap();
        assert!(matches!(
            terminal,
            Some(TaskFileMediaTerminal::Completed(_))
        ));

        let mut crash_identity = worker_identity;
        crash_identity.stage = "decoding".into();
        assert!(
            runtime
                .handle_event(
                    WorkerEvent::Crashed {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        identity: crash_identity,
                        process_id: Some(43),
                        exit_code: Some(1),
                        message: "重复崩溃".into(),
                    },
                    Some(&reporter),
                )
                .await
                .unwrap()
                .is_none()
        );
        let details = registry.details(runtime_task_id).await.unwrap();
        assert_eq!(details.workers.len(), 1);
        assert_eq!(details.workers[0].slot, 3);
        assert_eq!(details.workers[0].completed_files, 1);
        assert_eq!(
            details.workers[0].phase,
            Some(proto::RuntimeWorkerPhase::RuntimeWorkerIdle as i32)
        );
        assert!(details.failures.is_empty(), "重复 terminal 不得记录失败");
        assert_eq!(live.load(Ordering::SeqCst), 0);
    }

    impl Drop for TrackedPermit {
        fn drop(&mut self) {
            self.live.fetch_sub(1, Ordering::SeqCst);
        }
    }

    /// 只用于给 Media Runtime 指定许可类型的测试 provider。
    #[derive(Clone)]
    struct TestProvider;

    impl TaskLanePermitProvider for TestProvider {
        type Permit = TrackedPermit;

        fn acquire(
            &self,
            _lane: TaskDiskLane,
            _class: DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            Box::pin(async {
                Err(ReadFailure::Io {
                    path: std::path::PathBuf::from(r"C:\unused.bin"),
                    block_offset: 0,
                    source: std::io::Error::other("测试不应请求 provider"),
                })
            })
        }
    }

    /// 构造固定测试磁盘 lane。
    fn lane() -> TaskDiskLane {
        TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([7]).unwrap(),
            physical_disk_numbers: vec![7],
            disk_kind: LocalDiskKind::Hdd,
            configured_weight: 1,
            per_disk_limit: 1,
        }
    }

    /// 构造一条活动 Media 任务及其精确 Worker 身份。
    fn active_runtime(
        live: Arc<AtomicIsize>,
    ) -> (
        TaskFileMediaRuntime<TestProvider>,
        TaskFileIdentity,
        WorkerFileIdentity,
    ) {
        let lane = lane();
        let missing = TaskWorkMask::for_base(false, 1).unwrap();
        let item_id = uuid::Uuid::parse_str("01900000-0000-7000-8000-000000000221").unwrap();
        let identity = TaskFileIdentity::new(RUN_ID, &lane, item_id, 0, 80, missing).unwrap();
        let scanned = ScannedPath::new(
            NormalizedPath::new(r"C:\runtime-media.bin").unwrap(),
            DisplayPath::new(r"C:\runtime-media.bin").unwrap(),
            32,
        );
        let record = TaskFileRecord {
            item_id,
            work_kind: TaskWorkKind::Base,
            scanned: scanned.clone(),
            known_md5: Some([0x51; 16]),
            missing,
        };
        let worker_identity = WorkerFileIdentity {
            machine_id: MachineId::from_sha256([0x52; 32]),
            normalized_path: scanned.normalized_path,
            display_path: scanned.display_path,
            file_size: scanned.file_size,
            stage: "base_compute".into(),
            physical_disk_id: physical_disk_display(&lane),
        };
        let mut runtime = TaskFileMediaRuntime::<TestProvider>::new();
        runtime.active.insert(
            identity.clone(),
            ActiveMedia {
                identity: identity.clone(),
                record,
                context: super::super::TaskFileBaseContext {
                    lane,
                    content_id: None,
                    cached: None,
                    contact_sheet_valid: true,
                    force_recompute: false,
                },
                permit: Some(TrackedPermit { live }),
                worker_slot: None,
                source_read_complete: false,
                worker_identity: worker_identity.clone(),
            },
        );
        (runtime, identity, worker_identity)
    }

    /// Started 只接受第一条精确身份，错误身份或重复 slot 不得改写已冻结 Worker 槽位。
    #[tokio::test]
    async fn media_runtime_started_freezes_first_exact_identity_and_slot() {
        let live = Arc::new(AtomicIsize::new(1));
        let (mut runtime, identity, worker_identity) = active_runtime(Arc::clone(&live));
        let mut wrong_identity = worker_identity.clone();
        wrong_identity.stage = "wrong-stage".into();

        assert!(
            runtime
                .handle_event(
                    WorkerEvent::Started {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        identity: wrong_identity,
                        slot: 9,
                        process_id: None,
                        cpu_weight: 1,
                        decoder_threads: Some(1),
                        queue_wait_us: 0,
                    },
                    None,
                )
                .await
                .unwrap()
                .is_none()
        );
        runtime
            .handle_event(
                WorkerEvent::BaseSourceReadComplete {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    slot: 9,
                    request_elapsed_us: None,
                },
                None,
            )
            .await
            .unwrap();
        assert_eq!(
            live.load(Ordering::SeqCst),
            1,
            "错误 identity 不得冻结 slot"
        );

        for slot in [3, 4] {
            runtime
                .handle_event(
                    WorkerEvent::Started {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        identity: worker_identity.clone(),
                        slot,
                        process_id: None,
                        cpu_weight: 1,
                        decoder_threads: Some(1),
                        queue_wait_us: 0,
                    },
                    None,
                )
                .await
                .unwrap();
        }
        runtime
            .handle_event(
                WorkerEvent::BaseSourceReadComplete {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    slot: 3,
                    request_elapsed_us: None,
                },
                None,
            )
            .await
            .unwrap();
        assert_eq!(
            live.load(Ordering::SeqCst),
            0,
            "重复 Started 不得用错误 slot 覆盖首次精确 Started"
        );
    }

    /// 错误或重复 SourceReadComplete 不得提前或重复释放当前 Media 许可。
    #[tokio::test]
    async fn media_runtime_source_read_complete_requires_active_identity_and_frozen_slot() {
        let live = Arc::new(AtomicIsize::new(1));
        let (mut runtime, identity, worker_identity) = active_runtime(Arc::clone(&live));
        runtime
            .handle_event(
                WorkerEvent::Started {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    identity: worker_identity,
                    slot: 3,
                    process_id: None,
                    cpu_weight: 1,
                    decoder_threads: Some(1),
                    queue_wait_us: 0,
                },
                None,
            )
            .await
            .unwrap();

        for (item_id, slot) in [
            ("unknown-item".to_owned(), 3),
            (identity.item_id().to_string(), 4),
        ] {
            assert!(
                runtime
                    .handle_event(
                        WorkerEvent::BaseSourceReadComplete {
                            task_id: identity.run_id().into(),
                            item_id,
                            slot,
                            request_elapsed_us: None,
                        },
                        None,
                    )
                    .await
                    .unwrap()
                    .is_none()
            );
            assert_eq!(live.load(Ordering::SeqCst), 1);
        }
        for _ in 0..2 {
            assert!(
                runtime
                    .handle_event(
                        WorkerEvent::BaseSourceReadComplete {
                            task_id: identity.run_id().into(),
                            item_id: identity.item_id().to_string(),
                            slot: 3,
                            request_elapsed_us: None,
                        },
                        None,
                    )
                    .await
                    .unwrap()
                    .is_none()
            );
            assert_eq!(live.load(Ordering::SeqCst), 0);
        }
        assert!(runtime.has_active(), "SourceReadComplete 仍是非终态事件");
    }

    /// settled 或未知身份的 terminal 不得再次形成可交给持久化运行态的终态动作。
    #[tokio::test]
    async fn media_runtime_unknown_and_duplicate_terminal_emit_only_once() {
        let live = Arc::new(AtomicIsize::new(1));
        let (mut runtime, identity, mut worker_identity) = active_runtime(Arc::clone(&live));
        worker_identity.stage = "decoding".into();
        let crash = || WorkerEvent::Crashed {
            task_id: identity.run_id().into(),
            item_id: identity.item_id().to_string(),
            identity: worker_identity.clone(),
            process_id: Some(42),
            exit_code: Some(1),
            message: "测试 Worker 崩溃".into(),
        };

        let first = runtime.handle_event(crash(), None).await.unwrap();
        assert!(matches!(first, Some(TaskFileMediaTerminal::Failed(_))));
        assert!(!runtime.has_active());
        assert_eq!(live.load(Ordering::SeqCst), 0);
        assert!(
            runtime.handle_event(crash(), None).await.unwrap().is_none(),
            "重复 terminal 不得形成第二个持久化动作"
        );
        assert!(
            runtime
                .handle_event(
                    WorkerEvent::Completed {
                        task_id: "unknown-run".into(),
                        item_id: "unknown-item".into(),
                        response: proto::WorkerEnvelope { payload: None },
                    },
                    None,
                )
                .await
                .unwrap()
                .is_none(),
            "未知 terminal 不得形成持久化动作"
        );
    }

    #[tokio::test]
    async fn media_runtime_releases_permit_only_after_matching_source_read_complete() {
        let live = Arc::new(AtomicIsize::new(1));
        let (mut runtime, identity, worker_identity) = active_runtime(Arc::clone(&live));

        assert!(
            runtime
                .handle_event(
                    WorkerEvent::Started {
                        task_id: identity.run_id().into(),
                        item_id: identity.item_id().to_string(),
                        identity: worker_identity,
                        slot: 3,
                        process_id: None,
                        cpu_weight: 1,
                        decoder_threads: Some(1),
                        queue_wait_us: 0,
                    },
                    None,
                )
                .await
                .unwrap()
                .is_none()
        );
        assert_eq!(live.load(Ordering::SeqCst), 1);

        runtime
            .handle_event(
                WorkerEvent::BaseSourceReadComplete {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    slot: 4,
                    request_elapsed_us: None,
                },
                None,
            )
            .await
            .unwrap();
        assert_eq!(live.load(Ordering::SeqCst), 1, "错误 slot 不得释放 permit");

        runtime
            .handle_event(
                WorkerEvent::BaseSourceReadComplete {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    slot: 3,
                    request_elapsed_us: None,
                },
                None,
            )
            .await
            .unwrap();
        assert_eq!(live.load(Ordering::SeqCst), 0);

        let terminal = runtime
            .handle_event(
                WorkerEvent::Completed {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    response: proto::WorkerEnvelope { payload: None },
                },
                None,
            )
            .await
            .unwrap();
        assert!(matches!(
            terminal,
            Some(TaskFileMediaTerminal::Completed(_))
        ));
        assert!(!runtime.has_active());
    }

    #[tokio::test]
    async fn media_runtime_worker_crash_returns_file_terminal_and_releases_permit() {
        let live = Arc::new(AtomicIsize::new(1));
        let (mut runtime, identity, mut worker_identity) = active_runtime(Arc::clone(&live));
        worker_identity.stage = "decoding".into();

        let terminal = runtime
            .handle_event(
                WorkerEvent::Crashed {
                    task_id: identity.run_id().into(),
                    item_id: identity.item_id().to_string(),
                    identity: worker_identity,
                    process_id: Some(42),
                    exit_code: Some(1),
                    message: "测试 Worker 崩溃".into(),
                },
                None,
            )
            .await
            .unwrap();
        let Some(TaskFileMediaTerminal::Failed(failure)) = terminal else {
            panic!("Worker 崩溃必须形成当前文件失败终态");
        };
        assert_eq!(failure.identity, identity);
        assert_eq!(failure.message, "测试 Worker 崩溃");
        assert_eq!(live.load(Ordering::SeqCst), 0);
        assert!(!runtime.has_active());
    }
}

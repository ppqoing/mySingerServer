//! 瞬态任务文件 Media 结果的 taskless SQLite 持久化边界。
//!
//! 本模块只消费 Media 阶段已经拥有的结果。Worker 事件和调度由前一阶段负责，
//! 本模块在 SQLite ACK 到达前保持 TSV 行和内存上下文为 `P`，ACK 后才迁移为 `C/F`。

use std::{
    collections::{BTreeMap, VecDeque},
    time::{SystemTime, UNIX_EPOCH},
};

use super::{
    base_persistence::{
        BasePersistAck, BasePersistIdentity, BasePersistMessage, BasePersistOutcome,
        BasePersistSendError, BaseStoreHandle,
    },
    task_file_base_compute::TaskFileBaseComputePending,
    task_file_media_compute::{
        TaskFileMediaCompleted, TaskFileMediaFailure, TaskFileMediaTerminal,
    },
};
use crate::{
    artifact_registry::RegenerableArtifactRegistry,
    contact_sheet_cache::ContactSheetCacheEntry,
    disk_full_cleanup::DiskFullCleaner,
    io::ReadFailure,
    task_dispatch::TaskLanePermitProvider,
    task_files::TaskFileIdentity,
    worker::{BaseComputeOutput, Stage1Frame, Stage1Output, decode_base_compute_payload},
};
use dedup_core::{ContentKey, MediaKind};
use dedup_media::decode_contact_sheet;
use dedup_node_store::{FileFaultKind, FileFaultRecord, ResolvedScanFile, ScannedPath};
use dedup_protocol::{
    BASE_MISSING_CONTACT_SHEET, BASE_MISSING_PROBE, BASE_MISSING_STAGE1, proto::worker_envelope,
};

/// Media taskless 持久化调用所需的本机 artifact 和联系表配置。
#[derive(Clone)]
pub(crate) struct TaskFileMediaPersistenceOptions {
    /// 视频联系表缓存根目录。
    pub(crate) contact_sheet_root: std::path::PathBuf,
    /// 可选的进程级 artifact 注册表。
    pub(crate) artifact_registry: Option<std::sync::Arc<RegenerableArtifactRegistry>>,
    /// 可选的磁盘满清理器。
    pub(crate) disk_full_cleaner: Option<DiskFullCleaner>,
}

impl Default for TaskFileMediaPersistenceOptions {
    fn default() -> Self {
        Self {
            contact_sheet_root: std::env::temp_dir(),
            artifact_registry: None,
            disk_full_cleaner: None,
        }
    }
}

/// ACK 后应用的任务文件迁移动作；动作本身不持有 Worker 或数据库连接。
enum MediaPersistAction {
    /// 一筛写入成功后把 TSV 行置 C，并登记本轮 resolved 文件。
    Complete {
        /// 原始扫描值，用于稳定生成 resolved 清单。
        scanned: dedup_node_store::ScannedPath,
        /// 本次 Media 结果应绑定的内容键。
        content_key: ContentKey,
        /// Worker 结果确认的媒体类型。
        media_kind: MediaKind,
        /// Worker 槽位，必须原样回显到 ACK。
        worker_slot: Option<u32>,
    },
    /// 单文件失败 ACK 后把 TSV 行置 F。
    Failed {
        /// 原始显示路径，用于校验失败 ACK 没有串项。
        display_path: String,
        /// Worker 槽位，必须原样回显到 ACK。
        worker_slot: Option<u32>,
    },
}

/// 仍由队列或 actor 拥有的一条 Media taskless 持久化消息。
struct PendingMediaPersist {
    /// 完整任务文件身份。
    identity: TaskFileIdentity,
    /// 尚未投递或被有界队列归还的消息。
    message: BasePersistMessage,
    /// ACK 后执行的唯一状态迁移。
    action: MediaPersistAction,
}

/// 一个尚未写入 SQLite 或尚未收到 ACK 的通用任务文件持久化消息。
struct PendingTaskFilePersist {
    /// 消息对应的完整任务文件身份。
    identity: TaskFileIdentity,
    /// 仍由队列持有的 SQLite actor 消息。
    message: BasePersistMessage,
    /// 收到匹配 ACK 后才应用的 TSV 状态迁移。
    action: TaskFilePersistAction,
}

/// 单条 SQLite ACK 成功后允许执行的任务文件状态迁移。
enum TaskFilePersistAction {
    /// Hash 命中完整缓存并已提交当前位置。
    HashComplete {
        /// 当前扫描路径，确认后加入 resolved 清单。
        scanned: ScannedPath,
        /// 当前路径绑定的内容键。
        content_key: ContentKey,
    },
    /// Hash 读取失败并已写入故障诊断。
    HashFailed,
    /// Worker Media 结果已提交基础字段。
    MediaComplete {
        /// 当前扫描路径，确认后加入 resolved 清单。
        scanned: ScannedPath,
        /// 当前路径绑定的内容键。
        content_key: ContentKey,
        /// Worker 结果确认的媒体类型。
        media_kind: MediaKind,
        /// Worker 槽位必须由 ACK 原样回显。
        worker_slot: Option<u32>,
    },
    /// Worker Media 失败已写入故障诊断。
    MediaFailed {
        /// 显示路径用于拒绝串项 ACK。
        display_path: String,
        /// Worker 槽位必须由 ACK 原样回显。
        worker_slot: Option<u32>,
    },
}

/// 任务文件 SQLite 持久化的可逐 ACK 推进运行态。
pub(super) struct TaskFilePersistRuntime {
    /// 尚未投递到有界 SQLite actor 队列的消息。
    queue: VecDeque<PendingTaskFilePersist>,
    /// 已投递、必须等待精确 ACK 后才迁移 TSV 的动作。
    in_flight: BTreeMap<TaskFileIdentity, TaskFilePersistAction>,
}

impl TaskFilePersistRuntime {
    /// 创建没有待投递或在途 SQLite 操作的运行态。
    pub(super) fn new() -> Self {
        Self {
            queue: VecDeque::new(),
            in_flight: BTreeMap::new(),
        }
    }

    /// 排入一个完整缓存命中的 Hash 成功动作；ACK 前任务行仍是 P。
    pub(super) fn enqueue_hash_complete(
        &mut self,
        identity: TaskFileIdentity,
        scanned: ScannedPath,
        md5: [u8; 16],
        media_kind: MediaKind,
        content_key: ContentKey,
    ) -> Result<(), String> {
        self.ensure_unique_identity(&identity)?;
        let operation_scanned = scanned.clone();
        let message = BasePersistMessage::new_task_file(identity.clone(), move |store| {
            store
                .upsert_content_and_location(&operation_scanned, md5, media_kind)
                .map(|_| BasePersistOutcome::Succeeded {
                    worker_slot: None,
                    cache_hit: true,
                    media_kind,
                    file_size: operation_scanned.file_size,
                })
                .map_err(|error| error.to_string())
        });
        self.queue.push_back(PendingTaskFilePersist {
            identity,
            message,
            action: TaskFilePersistAction::HashComplete {
                scanned,
                content_key,
            },
        });
        Ok(())
    }

    /// 排入一个 Hash 读取失败动作；故障写入与失败 ACK 使用同一 SQLite 操作。
    pub(super) fn enqueue_hash_failure(
        &mut self,
        identity: TaskFileIdentity,
        scanned: ScannedPath,
        error: ReadFailure,
    ) -> Result<(), String> {
        self.ensure_unique_identity(&identity)?;
        let message_text = error.to_string();
        let fault_normalized_path = scanned.normalized_path.clone();
        let fault_display_path = scanned.display_path.clone();
        let fault_file_size = scanned.file_size;
        let (windows_error_code, read_offset, read_size) = match error {
            ReadFailure::SuspectedPhysical {
                block_offset,
                block_len,
                raw_os_error,
                ..
            } => (raw_os_error, Some(block_offset), Some(block_len as u64)),
            ReadFailure::Io {
                block_offset,
                source,
                ..
            } => (source.raw_os_error(), Some(block_offset), None),
            ReadFailure::Cancelled => (None, None, None),
        };
        let fault_message = message_text.clone();
        let fault_seen_at_ms = now_ms();
        let display_path = scanned
            .display_path
            .as_path()
            .to_string_lossy()
            .into_owned();
        let operation_display_path = display_path.clone();
        let operation_message = message_text;
        let message = BasePersistMessage::new_task_file(identity.clone(), move |store| {
            store
                .upsert_file_fault(&FileFaultRecord {
                    machine_id: store.machine_id().clone(),
                    normalized_path: fault_normalized_path,
                    display_path: fault_display_path,
                    file_size: fault_file_size,
                    kind: FileFaultKind::SuspectedPhysicalRead,
                    stage: "base".to_owned(),
                    windows_error_code,
                    read_offset,
                    read_size,
                    worker_pid: None,
                    worker_exit_code: None,
                    first_seen_at_ms: fault_seen_at_ms,
                    last_seen_at_ms: fault_seen_at_ms,
                    occurrence_count: 1,
                    message: fault_message,
                })
                .map_err(|error| error.to_string())?;
            Ok(BasePersistOutcome::Failed {
                display_path: operation_display_path,
                message: operation_message,
                worker_slot: None,
                skipped_incomplete: false,
            })
        });
        self.queue.push_back(PendingTaskFilePersist {
            identity,
            message,
            action: TaskFilePersistAction::HashFailed,
        });
        Ok(())
    }

    /// 把一条 Media Worker 终态转换为单条 SQLite 操作，尚不迁移 TSV。
    pub(super) fn enqueue_media_terminal(
        &mut self,
        terminal: TaskFileMediaTerminal,
        options: &TaskFileMediaPersistenceOptions,
    ) -> Result<(), String> {
        let PendingMediaPersist {
            identity,
            message,
            action,
        } = match terminal {
            TaskFileMediaTerminal::Completed(item) => build_completed_message(item, options),
            TaskFileMediaTerminal::Failed(item) => build_failure_message(item),
        };
        let action = match action {
            MediaPersistAction::Complete {
                scanned,
                content_key,
                media_kind,
                worker_slot,
            } => TaskFilePersistAction::MediaComplete {
                scanned,
                content_key,
                media_kind,
                worker_slot,
            },
            MediaPersistAction::Failed {
                display_path,
                worker_slot,
            } => TaskFilePersistAction::MediaFailed {
                display_path,
                worker_slot,
            },
        };
        self.ensure_unique_identity(&identity)?;
        self.queue.push_back(PendingTaskFilePersist {
            identity,
            message,
            action,
        });
        Ok(())
    }

    /// 拒绝队列或在途集合中已有的身份，避免消息在发送前覆盖唯一 ACK 所有权。
    fn ensure_unique_identity(&self, identity: &TaskFileIdentity) -> Result<(), String> {
        if self
            .queue
            .iter()
            .any(|pending| &pending.identity == identity)
            || self.in_flight.contains_key(identity)
        {
            return Err("持久化运行态收到重复任务文件身份".into());
        }
        Ok(())
    }

    /// 尽可能投递消息；有界队列满时保留消息，等待外部事件泵先交付 ACK。
    #[cfg(test)]
    pub(super) fn try_submit(&mut self, store: &BaseStoreHandle) -> Result<(), String> {
        while !self.queue.is_empty() {
            if !self.try_submit_one(store)? {
                break;
            }
        }
        Ok(())
    }

    /// 只尝试投递一条消息；返回 false 表示 SQLite actor 队列暂满。
    pub(super) fn try_submit_one(&mut self, store: &BaseStoreHandle) -> Result<bool, String> {
        let Some(mut pending) = self.queue.pop_front() else {
            return Ok(true);
        };
        if self.in_flight.contains_key(&pending.identity) {
            return Err("持久化运行态存在重复在途身份".into());
        }
        match store.try_persist(pending.message) {
            Ok(()) => {
                self.in_flight.insert(pending.identity, pending.action);
                Ok(true)
            }
            Err(BasePersistSendError::Full(message)) => {
                pending.message = message;
                self.queue.push_front(pending);
                Ok(false)
            }
            Err(BasePersistSendError::Closed(_)) => Err("基础持久化 actor 已关闭".into()),
        }
    }

    /// 返回是否还有已经投递但等待 SQLite ACK 的动作。
    pub(super) fn has_in_flight(&self) -> bool {
        !self.in_flight.is_empty()
    }

    /// 返回待投递和在途操作是否都已经清空。
    pub(super) fn is_empty(&self) -> bool {
        self.queue.is_empty() && self.in_flight.is_empty()
    }

    /// 应用一条精确 SQLite ACK；只有成功匹配后才把对应 TSV 行改为 C/F。
    pub(super) fn apply_ack<P: TaskLanePermitProvider>(
        &mut self,
        pending: &mut TaskFileBaseComputePending<P>,
        ack: BasePersistAck,
    ) -> Result<(), String> {
        let identity = match ack.identity {
            BasePersistIdentity::TaskFile(identity) => identity,
            BasePersistIdentity::Legacy(_) => return Err("任务文件收到旧任务表持久化 ACK".into()),
        };
        let outcome = ack.result?;
        let action = self
            .in_flight
            .get(&identity)
            .ok_or_else(|| "持久化运行态收到未知 ACK".to_owned())?;
        match (action, outcome) {
            (
                TaskFilePersistAction::HashComplete {
                    scanned,
                    content_key,
                },
                BasePersistOutcome::Succeeded { .. },
            ) => {
                pending
                    .dispatcher
                    .mark_completed(&identity)
                    .map_err(|error| error.to_string())?;
                pending.contexts.remove(&identity);
                pending.manifest.resolved_files.push(ResolvedScanFile {
                    scanned: scanned.clone(),
                    content: *content_key,
                });
                pending.manifest.resolved_files.sort_by(|left, right| {
                    left.scanned
                        .normalized_path
                        .cmp(&right.scanned.normalized_path)
                });
                pending.manifest.cache_hits += 1;
                self.in_flight.remove(&identity);
            }
            (TaskFilePersistAction::HashFailed, BasePersistOutcome::Failed { .. }) => {
                pending
                    .dispatcher
                    .mark_failed(&identity)
                    .map_err(|error| error.to_string())?;
                pending.contexts.remove(&identity);
                self.in_flight.remove(&identity);
            }
            (
                TaskFilePersistAction::MediaComplete {
                    scanned,
                    content_key,
                    media_kind,
                    worker_slot,
                },
                BasePersistOutcome::Succeeded {
                    worker_slot: ack_slot,
                    cache_hit,
                    media_kind: ack_kind,
                    file_size,
                },
            ) if ack_slot == *worker_slot
                && !cache_hit
                && ack_kind == *media_kind
                && file_size == scanned.file_size =>
            {
                pending
                    .dispatcher
                    .mark_completed(&identity)
                    .map_err(|error| error.to_string())?;
                pending.contexts.remove(&identity);
                pending.manifest.resolved_files.push(ResolvedScanFile {
                    scanned: scanned.clone(),
                    content: *content_key,
                });
                pending.manifest.resolved_files.sort_by(|left, right| {
                    left.scanned
                        .normalized_path
                        .cmp(&right.scanned.normalized_path)
                });
                self.in_flight.remove(&identity);
            }
            (
                TaskFilePersistAction::MediaFailed {
                    display_path,
                    worker_slot,
                },
                BasePersistOutcome::Failed {
                    display_path: ack_path,
                    worker_slot: ack_slot,
                    skipped_incomplete: false,
                    ..
                },
            ) if ack_path == *display_path && ack_slot == *worker_slot => {
                pending
                    .dispatcher
                    .mark_failed(&identity)
                    .map_err(|error| error.to_string())?;
                pending.contexts.remove(&identity);
                self.in_flight.remove(&identity);
            }
            (_, BasePersistOutcome::Ignored) => return Err("任务文件持久化 ACK 被忽略".into()),
            (_, BasePersistOutcome::Cancelled { .. }) => {
                return Err("任务文件持久化 ACK 被取消".into());
            }
            _ => return Err("任务文件持久化 ACK 类型或字段不匹配".into()),
        }
        Ok(())
    }

    /// 取消流程放弃尚未确认的本地动作，保持这些 TSV 行为 P。
    pub(super) fn drop_unacknowledged(&mut self) {
        self.queue.clear();
        self.in_flight.clear();
    }
}

/// Media 结果校验后得到的可持久化动作。
enum PreparedMediaResult {
    /// 校验通过，稍后在 actor 中加载缓存并提交 taskless stage1。
    Complete {
        /// Worker 解码结果。
        output: BaseComputeOutput,
        /// 当前任务行缺失掩码。
        missing_parts: u32,
        /// 本地内容 ID。
        content_id: dedup_node_store::ContentId,
        /// Worker 槽位。
        worker_slot: Option<u32>,
    },
    // 校验失败由调用方转为当前文件的失败持久化。
}

/// 返回用于故障表的单调毫秒时间戳。
fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis()
        .try_into()
        .unwrap_or(u64::MAX)
}

/// 构造失败动作；故障表写入和失败 ACK 必须处于同一个 actor 操作边界。
fn build_failure_message(item: TaskFileMediaFailure) -> PendingMediaPersist {
    let fault_normalized_path = item.record.scanned.normalized_path.clone();
    let fault_display_path = item.record.scanned.display_path.clone();
    let fault_file_size = item.record.scanned.file_size;
    let fault_message = item.message.clone();
    let fault_first_seen_at_ms = now_ms();
    let display_path = item
        .record
        .scanned
        .display_path
        .as_path()
        .to_string_lossy()
        .into_owned();
    let operation_display_path = display_path.clone();
    let operation_message = item.message;
    let worker_slot = item.worker_slot;
    let identity = item.identity;
    let operation = BasePersistMessage::new_task_file(identity.clone(), move |store| {
        store
            .upsert_file_fault(&FileFaultRecord {
                machine_id: store.machine_id().clone(),
                normalized_path: fault_normalized_path,
                display_path: fault_display_path,
                file_size: fault_file_size,
                kind: FileFaultKind::WorkerCrash,
                stage: "base".to_owned(),
                windows_error_code: None,
                read_offset: None,
                read_size: None,
                worker_pid: None,
                worker_exit_code: None,
                first_seen_at_ms: fault_first_seen_at_ms,
                last_seen_at_ms: fault_first_seen_at_ms,
                occurrence_count: 1,
                message: fault_message,
            })
            .map_err(|error| error.to_string())?;
        Ok(BasePersistOutcome::Failed {
            display_path: operation_display_path,
            message: operation_message,
            worker_slot,
            skipped_incomplete: false,
        })
    });
    PendingMediaPersist {
        identity,
        message: operation,
        action: MediaPersistAction::Failed {
            display_path,
            worker_slot,
        },
    }
}

/// 校验 Worker Completed，并把协议错误降级为当前文件失败。
fn build_completed_message(
    item: TaskFileMediaCompleted,
    options: &TaskFileMediaPersistenceOptions,
) -> PendingMediaPersist {
    let identity = item.identity.clone();
    let worker_slot = item.worker_slot;
    let expected_key = item
        .record
        .known_md5
        .map(|md5| ContentKey::new(md5, item.record.scanned.file_size));
    let prepared = match expected_key {
        Some(expected_key) => match validate_completed_response(&item, expected_key) {
            Ok(output) => match item.context.content_id {
                Some(content_id) => Ok(PreparedMediaResult::Complete {
                    output,
                    missing_parts: item.record.missing.base_missing_parts(),
                    content_id,
                    worker_slot: item.worker_slot,
                }),
                None => Err("Media Completed 缺少已经关联的 SQLite content_id".to_owned()),
            },
            Err(message) => Err(message),
        },
        None => Err("Media Completed 缺少已知 MD5".to_owned()),
    };
    match prepared {
        Ok(PreparedMediaResult::Complete {
            output,
            missing_parts,
            content_id,
            worker_slot,
        }) => {
            let content_key = expected_key.expect("上方已检查 known_md5");
            let contact_sheet_root = options.contact_sheet_root.clone();
            let artifact_registry = options.artifact_registry.clone();
            let disk_full_cleaner = options.disk_full_cleaner.clone();
            let item_id = identity.item_id().to_string();
            let media_kind = output.probe.as_ref().map_or_else(
                || {
                    item.context
                        .cached
                        .as_ref()
                        .map_or(MediaKind::Other, |cached| cached.media_kind)
                },
                |probe| probe.media_kind,
            );
            let operation = BasePersistMessage::new_task_file(identity.clone(), move |store| {
                persist_completed_stage1(
                    store,
                    content_id,
                    content_key,
                    &item_id,
                    missing_parts,
                    output,
                    &contact_sheet_root,
                    artifact_registry.as_ref(),
                    disk_full_cleaner.as_ref(),
                    worker_slot,
                )
            });
            PendingMediaPersist {
                identity,
                message: operation,
                action: MediaPersistAction::Complete {
                    scanned: item.record.scanned,
                    content_key,
                    media_kind,
                    worker_slot,
                },
            }
        }
        Err(message) => build_failure_message(TaskFileMediaFailure {
            identity,
            record: item.record,
            message,
            worker_slot,
        }),
    }
}

/// 校验 Worker 终态身份、MD5、payload 和缺失部分。
fn validate_completed_response(
    item: &TaskFileMediaCompleted,
    expected_key: ContentKey,
) -> Result<BaseComputeOutput, String> {
    let Some(worker_envelope::Payload::BaseComputeResult(result)) = item.response.payload.as_ref()
    else {
        if matches!(
            item.response.payload.as_ref(),
            Some(worker_envelope::Payload::WorkerFailure(_))
        ) {
            return Err("Worker 返回了失败载荷而非 Completed".into());
        }
        return Err("Worker 返回了非基础计算响应".into());
    };
    if result.task_id != item.identity.run_id() {
        return Err("Worker 基础结果 task_id 不匹配".into());
    }
    if result.item_id != item.identity.item_id().to_string() {
        return Err("Worker 基础结果 item_id 不匹配".into());
    }
    let returned_md5: [u8; 16] = result
        .md5
        .as_slice()
        .try_into()
        .map_err(|_| "Worker 基础结果 MD5 长度不是 16 字节".to_owned())?;
    if returned_md5 != expected_key.md5() {
        return Err("Worker 基础结果 MD5 与任务内容身份不一致".into());
    }
    let missing_parts = item.record.missing.base_missing_parts();
    if missing_parts != 0 && result.payload.is_empty() {
        return Err("Worker 基础结果在仍有缺失字段时返回空 payload".into());
    }
    let output = if result.payload.is_empty() {
        BaseComputeOutput {
            probe: None,
            stage1_frames: None,
            contact_sheet_jpeg: None,
        }
    } else {
        decode_base_compute_payload(&result.payload)
            .map_err(|error| format!("基础计算结果解析失败: {error}"))?
    };
    if missing_parts & BASE_MISSING_PROBE != 0 && output.probe.is_none() {
        return Err("Worker 基础结果缺少 probe".into());
    }
    validate_completed_output(item, &output, missing_parts)?;
    Ok(output)
}

/// 校验 Worker 输出的媒体类型、请求字段、尺寸、槽位和联系表。
fn validate_completed_output(
    item: &TaskFileMediaCompleted,
    output: &BaseComputeOutput,
    missing_parts: u32,
) -> Result<(), String> {
    if missing_parts == 0 {
        if output.probe.is_some()
            || output.stage1_frames.is_some()
            || output.contact_sheet_jpeg.is_some()
        {
            return Err("Worker 返回了未请求的基础字段".into());
        }
        return Ok(());
    }

    let probe = output
        .probe
        .as_ref()
        .ok_or_else(|| "Worker 基础结果缺少媒体 probe".to_owned())?;
    if let Some(cached) = item.context.cached.as_ref()
        && matches!(cached.media_kind, MediaKind::Image | MediaKind::Video)
        && cached.media_kind != probe.media_kind
    {
        return Err("Worker 媒体类型与已有缓存不一致".into());
    }

    let stage1_requested = missing_parts & BASE_MISSING_STAGE1 != 0;
    // Other 的 Worker 协议在请求 stage1 时返回显式空数组；实际媒体类型只
    // 决定数组内容结构，不能因为数组为空就把已请求字段当成缺失。
    let stage1_expected = stage1_requested;
    if output.stage1_frames.is_some() != stage1_expected {
        return Err(if stage1_expected {
            "Worker 基础结果缺少已请求的 stage1 字段"
        } else {
            "Worker 返回了未请求的 stage1 字段"
        }
        .into());
    }
    let contact_requested = missing_parts & BASE_MISSING_CONTACT_SHEET != 0;
    // 联系表只属于 Video。Image/Other 在通用初始掩码带有 CONTACT 位时仍应
    // 接受没有联系表的合法结果，同时继续拒绝它们额外返回联系表。
    let contact_expected = matches!(probe.media_kind, MediaKind::Video) && contact_requested;
    if output.contact_sheet_jpeg.is_some() != contact_expected {
        return Err(if contact_expected {
            "Worker 基础结果缺少已请求的联系表"
        } else {
            "Worker 返回了未请求的联系表"
        }
        .into());
    }

    match probe.media_kind {
        MediaKind::Image => {
            if probe.width == 0 || probe.height == 0 || probe.duration_ms.is_some() {
                return Err("图片 probe 的尺寸或时长无效".into());
            }
            if let Some(frames) = output.stage1_frames.as_ref() {
                if frames.len() != 1 {
                    return Err("图片 stage1 必须只有一个槽位".into());
                }
                let frame = &frames[0];
                if frame.slot != 0 {
                    return Err("图片 stage1 槽位必须为 0".into());
                }
                validate_stage1_frame(frame, "图片")?;
            }
        }
        MediaKind::Video => {
            if probe.width == 0 || probe.height == 0 || probe.duration_ms.unwrap_or(0) == 0 {
                return Err("视频 probe 的尺寸或正时长无效".into());
            }
            if let Some(frames) = output.stage1_frames.as_ref() {
                if frames.len() != 6 {
                    return Err("视频 stage1 必须包含六个槽位".into());
                }
                let mut seen = [false; 6];
                for frame in frames {
                    if frame.slot > 5 || seen[usize::from(frame.slot)] {
                        return Err("视频 stage1 槽位必须唯一且位于 0..=5".into());
                    }
                    seen[usize::from(frame.slot)] = true;
                    validate_stage1_frame(frame, "视频")?;
                }
                if seen.iter().any(|present| !present) {
                    return Err("视频 stage1 必须覆盖 0..=5 全部槽位".into());
                }
            }
            if let Some(jpeg) = output.contact_sheet_jpeg.as_ref() {
                let cells = decode_contact_sheet(jpeg)
                    .map_err(|error| format!("联系表 JPEG 无法解码: {error}"))?;
                if cells
                    .iter()
                    .any(|cell| cell.width() != 320 || cell.height() != 180)
                {
                    return Err("联系表必须为固定 960x360 画布".into());
                }
            }
        }
        MediaKind::Other => {
            if probe.duration_ms.is_some() {
                return Err("其他文件 probe 不应包含视频时长".into());
            }
            if output
                .stage1_frames
                .as_ref()
                .is_some_and(|frames| !frames.is_empty())
            {
                return Err("其他文件 stage1 必须是空数组".into());
            }
        }
    }
    Ok(())
}

/// 校验一个图片或视频槽位恰好只有成功特征或失败诊断，并拒绝无效范围。
fn validate_stage1_frame(frame: &Stage1Frame, media_label: &str) -> Result<(), String> {
    if frame.feature.is_some() == frame.error.is_some() {
        return Err(format!(
            "{media_label} stage1 槽位必须恰好包含 feature 或 error"
        ));
    }
    if let Some(feature) = frame.feature.as_ref()
        && (feature.width == 0 || feature.height == 0 || feature.quality > 100)
    {
        return Err(format!("{media_label} stage1 特征尺寸或 Quality 无效"));
    }
    if let Some(error) = frame.error.as_ref()
        && error.trim().is_empty()
    {
        return Err(format!("{media_label} stage1 失败诊断不能为空"));
    }
    Ok(())
}

/// 在单写 actor 中准备联系表、提交 taskless stage1，并按事务结果确认或回滚。
#[allow(clippy::too_many_arguments)]
fn persist_completed_stage1(
    store: &mut dedup_node_store::NodeStore,
    content_id: dedup_node_store::ContentId,
    expected_key: ContentKey,
    item_id: &str,
    missing_parts: u32,
    output: BaseComputeOutput,
    contact_sheet_root: &std::path::Path,
    artifact_registry: Option<&std::sync::Arc<RegenerableArtifactRegistry>>,
    disk_full_cleaner: Option<&DiskFullCleaner>,
    worker_slot: Option<u32>,
) -> Result<BasePersistOutcome, String> {
    let cached = store
        .load_base_cache_record(content_id)
        .map_err(|error| error.to_string())?;
    if cached.content_key != expected_key {
        return Err("Media 持久化 content_id 与任务 ContentKey 不一致".into());
    }
    let media_kind = output
        .probe
        .as_ref()
        .map_or(cached.media_kind, |probe| probe.media_kind);
    let stage1_output = Stage1Output {
        media_kind,
        width: output
            .probe
            .as_ref()
            .map_or(cached.width.unwrap_or_default(), |probe| probe.width),
        height: output
            .probe
            .as_ref()
            .map_or(cached.height.unwrap_or_default(), |probe| probe.height),
        duration_ms: output
            .probe
            .as_ref()
            .and_then(|probe| probe.duration_ms)
            .or(cached.duration_ms),
        frames: output.stage1_frames.unwrap_or_default(),
        contact_sheet_jpeg: output.contact_sheet_jpeg,
    };
    let contact = ContactSheetCacheEntry::from_md5(contact_sheet_root, expected_key.md5());
    let prepared = super::engine::prepare_stage1_writes(
        store,
        item_id,
        &contact,
        missing_parts,
        missing_parts & BASE_MISSING_CONTACT_SHEET != 0,
        artifact_registry,
        disk_full_cleaner,
        stage1_output,
    )
    .map_err(|error| format!("基础特征准备失败: {error}"))?;
    let published = prepared
        .contact
        .map(|contact| contact.publish())
        .transpose()
        .map_err(|error| format!("视频缩略图保存失败: {error}"))?;
    let mut writes = prepared.writes;
    if let Some(contact) = &published {
        writes.push(contact.feature_write());
    }
    match store.commit_scan_stage1_taskless(content_id, prepared.media_kind, writes) {
        Ok(_) => {
            if let Some(contact) = published {
                contact.confirm();
            }
            Ok(BasePersistOutcome::Succeeded {
                worker_slot,
                cache_hit: false,
                media_kind: prepared.media_kind,
                file_size: expected_key.file_size(),
            })
        }
        Err(error) => match published.map(|contact| contact.rollback()) {
            None => Err(error.to_string()),
            Some(Ok(())) => Err(error.to_string()),
            Some(Err(cleanup)) => Err(format!("{error}; {cleanup}")),
        },
    }
}

#[cfg(test)]
mod tests {
    use std::{path::Path, time::Duration};

    use dedup_core::{ContentKey, DisplayPath, MachineId, MediaKind, NormalizedPath};
    use dedup_media::{
        ImageStage1, PdqHash, Rgb24Image, decode_contact_sheet, encode_contact_sheet,
    };
    use dedup_media_ffmpeg::MediaProbe;
    use dedup_node_store::{BaseCacheRecord, NodeStore, ScannedPath};
    use dedup_protocol::{
        BASE_MISSING_CONTACT_SHEET, BASE_MISSING_PROBE, BASE_MISSING_STAGE1, proto,
        proto::worker_envelope,
    };
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};
    use tokio::sync::mpsc::UnboundedReceiver;

    use crate::{
        io::{DiskReadClass, ReadFailure},
        scan::{
            BaseTaskInput, BaseTaskProducer, PlannedScannedPath, TaskDiskLane,
            base_persistence::BaseStoreActor,
            publish_contact_sheet_for_test,
            task_file_base_compute::TaskFileBaseComputePending,
            task_file_media_compute::{
                TaskFileMediaCompleted, TaskFileMediaFailure, TaskFileMediaTerminal,
            },
        },
        task_dispatch::{
            TaskDispatchAdmission, TaskDispatchPoll, TaskFileDispatcher, TaskLanePermitFuture,
            TaskLanePermitProvider,
        },
        task_files::{TaskFileIdentity, TaskFileRecord, TransientTaskFileSet},
        worker::{BaseComputeOutput, Stage1Frame, encode_base_compute_payload},
    };

    use super::super::base_persistence::{BasePersistAck, BasePersistIdentity, BasePersistOutcome};
    use super::{TaskFileMediaPersistenceOptions, TaskFilePersistRuntime};

    const RUN_ID: &str = "01900000-0000-7000-8000-0000000002c2";

    /// 测试用即时许可提供者，验证持久化阶段不会再次申请磁盘许可。
    #[derive(Clone)]
    struct TestProvider;

    impl TaskLanePermitProvider for TestProvider {
        type Permit = ();

        fn acquire(
            &self,
            _lane: TaskDiskLane,
            _class: DiskReadClass,
            _cancellation: ReadCancellationToken,
        ) -> TaskLanePermitFuture<Self::Permit> {
            Box::pin(async { Ok(()) })
        }
    }

    fn lane(disk: u32) -> TaskDiskLane {
        TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([disk]).unwrap(),
            physical_disk_numbers: vec![disk],
            disk_kind: LocalDiskKind::Hdd,
            configured_weight: 1,
            per_disk_limit: 1,
        }
    }

    fn scanned(path: &str, file_size: u64) -> ScannedPath {
        ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            file_size,
        )
    }

    fn seed_partial(
        store: &mut NodeStore,
        path: &str,
        md5: [u8; 16],
        file_size: u64,
    ) -> BaseCacheRecord {
        seed_partial_kind(store, path, md5, file_size, MediaKind::Other)
    }

    /// 创建指定媒体类型的部分缓存，供图片/视频 taskless 结果测试复用。
    fn seed_partial_kind(
        store: &mut NodeStore,
        path: &str,
        md5: [u8; 16],
        file_size: u64,
        media_kind: MediaKind,
    ) -> BaseCacheRecord {
        let content = store
            .upsert_content_and_location(&scanned(path, file_size), md5, media_kind)
            .unwrap();
        store.load_base_cache_record(content.id).unwrap()
    }

    fn production(
        root: &Path,
        path: &str,
        _md5: [u8; 16],
        file_size: u64,
        cached: BaseCacheRecord,
        contact_sheet_valid: bool,
    ) -> TaskFileBaseComputePending<TestProvider> {
        production_with_options(
            root,
            path,
            _md5,
            file_size,
            cached,
            contact_sheet_valid,
            false,
        )
    }

    /// 创建可控制强制重算标记的 Media pending，便于覆盖缓存快照和实际 probe 的组合。
    fn production_with_options(
        root: &Path,
        path: &str,
        _md5: [u8; 16],
        file_size: u64,
        cached: BaseCacheRecord,
        contact_sheet_valid: bool,
        force_recompute: bool,
    ) -> TaskFileBaseComputePending<TestProvider> {
        let files = TransientTaskFileSet::create(root, RUN_ID).unwrap();
        let mut producer = BaseTaskProducer::new(TaskFileDispatcher::new(files, TestProvider));
        producer
            .append_batch(&[BaseTaskInput {
                planned: PlannedScannedPath {
                    scanned: scanned(path, file_size),
                    lane: lane(7),
                },
                cached: Some(cached),
                contact_sheet_valid,
                force_recompute,
            }])
            .unwrap();
        TaskFileBaseComputePending::from_production(producer.seal().unwrap())
    }

    /// 从真实任务文件领取一项 Media 行，保留行记录供结果持久化使用。
    async fn take_media_task(
        mut pending: TaskFileBaseComputePending<TestProvider>,
    ) -> (
        TaskFileBaseComputePending<TestProvider>,
        TaskFileIdentity,
        TaskFileRecord,
    ) {
        let task = pending
            .dispatcher
            .next_with_admission(
                ReadCancellationToken::new(),
                TaskDispatchAdmission::media_only(),
            )
            .await;
        let task = match task.unwrap() {
            TaskDispatchPoll::Task(task) => task,
            other => panic!("expected Media task, got {other:?}"),
        };
        let identity = task.identity.clone();
        let record = task.record.clone();
        let _ = task.permit;
        (pending, identity, record)
    }

    fn image_output() -> BaseComputeOutput {
        BaseComputeOutput {
            probe: Some(MediaProbe {
                media_kind: MediaKind::Image,
                width: 2,
                height: 2,
                duration_ms: None,
            }),
            stage1_frames: Some(vec![Stage1Frame {
                slot: 0,
                feature: Some(ImageStage1 {
                    width: 2,
                    height: 2,
                    pdq: PdqHash::from_bytes([0; 32]),
                    quality: 100,
                }),
                error: None,
            }]),
            contact_sheet_jpeg: None,
        }
    }

    /// 生成合法的 Other probe；Other 的 stage1 结果是协议约定的空数组。
    fn other_output() -> BaseComputeOutput {
        BaseComputeOutput {
            probe: Some(MediaProbe {
                media_kind: MediaKind::Other,
                width: 0,
                height: 0,
                duration_ms: None,
            }),
            stage1_frames: Some(Vec::new()),
            contact_sheet_jpeg: None,
        }
    }

    /// 生成可由联系表缓存边界解码的固定六槽视频结果。
    fn video_output() -> BaseComputeOutput {
        let frames = (0..6)
            .map(|slot| Stage1Frame {
                slot,
                feature: Some(ImageStage1 {
                    width: 2,
                    height: 2,
                    pdq: PdqHash::from_bytes([slot; 32]),
                    quality: 100,
                }),
                error: None,
            })
            .collect();
        let frames_for_contact: [Option<Rgb24Image>; 6] = std::array::from_fn(|slot| {
            Some(Rgb24Image::new(8, 8, vec![slot as u8; 8 * 8 * 3]).unwrap())
        });
        BaseComputeOutput {
            probe: Some(MediaProbe {
                media_kind: MediaKind::Video,
                width: 2,
                height: 2,
                duration_ms: Some(1_000),
            }),
            stage1_frames: Some(frames),
            contact_sheet_jpeg: Some(encode_contact_sheet(&frames_for_contact, 320, 180).unwrap()),
        }
    }

    fn completed_response(
        identity: &TaskFileIdentity,
        md5: [u8; 16],
        output: BaseComputeOutput,
    ) -> proto::WorkerEnvelope {
        proto::WorkerEnvelope {
            payload: Some(worker_envelope::Payload::BaseComputeResult(
                proto::BaseComputeResult {
                    task_id: identity.run_id().to_owned(),
                    item_id: identity.item_id().to_string(),
                    md5: md5.to_vec(),
                    payload: encode_base_compute_payload(&output),
                },
            )),
        }
    }

    /// 用真实 Runtime 逐条投递并消费 ACK；仅供行为测试替代已删除的整轮 drain 包装器。
    async fn persist_terminals(
        mut pending: TaskFileBaseComputePending<TestProvider>,
        terminals: Vec<TaskFileMediaTerminal>,
        store: &super::super::base_persistence::BaseStoreHandle,
        acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
        options: &TaskFileMediaPersistenceOptions,
    ) -> Result<TaskFileBaseComputePending<TestProvider>, String> {
        let mut runtime = TaskFilePersistRuntime::new();
        for terminal in terminals {
            runtime.enqueue_media_terminal(terminal, options)?;
        }
        while !runtime.is_empty() {
            runtime.try_submit(store)?;
            if !runtime.is_empty() {
                let ack = acknowledgements
                    .recv()
                    .await
                    .ok_or_else(|| "测试持久化 actor 未返回 ACK".to_owned())?;
                runtime.apply_ack(&mut pending, ack)?;
            }
        }
        Ok(pending)
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn persist_runtime_applies_only_acknowledged_identity() {
        let root = tempfile::tempdir().unwrap();
        let machine = MachineId::from_sha256([0x81; 32]);
        let store = NodeStore::open_in_memory(machine).unwrap();
        let first_lane = lane(7);
        let second_lane = lane(8);
        let files = TransientTaskFileSet::create(root.path(), RUN_ID).unwrap();
        let mut producer = BaseTaskProducer::new(TaskFileDispatcher::new(files, TestProvider));
        producer
            .append_batch(&[
                BaseTaskInput {
                    planned: PlannedScannedPath {
                        scanned: scanned(r"C:\persist-runtime-first.bin", 10),
                        lane: first_lane.clone(),
                    },
                    cached: None,
                    contact_sheet_valid: true,
                    force_recompute: false,
                },
                BaseTaskInput {
                    planned: PlannedScannedPath {
                        scanned: scanned(r"C:\persist-runtime-second.bin", 11),
                        lane: second_lane.clone(),
                    },
                    cached: None,
                    contact_sheet_valid: true,
                    force_recompute: false,
                },
            ])
            .unwrap();
        let mut pending = TaskFileBaseComputePending::from_production(producer.seal().unwrap());
        let first = match pending
            .dispatcher
            .next_with_admission(
                ReadCancellationToken::new(),
                TaskDispatchAdmission::hash_only(),
            )
            .await
            .unwrap()
        {
            TaskDispatchPoll::Task(task) => task,
            other => panic!("expected first Hash task, got {other:?}"),
        };
        let second = match pending
            .dispatcher
            .next_with_admission(
                ReadCancellationToken::new(),
                TaskDispatchAdmission::hash_only(),
            )
            .await
            .unwrap()
        {
            TaskDispatchPoll::Task(task) => task,
            other => panic!("expected second Hash task, got {other:?}"),
        };
        let first_identity = first.identity.clone();
        let second_identity = second.identity.clone();
        let first_scanned = first.record.scanned.clone();
        let second_scanned = second.record.scanned.clone();
        let _ = first.permit;
        let _ = second.permit;

        let (controller, waiter) = super::super::base_persistence::BasePersistTestController::new();
        let (actor, handle, mut acknowledgements) =
            BaseStoreActor::spawn_with_first_persist_waiter(store, 2, waiter);
        let mut runtime = TaskFilePersistRuntime::new();
        runtime
            .enqueue_hash_complete(
                first_identity.clone(),
                first_scanned.clone(),
                [0x31; 16],
                MediaKind::Other,
                ContentKey::new([0x31; 16], 10),
            )
            .unwrap();
        assert!(
            runtime
                .enqueue_hash_complete(
                    first_identity.clone(),
                    first_scanned,
                    [0x31; 16],
                    MediaKind::Other,
                    ContentKey::new([0x31; 16], 10),
                )
                .is_err()
        );
        runtime
            .enqueue_hash_failure(
                second_identity,
                second_scanned,
                ReadFailure::Io {
                    path: std::path::PathBuf::from(r"C:\persist-runtime-second.bin"),
                    block_offset: 0,
                    source: std::io::Error::other("测试 Hash 失败"),
                },
            )
            .unwrap();
        runtime.try_submit(&handle).unwrap();
        tokio::time::timeout(Duration::from_secs(2), controller.wait_until_entered())
            .await
            .expect("首条持久化操作必须进入 actor");
        assert!(
            runtime
                .apply_ack(
                    &mut pending,
                    BasePersistAck {
                        identity: BasePersistIdentity::TaskFile(first_identity.clone()),
                        queue_wait: Duration::ZERO,
                        transaction_elapsed: Duration::ZERO,
                        result: Ok(BasePersistOutcome::Cancelled { worker_slot: None }),
                    },
                )
                .is_err()
        );
        assert!(runtime.has_in_flight(), "错误 ACK 不得丢弃仍为 P 的动作");
        controller.release();
        let ack = tokio::time::timeout(Duration::from_secs(2), acknowledgements.recv())
            .await
            .expect("首条持久化 ACK 必须返回")
            .unwrap();
        runtime.apply_ack(&mut pending, ack).unwrap();

        let first_path = pending.dispatcher.lane_path(&first_lane).unwrap();
        let second_path = pending.dispatcher.lane_path(&second_lane).unwrap();
        assert_eq!(std::fs::read(&first_path).unwrap()[0], b'C');
        assert_eq!(std::fs::read(&second_path).unwrap()[0], b'P');
        assert!(runtime.has_in_flight());
        let ack = tokio::time::timeout(Duration::from_secs(2), acknowledgements.recv())
            .await
            .expect("第二条持久化 ACK 必须返回")
            .unwrap();
        runtime.apply_ack(&mut pending, ack).unwrap();
        assert_eq!(std::fs::read(first_path).unwrap()[0], b'C');
        assert_eq!(std::fs::read(second_path).unwrap()[0], b'F');
        assert!(runtime.is_empty());
        drop(handle);
        drop(acknowledgements);
        tokio::time::timeout(Duration::from_secs(2), actor.finish())
            .await
            .expect("持久化 actor 必须关闭")
            .unwrap();
    }

    #[tokio::test]
    async fn first_uncached_image_with_generic_mask_completes_without_contact_sheet() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xE1; 16];
        let machine = MachineId::from_sha256([0xE2; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial_kind(
            &mut store,
            r"C:\seed-first-image.bin",
            md5,
            10,
            MediaKind::Other,
        );
        let (mut pending, identity, record) = take_media_task(production_with_options(
            root.path(),
            r"C:\first-image.bin",
            md5,
            10,
            cached,
            true,
            true,
        ))
        .await;
        assert_eq!(
            record.missing.base_missing_parts(),
            BASE_MISSING_PROBE | BASE_MISSING_STAGE1 | BASE_MISSING_CONTACT_SHEET
        );
        // Hash 已经关联 content_id，但没有可复用的缓存快照，模拟首次无缓存续算。
        pending.contexts.get_mut(&identity).unwrap().cached = None;
        let context = pending.contexts.get(&identity).unwrap().clone();
        let content_id = context.content_id.unwrap();
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let terminals = vec![TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
            identity: identity.clone(),
            record,
            context,
            response: completed_response(&identity, md5, image_output()),
            worker_slot: Some(7),
        })];
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_terminals(
            pending,
            terminals,
            &handle,
            &mut acknowledgements,
            &TaskFileMediaPersistenceOptions::default(),
        )
        .await
        .unwrap();
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'C');
        assert!(result.contexts.is_empty());
        drop(handle);
        let store = actor.finish().await.unwrap();
        assert!(
            store
                .load_base_cache_record(content_id)
                .unwrap()
                .base_complete
        );
        assert!(store.page_file_faults(None, 10).unwrap().items.is_empty());
    }

    #[tokio::test]
    async fn first_uncached_other_with_generic_mask_completes_without_features() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xE3; 16];
        let machine = MachineId::from_sha256([0xE4; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial_kind(
            &mut store,
            r"C:\seed-first-other.bin",
            md5,
            10,
            MediaKind::Other,
        );
        let (mut pending, identity, record) = take_media_task(production_with_options(
            root.path(),
            r"C:\first-other.bin",
            md5,
            10,
            cached,
            true,
            true,
        ))
        .await;
        assert_eq!(
            record.missing.base_missing_parts(),
            BASE_MISSING_PROBE | BASE_MISSING_STAGE1 | BASE_MISSING_CONTACT_SHEET
        );
        assert!(pending.contexts.get(&identity).unwrap().force_recompute);
        pending.contexts.get_mut(&identity).unwrap().cached = None;
        let context = pending.contexts.get(&identity).unwrap().clone();
        let content_id = context.content_id.unwrap();
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let terminals = vec![TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
            identity: identity.clone(),
            record,
            context,
            response: completed_response(&identity, md5, other_output()),
            worker_slot: Some(8),
        })];
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_terminals(
            pending,
            terminals,
            &handle,
            &mut acknowledgements,
            &TaskFileMediaPersistenceOptions::default(),
        )
        .await
        .unwrap();
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'C');
        assert!(result.contexts.is_empty());
        drop(handle);
        let store = actor.finish().await.unwrap();
        assert!(
            store
                .load_base_cache_record(content_id)
                .unwrap()
                .base_complete
        );
        assert!(store.page_file_faults(None, 10).unwrap().items.is_empty());
    }

    #[tokio::test]
    async fn forced_image_recompute_accepts_stage1_without_contact_sheet() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xE5; 16];
        let machine = MachineId::from_sha256([0xE6; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial_kind(
            &mut store,
            r"C:\seed-force-image.bin",
            md5,
            10,
            MediaKind::Image,
        );
        let (pending, identity, record) = take_media_task(production_with_options(
            root.path(),
            r"C:\force-image.bin",
            md5,
            10,
            cached,
            true,
            true,
        ))
        .await;
        let context = pending.contexts.get(&identity).unwrap().clone();
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let terminals = vec![TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
            identity: identity.clone(),
            record,
            context,
            response: completed_response(&identity, md5, image_output()),
            worker_slot: Some(9),
        })];
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_terminals(
            pending,
            terminals,
            &handle,
            &mut acknowledgements,
            &TaskFileMediaPersistenceOptions::default(),
        )
        .await
        .unwrap();
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'C');
        assert!(result.contexts.is_empty());
        drop(handle);
        let store = actor.finish().await.unwrap();
        assert!(store.page_file_faults(None, 10).unwrap().items.is_empty());
    }

    #[cfg(feature = "test-hooks")]
    #[tokio::test]
    async fn success_keeps_tsv_p_until_taskless_ack_then_marks_c_and_resolved() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xA1; 16];
        let machine = MachineId::from_sha256([0xA2; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial(&mut store, r"C:\seed-image.bin", md5, 10);
        let (pending, identity, record) = take_media_task(production(
            root.path(),
            r"C:\media-persist.bin",
            md5,
            10,
            cached,
            true,
        ))
        .await;
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let context = pending.contexts.get(&identity).unwrap().clone();
        let terminals = vec![TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
            identity: identity.clone(),
            record,
            context,
            response: completed_response(&identity, md5, image_output()),
            worker_slot: Some(1),
        })];
        let (controller, waiter) = super::super::base_persistence::BasePersistTestController::new();
        let (actor, handle, mut acknowledgements) =
            BaseStoreActor::spawn_with_first_persist_waiter(store, 2, waiter);
        let options = TaskFileMediaPersistenceOptions {
            contact_sheet_root: root.path().join("contacts"),
            ..Default::default()
        };
        let handle_for_run = handle.clone();
        let join = tokio::spawn(async move {
            persist_terminals(
                pending,
                terminals,
                &handle_for_run,
                &mut acknowledgements,
                &options,
            )
            .await
        });
        controller.wait_until_entered().await;
        assert_eq!(std::fs::read(&task_path).unwrap()[0], b'P');
        controller.release();
        let pending = join.await.unwrap().unwrap();
        assert_eq!(std::fs::read(&task_path).unwrap()[0], b'C');
        assert!(pending.contexts.is_empty());
        assert_eq!(pending.manifest.cache_hits, 0);
        assert_eq!(pending.manifest.resolved_files.len(), 1);
        drop(handle);
        let store = actor.finish().await.unwrap();
        assert!(store.page_tasks(None, 100).unwrap().items.is_empty());
    }

    #[tokio::test]
    async fn file_failure_ack_marks_f_without_writing_task_tables() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xB1; 16];
        let machine = MachineId::from_sha256([0xB2; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial(&mut store, r"C:\seed-failure.bin", md5, 10);
        let (pending, identity, record) = take_media_task(production(
            root.path(),
            r"C:\media-persist.bin",
            md5,
            10,
            cached,
            true,
        ))
        .await;
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let terminals = vec![TaskFileMediaTerminal::Failed(TaskFileMediaFailure {
            identity: identity.clone(),
            record,
            message: "测试 Worker 失败".into(),
            worker_slot: Some(2),
        })];
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_terminals(
            pending,
            terminals,
            &handle,
            &mut acknowledgements,
            &TaskFileMediaPersistenceOptions::default(),
        )
        .await
        .unwrap();
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'F');
        assert!(result.contexts.is_empty());
        drop(handle);
        let store = actor.finish().await.unwrap();
        assert!(store.page_tasks(None, 100).unwrap().items.is_empty());
        let faults = store.page_file_faults(None, 10).unwrap().items;
        assert_eq!(faults.len(), 1);
        assert_eq!(faults[0].kind, dedup_node_store::FileFaultKind::WorkerCrash);
        assert_eq!(faults[0].stage, "base");
    }

    #[tokio::test]
    async fn invalid_worker_quality_becomes_file_fault_and_never_completes_base() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xB6; 16];
        let machine = MachineId::from_sha256([0xB7; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial_kind(
            &mut store,
            r"C:\seed-invalid-quality.bin",
            md5,
            10,
            MediaKind::Image,
        );
        let (pending, identity, record) = take_media_task(production(
            root.path(),
            r"C:\media-invalid-quality.bin",
            md5,
            10,
            cached,
            true,
        ))
        .await;
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let context = pending.contexts.get(&identity).unwrap().clone();
        let content_id = context.content_id.unwrap();
        let mut output = image_output();
        output
            .stage1_frames
            .as_mut()
            .unwrap()
            .first_mut()
            .unwrap()
            .feature
            .as_mut()
            .unwrap()
            .quality = 101;
        let terminals = vec![TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
            identity: identity.clone(),
            record,
            context,
            response: completed_response(&identity, md5, output),
            worker_slot: Some(6),
        })];
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_terminals(
            pending,
            terminals,
            &handle,
            &mut acknowledgements,
            &TaskFileMediaPersistenceOptions::default(),
        )
        .await
        .unwrap();
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'F');
        assert!(result.contexts.is_empty());
        drop(handle);
        let store = actor.finish().await.unwrap();
        let faults = store.page_file_faults(None, 10).unwrap().items;
        assert_eq!(faults.len(), 1);
        assert_eq!(faults[0].kind, dedup_node_store::FileFaultKind::WorkerCrash);
        assert_eq!(faults[0].stage, "base");
        assert_eq!(
            faults[0].normalized_path.as_str(),
            r"C:\MEDIA-INVALID-QUALITY.BIN"
        );
        assert!(
            !store
                .load_base_cache_record(content_id)
                .unwrap()
                .base_complete
        );
    }

    #[test]
    fn damaged_existing_contact_sheet_is_replaced_by_valid_partial() {
        let root = tempfile::tempdir().unwrap();
        let temp = root.path().join("new.partial");
        let final_path = root.path().join("content.jpg");
        let valid = video_output().contact_sheet_jpeg.unwrap();
        std::fs::write(&final_path, b"damaged-jpeg").unwrap();
        std::fs::write(&temp, &valid).unwrap();

        publish_contact_sheet_for_test(&temp, &final_path, || Ok(())).unwrap();

        assert_eq!(std::fs::read(&final_path).unwrap(), valid);
        assert!(decode_contact_sheet(&std::fs::read(&final_path).unwrap()).is_ok());
        assert!(!temp.exists());
    }

    #[test]
    fn contact_sheet_replacement_rollback_restores_previous_file() {
        let root = tempfile::tempdir().unwrap();
        let temp = root.path().join("rollback.partial");
        let final_path = root.path().join("content.jpg");
        let previous = b"previous-valid-or-damaged".to_vec();
        let replacement = video_output().contact_sheet_jpeg.unwrap();
        std::fs::write(&final_path, &previous).unwrap();
        std::fs::write(&temp, &replacement).unwrap();

        let error = publish_contact_sheet_for_test(&temp, &final_path, || {
            Err(crate::scan::ScanError::Stage1("模拟引用失败".into()))
        })
        .unwrap_err();

        assert!(error.to_string().contains("模拟引用失败"));
        assert_eq!(std::fs::read(&final_path).unwrap(), previous);
        assert!(!temp.exists());
    }

    #[tokio::test]
    async fn file_failure_and_success_ack_each_move_only_their_own_row() {
        let root = tempfile::tempdir().unwrap();
        let success_md5 = [0xB3; 16];
        let failure_md5 = [0xB4; 16];
        let machine = MachineId::from_sha256([0xB5; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let success_cached = seed_partial(&mut store, r"C:\seed-success.bin", success_md5, 10);
        let failure_cached = seed_partial(&mut store, r"C:\seed-failure-2.bin", failure_md5, 10);
        let success_lane = lane(7);
        let failure_lane = lane(8);
        let files = TransientTaskFileSet::create(root.path(), RUN_ID).unwrap();
        let mut producer = BaseTaskProducer::new(TaskFileDispatcher::new(files, TestProvider));
        producer
            .append_batch(&[
                BaseTaskInput {
                    planned: PlannedScannedPath {
                        scanned: scanned(r"C:\media-success.bin", 10),
                        lane: success_lane.clone(),
                    },
                    cached: Some(success_cached),
                    contact_sheet_valid: true,
                    force_recompute: false,
                },
                BaseTaskInput {
                    planned: PlannedScannedPath {
                        scanned: scanned(r"C:\media-failure-2.bin", 10),
                        lane: failure_lane.clone(),
                    },
                    cached: Some(failure_cached),
                    contact_sheet_valid: true,
                    force_recompute: false,
                },
            ])
            .unwrap();
        let (pending, success_identity, success_record) = take_media_task(
            TaskFileBaseComputePending::from_production(producer.seal().unwrap()),
        )
        .await;
        let (pending, failure_identity, failure_record) = take_media_task(pending).await;
        let success_path = pending.dispatcher.lane_path(&success_lane).unwrap();
        let failure_path = pending.dispatcher.lane_path(&failure_lane).unwrap();
        let success_context = pending.contexts.get(&success_identity).unwrap().clone();
        let terminals = vec![
            TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
                identity: success_identity.clone(),
                record: success_record,
                context: success_context,
                response: completed_response(&success_identity, success_md5, image_output()),
                worker_slot: Some(4),
            }),
            TaskFileMediaTerminal::Failed(TaskFileMediaFailure {
                identity: failure_identity.clone(),
                record: failure_record,
                message: "测试另一项 Worker 失败".into(),
                worker_slot: Some(5),
            }),
        ];
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let pending = persist_terminals(
            pending,
            terminals,
            &handle,
            &mut acknowledgements,
            &TaskFileMediaPersistenceOptions::default(),
        )
        .await
        .unwrap();
        assert_eq!(std::fs::read(success_path).unwrap()[0], b'C');
        assert_eq!(std::fs::read(failure_path).unwrap()[0], b'F');
        assert!(pending.contexts.is_empty());
        assert_eq!(pending.manifest.resolved_files.len(), 1);
        drop(handle);
        let store = actor.finish().await.unwrap();
        assert!(store.page_tasks(None, 100).unwrap().items.is_empty());
    }

    #[tokio::test]
    async fn completed_md5_mismatch_becomes_file_failure_and_keeps_ack_boundary() {
        let root = tempfile::tempdir().unwrap();
        let expected = [0xC1; 16];
        let returned = [0xC2; 16];
        let machine = MachineId::from_sha256([0xC3; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial(&mut store, r"C:\seed-mismatch.bin", expected, 10);
        let (pending, identity, record) = take_media_task(production(
            root.path(),
            r"C:\media-persist.bin",
            expected,
            10,
            cached,
            true,
        ))
        .await;
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let context = pending.contexts.get(&identity).unwrap().clone();
        let terminals = vec![TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
            identity: identity.clone(),
            record,
            context,
            response: completed_response(&identity, returned, image_output()),
            worker_slot: None,
        })];
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_terminals(
            pending,
            terminals,
            &handle,
            &mut acknowledgements,
            &TaskFileMediaPersistenceOptions::default(),
        )
        .await
        .unwrap();
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'F');
        assert!(result.contexts.is_empty());
        drop(handle);
        actor.finish().await.unwrap();
    }

    #[tokio::test]
    async fn video_completion_publishes_contact_sheet_before_ack_then_confirms_it() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xD1; 16];
        let machine = MachineId::from_sha256([0xD2; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial_kind(&mut store, r"C:\seed-video.bin", md5, 10, MediaKind::Video);
        let (pending, identity, record) = take_media_task(production(
            root.path(),
            r"C:\media-video.bin",
            md5,
            10,
            cached,
            false,
        ))
        .await;
        assert_ne!(
            record.missing.base_missing_parts() & BASE_MISSING_CONTACT_SHEET,
            0
        );
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let context = pending.contexts.get(&identity).unwrap().clone();
        let content_id = context.content_id.unwrap();
        let terminals = vec![TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
            identity: identity.clone(),
            record,
            context,
            response: completed_response(&identity, md5, video_output()),
            worker_slot: Some(3),
        })];
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let options = TaskFileMediaPersistenceOptions {
            contact_sheet_root: root.path().join("contacts"),
            ..Default::default()
        };
        let pending =
            persist_terminals(pending, terminals, &handle, &mut acknowledgements, &options)
                .await
                .unwrap();
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'C');
        assert!(pending.contexts.is_empty());
        drop(handle);
        let store = actor.finish().await.unwrap();
        let cached = store.load_base_cache_record(content_id).unwrap();
        assert_eq!(cached.media_kind, MediaKind::Video);
        assert!(cached.contact_sheet_relative_path.is_some());
    }

    #[tokio::test]
    async fn contact_write_store_error_returns_pending_without_moving_tsv() {
        let root = tempfile::tempdir().unwrap();
        let md5 = [0xE1; 16];
        let machine = MachineId::from_sha256([0xE2; 32]);
        let mut store = NodeStore::open_in_memory(machine).unwrap();
        let cached = seed_partial_kind(
            &mut store,
            r"C:\seed-video-error.bin",
            md5,
            10,
            MediaKind::Video,
        );
        let (pending, identity, record) = take_media_task(production(
            root.path(),
            r"C:\media-video-error.bin",
            md5,
            10,
            cached,
            false,
        ))
        .await;
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let context = pending.contexts.get(&identity).unwrap().clone();
        let terminal = TaskFileMediaTerminal::Completed(TaskFileMediaCompleted {
            identity: identity.clone(),
            record,
            context,
            response: completed_response(&identity, md5, video_output()),
            worker_slot: None,
        });
        let bad_root = root.path().join("not-a-directory");
        std::fs::write(&bad_root, b"block directory creation").unwrap();
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let mut pending = pending;
        let mut runtime = TaskFilePersistRuntime::new();
        runtime
            .enqueue_media_terminal(
                terminal,
                &TaskFileMediaPersistenceOptions {
                    contact_sheet_root: bad_root,
                    ..Default::default()
                },
            )
            .unwrap();
        runtime.try_submit(&handle).unwrap();
        let ack = acknowledgements.recv().await.unwrap();
        assert!(
            runtime.apply_ack(&mut pending, ack).is_err(),
            "联系表写入失败必须保留未 ACK 的 pending owner"
        );
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'P');
        assert_eq!(pending.contexts.len(), 1);
        drop(handle);
        actor.finish().await.unwrap();
    }
}

//! 瞬态任务文件 Media 结果的 taskless SQLite 持久化边界。
//!
//! 本模块只消费 Media 阶段已经拥有的结果。Worker 事件和调度由前一阶段负责，
//! 本模块在 SQLite ACK 到达前保持 TSV 行和内存上下文为 `P`，ACK 后才迁移为 `C/F`。

use std::{
    collections::{BTreeMap, VecDeque},
    fmt,
    time::{SystemTime, UNIX_EPOCH},
};

use dedup_core::{ContentKey, MediaKind};
use dedup_media::decode_contact_sheet;
use dedup_node_store::{FileFaultKind, FileFaultRecord, ResolvedScanFile};
use dedup_protocol::{
    BASE_MISSING_CONTACT_SHEET, BASE_MISSING_PROBE, BASE_MISSING_STAGE1, proto::worker_envelope,
};
use tokio::sync::mpsc::UnboundedReceiver;

use super::{
    base_persistence::{
        BasePersistAck, BasePersistIdentity, BasePersistMessage, BasePersistOutcome,
        BasePersistSendError, BaseStoreHandle,
    },
    task_file_base_compute::TaskFileBaseComputePending,
    task_file_media_compute::{MediaPassResult, TaskFileMediaCompleted, TaskFileMediaFailure},
};
use crate::{
    artifact_registry::RegenerableArtifactRegistry,
    contact_sheet_cache::ContactSheetCacheEntry,
    disk_full_cleanup::DiskFullCleaner,
    task_dispatch::TaskLanePermitProvider,
    task_files::TaskFileIdentity,
    worker::{BaseComputeOutput, Stage1Frame, Stage1Output, decode_base_compute_payload},
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

/// Media taskless 持久化发生任务级错误时携带未完成 pending 所有权。
pub(crate) struct TaskFileMediaPersistenceError<P: TaskLanePermitProvider> {
    /// 面向调用方的诊断文本。
    message: String,
    /// 尚未 ACK 的 dispatcher、上下文和清单。
    pending: TaskFileBaseComputePending<P>,
}

impl<P: TaskLanePermitProvider> TaskFileMediaPersistenceError<P> {
    /// 消费错误并取回剩余任务文件所有权。
    pub(crate) fn into_pending(self) -> TaskFileBaseComputePending<P> {
        self.pending
    }
}

impl<P: TaskLanePermitProvider> fmt::Display for TaskFileMediaPersistenceError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.message)
    }
}

impl<P: TaskLanePermitProvider> fmt::Debug for TaskFileMediaPersistenceError<P> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("TaskFileMediaPersistenceError")
            .field("message", &self.message)
            .finish_non_exhaustive()
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

/// 消费一轮 Media 结果并执行 taskless stage1/失败 ACK。
///
/// 所有任务文件状态迁移都延迟到 ACK；函数返回时剩余 pending 仍拥有未完成的
/// dispatcher、内存上下文和清单，供外层继续 Hash/Media 或 discard。
pub(crate) async fn persist_task_file_media_results<P>(
    media: MediaPassResult<P>,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
    options: TaskFileMediaPersistenceOptions,
) -> Result<TaskFileBaseComputePending<P>, TaskFileMediaPersistenceError<P>>
where
    P: TaskLanePermitProvider,
{
    let MediaPassResult {
        mut pending,
        completed,
        file_failures,
        ..
    } = media;
    let mut queue = VecDeque::new();
    let mut identities = BTreeMap::<TaskFileIdentity, ()>::new();

    for item in completed {
        if let Err(message) = validate_context(&pending, &item.identity, &item.context) {
            return Err(persistence_error(pending, message));
        }
        if identities.insert(item.identity.clone(), ()).is_some() {
            return Err(persistence_error(pending, "Media 结果包含重复任务文件身份"));
        }
        queue.push_back(build_completed_message(item, &options));
    }
    for item in file_failures {
        if let Err(message) = validate_context(&pending, &item.identity, &item.context) {
            return Err(persistence_error(pending, message));
        }
        if identities.insert(item.identity.clone(), ()).is_some() {
            return Err(persistence_error(
                pending,
                "Media 失败结果包含重复任务文件身份",
            ));
        }
        queue.push_back(build_failure_message(item));
    }

    let mut in_flight = BTreeMap::new();
    if let Err(message) = flush_persist_queue(
        &mut pending,
        &mut queue,
        &mut in_flight,
        store,
        acknowledgements,
    )
    .await
    {
        return Err(persistence_error(pending, message));
    }
    Ok(pending)
}

/// 确认 Media 结果上下文仍是 Hash/生产阶段保存的同一份快照。
fn validate_context<P: TaskLanePermitProvider>(
    pending: &TaskFileBaseComputePending<P>,
    identity: &TaskFileIdentity,
    context: &super::TaskFileBaseContext,
) -> Result<(), String> {
    if pending.contexts.get(identity) != Some(context) {
        return Err("Media 结果上下文与任务文件身份不一致".into());
    }
    Ok(())
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
            context: item.context,
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
    // 通用缺失掩码对首次探测使用同一组位，但 Other 不会产生 stage1；实际
    // probe 类型决定该字段是否应该出现，不能把掩码位直接当成所有媒体的协议要求。
    let stage1_expected = !matches!(probe.media_kind, MediaKind::Other) && stage1_requested;
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

/// 尝试发送所有待持久化消息，队列满时先消费真实 ACK。
async fn flush_persist_queue<P: TaskLanePermitProvider>(
    pending: &mut TaskFileBaseComputePending<P>,
    queue: &mut VecDeque<PendingMediaPersist>,
    in_flight: &mut BTreeMap<TaskFileIdentity, MediaPersistAction>,
    store: &BaseStoreHandle,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
) -> Result<(), String> {
    while !queue.is_empty() || !in_flight.is_empty() {
        if let Some(mut item) = queue.pop_front() {
            match store.try_persist(item.message) {
                Ok(()) => {
                    in_flight.insert(item.identity, item.action);
                }
                Err(BasePersistSendError::Full(message)) => {
                    item.message = message;
                    queue.push_front(item);
                    if in_flight.is_empty() {
                        tokio::task::yield_now().await;
                    } else {
                        apply_one_ack(pending, in_flight, acknowledgements).await?;
                    }
                }
                Err(BasePersistSendError::Closed(_)) => {
                    return Err("基础持久化 actor 已关闭".into());
                }
            }
        } else {
            apply_one_ack(pending, in_flight, acknowledgements).await?;
        }
    }
    Ok(())
}

/// 消费一条 ACK；只有结果类型、身份和关键字段都匹配才迁移 TSV。
async fn apply_one_ack<P: TaskLanePermitProvider>(
    pending: &mut TaskFileBaseComputePending<P>,
    in_flight: &mut BTreeMap<TaskFileIdentity, MediaPersistAction>,
    acknowledgements: &mut UnboundedReceiver<BasePersistAck>,
) -> Result<(), String> {
    let ack = acknowledgements
        .recv()
        .await
        .ok_or_else(|| "基础持久化 actor 未返回 Media ACK".to_owned())?;
    let identity = match ack.identity {
        BasePersistIdentity::TaskFile(identity) => identity,
        BasePersistIdentity::Legacy(_) => return Err("Media 收到旧任务表持久化 ACK".into()),
    };
    let action = in_flight
        .remove(&identity)
        .ok_or_else(|| "Media 收到未知持久化 ACK".to_owned())?;
    let outcome = ack.result?;
    match (action, outcome) {
        (
            MediaPersistAction::Complete {
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
        ) if ack_slot == worker_slot
            && !cache_hit
            && ack_kind == media_kind
            && file_size == scanned.file_size =>
        {
            pending
                .dispatcher
                .mark_completed(&identity)
                .map_err(|error| error.to_string())?;
            pending.contexts.remove(&identity);
            pending.manifest.resolved_files.push(ResolvedScanFile {
                scanned,
                content: content_key,
            });
            pending.manifest.resolved_files.sort_by(|left, right| {
                left.scanned
                    .normalized_path
                    .cmp(&right.scanned.normalized_path)
            });
        }
        (
            MediaPersistAction::Failed {
                display_path,
                worker_slot,
            },
            BasePersistOutcome::Failed {
                display_path: ack_path,
                worker_slot: ack_slot,
                skipped_incomplete: false,
                ..
            },
        ) if ack_path == display_path && ack_slot == worker_slot => {
            pending
                .dispatcher
                .mark_failed(&identity)
                .map_err(|error| error.to_string())?;
            pending.contexts.remove(&identity);
        }
        (_, BasePersistOutcome::Ignored) => return Err("Media 持久化 ACK 被忽略".into()),
        (_, BasePersistOutcome::Cancelled { .. }) => return Err("Media 收到取消 ACK".into()),
        _ => return Err("Media 持久化 ACK 类型或字段不匹配".into()),
    }
    Ok(())
}

/// 构造带 pending 所有权的任务级错误。
fn persistence_error<P: TaskLanePermitProvider>(
    pending: TaskFileBaseComputePending<P>,
    message: impl Into<String>,
) -> TaskFileMediaPersistenceError<P> {
    TaskFileMediaPersistenceError {
        message: message.into(),
        pending,
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use dedup_core::{DisplayPath, MachineId, MediaKind, NormalizedPath};
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

    use crate::{
        io::DiskReadClass,
        scan::{
            BaseTaskInput, BaseTaskProducer, PlannedScannedPath, TaskDiskLane,
            base_persistence::BaseStoreActor,
            publish_contact_sheet_for_test,
            task_file_base_compute::TaskFileBaseComputePending,
            task_file_media_compute::{
                MediaPassResult, TaskFileMediaCompleted, TaskFileMediaFailure,
            },
        },
        task_dispatch::{
            TaskDispatchAdmission, TaskDispatchPoll, TaskFileDispatcher, TaskLanePermitFuture,
            TaskLanePermitProvider,
        },
        task_files::{TaskFileIdentity, TaskFileRecord, TransientTaskFileSet},
        worker::{BaseComputeOutput, Stage1Frame, encode_base_compute_payload},
    };

    use super::{TaskFileMediaPersistenceOptions, persist_task_file_media_results};

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

    /// 生成合法的 Other probe；Other 不产生图片 stage1 或视频联系表。
    fn other_output() -> BaseComputeOutput {
        BaseComputeOutput {
            probe: Some(MediaProbe {
                media_kind: MediaKind::Other,
                width: 0,
                height: 0,
                duration_ms: None,
            }),
            stage1_frames: None,
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
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: identity.clone(),
                record,
                context,
                response: completed_response(&identity, md5, image_output()),
                worker_slot: Some(7),
            }],
            file_failures: Vec::new(),
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_task_file_media_results(
            media,
            &handle,
            &mut acknowledgements,
            TaskFileMediaPersistenceOptions::default(),
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
        pending.contexts.get_mut(&identity).unwrap().cached = None;
        let context = pending.contexts.get(&identity).unwrap().clone();
        let content_id = context.content_id.unwrap();
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: identity.clone(),
                record,
                context,
                response: completed_response(&identity, md5, other_output()),
                worker_slot: Some(8),
            }],
            file_failures: Vec::new(),
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_task_file_media_results(
            media,
            &handle,
            &mut acknowledgements,
            TaskFileMediaPersistenceOptions::default(),
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
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: identity.clone(),
                record,
                context,
                response: completed_response(&identity, md5, image_output()),
                worker_slot: Some(9),
            }],
            file_failures: Vec::new(),
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_task_file_media_results(
            media,
            &handle,
            &mut acknowledgements,
            TaskFileMediaPersistenceOptions::default(),
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
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: identity.clone(),
                record,
                context,
                response: completed_response(&identity, md5, image_output()),
                worker_slot: Some(1),
            }],
            file_failures: Vec::new(),
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (controller, waiter) = super::super::base_persistence::BasePersistTestController::new();
        let (actor, handle, mut acknowledgements) =
            BaseStoreActor::spawn_with_first_persist_waiter(store, 2, waiter);
        let options = TaskFileMediaPersistenceOptions {
            contact_sheet_root: root.path().join("contacts"),
            ..Default::default()
        };
        let handle_for_run = handle.clone();
        let join = tokio::spawn(async move {
            persist_task_file_media_results(media, &handle_for_run, &mut acknowledgements, options)
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
        let context = pending.contexts.get(&identity).unwrap().clone();
        let media = MediaPassResult {
            pending,
            completed: Vec::new(),
            file_failures: vec![TaskFileMediaFailure {
                identity: identity.clone(),
                record,
                context,
                message: "测试 Worker 失败".into(),
                worker_slot: Some(2),
            }],
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_task_file_media_results(
            media,
            &handle,
            &mut acknowledgements,
            TaskFileMediaPersistenceOptions::default(),
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
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: identity.clone(),
                record,
                context,
                response: completed_response(&identity, md5, output),
                worker_slot: Some(6),
            }],
            file_failures: Vec::new(),
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_task_file_media_results(
            media,
            &handle,
            &mut acknowledgements,
            TaskFileMediaPersistenceOptions::default(),
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
        let files = TransientTaskFileSet::create(root.path(), RUN_ID).unwrap();
        let mut producer = BaseTaskProducer::new(TaskFileDispatcher::new(files, TestProvider));
        producer
            .append_batch(&[
                BaseTaskInput {
                    planned: PlannedScannedPath {
                        scanned: scanned(r"C:\media-success.bin", 10),
                        lane: lane(7),
                    },
                    cached: Some(success_cached),
                    contact_sheet_valid: true,
                    force_recompute: false,
                },
                BaseTaskInput {
                    planned: PlannedScannedPath {
                        scanned: scanned(r"C:\media-failure-2.bin", 10),
                        lane: lane(7),
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
        let task_path = pending.dispatcher.lane_path(&lane(7)).unwrap();
        let success_context = pending.contexts.get(&success_identity).unwrap().clone();
        let failure_context = pending.contexts.get(&failure_identity).unwrap().clone();
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: success_identity.clone(),
                record: success_record,
                context: success_context,
                response: completed_response(&success_identity, success_md5, image_output()),
                worker_slot: Some(4),
            }],
            file_failures: vec![TaskFileMediaFailure {
                identity: failure_identity.clone(),
                record: failure_record,
                context: failure_context,
                message: "测试另一项 Worker 失败".into(),
                worker_slot: Some(5),
            }],
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let pending = persist_task_file_media_results(
            media,
            &handle,
            &mut acknowledgements,
            TaskFileMediaPersistenceOptions::default(),
        )
        .await
        .unwrap();
        let statuses: Vec<_> = std::fs::read_to_string(&task_path)
            .unwrap()
            .lines()
            .filter(|line| !line.is_empty())
            .map(|line| line.as_bytes()[0])
            .collect();
        assert_eq!(statuses, vec![b'C', b'F']);
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
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: identity.clone(),
                record,
                context,
                response: completed_response(&identity, returned, image_output()),
                worker_slot: None,
            }],
            file_failures: Vec::new(),
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_task_file_media_results(
            media,
            &handle,
            &mut acknowledgements,
            TaskFileMediaPersistenceOptions::default(),
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
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: identity.clone(),
                record,
                context,
                response: completed_response(&identity, md5, video_output()),
                worker_slot: Some(3),
            }],
            file_failures: Vec::new(),
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let options = TaskFileMediaPersistenceOptions {
            contact_sheet_root: root.path().join("contacts"),
            ..Default::default()
        };
        let pending =
            persist_task_file_media_results(media, &handle, &mut acknowledgements, options)
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
        let media = MediaPassResult {
            pending,
            completed: vec![TaskFileMediaCompleted {
                identity: identity.clone(),
                record,
                context,
                response: completed_response(&identity, md5, video_output()),
                worker_slot: None,
            }],
            file_failures: Vec::new(),
            blocked_reason: None,
            remaining_hash_rows: 0,
            cancelled: false,
        };
        let bad_root = root.path().join("not-a-directory");
        std::fs::write(&bad_root, b"block directory creation").unwrap();
        let (actor, handle, mut acknowledgements) = BaseStoreActor::spawn(store, 2);
        let result = persist_task_file_media_results(
            media,
            &handle,
            &mut acknowledgements,
            TaskFileMediaPersistenceOptions {
                contact_sheet_root: bad_root,
                ..Default::default()
            },
        )
        .await;
        let error = match result {
            Err(error) => error,
            Ok(_) => panic!("联系表写入失败必须返回任务级错误"),
        };
        let pending = error.into_pending();
        assert_eq!(std::fs::read(task_path).unwrap()[0], b'P');
        assert_eq!(pending.contexts.len(), 1);
        drop(handle);
        actor.finish().await.unwrap();
    }
}

//! 扫描缓存短路径、内容复用、一筛提交和成功收尾事务。

use std::{
    collections::BTreeMap,
    fs,
    path::{Path, PathBuf},
    time::Duration,
};

use dedup_core::{DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath, TaskId};
use dedup_media::sample_positions;
use dedup_node_store::{
    ContentId, FeatureWrite, FileFaultKind, FileFaultRecord, ImageStage1Fields, NewTaskItem,
    NodeStore, ScannedPath, TaskItemCompletion, TaskStatus, VideoFrameStage1Fields,
    VideoMetadataFields,
};
use dedup_protocol::proto::{self, worker_envelope};
use dedup_windows::ReadCancellationToken;
use thiserror::Error;
use tokio::task::JoinSet;

use crate::{
    contact_sheet_cache::ContactSheetCacheEntry,
    io::ReadFailure,
    worker::{WorkerEvent, WorkerPool, decode_stage1_payload},
};

use super::{
    FileEnumerator, FileHasher, PipelineFileReader, PipelineLimits, ReadProduct, ScanError,
    pipeline::spawn_bounded_enumeration,
};

const LOOKUP_BATCH_SIZE: usize = 1000;

/// 用户创建扫描任务时固定的根和缓存策略。
#[derive(Clone, Debug)]
pub struct ScanOptions {
    /// 本任务持久化并限制旧路径失效范围的绝对目录。
    pub roots: Vec<DisplayPath>,
    /// 忽略路径大小缓存，重新读取 MD5 并重做媒体探测和一筛。
    pub force_recompute: bool,
}

impl ScanOptions {
    /// 创建使用普通缓存语义的扫描选项。
    pub const fn new(roots: Vec<DisplayPath>) -> Self {
        Self {
            roots,
            force_recompute: false,
        }
    }

    /// 切换为用户明确触发的强制重新计算。
    pub const fn force_recompute(mut self) -> Self {
        self.force_recompute = true;
        self
    }
}

/// 一次扫描完成后供事件、同步和界面读取的统计。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ScanSummary {
    /// 持久化扫描任务。
    pub task_id: TaskId,
    /// 枚举得到的文件数量。
    pub total_files: usize,
    /// 完全跳过文件读取的路径缓存命中数。
    pub cache_hits: usize,
    /// 实际读取并计算 MD5 的文件数。
    pub hashed: usize,
    /// 通过 MD5+大小复用已有内容的文件数。
    pub reused_contents: usize,
    /// 派发媒体探测和一筛的内容数。
    pub scheduled_stage1: usize,
    /// 默认扫描发现旧内容特征不完整而明确跳过的数量。
    pub skipped_incomplete: usize,
    /// 单文件读取或一筛失败数；不会把任务级状态改为 failed。
    pub file_failures: usize,
    /// 成功收尾事务提交后的 SQLite outbox 高水位。
    pub outbox_high_seq: u64,
}

/// 交给一筛处理器的持久任务身份和文件内容引用。
#[derive(Clone, Debug)]
pub struct Stage1Request {
    /// 扫描任务 ID。
    pub task_id: TaskId,
    /// SQLite 任务项 ID。
    pub item_id: String,
    /// dispatch 时冻结的物理机器身份。
    pub machine_id: MachineId,
    /// 与持久任务项相同的规范路径。
    pub normalized_path: NormalizedPath,
    /// 真实文件访问路径。
    pub display_path: DisplayPath,
    /// 与持久任务项相同的文件大小。
    pub file_size: u64,
    /// 当前 Worker 处理阶段。
    pub stage: String,
    /// 本机内容行。
    pub content_id: ContentId,
    /// 当前 MD5 目标不存在时要求 Worker 生成联系表。
    pub generate_contact_sheet: bool,
}

impl Stage1Request {
    fn worker_file_identity(&self) -> crate::worker::WorkerFileIdentity {
        crate::worker::WorkerFileIdentity {
            machine_id: self.machine_id.clone(),
            normalized_path: self.normalized_path.clone(),
            display_path: self.display_path.clone(),
            file_size: self.file_size,
            stage: self.stage.clone(),
        }
    }
}

/// 一条可乱序返回但仍以 item ID 归并的一筛结果。
pub struct Stage1BatchResult {
    /// 原持久任务项 ID。
    pub item_id: String,
    /// 该项的 Worker 输出或文件级错误。
    pub output: Result<crate::worker::Stage1Output, Stage1ProcessError>,
}

/// 一筛批次保留 Worker 崩溃、取消、基础设施和普通处理错误的不同终态。
#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum Stage1ProcessError {
    /// 文件被 Worker 正常处理但返回业务失败。
    #[error("{0}")]
    Processing(String),
    /// Worker 进程在当前文件运行期间意外退出。
    #[error("{message}")]
    WorkerCrash {
        /// dispatch 时冻结并由真实 Crashed event 返回的文件身份。
        identity: crate::worker::WorkerFileIdentity,
        /// Worker 进程/管道的非持久诊断文案。
        message: String,
    },
    /// 当前任务或任务项已取消。
    #[error("一筛已取消")]
    Cancelled,
    /// WorkerPool 或进程补建等基础设施失败。
    #[error("{0}")]
    Infrastructure(String),
}

/// 扫描引擎调用的媒体探测与一筛计算边界。
#[allow(async_fn_in_trait)]
pub trait Stage1Processor {
    /// 对新内容或明确强制重算内容执行一次拥有所有权的计算。
    async fn process(
        &mut self,
        request: Stage1Request,
    ) -> Result<crate::worker::Stage1Output, String>;

    /// 当前处理器可以同时保持的不同任务项数量。
    fn max_in_flight(&self) -> usize {
        1
    }

    /// 批量派发并允许按任意完成顺序返回；默认保持旧处理器的串行兼容。
    async fn process_batch(&mut self, requests: Vec<Stage1Request>) -> Vec<Stage1BatchResult> {
        let mut results = Vec::with_capacity(requests.len());
        for request in requests {
            let item_id = request.item_id.clone();
            results.push(Stage1BatchResult {
                item_id,
                output: self
                    .process(request)
                    .await
                    .map_err(Stage1ProcessError::Processing),
            });
        }
        results
    }
}

/// 校验根目录并在 SQLite 持久化真实扫描任务，计算可随后在独立 owner 中继续。
pub fn begin_scan_task(
    store: &mut NodeStore,
    options: &ScanOptions,
    now_ms: i64,
) -> Result<TaskId, ScanError> {
    let roots = options
        .roots
        .iter()
        .map(|root| NormalizedPath::new(root.as_path()))
        .collect::<Result<Vec<_>, _>>()
        .map_err(|error| ScanError::InvalidResult(error.to_string()))?;
    store.create_scan_task(&roots, now_ms).map_err(Into::into)
}

/// 串行借用真实 WorkerPool 完成一次扫描一筛请求的适配器。
pub struct WorkerPoolStage1Processor<'a> {
    pool: &'a mut WorkerPool,
    capacity: usize,
    cancellation: ReadCancellationToken,
}

impl<'a> WorkerPoolStage1Processor<'a> {
    /// 借用由 NodeEngine actor 独占的 WorkerPool。
    pub fn new(pool: &'a mut WorkerPool, cancellation: ReadCancellationToken) -> Self {
        let capacity = pool.worker_process_ids().len().max(1);
        Self {
            pool,
            capacity,
            cancellation,
        }
    }
}

impl Stage1Processor for WorkerPoolStage1Processor<'_> {
    async fn process(
        &mut self,
        request: Stage1Request,
    ) -> Result<crate::worker::Stage1Output, String> {
        let task_id = request.task_id.as_uuid().to_string();
        let item_id = request.item_id.clone();
        let file_identity = request.worker_file_identity();
        self.pool
            .dispatch_scan(
                proto::WorkerEnvelope {
                    payload: Some(worker_envelope::Payload::ProbeAndStage1(
                        proto::ProbeAndStage1 {
                            task_id: task_id.clone(),
                            item_id: item_id.clone(),
                            display_path: request
                                .display_path
                                .as_path()
                                .to_string_lossy()
                                .into_owned(),
                            media_kind: proto::MediaKind::MediaOther as i32,
                            generate_contact_sheet: request.generate_contact_sheet,
                        },
                    )),
                },
                self.cancellation.clone(),
                true,
                file_identity,
            )
            .await
            .map_err(|error| error.to_string())?;
        match self.pool.next_event().await {
            Some(WorkerEvent::Completed {
                task_id: event_task,
                item_id: event_item,
                response,
            }) if event_task == task_id && event_item == item_id => match response.payload {
                Some(worker_envelope::Payload::Stage1Result(result)) => {
                    decode_stage1_payload(&result.payload).map_err(|error| error.to_string())
                }
                Some(worker_envelope::Payload::WorkerFailure(failure)) => Err(failure.message),
                _ => Err("Worker 返回了非一筛响应".into()),
            },
            Some(WorkerEvent::Crashed {
                task_id: event_task,
                item_id: event_item,
                message,
                ..
            }) if event_task == task_id && event_item == item_id => Err(message),
            Some(WorkerEvent::Cancelled {
                task_id: event_task,
                item_id: event_item,
            }) if event_task == task_id && event_item == item_id => Err("一筛已取消".into()),
            Some(WorkerEvent::InfrastructureFailure { message }) => Err(message),
            Some(_) => Err("WorkerPool 在串行扫描中返回了其他任务事件".into()),
            None => Err("WorkerPool 已关闭".into()),
        }
    }

    fn max_in_flight(&self) -> usize {
        self.capacity
    }

    async fn process_batch(&mut self, requests: Vec<Stage1Request>) -> Vec<Stage1BatchResult> {
        let mut pending = BTreeMap::new();
        let mut results = Vec::with_capacity(requests.len());
        for request in requests {
            let task_id = request.task_id.as_uuid().to_string();
            let item_id = request.item_id.clone();
            let file_identity = request.worker_file_identity();
            let envelope = proto::WorkerEnvelope {
                payload: Some(worker_envelope::Payload::ProbeAndStage1(
                    proto::ProbeAndStage1 {
                        task_id: task_id.clone(),
                        item_id: item_id.clone(),
                        display_path: request
                            .display_path
                            .as_path()
                            .to_string_lossy()
                            .into_owned(),
                        media_kind: proto::MediaKind::MediaOther as i32,
                        generate_contact_sheet: request.generate_contact_sheet,
                    },
                )),
            };
            match self
                .pool
                .dispatch_scan(envelope, self.cancellation.clone(), true, file_identity)
                .await
            {
                Ok(()) => {
                    pending.insert(item_id, task_id);
                }
                Err(error) => results.push(Stage1BatchResult {
                    item_id,
                    output: Err(Stage1ProcessError::Infrastructure(error.to_string())),
                }),
            }
        }
        while !pending.is_empty() {
            let Some(event) = self.pool.next_event().await else {
                for item_id in pending.into_keys() {
                    results.push(Stage1BatchResult {
                        item_id,
                        output: Err(Stage1ProcessError::Infrastructure(
                            "WorkerPool 已关闭".into(),
                        )),
                    });
                }
                break;
            };
            match event {
                WorkerEvent::Completed {
                    task_id,
                    item_id,
                    response,
                } if pending.get(&item_id) == Some(&task_id) => {
                    pending.remove(&item_id);
                    let output = match response.payload {
                        Some(worker_envelope::Payload::Stage1Result(result)) => {
                            decode_stage1_payload(&result.payload)
                                .map_err(|error| Stage1ProcessError::Processing(error.to_string()))
                        }
                        Some(worker_envelope::Payload::WorkerFailure(failure)) => {
                            Err(Stage1ProcessError::Processing(failure.message))
                        }
                        _ => Err(Stage1ProcessError::Infrastructure(
                            "Worker 返回了非一筛响应".into(),
                        )),
                    };
                    results.push(Stage1BatchResult { item_id, output });
                }
                WorkerEvent::Crashed {
                    task_id,
                    item_id,
                    identity,
                    message,
                } if pending.get(&item_id) == Some(&task_id) => {
                    pending.remove(&item_id);
                    results.push(Stage1BatchResult {
                        item_id,
                        output: Err(Stage1ProcessError::WorkerCrash { identity, message }),
                    });
                }
                WorkerEvent::Cancelled { task_id, item_id }
                    if pending.get(&item_id) == Some(&task_id) =>
                {
                    pending.remove(&item_id);
                    results.push(Stage1BatchResult {
                        item_id,
                        output: Err(Stage1ProcessError::Cancelled),
                    });
                }
                WorkerEvent::InfrastructureFailure { message } => {
                    for item_id in std::mem::take(&mut pending).into_keys() {
                        results.push(Stage1BatchResult {
                            item_id,
                            output: Err(Stage1ProcessError::Infrastructure(message.clone())),
                        });
                    }
                }
                _ => {
                    for item_id in std::mem::take(&mut pending).into_keys() {
                        results.push(Stage1BatchResult {
                            item_id,
                            output: Err(Stage1ProcessError::Infrastructure(
                                "WorkerPool 返回了不属于当前扫描批次的事件".into(),
                            )),
                        });
                    }
                }
            }
        }
        results
    }
}

/// 组合一个枚举器、一个可计数哈希实现和联系表缓存目录的扫描引擎。
pub struct ScanEngine<E, H> {
    enumerator: E,
    hasher: H,
    contact_sheet_root: PathBuf,
}

impl<E, H> ScanEngine<E, H>
where
    E: FileEnumerator,
    H: FileHasher,
{
    /// 装配扫描引擎；缓存目录只写视频 JPG 联系表。
    pub fn new(enumerator: E, hasher: H, contact_sheet_root: impl Into<PathBuf>) -> Self {
        Self {
            enumerator,
            hasher,
            contact_sheet_root: contact_sheet_root.into(),
        }
    }

    /// 返回哈希实现，主要用于确认缓存路径没有发生文件读取。
    pub const fn hasher(&self) -> &H {
        &self.hasher
    }

    /// 完成枚举、1000 条批量缓存查询、必要 MD5、一筛和成功失效事务。
    pub async fn run<P>(
        &mut self,
        store: &mut NodeStore,
        options: ScanOptions,
        processor: &mut P,
        now_ms: i64,
    ) -> Result<ScanSummary, ScanError>
    where
        P: Stage1Processor,
    {
        let task_id = begin_scan_task(store, &options, now_ms)?;
        self.run_existing(store, task_id, options, processor, now_ms)
            .await
    }

    /// 使用有界枚举、并行读取、批量 Worker 和当前任务唯一 SQLite writer 执行扫描。
    pub async fn run_parallel_with<R, P>(
        &mut self,
        store: &mut NodeStore,
        options: ScanOptions,
        reader: R,
        processor: &mut P,
        limits: PipelineLimits,
        cancellation: ReadCancellationToken,
        now_ms: i64,
    ) -> Result<ScanSummary, ScanError>
    where
        E: Clone + Send + 'static,
        R: PipelineFileReader,
        P: Stage1Processor,
    {
        let task_id = begin_scan_task(store, &options, now_ms)?;
        self.run_existing_parallel_with(
            store,
            task_id,
            options,
            reader,
            processor,
            limits,
            cancellation,
            now_ms,
        )
        .await
    }

    /// 从已持久化扫描任务继续运行同一有界流水线。
    #[allow(clippy::too_many_arguments)]
    pub async fn run_existing_parallel_with<R, P>(
        &mut self,
        store: &mut NodeStore,
        task_id: TaskId,
        options: ScanOptions,
        reader: R,
        processor: &mut P,
        limits: PipelineLimits,
        cancellation: ReadCancellationToken,
        now_ms: i64,
    ) -> Result<ScanSummary, ScanError>
    where
        E: Clone + Send + 'static,
        R: PipelineFileReader,
        P: Stage1Processor,
    {
        if limits.channel_capacity() == 0 || limits.max_read_tasks() == 0 {
            return Err(ScanError::Stage1("扫描管道容量必须大于零".into()));
        }
        let (mut enumerated, enumeration_task) = spawn_bounded_enumeration(
            self.enumerator.clone(),
            options.roots.clone(),
            limits.channel_capacity(),
            cancellation.clone(),
        );
        let mut reads = JoinSet::new();
        let mut enumeration_closed = false;
        let mut pending_stage1 = Vec::new();
        let mut summary = ScanSummary {
            task_id,
            total_files: 0,
            cache_hits: 0,
            hashed: 0,
            reused_contents: 0,
            scheduled_stage1: 0,
            skipped_incomplete: 0,
            file_failures: 0,
            outbox_high_seq: 0,
        };
        loop {
            if cancellation.is_cancelled() {
                drop(enumerated);
                drain_parallel_reads(&mut reads).await;
                let _ = enumeration_task.await;
                return Err(ScanError::Cancelled);
            }
            while reads.len() < limits.max_read_tasks() {
                match enumerated.try_recv() {
                    Ok(scanned) => self.accept_parallel_row(
                        store,
                        task_id,
                        &options,
                        &reader,
                        &cancellation,
                        &mut reads,
                        &mut summary,
                        scanned,
                        now_ms,
                    )?,
                    Err(tokio::sync::mpsc::error::TryRecvError::Empty) => break,
                    Err(tokio::sync::mpsc::error::TryRecvError::Disconnected) => {
                        enumeration_closed = true;
                        break;
                    }
                }
            }
            if pending_stage1.len() >= processor.max_in_flight().max(1) {
                let result = flush_stage1_batch(
                    store,
                    processor,
                    &cancellation,
                    &mut pending_stage1,
                    &mut summary,
                    now_ms,
                )
                .await;
                if matches!(result, Err(ScanError::Cancelled)) {
                    drop(enumerated);
                    drain_parallel_reads(&mut reads).await;
                    let _ = enumeration_task.await;
                    return Err(ScanError::Cancelled);
                }
                result?;
            }
            if enumeration_closed && reads.is_empty() {
                break;
            }
            if reads.len() >= limits.max_read_tasks() || enumeration_closed {
                let joined = reads
                    .join_next()
                    .await
                    .ok_or_else(|| ScanError::Stage1("读取任务意外为空".into()))?;
                let result = self.accept_parallel_read(
                    store,
                    task_id,
                    &options,
                    &mut pending_stage1,
                    &mut summary,
                    joined.map_err(|error| ScanError::Stage1(error.to_string()))?,
                    now_ms,
                );
                if matches!(result, Err(ScanError::Cancelled)) {
                    drop(enumerated);
                    drain_parallel_reads(&mut reads).await;
                    let _ = enumeration_task.await;
                    return Err(ScanError::Cancelled);
                }
                result?;
                continue;
            }
            let step = tokio::select! {
                row = enumerated.recv() => match row {
                    Some(scanned) => self.accept_parallel_row(
                        store, task_id, &options, &reader, &cancellation, &mut reads,
                        &mut summary, scanned, now_ms,
                    ),
                    None => {
                        enumeration_closed = true;
                        Ok(())
                    },
                },
                joined = reads.join_next(), if !reads.is_empty() => {
                    match joined.expect("分支保证存在") {
                        Ok(value) => self.accept_parallel_read(
                            store, task_id, &options, &mut pending_stage1, &mut summary,
                            value, now_ms,
                        ),
                        Err(error) => Err(ScanError::Stage1(error.to_string())),
                    }
                }
            };
            if matches!(step, Err(ScanError::Cancelled)) {
                drop(enumerated);
                drain_parallel_reads(&mut reads).await;
                let _ = enumeration_task.await;
                return Err(ScanError::Cancelled);
            }
            step?;
        }
        flush_stage1_batch(
            store,
            processor,
            &cancellation,
            &mut pending_stage1,
            &mut summary,
            now_ms,
        )
        .await?;
        match enumeration_task.await {
            Ok(Ok(())) => {}
            Ok(Err(ScanError::Cancelled)) => return Err(ScanError::Cancelled),
            Ok(Err(error)) => {
                store.fail_task(task_id, now_ms)?;
                return Err(error);
            }
            Err(error) => {
                store.fail_task(task_id, now_ms)?;
                return Err(ScanError::Stage1(error.to_string()));
            }
        }
        if cancellation.is_cancelled()
            || store.task_snapshot(task_id)?.status == TaskStatus::Cancelled
        {
            return Err(ScanError::Cancelled);
        }
        summary.outbox_high_seq = store.finalize_scan_task_from_items(task_id, now_ms)?;
        Ok(summary)
    }

    /// 从已持久化的真实任务继续枚举、哈希、一筛和成功收尾。
    pub async fn run_existing<P>(
        &mut self,
        store: &mut NodeStore,
        task_id: TaskId,
        options: ScanOptions,
        processor: &mut P,
        now_ms: i64,
    ) -> Result<ScanSummary, ScanError>
    where
        P: Stage1Processor,
    {
        let rows = match self.enumerator.enumerate(&options.roots) {
            Ok(rows) => rows,
            Err(error) => {
                store.fail_task(task_id, now_ms)?;
                return Err(error);
            }
        };
        let mut summary = ScanSummary {
            task_id,
            total_files: rows.len(),
            cache_hits: 0,
            hashed: 0,
            reused_contents: 0,
            scheduled_stage1: 0,
            skipped_incomplete: 0,
            file_failures: 0,
            outbox_high_seq: 0,
        };
        for batch in rows.chunks(LOOKUP_BATCH_SIZE) {
            let lookups = if options.force_recompute {
                vec![None; batch.len()]
            } else {
                store
                    .lookup_scanned_paths(batch)?
                    .into_iter()
                    .map(|lookup| lookup.content_id)
                    .collect()
            };
            for (scanned, cached_content) in batch.iter().zip(lookups) {
                if let Some(content_id) = cached_content {
                    summary.cache_hits += 1;
                    if self.complete_reused_item(store, task_id, scanned, content_id, now_ms)? {
                        summary.skipped_incomplete += 1;
                    }
                    continue;
                }
                let md5 = match self.hasher.md5(scanned.display_path.as_path()) {
                    Ok(md5) => {
                        summary.hashed += 1;
                        md5
                    }
                    Err(error) => {
                        summary.file_failures += 1;
                        complete_file_failure(
                            store,
                            task_id,
                            scanned,
                            None,
                            "md5",
                            error.to_string(),
                            now_ms,
                        )?;
                        continue;
                    }
                };
                let content = store.upsert_content_and_location(scanned, md5, MediaKind::Other)?;
                if content.reused && !options.force_recompute {
                    summary.reused_contents += 1;
                    if self.complete_reused_item(store, task_id, scanned, content.id, now_ms)? {
                        summary.skipped_incomplete += 1;
                    }
                    continue;
                }
                summary.scheduled_stage1 += 1;
                let contact_sheet =
                    ContactSheetCacheEntry::from_md5(&self.contact_sheet_root, md5);
                let succeeded = self
                    .process_stage1(
                        store,
                        task_id,
                        scanned,
                        content.id,
                        contact_sheet,
                        processor,
                        now_ms,
                    )
                    .await?;
                if !succeeded {
                    summary.file_failures += 1;
                }
            }
        }
        let seen = rows
            .iter()
            .map(|row| row.normalized_path.clone())
            .collect::<Vec<_>>();
        summary.outbox_high_seq = store.finalize_scan_task(task_id, &seen, now_ms)?;
        Ok(summary)
    }

    #[allow(clippy::too_many_arguments)]
    fn accept_parallel_row<R>(
        &self,
        store: &mut NodeStore,
        task_id: TaskId,
        options: &ScanOptions,
        reader: &R,
        cancellation: &ReadCancellationToken,
        reads: &mut JoinSet<(ReservedScan, Result<ReadProduct<R::Lease>, ReadFailure>)>,
        summary: &mut ScanSummary,
        scanned: ScannedPath,
        now_ms: i64,
    ) -> Result<(), ScanError>
    where
        R: PipelineFileReader,
    {
        let Some(item_id) = store.reserve_scan_path(task_id, &scanned, now_ms)? else {
            return Ok(());
        };
        summary.total_files += 1;
        let cached_content = if options.force_recompute {
            None
        } else {
            store.lookup_scanned_paths(std::slice::from_ref(&scanned))?[0].content_id
        };
        if let Some(content_id) = cached_content {
            summary.cache_hits += 1;
            if self.complete_reserved_reused_item(store, &item_id, content_id, now_ms)? {
                summary.skipped_incomplete += 1;
            }
            return Ok(());
        }
        let task_reader = reader.clone();
        let reserved = ReservedScan { scanned, item_id };
        let task_scanned = reserved.scanned.clone();
        let task_cancellation = cancellation.clone();
        reads.spawn(async move {
            let result = task_reader.read(task_scanned, task_cancellation).await;
            (reserved, result)
        });
        Ok(())
    }

    fn accept_parallel_read<L>(
        &self,
        store: &mut NodeStore,
        task_id: TaskId,
        options: &ScanOptions,
        pending_stage1: &mut Vec<PendingStage1<L>>,
        summary: &mut ScanSummary,
        (reserved, result): (ReservedScan, Result<ReadProduct<L>, ReadFailure>),
        now_ms: i64,
    ) -> Result<(), ScanError> {
        let ReservedScan { scanned, item_id } = reserved;
        let ReadProduct { md5, lease } = match result {
            Ok(value) => value,
            Err(ReadFailure::Cancelled) => return Err(ScanError::Cancelled),
            Err(error) => {
                summary.file_failures += 1;
                if store.task_snapshot(task_id)?.status != TaskStatus::Cancelled {
                    store.complete_item(
                        &item_id,
                        TaskItemCompletion::Failed(error.to_string()),
                        now_ms,
                    )?;
                }
                return Ok(());
            }
        };
        summary.hashed += 1;
        let content = store.upsert_content_and_location(&scanned, md5, MediaKind::Other)?;
        if content.reused && !options.force_recompute {
            summary.reused_contents += 1;
            if self.complete_reserved_reused_item(store, &item_id, content.id, now_ms)? {
                summary.skipped_incomplete += 1;
            }
            drop(lease);
            return Ok(());
        }
        summary.scheduled_stage1 += 1;
        let contact_sheet = ContactSheetCacheEntry::from_md5(&self.contact_sheet_root, md5);
        store.set_running_item_content_and_stage(&item_id, content.id, "probe_stage1")?;
        let machine_id = store.machine_id().clone();
        let normalized_path = scanned.normalized_path.clone();
        let display_path = scanned.display_path.clone();
        let file_size = scanned.file_size;
        pending_stage1.push(PendingStage1 {
            request: Stage1Request {
                task_id,
                item_id,
                machine_id,
                normalized_path,
                display_path,
                file_size,
                stage: "probe_stage1".into(),
                content_id: content.id,
                generate_contact_sheet: !contact_sheet.exists(),
            },
            lease,
            contact_sheet,
        });
        Ok(())
    }

    fn complete_reserved_reused_item(
        &self,
        store: &mut NodeStore,
        item_id: &str,
        content_id: ContentId,
        now_ms: i64,
    ) -> Result<bool, ScanError> {
        let kind = store.content_media_kind(content_id)?;
        let incomplete =
            kind != MediaKind::Other && store.load_complete_stage1(content_id)?.is_none();
        store.complete_item(
            item_id,
            TaskItemCompletion::Succeeded {
                content_id: Some(content_id),
            },
            now_ms,
        )?;
        Ok(incomplete)
    }

    fn complete_reused_item(
        &self,
        store: &mut NodeStore,
        task_id: TaskId,
        scanned: &ScannedPath,
        content_id: ContentId,
        now_ms: i64,
    ) -> Result<bool, ScanError> {
        let kind = store.content_media_kind(content_id)?;
        let incomplete =
            kind != MediaKind::Other && store.load_complete_stage1(content_id)?.is_none();
        let stage = if incomplete {
            "skipped_incomplete"
        } else {
            "reused"
        };
        complete_file_success(store, task_id, scanned, content_id, stage, now_ms)?;
        Ok(incomplete)
    }

    async fn process_stage1<P: Stage1Processor>(
        &self,
        store: &mut NodeStore,
        task_id: TaskId,
        scanned: &ScannedPath,
        content_id: ContentId,
        contact_sheet: ContactSheetCacheEntry,
        processor: &mut P,
        now_ms: i64,
    ) -> Result<bool, ScanError> {
        let item_id =
            append_and_claim(store, task_id, scanned, content_id, "probe_stage1", now_ms)?;
        let request = Stage1Request {
            task_id,
            item_id: item_id.clone(),
            machine_id: store.machine_id().clone(),
            normalized_path: scanned.normalized_path.clone(),
            display_path: scanned.display_path.clone(),
            file_size: scanned.file_size,
            stage: "probe_stage1".into(),
            content_id,
            generate_contact_sheet: !contact_sheet.exists(),
        };
        match processor.process(request).await {
            Ok(output) => {
                persist_stage1(store, content_id, contact_sheet, output)?;
                store.complete_item(
                    &item_id,
                    TaskItemCompletion::Succeeded {
                        content_id: Some(content_id),
                    },
                    now_ms,
                )?;
            }
            Err(error) => {
                store.complete_item(&item_id, TaskItemCompletion::Failed(error), now_ms)?;
                return Ok(false);
            }
        }
        Ok(true)
    }
}

struct ReservedScan {
    scanned: ScannedPath,
    item_id: String,
}

async fn drain_parallel_reads<L>(
    reads: &mut JoinSet<(ReservedScan, Result<ReadProduct<L>, ReadFailure>)>,
) where
    L: Send + 'static,
{
    while reads.join_next().await.is_some() {}
}

struct PendingStage1<L> {
    request: Stage1Request,
    lease: L,
    contact_sheet: ContactSheetCacheEntry,
}

async fn flush_stage1_batch<P, L>(
    store: &mut NodeStore,
    processor: &mut P,
    cancellation: &ReadCancellationToken,
    pending: &mut Vec<PendingStage1<L>>,
    summary: &mut ScanSummary,
    now_ms: i64,
) -> Result<(), ScanError>
where
    P: Stage1Processor,
{
    if pending.is_empty() {
        return Ok(());
    }
    let works = std::mem::take(pending);
    let mut dispatchable = Vec::with_capacity(works.len());
    for work in works {
        if cancellation.is_cancelled() {
            drop(work);
            return Err(ScanError::Cancelled);
        }
        if !matches!(
            store.task_snapshot(work.request.task_id)?.status,
            TaskStatus::Queued | TaskStatus::Running
        ) {
            drop(work);
            continue;
        }
        dispatchable.push(work);
    }
    let works = dispatchable;
    if works.is_empty() {
        return Ok(());
    }
    let requests = works
        .iter()
        .map(|work| work.request.clone())
        .collect::<Vec<_>>();
    let mut results = processor
        .process_batch(requests)
        .await
        .into_iter()
        .map(|result| (result.item_id, result.output))
        .collect::<BTreeMap<_, _>>();
    for PendingStage1 {
        request,
        lease,
        contact_sheet,
    } in works
    {
        if cancellation.is_cancelled()
            || store.task_snapshot(request.task_id)?.status == TaskStatus::Cancelled
        {
            drop(lease);
            continue;
        }
        let output = results.remove(&request.item_id).unwrap_or_else(|| {
            Err(Stage1ProcessError::Infrastructure(
                "Worker 批次缺少对应 item 结果".into(),
            ))
        });
        match output {
            Ok(output) => {
                let prepared = prepare_stage1_writes(
                    &request.item_id,
                    &contact_sheet,
                    request.generate_contact_sheet,
                    output,
                )?;
                let committed = match store.commit_scan_stage1_if_running(
                    &request.item_id,
                    request.content_id,
                    prepared.media_kind,
                    prepared.writes,
                    now_ms,
                ) {
                    Ok(committed) => committed,
                    Err(error) => {
                        if let Some(contact) = prepared.contact {
                            contact.remove_partial();
                        }
                        return Err(error.into());
                    }
                };
                if !committed {
                    if let Some(contact) = prepared.contact {
                        contact.remove_partial();
                    }
                    drop(lease);
                    continue;
                }
                if let Some(contact) = prepared.contact {
                    commit_contact_sheet(store, request.content_id, contact)?;
                }
            }
            Err(Stage1ProcessError::WorkerCrash { identity, message }) => {
                if store.task_snapshot(request.task_id)?.status != TaskStatus::Cancelled {
                    let fault = FileFaultRecord {
                        machine_id: identity.machine_id,
                        normalized_path: identity.normalized_path,
                        display_path: identity.display_path,
                        file_size: identity.file_size,
                        kind: FileFaultKind::WorkerCrash,
                        stage: identity.stage,
                        windows_error_code: None,
                        message: message.clone(),
                    };
                    store.fail_running_item_with_file_fault(
                        &request.item_id,
                        &fault,
                        &message,
                        now_ms,
                    )?;
                    summary.file_failures += 1;
                }
            }
            Err(error) => {
                if store.task_snapshot(request.task_id)?.status != TaskStatus::Cancelled {
                    store.complete_item(
                        &request.item_id,
                        TaskItemCompletion::Failed(error.to_string()),
                        now_ms,
                    )?;
                    summary.file_failures += 1;
                }
            }
        }
        drop(lease);
    }
    Ok(())
}

fn append_and_claim(
    store: &mut NodeStore,
    task_id: TaskId,
    scanned: &ScannedPath,
    content_id: ContentId,
    stage: &str,
    now_ms: i64,
) -> Result<String, ScanError> {
    let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path.clone());
    let expected = store.append_task_item(
        task_id,
        &NewTaskItem::for_content(
            location,
            scanned.display_path.clone(),
            scanned.file_size,
            content_id,
            stage,
        ),
        now_ms,
    )?;
    let claimed = store
        .claim_next_item(task_id, now_ms)?
        .ok_or_else(|| ScanError::Stage1("刚追加的任务项无法领取".into()))?;
    if claimed.item_id != expected {
        return Err(ScanError::Stage1("扫描任务项领取顺序不一致".into()));
    }
    Ok(expected)
}

fn complete_file_success(
    store: &mut NodeStore,
    task_id: TaskId,
    scanned: &ScannedPath,
    content_id: ContentId,
    stage: &str,
    now_ms: i64,
) -> Result<(), ScanError> {
    let item_id = append_and_claim(store, task_id, scanned, content_id, stage, now_ms)?;
    store.complete_item(
        &item_id,
        TaskItemCompletion::Succeeded {
            content_id: Some(content_id),
        },
        now_ms,
    )?;
    Ok(())
}

fn complete_file_failure(
    store: &mut NodeStore,
    task_id: TaskId,
    scanned: &ScannedPath,
    content_id: Option<ContentId>,
    stage: &str,
    error: String,
    now_ms: i64,
) -> Result<(), ScanError> {
    let location = LocationKey::new(store.machine_id().clone(), scanned.normalized_path.clone());
    let item = NewTaskItem {
        location: Some(location),
        display_path: Some(scanned.display_path.clone()),
        file_size: Some(scanned.file_size),
        content_id,
        stage: stage.into(),
    };
    let expected = store.append_task_item(task_id, &item, now_ms)?;
    let claimed = store
        .claim_next_item(task_id, now_ms)?
        .ok_or_else(|| ScanError::Stage1("失败任务项无法领取".into()))?;
    if claimed.item_id != expected {
        return Err(ScanError::Stage1("扫描任务项领取顺序不一致".into()));
    }
    store.complete_item(&expected, TaskItemCompletion::Failed(error), now_ms)?;
    Ok(())
}

fn persist_stage1(
    store: &mut NodeStore,
    content_id: ContentId,
    contact_sheet: ContactSheetCacheEntry,
    output: crate::worker::Stage1Output,
) -> Result<(), ScanError> {
    let prepared = prepare_stage1_writes(
        &format!("legacy-{}", content_id.as_i64()),
        &contact_sheet,
        !contact_sheet.exists(),
        output,
    )?;
    store.set_content_media_kind(content_id, prepared.media_kind)?;
    for write in prepared.writes {
        store.commit_feature_result(content_id, None, write)?;
    }
    if let Some(contact) = prepared.contact {
        commit_contact_sheet(store, content_id, contact)?;
    }
    Ok(())
}

struct PreparedStage1 {
    media_kind: MediaKind,
    writes: Vec<FeatureWrite>,
    contact: Option<PendingContactSheet>,
}

struct PendingContactSheet {
    temp_path: Option<PathBuf>,
    final_path: PathBuf,
    relative_path: String,
}

impl PendingContactSheet {
    fn remove_partial(self) {
        if let Some(temp_path) = self.temp_path {
            let _ = fs::remove_file(temp_path);
        }
    }
}

fn prepare_stage1_writes(
    item_id: &str,
    contact_sheet: &ContactSheetCacheEntry,
    generate_contact_sheet: bool,
    output: crate::worker::Stage1Output,
) -> Result<PreparedStage1, ScanError> {
    let media_kind = output.media_kind;
    let mut writes = Vec::new();
    let mut contact = None;
    match media_kind {
        MediaKind::Other => {}
        MediaKind::Image => {
            let fields = output
                .frames
                .first()
                .and_then(|frame| frame.feature)
                .map(ImageStage1Fields::from)
                .unwrap_or_default();
            writes.push(FeatureWrite::ImageStage1(fields));
        }
        MediaKind::Video => {
            writes.push(FeatureWrite::VideoMetadata(VideoMetadataFields {
                duration_ms: output.duration_ms,
                width: Some(output.width),
                height: Some(output.height),
            }));
            let positions =
                sample_positions(Duration::from_millis(output.duration_ms.unwrap_or(0)));
            for slot in 0..6_u8 {
                let frame = output.frames.iter().find(|frame| frame.slot == slot);
                let feature = frame.and_then(|frame| frame.feature);
                writes.push(FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                    slot,
                    time_ms: positions[slot as usize].as_millis() as u64,
                    decoded: feature.is_some(),
                    width: feature.map(|value| value.width),
                    height: feature.map(|value| value.height),
                    pdq: feature.map(|value| value.pdq),
                    quality: feature.map(|value| value.quality),
                }));
            }
            if let Some(jpeg) = output.contact_sheet_jpeg {
                contact = Some(PendingContactSheet {
                    temp_path: Some(contact_sheet.write_partial(item_id, &jpeg)?),
                    final_path: contact_sheet.final_path().to_path_buf(),
                    relative_path: contact_sheet.relative_path().to_owned(),
                });
            } else if !generate_contact_sheet {
                contact = Some(PendingContactSheet {
                    temp_path: None,
                    final_path: contact_sheet.final_path().to_path_buf(),
                    relative_path: contact_sheet.relative_path().to_owned(),
                });
            }
        }
    }
    Ok(PreparedStage1 {
        media_kind,
        writes,
        contact,
    })
}

fn commit_contact_sheet(
    store: &mut NodeStore,
    content_id: ContentId,
    contact: PendingContactSheet,
) -> Result<(), ScanError> {
    let PendingContactSheet {
        temp_path,
        final_path,
        relative_path,
    } = contact;
    let commit_reference = || {
        store
            .commit_feature_result(content_id, None, FeatureWrite::ContactSheet(relative_path))
            .map(|_| ())
            .map_err(ScanError::from)
    };
    if let Some(temp_path) = temp_path {
        publish_contact_sheet(&temp_path, &final_path, commit_reference)
    } else if final_path.is_file() {
        commit_reference()
    } else {
        Err(ScanError::Stage1(format!(
            "复用的视频联系表在写回引用前已不存在: {}",
            final_path.display()
        )))
    }
}

fn publish_contact_sheet(
    temp_path: &Path,
    final_path: &Path,
    commit_reference: impl FnOnce() -> Result<(), ScanError>,
) -> Result<(), ScanError> {
    let owned_final = if final_path.exists() {
        fs::remove_file(temp_path)?;
        false
    } else {
        if let Err(error) = fs::rename(temp_path, final_path) {
            let _ = fs::remove_file(temp_path);
            return Err(error.into());
        }
        true
    };
    if let Err(error) = commit_reference() {
        if owned_final {
            if let Err(cleanup) = fs::remove_file(final_path) {
                return Err(ScanError::Stage1(format!(
                    "{error}; 联系表引用失败后的本轮文件补偿也失败: {cleanup}"
                )));
            }
        }
        return Err(error);
    }
    Ok(())
}

/// 只供直接行为测试验证联系表发布与引用失败补偿。
#[doc(hidden)]
pub fn publish_contact_sheet_for_test(
    temp_path: &Path,
    final_path: &Path,
    commit_reference: impl FnOnce() -> Result<(), ScanError>,
) -> Result<(), ScanError> {
    publish_contact_sheet(temp_path, final_path, commit_reference)
}

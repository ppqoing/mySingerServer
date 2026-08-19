//! 扫描缓存短路径、内容复用、一筛提交和成功收尾事务。

use std::{fs, path::PathBuf, time::Duration};

use dedup_core::{DisplayPath, LocationKey, MediaKind, NormalizedPath, TaskId};
use dedup_media::sample_positions;
use dedup_node_store::{
    ContentId, FeatureWrite, ImageStage1Fields, NewTaskItem, NodeStore, ScannedPath,
    TaskItemCompletion, VideoFrameStage1Fields, VideoMetadataFields,
};
use dedup_protocol::proto::{self, worker_envelope};

use crate::worker::{WorkerEvent, WorkerPool, decode_stage1_payload};

use super::{FileEnumerator, FileHasher, ScanError};

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
    /// 真实文件访问路径。
    pub display_path: DisplayPath,
    /// 本机内容行。
    pub content_id: ContentId,
}

/// 扫描引擎调用的媒体探测与一筛计算边界。
#[allow(async_fn_in_trait)]
pub trait Stage1Processor {
    /// 对新内容或明确强制重算内容执行一次拥有所有权的计算。
    async fn process(
        &mut self,
        request: Stage1Request,
    ) -> Result<crate::worker::Stage1Output, String>;
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
}

impl<'a> WorkerPoolStage1Processor<'a> {
    /// 借用由 NodeEngine actor 独占的 WorkerPool。
    pub const fn new(pool: &'a mut WorkerPool) -> Self {
        Self { pool }
    }
}

impl Stage1Processor for WorkerPoolStage1Processor<'_> {
    async fn process(
        &mut self,
        request: Stage1Request,
    ) -> Result<crate::worker::Stage1Output, String> {
        let task_id = request.task_id.as_uuid().to_string();
        let item_id = request.item_id.clone();
        self.pool
            .dispatch(proto::WorkerEnvelope {
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
                    },
                )),
            })
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
                let succeeded = self
                    .process_stage1(store, task_id, scanned, content.id, processor, now_ms)
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
        processor: &mut P,
        now_ms: i64,
    ) -> Result<bool, ScanError> {
        let item_id =
            append_and_claim(store, task_id, scanned, content_id, "probe_stage1", now_ms)?;
        let request = Stage1Request {
            task_id,
            item_id: item_id.clone(),
            display_path: scanned.display_path.clone(),
            content_id,
        };
        match processor.process(request).await {
            Ok(output) => {
                persist_stage1(store, content_id, &self.contact_sheet_root, output)?;
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
    contact_sheet_root: &PathBuf,
    output: crate::worker::Stage1Output,
) -> Result<(), ScanError> {
    store.set_content_media_kind(content_id, output.media_kind)?;
    match output.media_kind {
        MediaKind::Other => {}
        MediaKind::Image => {
            let fields = output
                .frames
                .first()
                .and_then(|frame| frame.feature)
                .map(ImageStage1Fields::from)
                .unwrap_or_default();
            store.commit_feature_result(content_id, None, FeatureWrite::ImageStage1(fields))?;
        }
        MediaKind::Video => {
            store.commit_feature_result(
                content_id,
                None,
                FeatureWrite::VideoMetadata(VideoMetadataFields {
                    duration_ms: output.duration_ms,
                    width: Some(output.width),
                    height: Some(output.height),
                }),
            )?;
            let positions =
                sample_positions(Duration::from_millis(output.duration_ms.unwrap_or(0)));
            for slot in 0..6_u8 {
                let frame = output.frames.iter().find(|frame| frame.slot == slot);
                let feature = frame.and_then(|frame| frame.feature);
                store.commit_feature_result(
                    content_id,
                    None,
                    FeatureWrite::VideoFrameStage1(VideoFrameStage1Fields {
                        slot,
                        time_ms: positions[slot as usize].as_millis() as u64,
                        decoded: feature.is_some(),
                        width: feature.map(|value| value.width),
                        height: feature.map(|value| value.height),
                        pdq: feature.map(|value| value.pdq),
                        quality: feature.map(|value| value.quality),
                    }),
                )?;
            }
            if let Some(jpeg) = output.contact_sheet_jpeg {
                fs::create_dir_all(contact_sheet_root)?;
                let key = store
                    .lookup_scanned_paths(&[])
                    .map(|_| content_id.as_i64().to_string())?;
                let file_name = format!("{key}.jpg");
                fs::write(contact_sheet_root.join(&file_name), jpeg)?;
                store.commit_feature_result(
                    content_id,
                    None,
                    FeatureWrite::ContactSheet(format!("contact-sheets/{file_name}")),
                )?;
            }
        }
    }
    Ok(())
}

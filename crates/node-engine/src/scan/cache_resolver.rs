//! 有界远端基础缓存解析服务；只持有远端连接，不接触 Node SQLite。

use std::{
    collections::{BTreeMap, VecDeque},
    sync::Arc,
    time::{Duration, Instant},
};

use dedup_core::{ContentKey, MachineId, TaskId};
use dedup_node_store::{BaseCacheRecord, ScannedPath};
use tokio::{
    sync::{OwnedSemaphorePermit, Semaphore, mpsc},
    task::{JoinHandle, JoinSet},
};

use crate::{RemoteCacheError, RemoteFeatureCache};

/// 单次 PostgreSQL path/content 查询的产品级项目上限。
pub(super) const MAX_CACHE_BATCH_ITEMS: usize = 1_000;
/// path lane 允许两批同时远端查询，使第二批 gate 不阻塞首批结果。
pub(super) const PATH_REMOTE_SLOTS: usize = 2;
/// content lane 的产品级远端并发硬上限。
pub(super) const MAX_CONTENT_REMOTE_SLOTS: usize = 64;

/// resolver 与 actor 共同使用的稳定任务项身份。
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(super) struct CacheContextKey {
    /// 当前基础计算任务身份。
    pub(super) task_id: TaskId,
    /// SQLite 持久任务项身份。
    pub(super) item_id: String,
}

/// path 远端查询的单项输入。
#[derive(Clone, Debug)]
pub(super) struct PathResolveItem {
    /// 乱序归并使用的任务项身份。
    pub(super) key: CacheContextKey,
    /// 枚举时冻结的路径和文件大小。
    pub(super) scanned: ScannedPath,
}

/// content 远端查询的单项输入。
#[derive(Clone, Debug)]
pub(super) struct ContentResolveItem {
    /// 乱序归并使用的任务项身份。
    pub(super) key: CacheContextKey,
    /// Node 已计算的 MD5 与文件大小内容键。
    pub(super) content_key: ContentKey,
}

/// actor 发送给独占 remote resolver 的两类有界请求。
#[derive(Clone, Debug)]
pub(super) enum CacheResolveRequest {
    /// 按机器、路径和大小查询 Hash 前快速缓存。
    Paths {
        /// 单调递增请求身份。
        request_id: u64,
        /// 请求进入 actor 有界 lane 前的单调时钟起点。
        enqueued_at: Instant,
        /// 路径缓存所属物理机器。
        machine_id: MachineId,
        /// 本批最多 1000 个任务项。
        items: Vec<PathResolveItem>,
    },
    /// 按 MD5 与大小查询 Hash 后内容缓存。
    Contents {
        /// 单调递增请求身份。
        request_id: u64,
        /// 请求进入 actor 有界 lane 前的单调时钟起点。
        enqueued_at: Instant,
        /// 本批最多 1000 个任务项。
        items: Vec<ContentResolveItem>,
        /// 与项目数相等的完成容量；结果被 actor 消费前不会释放。
        completion_credits: Arc<OwnedSemaphorePermit>,
    },
}

impl CacheResolveRequest {
    /// 返回 actor 分配的请求身份。
    const fn request_id(&self) -> u64 {
        match self {
            Self::Paths { request_id, .. } | Self::Contents { request_id, .. } => *request_id,
        }
    }

    /// 返回本批项目数。
    fn len(&self) -> usize {
        match self {
            Self::Paths { items, .. } => items.len(),
            Self::Contents { items, .. } => items.len(),
        }
    }

    /// 返回请求开始等待 resolver lane 的真实单调时钟起点。
    fn enqueued_at(&self) -> Instant {
        match self {
            Self::Paths { enqueued_at, .. } | Self::Contents { enqueued_at, .. } => *enqueued_at,
        }
    }
}

/// 一项远端候选；SQLite 排名、导入和提交仍由 actor 完成。
#[derive(Debug)]
pub(super) struct CacheResolvedItem {
    /// 原请求中的稳定任务项身份。
    pub(super) key: CacheContextKey,
    /// PostgreSQL 候选；`None` 表示未命中或 local-only。
    pub(super) remote: Option<BaseCacheRecord>,
}

/// path/content 远端查询的类型化结果。
#[derive(Debug)]
pub(super) enum CacheResolutionKind {
    /// path 查询结果，顺序已重新绑定到任务项身份。
    Paths(Vec<CacheResolvedItem>),
    /// content 查询结果，顺序已重新绑定到任务项身份。
    Contents(VecDeque<CacheResolvedItem>),
}

/// resolver 返回 actor 的有界结果消息。
#[derive(Debug)]
pub(super) struct CacheResolution {
    /// 与请求上下文精确对应的请求身份。
    pub(super) request_id: u64,
    /// 第一次远端降级时携带的一次性告警。
    pub(super) warning: Option<String>,
    /// 请求从进入有界 lane 到远端 future 真正开始执行的等待时间。
    pub(super) queue_wait: Option<Duration>,
    /// 真实远端查询 future 的运行时间；local-only 回退没有该值。
    pub(super) query_elapsed: Option<Duration>,
    /// 不含任何 SQLite 状态的远端候选。
    pub(super) kind: CacheResolutionKind,
    /// content 结果占用的完成容量；path 结果不需要此许可。
    completion_credits: Option<Arc<OwnedSemaphorePermit>>,
}

impl CacheResolution {
    /// 返回本结果显式预留的 content 完成项目数。
    pub(super) fn completion_credit_count(&self) -> usize {
        self.completion_credits
            .as_ref()
            .map_or(0, |credits| credits.num_permits())
    }
}

/// resolver 正常关闭后归还的远端连接和可用状态。
pub(super) struct CacheResolverExit<R> {
    /// 已确认没有在途查询引用的远端连接。
    pub(super) remote: R,
    /// 远端在本任务剩余生命周期是否仍可发布 outbox。
    pub(super) remote_available: bool,
}

/// actor 使用的双输入 lane、单结果 lane 与 resolver 生命周期句柄。
pub(super) struct CacheResolverHandle<R> {
    /// path 请求专用有界通道，避免占满 content lane。
    pub(super) path_requests: mpsc::Sender<CacheResolveRequest>,
    /// content 请求专用有界通道，避免被 path gate 阻塞。
    pub(super) content_requests: mpsc::Sender<CacheResolveRequest>,
    /// path/content 共用的有界结果通道。
    pub(super) resolutions: mpsc::Receiver<CacheResolution>,
    /// actor 非阻塞预留多项 content 结果容量的产品级许可池。
    pub(super) content_credits: Arc<Semaphore>,
    /// endpoint 关闭后归还 remote 的 resolver 任务。
    pub(super) task: JoinHandle<CacheResolverExit<R>>,
}

/// 启动独占 remote 的 resolver；输入容量和 content 并发均由调用方显式限制。
pub(super) fn spawn_cache_resolver<R: RemoteFeatureCache>(
    remote: R,
    remote_available: bool,
    channel_capacity: usize,
    content_slots: usize,
) -> CacheResolverHandle<R> {
    assert!(channel_capacity > 0, "缓存解析通道容量必须大于 0");
    assert!(content_slots > 0, "content 远端槽位必须大于 0");
    assert!(
        content_slots <= MAX_CONTENT_REMOTE_SLOTS,
        "content 远端槽位超过产品硬上限"
    );
    let (path_requests, path_rx) = mpsc::channel(PATH_REMOTE_SLOTS);
    let (content_requests, content_rx) = mpsc::channel(channel_capacity);
    let (resolution_tx, resolutions) = mpsc::channel(channel_capacity);
    let content_credits = Arc::new(Semaphore::new(channel_capacity));
    let task = tokio::spawn(run_cache_resolver(
        remote,
        remote_available,
        channel_capacity,
        content_slots,
        path_rx,
        content_rx,
        resolution_tx,
    ));
    CacheResolverHandle {
        path_requests,
        content_requests,
        resolutions,
        content_credits,
        task,
    }
}

/// 单个远端 future 的原请求和未解释响应。
struct RemoteTaskOutput {
    request: CacheResolveRequest,
    result: Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError>,
    /// 请求在 resolver lane 的真实等待时间。
    queue_wait: Duration,
    /// 远端查询 future 的真实运行时间。
    query_elapsed: Duration,
}

/// 已实际进入远端 future 但查询失败的请求及其精确时间边界。
struct RemoteTaskFailure {
    /// 原请求，用于生成保持身份的 local-only 结果。
    request: CacheResolveRequest,
    /// 锁定 local-only 的一次性告警。
    warning: String,
    /// 请求在 resolver lane 的真实等待时间。
    queue_wait: Duration,
    /// 失败远端查询 future 的真实运行时间。
    query_elapsed: Duration,
}

/// resolver 主循环：双 lane 并发查询，结果通道满时只反压本服务。
async fn run_cache_resolver<R: RemoteFeatureCache>(
    remote: R,
    mut remote_available: bool,
    content_owned_limit: usize,
    content_slots: usize,
    mut path_rx: mpsc::Receiver<CacheResolveRequest>,
    mut content_rx: mpsc::Receiver<CacheResolveRequest>,
    resolution_tx: mpsc::Sender<CacheResolution>,
) -> CacheResolverExit<R> {
    let remote = Arc::new(remote);
    let mut path_open = true;
    let mut content_open = true;
    let mut pending_paths = VecDeque::new();
    let mut pending_contents = VecDeque::new();
    let mut path_tasks = JoinSet::new();
    let mut content_tasks = JoinSet::new();
    let mut inflight = BTreeMap::<u64, CacheResolveRequest>::new();
    let mut path_results = VecDeque::new();
    let mut content_results = VecDeque::new();

    loop {
        if remote_available {
            while path_tasks.len() < PATH_REMOTE_SLOTS {
                let Some(request) = pending_paths.pop_front() else {
                    break;
                };
                spawn_remote_task(&remote, request, &mut path_tasks, &mut inflight);
            }
            while content_tasks.len() < content_slots {
                let Some(request) = pending_contents.pop_front() else {
                    break;
                };
                spawn_remote_task(&remote, request, &mut content_tasks, &mut inflight);
            }
        } else {
            while let Some(request) = pending_contents.pop_front() {
                content_results.push_back(unqueried_resolution(request, None));
            }
            while let Some(request) = pending_paths.pop_front() {
                path_results.push_back(unqueried_resolution(request, None));
            }
        }

        if !path_open
            && !content_open
            && pending_paths.is_empty()
            && pending_contents.is_empty()
            && path_tasks.is_empty()
            && content_tasks.is_empty()
            && path_results.is_empty()
            && content_results.is_empty()
        {
            break;
        }

        tokio::select! {
            biased;
            _ = resolution_tx.closed() => break,
            permit = resolution_tx.reserve(), if !content_results.is_empty() || !path_results.is_empty() => {
                let Ok(permit) = permit else {
                    break;
                };
                let resolution = content_results
                    .pop_front()
                    .or_else(|| path_results.pop_front())
                    .expect("结果分支只会在队列非空时启用");
                permit.send(resolution);
            }
            joined = content_tasks.join_next(), if !content_tasks.is_empty() => {
                handle_joined(
                    joined,
                    &mut remote_available,
                    &mut pending_paths,
                    &mut pending_contents,
                    &mut path_tasks,
                    &mut content_tasks,
                    &mut inflight,
                    &mut path_results,
                    &mut content_results,
                );
            }
            joined = path_tasks.join_next(), if !path_tasks.is_empty() => {
                handle_joined(
                    joined,
                    &mut remote_available,
                    &mut pending_paths,
                    &mut pending_contents,
                    &mut path_tasks,
                    &mut content_tasks,
                    &mut inflight,
                    &mut path_results,
                    &mut content_results,
                );
            }
            request = content_rx.recv(), if content_open && lane_owned(
                &pending_contents,
                &content_tasks,
                &content_results,
            ) < content_owned_limit => {
                match request {
                    Some(request) => pending_contents.push_back(request),
                    None => content_open = false,
                }
            }
            request = path_rx.recv(), if path_open && lane_owned(
                &pending_paths,
                &path_tasks,
                &path_results,
            ) < PATH_REMOTE_SLOTS => {
                match request {
                    Some(request) => pending_paths.push_back(request),
                    None => path_open = false,
                }
            }
        }
    }

    path_tasks.abort_all();
    content_tasks.abort_all();
    drain_remote_tasks(&mut path_tasks, "path").await;
    drain_remote_tasks(&mut content_tasks, "content").await;
    drop(resolution_tx);
    let remote = match Arc::try_unwrap(remote) {
        Ok(remote) => remote,
        Err(remaining_remote) => panic!(
            "缓存 resolver 退出时仍有远端查询引用: strong_count={}",
            Arc::strong_count(&remaining_remote)
        ),
    };
    CacheResolverExit {
        remote,
        remote_available,
    }
}

/// Resolver 退出时检查已取消远端任务的 JoinError，并保留恰好一次查询失败记录。
async fn drain_remote_tasks(tasks: &mut JoinSet<RemoteTaskOutput>, lane: &'static str) {
    while let Some(joined) = tasks.join_next().await {
        match joined {
            Ok(output) => {
                if let Err(error) = output.result {
                    tracing::warn!(
                        event = "central_store_degraded",
                        operation = "drain_remote_cache_query",
                        fallback = "resolver_shutdown",
                        lane,
                        error = %error,
                        "缓存 resolver 收束期间远端查询失败"
                    );
                }
            }
            Err(error) if error.is_cancelled() => tracing::info!(
                event = "expected_condition",
                component = "cache_resolver",
                operation = "join_remote_query",
                reason = "resolver_shutdown",
                lane,
                error = %error,
                "缓存 resolver 退出时已取消远端查询"
            ),
            Err(error) => tracing::error!(
                event = "background_task_failed",
                component = "cache_resolver",
                task_name = "remote_cache_query",
                operation = "join",
                lane,
                error = %error,
                "远端缓存查询任务异常终止"
            ),
        }
    }
}

/// 统计 resolver 当前 lane 内部拥有的请求数；达到上限后停止接收并反压输入通道。
fn lane_owned(
    pending: &VecDeque<CacheResolveRequest>,
    tasks: &JoinSet<RemoteTaskOutput>,
    results: &VecDeque<CacheResolution>,
) -> usize {
    pending
        .len()
        .checked_add(tasks.len())
        .and_then(|owned| owned.checked_add(results.len()))
        .expect("缓存 resolver lane ownership 计数溢出")
}

/// 把一条请求放入对应 `JoinSet`，并保留取消/降级所需的上下文副本。
fn spawn_remote_task<R: RemoteFeatureCache>(
    remote: &Arc<R>,
    request: CacheResolveRequest,
    tasks: &mut JoinSet<RemoteTaskOutput>,
    inflight: &mut BTreeMap<u64, CacheResolveRequest>,
) {
    assert!(
        !request.len().eq(&0) && request.len() <= MAX_CACHE_BATCH_ITEMS,
        "缓存解析批次必须包含 1 到 1000 项"
    );
    if let CacheResolveRequest::Contents {
        items,
        completion_credits,
        ..
    } = &request
    {
        assert_eq!(
            completion_credits.num_permits(),
            items.len(),
            "content 请求必须逐项预留完成容量"
        );
    }
    let request_id = request.request_id();
    let previous = inflight.insert(request_id, request.clone());
    assert!(previous.is_none(), "缓存解析请求身份不得重复");
    let remote = Arc::clone(remote);
    tasks.spawn(async move {
        let queue_wait = request.enqueued_at().elapsed();
        let query_started = Instant::now();
        let result = match &request {
            CacheResolveRequest::Paths {
                machine_id, items, ..
            } => {
                let paths = items
                    .iter()
                    .map(|item| item.scanned.clone())
                    .collect::<Vec<_>>();
                remote.lookup_paths(machine_id, &paths).await
            }
            CacheResolveRequest::Contents { items, .. } => {
                let keys = items
                    .iter()
                    .map(|item| item.content_key)
                    .collect::<Vec<_>>();
                remote.lookup_contents(&keys).await
            }
        };
        RemoteTaskOutput {
            request,
            result,
            queue_wait,
            query_elapsed: query_started.elapsed(),
        }
    });
}

/// 归并一个远端 future；首次错误会取消其余调用并锁定 local-only。
#[allow(clippy::too_many_arguments)]
fn handle_joined(
    joined: Option<Result<RemoteTaskOutput, tokio::task::JoinError>>,
    remote_available: &mut bool,
    pending_paths: &mut VecDeque<CacheResolveRequest>,
    pending_contents: &mut VecDeque<CacheResolveRequest>,
    path_tasks: &mut JoinSet<RemoteTaskOutput>,
    content_tasks: &mut JoinSet<RemoteTaskOutput>,
    inflight: &mut BTreeMap<u64, CacheResolveRequest>,
    path_results: &mut VecDeque<CacheResolution>,
    content_results: &mut VecDeque<CacheResolution>,
) {
    let Some(joined) = joined else {
        return;
    };
    let output = match joined {
        Ok(output) => output,
        Err(error) if error.is_cancelled() && !*remote_available => {
            tracing::info!(
                event = "expected_condition",
                component = "cache_resolver",
                operation = "join_remote_query",
                reason = "sqlite_fallback_cancelled_peer_queries",
                error = %error,
                "切换 SQLite-only 后取消其余远端缓存查询"
            );
            return;
        }
        Err(error) => {
            if *remote_available {
                lock_local_only(
                    None,
                    format!("PostgreSQL 缓存查询任务异常，后续仅使用 SQLite: {error}"),
                    remote_available,
                    pending_paths,
                    pending_contents,
                    path_tasks,
                    content_tasks,
                    inflight,
                    path_results,
                    content_results,
                );
            } else {
                tracing::error!(
                    event = "background_task_failed",
                    component = "cache_resolver",
                    task_name = "remote_cache_query",
                    operation = "join_after_fallback",
                    error = %error,
                    "远端缓存查询任务在降级后异常终止"
                );
            }
            return;
        }
    };
    if inflight.remove(&output.request.request_id()).is_none() {
        return;
    }
    match checked_resolution(output) {
        Ok(resolution) => push_resolution(resolution, path_results, content_results),
        Err(failure) => {
            let warning = failure.warning.clone();
            lock_local_only(
                Some(failure),
                warning,
                remote_available,
                pending_paths,
                pending_contents,
                path_tasks,
                content_tasks,
                inflight,
                path_results,
                content_results,
            );
        }
    }
}

/// 校验远端结果长度并把顺序响应重新绑定到稳定任务项身份。
fn checked_resolution(output: RemoteTaskOutput) -> Result<CacheResolution, RemoteTaskFailure> {
    let RemoteTaskOutput {
        request,
        result,
        queue_wait,
        query_elapsed,
    } = output;
    let expected = request.len();
    let hits = match result {
        Ok(hits) if hits.len() == expected => hits,
        Ok(hits) => {
            let warning = match &request {
                CacheResolveRequest::Paths { .. } => {
                    format!(
                        "PostgreSQL 路径缓存返回数量不匹配，后续仅使用 SQLite: expected={expected}, actual={}",
                        hits.len()
                    )
                }
                CacheResolveRequest::Contents { .. } => {
                    format!(
                        "PostgreSQL 内容缓存返回数量不匹配，后续仅使用 SQLite: expected={expected}, actual={}",
                        hits.len()
                    )
                }
            };
            return Err(RemoteTaskFailure {
                request,
                warning,
                queue_wait,
                query_elapsed,
            });
        }
        Err(error) => {
            let boundary = match &request {
                CacheResolveRequest::Paths { .. } => "路径",
                CacheResolveRequest::Contents { .. } => "内容",
            };
            return Err(RemoteTaskFailure {
                request,
                warning: format!("PostgreSQL {boundary}缓存不可用，后续仅使用 SQLite: {error}"),
                queue_wait,
                query_elapsed,
            });
        }
    };
    let request_id = request.request_id();
    let (kind, completion_credits) = match request {
        CacheResolveRequest::Paths { items, .. } => (
            CacheResolutionKind::Paths(
                items
                    .into_iter()
                    .zip(hits)
                    .map(|(item, remote)| CacheResolvedItem {
                        key: item.key,
                        remote,
                    })
                    .collect(),
            ),
            None,
        ),
        CacheResolveRequest::Contents {
            items,
            completion_credits,
            ..
        } => (
            CacheResolutionKind::Contents(
                items
                    .into_iter()
                    .zip(hits)
                    .map(|(item, remote)| CacheResolvedItem {
                        key: item.key,
                        remote,
                    })
                    .collect(),
            ),
            Some(completion_credits),
        ),
    };
    Ok(CacheResolution {
        request_id,
        warning: None,
        queue_wait: Some(queue_wait),
        query_elapsed: Some(query_elapsed),
        kind,
        completion_credits,
    })
}

/// 首次远端异常后取消全部在途查询，并为所有上下文生成 local-only 结果。
#[allow(clippy::too_many_arguments)]
fn lock_local_only(
    failed: Option<RemoteTaskFailure>,
    warning: String,
    remote_available: &mut bool,
    pending_paths: &mut VecDeque<CacheResolveRequest>,
    pending_contents: &mut VecDeque<CacheResolveRequest>,
    path_tasks: &mut JoinSet<RemoteTaskOutput>,
    content_tasks: &mut JoinSet<RemoteTaskOutput>,
    inflight: &mut BTreeMap<u64, CacheResolveRequest>,
    path_results: &mut VecDeque<CacheResolution>,
    content_results: &mut VecDeque<CacheResolution>,
) {
    if !*remote_available {
        return;
    }
    tracing::warn!(
        event = "central_store_degraded",
        operation = "remote_cache_query",
        fallback = "sqlite_only",
        error = %warning,
        "PostgreSQL 缓存查询失败，后续仅使用 SQLite"
    );
    *remote_available = false;
    path_tasks.abort_all();
    content_tasks.abort_all();
    let inflight_requests = std::mem::take(inflight).into_values();
    let mut warning = Some(warning);
    if let Some(failure) = failed {
        push_resolution(
            fallback_resolution(
                failure.request,
                warning.take(),
                Some(failure.queue_wait),
                Some(failure.query_elapsed),
            ),
            path_results,
            content_results,
        );
    }
    for request in inflight_requests {
        push_resolution(
            fallback_resolution(request, warning.take(), None, None),
            path_results,
            content_results,
        );
    }
    for request in pending_contents.drain(..).chain(pending_paths.drain(..)) {
        push_resolution(
            unqueried_resolution(request, warning.take()),
            path_results,
            content_results,
        );
    }
}

/// 为未命中或 local-only 请求生成保持身份与长度的空候选结果。
fn fallback_resolution(
    request: CacheResolveRequest,
    warning: Option<String>,
    queue_wait: Option<Duration>,
    query_elapsed: Option<Duration>,
) -> CacheResolution {
    let request_id = request.request_id();
    let (kind, completion_credits) = match request {
        CacheResolveRequest::Paths { items, .. } => (
            CacheResolutionKind::Paths(
                items
                    .into_iter()
                    .map(|item| CacheResolvedItem {
                        key: item.key,
                        remote: None,
                    })
                    .collect(),
            ),
            None,
        ),
        CacheResolveRequest::Contents {
            items,
            completion_credits,
            ..
        } => (
            CacheResolutionKind::Contents(
                items
                    .into_iter()
                    .map(|item| CacheResolvedItem {
                        key: item.key,
                        remote: None,
                    })
                    .collect(),
            ),
            Some(completion_credits),
        ),
    };
    CacheResolution {
        request_id,
        warning,
        queue_wait,
        query_elapsed,
        kind,
        completion_credits,
    }
}

/// 为从未进入远端 future 的请求只记录真实 lane 等待，不伪造查询耗时。
fn unqueried_resolution(request: CacheResolveRequest, warning: Option<String>) -> CacheResolution {
    let queue_wait = request.enqueued_at().elapsed();
    fallback_resolution(request, warning, Some(queue_wait), None)
}

/// 按消息类型进入独立结果队列，发送时优先 content 以缩短 Worker 补位等待。
fn push_resolution(
    resolution: CacheResolution,
    path_results: &mut VecDeque<CacheResolution>,
    content_results: &mut VecDeque<CacheResolution>,
) {
    if resolution.warning.is_some() {
        // 首次降级必须先到 actor，使其在准备后续请求前同步翻转 local-only。
        content_results.push_front(resolution);
        return;
    }
    match resolution.kind {
        CacheResolutionKind::Paths(_) => path_results.push_back(resolution),
        CacheResolutionKind::Contents(_) => content_results.push_back(resolution),
    }
}

#[cfg(test)]
mod tests {
    use std::{
        sync::{
            Arc,
            atomic::{AtomicUsize, Ordering},
        },
        time::{Duration, Instant},
    };

    use dedup_core::{ContentKey, MachineId, TaskId};
    use dedup_node_store::{BaseCacheRecord, ScannedPath};
    use tokio::sync::{Notify, Semaphore};

    use super::{
        CacheContextKey, CacheResolveRequest, CacheResolverHandle, ContentResolveItem,
        spawn_cache_resolver,
    };
    use crate::{RemoteCacheError, RemoteFeatureCache};

    /// 永久停在第一次 content 查询的远端缓存，用于观察 resolver 自身反压和关闭行为。
    struct GatedResolverCache {
        /// 已进入远端 future 的调用数。
        calls: Arc<AtomicUsize>,
        /// 第一条远端 future 已开始的通知。
        entered: Arc<Notify>,
    }

    /// 首次 content 查询直接失败的远端缓存，用于验证一次性降级告警。
    struct ErrorResolverCache {
        /// 实际进入远端边界的次数。
        calls: Arc<AtomicUsize>,
    }

    /// 固定延迟的远端缓存，用于区分 resolver 排队等待和真实查询耗时。
    struct SlowResolverCache {
        /// 每条 content 查询的稳定延迟。
        delay: Duration,
    }

    impl RemoteFeatureCache for GatedResolverCache {
        async fn lookup_paths(
            &self,
            _machine_id: &MachineId,
            paths: &[ScannedPath],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            Ok(vec![None; paths.len()])
        }

        async fn lookup_contents(
            &self,
            _keys: &[ContentKey],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            self.entered.notify_one();
            std::future::pending().await
        }

        async fn publish_outbox(
            &mut self,
            _machine_id: &MachineId,
            _batch: &dedup_protocol::proto::SyncChangeBatch,
        ) -> Result<u64, RemoteCacheError> {
            Ok(0)
        }
    }

    impl RemoteFeatureCache for ErrorResolverCache {
        async fn lookup_paths(
            &self,
            _machine_id: &MachineId,
            paths: &[ScannedPath],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            Ok(vec![None; paths.len()])
        }

        async fn lookup_contents(
            &self,
            _keys: &[ContentKey],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            Err(RemoteCacheError::ConnectTimeout)
        }

        async fn publish_outbox(
            &mut self,
            _machine_id: &MachineId,
            _batch: &dedup_protocol::proto::SyncChangeBatch,
        ) -> Result<u64, RemoteCacheError> {
            Ok(0)
        }
    }

    impl RemoteFeatureCache for SlowResolverCache {
        async fn lookup_paths(
            &self,
            _machine_id: &MachineId,
            paths: &[ScannedPath],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            Ok(vec![None; paths.len()])
        }

        async fn lookup_contents(
            &self,
            keys: &[ContentKey],
        ) -> Result<Vec<Option<BaseCacheRecord>>, RemoteCacheError> {
            tokio::time::sleep(self.delay).await;
            Ok(vec![None; keys.len()])
        }

        async fn publish_outbox(
            &mut self,
            _machine_id: &MachineId,
            _batch: &dedup_protocol::proto::SyncChangeBatch,
        ) -> Result<u64, RemoteCacheError> {
            Ok(0)
        }
    }

    /// 构造一个单项 content 请求，稳定占用一个 resolver 请求槽。
    fn content_request(
        task_id: TaskId,
        request_id: u64,
        content_credits: &Arc<Semaphore>,
    ) -> CacheResolveRequest {
        let completion_credits = Arc::new(
            Arc::clone(content_credits)
                .try_acquire_many_owned(1)
                .expect("测试请求必须先取得一个 content 完成许可"),
        );
        CacheResolveRequest::Contents {
            request_id,
            enqueued_at: Instant::now(),
            items: vec![ContentResolveItem {
                key: CacheContextKey {
                    task_id,
                    item_id: format!("item-{request_id}"),
                },
                content_key: ContentKey::new([request_id as u8; 16], request_id),
            }],
            completion_credits,
        }
    }

    #[tokio::test]
    async fn content_lane_backpressures_at_hard_bound_and_endpoint_close_aborts_gate() {
        let task_id = TaskId::new();
        let calls = Arc::new(AtomicUsize::new(0));
        let entered = Arc::new(Notify::new());
        let CacheResolverHandle {
            path_requests,
            content_requests,
            resolutions,
            content_credits,
            task,
        } = spawn_cache_resolver(
            GatedResolverCache {
                calls: Arc::clone(&calls),
                entered: Arc::clone(&entered),
            },
            true,
            2,
            1,
        );

        content_requests
            .send(content_request(task_id, 1, &content_credits))
            .await
            .unwrap();
        tokio::time::timeout(std::time::Duration::from_secs(1), entered.notified())
            .await
            .expect("第一条 content 查询应进入永久 gate");
        content_requests
            .send(content_request(task_id, 2, &content_credits))
            .await
            .expect("第二个完成许可应允许一条有界等待请求");
        assert!(
            Arc::clone(&content_credits).try_acquire_owned().is_err(),
            "第三个项目必须先被 completion credit 硬上限反压"
        );
        assert_eq!(calls.load(Ordering::SeqCst), 1, "远端并发槽固定为一条");

        drop(content_requests);
        drop(path_requests);
        drop(resolutions);
        let exit = tokio::time::timeout(std::time::Duration::from_secs(1), task)
            .await
            .expect("关闭结果 endpoint 后 resolver 必须丢弃 gated future")
            .expect("resolver 任务不得 panic");
        assert!(exit.remote_available);
        assert_eq!(exit.remote.calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn remote_error_warns_once_and_all_later_requests_stay_local_only() {
        let task_id = TaskId::new();
        let calls = Arc::new(AtomicUsize::new(0));
        let CacheResolverHandle {
            path_requests,
            content_requests,
            mut resolutions,
            content_credits,
            task,
        } = spawn_cache_resolver(
            ErrorResolverCache {
                calls: Arc::clone(&calls),
            },
            true,
            2,
            1,
        );

        content_requests
            .send(content_request(task_id, 1, &content_credits))
            .await
            .unwrap();
        let first = tokio::time::timeout(std::time::Duration::from_secs(1), resolutions.recv())
            .await
            .expect("远端错误请求应返回 local-only resolution")
            .expect("resolver 结果通道不应关闭");
        assert!(first.warning.is_some(), "首次远端错误必须携带告警");
        drop(first);

        content_requests
            .send(content_request(task_id, 2, &content_credits))
            .await
            .unwrap();
        let second = tokio::time::timeout(std::time::Duration::from_secs(1), resolutions.recv())
            .await
            .expect("降级后的请求应立即返回 local-only resolution")
            .expect("resolver 结果通道不应关闭");
        assert!(second.warning.is_none(), "降级告警不得重复");
        assert_eq!(calls.load(Ordering::SeqCst), 1, "后续不得再次调用远端");

        drop(content_requests);
        drop(path_requests);
        drop(resolutions);
        let exit = task.await.expect("resolver 任务不得 panic");
        assert!(!exit.remote_available);
        assert_eq!(exit.remote.calls.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn content_resolution_separates_queue_wait_from_query_time() {
        let task_id = TaskId::new();
        let delay = Duration::from_millis(25);
        let CacheResolverHandle {
            path_requests,
            content_requests,
            mut resolutions,
            content_credits,
            task,
        } = spawn_cache_resolver(SlowResolverCache { delay }, true, 2, 1);

        content_requests
            .send(content_request(task_id, 1, &content_credits))
            .await
            .unwrap();
        content_requests
            .send(content_request(task_id, 2, &content_credits))
            .await
            .unwrap();
        let first = resolutions.recv().await.expect("第一条查询应返回");
        let second = resolutions.recv().await.expect("第二条查询应返回");

        assert!(
            first.query_elapsed.expect("成功查询必须携带耗时") >= delay,
            "查询耗时必须来自真实远端 future"
        );
        assert!(
            second.queue_wait.expect("排队请求必须携带等待耗时") >= delay,
            "第二条请求必须观测第一条查询占用的远端槽位"
        );
        assert!(
            second.query_elapsed.expect("成功查询必须携带耗时") >= delay,
            "第二条查询的服务耗时不得混入排队等待"
        );

        drop(first);
        drop(second);
        drop(content_requests);
        drop(path_requests);
        drop(resolutions);
        task.await.expect("resolver 任务不得 panic");
    }
}

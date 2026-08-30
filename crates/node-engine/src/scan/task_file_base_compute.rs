//! 瞬态任务文件基础 Hash 阶段的逐项运行边界。
//!
//! 本模块只保留统一流式协调器需要的 pending owner 和 Hash future 窗口；缓存查询、
//! Media 续算和 SQLite ACK 均由单一事件泵按项推进。

use std::collections::BTreeMap;

use dedup_windows::ReadCancellationToken;
use tokio::task::JoinSet;

use super::{BaseTaskManifest, BaseTaskProduction, HashPermitReader};
use crate::{
    io::ReadFailure,
    task_dispatch::{DispatchedTask, TaskLanePermitProvider},
    task_files::{TaskFileIdentity, TaskFileRecord},
};

/// Hash 阶段结束后仍由统一流式协调器拥有的任务文件状态。
pub(crate) struct TaskFileBaseComputePending<P: TaskLanePermitProvider> {
    /// 已封闭的任务文件 dispatcher，继续拥有所有 P/C/F 行状态。
    pub(crate) dispatcher: crate::task_dispatch::TaskFileDispatcher<P>,
    /// 任务文件身份对应的内存上下文；只保留尚未完成的行。
    pub(crate) contexts: BTreeMap<TaskFileIdentity, super::TaskFileBaseContext>,
    /// 当前扫描清单，包含 ACK 后新增的 resolved 文件和命中数。
    pub(crate) manifest: BaseTaskManifest,
    /// 尚未从 dispatcher 领取的 Hash 任务行数量。
    pub(crate) remaining_hash_rows: usize,
    /// dispatcher admission 最近一次阻塞的明确原因。
    pub(crate) blocked_reason: Option<crate::task_dispatch::TaskDispatchBlockReason>,
}

impl<P: TaskLanePermitProvider> TaskFileBaseComputePending<P> {
    /// 将已封闭生产结果转换为统一流式协调器的唯一 pending owner。
    pub(crate) fn from_production(production: BaseTaskProduction<P>) -> Self {
        let BaseTaskProduction {
            dispatcher,
            contexts,
            manifest,
        } = production;
        let remaining_hash_rows = contexts
            .keys()
            .filter(|identity| identity.missing().needs_md5())
            .count();
        Self {
            dispatcher,
            contexts,
            manifest,
            remaining_hash_rows,
            blocked_reason: None,
        }
    }
}

/// 一个并发 Hash future 的顺序化结果；JoinSet 完成顺序不代表任务文件顺序。
pub(super) struct HashReadOutcome {
    /// dispatcher 交付任务时分配的单调序号。
    pub(super) sequence: usize,
    /// 读取任务的完整文件身份。
    pub(super) identity: TaskFileIdentity,
    /// 读取任务的原始行记录。
    pub(super) record: TaskFileRecord,
    /// 读取结果；成功读取在 future 内已释放 dispatcher 交付的 permit。
    pub(super) result: Result<[u8; 16], ReadFailure>,
}

/// Hash 读取的可逐项推进运行态；每次只交付一个已经结束的读取结果。
pub(super) struct TaskFileHashRuntime {
    /// 正在读取的 Hash future；拥有 permit 直到读取 future 释放它。
    reads: JoinSet<HashReadOutcome>,
    /// 尚未由 dispatcher 领取的 Hash 行数。
    unclaimed_rows: usize,
    /// 为完成顺序无关的后续处理分配稳定序号。
    next_sequence: usize,
    /// 仍在途读取的取消令牌；键与 future 的单调序号一一对应。
    cancellations: BTreeMap<usize, ReadCancellationToken>,
}

impl TaskFileHashRuntime {
    /// 用当前未领取的 Hash 行数量创建空运行态。
    pub(super) fn new(remaining_hash_rows: usize) -> Self {
        Self {
            reads: JoinSet::new(),
            unclaimed_rows: remaining_hash_rows,
            next_sequence: 0,
            cancellations: BTreeMap::new(),
        }
    }

    /// 返回当前窗口能否再领取一条 Hash 读取任务。
    pub(super) fn can_dispatch(&self, hash_capacity: usize) -> bool {
        self.unclaimed_rows > 0 && self.reads.len() < hash_capacity
    }

    /// 启动一个已领取的 Hash 读取，并把读取 permit 的释放限制在 future 内。
    pub(super) fn spawn<P, H>(&mut self, task: DispatchedTask<P>, reader: H) -> Result<(), String>
    where
        P: Send + 'static,
        H: HashPermitReader<Permit = P>,
    {
        if self.unclaimed_rows == 0 {
            return Err("Hash 运行态没有可领取的任务行".into());
        }
        if task.record.known_md5.is_some() || !task.record.missing.needs_md5() {
            return Err("Hash 运行态收到非 Hash 任务行".into());
        }
        self.unclaimed_rows -= 1;
        let sequence = self.next_sequence;
        self.next_sequence += 1;
        let identity = task.identity;
        let record = task.record;
        let scanned = record.scanned.clone();
        // 每条 Hash 读取使用内部 token；清理只能取消自身 owner，不能改写用户共享 token。
        let cancellation = ReadCancellationToken::new();
        self.cancellations.insert(sequence, cancellation.clone());
        self.reads.spawn(async move {
            let result = reader
                .read_with_permit(scanned, task.permit, cancellation, None)
                .await
                .map(|product| {
                    let md5 = product.md5;
                    // Hash 完成即释放读取许可，SQLite 查询和 ACK 不占据磁盘窗口。
                    drop(product.lease);
                    md5
                });
            HashReadOutcome {
                sequence,
                identity,
                record,
                result,
            }
        });
        Ok(())
    }

    /// 等待并返回恰好一条 Hash 读取结果，不会排空其它已在窗口内的 future。
    pub(super) async fn join_one(&mut self) -> Result<HashReadOutcome, String> {
        let joined = self
            .reads
            .join_next()
            .await
            .ok_or_else(|| "Hash 运行态没有在途读取".to_owned())?;
        let outcome = joined.map_err(|error| format!("Hash 读取 future 异常结束: {error}"))?;
        self.cancellations.remove(&outcome.sequence);
        Ok(outcome)
    }

    /// 返回仍在读取窗口内的 future 数量。
    pub(super) fn active_len(&self) -> usize {
        self.reads.len()
    }

    /// 返回仍与在途读取一一对应的取消令牌数，供状态收束测试观察。
    #[cfg(test)]
    fn active_cancellation_len(&self) -> usize {
        self.cancellations.len()
    }

    /// 所有 Hash 行均已领取并且所有读取 future 都已结束。
    pub(super) fn is_finished(&self) -> bool {
        self.unclaimed_rows == 0 && self.reads.is_empty()
    }

    /// 请求读取自行取消并回收所有在途 future，确保 permit 在返回前已经释放。
    pub(super) async fn cancel_and_join(&mut self) {
        for cancellation in self.cancellations.values() {
            cancellation.cancel();
        }
        self.reads.abort_all();
        while self.reads.join_next().await.is_some() {}
        self.cancellations.clear();
    }
}

#[cfg(test)]
mod tests {
    use std::{
        future::Future,
        pin::Pin,
        sync::{
            Arc,
            atomic::{AtomicIsize, Ordering},
        },
        time::Duration,
    };

    use dedup_core::{DisplayPath, NormalizedPath};
    use dedup_node_store::ScannedPath;
    use dedup_windows::{LocalDiskKind, PhysicalDiskId, ReadCancellationToken};
    use tokio::{sync::Notify, time::timeout};

    use super::TaskFileHashRuntime;
    use crate::{
        io::{DiskReadClass, ReadFailure},
        scan::{HashPermitReader, ReadProduct, TaskDiskLane},
        task_dispatch::DispatchedTask,
        task_files::{TaskFileIdentity, TaskFileRecord, TaskWorkKind, TaskWorkMask},
    };

    const RUN_ID: &str = "01900000-0000-7000-8000-000000000101";

    /// 不需要观测释放的轻量测试许可。
    #[derive(Clone, Copy, Debug)]
    struct TestPermit;

    /// 一个首项立即完成、次项等待显式放行的 Hash 读取器。
    #[derive(Clone)]
    struct GatedHashReader {
        /// 次项等待的测试门闩。
        gate: Arc<Notify>,
    }

    impl HashPermitReader for GatedHashReader {
        type Permit = TestPermit;

        fn read_with_permit(
            &self,
            scanned: ScannedPath,
            permit: Self::Permit,
            cancellation: ReadCancellationToken,
            _started: Option<crate::scan::HashReadStartedSignal>,
        ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Permit>, ReadFailure>> + Send>>
        {
            let first = scanned
                .normalized_path
                .as_str()
                .to_ascii_lowercase()
                .ends_with("runtime-first.bin");
            let gate = Arc::clone(&self.gate);
            Box::pin(async move {
                if first {
                    return Ok(ReadProduct {
                        md5: [0x41; 16],
                        lease: permit,
                    });
                }
                loop {
                    if cancellation.is_cancelled() {
                        let _ = permit;
                        return Err(ReadFailure::Cancelled);
                    }
                    tokio::select! {
                        _ = gate.notified() => return Ok(ReadProduct { md5: [0x42; 16], lease: permit }),
                        _ = tokio::time::sleep(Duration::from_millis(2)) => {}
                    }
                }
            })
        }
    }

    /// 不观察取消令牌的读取器，用于验证运行态仍会强制回收 future 和 permit。
    #[derive(Clone)]
    struct NeverCancelsHashReader;

    impl HashPermitReader for NeverCancelsHashReader {
        type Permit = TrackedPermit;

        fn read_with_permit(
            &self,
            _scanned: ScannedPath,
            permit: Self::Permit,
            _cancellation: ReadCancellationToken,
            _started: Option<crate::scan::HashReadStartedSignal>,
        ) -> Pin<Box<dyn Future<Output = Result<ReadProduct<Self::Permit>, ReadFailure>> + Send>>
        {
            Box::pin(async move {
                let _permit = permit;
                std::future::pending::<Result<ReadProduct<TrackedPermit>, ReadFailure>>().await
            })
        }
    }

    /// 能观察 permit 是否已被运行态释放的读取许可。
    struct TrackedPermit {
        /// 当前仍存活的许可计数。
        active: Arc<AtomicIsize>,
    }

    impl Drop for TrackedPermit {
        fn drop(&mut self) {
            self.active.fetch_sub(1, Ordering::SeqCst);
        }
    }

    /// 构造固定测试磁盘 lane。
    fn lane() -> TaskDiskLane {
        TaskDiskLane {
            physical_disk_id: PhysicalDiskId::from_disk_numbers([7]).unwrap(),
            physical_disk_numbers: vec![7],
            disk_kind: LocalDiskKind::Hdd,
            configured_weight: 1,
            per_disk_limit: 2,
        }
    }

    /// 构造测试扫描路径。
    fn scanned(path: &str) -> ScannedPath {
        ScannedPath::new(
            NormalizedPath::new(path).unwrap(),
            DisplayPath::new(path).unwrap(),
            16,
        )
    }

    /// 生成单条已领取的 Hash 行，permit 类型由调用方决定。
    fn dispatched_hash_task<P>(
        item_id: &str,
        offset: u64,
        path: &str,
        permit: P,
    ) -> DispatchedTask<P> {
        let lane = lane();
        let missing = TaskWorkMask::for_base(true, 0).expect("Hash 行必须携带 needs_md5");
        let item_id = uuid::Uuid::parse_str(item_id).unwrap();
        DispatchedTask {
            identity: TaskFileIdentity::new(RUN_ID, &lane, item_id, offset, 80, missing).unwrap(),
            record: TaskFileRecord {
                item_id,
                work_kind: TaskWorkKind::Base,
                scanned: scanned(path),
                known_md5: None,
                missing,
            },
            class: DiskReadClass::HashSequential,
            permit,
            continuation: false,
        }
    }

    #[tokio::test]
    async fn hash_runtime_returns_one_ready_item_without_draining_window() {
        let gate = Arc::new(Notify::new());
        let mut runtime = TaskFileHashRuntime::new(2);
        let reader = GatedHashReader {
            gate: Arc::clone(&gate),
        };
        runtime
            .spawn(
                dispatched_hash_task(
                    "01900000-0000-7000-8000-000000000111",
                    0,
                    r"C:\runtime-first.bin",
                    TestPermit,
                ),
                reader.clone(),
            )
            .unwrap();
        runtime
            .spawn(
                dispatched_hash_task(
                    "01900000-0000-7000-8000-000000000112",
                    80,
                    r"C:\runtime-second.bin",
                    TestPermit,
                ),
                reader,
            )
            .unwrap();

        let first = timeout(Duration::from_secs(1), runtime.join_one())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(first.result.unwrap(), [0x41; 16]);
        assert_eq!(runtime.active_len(), 1);
        assert_eq!(runtime.active_cancellation_len(), 1);
        gate.notify_waiters();
        runtime.cancel_and_join().await;
    }

    #[tokio::test]
    async fn hash_runtime_cancellation_aborts_reader_that_ignores_token_and_releases_permit() {
        let active = Arc::new(AtomicIsize::new(1));
        let mut runtime = TaskFileHashRuntime::new(1);
        runtime
            .spawn(
                dispatched_hash_task(
                    "01900000-0000-7000-8000-000000000113",
                    0,
                    r"C:\runtime-cancel.bin",
                    TrackedPermit {
                        active: Arc::clone(&active),
                    },
                ),
                NeverCancelsHashReader,
            )
            .unwrap();
        timeout(Duration::from_millis(100), runtime.cancel_and_join())
            .await
            .expect("忽略取消令牌的读取器也必须被回收");
        assert_eq!(active.load(Ordering::SeqCst), 0);
    }
}

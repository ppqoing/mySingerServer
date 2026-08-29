//! 文件列表进入 SQLite 内容模型和一筛结果的单一扫描流程。

mod base_compute;
mod base_flow_control;
mod base_persistence;
mod base_task_producer;
mod cache_resolver;
mod engine;
mod enumerator;
mod everything;
mod hash;
pub(crate) mod input_order;
mod pipeline;
mod root_plan;
mod task_file_base_compute;
mod task_file_base_coordinator;
mod task_file_cache;
mod task_file_media_compute;
mod task_file_media_persistence;
mod task_file_scan_run;
mod task_file_stage2_compute;
pub(crate) use task_file_base_coordinator::{
    TaskFileBaseCoordinatorError, TaskFileBaseCoordinatorOptions, TaskFileBaseCoordinatorResult,
    TaskFileBaseCoordinatorSummary, run_task_file_base_coordinator,
    run_task_file_base_coordinator_with_remote, run_task_file_base_coordinator_with_runtime,
};
pub(crate) use task_file_cache::{
    TaskFileCacheError, TaskFileCacheResult, resolve_task_file_cache,
};
pub(crate) use task_file_media_compute::{
    MediaPassResult, TaskFileMediaCompleted, TaskFileMediaComputeError, TaskFileMediaFailure,
    run_task_file_media_compute,
};
pub(crate) use task_file_media_persistence::{
    TaskFileMediaPersistenceError, TaskFileMediaPersistenceOptions, persist_task_file_media_results,
};
pub(crate) use task_file_scan_run::{
    CompletedScanSnapshot, ScanRunResult, TaskFileScanRunOptions, run_task_file_scan,
    run_task_file_scan_with_runtime,
};
pub(crate) use task_file_stage2_compute::{
    Stage2TaskComputeError, Stage2TaskInput, Stage2TaskProducerError, Stage2TaskProduction,
    Stage2TaskRunResult, build_stage2_task_production, run_task_file_stage2,
};

#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub use base_compute::BaseComputeJoinObservationHooks;
pub(crate) use base_compute::configure_base_compute_runtime;
pub use base_compute::{BaseComputeDecision, BaseComputeEngine};
/// 生产读取器在真实 Hash 磁盘许可边界发布的阶段信号。
pub use base_flow_control::HashReadStartedSignal;
/// 二筛 task-file 编排复用的唯一 SQLite 单写 actor。
pub(crate) use base_persistence::BaseStoreActor;
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub use base_persistence::{BasePersistTestController, BasePersistTestWaiter};
pub use base_task_producer::{
    BaseTaskInput, BaseTaskManifest, BaseTaskProducer, BaseTaskProducerError, BaseTaskProduction,
    MAX_BASE_TASK_BATCH, TaskFileBaseContext,
};
pub use dedup_windows::WindowsWalker;
pub use engine::{
    ScanEngine, ScanOptions, ScanSummary, Stage1BatchResult, Stage1ProcessError, Stage1Processor,
    Stage1Request, WorkerPoolStage1Processor, begin_scan_task, publish_contact_sheet_for_test,
};
pub use enumerator::FileEnumerator;
pub use everything::EverythingEnumerator;
pub(crate) use everything::{PreferredEverythingEnumerator, ensure_everything_ready};
pub use hash::{FileHasher, SystemMd5, md5_bytes, md5_file};
#[cfg(feature = "test-hooks")]
#[doc(hidden)]
pub use input_order::interleave_rows_by_root_for_test;
pub use pipeline::{
    HashPermitReader, PipelineFileReader, PipelineLimits, ReadProduct, ScheduledFileReader,
    ScheduledReadPermit,
};
pub use root_plan::{
    PlannedScannedPath, ResolvedScanRootStorage, ScanDiskPlan, ScanRootStorageResolver,
    SystemScanRootStorageResolver, TaskDiskLane,
};

use thiserror::Error;

/// 枚举、文件读取、SQLite 或一筛编排失败。
#[derive(Debug, Error)]
pub enum ScanError {
    /// 用户取消当前扫描，不能继续枚举、读取、计算或成功收尾。
    #[error("扫描已取消")]
    Cancelled,
    /// 当前枚举器无法完成整次文件列表。
    #[error("文件枚举失败: {0}")]
    Enumeration(String),
    /// 文件读取或联系表写入失败。
    #[error("文件 IO 失败: {0}")]
    Io(#[from] std::io::Error),
    /// SQLite 扫描任务或内容事务失败。
    #[error(transparent)]
    Store(#[from] dedup_node_store::StoreError),
    /// 枚举器返回了无效路径或字段。
    #[error("枚举结果无效: {0}")]
    InvalidResult(String),
    /// Worker 一筛结果与请求不匹配。
    #[error("一筛处理失败: {0}")]
    Stage1(String),
    /// 扫描根在枚举前无法解析到稳定的本机物理存储位置。
    #[error("SCAN_ROOT_STORAGE_RESOLVE_FAILED: {root}: {message}")]
    ScanRootStorageResolveFailed {
        /// 无法解析的用户显示扫描根。
        root: String,
        /// Windows 存储查询返回的原因。
        message: String,
    },
}

//! Worker 媒体流水线、子进程生命周期与调度池。

mod file_session;
mod pipeline;
mod pool;
mod process;

pub use file_session::{WorkerFileSession, WorkerFileSessionError, WorkerReadLimits};
pub use pipeline::{
    BaseComputeOutput, BaseMissingParts, FfmpegDecoder, MediaDecoder, PreparedStage2Compute,
    Stage1Frame, Stage1Output, Stage2Frame, Stage2Output, WorkerPipeline, WorkerPipelineError,
    WorkerRequestHandler, decode_base_compute_payload, decode_stage1_payload,
    decode_stage2_payload, encode_base_compute_payload, encode_stage1_payload,
    encode_stage2_payload, handle_worker_request,
};
pub use pool::{
    ControlledWorkerPool, WorkerDispatchBarrier, WorkerEvent, WorkerFileIdentity, WorkerPool,
    WorkerPoolConfig, WorkerPoolError, WorkerPoolHandle,
};
pub use process::WorkerLaunch;

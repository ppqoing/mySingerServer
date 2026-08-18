//! Worker 媒体流水线、子进程生命周期与调度池。

mod pipeline;
mod pool;
mod process;

pub use pipeline::{
    FfmpegDecoder, MediaDecoder, Stage1Frame, Stage1Output, Stage2Frame, Stage2Output,
    WorkerPipeline, WorkerPipelineError, decode_stage1_payload, decode_stage2_payload,
    encode_stage1_payload, encode_stage2_payload, handle_worker_request,
};
pub use pool::{WorkerEvent, WorkerPool, WorkerPoolConfig, WorkerPoolError};
pub use process::WorkerLaunch;

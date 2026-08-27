//! 基础计算旧流水线的固定参数基准入口；输出原始阶段时间、空闲时间与吞吐。

#[path = "../tests/base_compute_utilization.rs"]
mod fixture;

/// 以单次固定混合清单运行输出可写入验收记录的原始测量值。
fn main() {
    let runtime = tokio::runtime::Runtime::new().expect("应创建基准 Tokio 运行时");
    let metrics = runtime.block_on(fixture::run_mixed_baseline());
    println!("seed={:#018X}", metrics.seed);
    println!("files={}", metrics.total_files);
    println!("cache_hits={}", metrics.cache_hits);
    println!("hash_sessions={}", metrics.hash_sessions);
    println!("media_decode_jobs={}", metrics.media_decode_jobs);
    println!(
        "cache_wait_ms={:.3}",
        metrics.cache_wait.as_secs_f64() * 1_000.0
    );
    println!(
        "worker_idle_before_hash_ms={:.3}",
        metrics.worker_idle_before_hash.as_secs_f64() * 1_000.0
    );
    println!(
        "decode_and_persist_ms={:.3}",
        metrics.decode_and_persist.as_secs_f64() * 1_000.0
    );
    println!("elapsed_ms={:.3}", metrics.elapsed.as_secs_f64() * 1_000.0);
    println!(
        "throughput_files_per_second={:.3}",
        metrics.throughput_files_per_second
    );
    println!(
        "worker_idle_while_cache_waits={}",
        metrics.worker_idle_while_cache_waits
    );
    println!("persisted_completed={}", metrics.persisted_completed);
}

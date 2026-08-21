//! 真实媒体半小时验收客户端的参数、节拍和续跑行为契约。

#![cfg(windows)]

#[allow(dead_code)]
#[path = "../examples/runtime_acceptance.rs"]
mod runtime_acceptance;

use std::{
    collections::VecDeque,
    future::Future,
    path::Path,
    pin::Pin,
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_protocol::proto;
use runtime_acceptance::{
    AcceptanceClock, AcceptanceConfig, AcceptanceSession, AcceptanceSink, RuntimeAcceptanceSample,
    run_acceptance,
};

type TestFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T, String>> + Send + 'a>>;

#[derive(Clone, Default)]
struct FakeClock {
    elapsed: Arc<Mutex<Duration>>,
}

impl AcceptanceClock for FakeClock {
    fn elapsed(&self) -> Duration {
        *self.elapsed.lock().expect("测试时钟锁")
    }

    fn sleep<'a>(&'a self, duration: Duration) -> Pin<Box<dyn Future<Output = ()> + Send + 'a>> {
        Box::pin(async move {
            *self.elapsed.lock().expect("测试时钟锁") += duration;
        })
    }
}

#[derive(Default)]
struct FakeState {
    creates: Vec<(Vec<String>, bool, String)>,
    cancels: Vec<String>,
    active_runtime: Option<String>,
    runtime_sequence: u32,
    detail_reads: u32,
    cancelled: bool,
}

#[derive(Clone, Default)]
struct FakeSession {
    state: Arc<Mutex<FakeState>>,
}

impl AcceptanceSession for FakeSession {
    fn create_scan<'a>(
        &'a self,
        roots: Vec<String>,
        force_recalculate: bool,
        enumerator: &'a str,
    ) -> TestFuture<'a, String> {
        Box::pin(async move {
            let mut state = self.state.lock().expect("测试会话锁");
            state.runtime_sequence += 1;
            let sequence = state.runtime_sequence;
            let runtime_id = format!("runtime-{sequence}");
            state.active_runtime = Some(runtime_id);
            state
                .creates
                .push((roots, force_recalculate, enumerator.into()));
            Ok(format!("persistent-{sequence}"))
        })
    }

    fn list_runtime_tasks<'a>(&'a self) -> TestFuture<'a, Vec<proto::RuntimeTaskSummary>> {
        Box::pin(async move {
            let state = self.state.lock().expect("测试会话锁");
            Ok(state
                .active_runtime
                .iter()
                .map(|runtime_id| proto::RuntimeTaskSummary {
                    runtime_task_id: runtime_id.clone(),
                    machine_id: "a".repeat(64),
                    task_kind: "scan".into(),
                    title: "扫描".into(),
                    state: "running".into(),
                    stage_summary: "读取与 MD5".into(),
                    overall_completed: state.detail_reads as u64,
                    overall_total: 900,
                    overall_total_known: true,
                    overall_failed: 0,
                    overall_skipped: 0,
                })
                .collect())
        })
    }

    fn runtime_task_details<'a>(
        &'a self,
        runtime_task_id: &'a str,
    ) -> TestFuture<'a, proto::RuntimeTaskDetails> {
        Box::pin(async move {
            let mut state = self.state.lock().expect("测试会话锁");
            state.detail_reads += 1;
            let completed = state.runtime_sequence == 1 && state.detail_reads == 2;
            let summary = proto::RuntimeTaskSummary {
                runtime_task_id: runtime_task_id.into(),
                machine_id: "a".repeat(64),
                task_kind: "scan".into(),
                title: "扫描".into(),
                state: if completed {
                    "completed"
                } else if state.cancelled {
                    "cancelled"
                } else {
                    "running"
                }
                .into(),
                stage_summary: "读取与 MD5".into(),
                overall_completed: state.detail_reads as u64,
                overall_total: 900,
                overall_total_known: true,
                overall_failed: 0,
                overall_skipped: 0,
            };
            Ok(proto::RuntimeTaskDetails {
                summary: Some(summary),
                stages: Vec::new(),
                workers: Vec::new(),
                failures: Vec::new(),
            })
        })
    }

    fn cancel_task<'a>(&'a self, persistent_task_id: &'a str) -> TestFuture<'a, ()> {
        Box::pin(async move {
            let mut state = self.state.lock().expect("测试会话锁");
            state.cancels.push(persistent_task_id.into());
            state.cancelled = true;
            Ok(())
        })
    }
}

#[derive(Clone, Default)]
struct MemorySink {
    samples: Arc<Mutex<VecDeque<RuntimeAcceptanceSample>>>,
}

impl AcceptanceSink for MemorySink {
    fn write_sample(&mut self, sample: &RuntimeAcceptanceSample) -> Result<(), String> {
        self.samples
            .lock()
            .expect("样本锁")
            .push_back(sample.clone());
        Ok(())
    }
}

#[test]
fn duration_tick_and_output_boundary_are_fixed() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-1");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    assert_eq!(config.duration(), Duration::from_secs(1800));
    assert_eq!(config.sample_interval(), Duration::from_secs(2));
    assert_eq!(config.enumerator(), "everything");

    let short = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1799,
        root,
        &root.join("runtime.ndjson"),
    );
    assert!(short.is_err(), "少于1800秒必须拒绝");

    let escaped = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        Path::new(r"C:\tmp\outside.ndjson"),
    );
    assert!(escaped.is_err(), "输出不得逃逸证据根");
}

#[tokio::test]
async fn completed_scan_restarts_forced_and_deadline_cancels_active_task() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-2");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    let clock = FakeClock::default();
    let sink = MemorySink::default();

    let result = run_acceptance(&session, &clock, sink.clone(), &config)
        .await
        .expect("验收协调应完成");
    let state = session.state.lock().expect("测试会话锁");
    assert_eq!(
        state.creates[0],
        (vec![r"D:\Media".into()], false, "everything".into())
    );
    assert!(
        state
            .creates
            .iter()
            .skip(1)
            .all(|(_, force, enumerator)| *force && enumerator == "everything"),
        "提前完成后的所有续跑必须强制重算且继续使用Everything"
    );
    assert_eq!(state.cancels, vec!["persistent-2"]);
    assert_eq!(result.duration_seconds, 1800);
    assert_eq!(sink.samples.lock().expect("样本锁").len(), 900);
}

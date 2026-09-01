//! 真实媒体半小时验收客户端的参数、节拍和续跑行为契约。

#![cfg(windows)]

#[allow(dead_code)]
#[path = "../examples/runtime_acceptance.rs"]
mod runtime_acceptance;

use std::{
    collections::VecDeque,
    env,
    ffi::OsString,
    future::Future,
    path::Path,
    pin::Pin,
    sync::{Arc, Mutex},
    time::Duration,
};

use dedup_protocol::proto;
use runtime_acceptance::{
    AcceptanceClock, AcceptanceConfig, AcceptanceSession, AcceptanceSink, RuntimeAcceptanceResult,
    RuntimeAcceptanceSample, run_acceptance,
};

type TestFuture<'a, T> = Pin<Box<dyn Future<Output = Result<T, String>> + Send + 'a>>;

#[derive(Clone, Default)]
struct FakeClock {
    elapsed: Arc<Mutex<Duration>>,
    sleep_calls: Arc<Mutex<Vec<Duration>>>,
}

impl FakeClock {
    /// 注入一次查询耗时，模拟单调时钟在 Node 请求期间继续前进。
    fn advance(&self, duration: Duration) {
        *self.elapsed.lock().expect("测试时钟锁") += duration;
    }

    /// 读取所有 sleep 请求，验证调度使用绝对 tick 而非固定间隔叠加。
    fn sleep_calls(&self) -> Vec<Duration> {
        self.sleep_calls.lock().expect("测试 sleep 锁").clone()
    }
}

impl AcceptanceClock for FakeClock {
    fn elapsed(&self) -> Duration {
        *self.elapsed.lock().expect("测试时钟锁")
    }

    fn sleep<'a>(&'a self, duration: Duration) -> Pin<Box<dyn Future<Output = ()> + Send + 'a>> {
        Box::pin(async move {
            self.sleep_calls
                .lock()
                .expect("测试 sleep 锁")
                .push(duration);
            *self.elapsed.lock().expect("测试时钟锁") += duration;
        })
    }
}

#[derive(Default)]
struct FakeState {
    creates: Vec<(Vec<String>, bool, String)>,
    create_calls: u32,
    create_error_on_call: Option<u32>,
    cancels: Vec<String>,
    cancel_error: bool,
    active_runtime: Option<String>,
    hide_runtime: bool,
    runtime_sequence: u32,
    detail_reads: u32,
    cancelled: bool,
    complete_first: bool,
    complete_after_cancel: bool,
    /// 取消请求成功后将活动任务落为 failed，覆盖错误豁免场景。
    failed_after_cancel: bool,
    /// 首个扫描在第二次详情采样时进入 failed 终态。
    failed_first: bool,
    timeout_after_cancel: bool,
    list_calls: u32,
    list_error_on_call: Option<u32>,
    clock: Option<FakeClock>,
    detail_durations: VecDeque<Duration>,
    detail_error_on_read: Option<u32>,
    missing_summary_on_read: Option<u32>,
    /// 测试用的节点 outbox 高水位；真实值来自运行任务摘要协议字段。
    outbox_high_seq: Option<u64>,
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
            state.create_calls += 1;
            if state.create_error_on_call == Some(state.create_calls) {
                return Err("模拟创建扫描请求失败".into());
            }
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
            let mut state = self.state.lock().expect("测试会话锁");
            state.list_calls += 1;
            if state.list_error_on_call == Some(state.list_calls) {
                return Err("模拟运行任务发现请求失败".into());
            }
            if state.hide_runtime {
                return Ok(Vec::new());
            }
            Ok(state
                .active_runtime
                .iter()
                .map(|runtime_id| proto::RuntimeTaskSummary {
                    runtime_task_id: runtime_id.clone(),
                    machine_id: "a".repeat(64),
                    task_kind: "base_compute".into(),
                    title: "基础计算".into(),
                    state: "running".into(),
                    stage_summary: "基础计算".into(),
                    overall_completed: state.detail_reads as u64,
                    overall_total: 900,
                    overall_total_known: true,
                    overall_failed: 0,
                    overall_skipped: 0,
                    outbox_high_seq: state.outbox_high_seq,
                    ..Default::default()
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
            let detail_read = state.detail_reads;
            let work_duration = state.detail_durations.pop_front().unwrap_or_default();
            let clock = state.clock.clone();
            let should_error = state.detail_error_on_read == Some(detail_read);
            let missing_summary = state.missing_summary_on_read == Some(detail_read);
            let completed =
                (state.complete_first && state.runtime_sequence == 1 && detail_read == 2)
                    || (state.complete_after_cancel && state.cancelled);
            let failed = state.failed_first && state.runtime_sequence == 1 && detail_read == 2;
            let summary = proto::RuntimeTaskSummary {
                runtime_task_id: runtime_task_id.into(),
                machine_id: "a".repeat(64),
                task_kind: "base_compute".into(),
                title: "基础计算".into(),
                state: if state.cancelled && state.failed_after_cancel {
                    "failed"
                } else if failed {
                    "failed"
                } else if completed {
                    "completed"
                } else if state.cancelled && !state.timeout_after_cancel {
                    "cancelled"
                } else {
                    "running"
                }
                .into(),
                stage_summary: "基础计算".into(),
                overall_completed: state.detail_reads as u64,
                overall_total: 900,
                overall_total_known: true,
                overall_failed: u64::from(failed),
                overall_skipped: 0,
                outbox_high_seq: state.outbox_high_seq,
                ..Default::default()
            };
            let details = proto::RuntimeTaskDetails {
                summary: (!missing_summary).then_some(summary),
                stages: Vec::new(),
                workers: Vec::new(),
                failures: Vec::new(),
                execution_config: None,
                pipeline_metrics: None,
            };
            drop(state);
            if let Some(clock) = clock {
                clock.advance(work_duration);
            }
            if should_error {
                Err("模拟运行任务详情请求失败".into())
            } else {
                Ok(details)
            }
        })
    }

    fn cancel_task<'a>(&'a self, persistent_task_id: &'a str) -> TestFuture<'a, ()> {
        Box::pin(async move {
            let mut state = self.state.lock().expect("测试会话锁");
            if state.cancel_error {
                return Err("模拟取消请求失败".into());
            }
            state.cancels.push(persistent_task_id.into());
            state.cancelled = true;
            Ok(())
        })
    }
}

/// 临时设置验收环境变量，并在测试结束时恢复原值。
struct TestEnv {
    /// 测试期间修改过的环境变量及其原始值。
    original: Vec<(String, Option<OsString>)>,
}

impl TestEnv {
    /// 创建一个空的环境变量恢复器。
    fn new() -> Self {
        Self {
            original: Vec::new(),
        }
    }

    /// 记录原值后设置环境变量，避免污染其他测试。
    fn set(&mut self, name: &str, value: &str) {
        self.remember(name);
        // Rust 2024 将环境变量修改标记为 unsafe；测试串行运行并在 Drop 中恢复。
        unsafe { env::set_var(name, value) };
    }

    /// 记录原值后删除环境变量。
    fn remove(&mut self, name: &str) {
        self.remember(name);
        // Rust 2024 将环境变量修改标记为 unsafe；测试串行运行并在 Drop 中恢复。
        unsafe { env::remove_var(name) };
    }

    /// 只记录第一次修改，保证嵌套设置仍能恢复测试前的值。
    fn remember(&mut self, name: &str) {
        if !self.original.iter().any(|(key, _)| key == name) {
            self.original.push((name.into(), env::var_os(name)));
        }
    }
}

impl Drop for TestEnv {
    /// 按逆序恢复全部环境变量。
    fn drop(&mut self) {
        for (name, value) in self.original.iter().rev() {
            // Rust 2024 将环境变量修改标记为 unsafe；这里恢复测试前快照。
            unsafe {
                match value {
                    Some(value) => env::set_var(name, value),
                    None => env::remove_var(name),
                }
            }
        }
    }
}

/// 填充 from_env 所需的公共验收环境变量。
fn acceptance_test_env(roots_json: Option<&str>, single_run: Option<&str>) -> TestEnv {
    let mut environment = TestEnv::new();
    environment.set("RUST_V2_ACCEPTANCE_ENDPOINT", "127.0.0.1:39091");
    environment.set("RUST_V2_REAL_MEDIA_ROOT", "C:/Legacy");
    if let Some(roots_json) = roots_json {
        environment.set("RUST_V2_REAL_MEDIA_ROOTS_JSON", roots_json);
    } else {
        environment.remove("RUST_V2_REAL_MEDIA_ROOTS_JSON");
    }
    if let Some(single_run) = single_run {
        environment.set("RUST_V2_ACCEPTANCE_SINGLE_RUN", single_run);
    } else {
        environment.remove("RUST_V2_ACCEPTANCE_SINGLE_RUN");
    }
    environment.set(
        "RUST_V2_ACCEPTANCE_OUTPUT",
        "C:/tmp/rust-v2-runtime-acceptance/task-15/runtime.ndjson",
    );
    environment.set("RUST_V2_ACCEPTANCE_DURATION_SECONDS", "1800");
    environment.remove("RUST_V2_ACCEPTANCE_ENUMERATOR");
    environment
}

#[tokio::test]
async fn explicit_windows_walker_is_forwarded_to_create_scan() {
    let mut environment = acceptance_test_env(None, Some("1"));
    environment.set("RUST_V2_ACCEPTANCE_ENUMERATOR", "windows_walker");
    let config = AcceptanceConfig::from_env().expect("显式 Walker 应生成配置");
    let session = FakeSession::default();
    session.state.lock().expect("测试会话锁").complete_first = true;

    run_acceptance(
        &session,
        &FakeClock::default(),
        MemorySink::default(),
        &config,
    )
    .await
    .expect("显式 Walker 单轮应完成");

    let state = session.state.lock().expect("测试会话锁");
    assert_eq!(state.creates[0].2, "windows_walker");
}

#[test]
fn invalid_enumerator_value_returns_stable_error() {
    let mut environment = acceptance_test_env(None, None);
    environment.set("RUST_V2_ACCEPTANCE_ENUMERATOR", "unknown");

    let error = AcceptanceConfig::from_env().expect_err("非法枚举器必须拒绝");
    assert_eq!(
        error,
        "RUST_V2_ACCEPTANCE_ENUMERATOR 只接受 everything 或 windows_walker"
    );
}

#[tokio::test]
async fn json_media_roots_are_forwarded_to_first_scan_in_input_order() {
    let _environment = acceptance_test_env(Some(r#"["D:/Media-A","E:/Media-B"]"#), Some("1"));
    let config = AcceptanceConfig::from_env().expect("多根环境变量应生成配置");
    let session = FakeSession::default();
    session.state.lock().expect("测试会话锁").complete_first = true;

    let result = run_acceptance(
        &session,
        &FakeClock::default(),
        MemorySink::default(),
        &config,
    )
    .await
    .expect("单轮完成应正常返回");
    let state = session.state.lock().expect("测试会话锁");
    assert_eq!(
        state.creates.first().map(|create| &create.0),
        Some(&vec!["D:/Media-A".into(), "E:/Media-B".into()]),
        "首次 create_scan 必须按输入顺序收到全部根"
    );
    let result_json = serde_json::to_value(result).expect("最终结果应可序列化");
    assert_eq!(
        result_json["media_roots"],
        serde_json::json!(["D:/Media-A", "E:/Media-B"])
    );
    assert_eq!(result_json["single_run"], true);
}

#[tokio::test]
async fn single_run_completed_starts_once_and_writes_one_runtime_result() {
    let _environment = acceptance_test_env(None, Some("TrUe"));
    let config = AcceptanceConfig::from_env().expect("单轮环境变量应生成配置");
    let session = FakeSession::default();
    session.state.lock().expect("测试会话锁").complete_first = true;
    let sink = MemorySink::default();

    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config)
        .await
        .expect("completed 单轮应立即返回");
    let state = session.state.lock().expect("测试会话锁");
    assert_eq!(state.create_calls, 1, "单轮 completed 不得创建 forced scan");
    assert_eq!(result.scans_started, 1);
    assert_eq!(
        sink.results.lock().expect("结果锁").len(),
        1,
        "单轮 completed 只能写一条 runtime_result"
    );
}

#[tokio::test]
async fn single_run_records_roots_runtime_identity_terminal_and_outbox_highwater() {
    let _environment = acceptance_test_env(Some(r#"["D:/Media-A","E:/Media-B"]"#), Some("1"));
    let config = AcceptanceConfig::from_env().expect("多根单轮配置应有效");
    let session = FakeSession::default();
    {
        let mut state = session.state.lock().expect("测试会话锁");
        state.complete_first = true;
        state.outbox_high_seq = Some(77);
    }

    let result = run_acceptance(
        &session,
        &FakeClock::default(),
        MemorySink::default(),
        &config,
    )
    .await
    .expect("单轮完成应返回最终结果");
    let json = serde_json::to_value(result).expect("最终结果应可序列化");
    let scan = &json["scan_tasks"][0];
    assert_eq!(scan["runtime_task_id"], "runtime-1");
    assert_eq!(
        scan["media_roots"],
        serde_json::json!(["D:/Media-A", "E:/Media-B"])
    );
    assert_eq!(scan["terminal_state"], "completed");
    assert_eq!(scan["outbox_high_seq"], 77);
    assert_eq!(
        scan["task_file_stats"]["source"], "runtime_protocol_not_exposed",
        "协议没有任务文件 lane 时必须明确标记来源"
    );
    assert_eq!(scan["task_file_stats"]["pending"], serde_json::Value::Null);
    assert_eq!(
        scan["task_file_stats"]["completed"],
        serde_json::Value::Null
    );
    assert_eq!(scan["task_file_stats"]["failed"], serde_json::Value::Null);
    assert_eq!(
        scan["task_file_stats"]["cache_hits_not_in_task_file"],
        serde_json::Value::Null
    );
}

#[tokio::test]
async fn single_run_failed_starts_once_and_keeps_failure_correctness() {
    let _environment = acceptance_test_env(None, Some("1"));
    let config = AcceptanceConfig::from_env().expect("单轮环境变量应生成配置");
    let session = FakeSession::default();
    session.state.lock().expect("测试会话锁").failed_first = true;
    let sink = MemorySink::default();

    let result = run_acceptance(&session, &FakeClock::default(), sink, &config)
        .await
        .expect("failed 单轮仍应写出最终结果");
    let state = session.state.lock().expect("测试会话锁");
    assert_eq!(state.create_calls, 1, "单轮 failed 不得创建第二次扫描");
    assert_eq!(result.failed_scans, 1, "业务 failed 必须计入失败扫描数");
    assert_eq!(result.correctness, "FAIL", "业务 failed 仍必须裁决为 FAIL");
}

#[tokio::test]
async fn legacy_single_root_env_keeps_one_root_and_default_forced_scan_behavior() {
    let _environment = acceptance_test_env(None, None);
    let config = AcceptanceConfig::from_env().expect("旧单根环境变量应保持兼容");
    let session = FakeSession::default();
    session.state.lock().expect("测试会话锁").complete_first = true;

    let result = run_acceptance(
        &session,
        &FakeClock::default(),
        MemorySink::default(),
        &config,
    )
    .await
    .expect("默认持续模式应完成验收");
    let state = session.state.lock().expect("测试会话锁");
    assert_eq!(state.creates[0].0, vec!["C:/Legacy"]);
    assert!(state.creates.iter().skip(1).any(|(_, force, _)| *force));
    let result_json = serde_json::to_value(result).expect("最终结果应可序列化");
    assert_eq!(result_json["media_roots"], serde_json::json!(["C:/Legacy"]));
    assert_eq!(result_json["single_run"], false);
}

#[test]
fn invalid_roots_and_single_run_values_return_stable_errors() {
    for roots_json in ["[]", r#"["   "]"#, "not-json"] {
        let _environment = acceptance_test_env(Some(roots_json), None);
        let error = AcceptanceConfig::from_env().expect_err("非法多根配置必须失败");
        assert_eq!(
            error,
            "RUST_V2_REAL_MEDIA_ROOTS_JSON 必须是至少包含一个非空字符串的 JSON 数组"
        );
    }

    let _environment = acceptance_test_env(None, Some("yes"));
    let error = AcceptanceConfig::from_env().expect_err("非法单轮配置必须失败");
    assert_eq!(error, "RUST_V2_ACCEPTANCE_SINGLE_RUN 只接受 1 或 true");
}

#[derive(Clone, Default)]
struct MemorySink {
    samples: Arc<Mutex<VecDeque<RuntimeAcceptanceSample>>>,
    results: Arc<Mutex<Vec<RuntimeAcceptanceResult>>>,
    sample_write_attempts: Arc<Mutex<u32>>,
    result_write_attempts: Arc<Mutex<u32>>,
    sample_error_on_attempt: Option<u32>,
    result_error_on_attempt: Option<u32>,
}

impl AcceptanceSink for MemorySink {
    fn write_sample(&mut self, sample: &RuntimeAcceptanceSample) -> Result<(), String> {
        let mut attempts = self.sample_write_attempts.lock().expect("样本写出次数锁");
        *attempts += 1;
        if self.sample_error_on_attempt == Some(*attempts) {
            return Err("模拟样本 flush IO 错误".into());
        }
        self.samples
            .lock()
            .expect("样本锁")
            .push_back(sample.clone());
        Ok(())
    }

    /// 保存最终 runtime_result，便于错误路径确认先落盘再返回 Err。
    fn write_result(&mut self, result: &RuntimeAcceptanceResult) -> Result<(), String> {
        let mut attempts = self.result_write_attempts.lock().expect("结果写出次数锁");
        *attempts += 1;
        if self.result_error_on_attempt == Some(*attempts) {
            return Err("模拟结果 flush IO 错误".into());
        }
        self.results.lock().expect("结果锁").push(result.clone());
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
    assert_eq!(config.sample_interval(), Duration::from_secs(1));
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
    session.state.lock().expect("测试会话锁").complete_first = true;
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
        "提前完成后的所有续跑必须强制重算且继续使用 Everything"
    );
    assert_eq!(state.cancels, vec!["persistent-2"]);
    assert_eq!(result.duration_seconds, 1800);
    let samples = sink.samples.lock().expect("样本锁");
    assert_eq!(samples.len(), 1_800);
    let first =
        serde_json::to_value(samples.front().expect("应有首条样本")).expect("首条样本应可序列化");
    let second = serde_json::to_value(samples.get(1).expect("应有第二条样本"))
        .expect("第二条样本应可序列化");
    assert_eq!(first["sample_interval_ms"], 0);
    assert_eq!(second["sample_interval_ms"], 1_000);

    let result_json = serde_json::to_value(&result).expect("最终结果应可序列化");
    assert_eq!(result_json["scan_tasks"].as_array().unwrap().len(), 2);
    assert_eq!(
        result_json["scan_tasks"][0]["persistent_task_id"],
        "persistent-1"
    );
    assert_eq!(result_json["scan_tasks"][0]["runtime_task_id"], "runtime-1");
    assert_eq!(result_json["scan_tasks"][0]["terminal_state"], "completed");
    assert_eq!(
        result_json["scan_tasks"][1]["persistent_task_id"],
        "persistent-2"
    );
    assert_eq!(result_json["scan_tasks"][1]["runtime_task_id"], "runtime-2");
    assert_eq!(result_json["scan_tasks"][1]["terminal_state"], "cancelled");
    assert_eq!(
        result_json["latest_completed_persistent_task_id"],
        "persistent-1"
    );
    assert_eq!(
        result_json["deadline_cancelled_persistent_task_id"],
        "persistent-2"
    );
    assert_eq!(result_json["correctness"], "PASS");
}

#[tokio::test]
async fn no_completed_scan_is_explicitly_inconclusive_and_keeps_deadline_id_separate() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-3");
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

    let result = run_acceptance(&session, &clock, sink, &config)
        .await
        .expect("没有 completed 也应写出完整证据");
    let json = serde_json::to_value(result).expect("最终结果应可序列化");
    assert_eq!(
        json["latest_completed_persistent_task_id"],
        serde_json::Value::Null
    );
    assert_eq!(
        json["deadline_cancelled_persistent_task_id"],
        "persistent-1"
    );
    assert_eq!(json["correctness"], "INCONCLUSIVE");
    assert_eq!(json["scan_tasks"][0]["terminal_state"], "cancelled");
}

#[tokio::test]
async fn completion_during_deadline_cancel_wait_updates_latest_completed_id() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-cancel-race");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    session
        .state
        .lock()
        .expect("测试会话锁")
        .complete_after_cancel = true;
    let sink = MemorySink::default();

    let result = run_acceptance(&session, &FakeClock::default(), sink, &config)
        .await
        .expect("取消等待期间完成仍应返回结果");
    let json = serde_json::to_value(result).expect("最终结果应可序列化");
    assert_eq!(json["scan_tasks"][0]["terminal_state"], "completed");
    assert_eq!(json["latest_completed_persistent_task_id"], "persistent-1");
    assert_eq!(
        json["deadline_cancelled_persistent_task_id"],
        "persistent-1"
    );
    assert_eq!(json["correctness"], "PASS");
}

/// 验证 deadline 取消后的 failed 终态仍计入失败且不得 PASS。
#[tokio::test]
async fn failed_terminal_after_deadline_cancel_counts_failure_and_cannot_pass() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-cancel-failed");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    {
        let mut state = session.state.lock().expect("测试会话锁");
        state.complete_first = true;
        state.failed_after_cancel = true;
    }

    let result = run_acceptance(
        &session,
        &FakeClock::default(),
        MemorySink::default(),
        &config,
    )
    .await
    .expect("取消等待到 failed 仍应写出结果");
    let json = serde_json::to_value(result).expect("最终结果应可序列化");
    assert_eq!(json["scan_tasks"][1]["terminal_state"], "failed");
    assert_eq!(
        json["deadline_cancelled_persistent_task_id"],
        "persistent-2"
    );
    assert_eq!(json["failed_scans"], 1);
    assert_ne!(json["correctness"], "PASS");
}

#[tokio::test]
async fn runtime_discovery_error_writes_inconclusive_result_before_returning_error() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-discovery-error");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    session.state.lock().expect("测试会话锁").list_error_on_call = Some(2);
    let sink = MemorySink::default();
    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config).await;
    assert!(matches!(
        result,
        Err(error) if error == "runtime_task_discovery_failed"
    ));

    let results = sink.results.lock().expect("结果锁");
    let json = serde_json::to_value(results.last().expect("错误前必须落盘结果"))
        .expect("错误结果应可序列化");
    assert_eq!(json["fatal_error"], "runtime_task_discovery_failed");
    assert_eq!(json["diagnostic"], "runtime_task_list_request_failed");
    assert_eq!(json["correctness"], "INCONCLUSIVE");
    assert_eq!(json["scan_tasks"][0]["persistent_task_id"], "persistent-1");
    assert_eq!(
        json["scan_tasks"][0]["runtime_task_id"],
        serde_json::Value::Null
    );
}

#[tokio::test]
async fn missing_runtime_details_writes_result_before_returning_error() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-details-error");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    session
        .state
        .lock()
        .expect("测试会话锁")
        .missing_summary_on_read = Some(2);
    let sink = MemorySink::default();
    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config).await;
    assert!(matches!(
        result,
        Err(error) if error == "runtime_task_details_invalid"
    ));

    let results = sink.results.lock().expect("结果锁");
    let json = serde_json::to_value(results.last().expect("错误前必须落盘结果"))
        .expect("错误结果应可序列化");
    assert_eq!(json["fatal_error"], "runtime_task_details_invalid");
    assert_eq!(json["diagnostic"], "runtime_task_summary_missing");
    assert_eq!(json["correctness"], "INCONCLUSIVE");
    assert_eq!(json["scan_tasks"][0]["persistent_task_id"], "persistent-1");
    assert_eq!(json["scan_tasks"][0]["runtime_task_id"], "runtime-1");
    assert_eq!(
        json["scan_tasks"][0]["terminal_state"],
        serde_json::Value::Null
    );
}

#[tokio::test]
async fn sampling_uses_absolute_ticks_and_successful_boundaries_after_irregular_work() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-absolute-ticks");
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
    {
        let mut state = session.state.lock().expect("测试会话锁");
        state.clock = Some(clock.clone());
        state.detail_durations = VecDeque::from([
            Duration::from_millis(250),
            Duration::from_millis(350),
            Duration::from_millis(2_200),
            Duration::from_millis(100),
        ]);
    }
    let sink = MemorySink::default();

    run_acceptance(&session, &clock, sink.clone(), &config)
        .await
        .expect("不规则查询耗时仍应完成验收");
    let samples = sink.samples.lock().expect("样本锁");
    assert_eq!(samples[0].sample_interval_ms, 0);
    assert_eq!(samples[1].sample_interval_ms, 1_100);
    assert_eq!(samples[2].sample_interval_ms, 2_850);
    assert_eq!(samples[3].sample_interval_ms, 100);
    let sleeps = clock.sleep_calls();
    assert_eq!(sleeps[0], Duration::from_secs(1));
    assert_eq!(sleeps[1], Duration::from_millis(750));
    assert_eq!(sleeps[2], Duration::from_millis(650));
    assert_ne!(samples[1].sample_interval_ms, 1_000);
}

#[tokio::test]
async fn failed_detail_does_not_advance_last_successful_sample_boundary() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-sample-error");
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
    {
        let mut state = session.state.lock().expect("测试会话锁");
        state.clock = Some(clock.clone());
        state.detail_durations = VecDeque::from([
            Duration::from_millis(250),
            Duration::from_millis(350),
            Duration::from_millis(125),
        ]);
        state.detail_error_on_read = Some(3);
    }
    let sink = MemorySink::default();
    let result = run_acceptance(&session, &clock, sink.clone(), &config).await;
    assert!(matches!(
        result,
        Err(error) if error == "runtime_task_details_failed"
    ));

    let samples = sink.samples.lock().expect("样本锁");
    assert_eq!(samples.len(), 2);
    assert_eq!(samples[0].sample_interval_ms, 0);
    assert_eq!(samples[1].sample_interval_ms, 1_100);
    let results = sink.results.lock().expect("结果锁");
    let json = serde_json::to_value(results.last().expect("错误前必须落盘结果"))
        .expect("错误结果应可序列化");
    assert_eq!(json["sample_count"], 2);
    assert_eq!(json["fatal_error"], "runtime_task_details_failed");
}

#[tokio::test]
async fn terminal_wait_timeout_writes_result_before_returning_error() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-cancel-timeout");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    session
        .state
        .lock()
        .expect("测试会话锁")
        .timeout_after_cancel = true;
    let sink = MemorySink::default();
    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config).await;
    assert!(matches!(
        result,
        Err(error) if error == "runtime_terminal_wait_timeout"
    ));

    let results = sink.results.lock().expect("结果锁");
    let json = serde_json::to_value(results.last().expect("超时前必须落盘结果"))
        .expect("超时结果应可序列化");
    assert_eq!(json["fatal_error"], "runtime_terminal_wait_timeout");
    assert_eq!(json["diagnostic"], "runtime_terminal_state_unobserved");
    assert_eq!(json["correctness"], "INCONCLUSIVE");
    assert_eq!(
        json["deadline_cancelled_persistent_task_id"],
        "persistent-1"
    );
}

#[tokio::test]
async fn missing_runtime_after_successful_create_writes_one_inconclusive_result() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-no-runtime");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    session.state.lock().expect("测试会话锁").hide_runtime = true;
    let sink = MemorySink::default();
    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config).await;
    assert!(matches!(
        result,
        Err(error) if error == "runtime_task_discovery_failed"
    ));

    let results = sink.results.lock().expect("结果锁");
    assert_eq!(results.len(), 1, "发现失败只能写一条 runtime_result");
    assert_eq!(
        *sink.result_write_attempts.lock().expect("结果写出次数锁"),
        1
    );
    let json =
        serde_json::to_value(results.last().expect("应有错误结果")).expect("错误结果应可序列化");
    assert_eq!(json["scans_started"], 1);
    assert_eq!(json["correctness"], "INCONCLUSIVE");
    assert_eq!(json["fatal_error"], "runtime_task_discovery_failed");
    assert_eq!(json["scan_tasks"].as_array().unwrap().len(), 1);
    assert_eq!(json["scan_tasks"][0]["persistent_task_id"], "persistent-1");
}

#[tokio::test]
async fn initial_create_failure_writes_one_empty_inconclusive_result() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-create-error");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    session
        .state
        .lock()
        .expect("测试会话锁")
        .create_error_on_call = Some(1);
    let sink = MemorySink::default();
    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config).await;
    assert!(matches!(result, Err(error) if error == "create_scan_failed"));

    let results = sink.results.lock().expect("结果锁");
    assert_eq!(results.len(), 1, "创建失败只能写一条 runtime_result");
    assert_eq!(
        *sink.result_write_attempts.lock().expect("结果写出次数锁"),
        1
    );
    let json = serde_json::to_value(results.last().expect("应有创建错误结果"))
        .expect("创建错误结果应可序列化");
    assert_eq!(json["scans_started"], 0);
    assert_eq!(json["scan_tasks"].as_array().unwrap().len(), 0);
    assert_eq!(json["correctness"], "INCONCLUSIVE");
    assert_eq!(json["fatal_error"], "create_scan_failed");
}

#[tokio::test]
async fn cancel_request_failure_writes_one_result_without_deadline_id() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-cancel-error");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    session.state.lock().expect("测试会话锁").cancel_error = true;
    let sink = MemorySink::default();
    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config).await;
    assert!(matches!(result, Err(error) if error == "cancel_task_failed"));

    let results = sink.results.lock().expect("结果锁");
    assert_eq!(results.len(), 1, "取消失败只能写一条 runtime_result");
    let json = serde_json::to_value(results.last().expect("应有取消错误结果"))
        .expect("取消错误结果应可序列化");
    assert_eq!(json["fatal_error"], "cancel_task_failed");
    assert_eq!(
        json["deadline_cancelled_persistent_task_id"],
        serde_json::Value::Null
    );
    assert_eq!(json["cancelled_at_deadline"], false);
}

#[tokio::test]
async fn sample_write_failure_preserves_success_count_and_writes_one_failure_result() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-sample-write-error");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    let mut sink = MemorySink::default();
    sink.sample_error_on_attempt = Some(2);
    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config).await;
    assert!(matches!(result, Err(error) if error == "runtime_sample_write_failed"));

    assert_eq!(sink.samples.lock().expect("样本锁").len(), 1);
    assert_eq!(
        *sink.sample_write_attempts.lock().expect("样本写出次数锁"),
        2
    );
    let results = sink.results.lock().expect("结果锁");
    assert_eq!(results.len(), 1, "样本失败只能写一条 runtime_result");
    assert_eq!(
        *sink.result_write_attempts.lock().expect("结果写出次数锁"),
        1
    );
    let json = serde_json::to_value(results.last().expect("应有样本错误结果"))
        .expect("样本错误结果应可序列化");
    assert_eq!(json["sample_count"], 1);
    assert_eq!(json["fatal_error"], "runtime_sample_write_failed");
}

#[tokio::test]
async fn final_result_write_failure_returns_only_stable_result_write_code() {
    let root = Path::new(r"C:\tmp\rust-v2-runtime-acceptance\run-result-write-error");
    let config = AcceptanceConfig::new(
        "127.0.0.1:39091",
        r"D:\Media",
        1800,
        root,
        &root.join("runtime.ndjson"),
    )
    .expect("合法半小时配置");
    let session = FakeSession::default();
    let mut sink = MemorySink::default();
    sink.result_error_on_attempt = Some(1);
    let result = run_acceptance(&session, &FakeClock::default(), sink.clone(), &config).await;
    assert!(matches!(
        result,
        Err(error) if error == "runtime_result_write_failed"
    ));
    assert!(sink.results.lock().expect("结果锁").is_empty());
    assert_eq!(
        *sink.result_write_attempts.lock().expect("结果写出次数锁"),
        1
    );
}

#[test]
fn runtime_sample_preserves_pipeline_metrics_and_worker_phase() {
    let histogram = proto::RuntimeLatencyHistogram {
        buckets: vec![
            proto::RuntimeLatencyBucket {
                upper_bound_ms: Some(1),
                count: 2,
            },
            proto::RuntimeLatencyBucket {
                upper_bound_ms: None,
                count: 1,
            },
        ],
        count: 3,
        p50_ms: Some(1),
        p95_ms: Some(7),
        p99_ms: Some(9),
        max_ms: Some(11),
    };
    let queue = proto::RuntimeQueueMetrics {
        current: Some(2),
        peak: Some(5),
        capacity: Some(8),
        wait_latency: Some(histogram.clone()),
        service_latency: Some(histogram.clone()),
    };
    let resource = proto::RuntimeResourceMetrics {
        current: Some(1),
        peak: Some(4),
        capacity: Some(12),
        wait_latency: Some(histogram.clone()),
        service_latency: Some(histogram),
    };
    let details = proto::RuntimeTaskDetails {
        summary: Some(proto::RuntimeTaskSummary {
            runtime_task_id: "runtime-telemetry".into(),
            machine_id: "a".repeat(64),
            task_kind: "scan".into(),
            title: "扫描".into(),
            state: "running".into(),
            stage_summary: "基础计算".into(),
            overall_completed: 3,
            overall_total: 9,
            overall_total_known: true,
            overall_failed: 0,
            overall_skipped: 0,
            ..Default::default()
        }),
        stages: Vec::new(),
        workers: vec![proto::RuntimeWorkerDetails {
            slot: 2,
            process_id: Some(4321),
            stage_id: "base_compute".into(),
            display_path: r"I:\tmp\clip.mp4".into(),
            physical_disk_id: "PhysicalDisk7".into(),
            completed_files: 5,
            speed_per_second: 1.5,
            current_step: "媒体特征".into(),
            cache_detail: String::new(),
            phase: Some(proto::RuntimeWorkerPhase::RuntimeWorkerFeature as i32),
            cpu_weight: Some(3),
            decoder_threads: Some(3),
        }],
        failures: Vec::new(),
        execution_config: Some(proto::RuntimeExecutionConfig {
            hash_tasks: Some(16),
            path_cache_queue_capacity: Some(24),
            content_cache_queue_capacity: Some(48),
            decode_queue_capacity: Some(24),
            persist_queue_capacity: Some(1_012),
            worker_slots: Some(12),
            cpu_budget: Some(23),
            global_disk_permits: Some(16),
            hdd_per_disk_permits: Some(1),
            ssd_per_disk_permits: Some(16),
            unknown_per_disk_permits: Some(1),
        }),
        pipeline_metrics: Some(proto::RuntimePipelineMetrics {
            hash_queue: Some(queue.clone()),
            path_cache_queue: Some(queue.clone()),
            content_cache_queue: Some(queue.clone()),
            decode_queue: Some(queue.clone()),
            persist_queue: Some(queue),
            hash_io: Some(resource.clone()),
            media_io: Some(resource.clone()),
            cpu_weight: Some(resource.clone()),
            worker_slots: Some(resource),
            hash_bytes: Some(8_192),
            media_throughput: vec![proto::RuntimeMediaThroughput {
                media_kind: proto::MediaKind::MediaVideo as i32,
                size_bucket: "large".into(),
                files: 2,
                bytes: 512 * 1024 * 1024,
            }],
            hash_waiting_permit: Some(proto::RuntimeOwnershipMetrics {
                current: Some(1),
                peak: Some(2),
                capacity: Some(3),
            }),
            hash_reading: Some(proto::RuntimeOwnershipMetrics {
                current: Some(0),
                peak: Some(0),
                capacity: Some(0),
            }),
            hash_completed_unjoined: Some(proto::RuntimeOwnershipMetrics::default()),
            media_permit_waiting: Some(proto::RuntimeOwnershipMetrics::default()),
            media_acquire_ready: Some(proto::RuntimeOwnershipMetrics::default()),
            media_permit_ready: Some(proto::RuntimeOwnershipMetrics::default()),
            worker_dispatching: Some(proto::RuntimeOwnershipMetrics::default()),
            worker_start_pending: Some(proto::RuntimeOwnershipMetrics::default()),
            worker_decode: Some(proto::RuntimeOwnershipMetrics::default()),
            worker_feature: Some(proto::RuntimeOwnershipMetrics::default()),
            worker_result_wait: Some(proto::RuntimeOwnershipMetrics::default()),
            worker_phase_unknown: Some(proto::RuntimeOwnershipMetrics::default()),
            content_output_credit_owned: Some(proto::RuntimeOwnershipMetrics::default()),
            hash_refill_token_available: Some(proto::RuntimeOwnershipMetrics::default()),
            decode_credit_owned: Some(proto::RuntimeOwnershipMetrics::default()),
            item_completion_latency: Some(proto::RuntimeLatencyHistogram {
                count: 1,
                p95_ms: Some(42),
                ..Default::default()
            }),
            // 故意逆序输入，契约要求 NDJSON 按物理盘标识稳定排序。
            disk_reads: vec![
                proto::RuntimeDiskReadMetrics {
                    physical_disk_id: "PhysicalDisk2".into(),
                    capacity: Some(4),
                    hash_waiting: Some(3),
                    media_waiting: Some(2),
                    hash_active: Some(1),
                    media_active: Some(0),
                    hash_granted_total: Some(11),
                    media_granted_total: Some(12),
                    hash_released_total: Some(9),
                    media_released_total: Some(10),
                },
                proto::RuntimeDiskReadMetrics {
                    physical_disk_id: "PhysicalDisk1".into(),
                    capacity: Some(2),
                    hash_waiting: Some(1),
                    media_waiting: Some(0),
                    hash_active: Some(1),
                    media_active: Some(1),
                    hash_granted_total: Some(7),
                    media_granted_total: Some(8),
                    hash_released_total: Some(6),
                    media_released_total: Some(5),
                },
            ],
        }),
    };

    let sample = RuntimeAcceptanceSample::from_details(Duration::from_secs(2), details)
        .expect("遥测详情应转成验收样本");
    let json = serde_json::to_value(sample).expect("验收样本应可序列化");

    assert_eq!(json["workers"][0]["phase"], "feature");
    assert_eq!(json["workers"][0]["cpu_weight"], 3);
    assert_eq!(json["workers"][0]["decoder_threads"], 3);
    assert_eq!(json["execution_config"]["worker_slots"], 12);
    assert_eq!(json["execution_config"]["global_disk_permits"], 16);
    assert_eq!(json["pipeline_metrics"]["decode_queue"]["peak"], 5);
    assert_eq!(
        json["pipeline_metrics"]["decode_queue"]["wait_latency"]["p95_ms"],
        7
    );
    assert_eq!(json["pipeline_metrics"]["worker_slots"]["capacity"], 12);
    assert_eq!(json["pipeline_metrics"]["hash_bytes"], 8_192);
    assert_eq!(
        json["pipeline_metrics"]["media_throughput"][0]["size_bucket"],
        "large"
    );
    assert_eq!(
        json["pipeline_metrics"]["hash_waiting_permit"]["current"],
        1
    );
    assert_eq!(json["pipeline_metrics"]["hash_waiting_permit"]["peak"], 2);
    assert_eq!(
        json["pipeline_metrics"]["hash_waiting_permit"]["capacity"],
        3
    );
    assert_eq!(json["pipeline_metrics"]["hash_reading"]["current"], 0);
    assert_eq!(json["pipeline_metrics"]["hash_reading"]["peak"], 0);
    assert_eq!(json["pipeline_metrics"]["hash_reading"]["capacity"], 0);
    assert_eq!(
        json["pipeline_metrics"]["item_completion_latency"]["p95_ms"],
        42
    );
    assert_eq!(
        json["pipeline_metrics"]["disk_reads"],
        serde_json::json!([
            {
                "physical_disk_id": "PhysicalDisk1",
                "capacity": 2,
                "hash_waiting": 1,
                "media_waiting": 0,
                "hash_active": 1,
                "media_active": 1,
                "hash_granted_total": 7,
                "media_granted_total": 8,
                "hash_released_total": 6,
                "media_released_total": 5
            },
            {
                "physical_disk_id": "PhysicalDisk2",
                "capacity": 4,
                "hash_waiting": 3,
                "media_waiting": 2,
                "hash_active": 1,
                "media_active": 0,
                "hash_granted_total": 11,
                "media_granted_total": 12,
                "hash_released_total": 9,
                "media_released_total": 10
            }
        ])
    );
}

#[test]
fn missing_ownership_and_latency_fields_are_emitted_as_null() {
    let details = proto::RuntimeTaskDetails {
        summary: Some(proto::RuntimeTaskSummary {
            runtime_task_id: "runtime-legacy".into(),
            machine_id: "a".repeat(64),
            task_kind: "scan".into(),
            title: "旧节点扫描".into(),
            state: "running".into(),
            ..Default::default()
        }),
        pipeline_metrics: Some(proto::RuntimePipelineMetrics::default()),
        ..Default::default()
    };
    let json = serde_json::to_value(
        RuntimeAcceptanceSample::from_details(Duration::from_secs(1), details)
            .expect("旧节点详情应仍可映射"),
    )
    .expect("旧节点样本应可序列化");
    let pipeline = json["pipeline_metrics"]
        .as_object()
        .expect("应有流水线对象");
    for field in [
        "hash_waiting_permit",
        "hash_reading",
        "hash_completed_unjoined",
        "media_permit_waiting",
        "media_acquire_ready",
        "media_permit_ready",
        "worker_dispatching",
        "worker_start_pending",
        "worker_decode",
        "worker_feature",
        "worker_result_wait",
        "worker_phase_unknown",
        "content_output_credit_owned",
        "hash_refill_token_available",
        "decode_credit_owned",
        "item_completion_latency",
    ] {
        assert!(
            pipeline.contains_key(field),
            "缺失字段也必须显式写出 null：{field}"
        );
        assert!(
            pipeline[field].is_null(),
            "旧节点字段必须保持 null：{field}"
        );
    }
    assert_eq!(
        pipeline["disk_reads"],
        serde_json::json!([]),
        "旧 Node 不含 field 28 时必须固定输出空数组"
    );
    assert_eq!(json["sample_interval_ms"], 0);
}

//! 在隔离子进程验证 panic hook，避免修改并行测试进程的全局 hook。

use std::{env, fs, process::Command};

use dedup_core::logging::ProcessDiagnostics;

const CHILD_FLAG: &str = "DEDUP_DIAGNOSTIC_TEST_CHILD";
const LOG_PATH: &str = "DEDUP_DIAGNOSTIC_TEST_PATH";

/// 根据环境变量选择父进程断言或子进程 panic 路径。
fn main() {
    if env::var_os(CHILD_FLAG).is_some() {
        run_panicking_child();
    }
    verify_panicking_child_log();
}

/// 安装真实 hook 后触发未捕获 panic，使操作系统产生非零退出码。
fn run_panicking_child() -> ! {
    let path = env::var_os(LOG_PATH).expect("父进程必须传入应急日志路径");
    let diagnostics = ProcessDiagnostics::with_emergency_path("core-test", path);
    diagnostics.install_panic_hook();
    panic!("panic-log-sentinel");
}

/// 启动当前测试程序的子进程并核对实际落盘内容。
fn verify_panicking_child_log() {
    let directory = tempfile::tempdir().expect("必须创建隔离测试目录");
    let path = directory.path().join("core-test-emergency.log");
    let status = Command::new(env::current_exe().expect("必须定位测试程序"))
        .env(CHILD_FLAG, "1")
        .env(LOG_PATH, &path)
        .status()
        .expect("必须启动 panic 子进程");

    assert!(!status.success(), "未捕获 panic 必须让子进程失败");
    let log = fs::read_to_string(path).expect("panic hook 必须写应急日志");
    assert_eq!(log.lines().count(), 1);
    assert!(log.contains("event=\"process_panicked\""));
    assert!(log.contains("process=\"core-test\""));
    assert!(log.contains("thread=\"main\""));
    assert!(log.contains("source_file=\""));
    assert!(log.contains("source_line="));
    assert!(log.contains("panic_message=\"panic-log-sentinel\""));
}

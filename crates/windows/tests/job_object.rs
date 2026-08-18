//! Worker 子进程必须随 Job Object 关闭而退出。

use std::{
    os::windows::{io::AsRawHandle, process::CommandExt},
    process::{Command, Stdio},
    thread,
    time::{Duration, Instant},
};

use dedup_windows::{CREATE_WORKER_FLAGS, WorkerJob};

#[test]
/// 真实启动无窗口 ping 子进程，验证关闭 Job 句柄触发系统级终止。
fn dropping_job_terminates_assigned_worker_process() {
    let job = WorkerJob::create().unwrap();
    let mut child = Command::new("ping.exe")
        .args(["-t", "127.0.0.1"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .creation_flags(CREATE_WORKER_FLAGS)
        .spawn()
        .unwrap();
    job.assign_raw_process_handle(child.as_raw_handle())
        .unwrap();

    drop(job);

    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if child.try_wait().unwrap().is_some() {
            break;
        }
        assert!(Instant::now() < deadline, "assigned process remained alive");
        thread::sleep(Duration::from_millis(20));
    }
}

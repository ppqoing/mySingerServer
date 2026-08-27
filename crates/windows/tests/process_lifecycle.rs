//! Windows 进程替代启动与父进程退出等待边界。

use std::{
    fs,
    process::{Command, Stdio},
    sync::mpsc,
    thread,
    time::Duration,
};

use dedup_windows::{spawn_replacement_node, wait_for_process_exit};

#[test]
fn wait_for_process_exit_blocks_until_the_process_really_exits() {
    let mut parent = spawn_long_running_process();
    let parent_pid = parent.id();
    let (completed, completion) = mpsc::sync_channel(1);
    let waiter = thread::spawn(move || {
        completed.send(wait_for_process_exit(parent_pid)).unwrap();
    });

    assert!(completion.recv_timeout(Duration::from_millis(150)).is_err());
    parent.kill().unwrap();
    parent.wait().unwrap();
    completion
        .recv_timeout(Duration::from_secs(2))
        .unwrap()
        .unwrap();
    waiter.join().unwrap();
}

#[test]
fn wait_for_process_exit_accepts_a_pid_that_already_disappeared() {
    let mut parent = Command::new("cmd.exe")
        .args(["/d", "/c", "exit", "0"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    let parent_pid = parent.id();
    parent.wait().unwrap();

    wait_for_process_exit(parent_pid).unwrap();
}

#[test]
fn spawn_replacement_node_passes_only_the_fixed_parent_argument() {
    let directory = tempfile::tempdir().unwrap();
    let script = directory.path().join("replacement.cmd");
    let arguments = directory.path().join("arguments.txt");
    fs::write(
        &script,
        format!(
            "@echo off\r\n>\"{}\" echo %*\r\nping -n 2 127.0.0.1 >nul\r\n",
            arguments.display()
        ),
    )
    .unwrap();

    let mut replacement = spawn_replacement_node(&script, 424_242).unwrap();
    for _ in 0..40 {
        if arguments.exists() {
            break;
        }
        thread::sleep(Duration::from_millis(25));
    }
    assert_eq!(
        fs::read_to_string(arguments).unwrap().trim(),
        "--wait-for-parent 424242"
    );
    replacement.wait().unwrap();
}

fn spawn_long_running_process() -> std::process::Child {
    Command::new("ping.exe")
        .args(["-t", "127.0.0.1"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap()
}

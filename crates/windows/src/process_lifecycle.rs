//! Node 替代进程启动与父进程退出等待的 Windows API 边界。

use std::{
    io,
    path::Path,
    process::{Child, Command},
};

use windows::{
    Win32::{
        Foundation::{CloseHandle, ERROR_INVALID_PARAMETER, GetLastError, WAIT_FAILED},
        System::Threading::{INFINITE, OpenProcess, PROCESS_SYNCHRONIZE, WaitForSingleObject},
    },
    core::HRESULT,
};

/// 启动指定 node.exe，并只传递等待当前父 PID 的固定参数。
pub fn spawn_replacement_node(executable: &Path, parent_pid: u32) -> io::Result<Child> {
    Command::new(executable)
        .arg("--wait-for-parent")
        .arg(parent_pid.to_string())
        .spawn()
}

/// 使用可同步进程句柄无限等待指定 PID 退出；PID 已不存在时立即成功。
pub fn wait_for_process_exit(pid: u32) -> io::Result<()> {
    let handle = match unsafe { OpenProcess(PROCESS_SYNCHRONIZE, false, pid) } {
        Ok(handle) => handle,
        Err(error) if error.code() == HRESULT::from_win32(ERROR_INVALID_PARAMETER.0) => {
            return Ok(());
        }
        Err(error) => return Err(io::Error::other(error.to_string())),
    };

    let wait = unsafe { WaitForSingleObject(handle, INFINITE) };
    let wait_error = (wait == WAIT_FAILED).then(|| unsafe { GetLastError() });
    unsafe { CloseHandle(handle) }.map_err(|error| io::Error::other(error.to_string()))?;
    if let Some(error) = wait_error {
        return Err(io::Error::from_raw_os_error(error.0 as i32));
    }
    Ok(())
}

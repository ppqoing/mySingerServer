//! 用 `KILL_ON_JOB_CLOSE` 约束节点创建的全部 Worker 子进程。

use std::{ffi::c_void, mem::size_of, os::windows::io::RawHandle};

use windows::{
    Win32::{
        Foundation::{CloseHandle, HANDLE},
        System::{
            JobObjects::{
                AssignProcessToJobObject, CreateJobObjectW, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
                JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JobObjectExtendedLimitInformation,
                SetInformationJobObject,
            },
            Threading::CREATE_NO_WINDOW,
        },
    },
    core::PCWSTR,
};

/// Worker 进程固定使用的 Windows 创建标志，禁止产生控制台窗口。
pub const CREATE_WORKER_FLAGS: u32 = CREATE_NO_WINDOW.0;

/// 节点持有的 Worker Job Object。
///
/// 关闭最后一个句柄时 Windows 会终止仍属于该 Job 的进程，因此节点崩溃或正常退出都
/// 不会遗留孤立 Worker。
#[derive(Debug)]
pub struct WorkerJob {
    handle: HANDLE,
}

// SAFETY: Windows Job HANDLE 是进程内可跨线程使用的内核句柄；本类型只执行线程安全的
// AssignProcessToJobObject，并在唯一所有者 Drop 时关闭一次。
unsafe impl Send for WorkerJob {}
// SAFETY: 与 Send 相同；共享引用不会关闭或改变句柄所有权。
unsafe impl Sync for WorkerJob {}

impl WorkerJob {
    /// 创建匿名 Job，并立即设置 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`。
    pub fn create() -> windows::core::Result<Self> {
        // SAFETY: 不传安全属性和名称，Windows 返回当前进程拥有的新句柄。
        let handle = unsafe { CreateJobObjectW(None, PCWSTR::null()) }?;
        let job = Self { handle };
        let mut information = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
        information.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
        // SAFETY: 指针指向完整、初始化的结构，长度与信息类别匹配。
        unsafe {
            SetInformationJobObject(
                job.handle,
                JobObjectExtendedLimitInformation,
                (&raw const information).cast::<c_void>(),
                size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            )
        }?;
        Ok(job)
    }

    /// 把一个仍在运行且由调用方持有句柄的子进程加入 Job。
    pub fn assign_raw_process_handle(&self, process: RawHandle) -> windows::core::Result<()> {
        // SAFETY: RawHandle 来自 Child/Process 的 AsRawHandle；本方法不取得或关闭其所有权。
        unsafe { AssignProcessToJobObject(self.handle, HANDLE(process)) }
    }
}

impl Drop for WorkerJob {
    fn drop(&mut self) {
        // SAFETY: handle 由 CreateJobObjectW 创建，只由本值关闭一次。
        let _ = unsafe { CloseHandle(self.handle) };
    }
}

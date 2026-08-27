//! Windows Shell 文件操作边界：回收站删除和资源管理器打开目录。

use std::{io, os::windows::ffi::OsStrExt, path::Path, process::Command};

use windows::{
    Win32::{
        System::Com::{
            CLSCTX_INPROC_SERVER, COINIT_APARTMENTTHREADED, CoCreateInstance, CoInitializeEx,
            CoUninitialize, IBindCtx,
        },
        UI::Shell::{
            FOF_ALLOWUNDO, FOF_NOCONFIRMATION, FOF_NOERRORUI, FOF_SILENT, FOFX_RECYCLEONDELETE,
            FileOperation, IFileOperation, IFileOperationProgressSink, IShellItem,
            SHCreateItemFromParsingName,
        },
    },
    core::PCWSTR,
};

/// 使用独立 STA 线程把一个文件交给 Windows 回收站。
///
/// 独立线程让调用方不必了解当前 Tokio 或 UI 线程的 COM apartment 状态。
pub fn move_to_recycle_bin(path: &Path) -> io::Result<()> {
    let owned = path.to_path_buf();
    std::thread::spawn(move || recycle_on_sta(&owned))
        .join()
        .map_err(|_| io::Error::other("Windows 回收站线程异常退出"))?
}

/// 让 Windows 资源管理器打开给定目录，供托盘“打开日志目录”使用。
pub fn open_folder(path: &Path) -> io::Result<()> {
    Command::new("explorer.exe").arg(path).spawn()?;
    Ok(())
}

fn recycle_on_sta(path: &Path) -> io::Result<()> {
    // SAFETY: 当前函数运行在刚创建的线程上，使用固定 STA 初始化并在作用域结束时配对释放。
    unsafe { CoInitializeEx(None, COINIT_APARTMENTTHREADED) }
        .ok()
        .map_err(shell_error)?;
    let _apartment = ComApartment;
    let wide = path
        .as_os_str()
        .encode_wide()
        .chain(Some(0))
        .collect::<Vec<_>>();
    // SAFETY: wide 在调用期间以 NUL 结尾且保持有效；COM 已在当前线程初始化。
    let item: IShellItem =
        unsafe { SHCreateItemFromParsingName(PCWSTR(wide.as_ptr()), None::<&IBindCtx>) }
            .map_err(shell_error)?;
    // SAFETY: FileOperation 是进程内 COM 类，接口类型与 CLSID 匹配。
    let operation: IFileOperation =
        unsafe { CoCreateInstance(&FileOperation, None, CLSCTX_INPROC_SERVER) }
            .map_err(shell_error)?;
    // FOF_ALLOWUNDO 保留旧系统兼容语义；Windows 8+ 以 FOFX_RECYCLEONDELETE 明确禁止
    // IFileOperation 在静默模式下把 DeleteItem 降级为永久删除。
    let flags =
        FOF_ALLOWUNDO | FOFX_RECYCLEONDELETE | FOF_NOCONFIRMATION | FOF_NOERRORUI | FOF_SILENT;
    // SAFETY: item 和 operation 均为当前 STA 中的有效 COM 接口；不注册进度回调。
    unsafe {
        operation.SetOperationFlags(flags).map_err(shell_error)?;
        operation
            .DeleteItem(&item, None::<&IFileOperationProgressSink>)
            .map_err(shell_error)?;
        operation.PerformOperations().map_err(shell_error)?;
        if operation
            .GetAnyOperationsAborted()
            .map_err(shell_error)?
            .as_bool()
        {
            return Err(io::Error::other("Windows 取消了回收站操作"));
        }
    }
    Ok(())
}

fn shell_error(error: windows::core::Error) -> io::Error {
    io::Error::other(error.to_string())
}

struct ComApartment;

impl Drop for ComApartment {
    fn drop(&mut self) {
        // SAFETY: 只在 CoInitializeEx 成功后创建，并在同一线程释放一次。
        unsafe { CoUninitialize() };
    }
}

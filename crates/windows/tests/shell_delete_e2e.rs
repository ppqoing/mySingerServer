//! Windows 回收站与永久删除的显式运行验收。
//!
//! 测试只接受仓库 `tests/.runtime-delete-fixtures` 下已经存在的两个文件；路径由环境变量
//! 传入，因此普通工作区测试不会创建或删除用户文件。

#![cfg(windows)]

use std::{
    env, fs,
    iter::once,
    mem::size_of,
    os::windows::ffi::OsStrExt,
    path::{Path, PathBuf},
};

use dedup_windows::move_to_recycle_bin;
use windows::{
    Win32::UI::Shell::{SHQUERYRBINFO, SHQueryRecycleBinW},
    core::PCWSTR,
};

/// 把一个专用夹具交给 Windows 回收站，并永久删除另一个专用夹具。
#[test]
#[ignore = "requires DEDUP_TEST_RECYCLE_FILE and DEDUP_TEST_PERMANENT_FILE"]
fn recycle_bin_and_permanent_delete_have_distinct_windows_outcomes() {
    let allowed_root = repository_root()
        .join("tests")
        .join(".runtime-delete-fixtures")
        .canonicalize()
        .expect("删除验收目录必须由调用方预先创建");
    let recycle = fixture_path("DEDUP_TEST_RECYCLE_FILE", &allowed_root);
    let permanent = fixture_path("DEDUP_TEST_PERMANENT_FILE", &allowed_root);
    assert_ne!(recycle, permanent, "两个删除模式必须使用不同夹具");

    let before = recycle_item_count(&recycle);
    move_to_recycle_bin(&recycle).expect("IFileOperation 应把夹具移入 Windows 回收站");
    let after_recycle = recycle_item_count(&recycle);
    assert_eq!(after_recycle, before + 1, "回收站模式必须增加一个项目");
    fs::remove_file(&permanent).expect("永久删除应直接移除专用夹具");
    assert_eq!(
        recycle_item_count(&permanent),
        after_recycle,
        "永久删除不得增加回收站项目"
    );

    assert!(!recycle.exists(), "回收站模式后原路径必须消失");
    assert!(!permanent.exists(), "永久删除后原路径必须消失");
    println!("RECYCLED_FIXTURE={}", recycle.display());
    println!("PERMANENT_FIXTURE={}", permanent.display());
}

/// 读取、规范化并限制一个本轮删除夹具路径。
fn fixture_path(variable: &str, allowed_root: &Path) -> PathBuf {
    let path = PathBuf::from(env::var_os(variable).expect("必须显式提供删除夹具路径"));
    assert!(path.is_absolute(), "删除夹具必须使用绝对显示路径");
    let canonical = path.canonicalize().expect("删除夹具必须已经存在");
    assert_eq!(
        canonical.parent(),
        Some(allowed_root),
        "删除夹具必须直接位于专用目录"
    );
    assert!(canonical.is_file(), "删除夹具必须是普通文件");
    // `canonicalize` 在 Windows 返回 `\\?\` 形式；它只用于白名单判定，Shell 接口继续接收
    // 用户可见绝对路径，与节点保存的 DisplayPath 边界保持一致。
    path
}

/// 查询夹具所在卷的 Windows 回收站项目数，验证 Shell 成功码对应真实回收语义。
fn recycle_item_count(path: &Path) -> i64 {
    assert!(path.is_absolute(), "回收站查询必须来自绝对夹具路径");
    let volume_root = path.ancestors().last().expect("绝对路径必须包含卷根");
    let volume_root = volume_root
        .as_os_str()
        .encode_wide()
        .chain(once(0))
        .collect::<Vec<_>>();
    let mut info = SHQUERYRBINFO {
        cbSize: size_of::<SHQUERYRBINFO>() as u32,
        ..Default::default()
    };
    // SAFETY: root 是以 NUL 结尾且在调用期间有效的卷根；info 具有官方结构大小和可写生命周期。
    unsafe { SHQueryRecycleBinW(PCWSTR(volume_root.as_ptr()), &mut info) }
        .expect("应能查询夹具所在卷的 Windows 回收站");
    info.i64NumItems
}

/// 从当前 crate 清单目录定位工作区根，不依赖启动时当前目录。
fn repository_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("..")
        .canonicalize()
        .expect("应能定位工作区根目录")
}

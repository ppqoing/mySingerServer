//! Windows DLL 固定相对目录、白名单顺序和搜索隔离测试。

#![cfg(windows)]

use std::{
    env,
    path::{Path, PathBuf},
    sync::Mutex,
};

use dedup_media_ffmpeg::{Ffmpeg, dll_directory, required_dlls};

static PROCESS_ENVIRONMENT: Mutex<()> = Mutex::new(());

#[test]
fn loader_uses_worker_relative_runtime_directory() {
    let worker = Path::new(r"C:\App\worker.exe");
    assert_eq!(
        dll_directory(worker).unwrap(),
        Path::new(r"C:\App\runtime\ffmpeg")
    );
    assert_eq!(
        required_dlls(),
        &[
            "avutil-60.dll",
            "swresample-6.dll",
            "swscale-9.dll",
            "avcodec-62.dll",
            "avformat-62.dll"
        ]
    );
}

#[test]
fn loader_ignores_current_directory_and_path() {
    let Some(source) = test_source() else {
        return;
    };
    let _lock = PROCESS_ENVIRONMENT.lock().unwrap();
    let fixture = runtime_fixture(&source, None);
    let old_directory = env::current_dir().unwrap();
    let old_path = env::var_os("PATH");
    let unrelated = tempfile::tempdir().unwrap();
    env::set_current_dir(unrelated.path()).unwrap();
    // SAFETY: 本测试用全局互斥锁串行修改进程环境，并在加载调用后立即恢复。
    unsafe { env::set_var("PATH", "") };

    let loaded = Ffmpeg::load_from_worker_executable(&fixture.worker);

    env::set_current_dir(old_directory).unwrap();
    match old_path {
        // SAFETY: 同一互斥区内恢复测试前的原值。
        Some(value) => unsafe { env::set_var("PATH", value) },
        // SAFETY: 同一互斥区内恢复测试前的缺失状态。
        None => unsafe { env::remove_var("PATH") },
    }
    loaded.unwrap();
}

#[test]
fn loader_reports_the_exact_missing_dll() {
    let Some(source) = test_source() else {
        return;
    };
    let fixture = runtime_fixture(&source, Some("swscale-9.dll"));
    let error = Ffmpeg::load_from_worker_executable(&fixture.worker).unwrap_err();
    assert!(error.to_string().contains("swscale-9.dll"));
}

fn test_source() -> Option<PathBuf> {
    env::var_os("DEDUP_FFMPEG_TEST_SOURCE_DIR")
        .map(PathBuf::from)
        .or_else(|| {
            eprintln!("skipped: DEDUP_FFMPEG_TEST_SOURCE_DIR is not set");
            None
        })
}

struct RuntimeFixture {
    _directory: tempfile::TempDir,
    worker: PathBuf,
}

fn runtime_fixture(source: &Path, omitted: Option<&str>) -> RuntimeFixture {
    let directory = tempfile::tempdir().unwrap();
    let worker = directory.path().join("worker.exe");
    let runtime = directory.path().join("runtime").join("ffmpeg");
    std::fs::create_dir_all(&runtime).unwrap();
    std::fs::write(&worker, []).unwrap();
    for name in required_dlls() {
        if Some(*name) != omitted {
            std::fs::copy(source.join(name), runtime.join(name)).unwrap();
        }
    }
    RuntimeFixture {
        _directory: directory,
        worker,
    }
}

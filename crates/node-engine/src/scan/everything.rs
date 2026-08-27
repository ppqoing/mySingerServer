//! Everything 1.4/1.5 Window Message IPC 枚举、同会话启动与就绪等待。

use std::{
    env, future,
    path::{Path, PathBuf},
    process::Command,
    time::Duration,
};

use dedup_core::{DisplayPath, NormalizedPath};
use dedup_node_store::ScannedPath;
use everything_ipc::wm::{EverythingClient, RequestFlags};

use super::{FileEnumerator, ScanError};

const EVERYTHING_READY_ATTEMPTS: usize = 120;
const EVERYTHING_READY_INTERVAL: Duration = Duration::from_millis(250);

/// 用户明确选择 Everything 时使用的枚举器；不可用会直接返回错误。
#[derive(Clone, Copy, Debug, Default)]
pub struct EverythingEnumerator;

impl FileEnumerator for EverythingEnumerator {
    fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        let client = EverythingClient::new()
            .map_err(|error| ScanError::Enumeration(format!("Everything IPC 不可用: {error}")))?;
        let mut rows = Vec::new();
        for root in roots {
            let normalized_root = NormalizedPath::new(root.as_path())
                .map_err(|error| ScanError::InvalidResult(error.to_string()))?;
            let query = format!(r#"file: path:"{}""#, root.as_path().display());
            let list = client
                .query_wait(&query)
                .request_flags(RequestFlags::FullPathAndFileName | RequestFlags::Size)
                .call()
                .map_err(|error| ScanError::Enumeration(format!("Everything 查询失败: {error}")))?;
            for item in list.iter() {
                let path = item
                    .get_string(RequestFlags::FullPathAndFileName)
                    .map(PathBuf::from)
                    .ok_or_else(|| ScanError::InvalidResult("Everything 缺少完整路径".into()))?;
                let normalized = NormalizedPath::new(&path)
                    .map_err(|error| ScanError::InvalidResult(error.to_string()))?;
                if !normalized.is_within(&normalized_root) {
                    continue;
                }
                rows.push(ScannedPath::new(
                    normalized,
                    DisplayPath::new(path)
                        .map_err(|error| ScanError::InvalidResult(error.to_string()))?,
                    item.get_size(RequestFlags::Size).ok_or_else(|| {
                        ScanError::InvalidResult("Everything 缺少文件大小".into())
                    })?,
                ));
            }
        }
        rows.sort_by(|left, right| left.normalized_path.cmp(&right.normalized_path));
        rows.dedup_by(|left, right| left.normalized_path == right.normalized_path);
        Ok(rows)
    }

    /// Everything 已返回完整去重清单后先冻结总数，再受有界下游背压逐项交付。
    fn enumerate_into_with_completion(
        &self,
        roots: &[DisplayPath],
        complete: &mut dyn FnMut(Option<(u64, u64)>) -> Result<(), ScanError>,
        emit: &mut dyn FnMut(ScannedPath) -> Result<(), ScanError>,
    ) -> Result<(), ScanError> {
        emit_materialized_rows(self.enumerate(roots)?, complete, emit)
    }
}

/// Everything 在首次完整枚举失败时整次回退到 Windows Walker，不混合两种结果。
#[derive(Clone, Copy, Debug, Default)]
pub(crate) struct PreferredEverythingEnumerator;

impl FileEnumerator for PreferredEverythingEnumerator {
    fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        enumerate_preferred_with(&EverythingEnumerator, &dedup_windows::WindowsWalker, roots)
    }

    /// Everything 或整次 Walker 回退先形成稳定清单，再独立报告枚举完成。
    fn enumerate_into_with_completion(
        &self,
        roots: &[DisplayPath],
        complete: &mut dyn FnMut(Option<(u64, u64)>) -> Result<(), ScanError>,
        emit: &mut dyn FnMut(ScannedPath) -> Result<(), ScanError>,
    ) -> Result<(), ScanError> {
        emit_materialized_rows(self.enumerate(roots)?, complete, emit)
    }
}

/// 把已完成的稳定清单与后续有界交付拆成两个生命周期边界。
fn emit_materialized_rows(
    rows: Vec<ScannedPath>,
    complete: &mut dyn FnMut(Option<(u64, u64)>) -> Result<(), ScanError>,
    emit: &mut dyn FnMut(ScannedPath) -> Result<(), ScanError>,
) -> Result<(), ScanError> {
    let total_files = rows.len() as u64;
    let total_bytes = rows
        .iter()
        .fold(0_u64, |total, row| total.saturating_add(row.file_size));
    complete(Some((total_files, total_bytes)))?;
    for row in rows {
        emit(row)?;
    }
    Ok(())
}

fn enumerate_preferred_with<E, W>(
    everything: &E,
    walker: &W,
    roots: &[DisplayPath],
) -> Result<Vec<ScannedPath>, ScanError>
where
    E: FileEnumerator + ?Sized,
    W: FileEnumerator + ?Sized,
{
    match everything.enumerate(roots) {
        Ok(rows) => Ok(rows),
        Err(error) => {
            tracing::warn!(error = %error, "Everything 枚举失败，整次回退 Windows Walker");
            walker.enumerate(roots)
        }
    }
}

/// 由收到 CreateScan 的 node.exe 启动同目录 Everything，并等待 IPC 数据库就绪。
pub(crate) async fn ensure_everything_ready() -> bool {
    let executable = match env::current_exe()
        .ok()
        .and_then(|path| path.parent().map(|parent| parent.join("Everything.exe")))
    {
        Some(path) => path,
        None => {
            tracing::warn!("无法解析 node.exe 所在目录，回退 Windows Walker");
            return false;
        }
    };
    let ready = ensure_everything_ready_with_policy(
        everything_is_ready,
        || start_everything(&executable),
        tokio::time::sleep,
    )
    .await;
    if ready {
        tracing::info!(path = %executable.display(), "Everything IPC 与数据库已经就绪");
    } else {
        tracing::warn!(path = %executable.display(), "Everything 启动或初始化失败，回退 Windows Walker");
    }
    ready
}

fn everything_is_ready() -> bool {
    EverythingClient::new().is_ok_and(|client| client.is_ipc_available() && client.is_db_loaded())
}

fn start_everything(executable: &Path) -> std::io::Result<()> {
    tracing::info!(path = %executable.display(), "启动同会话 Everything 客户端");
    Command::new(executable)
        .arg("-startup")
        .spawn()
        .map(|_| ())
        .inspect_err(|error| {
            tracing::warn!(path = %executable.display(), error = %error, "启动 Everything 失败");
        })
}

async fn ensure_everything_ready_with_policy<Probe, Start, Wait, WaitFuture>(
    probe: Probe,
    start: Start,
    mut wait: Wait,
) -> bool
where
    Probe: FnMut() -> bool,
    Start: FnMut() -> std::io::Result<()>,
    Wait: FnMut(Duration) -> WaitFuture,
    WaitFuture: future::Future<Output = ()>,
{
    ensure_everything_ready_with(
        probe,
        start,
        || wait(EVERYTHING_READY_INTERVAL),
        EVERYTHING_READY_ATTEMPTS,
    )
    .await
}

async fn ensure_everything_ready_with<Probe, Start, Wait, WaitFuture>(
    mut probe: Probe,
    mut start: Start,
    mut wait: Wait,
    attempts: usize,
) -> bool
where
    Probe: FnMut() -> bool,
    Start: FnMut() -> std::io::Result<()>,
    Wait: FnMut() -> WaitFuture,
    WaitFuture: future::Future<Output = ()>,
{
    if probe() {
        return true;
    }
    if start().is_err() {
        return false;
    }
    for _ in 0..attempts {
        wait().await;
        if probe() {
            return true;
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use std::{cell::Cell, future};

    use dedup_core::{DisplayPath, NormalizedPath};
    use dedup_node_store::ScannedPath;

    use super::{
        FileEnumerator, ScanError, ensure_everything_ready_with,
        ensure_everything_ready_with_policy, enumerate_preferred_with,
    };

    struct FailingEverything<'a>(&'a Cell<usize>);

    impl FileEnumerator for FailingEverything<'_> {
        fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
            self.0.set(self.0.get() + 1);
            Err(ScanError::Enumeration("Everything 查询失败".into()))
        }
    }

    struct RecordingWalker<'a>(&'a Cell<usize>);

    impl FileEnumerator for RecordingWalker<'_> {
        fn enumerate(&self, roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
            self.0.set(self.0.get() + 1);
            Ok(vec![ScannedPath::new(
                NormalizedPath::new(roots[0].as_path()).unwrap(),
                roots[0].clone(),
                17,
            )])
        }
    }

    #[tokio::test]
    async fn ready_everything_is_reused_without_starting_another_process() {
        let launches = Cell::new(0);

        let ready = ensure_everything_ready_with(
            || true,
            || {
                launches.set(launches.get() + 1);
                Ok(())
            },
            || future::ready(()),
            3,
        )
        .await;

        assert!(ready);
        assert_eq!(launches.get(), 0);
    }

    #[tokio::test]
    async fn unavailable_everything_is_started_once_and_waited_until_ready() {
        let probes = Cell::new(0);
        let launches = Cell::new(0);
        let waits = Cell::new(0);

        let ready = ensure_everything_ready_with(
            || {
                let attempt = probes.get() + 1;
                probes.set(attempt);
                attempt >= 3
            },
            || {
                launches.set(launches.get() + 1);
                Ok(())
            },
            || {
                waits.set(waits.get() + 1);
                future::ready(())
            },
            4,
        )
        .await;

        assert!(ready);
        assert_eq!(launches.get(), 1);
        assert_eq!(probes.get(), 3);
        assert_eq!(waits.get(), 2);
    }

    #[tokio::test]
    async fn start_failure_or_readiness_timeout_selects_windows_walker() {
        let start_failure = ensure_everything_ready_with(
            || false,
            || Err(std::io::Error::new(std::io::ErrorKind::NotFound, "missing")),
            || future::ready(()),
            3,
        )
        .await;
        assert!(!start_failure);

        let launches = Cell::new(0);
        let waits = Cell::new(0);
        let timeout = ensure_everything_ready_with(
            || false,
            || {
                launches.set(launches.get() + 1);
                Ok(())
            },
            || {
                waits.set(waits.get() + 1);
                future::ready(())
            },
            3,
        )
        .await;

        assert!(!timeout);
        assert_eq!(launches.get(), 1);
        assert_eq!(waits.get(), 3);
    }

    #[tokio::test]
    async fn production_readiness_policy_waits_120_times_at_250_milliseconds() {
        let waits = Cell::new(0);

        let ready = ensure_everything_ready_with_policy(
            || false,
            || Ok(()),
            |interval| {
                assert_eq!(interval, std::time::Duration::from_millis(250));
                waits.set(waits.get() + 1);
                future::ready(())
            },
        )
        .await;

        assert!(!ready);
        assert_eq!(waits.get(), 120);
    }

    #[test]
    fn failed_everything_enumeration_restarts_the_whole_scan_with_windows_walker() {
        let directory = tempfile::tempdir().unwrap();
        let root = DisplayPath::new(directory.path()).unwrap();
        let everything_calls = Cell::new(0);
        let walker_calls = Cell::new(0);

        let rows = enumerate_preferred_with(
            &FailingEverything(&everything_calls),
            &RecordingWalker(&walker_calls),
            &[root],
        )
        .unwrap();

        assert_eq!(everything_calls.get(), 1);
        assert_eq!(walker_calls.get(), 1);
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0].file_size, 17);
    }
}

//! 可取消分块读取的重试上限、故障身份和流式 MD5 行为测试。

use std::{
    collections::VecDeque,
    fs,
    path::{Path, PathBuf},
    sync::{Arc, Condvar, Mutex},
    thread,
    time::Duration,
};

use dedup_core::NodeConfig;
use dedup_node_engine::{
    io::{BlockReadError, BlockReader, ReadFailure, RetryingFileReader},
    scan::md5_file,
};
use dedup_windows::ReadCancellationToken;

#[derive(Clone, Debug)]
enum Action {
    Timeout(Option<i32>),
    Read,
    ReadLimit(usize),
    Pending(Arc<ManualPending>, PendingCompletion),
}

#[derive(Clone, Copy, Debug)]
enum PendingCompletion {
    Timeout(Option<i32>),
    Read,
}

#[derive(Debug, Default)]
struct ManualPending {
    state: Mutex<(bool, bool)>,
    entered: Condvar,
    released: Condvar,
}

impl ManualPending {
    fn wait_until_entered(&self) {
        let mut state = self.state.lock().unwrap();
        while !state.0 {
            state = self.entered.wait(state).unwrap();
        }
    }

    fn complete(&self) {
        let mut state = self.state.lock().unwrap();
        state.1 = true;
        self.released.notify_all();
    }

    fn wait_for_completion(&self) {
        let mut state = self.state.lock().unwrap();
        state.0 = true;
        self.entered.notify_all();
        while !state.1 {
            state = self.released.wait(state).unwrap();
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct ReadCall {
    path: PathBuf,
    offset: u64,
    len: usize,
    timeout: Duration,
}

#[derive(Clone)]
struct FakeBlockReader {
    data: Arc<Vec<u8>>,
    actions: Arc<Mutex<VecDeque<Action>>>,
    calls: Arc<Mutex<Vec<ReadCall>>>,
}

impl FakeBlockReader {
    fn new(data: Vec<u8>, actions: impl IntoIterator<Item = Action>) -> Self {
        Self {
            data: Arc::new(data),
            actions: Arc::new(Mutex::new(actions.into_iter().collect())),
            calls: Arc::new(Mutex::new(Vec::new())),
        }
    }

    fn calls(&self) -> Vec<ReadCall> {
        self.calls.lock().unwrap().clone()
    }

    fn copy_at(&self, offset: u64, buffer: &mut [u8]) -> usize {
        let start = usize::try_from(offset).unwrap();
        if start >= self.data.len() {
            return 0;
        }
        let len = buffer.len().min(self.data.len() - start);
        buffer[..len].copy_from_slice(&self.data[start..start + len]);
        len
    }
}

impl BlockReader for FakeBlockReader {
    fn read_at(
        &self,
        path: &Path,
        offset: u64,
        buffer: &mut [u8],
        timeout: Duration,
        cancellation: &ReadCancellationToken,
    ) -> Result<usize, BlockReadError> {
        self.calls.lock().unwrap().push(ReadCall {
            path: path.to_path_buf(),
            offset,
            len: buffer.len(),
            timeout,
        });
        match self
            .actions
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(Action::Read)
        {
            Action::Timeout(raw_os_error) => Err(BlockReadError::Timeout { raw_os_error }),
            Action::Read => Ok(self.copy_at(offset, buffer)),
            Action::ReadLimit(limit) => {
                let limit = limit.min(buffer.len());
                Ok(self.copy_at(offset, &mut buffer[..limit]))
            }
            Action::Pending(pending, completion) => {
                pending.wait_for_completion();
                if cancellation.is_cancelled() {
                    return Err(BlockReadError::Cancelled);
                }
                match completion {
                    PendingCompletion::Timeout(raw_os_error) => {
                        Err(BlockReadError::Timeout { raw_os_error })
                    }
                    PendingCompletion::Read => Ok(self.copy_at(offset, buffer)),
                }
            }
        }
    }
}

fn media_file(data: &[u8]) -> (tempfile::TempDir, PathBuf) {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("media.bin");
    fs::write(&path, data).unwrap();
    (directory, path)
}

#[test]
fn second_attempt_success_returns_md5_without_a_physical_fault() {
    let data = b"second attempt succeeds".repeat(97);
    let (_directory, path) = media_file(&data);
    let fake = FakeBlockReader::new(data, [Action::Timeout(Some(121)), Action::Read]);
    let calls = fake.clone();
    let reader = RetryingFileReader::new(fake, &NodeConfig::default()).unwrap();

    let actual = reader
        .read_file_md5(&path, &ReadCancellationToken::new())
        .unwrap();

    assert_eq!(actual, md5_file(&path).unwrap());
    let calls = calls.calls();
    assert_eq!(calls.len(), 2);
    assert!(
        calls
            .iter()
            .all(|call| call.timeout == Duration::from_secs(3))
    );
}

#[test]
fn third_timeout_becomes_suspected_physical_with_exact_block_identity() {
    let data = b"three timeouts".repeat(113);
    let expected_size = data.len() as u64;
    let (_directory, path) = media_file(&data);
    let pending = [
        Arc::new(ManualPending::default()),
        Arc::new(ManualPending::default()),
        Arc::new(ManualPending::default()),
    ];
    let fake = FakeBlockReader::new(
        data,
        [
            Action::Pending(pending[0].clone(), PendingCompletion::Timeout(Some(121))),
            Action::Pending(pending[1].clone(), PendingCompletion::Timeout(Some(23))),
            Action::Pending(pending[2].clone(), PendingCompletion::Timeout(Some(1117))),
        ],
    );
    let calls = fake.clone();
    let reader = RetryingFileReader::new(fake, &NodeConfig::default()).unwrap();
    let task_path = path.clone();
    let task = thread::spawn(move || {
        reader
            .read_file_md5(&task_path, &ReadCancellationToken::new())
            .unwrap_err()
    });
    for attempt in &pending {
        attempt.wait_until_entered();
        attempt.complete();
    }
    let error = task.join().unwrap();

    assert!(matches!(
        &error,
        ReadFailure::SuspectedPhysical {
            path: failed_path,
            file_size,
            block_offset: 0,
            block_len,
            raw_os_error: Some(1117),
        } if failed_path == &path
            && *file_size == expected_size
            && *block_len == expected_size as usize
    ));
    assert_eq!(calls.calls().len(), 3);
    assert!(error.to_string().contains("疑似物理读取故障"));
    assert!(!error.to_string().contains("已确认损坏"));
}

#[test]
fn cancellation_of_a_pending_block_does_not_start_the_next_block() {
    let mut config = NodeConfig::default();
    config.read.block_size_bytes = 64 * 1024;
    let data = vec![0x5a; config.read.block_size_bytes + 17];
    let (_directory, path) = media_file(&data);
    let pending = Arc::new(ManualPending::default());
    let fake = FakeBlockReader::new(
        data,
        [Action::Pending(pending.clone(), PendingCompletion::Read)],
    );
    let calls = fake.clone();
    let cancellation = ReadCancellationToken::new();
    let reader = RetryingFileReader::new(fake, &config).unwrap();
    let task_path = path.clone();
    let task_cancellation = cancellation.clone();
    let task = thread::spawn(move || reader.read_file_md5(&task_path, &task_cancellation));
    pending.wait_until_entered();
    cancellation.cancel();
    pending.complete();
    let error = task.join().unwrap().unwrap_err();

    assert!(matches!(error, ReadFailure::Cancelled));
    assert!(cancellation.is_cancelled());
    assert_eq!(calls.calls().len(), 1);
}

#[test]
fn configured_blocks_produce_the_same_md5_as_the_existing_streaming_reader() {
    let mut config = NodeConfig::default();
    config.read.block_size_bytes = 64 * 1024;
    let data = (0..(config.read.block_size_bytes * 2 + 17))
        .map(|index| (index % 251) as u8)
        .collect::<Vec<_>>();
    let (_directory, path) = media_file(&data);
    let fake = FakeBlockReader::new(data, [Action::ReadLimit(7)]);
    let calls = fake.clone();
    let reader = RetryingFileReader::new(fake, &config).unwrap();

    let actual = reader
        .read_file_md5(&path, &ReadCancellationToken::new())
        .unwrap();

    assert_eq!(actual, md5_file(&path).unwrap());
    let calls = calls.calls();
    assert_eq!(calls.len(), 4);
    assert_eq!(calls[1].offset, 7);
    assert_eq!(calls[3].len, 17);
}

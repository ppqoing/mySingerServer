use std::{
    collections::BTreeMap,
    fs, io,
    path::Path,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use dedup_core::{DiskReadConfig, DisplayPath, MachineId, MediaKind, NormalizedPath};
use dedup_node_engine::{
    io::{BlockReadError, ReadFailure},
    scan::{
        FileEnumerator, PipelineFileReader, ResolvedScanRootStorage, ScanDiskPlan, ScanEngine,
        ScanError, ScanOptions, ScanRootStorageResolver, ScheduledFileReader, Stage1Processor,
        Stage1Request, SystemMd5,
    },
    worker::Stage1Output,
};
use dedup_node_store::{NodeStore, ScannedPath, TaskStatus};
use dedup_windows::{LocalDiskKind, PhysicalDiskId};

/// 记录根解析和枚举的实际先后，防止枚举先于物理盘计划建立。
#[derive(Clone, Default)]
struct TraceResolver {
    trace: Arc<Mutex<Vec<String>>>,
    locations: Arc<BTreeMap<String, (Vec<u32>, LocalDiskKind)>>,
    failure: Option<String>,
}

impl TraceResolver {
    fn new(locations: impl IntoIterator<Item = (&'static str, (Vec<u32>, LocalDiskKind))>) -> Self {
        Self {
            trace: Arc::new(Mutex::new(Vec::new())),
            locations: Arc::new(
                locations
                    .into_iter()
                    .map(|(path, location)| (path.to_owned(), location))
                    .collect(),
            ),
            failure: None,
        }
    }

    fn failing(path: &'static str) -> Self {
        Self {
            trace: Arc::new(Mutex::new(Vec::new())),
            locations: Arc::new(BTreeMap::new()),
            failure: Some(path.to_owned()),
        }
    }

    fn trace(&self) -> Vec<String> {
        self.trace.lock().unwrap().clone()
    }
}

impl ScanRootStorageResolver for TraceResolver {
    fn resolve(&self, root: &Path) -> io::Result<ResolvedScanRootStorage> {
        let display = root.to_string_lossy().to_string();
        self.trace
            .lock()
            .unwrap()
            .push(format!("resolve:{}", display.chars().next().unwrap_or('?')));
        if self.failure.as_deref() == Some(display.as_str()) {
            return Err(io::Error::other("resolver fixture failure"));
        }
        let (numbers, kind) = self
            .locations
            .get(&display)
            .cloned()
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "missing fixture root"))?;
        Ok(ResolvedScanRootStorage {
            normalized_root: NormalizedPath::new(root).unwrap(),
            physical_disk_id: PhysicalDiskId::from_disk_numbers(numbers).unwrap(),
            disk_kind: kind,
        })
    }
}

fn root(path: &str) -> DisplayPath {
    DisplayPath::new(path).unwrap()
}

fn row(path: &str) -> ScannedPath {
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        1,
    )
}

fn config(hdd: usize, ssd: usize, unknown: usize) -> DiskReadConfig {
    DiskReadConfig {
        hdd_threads_per_disk: hdd,
        ssd_threads_per_disk: ssd,
        unknown_threads_per_disk: unknown,
        total_threads: hdd.max(ssd).max(unknown),
        ..DiskReadConfig::default()
    }
}

struct Rows(Vec<ScannedPath>);

impl FileEnumerator for Rows {
    fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Ok(self.0.clone())
    }
}

struct FailingEnumerator;

impl FileEnumerator for FailingEnumerator {
    fn enumerate(&self, _roots: &[DisplayPath]) -> Result<Vec<ScannedPath>, ScanError> {
        Err(ScanError::Enumeration("fixture failure".into()))
    }
}

struct OtherProcessor;

impl Stage1Processor for OtherProcessor {
    async fn process(&mut self, _request: Stage1Request) -> Result<Stage1Output, String> {
        Ok(Stage1Output {
            media_kind: MediaKind::Other,
            width: 0,
            height: 0,
            duration_ms: None,
            frames: Vec::new(),
            contact_sheet_jpeg: None,
        })
    }
}

#[tokio::test]
async fn successful_partial_root_scan_deactivates_only_exact_component_boundary() {
    let machine = MachineId::parse(&"33".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    seed(&mut store, r"D:\A\old.jpg", b"old-a");
    seed(&mut store, r"D:\B\keep.jpg", b"keep-b");
    seed(&mut store, r"D:\AB\keep.jpg", b"keep-ab");
    let mut engine = ScanEngine::new(Rows(Vec::new()), SystemMd5, Path::new(r"D:\cache"));
    let mut processor = OtherProcessor;

    let result = engine
        .run(
            &mut store,
            ScanOptions::new(vec![DisplayPath::new(r"D:\A").unwrap()]),
            &mut processor,
            20,
        )
        .await
        .unwrap();

    assert!(
        !store
            .is_location_active(&NormalizedPath::new(r"D:\A\old.jpg").unwrap())
            .unwrap()
    );
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\B\keep.jpg").unwrap())
            .unwrap()
    );
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\AB\keep.jpg").unwrap())
            .unwrap()
    );
    assert!(result.outbox_high_seq > 0);
    assert_eq!(
        store.task_snapshot(result.task_id).unwrap().status,
        TaskStatus::Completed
    );
    assert_eq!(result.outbox_high_seq, store.outbox_high_seq().unwrap());
}

#[tokio::test]
async fn failed_enumeration_keeps_all_existing_locations_active() {
    let machine = MachineId::parse(&"44".repeat(32)).unwrap();
    let mut store = NodeStore::open_in_memory(machine).unwrap();
    seed(&mut store, r"D:\A\old.jpg", b"old-a");
    let before = store.outbox_high_seq().unwrap();
    let mut engine = ScanEngine::new(FailingEnumerator, SystemMd5, Path::new(r"D:\cache"));
    let mut processor = OtherProcessor;

    let error = engine
        .run(
            &mut store,
            ScanOptions::new(vec![DisplayPath::new(r"D:\A").unwrap()]),
            &mut processor,
            21,
        )
        .await
        .unwrap_err();

    assert!(matches!(error, ScanError::Enumeration(_)));
    assert!(
        store
            .is_location_active(&NormalizedPath::new(r"D:\A\old.jpg").unwrap())
            .unwrap()
    );
    assert_eq!(store.outbox_high_seq().unwrap(), before);
}

fn seed(store: &mut NodeStore, path: &str, bytes: &[u8]) {
    let md5 = dedup_node_engine::scan::md5_bytes(bytes);
    let scanned = ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        bytes.len() as u64,
    );
    store
        .upsert_content_and_location(&scanned, md5, MediaKind::Other)
        .unwrap();
}

#[allow(dead_code)]
fn make_file(path: &Path, bytes: &[u8]) -> ScannedPath {
    fs::write(path, bytes).unwrap();
    ScannedPath::new(
        NormalizedPath::new(path).unwrap(),
        DisplayPath::new(path).unwrap(),
        bytes.len() as u64,
    )
}

#[test]
fn physical_storage_is_frozen_before_first_enumerator_call() {
    let resolver = TraceResolver::new([
        (r"H:\media", (vec![5], LocalDiskKind::Hdd)),
        (r"I:\tmp", (vec![12], LocalDiskKind::Ssd)),
    ]);
    let roots = vec![root(r"I:\tmp"), root(r"H:\media")];
    let plan = ScanDiskPlan::build(&roots, &config(3, 7, 2), &resolver).unwrap();
    resolver.trace.lock().unwrap().push("enumerate".into());

    assert!(plan.assign(row(r"H:\media\a.jpg")).is_ok());
    assert_eq!(resolver.trace(), ["resolve:H", "resolve:I", "enumerate"]);
}

#[test]
fn storage_resolution_failure_never_reaches_enumerator() {
    let resolver = TraceResolver::failing(r"H:\missing");
    let error =
        ScanDiskPlan::build(&[root(r"H:\missing")], &config(3, 7, 2), &resolver).unwrap_err();

    assert!(
        error
            .to_string()
            .contains("SCAN_ROOT_STORAGE_RESOLVE_FAILED")
    );
    assert_eq!(resolver.trace(), ["resolve:H"]);
}

#[test]
fn lane_assignment_merges_disks_preserves_composites_and_uses_configured_limits() {
    let resolver = TraceResolver::new([
        (r"D:\A", (vec![7], LocalDiskKind::Hdd)),
        (r"D:\A\nested", (vec![7], LocalDiskKind::Hdd)),
        (r"E:\ssd", (vec![8], LocalDiskKind::Ssd)),
        (r"F:\stripe", (vec![12, 5, 12], LocalDiskKind::Unknown)),
    ]);
    let plan = ScanDiskPlan::build(
        &[
            root(r"D:\A"),
            root(r"D:\A\nested"),
            root(r"E:\ssd"),
            root(r"F:\stripe"),
            root(r"D:\A"),
        ],
        &config(3, 7, 2),
        &resolver,
    )
    .unwrap();

    let nested = plan.assign(row(r"D:\A\nested\x.bin")).unwrap();
    let parent = plan.assign(row(r"D:\A\other.bin")).unwrap();
    let ssd = plan.assign(row(r"E:\ssd\x.bin")).unwrap();
    let stripe = plan.assign(row(r"F:\stripe\x.bin")).unwrap();

    assert_eq!(nested.lane.physical_disk_id.disk_numbers(), &[7]);
    assert_eq!(nested.lane.physical_disk_numbers, vec![7]);
    assert_eq!(nested.lane.disk_kind, LocalDiskKind::Hdd);
    assert_eq!(nested.lane.configured_weight, 3);
    assert_eq!(nested.lane.per_disk_limit, 3);
    assert_eq!(parent.lane, nested.lane);
    assert_eq!(ssd.lane.physical_disk_numbers, vec![8]);
    assert_eq!(ssd.lane.disk_kind, LocalDiskKind::Ssd);
    assert_eq!(ssd.lane.configured_weight, 7);
    assert_eq!(ssd.lane.per_disk_limit, 7);
    assert_eq!(stripe.lane.physical_disk_numbers, vec![5, 12]);
    assert_eq!(stripe.lane.disk_kind, LocalDiskKind::Unknown);
    assert_eq!(stripe.lane.configured_weight, 2);
    assert_eq!(stripe.lane.per_disk_limit, 2);
}

#[test]
fn lane_matching_uses_components_and_rejects_unplanned_paths() {
    let resolver = TraceResolver::new([
        (r"D:\A", (vec![7], LocalDiskKind::Hdd)),
        (r"D:\AB", (vec![8], LocalDiskKind::Ssd)),
    ]);
    let plan = ScanDiskPlan::build(
        &[root(r"D:\A"), root(r"D:\AB")],
        &config(3, 7, 2),
        &resolver,
    )
    .unwrap();

    assert_eq!(
        plan.assign(row(r"D:\AB\file.bin"))
            .unwrap()
            .lane
            .physical_disk_numbers,
        vec![8]
    );
    assert_eq!(
        plan.assign(row(r"D:\A\file.bin"))
            .unwrap()
            .lane
            .physical_disk_numbers,
        vec![7]
    );
    assert!(plan.assign(row(r"D:\ABC\file.bin")).is_err());
}

#[test]
fn mixed_kinds_on_one_lane_degrade_to_unknown_with_conservative_limit() {
    let resolver = TraceResolver::new([
        (r"H:\one", (vec![5], LocalDiskKind::Hdd)),
        (r"H:\two", (vec![5], LocalDiskKind::Ssd)),
    ]);
    let plan = ScanDiskPlan::build(
        &[root(r"H:\one"), root(r"H:\two")],
        &config(9, 5, 3),
        &resolver,
    )
    .unwrap();
    let lane = plan.assign(row(r"H:\one\a.bin")).unwrap().lane;

    assert_eq!(lane.disk_kind, LocalDiskKind::Unknown);
    assert_eq!(lane.physical_disk_numbers, vec![5]);
    assert_eq!(lane.configured_weight, 3);
    assert_eq!(lane.per_disk_limit, 3);
    assert_eq!(plan.assign(row(r"H:\two\b.bin")).unwrap().lane, lane);
}

#[test]
fn assigned_row_keeps_one_frozen_lane_for_hash_and_media_boundaries() {
    let resolver = TraceResolver::new([(r"H:\media", (vec![5, 12], LocalDiskKind::Unknown))]);
    let plan = ScanDiskPlan::build(&[root(r"H:\media")], &config(3, 7, 2), &resolver).unwrap();
    let planned = plan.assign(row(r"H:\media\movie.mkv")).unwrap();
    let resolver_calls_after_plan = resolver.trace().len();

    let hash_lane = planned.lane.clone();
    let media_lane = planned.lane.clone();

    assert_eq!(hash_lane, media_lane);
    assert_eq!(resolver.trace().len(), resolver_calls_after_plan);
}

#[derive(Clone, Copy)]
struct FixedBlockReader;

impl dedup_node_engine::io::BlockReader for FixedBlockReader {
    fn read_at(
        &self,
        _path: &Path,
        _offset: u64,
        buffer: &mut [u8],
        _timeout: Duration,
        _cancellation: &dedup_windows::ReadCancellationToken,
    ) -> Result<usize, BlockReadError> {
        buffer.fill(0x5A);
        Ok(buffer.len())
    }
}

struct CountingStorageResolver {
    calls: Arc<AtomicUsize>,
}

impl ScanRootStorageResolver for CountingStorageResolver {
    fn resolve(&self, root: &Path) -> io::Result<ResolvedScanRootStorage> {
        self.calls.fetch_add(1, Ordering::AcqRel);
        Ok(ResolvedScanRootStorage {
            normalized_root: NormalizedPath::new(root).unwrap(),
            physical_disk_id: PhysicalDiskId::from_disk_numbers([5]).unwrap(),
            disk_kind: LocalDiskKind::Hdd,
        })
    }
}

#[tokio::test]
async fn hash_and_media_consume_frozen_lane_without_second_storage_resolution() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("sample.bin");
    fs::write(&path, b"sample").unwrap();
    let root_path = DisplayPath::new(directory.path()).unwrap();
    let calls = Arc::new(AtomicUsize::new(0));
    let resolver = CountingStorageResolver {
        calls: Arc::clone(&calls),
    };
    let plan = Arc::new(ScanDiskPlan::build(&[root_path], &config(3, 7, 2), &resolver).unwrap());
    assert_eq!(calls.load(Ordering::Acquire), 1);

    let (reader, _) = ScheduledFileReader::controlled_with_plan_for_test(
        &config(3, 7, 2),
        1,
        FixedBlockReader,
        plan,
    )
    .unwrap();
    let scanned = ScannedPath::new(
        NormalizedPath::new(&path).unwrap(),
        DisplayPath::new(&path).unwrap(),
        6,
    );
    let product = reader
        .read(scanned.clone(), dedup_windows::ReadCancellationToken::new())
        .await
        .map_err(|error| match error {
            ReadFailure::Cancelled => "读取取消".to_owned(),
            other => other.to_string(),
        })
        .unwrap();
    drop(product);
    let permit = reader
        .acquire_media_permit(scanned, dedup_windows::ReadCancellationToken::new())
        .await
        .unwrap();
    drop(permit);
    assert_eq!(calls.load(Ordering::Acquire), 1);
}

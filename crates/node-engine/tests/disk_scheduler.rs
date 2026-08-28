//! 每盘加全局读取许可的容量、公平、取消和关闭行为测试。

use std::{
    future::Future,
    pin::Pin,
    task::{Context, Poll, Waker},
};

use dedup_core::DiskReadConfig;
use dedup_windows::{LocalDiskKind, PhysicalDiskId, StorageLocation};

#[allow(dead_code)]
#[path = "../src/io/scheduler.rs"]
mod scheduler;

use scheduler::{DiskReadClass, DiskReadLane, DiskReadPermit, DiskReadScheduler, SchedulerError};

fn weighted_lane(
    disk_numbers: &[u32],
    kind: LocalDiskKind,
    effective_limit: usize,
    configured_weight: usize,
) -> DiskReadLane {
    DiskReadLane {
        location: StorageLocation::from_parts(
            PhysicalDiskId::from_disk_numbers(disk_numbers.iter().copied()).unwrap(),
            kind,
        ),
        effective_limit,
        configured_weight,
    }
}

type WeightedRequest =
    Pin<Box<dyn Future<Output = Result<(usize, DiskReadPermit), SchedulerError>> + Send>>;

/// 在唯一全局席位上收集两个 lane 的真实交付顺序，避免把队列入队顺序当作公平结果。
async fn collect_weighted_sequence(
    high_weight: usize,
    low_weight: usize,
    high_items: usize,
    low_items: usize,
) -> Vec<usize> {
    let mut config = DiskReadConfig::default();
    config.total_threads = 1;
    let scheduler = DiskReadScheduler::new(&config, 20).unwrap();
    let high = weighted_lane(&[1], LocalDiskKind::Ssd, 8, high_weight);
    let low = weighted_lane(&[2], LocalDiskKind::Hdd, 8, low_weight);
    let blocker = scheduler
        .acquire_for_test(&[99], LocalDiskKind::Unknown, DiskReadClass::HashSequential)
        .await
        .unwrap();

    let mut requests: Vec<Option<WeightedRequest>> = Vec::new();
    for _ in 0..high_items {
        let scheduler = scheduler.clone();
        let lane = high.clone();
        requests.push(Some(Box::pin(async move {
            scheduler
                .acquire_lane(lane, DiskReadClass::HashSequential)
                .await
                .map(|permit| (0, permit))
        })));
    }
    for _ in 0..low_items {
        let scheduler = scheduler.clone();
        let lane = low.clone();
        requests.push(Some(Box::pin(async move {
            scheduler
                .acquire_lane(lane, DiskReadClass::HashSequential)
                .await
                .map(|permit| (1, permit))
        })));
    }

    for request in &mut requests {
        assert!(poll_once(request.as_mut().unwrap().as_mut()).is_pending());
    }
    scheduler.barrier_for_test().await.unwrap();
    drop(blocker);
    scheduler.barrier_for_test().await.unwrap();

    let mut sequence = Vec::with_capacity(requests.len());
    while requests.iter().any(Option::is_some) {
        let ready = requests
            .iter_mut()
            .enumerate()
            .find_map(|(index, request)| {
                let request = request.as_mut()?;
                match poll_once(request.as_mut()) {
                    Poll::Ready(result) => Some((index, result.unwrap())),
                    Poll::Pending => None,
                }
            })
            .expect("全局许可释放后应存在下一个已交付请求");
        let (index, (lane_index, permit)) = ready;
        requests[index] = None;
        sequence.push(lane_index);
        drop(permit);
        scheduler.barrier_for_test().await.unwrap();
    }
    sequence
}

#[tokio::test]
async fn configured_weight_five_to_one_is_used_by_real_scheduler_actor() {
    let sequence = collect_weighted_sequence(5, 1, 10, 2).await;
    assert_eq!(&sequence[..6], &[0, 0, 0, 0, 0, 1]);
    assert_eq!(&sequence[6..], &[0, 0, 0, 0, 0, 1]);
}

#[tokio::test]
async fn configured_weight_seven_to_two_is_not_a_hardcoded_five_to_one_ratio() {
    let sequence = collect_weighted_sequence(7, 2, 7, 2).await;
    assert_eq!(sequence, vec![0, 0, 0, 0, 0, 0, 0, 1, 1]);
}

#[tokio::test]
async fn configured_weight_does_not_raise_the_frozen_per_disk_limit() {
    let mut config = DiskReadConfig::default();
    config.total_threads = 3;
    let scheduler = DiskReadScheduler::new(&config, 3).unwrap();
    let high_weight_lane = weighted_lane(&[31], LocalDiskKind::Ssd, 1, 7);
    let other_lane = weighted_lane(&[32], LocalDiskKind::Hdd, 3, 2);

    let high_first = scheduler
        .acquire_lane(high_weight_lane.clone(), DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut high_second =
        Box::pin(scheduler.acquire_lane(high_weight_lane, DiskReadClass::HashSequential));
    assert!(poll_once(high_second.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    let other_first = scheduler
        .acquire_lane(other_lane.clone(), DiskReadClass::HashSequential)
        .await
        .unwrap();
    let other_second = scheduler
        .acquire_lane(other_lane, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let snapshot = scheduler.active_snapshot_for_test(&[31, 32]).await.unwrap();
    assert_eq!(snapshot.disks, vec![(31, 1, 1, 0), (32, 2, 2, 0)]);

    drop(high_first);
    drop(other_first);
    drop(other_second);
    let high_second = high_second.await.unwrap();
    drop(high_second);
}

#[tokio::test]
async fn weighted_lanes_on_distinct_physical_disks_keep_independent_active_counts() {
    let mut config = DiskReadConfig::default();
    config.total_threads = 2;
    let scheduler = DiskReadScheduler::new(&config, 2).unwrap();
    let ssd_lane = weighted_lane(&[41], LocalDiskKind::Ssd, 1, 5);
    let hdd_lane = weighted_lane(&[42], LocalDiskKind::Hdd, 1, 1);

    let ssd = scheduler
        .acquire_lane(ssd_lane, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    let hdd = scheduler
        .acquire_lane(hdd_lane, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let snapshot = scheduler.active_snapshot_for_test(&[41, 42]).await.unwrap();
    assert_eq!(snapshot.global_total, 2);
    assert_eq!(snapshot.disks, vec![(41, 1, 0, 1), (42, 1, 1, 0)]);

    drop(ssd);
    drop(hdd);
}

fn poll_once<F>(future: Pin<&mut F>) -> Poll<F::Output>
where
    F: Future + ?Sized,
{
    let mut context = Context::from_waker(Waker::noop());
    future.poll(&mut context)
}

/// 在指定盘被占用时同时排入 Hash 与媒体请求，返回首次真实竞争的类别和许可。
async fn first_contended_grant(
    scheduler: &DiskReadScheduler,
    disk_number: u32,
    blocker: DiskReadPermit,
) -> (DiskReadClass, DiskReadPermit) {
    let disk_numbers = [disk_number];
    let mut hash = Box::pin(scheduler.acquire_for_test(
        &disk_numbers,
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    let mut media = Box::pin(scheduler.acquire_for_test(
        &disk_numbers,
        LocalDiskKind::Hdd,
        DiskReadClass::MediaDecode,
    ));
    assert!(poll_once(hash.as_mut()).is_pending());
    assert!(poll_once(media.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    drop(blocker);
    scheduler.barrier_for_test().await.unwrap();
    let granted = tokio::select! {
        result = &mut hash => (DiskReadClass::HashSequential, result.unwrap()),
        result = &mut media => (DiskReadClass::MediaDecode, result.unwrap()),
    };
    drop(hash);
    drop(media);
    scheduler.barrier_for_test().await.unwrap();
    granted
}

#[tokio::test]
async fn hash_and_media_share_disk_and_global_hard_limits() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.total_threads = 2;
    let scheduler = DiskReadScheduler::new(&config, 2).unwrap();

    let hash_disk_one = scheduler
        .acquire_for_test(&[1], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut media_same_disk =
        Box::pin(scheduler.acquire_for_test(&[1], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    assert!(poll_once(media_same_disk.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(media_same_disk.as_mut()).is_pending());

    drop(hash_disk_one);
    let media_disk_one = media_same_disk.await.unwrap();
    let hash_disk_two = scheduler
        .acquire_for_test(&[2], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut media_global_blocked =
        Box::pin(scheduler.acquire_for_test(&[3], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    assert!(poll_once(media_global_blocked.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(media_global_blocked.as_mut()).is_pending());

    drop(hash_disk_two);
    let media_disk_three = media_global_blocked.await.unwrap();
    drop(media_disk_one);
    drop(media_disk_three);

    let hash_again = scheduler
        .acquire_for_test(&[1], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let media_again = scheduler
        .acquire_for_test(&[2], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    drop(hash_again);
    drop(media_again);
}

async fn contended_same_disk_grants_media_media_media_hash_even_when_hash_arrived_first_scenario() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.total_threads = 1;
    let scheduler = DiskReadScheduler::new(&config, 1).unwrap();
    let seed = scheduler
        .acquire_for_test(&[10], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();

    let mut hash = Box::pin(scheduler.acquire_for_test(
        &[10],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    let mut media_one =
        Box::pin(scheduler.acquire_for_test(&[10], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    let mut media_two =
        Box::pin(scheduler.acquire_for_test(&[10], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    let mut media_three =
        Box::pin(scheduler.acquire_for_test(&[10], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    let mut media_four =
        Box::pin(scheduler.acquire_for_test(&[10], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    for pending in [
        &mut hash,
        &mut media_one,
        &mut media_two,
        &mut media_three,
        &mut media_four,
    ] {
        assert!(poll_once(pending.as_mut()).is_pending());
    }
    scheduler.barrier_for_test().await.unwrap();

    drop(seed);
    let media_one = media_one.await.unwrap();
    assert!(poll_once(hash.as_mut()).is_pending());
    assert!(poll_once(media_two.as_mut()).is_pending());
    drop(media_one);

    let media_two = media_two.await.unwrap();
    assert!(poll_once(hash.as_mut()).is_pending());
    assert!(poll_once(media_three.as_mut()).is_pending());
    drop(media_two);

    let media_three = media_three.await.unwrap();
    assert!(poll_once(hash.as_mut()).is_pending());
    assert!(poll_once(media_four.as_mut()).is_pending());
    drop(media_three);

    let hash = hash.await.unwrap();
    assert!(poll_once(media_four.as_mut()).is_pending());
    drop(hash);
    let media_four = media_four.await.unwrap();
    drop(media_four);
}

#[tokio::test]
async fn contended_same_disk_grants_media_media_media_hash_even_when_hash_arrived_first() {
    contended_same_disk_grants_media_media_media_hash_even_when_hash_arrived_first_scenario().await;
}

#[tokio::test]
async fn only_contended_overlapping_grants_change_three_to_one_quota() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.total_threads = 1;

    let media_history_scheduler = DiskReadScheduler::new(&config, 1).unwrap();
    for _ in 0..4 {
        let permit = media_history_scheduler
            .acquire_for_test(&[30], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
            .await
            .unwrap();
        drop(permit);
        media_history_scheduler.barrier_for_test().await.unwrap();
    }
    let media_blocker = media_history_scheduler
        .acquire_for_test(&[30], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    let (after_uncontended_media, permit) =
        first_contended_grant(&media_history_scheduler, 30, media_blocker).await;
    drop(permit);

    let hash_history_scheduler = DiskReadScheduler::new(&config, 1).unwrap();
    let mut blocker = hash_history_scheduler
        .acquire_for_test(&[40], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    for _ in 0..3 {
        let (class, permit) = first_contended_grant(&hash_history_scheduler, 40, blocker).await;
        assert_eq!(class, DiskReadClass::MediaDecode);
        blocker = permit;
    }
    drop(blocker);
    hash_history_scheduler.barrier_for_test().await.unwrap();

    let uncontended_hash = hash_history_scheduler
        .acquire_for_test(&[40], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let (after_uncontended_hash, permit) =
        first_contended_grant(&hash_history_scheduler, 40, uncontended_hash).await;
    drop(permit);

    assert_eq!(
        [after_uncontended_media, after_uncontended_hash],
        [DiskReadClass::HashSequential, DiskReadClass::MediaDecode,]
    );
}

#[tokio::test]
async fn single_waiting_class_uses_all_available_permits() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 3;
    config.total_threads = 3;
    let scheduler = DiskReadScheduler::new(&config, 3).unwrap();

    let mut hashes = Vec::new();
    for _ in 0..3 {
        hashes.push(
            scheduler
                .acquire_for_test(&[20], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
                .await
                .unwrap(),
        );
    }
    let mut fourth_hash = Box::pin(scheduler.acquire_for_test(
        &[20],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(fourth_hash.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    drop(hashes);
    let fourth_hash = fourth_hash.await.unwrap();
    drop(fourth_hash);

    let mut media = Vec::new();
    for _ in 0..3 {
        media.push(
            scheduler
                .acquire_for_test(&[20], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
                .await
                .unwrap(),
        );
    }
    let mut fourth_media =
        Box::pin(scheduler.acquire_for_test(&[20], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    assert!(poll_once(fourth_media.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    drop(media);
    let fourth_media = fourth_media.await.unwrap();
    drop(fourth_media);
}

#[tokio::test]
async fn both_classes_on_four_seat_disk_converge_to_three_media_and_one_hash_by_active_count() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 4;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();

    // 先占满四个媒体 seat，再观察新请求必须依据现有 active 类别压力选择。
    let mut media_blockers = Vec::new();
    for _ in 0..4 {
        media_blockers.push(
            scheduler
                .acquire_for_test(&[21], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
                .await
                .unwrap(),
        );
    }

    // Hash 队首比 Media 更老；释放一个 seat 后，3/3 的 Media 压力应让出最后一个名义 Hash seat。
    let mut hash = Box::pin(scheduler.acquire_for_test(
        &[21],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    let mut media =
        Box::pin(scheduler.acquire_for_test(&[21], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    assert!(poll_once(hash.as_mut()).is_pending());
    assert!(poll_once(media.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    drop(media_blockers.pop().unwrap());
    scheduler.barrier_for_test().await.unwrap();

    // 当前旧实现按媒体 streak 选择 Media；active-seat 策略此处必须先交付 Hash。
    assert!(poll_once(media.as_mut()).is_pending());
    let hash = hash.await.unwrap();
    drop(hash);
    let media = media.await.unwrap();
    drop(media);
    drop(media_blockers);
}

async fn aged_composite_blocks_overlapping_bypass_but_allows_disjoint_disk_scenario() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.total_threads = 3;
    let scheduler = DiskReadScheduler::new(&config, 3).unwrap();
    let mut block_five = scheduler
        .acquire_for_test(&[5], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut block_twelve = scheduler
        .acquire_for_test(&[12], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut composite = Box::pin(scheduler.acquire_for_test(
        &[5, 12],
        LocalDiskKind::Hdd,
        DiskReadClass::MediaDecode,
    ));
    assert!(poll_once(composite.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    for bypass in 0..8 {
        let disk = if bypass % 2 == 0 { 5 } else { 12 };
        let disk_numbers = [disk];
        let mut younger = Box::pin(scheduler.acquire_for_test(
            &disk_numbers,
            LocalDiskKind::Hdd,
            DiskReadClass::HashSequential,
        ));
        assert!(poll_once(younger.as_mut()).is_pending());
        scheduler.barrier_for_test().await.unwrap();
        if disk == 5 {
            drop(block_five);
            block_five = younger.await.unwrap();
        } else {
            drop(block_twelve);
            block_twelve = younger.await.unwrap();
        }
        assert!(poll_once(composite.as_mut()).is_pending());
    }

    let mut younger_five = Box::pin(scheduler.acquire_for_test(
        &[5],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    let mut younger_twelve =
        Box::pin(scheduler.acquire_for_test(&[12], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    assert!(poll_once(younger_five.as_mut()).is_pending());
    assert!(poll_once(younger_twelve.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    drop(block_five);
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(younger_five.as_mut()).is_pending());
    assert!(poll_once(younger_twelve.as_mut()).is_pending());

    let disjoint = scheduler
        .acquire_for_test(&[20], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    drop(disjoint);
    drop(block_twelve);
    let composite = composite.await.unwrap();
    assert!(poll_once(younger_five.as_mut()).is_pending());
    assert!(poll_once(younger_twelve.as_mut()).is_pending());
    drop(composite);
    let younger_five = younger_five.await.unwrap();
    let younger_twelve = younger_twelve.await.unwrap();
    drop(younger_five);
    drop(younger_twelve);
}

#[tokio::test]
async fn aged_composite_blocks_overlapping_bypass_but_allows_disjoint_disk() {
    aged_composite_blocks_overlapping_bypass_but_allows_disjoint_disk_scenario().await;
}

async fn cross_class_composite_reserves_every_underlying_disk_atomically_scenario() {
    let mut config = DiskReadConfig::default();
    config.unknown_threads_per_disk = 1;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();
    let twelve = scheduler
        .acquire_for_test(&[12], LocalDiskKind::Unknown, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut composite = Box::pin(scheduler.acquire_for_test(
        &[5, 12],
        LocalDiskKind::Unknown,
        DiskReadClass::MediaDecode,
    ));
    assert!(poll_once(composite.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    let five = scheduler
        .acquire_for_test(&[5], LocalDiskKind::Unknown, DiskReadClass::HashSequential)
        .await
        .unwrap();
    drop(twelve);
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(composite.as_mut()).is_pending());
    drop(five);
    let composite = composite.await.unwrap();

    let mut hash_five = Box::pin(scheduler.acquire_for_test(
        &[5],
        LocalDiskKind::Unknown,
        DiskReadClass::HashSequential,
    ));
    let mut media_twelve = Box::pin(scheduler.acquire_for_test(
        &[12],
        LocalDiskKind::Unknown,
        DiskReadClass::MediaDecode,
    ));
    assert!(poll_once(hash_five.as_mut()).is_pending());
    assert!(poll_once(media_twelve.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    drop(composite);
    let hash_five = hash_five.await.unwrap();
    let media_twelve = media_twelve.await.unwrap();
    drop(hash_five);
    drop(media_twelve);
}

#[tokio::test]
async fn cross_class_composite_reserves_every_underlying_disk_atomically() {
    cross_class_composite_reserves_every_underlying_disk_atomically_scenario().await;
}

#[tokio::test]
async fn distinct_t1_disks_sharing_t2_disk_reach_global_pressure_arbitration() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.ssd_threads_per_disk = 2;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();

    // 先固定两块独立 T=1 盘；后续复合请求使用 Ssd 观察值时仍保留各自的 T=1 约束。
    for disk_number in [1, 2] {
        let permit = scheduler
            .acquire_for_test(
                &[disk_number],
                LocalDiskKind::Hdd,
                DiskReadClass::HashSequential,
            )
            .await
            .unwrap();
        drop(permit);
    }
    // 共享盘单独以 T=2 的观察值注册，避免把它误建成 T=1。
    let shared = scheduler
        .acquire_for_test(&[10], LocalDiskKind::Ssd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    drop(shared);

    // 让两个候选都必须进入全局裁决；释放一个全局 seat 后，Hash 压力应低于 Media 压力。
    let mut blockers = Vec::new();
    for disk_number in [20, 21, 22, 23] {
        blockers.push(
            scheduler
                .acquire_for_test(
                    &[disk_number],
                    LocalDiskKind::Ssd,
                    DiskReadClass::MediaDecode,
                )
                .await
                .unwrap(),
        );
    }

    let mut hash_candidate = Box::pin(scheduler.acquire_for_test(
        &[1, 10],
        LocalDiskKind::Ssd,
        DiskReadClass::HashSequential,
    ));
    let mut media_candidate = Box::pin(scheduler.acquire_for_test(
        &[2, 10],
        LocalDiskKind::Ssd,
        DiskReadClass::MediaDecode,
    ));
    assert!(poll_once(hash_candidate.as_mut()).is_pending());
    assert!(poll_once(media_candidate.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    drop(blockers.pop().unwrap());
    scheduler.barrier_for_test().await.unwrap();

    let hash_candidate =
        tokio::time::timeout(std::time::Duration::from_millis(200), &mut hash_candidate)
            .await
            .expect("两个 T=1 候选都必须进入全局压力裁决")
            .unwrap();
    assert!(poll_once(media_candidate.as_mut()).is_pending());
    assert_eq!(hash_candidate.physical_disk_id(), "PhysicalDisk1+10");

    drop(hash_candidate);
    let media_candidate = media_candidate.await.unwrap();
    assert_eq!(media_candidate.physical_disk_id(), "PhysicalDisk2+10");
    drop(media_candidate);
    drop(blockers);
}

#[tokio::test]
async fn configured_disk_and_global_limits_are_held_until_permit_drop() {
    let config = DiskReadConfig::default();
    let scheduler = DiskReadScheduler::new(&config, 3).unwrap();

    let hdd_first = scheduler
        .acquire_for_test(&[1], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut hdd_second = Box::pin(scheduler.acquire_for_test(
        &[1],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(hdd_second.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(hdd_second.as_mut()).is_pending());
    drop(hdd_first);
    let hdd_second = hdd_second.await.unwrap();

    let ssd_first = scheduler
        .acquire_for_test(&[2], LocalDiskKind::Ssd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let ssd_second = scheduler
        .acquire_for_test(&[2], LocalDiskKind::Ssd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut ssd_third = Box::pin(scheduler.acquire_for_test(
        &[2],
        LocalDiskKind::Ssd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(ssd_third.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(ssd_third.as_mut()).is_pending());

    let unknown = scheduler
        .acquire_for_test(&[3], LocalDiskKind::Unknown, DiskReadClass::HashSequential)
        .await
        .unwrap();
    assert_eq!(scheduler.request_capacity_for_test(), 16);
    let mut global_fifth = Box::pin(scheduler.acquire_for_test(
        &[4],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(global_fifth.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(global_fifth.as_mut()).is_pending());

    drop(hdd_second);
    let global_fifth = global_fifth.await.unwrap();
    drop(global_fifth);
    drop(unknown);
    drop(ssd_first);
    drop(ssd_second);
    let ssd_third = ssd_third.await.unwrap();
    drop(ssd_third);
}

#[tokio::test]
async fn active_disks_are_round_robin_even_when_disk_a_has_a_long_fifo() {
    let mut config = DiskReadConfig::default();
    config.total_threads = 1;
    let scheduler = DiskReadScheduler::new(&config, 1).unwrap();
    let first_a = scheduler
        .acquire_for_test(&[10], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();

    let mut second_a = Box::pin(scheduler.acquire_for_test(
        &[10],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    let mut third_a = Box::pin(scheduler.acquire_for_test(
        &[10],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    let mut fourth_a = Box::pin(scheduler.acquire_for_test(
        &[10],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    let mut first_b = Box::pin(scheduler.acquire_for_test(
        &[20],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(second_a.as_mut()).is_pending());
    assert!(poll_once(third_a.as_mut()).is_pending());
    assert!(poll_once(fourth_a.as_mut()).is_pending());
    assert!(poll_once(first_b.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    drop(first_a);
    let second_a = second_a.await.unwrap();
    drop(second_a);
    let first_b = first_b.await.unwrap();
    assert!(poll_once(third_a.as_mut()).is_pending());
    drop(first_b);
    let third_a = third_a.await.unwrap();
    drop(third_a);
    let fourth_a = fourth_a.await.unwrap();
    drop(fourth_a);
}

#[tokio::test]
async fn blocked_disk_fifo_does_not_consume_the_disjoint_disks_global_seat() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.total_threads = 2;
    let scheduler = DiskReadScheduler::new(&config, 2).unwrap();
    let disk_one_active = scheduler
        .acquire_for_test(&[301], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();

    // Disk1 长 FIFO 全部受每盘上限阻塞；Disk2 应使用仍空闲的第二个全局 seat。
    let mut disk_one_waiters = (0..4)
        .map(|_| {
            Box::pin(scheduler.acquire_for_test(
                &[301],
                LocalDiskKind::Hdd,
                DiskReadClass::HashSequential,
            ))
        })
        .collect::<Vec<_>>();
    for waiter in &mut disk_one_waiters {
        assert!(poll_once(waiter.as_mut()).is_pending());
    }
    let mut disk_two = Box::pin(scheduler.acquire_for_test(
        &[302],
        LocalDiskKind::Hdd,
        DiskReadClass::MediaDecode,
    ));
    let disk_two = match poll_once(disk_two.as_mut()) {
        Poll::Ready(result) => result.unwrap(),
        Poll::Pending => {
            scheduler.barrier_for_test().await.unwrap();
            tokio::time::timeout(std::time::Duration::from_millis(200), disk_two)
                .await
                .expect("Disk1 长 FIFO 不得阻塞不相交的 Disk2")
                .unwrap()
        }
    };
    assert_eq!(disk_two.physical_disk_id(), "PhysicalDisk302");
    for waiter in &mut disk_one_waiters {
        assert!(poll_once(waiter.as_mut()).is_pending());
    }

    drop(disk_two);
    drop(disk_one_active);
    drop(disk_one_waiters);
}

#[tokio::test]
async fn cancelled_fifo_head_is_skipped_without_leaking_capacity() {
    let mut config = DiskReadConfig::default();
    config.total_threads = 1;
    let scheduler = DiskReadScheduler::new(&config, 1).unwrap();
    let active = scheduler
        .acquire_for_test(&[30], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut cancelled = Box::pin(scheduler.acquire_for_test(
        &[30],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    let mut live = Box::pin(scheduler.acquire_for_test(
        &[30],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(cancelled.as_mut()).is_pending());
    assert!(poll_once(live.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    drop(cancelled);
    drop(active);
    let live = live.await.unwrap();
    drop(live);
    let after_cancel = scheduler
        .acquire_for_test(&[30], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    drop(after_cancel);
}

#[tokio::test]
async fn capacity_uses_total_or_workers_and_shutdown_fails_pending_and_new_requests() {
    let mut config = DiskReadConfig::default();
    config.total_threads = 1;
    let scheduler = DiskReadScheduler::new(&config, 10).unwrap();
    assert_eq!(scheduler.request_capacity_for_test(), 20);

    let active = scheduler
        .acquire_for_test(&[40], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut pending = Box::pin(scheduler.acquire_for_test(
        &[40],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(pending.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    scheduler.shutdown().await.unwrap();

    assert!(matches!(pending.await, Err(SchedulerError::Closed)));
    assert!(matches!(
        scheduler
            .acquire_for_test(&[50], LocalDiskKind::Hdd, DiskReadClass::HashSequential,)
            .await,
        Err(SchedulerError::Closed)
    ));
    drop(active);
}

#[tokio::test]
async fn composite_and_single_locations_share_every_underlying_disk_limit() {
    let config = DiskReadConfig::default();
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();
    let composite = scheduler
        .acquire_for_test(&[5, 12], LocalDiskKind::Unknown, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    let mut single_five = Box::pin(scheduler.acquire_for_test(
        &[5],
        LocalDiskKind::Ssd,
        DiskReadClass::HashSequential,
    ));
    let mut single_twelve = Box::pin(scheduler.acquire_for_test(
        &[12],
        LocalDiskKind::Ssd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(single_five.as_mut()).is_pending());
    assert!(poll_once(single_twelve.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(single_five.as_mut()).is_pending());
    assert!(poll_once(single_twelve.as_mut()).is_pending());

    drop(composite);
    let single_five = single_five.await.unwrap();
    let single_twelve = single_twelve.await.unwrap();
    let mut composite_again = Box::pin(scheduler.acquire_for_test(
        &[5, 12],
        LocalDiskKind::Unknown,
        DiskReadClass::MediaDecode,
    ));
    assert!(poll_once(composite_again.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(composite_again.as_mut()).is_pending());

    drop(single_five);
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(composite_again.as_mut()).is_pending());
    drop(single_twelve);
    let composite_again = composite_again.await.unwrap();
    drop(composite_again);
}

#[tokio::test]
async fn overlapping_composites_check_all_disks_atomically_without_partial_reservation() {
    let config = DiskReadConfig::default();
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();
    let twelve = scheduler
        .acquire_for_test(&[12], LocalDiskKind::Unknown, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut five_twelve = Box::pin(scheduler.acquire_for_test(
        &[5, 12],
        LocalDiskKind::Unknown,
        DiskReadClass::MediaDecode,
    ));
    let mut twelve_twenty = Box::pin(scheduler.acquire_for_test(
        &[12, 20],
        LocalDiskKind::Unknown,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(five_twelve.as_mut()).is_pending());
    assert!(poll_once(twelve_twenty.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(five_twelve.as_mut()).is_pending());
    assert!(poll_once(twelve_twenty.as_mut()).is_pending());

    let five = scheduler
        .acquire_for_test(&[5], LocalDiskKind::Unknown, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let twenty = scheduler
        .acquire_for_test(&[20], LocalDiskKind::Unknown, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    drop(twelve);
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(five_twelve.as_mut()).is_pending());
    assert!(poll_once(twelve_twenty.as_mut()).is_pending());

    drop(five);
    let five_twelve = five_twelve.await.unwrap();
    drop(twenty);
    assert!(poll_once(twelve_twenty.as_mut()).is_pending());
    drop(five_twelve);
    let twelve_twenty = twelve_twenty.await.unwrap();
    drop(twelve_twenty);
}

#[tokio::test]
async fn single_waiting_class_can_borrow_all_four_seats() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 4;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();

    let mut hashes = Vec::new();
    for _ in 0..4 {
        hashes.push(
            scheduler
                .acquire_for_test(&[80], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
                .await
                .unwrap(),
        );
    }
    let mut media =
        Box::pin(scheduler.acquire_for_test(&[80], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    assert!(poll_once(media.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    drop(hashes);
    let media = media.await.unwrap();
    drop(media);
}

#[tokio::test]
async fn borrowed_seat_is_not_preempted_and_natural_drop_restores_media_target() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 4;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();

    let mut hashes = Vec::new();
    for _ in 0..4 {
        hashes.push(
            scheduler
                .acquire_for_test(&[81], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
                .await
                .unwrap(),
        );
    }
    let mut media =
        Box::pin(scheduler.acquire_for_test(&[81], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    assert!(poll_once(media.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    // 名义 Media seat 不是抢占式配额；已有 Hash permit 未 Drop 前不得被替换。
    assert!(poll_once(media.as_mut()).is_pending());
    drop(hashes.pop().unwrap());
    let media = media.await.unwrap();
    drop(media);
    drop(hashes);
}

#[tokio::test]
async fn global_class_pressure_applies_to_cross_disk_candidates() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 4;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();

    let mut media_blockers = Vec::new();
    for _ in 0..4 {
        media_blockers.push(
            scheduler
                .acquire_for_test(&[82], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
                .await
                .unwrap(),
        );
    }
    // 预加载 Media 只占 disk82；disk83/disk84 的候选盘本地均为空。
    // 若只看本地压力会偏向 Media，Hash 先获全局 class seat 才能证明全局压力生效。
    let mut media =
        Box::pin(scheduler.acquire_for_test(&[83], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    let mut hash = Box::pin(scheduler.acquire_for_test(
        &[84],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(media.as_mut()).is_pending());
    assert!(poll_once(hash.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    // 先释放 disk82 的一个占位，让两类候选同时面对同一个全局空位。
    drop(media_blockers.pop().unwrap());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(media.as_mut()).is_pending());
    let hash = hash.await.unwrap();
    drop(hash);
    let media = media.await.unwrap();
    drop(media);
    drop(media_blockers);
}

#[tokio::test]
async fn capacity_one_rotation_is_media_three_then_hash_one() {
    contended_same_disk_grants_media_media_media_hash_even_when_hash_arrived_first_scenario().await;
}

#[tokio::test]
async fn capacity_one_composite_with_conflicting_preferences_chooses_oldest_atomic_request() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.total_threads = 1;
    let scheduler = DiskReadScheduler::new(&config, 1).unwrap();

    // 让盘 5 的下一次偏好切换为 Hash，盘 12 仍偏好 Media，制造复合盘偏好冲突。
    for _ in 0..3 {
        let permit = scheduler
            .acquire_for_test(&[85], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
            .await
            .unwrap();
        drop(permit);
        scheduler.barrier_for_test().await.unwrap();
    }
    let blocker = scheduler
        .acquire_for_test(&[86], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut composite = Box::pin(scheduler.acquire_for_test(
        &[85, 87],
        LocalDiskKind::Hdd,
        DiskReadClass::MediaDecode,
    ));
    let mut hash = Box::pin(scheduler.acquire_for_test(
        &[85],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(composite.as_mut()).is_pending());
    assert!(poll_once(hash.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    drop(blocker);
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(hash.as_mut()).is_pending());
    let composite = composite.await.unwrap();
    assert_eq!(composite.physical_disk_id(), "PhysicalDisk85+87");
    drop(composite);
    let hash = hash.await.unwrap();
    drop(hash);
}

#[tokio::test]
async fn aged_reservation_freezes_intersecting_and_last_global_seat_but_allows_disjoint_work() {
    aged_composite_blocks_overlapping_bypass_but_allows_disjoint_disk_scenario().await;
}

#[tokio::test]
async fn aged_reservation_is_cleared_after_cancel() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.total_threads = 2;
    let scheduler = DiskReadScheduler::new(&config, 2).unwrap();
    let mut block_five = scheduler
        .acquire_for_test(&[90], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let block_twelve = scheduler
        .acquire_for_test(&[91], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut aged = Box::pin(scheduler.acquire_for_test(
        &[90, 91],
        LocalDiskKind::Hdd,
        DiskReadClass::MediaDecode,
    ));
    assert!(poll_once(aged.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    for _ in 0..8 {
        let mut younger = Box::pin(scheduler.acquire_for_test(
            &[90],
            LocalDiskKind::Hdd,
            DiskReadClass::HashSequential,
        ));
        assert!(poll_once(younger.as_mut()).is_pending());
        scheduler.barrier_for_test().await.unwrap();
        drop(block_five);
        block_five = younger.await.unwrap();
        assert!(poll_once(aged.as_mut()).is_pending());
    }

    drop(aged);
    let mut replacement =
        Box::pin(scheduler.acquire_for_test(&[90], LocalDiskKind::Hdd, DiskReadClass::MediaDecode));
    assert!(poll_once(replacement.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    drop(block_five);
    let replacement = replacement.await.unwrap();
    drop(replacement);
    drop(block_twelve);
}

#[tokio::test]
async fn class_active_counts_return_to_zero_after_permit_drop() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 4;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();
    let media = scheduler
        .acquire_for_test(&[92], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    let hash = scheduler
        .acquire_for_test(&[93], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let composite = scheduler
        .acquire_for_test(&[94, 95], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    drop(media);
    drop(hash);
    drop(composite);
    scheduler.barrier_for_test().await.unwrap();
    let snapshot = scheduler
        .active_snapshot_for_test(&[92, 93, 94, 95])
        .await
        .unwrap();
    assert_eq!(
        (
            snapshot.global_total,
            snapshot.global_hash,
            snapshot.global_media
        ),
        (0, 0, 0)
    );
    assert!(
        snapshot
            .disks
            .iter()
            .all(|(_, total, hash, media)| (*total, *hash, *media) == (0, 0, 0))
    );
}

#[tokio::test]
async fn composite_permit_updates_and_releases_every_disk_class_counter_atomically() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 4;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();

    // 独立验证复合 permit：一个逻辑 permit 同时占用两块底层盘及全局 class 计数。
    let composite = scheduler
        .acquire_for_test(&[103, 104], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    scheduler.barrier_for_test().await.unwrap();
    let held = scheduler
        .active_snapshot_for_test(&[103, 104])
        .await
        .unwrap();
    assert_eq!(
        (held.global_total, held.global_hash, held.global_media),
        (1, 0, 1)
    );
    assert_eq!(held.disks.len(), 2);
    assert!(
        held.disks
            .iter()
            .all(|(_, total, hash, media)| (*total, *hash, *media) == (1, 0, 1))
    );

    drop(composite);
    scheduler.barrier_for_test().await.unwrap();
    let released = scheduler
        .active_snapshot_for_test(&[103, 104])
        .await
        .unwrap();
    assert_eq!(
        (
            released.global_total,
            released.global_hash,
            released.global_media
        ),
        (0, 0, 0)
    );
    assert!(
        released
            .disks
            .iter()
            .all(|(_, total, hash, media)| (*total, *hash, *media) == (0, 0, 0))
    );
}

#[tokio::test]
async fn same_disk_lower_observed_limit_recomputes_nominal_seats_without_preempting_active_permits()
{
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 4;
    config.ssd_threads_per_disk = 2;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();
    let first = scheduler
        .acquire_for_test(&[98], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    let second = scheduler
        .acquire_for_test(&[98], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();

    let mut hash = Box::pin(scheduler.acquire_for_test(
        &[98],
        LocalDiskKind::Ssd,
        DiskReadClass::HashSequential,
    ));
    let mut media =
        Box::pin(scheduler.acquire_for_test(&[98], LocalDiskKind::Ssd, DiskReadClass::MediaDecode));
    assert!(poll_once(hash.as_mut()).is_pending());
    assert!(poll_once(media.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    // 观察到更小 Ssd limit 后，已有两个 Hdd permit 仍保持，不被抢占。
    assert!(poll_once(hash.as_mut()).is_pending());
    drop(first);
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(media.as_mut()).is_pending());
    let hash = hash.await.unwrap();
    drop(hash);
    drop(second);
    let media = media.await.unwrap();
    drop(media);
}

#[tokio::test]
async fn composite_location_uses_minimum_limit_for_all_underlying_disks() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 4;
    config.ssd_threads_per_disk = 2;
    config.total_threads = 4;
    let scheduler = DiskReadScheduler::new(&config, 4).unwrap();

    let disk_a = scheduler
        .acquire_for_test(&[99], LocalDiskKind::Ssd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    drop(disk_a);
    let disk_b = scheduler
        .acquire_for_test(&[100], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    drop(disk_b);
    let composite = scheduler
        .acquire_for_test(
            &[99, 100],
            LocalDiskKind::Ssd,
            DiskReadClass::HashSequential,
        )
        .await
        .unwrap();
    drop(composite);

    let blocker = scheduler
        .acquire_for_test(&[100], LocalDiskKind::Ssd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    let global_blocker_one = scheduler
        .acquire_for_test(&[101], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let global_blocker_two = scheduler
        .acquire_for_test(&[102], LocalDiskKind::Hdd, DiskReadClass::HashSequential)
        .await
        .unwrap();
    let mut hash = Box::pin(scheduler.acquire_for_test(
        &[100],
        LocalDiskKind::Ssd,
        DiskReadClass::HashSequential,
    ));
    let mut media = Box::pin(scheduler.acquire_for_test(
        &[100],
        LocalDiskKind::Ssd,
        DiskReadClass::MediaDecode,
    ));
    assert!(poll_once(hash.as_mut()).is_pending());
    assert!(poll_once(media.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    drop(global_blocker_one);
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(media.as_mut()).is_pending());
    let hash = hash.await.unwrap();
    drop(hash);
    drop(blocker);
    drop(global_blocker_two);
    let media = media.await.unwrap();
    drop(media);
}

#[tokio::test]
async fn disjoint_capacity_one_media_cannot_bypass_older_hash_on_global_last_seat() {
    let mut config = DiskReadConfig::default();
    config.hdd_threads_per_disk = 1;
    config.total_threads = 1;
    let scheduler = DiskReadScheduler::new(&config, 1).unwrap();

    // 先占住唯一 global seat，再让不相交盘 B 的 Hash 早于盘 A 的连续 Media 入队。
    let blocker = scheduler
        .acquire_for_test(&[201], LocalDiskKind::Hdd, DiskReadClass::MediaDecode)
        .await
        .unwrap();
    let mut hash = Box::pin(scheduler.acquire_for_test(
        &[202],
        LocalDiskKind::Hdd,
        DiskReadClass::HashSequential,
    ));
    assert!(poll_once(hash.as_mut()).is_pending());

    // 连续补入 A/Media，验证 T=1 轮换不能跨盘压过更老的 B/Hash。
    let mut media_requests = Vec::new();
    for _ in 0..4 {
        let mut media = Box::pin(scheduler.acquire_for_test(
            &[201],
            LocalDiskKind::Hdd,
            DiskReadClass::MediaDecode,
        ));
        assert!(poll_once(media.as_mut()).is_pending());
        media_requests.push(media);
    }
    scheduler.barrier_for_test().await.unwrap();

    drop(blocker);
    scheduler.barrier_for_test().await.unwrap();

    // B/Hash 应获得释放后的唯一 global seat；当前实现会错误地先交付 A/Media。
    let hash = match poll_once(hash.as_mut()) {
        Poll::Ready(result) => result.unwrap(),
        Poll::Pending => panic!("不相交盘的较老 Hash 被 T=1 Media 跨盘绕过"),
    };
    for media in &mut media_requests {
        assert!(poll_once(media.as_mut()).is_pending());
    }
    drop(hash);
    drop(media_requests);
}

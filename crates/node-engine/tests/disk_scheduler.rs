//! 每盘加全局读取许可的容量、公平、取消和关闭行为测试。

use std::{
    future::Future,
    pin::Pin,
    task::{Context, Poll, Waker},
};

use dedup_core::DiskReadConfig;
use dedup_windows::LocalDiskKind;

#[allow(dead_code)]
#[path = "../src/io/scheduler.rs"]
mod scheduler;

use scheduler::{DiskReadScheduler, SchedulerError};

fn poll_once<F>(future: Pin<&mut F>) -> Poll<F::Output>
where
    F: Future,
{
    let mut context = Context::from_waker(Waker::noop());
    future.poll(&mut context)
}

#[tokio::test]
async fn configured_disk_and_global_limits_are_held_until_permit_drop() {
    let config = DiskReadConfig::default();
    let scheduler = DiskReadScheduler::new(&config, 3).unwrap();

    let hdd_first = scheduler
        .acquire_for_test(&[1], LocalDiskKind::Hdd)
        .await
        .unwrap();
    let mut hdd_second = Box::pin(scheduler.acquire_for_test(&[1], LocalDiskKind::Hdd));
    assert!(poll_once(hdd_second.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(hdd_second.as_mut()).is_pending());
    drop(hdd_first);
    let hdd_second = hdd_second.await.unwrap();

    let ssd_first = scheduler
        .acquire_for_test(&[2], LocalDiskKind::Ssd)
        .await
        .unwrap();
    let ssd_second = scheduler
        .acquire_for_test(&[2], LocalDiskKind::Ssd)
        .await
        .unwrap();
    let mut ssd_third = Box::pin(scheduler.acquire_for_test(&[2], LocalDiskKind::Ssd));
    assert!(poll_once(ssd_third.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    assert!(poll_once(ssd_third.as_mut()).is_pending());

    let unknown = scheduler
        .acquire_for_test(&[3], LocalDiskKind::Unknown)
        .await
        .unwrap();
    assert_eq!(scheduler.request_capacity_for_test(), 16);
    let mut global_fifth = Box::pin(scheduler.acquire_for_test(&[4], LocalDiskKind::Hdd));
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
        .acquire_for_test(&[10], LocalDiskKind::Hdd)
        .await
        .unwrap();

    let mut second_a = Box::pin(scheduler.acquire_for_test(&[10], LocalDiskKind::Hdd));
    let mut third_a = Box::pin(scheduler.acquire_for_test(&[10], LocalDiskKind::Hdd));
    let mut fourth_a = Box::pin(scheduler.acquire_for_test(&[10], LocalDiskKind::Hdd));
    let mut first_b = Box::pin(scheduler.acquire_for_test(&[20], LocalDiskKind::Hdd));
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
async fn cancelled_fifo_head_is_skipped_without_leaking_capacity() {
    let mut config = DiskReadConfig::default();
    config.total_threads = 1;
    let scheduler = DiskReadScheduler::new(&config, 1).unwrap();
    let active = scheduler
        .acquire_for_test(&[30], LocalDiskKind::Hdd)
        .await
        .unwrap();
    let mut cancelled = Box::pin(scheduler.acquire_for_test(&[30], LocalDiskKind::Hdd));
    let mut live = Box::pin(scheduler.acquire_for_test(&[30], LocalDiskKind::Hdd));
    assert!(poll_once(cancelled.as_mut()).is_pending());
    assert!(poll_once(live.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();

    drop(cancelled);
    drop(active);
    let live = live.await.unwrap();
    drop(live);
    let after_cancel = scheduler
        .acquire_for_test(&[30], LocalDiskKind::Hdd)
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
        .acquire_for_test(&[40], LocalDiskKind::Hdd)
        .await
        .unwrap();
    let mut pending = Box::pin(scheduler.acquire_for_test(&[40], LocalDiskKind::Hdd));
    assert!(poll_once(pending.as_mut()).is_pending());
    scheduler.barrier_for_test().await.unwrap();
    scheduler.shutdown().await.unwrap();

    assert!(matches!(pending.await, Err(SchedulerError::Closed)));
    assert!(matches!(
        scheduler.acquire_for_test(&[50], LocalDiskKind::Hdd).await,
        Err(SchedulerError::Closed)
    ));
    drop(active);
}

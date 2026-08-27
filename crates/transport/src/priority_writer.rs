//! 两个有界发送队列之间的控制消息优先调度。

use std::{
    collections::VecDeque,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};

use tokio::sync::{Mutex, Notify};

use crate::TransportError;

/// 一个低优先级块发出后会重新检查高优先级消息的有界队列。
pub struct PriorityWriter<T> {
    inner: Arc<PriorityInner<T>>,
}

struct PriorityInner<T> {
    state: Mutex<PriorityState<T>>,
    items: Notify,
    spaces: Notify,
    closed: AtomicBool,
}

struct PriorityState<T> {
    selected_low: Option<T>,
    high: VecDeque<T>,
    low: VecDeque<T>,
    high_capacity: usize,
    low_capacity: usize,
    closed: bool,
}

impl<T> Clone for PriorityWriter<T> {
    fn clone(&self) -> Self {
        Self {
            inner: Arc::clone(&self.inner),
        }
    }
}

impl<T> PriorityWriter<T> {
    /// 创建高低两个固定容量的发送队列；容量必须大于零。
    pub fn new(high_capacity: usize, low_capacity: usize) -> Self {
        assert!(high_capacity > 0 && low_capacity > 0);
        Self {
            inner: Arc::new(PriorityInner {
                state: Mutex::new(PriorityState {
                    selected_low: None,
                    high: VecDeque::with_capacity(high_capacity),
                    low: VecDeque::with_capacity(low_capacity),
                    high_capacity,
                    low_capacity,
                    closed: false,
                }),
                items: Notify::new(),
                spaces: Notify::new(),
                closed: AtomicBool::new(false),
            }),
        }
    }

    /// 排队任务控制、进度、删除或同步 ACK 等高优先级消息。
    pub async fn send_high(&self, value: T) -> Result<(), TransportError> {
        self.send(value, true).await
    }

    /// 排队原图、联系表或全量快照块等低优先级消息。
    pub async fn send_low(&self, value: T) -> Result<(), TransportError> {
        self.send(value, false).await
    }

    /// 取出下一条待写消息；低优先级之间始终重新检查高优先级队列。
    pub async fn next(&self) -> Option<T> {
        loop {
            let notified = self.inner.items.notified();
            {
                let mut state = self.inner.state.lock().await;
                let next = state
                    .selected_low
                    .take()
                    .or_else(|| state.high.pop_front())
                    .or_else(|| state.low.pop_front());
                if next.is_some() {
                    self.inner.spaces.notify_waiters();
                    return next;
                }
                if state.closed || self.inner.closed.load(Ordering::Acquire) {
                    return None;
                }
            }
            notified.await;
        }
    }

    /// 关闭队列并唤醒所有等待的发送者和消费者。
    pub async fn close(&self) {
        self.close_now();
        self.inner.state.lock().await.closed = true;
    }

    /// 从同步 Drop 边界标记关闭并唤醒读写循环；已排队消息仍按原顺序排空。
    pub fn close_now(&self) {
        self.inner.closed.store(true, Ordering::Release);
        self.inner.items.notify_waiters();
        self.inner.spaces.notify_waiters();
    }

    async fn send(&self, value: T, high_priority: bool) -> Result<(), TransportError> {
        loop {
            if self.inner.closed.load(Ordering::Acquire) {
                return Err(TransportError::ConnectionClosed);
            }
            let notified = self.inner.spaces.notified();
            {
                let mut state = self.inner.state.lock().await;
                if state.closed || self.inner.closed.load(Ordering::Acquire) {
                    return Err(TransportError::ConnectionClosed);
                }
                let has_space = if high_priority {
                    state.high.len() < state.high_capacity
                } else {
                    state.low.len() + usize::from(state.selected_low.is_some()) < state.low_capacity
                };
                if has_space {
                    if high_priority {
                        state.high.push_back(value);
                    } else if state.selected_low.is_none()
                        && state.high.is_empty()
                        && state.low.is_empty()
                    {
                        // 视为写循环已经选择的首块；随后到达的控制消息只能抢占下一块。
                        state.selected_low = Some(value);
                    } else {
                        state.low.push_back(value);
                    }
                    self.inner.items.notify_one();
                    return Ok(());
                }
            }
            notified.await;
        }
    }
}

//! 基础计算阶段守卫：只记录真实 future、许可和归并边界，不改变调度策略。

use std::sync::{
    Arc, Mutex, Weak,
    atomic::{AtomicU8, AtomicUsize, Ordering},
};

use tokio::sync::{OwnedSemaphorePermit, Semaphore};

/// Hash future 尚未取得真实磁盘许可。
const HASH_WAITING: u8 = 0;
/// Hash future 已取得真实磁盘许可并进入读取。
const HASH_READING: u8 = 1;
/// Hash future 已返回但尚未由协调器归并。
const HASH_COMPLETED_UNJOINED: u8 = 2;
/// 守卫已经清理，禁止再次计数。
const PHASE_RELEASED: u8 = 3;

/// 媒体许可 future 正在等待返回。
const MEDIA_WAITING: u8 = 0;
/// 媒体许可 future 已完成但不携带真实许可。
const MEDIA_READY: u8 = 1;
/// 媒体许可 future 已完成并携带真实许可。
const MEDIA_PERMIT_READY: u8 = 2;

/// Hash 三阶段的聚合计数，生命周期由一次基础计算协调器持有。
struct HashPhaseCounters {
    /// 统一串行化单项状态与聚合计数器，避免 snapshot 观察迁移中间态。
    lock: Mutex<()>,
    /// 已创建但尚未取得真实 Hash 读取许可的 future 数量。
    waiting_permit: AtomicUsize,
    /// 已取得许可并正在读取 MD5 的 future 数量。
    reading: AtomicUsize,
    /// future 已返回、结果尚未由协调器 join 的数量。
    completed_unjoined: AtomicUsize,
}

impl HashPhaseCounters {
    /// 创建全为零的 Hash 阶段计数器。
    fn new() -> Self {
        Self {
            lock: Mutex::new(()),
            waiting_permit: AtomicUsize::new(0),
            reading: AtomicUsize::new(0),
            completed_unjoined: AtomicUsize::new(0),
        }
    }

    /// 读取一个稳定快照，供协调器在状态迁移后投影遥测。
    fn snapshot(&self) -> HashPhaseSnapshot {
        let _lock = self.lock.lock().expect("Hash phase lock 不得中毒");
        HashPhaseSnapshot {
            waiting_permit: self.waiting_permit.load(Ordering::SeqCst),
            reading: self.reading.load(Ordering::SeqCst),
            completed_unjoined: self.completed_unjoined.load(Ordering::SeqCst),
        }
    }

    /// 返回一个阶段对应的聚合计数器；调用方必须已持有统一锁。
    fn counter(&self, phase: u8) -> &AtomicUsize {
        match phase {
            HASH_WAITING => &self.waiting_permit,
            HASH_READING => &self.reading,
            HASH_COMPLETED_UNJOINED => &self.completed_unjoined,
            PHASE_RELEASED => unreachable!("已释放阶段没有计数器"),
            _ => unreachable!("未知 Hash 阶段"),
        }
    }

    /// 在线性化边界内增加一个新守卫的 waiting 计数。
    fn add(&self, phase: u8) {
        let _lock = self.lock.lock().expect("Hash phase lock 不得中毒");
        increment(self.counter(phase));
    }
}

/// Hash 阶段当前计数；三个值之和必须等于 Hash JoinSet 长度。
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(super) struct HashPhaseSnapshot {
    /// 等待读取许可的 future 数量。
    pub(super) waiting_permit: usize,
    /// 正在读取的 future 数量。
    pub(super) reading: usize,
    /// 已完成但尚未归并的 future 数量。
    pub(super) completed_unjoined: usize,
}

impl HashPhaseSnapshot {
    /// 返回三个阶段的总数，用于校验 JoinSet 守恒。
    pub(super) const fn total(self) -> usize {
        self.waiting_permit + self.reading + self.completed_unjoined
    }
}

/// Hash 守卫与读取边界信号共享的单项状态。
struct HashPhaseInner {
    /// 单项当前阶段；释放后不可再次迁移。
    state: AtomicU8,
    /// 协调器级阶段计数器。
    counters: Arc<HashPhaseCounters>,
}

/// 一个 Hash future 的 RAII 阶段守卫。
pub(super) struct HashPhaseGuard {
    /// 共享状态保证 future、读取信号和归并边界只清理一次。
    inner: Arc<HashPhaseInner>,
}

impl HashPhaseGuard {
    /// 根据共享 tracker 创建等待许可阶段的守卫。
    fn new(counters: Arc<HashPhaseCounters>) -> Self {
        counters.add(HASH_WAITING);
        Self {
            inner: Arc::new(HashPhaseInner {
                state: AtomicU8::new(HASH_WAITING),
                counters,
            }),
        }
    }

    /// 生成弱引用信号；ScheduledFileReader 在真实许可成功后调用它。
    pub(super) fn read_started_signal(&self) -> HashReadStartedSignal {
        HashReadStartedSignal {
            inner: Arc::downgrade(&self.inner),
        }
    }

    /// 标记读取 future 已返回但协调器尚未 join，供归并边界持有。
    pub(super) fn mark_completed_unjoined(&self) {
        // 许可取得失败的 future 没有 reading 阶段，但返回值仍需经过归并边界。
        if !self.inner.transition(HASH_READING, HASH_COMPLETED_UNJOINED) {
            self.inner.transition(HASH_WAITING, HASH_COMPLETED_UNJOINED);
        }
    }
}

impl Drop for HashPhaseGuard {
    /// 在协调器归并、取消或 future 异常退出时精确释放当前阶段。
    fn drop(&mut self) {
        self.inner.release();
    }
}

impl HashPhaseInner {
    /// 在共享锁内迁移单项阶段并同步递减旧计数、递增新计数。
    fn transition(&self, from: u8, to: u8) -> bool {
        let _lock = self.counters.lock.lock().expect("Hash phase lock 不得中毒");
        if self
            .state
            .compare_exchange(from, to, Ordering::SeqCst, Ordering::SeqCst)
            .is_ok()
        {
            decrement(self.counters.counter(from));
            increment(self.counters.counter(to));
            true
        } else {
            false
        }
    }

    /// 在共享锁内把尚存阶段置为 Released；重复 Drop 不会下溢。
    fn release(&self) {
        let _lock = self.counters.lock.lock().expect("Hash phase lock 不得中毒");
        let phase = self.state.swap(PHASE_RELEASED, Ordering::SeqCst);
        if phase != PHASE_RELEASED {
            decrement(self.counters.counter(phase));
        }
    }
}

/// Hash 读取器取得真实磁盘许可后调用的一次性阶段信号。
#[doc(hidden)]
#[derive(Clone)]
pub struct HashReadStartedSignal {
    /// 弱引用避免读取器信号延长 future 守卫生命周期。
    inner: Weak<HashPhaseInner>,
}

impl HashReadStartedSignal {
    /// 在守卫仍存活且仍等待许可时进入 Reading；重复信号安全忽略。
    pub fn mark_reading(&self) {
        if let Some(inner) = self.inner.upgrade() {
            inner.transition(HASH_WAITING, HASH_READING);
        }
    }
}

/// 一个媒体许可 future 的三态共享计数。
struct MediaPhaseCounters {
    /// 统一串行化单项状态与三个聚合计数，保证 ready 子集快照一致。
    lock: Mutex<()>,
    /// 尚未完成许可 future 的数量。
    waiting: AtomicUsize,
    /// future 已完成但协调器尚未 join 的数量。
    ready: AtomicUsize,
    /// ready 中实际携带 Some(permit) 的数量。
    permit_ready: AtomicUsize,
}

impl MediaPhaseCounters {
    /// 创建全为零的媒体阶段计数。
    fn new() -> Self {
        Self {
            lock: Mutex::new(()),
            waiting: AtomicUsize::new(0),
            ready: AtomicUsize::new(0),
            permit_ready: AtomicUsize::new(0),
        }
    }

    /// 读取媒体 future 的当前阶段快照。
    fn snapshot(&self) -> MediaAcquirePhaseSnapshot {
        let _lock = self.lock.lock().expect("Media phase lock 不得中毒");
        MediaAcquirePhaseSnapshot {
            waiting: self.waiting.load(Ordering::SeqCst),
            ready: self.ready.load(Ordering::SeqCst),
            permit_ready: self.permit_ready.load(Ordering::SeqCst),
        }
    }

    /// 返回一个媒体阶段对应的聚合计数器；调用方必须已持有统一锁。
    fn counter(&self, phase: u8) -> &AtomicUsize {
        match phase {
            MEDIA_WAITING => &self.waiting,
            MEDIA_READY => &self.ready,
            MEDIA_PERMIT_READY => &self.permit_ready,
            PHASE_RELEASED => unreachable!("已释放媒体阶段没有计数器"),
            _ => unreachable!("未知媒体阶段"),
        }
    }

    /// 在线性化边界内增加 waiting future。
    fn add(&self, phase: u8) {
        let _lock = self.lock.lock().expect("Media phase lock 不得中毒");
        increment(self.counter(phase));
    }
}

/// 媒体许可 future 的 waiting/ready/permit-ready 聚合计数。
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub(super) struct MediaAcquirePhaseSnapshot {
    /// future 尚未返回的数量。
    pub(super) waiting: usize,
    /// future 已返回但尚未由协调器归并的数量。
    pub(super) ready: usize,
    /// ready 中持有真实许可的数量。
    pub(super) permit_ready: usize,
}

impl MediaAcquirePhaseSnapshot {
    /// 返回 JoinSet 中 waiting 与 ready 的守恒总数。
    pub(super) const fn total(self) -> usize {
        self.waiting + self.ready
    }
}

/// 媒体许可 future 的 RAII 阶段守卫。
pub(super) struct MediaAcquirePhaseGuard {
    /// 单项状态决定 Drop 时是否同时释放 permit-ready 子集。
    inner: Arc<MediaPhaseInner>,
}

/// Media 许可 future 的单项状态；与聚合计数共享同一同步边界。
struct MediaPhaseInner {
    /// 单项当前阶段；Released 后不可重复清理。
    state: AtomicU8,
    /// 协调器级媒体阶段计数器。
    counters: Arc<MediaPhaseCounters>,
}

impl MediaAcquirePhaseGuard {
    /// 创建并计入 waiting 阶段的媒体许可守卫。
    fn new(counters: Arc<MediaPhaseCounters>) -> Self {
        counters.add(MEDIA_WAITING);
        Self {
            inner: Arc::new(MediaPhaseInner {
                state: AtomicU8::new(MEDIA_WAITING),
                counters,
            }),
        }
    }

    /// 标记 future 已完成；只有 Some(permit) 进入 permit-ready 子集。
    pub(super) fn mark_ready(&self, has_permit: bool) {
        let target = if has_permit {
            MEDIA_PERMIT_READY
        } else {
            MEDIA_READY
        };
        let _lock = self
            .inner
            .counters
            .lock
            .lock()
            .expect("Media phase lock 不得中毒");
        if self
            .inner
            .state
            .compare_exchange(MEDIA_WAITING, target, Ordering::SeqCst, Ordering::SeqCst)
            .is_ok()
        {
            decrement(&self.inner.counters.waiting);
            increment(&self.inner.counters.ready);
            if has_permit {
                increment(&self.inner.counters.permit_ready);
            }
        }
    }
}

impl Drop for MediaAcquirePhaseGuard {
    /// 在共享锁内于归并、取消或 future 异常退出时精确清理计数。
    fn drop(&mut self) {
        let _lock = self
            .inner
            .counters
            .lock
            .lock()
            .expect("Media phase lock 不得中毒");
        let phase = self.inner.state.swap(PHASE_RELEASED, Ordering::SeqCst);
        match phase {
            MEDIA_WAITING => decrement(self.inner.counters.counter(MEDIA_WAITING)),
            MEDIA_READY => decrement(self.inner.counters.counter(MEDIA_READY)),
            MEDIA_PERMIT_READY => {
                decrement(self.inner.counters.counter(MEDIA_READY));
                decrement(self.inner.counters.counter(MEDIA_PERMIT_READY));
            }
            PHASE_RELEASED => {}
            _ => unreachable!("未知媒体许可阶段"),
        }
    }
}

/// Hash 阶段守卫的共享工厂，生命周期覆盖一个 BaseCompute 协调器。
pub(super) struct HashPhaseTracker {
    /// 所有 Hash guard 共享的聚合计数器。
    counters: Arc<HashPhaseCounters>,
}

impl HashPhaseTracker {
    /// 创建空 tracker；不预填任何未发生的阶段。
    pub(super) fn new() -> Self {
        Self {
            counters: Arc::new(HashPhaseCounters::new()),
        }
    }

    /// 创建一个初始为 waiting 的 Hash future 守卫。
    pub(super) fn guard(&self) -> HashPhaseGuard {
        HashPhaseGuard::new(Arc::clone(&self.counters))
    }

    /// 返回当前 Hash 阶段聚合值。
    pub(super) fn snapshot(&self) -> HashPhaseSnapshot {
        self.counters.snapshot()
    }
}

/// 媒体许可阶段守卫的共享工厂，生命周期覆盖一个 BaseCompute 协调器。
pub(super) struct MediaAcquirePhaseTracker {
    /// 所有媒体许可 future 共享的聚合计数器。
    counters: Arc<MediaPhaseCounters>,
}

impl MediaAcquirePhaseTracker {
    /// 创建空 tracker；后续 future 显式进入 waiting。
    pub(super) fn new() -> Self {
        Self {
            counters: Arc::new(MediaPhaseCounters::new()),
        }
    }

    /// 创建一个初始为 waiting 的媒体许可守卫。
    pub(super) fn guard(&self) -> MediaAcquirePhaseGuard {
        MediaAcquirePhaseGuard::new(Arc::clone(&self.counters))
    }

    /// 返回当前媒体许可阶段聚合值。
    pub(super) fn snapshot(&self) -> MediaAcquirePhaseSnapshot {
        self.counters.snapshot()
    }
}

/// 限制 Hash 结果从读取完成到离开内容供给阶段的真实内存所有权。
#[derive(Clone)]
pub(super) struct ContentOutputCredits {
    /// 由 Tokio semaphore 承担 credit 的 RAII 归还，避免异常路径遗漏。
    semaphore: Arc<Semaphore>,
    /// 产品允许同时滞留的 Hash/内容输出数量。
    capacity: usize,
}

impl ContentOutputCredits {
    /// 创建固定容量的独立 output credit 池；不复用远端缓存查询许可。
    pub(super) fn new(capacity: usize) -> Self {
        Self {
            semaphore: Arc::new(Semaphore::new(capacity)),
            capacity,
        }
    }

    /// 非阻塞取得一个随文件移动的 credit；没有容量时保留 Hash refill token。
    pub(super) fn try_acquire(&self) -> Option<ContentOutputCredit> {
        Arc::clone(&self.semaphore)
            .try_acquire_owned()
            .ok()
            .map(|permit| ContentOutputCredit { _permit: permit })
    }

    /// 返回池当前还可取得的 credit 数量，用于 Hash admission 背压。
    pub(super) fn available_permits(&self) -> usize {
        self.semaphore.available_permits()
    }

    /// 返回已被真实文件所有权持有的 credit 数量，供遥测守恒校验。
    pub(super) fn owned(&self) -> usize {
        self.capacity.saturating_sub(self.available_permits())
    }

    /// 返回固定容量，供协议 current/capacity 校验使用。
    pub(super) const fn capacity(&self) -> usize {
        self.capacity
    }
}

/// 随单个文件跨越 Hash、内容查询和媒体准备阶段的 output credit。
pub(super) struct ContentOutputCredit {
    /// Drop 时自动归还到所属 output credit 池。
    _permit: OwnedSemaphorePermit,
}

/// 限制尚未收到权威 Worker Started 的解码候选总数。
#[derive(Clone)]
pub(super) struct DecodeCredits {
    /// 由 Tokio semaphore 负责 credit 的公平取得与 RAII 归还。
    semaphore: Arc<Semaphore>,
    /// 受检后的固定 2W 容量，用于遥测和守恒校验。
    capacity: usize,
}

impl DecodeCredits {
    /// 创建固定容量的解码 credit 池；容量必须已由调用方按 2W 校验。
    pub(super) fn new(capacity: usize) -> Self {
        Self {
            semaphore: Arc::new(Semaphore::new(capacity)),
            capacity,
        }
    }

    /// 非阻塞取得一枚随候选生命周期移动的解码 credit。
    pub(super) fn try_acquire(&self) -> Option<DecodeCredit> {
        Arc::clone(&self.semaphore)
            .try_acquire_owned()
            .ok()
            .map(|permit| DecodeCredit { _permit: permit })
    }

    /// 返回当前仍由 pending/media/dispatch/start-pending 持有的 credit 数量。
    pub(super) fn owned(&self) -> usize {
        self.capacity
            .saturating_sub(self.semaphore.available_permits())
    }

    /// 返回 credit 的固定容量，供 runtime current/capacity 发布。
    pub(super) const fn capacity(&self) -> usize {
        self.capacity
    }

    /// 返回尚可取得的 credit 数量，供 content 游标避免无效消费。
    pub(super) fn available_permits(&self) -> usize {
        self.semaphore.available_permits()
    }
}

/// 随 pending、媒体许可、派发中和 Started 待归并状态移动的单项解码所有权。
pub(super) struct DecodeCredit {
    /// Drop 时自动归还到所属 2W credit 池。
    _permit: OwnedSemaphorePermit,
}

/// Hash 补位所处的一次性预热或永久稳定阶段。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum HashRefillPhase {
    /// 启动阶段使用预置令牌逐步填充 Hash 窗口。
    Warmup,
    /// 第一次媒体请求后永久进入按 departure 补位的稳定阶段。
    Stable,
}

/// 控制每个 select epoch 最多启动一个 Hash 的补位令牌状态。
pub(super) struct HashRefillController {
    /// 令牌硬上限，与 Hash task capacity 一致。
    capacity: usize,
    /// 当前尚未消费的可用令牌。
    available: usize,
    /// 当前是预热还是永久稳定阶段。
    phase: HashRefillPhase,
    /// 上游已权威确认为空，后续不再 claim 或补位。
    input_exhausted: bool,
    /// 上游暂时无项目但仍可能发布，避免无意义的忙轮询。
    waiting_for_upstream_publish: bool,
    /// lookup producer 是否已永久关闭；只有关闭后的空 claim 才能耗尽。
    upstream_closed: bool,
}

impl HashRefillController {
    /// 创建预热状态；初始令牌数等于 Hash 容量。
    pub(super) fn new(capacity: usize) -> Self {
        Self {
            capacity,
            available: capacity,
            phase: HashRefillPhase::Warmup,
            input_exhausted: false,
            waiting_for_upstream_publish: false,
            upstream_closed: false,
        }
    }

    /// 判断当前是否值得尝试一次 SQLite claim；不代表已经拥有 task slot/credit。
    pub(super) fn can_attempt_claim(&self) -> bool {
        !self.input_exhausted && !self.waiting_for_upstream_publish && self.available > 0
    }

    /// 在 claim 成功且 Hash future 已 spawn 后精确消费一个令牌。
    pub(super) fn consume_after_started(&mut self) -> bool {
        if self.available == 0 {
            return false;
        }
        self.available -= 1;
        true
    }

    /// 观察一次权威 claim 为空；开放上游等待发布，关闭上游才进入耗尽。
    pub(super) fn observe_empty_claim(&mut self) {
        if self.upstream_closed {
            self.input_exhausted = true;
            self.waiting_for_upstream_publish = false;
            self.available = 0;
        } else if !self.input_exhausted {
            self.waiting_for_upstream_publish = true;
        }
    }

    /// 上游成功发布可领取项后解除 open-empty 等待状态。
    pub(super) fn on_upstream_item_published(&mut self) {
        if !self.input_exhausted {
            self.waiting_for_upstream_publish = false;
        }
    }

    /// 记录 lookup producer 永久关闭；下一次空 claim 仍是最终权威判断。
    pub(super) fn on_upstream_closed(&mut self) {
        self.upstream_closed = true;
        self.waiting_for_upstream_publish = false;
    }

    /// 文件离开内容供给阶段时产生至多一个替代令牌；取消路径不调用此方法。
    pub(super) fn on_content_departed(&mut self, departure: ContentDeparture) {
        if self.input_exhausted {
            return;
        }
        if self.phase == HashRefillPhase::Warmup
            && matches!(departure, ContentDeparture::MediaRequested)
        {
            self.phase = HashRefillPhase::Stable;
            // 转换时清空未消费的 warmup token，当前 departure 只贡献一次稳定令牌。
            self.available = 0;
        }
        self.available = self.available.saturating_add(1).min(self.capacity);
    }

    /// 任务取消或异常收尾时清空控制状态，不伪造任何 departure 补位。
    pub(super) fn finish(&mut self) {
        self.input_exhausted = true;
        self.waiting_for_upstream_publish = false;
        self.available = 0;
    }

    /// 返回当前可用 token 数量。
    pub(super) const fn available(&self) -> usize {
        self.available
    }

    /// 返回 token 硬上限。
    pub(super) const fn capacity(&self) -> usize {
        self.capacity
    }

    /// 返回当前阶段，供状态机行为测试与运行时诊断使用。
    pub(super) const fn phase(&self) -> HashRefillPhase {
        self.phase
    }

    /// 返回是否已权威耗尽输入。
    pub(super) const fn input_exhausted(&self) -> bool {
        self.input_exhausted
    }

    /// 返回是否等待上游 publish 唤醒。
    pub(super) const fn waiting_for_upstream_publish(&self) -> bool {
        self.waiting_for_upstream_publish
    }
}

/// 文件离开内容供给阶段的唯一原因；任务取消只 Drop credit，不产生 token。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum ContentDeparture {
    /// 首次注册媒体许可请求，credit 从内容阶段离开。
    MediaRequested,
    /// 缓存命中、Hash 失败或媒体许可失败进入单项终态。
    TerminalItem,
}

/// 单个 select epoch 的 Hash admission 结果；只有 Started 会消费 refill token。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum HashStartResult {
    /// 已取得 task slot、output credit 并成功 spawn 一个 Hash future。
    Started,
    /// 当前 Hash future 数量已经达到硬上限。
    NoTaskSlot,
    /// 没有可用 output credit，令牌保持不变。
    NoOutputCredit,
    /// 没有可消费的 refill token。
    NoToken,
    /// 上游开放但暂时没有可领取项目。
    WaitingForUpstream,
    /// 上游已关闭且权威空 claim，后续不再领取。
    InputExhausted,
}

/// 计数增加 helper，集中保持原子顺序。
fn increment(counter: &AtomicUsize) {
    counter.fetch_add(1, Ordering::SeqCst);
}

/// 计数减少 helper，重复释放时饱和到零而不发生下溢。
fn decrement(counter: &AtomicUsize) {
    let _ = counter.fetch_update(Ordering::SeqCst, Ordering::SeqCst, |value| {
        Some(value.saturating_sub(1))
    });
}

#[cfg(test)]
mod tests {
    use std::{
        sync::mpsc,
        sync::{
            Arc,
            atomic::{AtomicBool, Ordering},
        },
        thread,
        time::Duration,
    };

    use super::{
        ContentDeparture, ContentOutputCredits, DecodeCredits, HashPhaseTracker,
        HashRefillController, HashRefillPhase, MediaAcquirePhaseTracker,
    };

    /// 2W credit 池必须按 RAII 持有，直到测试显式模拟 Started 后 Drop。
    #[test]
    fn decode_credit_moves_through_pending_and_started_release() {
        let credits = DecodeCredits::new(4);
        let first = credits
            .try_acquire()
            .expect("pending 必须取得第一枚 credit");
        let second = credits
            .try_acquire()
            .expect("pending 必须取得第二枚 credit");
        assert_eq!(credits.owned(), 2);
        drop(first);
        assert_eq!(credits.owned(), 1, "Started 释放必须只归还一枚 credit");
        drop(second);
        assert_eq!(credits.owned(), 0, "终态后全部 credit 必须归还");
    }

    /// 真实 credit 在 pending→media→dispatch→start-pending 迁移时始终只占一份。
    #[test]
    fn decode_credit_moves_pending_media_dispatch_start_pending() {
        let credits = DecodeCredits::new(1);
        let pending = credits.try_acquire().expect("pending 必须取得 credit");
        assert_eq!(credits.owned(), 1);
        let media = pending;
        assert_eq!(credits.owned(), 1, "进入 media acquiring 不得复制 credit");
        let dispatch = media;
        assert_eq!(credits.owned(), 1, "进入 dispatching 不得提前释放");
        let start_pending = dispatch;
        assert_eq!(credits.owned(), 1, "ACK 后 start-pending 仍需持有 credit");
        drop(start_pending);
        assert_eq!(credits.owned(), 0, "权威 Started 后才归还 credit");
    }

    /// 派发 ACK 不是 Started；在 ACK 返回和权威 Started 之间 credit 必须保持占用。
    #[test]
    fn dispatch_ack_keeps_credit_until_authoritative_started() {
        let credits = DecodeCredits::new(1);
        let credit = credits.try_acquire().expect("dispatch 必须取得 credit");
        assert_eq!(credits.available_permits(), 0);
        drop(credit);
        assert_eq!(credits.available_permits(), 1);
    }

    /// 媒体许可失败释放候选 credit 恰好一次，重复 Drop 不得制造额外可用量。
    #[test]
    fn media_acquire_failure_releases_credit_once() {
        let credits = DecodeCredits::new(1);
        let credit = credits.try_acquire().expect("媒体许可前必须取得 credit");
        drop(credit);
        assert_eq!(credits.owned(), 0);
        let reacquired = credits
            .try_acquire()
            .expect("释放后必须能够再次取得 credit");
        assert_eq!(credits.owned(), 1);
        drop(reacquired);
    }

    /// Worker 派发失败时 pending dispatch 的 Drop 必须归还 decode credit。
    #[test]
    fn dispatch_failure_releases_credit_once() {
        let credits = DecodeCredits::new(1);
        let credit = credits.try_acquire().expect("派发前必须取得 credit");
        drop(credit);
        assert_eq!(credits.available_permits(), 1);
    }

    /// Started 前收到终态时，活动项的 credit 必须随终态移除恰好释放一次。
    #[test]
    fn terminal_before_started_releases_credit_once() {
        let credits = DecodeCredits::new(1);
        let credit = credits
            .try_acquire()
            .expect("start-pending 必须持有 credit");
        drop(credit);
        assert_eq!(credits.available_permits(), 1);
    }

    /// 任务取消只依靠 RAII 清理所有 decode credit，不产生新的任务或 token。
    #[test]
    fn task_cancel_releases_all_decode_credit() {
        let credits = DecodeCredits::new(2);
        let first = credits.try_acquire().unwrap();
        let second = credits.try_acquire().unwrap();
        drop(first);
        drop(second);
        assert_eq!(credits.owned(), 0);
        assert_eq!(credits.available_permits(), credits.capacity());
    }

    /// 预热阶段必须一次拥有完整 Hash 容量的可用令牌。
    #[test]
    fn warmup_starts_with_hash_capacity_tokens() {
        let refill = HashRefillController::new(4);
        assert_eq!(refill.phase(), HashRefillPhase::Warmup);
        assert_eq!(refill.available(), 4);
        assert!(!refill.input_exhausted());
    }

    /// 只有真实启动成功才能消耗一个令牌，重复尝试不会隐式扣减。
    #[test]
    fn successful_spawn_consumes_exactly_one_token() {
        let mut refill = HashRefillController::new(3);
        assert!(refill.consume_after_started());
        assert_eq!(refill.available(), 2);
        assert!(refill.consume_after_started());
        assert_eq!(refill.available(), 1);
    }

    /// 没有 task slot 或 output credit 时，令牌必须留在控制状态中。
    #[test]
    fn missing_task_slot_or_output_credit_keeps_token() {
        let refill = HashRefillController::new(2);
        let credits = ContentOutputCredits::new(0);
        assert!(credits.try_acquire().is_none());
        assert_eq!(refill.available(), 2);
        assert!(refill.can_attempt_claim());
    }

    /// 上游开放但暂时无项目时进入等待发布状态，不能忙轮询或清空令牌。
    #[test]
    fn open_upstream_empty_claim_waits_for_publish_without_spinning() {
        let mut refill = HashRefillController::new(2);
        refill.observe_empty_claim();
        assert!(refill.waiting_for_upstream_publish());
        assert_eq!(refill.available(), 2);
        assert!(!refill.input_exhausted());
        assert!(!refill.can_attempt_claim());
        refill.on_upstream_item_published();
        assert!(!refill.waiting_for_upstream_publish());
        assert!(refill.can_attempt_claim());
    }

    /// 上游已关闭且权威空领取后才允许清空令牌并标记输入耗尽。
    #[test]
    fn closed_upstream_empty_claim_clears_tokens_and_marks_exhausted() {
        let mut refill = HashRefillController::new(2);
        let mut claim_attempts = 0;
        assert!(refill.can_attempt_claim());
        claim_attempts += 1;
        refill.observe_empty_claim();
        assert!(!refill.input_exhausted());
        assert_eq!(refill.available(), 2);
        refill.on_upstream_closed();
        assert!(refill.can_attempt_claim());
        claim_attempts += 1;
        refill.observe_empty_claim();
        assert_eq!(
            claim_attempts, 2,
            "closed-empty 必须保留最后一次权威空 claim"
        );
        assert!(refill.input_exhausted());
        assert_eq!(refill.available(), 0);
        assert!(!refill.can_attempt_claim());
    }

    /// 第一次媒体离开会永久切换稳定态，并只为当前文件生成一个替代令牌。
    #[test]
    fn first_media_departure_clears_unused_warmup_and_adds_one_stable_token() {
        let mut refill = HashRefillController::new(4);
        assert!(refill.consume_after_started());
        assert!(refill.consume_after_started());
        refill.on_content_departed(ContentDeparture::MediaRequested);
        assert_eq!(refill.phase(), HashRefillPhase::Stable);
        assert_eq!(refill.available(), 1);
        refill.on_content_departed(ContentDeparture::MediaRequested);
        assert_eq!(refill.available(), 2);
    }

    /// 从非满 token 状态开始，缓存命中与单项失败各只产生一个替代令牌。
    #[test]
    fn cache_hit_and_item_failure_each_add_at_most_one_token() {
        let mut refill = HashRefillController::new(4);
        assert!(refill.consume_after_started());
        assert!(refill.consume_after_started());
        assert!(refill.consume_after_started());
        assert_eq!(refill.available(), 1);
        refill.on_content_departed(ContentDeparture::TerminalItem);
        assert_eq!(refill.available(), 2, "cache hit 只能增加一枚 token");
        assert!(refill.consume_after_started());
        refill.on_content_departed(ContentDeparture::TerminalItem);
        assert_eq!(refill.available(), 2, "item failure 只能增加一枚 token");
    }

    /// 取消只让 RAII credit 归还，不可借取消事件伪造补位令牌。
    #[test]
    fn task_cancellation_returns_credit_without_adding_token() {
        let refill = HashRefillController::new(1);
        let credits = ContentOutputCredits::new(1);
        let credit = credits.try_acquire().expect("必须取得测试 credit");
        assert_eq!(credits.available_permits(), 0);
        drop(credit);
        assert_eq!(credits.available_permits(), 1);
        assert_eq!(refill.available(), 1);
    }

    /// 所有 departure 路径都必须对令牌容量做上限保护。
    #[test]
    fn token_count_never_exceeds_hash_capacity() {
        let mut refill = HashRefillController::new(2);
        for _ in 0..16 {
            refill.on_content_departed(ContentDeparture::TerminalItem);
        }
        assert_eq!(refill.available(), 2);
        assert!(refill.available() <= refill.capacity());
    }

    /// 验证 Hash future 在等待许可、实际读取和完成未归并之间单调迁移。
    #[test]
    fn hash_guard_transitions_are_exact_and_terminal_drop_is_idempotent() {
        let tracker = HashPhaseTracker::new();
        let guard = tracker.guard();
        assert_eq!(tracker.snapshot().waiting_permit, 1);

        let started = guard.read_started_signal();
        started.mark_reading();
        assert_eq!(tracker.snapshot().reading, 1);

        guard.mark_completed_unjoined();
        assert_eq!(tracker.snapshot().completed_unjoined, 1);
        guard.mark_completed_unjoined();
        drop(guard);
        assert_eq!(tracker.snapshot().total(), 0);

        let failed_before_permit = tracker.guard();
        failed_before_permit.mark_completed_unjoined();
        assert_eq!(tracker.snapshot().completed_unjoined, 1);
        drop(failed_before_permit);
        assert_eq!(tracker.snapshot().total(), 0);
    }

    /// 验证媒体 future 的错误、None 与真实许可结果都先进入 ready，只有真实许可计入子集。
    #[test]
    fn media_guard_counts_all_ready_results_and_only_real_permits() {
        let tracker = MediaAcquirePhaseTracker::new();
        let error = tracker.guard();
        let none = tracker.guard();
        let permit = tracker.guard();
        assert_eq!(tracker.snapshot().waiting, 3);

        error.mark_ready(false);
        none.mark_ready(false);
        permit.mark_ready(true);
        permit.mark_ready(false);
        let snapshot = tracker.snapshot();
        assert_eq!(snapshot.waiting, 0);
        assert_eq!(snapshot.ready, 3);
        assert_eq!(snapshot.permit_ready, 1);

        drop(error);
        drop(none);
        drop(permit);
        assert_eq!(tracker.snapshot().total(), 0);
        assert_eq!(tracker.snapshot().permit_ready, 0);
    }

    /// 并发迁移、快照和终态 Drop 压力验证阶段计数必须在线性化边界内守恒。
    #[test]
    fn concurrent_phase_transitions_keep_hash_and_media_totals_linearizable() {
        const WORKERS: usize = 24;
        const GUARDS_PER_WORKER: usize = 32;
        const SNAPSHOTS: usize = 250_000;

        /// 压力测试异常退出时同时放行 worker 的启动与释放，避免遗留自旋线程。
        struct WorkerReleaseGuard {
            /// worker 等待开始信号。
            start: Arc<AtomicBool>,
            /// worker 完成迁移后等待的释放信号。
            release: Arc<AtomicBool>,
        }

        impl WorkerReleaseGuard {
            /// 创建一个 panic-safe 的 worker 信号清理器。
            fn new(start: Arc<AtomicBool>, release: Arc<AtomicBool>) -> Self {
                Self { start, release }
            }
        }

        impl Drop for WorkerReleaseGuard {
            /// 测试断言 panic 时也唤醒所有等待中的 worker。
            fn drop(&mut self) {
                self.start.store(true, Ordering::Release);
                self.release.store(true, Ordering::Release);
            }
        }

        let hash_tracker = Arc::new(HashPhaseTracker::new());
        let (hash_ready_tx, hash_ready_rx) = mpsc::channel();
        let (hash_marked_tx, hash_marked_rx) = mpsc::channel();
        let hash_start = Arc::new(AtomicBool::new(false));
        let hash_release = Arc::new(AtomicBool::new(false));
        let _hash_release_guard =
            WorkerReleaseGuard::new(Arc::clone(&hash_start), Arc::clone(&hash_release));
        let hash_violation = Arc::new(AtomicBool::new(false));
        let mut hash_workers = Vec::with_capacity(WORKERS);
        for _ in 0..WORKERS {
            let tracker = Arc::clone(&hash_tracker);
            let ready = hash_ready_tx.clone();
            let marked = hash_marked_tx.clone();
            let start = Arc::clone(&hash_start);
            let release = Arc::clone(&hash_release);
            hash_workers.push(thread::spawn(move || {
                let guards = (0..GUARDS_PER_WORKER)
                    .map(|_| tracker.guard())
                    .collect::<Vec<_>>();
                ready
                    .send(())
                    .expect("Hash 压力线程 ready 通道不得提前关闭");
                while !start.load(Ordering::Acquire) {
                    thread::yield_now();
                }
                for guard in &guards {
                    guard.read_started_signal().mark_reading();
                    std::hint::spin_loop();
                }
                marked
                    .send(())
                    .expect("Hash 压力线程 marked 通道不得提前关闭");
                while !release.load(Ordering::Acquire) {
                    thread::yield_now();
                }
                drop(guards);
            }));
        }
        drop(hash_ready_tx);
        drop(hash_marked_tx);
        for _ in 0..WORKERS {
            hash_ready_rx
                .recv_timeout(Duration::from_secs(2))
                .expect("Hash 压力线程必须在限时内到达 ready");
        }
        hash_start.store(true, Ordering::Release);
        for _ in 0..SNAPSHOTS {
            if hash_tracker.snapshot().total() != WORKERS * GUARDS_PER_WORKER {
                hash_violation.store(true, Ordering::Relaxed);
            }
        }
        for _ in 0..WORKERS {
            hash_marked_rx
                .recv_timeout(Duration::from_secs(2))
                .expect("Hash 压力线程必须在限时内完成迁移");
        }
        hash_release.store(true, Ordering::Release);
        for worker in hash_workers {
            worker.join().expect("Hash phase 压力线程不得 panic");
        }
        assert!(
            !hash_violation.load(Ordering::Relaxed),
            "Hash phase snapshot 观察到了迁移中间态"
        );
        assert_eq!(hash_tracker.snapshot().total(), 0);

        let media_tracker = Arc::new(MediaAcquirePhaseTracker::new());
        let (media_ready_tx, media_ready_rx) = mpsc::channel();
        let (media_marked_tx, media_marked_rx) = mpsc::channel();
        let media_start = Arc::new(AtomicBool::new(false));
        let media_release = Arc::new(AtomicBool::new(false));
        let _media_release_guard =
            WorkerReleaseGuard::new(Arc::clone(&media_start), Arc::clone(&media_release));
        let media_violation = Arc::new(AtomicBool::new(false));
        let mut media_workers = Vec::with_capacity(WORKERS);
        for worker_index in 0..WORKERS {
            let tracker = Arc::clone(&media_tracker);
            let ready = media_ready_tx.clone();
            let marked = media_marked_tx.clone();
            let start = Arc::clone(&media_start);
            let release = Arc::clone(&media_release);
            media_workers.push(thread::spawn(move || {
                let guards = (0..GUARDS_PER_WORKER)
                    .map(|_| tracker.guard())
                    .collect::<Vec<_>>();
                ready
                    .send(())
                    .expect("Media 压力线程 ready 通道不得提前关闭");
                while !start.load(Ordering::Acquire) {
                    thread::yield_now();
                }
                for (guard_index, guard) in guards.iter().enumerate() {
                    guard.mark_ready((worker_index + guard_index) % 2 == 0);
                    std::hint::spin_loop();
                }
                marked
                    .send(())
                    .expect("Media 压力线程 marked 通道不得提前关闭");
                while !release.load(Ordering::Acquire) {
                    thread::yield_now();
                }
                drop(guards);
            }));
        }
        drop(media_ready_tx);
        drop(media_marked_tx);
        for _ in 0..WORKERS {
            media_ready_rx
                .recv_timeout(Duration::from_secs(2))
                .expect("Media 压力线程必须在限时内到达 ready");
        }
        media_start.store(true, Ordering::Release);
        for _ in 0..SNAPSHOTS {
            let snapshot = media_tracker.snapshot();
            if snapshot.total() != WORKERS * GUARDS_PER_WORKER
                || snapshot.permit_ready > snapshot.ready
            {
                media_violation.store(true, Ordering::Relaxed);
            }
        }
        for _ in 0..WORKERS {
            media_marked_rx
                .recv_timeout(Duration::from_secs(2))
                .expect("Media 压力线程必须在限时内完成迁移");
        }
        media_release.store(true, Ordering::Release);
        for worker in media_workers {
            worker.join().expect("Media phase 压力线程不得 panic");
        }
        assert!(
            !media_violation.load(Ordering::Relaxed),
            "Media phase snapshot 观察到了迁移中间态"
        );
        assert_eq!(media_tracker.snapshot().total(), 0);
        assert_eq!(media_tracker.snapshot().permit_ready, 0);

        // 真实并发 Drop 期间重点检查 permit-ready 子集，旧实现会先减 ready 再减子集。
        let drop_tracker = Arc::new(MediaAcquirePhaseTracker::new());
        let (drop_ready_tx, drop_ready_rx) = mpsc::channel();
        let (drop_finished_tx, drop_finished_rx) = mpsc::channel();
        let drop_start = Arc::new(AtomicBool::new(false));
        let _drop_release_guard =
            WorkerReleaseGuard::new(Arc::clone(&drop_start), Arc::clone(&drop_start));
        let drop_violation = Arc::new(AtomicBool::new(false));
        let mut drop_workers = Vec::with_capacity(WORKERS);
        for _ in 0..WORKERS {
            let tracker = Arc::clone(&drop_tracker);
            let ready = drop_ready_tx.clone();
            let finished = drop_finished_tx.clone();
            let start = Arc::clone(&drop_start);
            drop_workers.push(thread::spawn(move || {
                let guards = (0..GUARDS_PER_WORKER)
                    .map(|_| tracker.guard())
                    .collect::<Vec<_>>();
                for guard in &guards {
                    guard.mark_ready(true);
                }
                ready
                    .send(())
                    .expect("Media Drop 压力线程 ready 通道不得提前关闭");
                while !start.load(Ordering::Acquire) {
                    thread::yield_now();
                }
                for guard in guards {
                    drop(guard);
                    std::hint::spin_loop();
                }
                finished
                    .send(())
                    .expect("Media Drop 压力线程 finished 通道不得提前关闭");
            }));
        }
        drop(drop_ready_tx);
        drop(drop_finished_tx);
        for _ in 0..WORKERS {
            drop_ready_rx
                .recv_timeout(Duration::from_secs(2))
                .expect("Media Drop 压力线程必须在限时内到达 ready");
        }
        drop_start.store(true, Ordering::Release);
        for _ in 0..SNAPSHOTS {
            let snapshot = drop_tracker.snapshot();
            if snapshot.permit_ready > snapshot.ready
                || snapshot.total() > WORKERS * GUARDS_PER_WORKER
            {
                drop_violation.store(true, Ordering::Relaxed);
            }
        }
        for _ in 0..WORKERS {
            drop_finished_rx
                .recv_timeout(Duration::from_secs(2))
                .expect("Media Drop 压力线程必须在限时内完成释放");
        }
        for worker in drop_workers {
            worker.join().expect("Media Drop 压力线程不得 panic");
        }
        assert!(
            !drop_violation.load(Ordering::Relaxed),
            "Media permit-ready 子集快照观察到了 Drop 中间态"
        );
        assert_eq!(drop_tracker.snapshot().total(), 0);
        assert_eq!(drop_tracker.snapshot().permit_ready, 0);

        // HashReadStartedSignal 的 Weak 与 guard Drop 并发竞态：信号可能先迁移，也可能观察到已释放。
        let race_tracker = Arc::new(HashPhaseTracker::new());
        let race_violation = Arc::new(AtomicBool::new(false));
        for _ in 0..(WORKERS * GUARDS_PER_WORKER) {
            let guard = race_tracker.guard();
            let started = guard.read_started_signal();
            let race_start = Arc::new(AtomicBool::new(false));
            let drop_start = Arc::clone(&race_start);
            let mark_start = Arc::clone(&race_start);
            let drop_thread = thread::spawn(move || {
                while !drop_start.load(Ordering::Acquire) {
                    thread::yield_now();
                }
                drop(guard);
            });
            let mark_thread = thread::spawn(move || {
                while !mark_start.load(Ordering::Acquire) {
                    thread::yield_now();
                }
                started.mark_reading();
            });
            race_start.store(true, Ordering::Release);
            for _ in 0..32 {
                let snapshot = race_tracker.snapshot();
                if snapshot.total() > 1 {
                    race_violation.store(true, Ordering::Relaxed);
                }
            }
            drop_thread.join().expect("Hash Drop 竞态线程不得 panic");
            mark_thread
                .join()
                .expect("HashReadStartedSignal 竞态线程不得 panic");
        }
        assert!(
            !race_violation.load(Ordering::Relaxed),
            "HashReadStartedSignal/Drop 竞态不得产生重复 ownership"
        );
        assert_eq!(race_tracker.snapshot().total(), 0);
    }
}

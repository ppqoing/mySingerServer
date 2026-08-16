package diskio

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"time"
)

const (
	absoluteMinLeaseBytes = int64(1 << 20)
	absoluteMaxLeaseBytes = int64(16 << 20)
	absoluteMaxPerDisk    = 24
	defaultHDDRandomMax   = 8
	starvationAge         = 5 * time.Second
)

var (
	ErrTaskCancelled    = errors.New("disk I/O task cancelled")
	ErrWorkerReclaimed  = errors.New("disk I/O worker reclaimed")
	ErrWorkerQueueFull  = errors.New("disk I/O worker queue full")
	ErrControllerClosed = errors.New("disk I/O controller closed")
)

type Request struct {
	RequestID  uint64
	TaskID     string
	InstanceID string
	WorkerID   int
	Disk       DiskKey
	Class      SourceClass
	WantBytes  int64
	WantSeek   bool
}

type Grant struct {
	LeaseID    uint64
	Generation uint64
	Bytes      int64
	Seeks      uint32
}

type Report struct {
	LeaseID, Generation  uint64
	TaskID, InstanceID   string
	WorkerID             int
	Disk                 DiskKey
	Bytes                int64
	Seeks                uint32
	ReadTime, WaitTime   time.Duration
	Completed, Cancelled bool
}

type Snapshot struct {
	Concurrency, BusyWorkers, IOWaitWorkers int
	EffectiveBytesPerSecond                 float64
	LeaseWait                               time.Duration
	SequentialBytes                         int64
	SeekCount                               int64
}

type Controller interface {
	Acquire(context.Context, Request) (Grant, error)
	Report(Report)
	CancelTask(taskID, instanceID string)
	ReclaimWorker(workerID int)
	Snapshot(taskID, instanceID string) Snapshot
}

type Clock interface {
	Now() time.Time
}

type ControllerOptions struct {
	Clock        Clock
	WorkerCount  int
	Policy       PolicyConfig
	Identities   map[DiskKey]Identity
	CommandQueue int
}

type WindowSample struct {
	Duration               time.Duration
	Bytes                  int64
	PreviousBytesPerSecond float64
	P95Wait                time.Duration
	PreviousP95Wait        time.Duration
	SeekCongested          bool
	Queued                 int
	BusyWorkers            int
	WorkerCount            int
	HDDRandom              bool
}

type TaskIdentity struct {
	TaskID     string
	InstanceID string
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type controller struct {
	ctx      context.Context
	commands chan any
	done     chan struct{}
	clock    Clock
	options  ControllerOptions
	token    atomic.Uint64
}

type acquireReply struct {
	grant Grant
	err   error
}

type acquireCommand struct {
	token uint64
	ctx   context.Context
	req   Request
	reply chan acquireReply
}

type cancelAcquireCommand struct {
	token uint64
	done  chan struct{}
}

type reportCommand struct {
	report Report
	done   chan struct{}
}

type cancelTaskCommand struct {
	identity TaskIdentity
	done     chan struct{}
}

type reclaimWorkerCommand struct {
	workerID int
	done     chan struct{}
}

type snapshotCommand struct {
	identity TaskIdentity
	reply    chan Snapshot
}

type pendingRequest struct {
	token    uint64
	ctx      context.Context
	req      Request
	reply    chan acquireReply
	enqueued time.Time
}

type taskQueue struct {
	items       []*pendingRequest
	lastGranted time.Time
	grants      uint64
}

type activeLease struct {
	token   uint64
	req     Request
	grant   Grant
	granted time.Time
}

type taskStats struct {
	sequentialBytes int64
	seekCount       int64
	readTime        time.Duration
	waitTime        time.Duration
	reports         int64
}

type observationWindow struct {
	started     time.Time
	bytes       int64
	seeks       int64
	waits       []time.Duration
	hddRandom   bool
	previousBPS float64
	previousP95 time.Duration
}

type diskState struct {
	identity Identity
	limit    int
	active   map[uint64]*activeLease
	queues   map[TaskIdentity]*taskQueue
	window   observationWindow
}

type ownerState struct {
	disks       map[DiskKey]*diskState
	workers     map[int]*activeLease
	leases      map[uint64]*activeLease
	generations map[TaskIdentity]uint64
	stats       map[TaskIdentity]*taskStats
	nextLease   uint64
}

func NewController(ctx context.Context, options ControllerOptions) Controller {
	if ctx == nil {
		ctx = context.Background()
	}
	options = normalizeOptions(options)
	c := &controller{
		ctx:      ctx,
		commands: make(chan any, options.CommandQueue),
		done:     make(chan struct{}),
		clock:    options.Clock,
		options:  options,
	}
	go c.run()
	return c
}

func normalizeOptions(options ControllerOptions) ControllerOptions {
	if options.Clock == nil {
		options.Clock = systemClock{}
	}
	if options.WorkerCount < 1 {
		options.WorkerCount = 1
	}
	if options.CommandQueue < 1 {
		options.CommandQueue = 64
	}
	if options.Policy.LeaseBytes <= 0 {
		options.Policy.LeaseBytes = 4 << 20
	}
	if options.Policy.MinLeaseBytes <= 0 {
		options.Policy.MinLeaseBytes = absoluteMinLeaseBytes
	}
	if options.Policy.MaxLeaseBytes <= 0 {
		options.Policy.MaxLeaseBytes = absoluteMaxLeaseBytes
	}
	if options.Policy.HDDInitial < 1 {
		options.Policy.HDDInitial = 1
	}
	if options.Policy.SSDInitial < 1 {
		options.Policy.SSDInitial = 1
	}
	if options.Policy.MaxPerDisk < 1 {
		options.Policy.MaxPerDisk = absoluteMaxPerDisk
	}
	if options.Policy.HDDRandomMax < 1 {
		options.Policy.HDDRandomMax = defaultHDDRandomMax
	}
	if options.Policy.Window < 2*time.Second {
		options.Policy.Window = 2 * time.Second
	}
	if options.Policy.IncreaseThreshold <= 0 {
		options.Policy.IncreaseThreshold = 0.05
	}
	if options.Policy.DecreaseThreshold <= 0 {
		options.Policy.DecreaseThreshold = 0.08
	}
	if options.Policy.MaxQueuedPerWorker < 1 {
		options.Policy.MaxQueuedPerWorker = 1
	}
	if options.Identities == nil {
		options.Identities = make(map[DiskKey]Identity)
	}
	return options
}

func (c *controller) Acquire(ctx context.Context, req Request) (Grant, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	token := c.token.Add(1)
	reply := make(chan acquireReply, 1)
	command := acquireCommand{token: token, ctx: ctx, req: req, reply: reply}
	select {
	case c.commands <- command:
	case <-ctx.Done():
		return Grant{}, ctx.Err()
	case <-c.done:
		return Grant{}, ErrControllerClosed
	}

	select {
	case result := <-reply:
		return result.grant, result.err
	case <-ctx.Done():
		ack := make(chan struct{})
		select {
		case c.commands <- cancelAcquireCommand{token: token, done: ack}:
			select {
			case <-ack:
			case <-c.done:
			}
		case <-c.done:
		}
		return Grant{}, ctx.Err()
	case <-c.done:
		return Grant{}, ErrControllerClosed
	}
}

func (c *controller) Report(report Report) {
	done := make(chan struct{})
	select {
	case c.commands <- reportCommand{report: report, done: done}:
		select {
		case <-done:
		case <-c.done:
		}
	case <-c.done:
	}
}

func (c *controller) CancelTask(taskID, instanceID string) {
	done := make(chan struct{})
	command := cancelTaskCommand{identity: TaskIdentity{TaskID: taskID, InstanceID: instanceID}, done: done}
	select {
	case c.commands <- command:
		select {
		case <-done:
		case <-c.done:
		}
	case <-c.done:
	}
}

func (c *controller) ReclaimWorker(workerID int) {
	done := make(chan struct{})
	select {
	case c.commands <- reclaimWorkerCommand{workerID: workerID, done: done}:
		select {
		case <-done:
		case <-c.done:
		}
	case <-c.done:
	}
}

func (c *controller) Snapshot(taskID, instanceID string) Snapshot {
	reply := make(chan Snapshot, 1)
	command := snapshotCommand{identity: TaskIdentity{TaskID: taskID, InstanceID: instanceID}, reply: reply}
	select {
	case c.commands <- command:
		select {
		case snapshot := <-reply:
			return snapshot
		case <-c.done:
			return Snapshot{}
		}
	case <-c.done:
		return Snapshot{}
	}
}

func (c *controller) run() {
	defer close(c.done)
	state := ownerState{
		disks:       make(map[DiskKey]*diskState),
		workers:     make(map[int]*activeLease),
		leases:      make(map[uint64]*activeLease),
		generations: make(map[TaskIdentity]uint64),
		stats:       make(map[TaskIdentity]*taskStats),
	}
	for {
		select {
		case <-c.ctx.Done():
			state.failPending(c.ctx.Err())
			return
		case raw := <-c.commands:
			switch command := raw.(type) {
			case acquireCommand:
				c.handleAcquire(&state, command)
			case cancelAcquireCommand:
				c.handleCancelAcquire(&state, command.token)
				close(command.done)
			case reportCommand:
				c.handleReport(&state, command.report)
				close(command.done)
			case cancelTaskCommand:
				c.handleCancelTask(&state, command.identity)
				close(command.done)
			case reclaimWorkerCommand:
				c.handleReclaimWorker(&state, command.workerID)
				close(command.done)
			case snapshotCommand:
				command.reply <- c.makeSnapshot(&state, command.identity)
			}
		}
	}
}

func (s *ownerState) failPending(err error) {
	for _, disk := range s.disks {
		for _, queue := range disk.queues {
			for _, pending := range queue.items {
				pending.reply <- acquireReply{err: err}
			}
		}
	}
}

func (c *controller) handleAcquire(state *ownerState, command acquireCommand) {
	if err := command.ctx.Err(); err != nil {
		command.reply <- acquireReply{err: err}
		return
	}
	if c.queuedForWorker(state, command.req.WorkerID) >= c.options.Policy.MaxQueuedPerWorker {
		command.reply <- acquireReply{err: ErrWorkerQueueFull}
		return
	}
	disk := c.ensureDisk(state, command.req.Disk)
	identity := identityOf(command.req)
	queue := disk.queues[identity]
	if queue == nil {
		queue = &taskQueue{}
		disk.queues[identity] = queue
	}
	queue.items = append(queue.items, &pendingRequest{
		token: command.token, ctx: command.ctx, req: command.req, reply: command.reply, enqueued: c.clock.Now(),
	})
	c.dispatch(state, disk)
}

func (c *controller) queuedForWorker(state *ownerState, workerID int) int {
	count := 0
	for _, disk := range state.disks {
		for _, queue := range disk.queues {
			for _, pending := range queue.items {
				if pending.req.WorkerID == workerID {
					count++
				}
			}
		}
	}
	return count
}

func (c *controller) ensureDisk(state *ownerState, key DiskKey) *diskState {
	if disk := state.disks[key]; disk != nil {
		return disk
	}
	identity := c.options.Identities[key]
	identity.Key = key
	initial := c.options.Policy.HDDInitial
	if identity.KnownSSD && identity.SSD {
		initial = c.options.Policy.SSDInitial
	}
	hard := c.hardLimit(false)
	if initial > hard {
		initial = hard
	}
	disk := &diskState{
		identity: identity,
		limit:    initial,
		active:   make(map[uint64]*activeLease),
		queues:   make(map[TaskIdentity]*taskQueue),
		window:   observationWindow{started: c.clock.Now()},
	}
	state.disks[key] = disk
	return disk
}

func (c *controller) hardLimit(hddRandom bool) int {
	hard := absoluteMaxPerDisk
	if c.options.WorkerCount < hard {
		hard = c.options.WorkerCount
	}
	if c.options.Policy.MaxPerDisk < hard {
		hard = c.options.Policy.MaxPerDisk
	}
	if hddRandom && c.options.Policy.HDDRandomMax < hard {
		hard = c.options.Policy.HDDRandomMax
	}
	if hard < 1 {
		return 1
	}
	return hard
}

func (c *controller) dispatch(state *ownerState, disk *diskState) {
	for len(disk.active) < disk.limit {
		c.pruneCancelled(disk)
		eligible := c.eligibleQueues(state, disk)
		if len(eligible) == 0 {
			return
		}
		identity := chooseTask(c.clock.Now(), eligible)
		queue := disk.queues[identity]
		pending := queue.items[0]
		queue.items = queue.items[1:]
		if pending.ctx.Err() != nil {
			pending.reply <- acquireReply{err: pending.ctx.Err()}
			continue
		}
		if c.isHDDRandom(disk, pending.req) && c.hddRandomActive(disk) >= c.hardLimit(true) {
			queue.items = append([]*pendingRequest{pending}, queue.items...)
			disk.queues[identity] = queue
			return
		}

		state.nextLease++
		generation := state.generations[identity]
		if generation == 0 {
			generation = 1
			state.generations[identity] = generation
		}
		grant := Grant{
			LeaseID:    state.nextLease,
			Generation: generation,
			Bytes:      c.grantBytes(pending.req.WantBytes),
		}
		if pending.req.WantSeek {
			grant.Seeks = 1
		}
		lease := &activeLease{token: pending.token, req: pending.req, grant: grant, granted: c.clock.Now()}
		disk.active[grant.LeaseID] = lease
		state.leases[grant.LeaseID] = lease
		state.workers[pending.req.WorkerID] = lease
		queue.lastGranted = c.clock.Now()
		queue.grants++
		pending.reply <- acquireReply{grant: grant}
	}
}

func (c *controller) pruneCancelled(disk *diskState) {
	for _, queue := range disk.queues {
		kept := queue.items[:0]
		for _, pending := range queue.items {
			if err := pending.ctx.Err(); err != nil {
				pending.reply <- acquireReply{err: err}
				continue
			}
			kept = append(kept, pending)
		}
		queue.items = kept
	}
}

func (c *controller) eligibleQueues(state *ownerState, disk *diskState) map[TaskIdentity]*taskQueue {
	eligible := make(map[TaskIdentity]*taskQueue)
	for identity, queue := range disk.queues {
		if len(queue.items) == 0 {
			continue
		}
		pending := queue.items[0]
		if state.workers[pending.req.WorkerID] != nil {
			continue
		}
		if c.isHDDRandom(disk, pending.req) && c.hddRandomActive(disk) >= c.hardLimit(true) {
			continue
		}
		eligible[identity] = queue
	}
	return eligible
}

func (c *controller) isHDDRandom(disk *diskState, req Request) bool {
	return req.Class == SourceRandom && (!disk.identity.KnownSSD || !disk.identity.SSD)
}

func (c *controller) hddRandomActive(disk *diskState) int {
	count := 0
	for _, lease := range disk.active {
		if c.isHDDRandom(disk, lease.req) {
			count++
		}
	}
	return count
}

func (c *controller) grantBytes(want int64) int64 {
	if want <= 0 {
		want = c.options.Policy.LeaseBytes
	}
	minimum := c.options.Policy.MinLeaseBytes
	if minimum < absoluteMinLeaseBytes {
		minimum = absoluteMinLeaseBytes
	}
	maximum := c.options.Policy.MaxLeaseBytes
	if maximum > absoluteMaxLeaseBytes {
		maximum = absoluteMaxLeaseBytes
	}
	if maximum < minimum {
		maximum = minimum
	}
	if want < minimum {
		return minimum
	}
	if want > maximum {
		return maximum
	}
	return want
}

func identityOf(req Request) TaskIdentity {
	return TaskIdentity{TaskID: req.TaskID, InstanceID: req.InstanceID}
}

func chooseTask(now time.Time, queues map[TaskIdentity]*taskQueue) TaskIdentity {
	identities := make([]TaskIdentity, 0, len(queues))
	for identity, queue := range queues {
		if queue != nil && len(queue.items) != 0 {
			identities = append(identities, identity)
		}
	}
	sort.Slice(identities, func(i, j int) bool {
		left, right := queues[identities[i]], queues[identities[j]]
		leftAge := now.Sub(left.items[0].enqueued)
		rightAge := now.Sub(right.items[0].enqueued)
		leftStarved := leftAge >= starvationAge
		rightStarved := rightAge >= starvationAge
		if leftStarved != rightStarved {
			return leftStarved
		}
		if leftStarved && left.items[0].enqueued != right.items[0].enqueued {
			return left.items[0].enqueued.Before(right.items[0].enqueued)
		}
		if left.grants != right.grants {
			return left.grants < right.grants
		}
		if !left.lastGranted.Equal(right.lastGranted) {
			return left.lastGranted.Before(right.lastGranted)
		}
		if !left.items[0].enqueued.Equal(right.items[0].enqueued) {
			return left.items[0].enqueued.Before(right.items[0].enqueued)
		}
		if identities[i].TaskID != identities[j].TaskID {
			return identities[i].TaskID < identities[j].TaskID
		}
		return identities[i].InstanceID < identities[j].InstanceID
	})
	if len(identities) == 0 {
		return TaskIdentity{}
	}
	return identities[0]
}

func (c *controller) handleCancelAcquire(state *ownerState, token uint64) {
	for _, disk := range state.disks {
		for _, queue := range disk.queues {
			for i, pending := range queue.items {
				if pending.token != token {
					continue
				}
				queue.items = append(queue.items[:i], queue.items[i+1:]...)
				return
			}
		}
	}
	for _, lease := range state.leases {
		if lease.token == token {
			disk := state.disks[lease.req.Disk]
			c.releaseLease(state, disk, lease)
			c.dispatch(state, disk)
			return
		}
	}
}

func (c *controller) handleCancelTask(state *ownerState, identity TaskIdentity) {
	generation := state.generations[identity]
	if generation == 0 {
		generation = 1
	}
	state.generations[identity] = generation + 1
	for _, disk := range state.disks {
		if queue := disk.queues[identity]; queue != nil {
			delete(disk.queues, identity)
			for _, pending := range queue.items {
				pending.reply <- acquireReply{err: ErrTaskCancelled}
			}
		}
		c.dispatch(state, disk)
	}
}

func (c *controller) handleReclaimWorker(state *ownerState, workerID int) {
	affected := make(map[*diskState]struct{})
	for _, disk := range state.disks {
		for _, queue := range disk.queues {
			kept := queue.items[:0]
			for _, pending := range queue.items {
				if pending.req.WorkerID == workerID {
					pending.reply <- acquireReply{err: ErrWorkerReclaimed}
					affected[disk] = struct{}{}
					continue
				}
				kept = append(kept, pending)
			}
			queue.items = kept
		}
	}
	if lease := state.workers[workerID]; lease != nil {
		disk := state.disks[lease.req.Disk]
		c.releaseLease(state, disk, lease)
		affected[disk] = struct{}{}
	}
	for disk := range affected {
		c.dispatch(state, disk)
	}
}

func (c *controller) handleReport(state *ownerState, report Report) {
	lease := state.leases[report.LeaseID]
	if lease == nil {
		return
	}
	disk := state.disks[lease.req.Disk]
	identity := identityOf(lease.req)
	valid := report.Generation == lease.grant.Generation &&
		report.TaskID == lease.req.TaskID && report.InstanceID == lease.req.InstanceID &&
		report.WorkerID == lease.req.WorkerID && report.Disk == lease.req.Disk &&
		state.generations[identity] == report.Generation
	c.releaseLease(state, disk, lease)
	if valid && report.Completed && !report.Cancelled {
		c.recordReport(state, disk, identity, lease, report)
	}
	c.dispatch(state, disk)
}

func (c *controller) releaseLease(state *ownerState, disk *diskState, lease *activeLease) {
	delete(disk.active, lease.grant.LeaseID)
	delete(state.leases, lease.grant.LeaseID)
	if state.workers[lease.req.WorkerID] == lease {
		delete(state.workers, lease.req.WorkerID)
	}
}

func (c *controller) recordReport(state *ownerState, disk *diskState, identity TaskIdentity, lease *activeLease, report Report) {
	stats := state.stats[identity]
	if stats == nil {
		stats = &taskStats{}
		state.stats[identity] = stats
	}
	if lease.req.Class == SourceSequential {
		stats.sequentialBytes += report.Bytes
	}
	stats.seekCount += int64(report.Seeks)
	stats.readTime += report.ReadTime
	stats.waitTime += c.clock.Now().Sub(lease.granted) + report.WaitTime
	stats.reports++

	disk.window.bytes += report.Bytes
	disk.window.seeks += int64(report.Seeks)
	disk.window.waits = append(disk.window.waits, report.WaitTime)
	disk.window.hddRandom = disk.window.hddRandom || c.isHDDRandom(disk, lease.req)
	duration := c.clock.Now().Sub(disk.window.started)
	if duration < c.options.Policy.Window {
		return
	}
	p95 := percentile95(disk.window.waits)
	sample := WindowSample{
		Duration:               duration,
		Bytes:                  disk.window.bytes,
		PreviousBytesPerSecond: disk.window.previousBPS,
		P95Wait:                p95,
		PreviousP95Wait:        disk.window.previousP95,
		SeekCongested:          disk.window.seeks > int64(maxInt(4, disk.limit*2)),
		Queued:                 queuedCount(disk),
		BusyWorkers:            len(state.workers),
		WorkerCount:            c.options.WorkerCount,
		HDDRandom:              disk.window.hddRandom,
	}
	disk.limit = nextLimit(disk.limit, sample, c.options.Policy)
	bps := float64(disk.window.bytes) / duration.Seconds()
	disk.window = observationWindow{started: c.clock.Now(), previousBPS: bps, previousP95: p95}
}

func percentile95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	copyOfValues := append([]time.Duration(nil), values...)
	sort.Slice(copyOfValues, func(i, j int) bool { return copyOfValues[i] < copyOfValues[j] })
	index := (95*len(copyOfValues) + 99) / 100
	return copyOfValues[index-1]
}

func queuedCount(disk *diskState) int {
	count := 0
	for _, queue := range disk.queues {
		count += len(queue.items)
	}
	return count
}

func nextLimit(current int, sample WindowSample, cfg PolicyConfig) int {
	hard := absoluteMaxPerDisk
	if cfg.MaxPerDisk > 0 && cfg.MaxPerDisk < hard {
		hard = cfg.MaxPerDisk
	}
	if sample.WorkerCount > 0 && sample.WorkerCount < hard {
		hard = sample.WorkerCount
	}
	if sample.HDDRandom {
		randomMax := cfg.HDDRandomMax
		if randomMax < 1 {
			randomMax = defaultHDDRandomMax
		}
		if randomMax < hard {
			hard = randomMax
		}
	}
	if hard < 1 {
		hard = 1
	}
	if current < 1 {
		current = 1
	}
	if current > hard {
		current = hard
	}
	if sample.Duration < 2*time.Second || sample.Bytes < absoluteMinLeaseBytes {
		return current
	}

	increaseThreshold := cfg.IncreaseThreshold
	if increaseThreshold <= 0 {
		increaseThreshold = 0.05
	}
	decreaseThreshold := cfg.DecreaseThreshold
	if decreaseThreshold <= 0 {
		decreaseThreshold = 0.08
	}
	bps := float64(sample.Bytes) / sample.Duration.Seconds()
	throughputDrop := sample.PreviousBytesPerSecond > 0 &&
		(sample.PreviousBytesPerSecond-bps)/sample.PreviousBytesPerSecond > decreaseThreshold
	p95Spike := sample.PreviousP95Wait > 0 && sample.P95Wait > 2*sample.PreviousP95Wait
	if throughputDrop || p95Spike || sample.SeekCongested {
		decrease := maxInt(1, current/4)
		if current-decrease < 1 {
			return 1
		}
		return current - decrease
	}

	allWorkersBusy := sample.WorkerCount > 0 && sample.BusyWorkers >= sample.WorkerCount
	if allWorkersBusy || current >= hard {
		return current
	}
	improved := sample.PreviousBytesPerSecond > 0 &&
		(bps-sample.PreviousBytesPerSecond)/sample.PreviousBytesPerSecond >= increaseThreshold
	queueProbe := sample.Queued > 0 && sample.BusyWorkers < sample.WorkerCount
	if improved || queueProbe {
		return current + 1
	}
	return current
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func (c *controller) makeSnapshot(state *ownerState, identity TaskIdentity) Snapshot {
	var snapshot Snapshot
	for _, disk := range state.disks {
		for _, lease := range disk.active {
			if identityOf(lease.req) == identity {
				snapshot.Concurrency++
				snapshot.BusyWorkers++
			}
		}
		if queue := disk.queues[identity]; queue != nil {
			snapshot.IOWaitWorkers += len(queue.items)
			for _, pending := range queue.items {
				wait := c.clock.Now().Sub(pending.enqueued)
				if wait > snapshot.LeaseWait {
					snapshot.LeaseWait = wait
				}
			}
		}
	}
	if stats := state.stats[identity]; stats != nil {
		snapshot.SequentialBytes = stats.sequentialBytes
		snapshot.SeekCount = stats.seekCount
		if stats.readTime > 0 {
			snapshot.EffectiveBytesPerSecond = float64(stats.sequentialBytes) / stats.readTime.Seconds()
		}
		if stats.reports > 0 && snapshot.LeaseWait == 0 {
			snapshot.LeaseWait = stats.waitTime / time.Duration(stats.reports)
		}
	}
	return snapshot
}

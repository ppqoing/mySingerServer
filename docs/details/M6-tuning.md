# M6 调优与压测 — 详细实施文档

> 依据：`docs/architecture-plan.md` v1.1。本文档只做调优、压测与验收，不改变任何已定架构、选型、协议语义与数据模型。
> 前置里程碑：M2~M5 全部完成（系统功能已端到端可用）。

---

## 1. 目标与范围

### 1.1 目标

在真实硬件（≥1 台调优机，HDD + SSD 混合，单机 ≥3 块盘）与真实语料（百万级文件）上完成四件事：

1. **盘级 IO 调度调优**：HDD 每盘 1~2 条 4MB 顺序读流、SSD 每盘 4~8 条 1MB 读流、调度器按目录序取任务、背压阈值调参方法落地，实测 HDD 顺序带宽占用 ≥80%。
2. **CPU 吃满调优**：Worker 数 = CPU 核数基线，pprof 定位瓶颈，验证读算重叠（IO 等待时 CPU 不空转），实测扫描期 CPU 占用 ≥85%。
3. **压测**：同步链路（5min/5万行策略在百万级积压下的 PostgreSQL 写入压测与批量大小调优）；一筛（百万级 PDQ band 倒排耗时测量）。
4. **百万文件浸泡测试**：构建含正常/损坏/超大/重复的图片与视频语料，连续全量 + 增量扫描，主进程零崩溃，`crash.log`/`errors.log` 完备性检查。
5. **指标采集与报告**：每盘吞吐、CPU 占用、Worker 存活率、单文件耗时分布，输出结构化压测报告。

### 1.2 验收标准（与架构计划 §10 M6 一致，可测量化）

| # | 指标 | 通过标准 | 测量方法 |
|---|---|---|---|
| A1 | HDD 顺序带宽占用 | 扫描期每块 HDD 实测读带宽 ≥ 该盘空载顺序读基准（fio 测得）的 80% | Windows Performance Monitor `\PhysicalDisk(N)\Disk Read Bytes/sec` ÷ fio 基准 |
| A2 | CPU 占用 | 全量扫描稳态阶段，Agent 主进程 + 全部 Worker 合计 CPU ≥ 机器总 CPU 的 85% | stats 自采 + `\Process(*)\% Processor Time` 交叉验证 |
| A3 | 主进程零崩溃 | 浸泡测试全程（≥ 3 轮全量 + ≥ 10 轮增量，跨 ≥ 24h）Agent 主进程退出码非 0 次数 = 0，无需人工干预完成 | 浸泡编排器记录主进程 PID 存活与退出码 |
| A4 | 崩溃可恢复 | Worker/ffmpeg 崩溃全部写入 `crash.log`（一行一次，字段齐），崩溃后 ≤5s 内池内补位，整轮扫描不中断 | 注入崩溃用例 + 浸泡日志审计 |
| A5 | 错误日志完备 | 每个计算失败字段在 `errors.log` 恰有一行 `{ts, path, stage, err}`，与 SQLite `files.status/error` 对账一致 | 对账脚本 |
| A6 | 同步吞吐 | 百万级积压上行不丢行、不重行（`ON CONFLICT` 幂等生效），5万行批量在选定的批量大小下稳定完成，中心库行数与本地库对账一致 | 同步压测工具 |
| A7 | 一筛性能 | 百万级 PDQ-256 band 倒排出候选对端到端 < 10s（GUI 所在机内存 ≥16GB） | 一筛压测工具 |
| A8 | 报告 | 输出 `perf-report.md`，含全部测试矩阵结果与结论 | 报告生成器 |

### 1.3 不做什么

- **不改架构与选型**：不引入新语言/组件；不改 TCP 帧格式与消息语义（仅在协议尾部追加 `StatsQuery`/`StatsReport` 两类消息，见 §4.4）；不改 SQLite/PostgreSQL 表结构语义（仅新增压测专用独立表 `sync_bench`，业务表零变更）。
- **不改判定阈值**：T1/T2/T3/T4、长宽比宽容度、时长差剪枝等业务阈值不在本里程碑调整。
- **不做 GUI 界面性能优化**：一筛压测用命令行工具直接驱动分析代码，不经过 Web 页面。
- **不做跨公网压测**：同步压测目标为局域网自建 PostgreSQL 16（架构计划 §1 默认决策）。
- **不做删除链路压测**：M5 已验收，本里程碑仅在浸泡测试中保留删除冒烟用例。
- **不优化 mediacore.dll 算法实现**：PDQ/pHash/Sobel 内核只做耗时测量，若成为瓶颈，记录结论但不重写算法（超范围时提交架构层面评审）。
- **不测 M1 之前的回退路径**：Everything 未运行的常规遍历回退只做一次冒烟，不纳入性能矩阵。

---

## 2. 任务分解（Checklist）

### 2.1 基础设施：指标采集

- [ ] 实现 `internal/stats` 采集器：每盘读字节计数、每文件耗时直方图、Worker 心跳/崩溃计数、主进程 RSS/句柄数（见 §4.1）
- [ ] 实现 CPU 自采样（主进程 + Worker 进程组，`GetProcessTimes` 族，1s 采样间隔）
- [ ] 实现 `stats.log` JSON 行落盘（lumberjack 滚动，与 `agent.log` 同规范）+ `StatsQuery`/`StatsReport` TCP 消息（见 §4.4）
- [ ] 主进程接入 `net/http/pprof`（默认关，`--pprof=:16060` 开启，仅监听 loopback）
- [ ] 验收：采集开销 < 1% CPU；stats.log 字段与 §4.1 结构体一致

### 2.2 盘级 IO 调度调优

- [ ] fio 基准：每块盘空载顺序读带宽（4MB 块）与随机读 IOPS（1MB 块）建档（§6.1 前置步骤）
- [ ] 调度器参数外置：每盘 `streams`、`chunk_size` 可由 config 覆盖（默认值保持架构计划 §9）
- [ ] 按目录序取任务验证：从 `files` 表取任务时按 `path` 字典序（同目录物理邻近），用 10 万文件语料对比"目录序 vs 随机序"的 HDD 吞吐
- [ ] 背压扫描：待算字节阈值 `{512MB, 1GB, 2GB, 4GB}` × HDD 流 `{1,2}` 矩阵，记录吞吐与内存曲线，选定默认值
- [ ] SSD 流数扫描：`{4, 6, 8}` 流 × 1MB 块，选定默认值
- [ ] 验收：A1 达成；背压触发时 RSS 不超限（≤ 阈值 × 1.2）

### 2.3 CPU 吃满调优

- [ ] Worker 数 = 核数基线跑全量扫描，stats 记录 CPU 曲线（区分 IO-bound / CPU-bound 时段）
- [ ] pprof CPU profile 采集（扫描稳态 60s 窗口）：定位热点（cgo 边界、SQLite、msgpack 编解码、GC）
- [ ] 读算重叠验证：调度器保证"读盘 goroutine 与 Worker 计算并行"，用 stats 的 `disk_busy` 与 `cpu_busy` 时间线叠加验证，IO 忙时 CPU 忙占比 ≥ 90%
- [ ] 若 CPU < 85%：按 §6.3 决策树处理（调流数/块大小、查锁竞争、查 GC 压力）
- [ ] 验收：A2 达成

### 2.4 同步压测

- [ ] 实现 `cmd/benchsync`：造数 → 多并发上行 → 对账（见 §4.3）
- [ ] 批量大小扫描：`{1000, 5000, 10000, 50000}` 行/批 × 并发 Agent `{1, 2, 4}` 矩阵
- [ ] 5min/5万行策略回归：按架构计划 §6.3 默认策略在百万级积压下连跑 3 个周期
- [ ] 断点重发：压测中 kill 一次 PostgreSQL 连接，验证 `sync_queue` 重发不丢不重
- [ ] 验收：A6 达成；中心库与本地库行数/主键对账 100% 一致

### 2.5 一筛性能压测

- [ ] 实现 `cmd/benchscreen`：生成百万级合成 PDQ-256（含可控重复率），直接调用 GUI 侧 band 倒排代码
- [ ] 规模扫描：`{10万, 50万, 100万}` 条 × band 配置（保持 M3 既定分段），记录耗时/内存/候选对数
- [ ] 验收：A7 达成

### 2.6 百万文件浸泡测试

- [ ] 实现 `cmd/corpusgen` 语料构建（§4.2）：正常/损坏/超大/精确重复/近似重复 图片与视频
- [ ] 实现 `cmd/soakrun` 浸泡编排器：全量×3 + 增量×10 + 中途注入崩溃/损坏文件，监控主进程存活与日志完备性（§4.5）
- [ ] 24h 浸泡执行（≥1 台调优机，语料 ≥100 万文件分布在 HDD×2 + SSD×1）
- [ ] `crash.log`/`errors.log` 与 SQLite 对账审计
- [ ] 验收：A3/A4/A5 达成

### 2.7 报告

- [ ] 实现 `cmd/perfreport`：聚合 stats.log + 各压测 JSON 结果 → `perf-report.md`（模板见 §7）
- [ ] 填写测试矩阵全部结果，给出默认值最终结论（维持或调整架构计划 §9 中"全部可配"参数的建议值）

---

## 3. 目录与文件结构

仅列出 M6 新增/触动的文件；业务代码结构以 M1~M5 为准，不在此重复。

```
agent/
├── cmd/
│   ├── agent/main.go                # （改）挂 stats、pprof、config 新键
│   ├── benchsync/main.go            # 新增：同步压测工具
│   ├── benchscreen/main.go          # 新增：一筛性能压测工具
│   ├── corpusgen/main.go            # 新增：浸泡语料构建
│   ├── soakrun/main.go              # 新增：浸泡编排器
│   └── perfreport/main.go           # 新增：报告生成器
├── internal/
│   ├── stats/
│   │   ├── stats.go                 # 新增：采集器核心
│   │   ├── cpu_windows.go           # 新增：进程组 CPU 采样
│   │   └── sink.go                  # 新增：stats.log 落盘 + 内存环形快照
│   ├── sched/                       # （改）背压/流数/块大小 config 可覆盖，目录序取任务
│   └── syncer/                      # （改）暴露批量大小/并发参数供压测注入
├── config.agent.yaml                # （改）新增 tuning 节（§5.2）
└── tools/
    ├── fio-baseline.ps1             # 新增：盘基准脚本
    ├── perfmon-collect.ps1          # 新增：Windows Performance Monitor 计数器采集
    ├── audit_logs.go                # 新增：crash/errors 日志与 SQLite 对账
    └── check_corpus.ps1             # 新增：语料清单校验
tests/
└── m6/
    ├── corpus-manifest.json         # 语料清单（corpusgen 生成）
    ├── matrix.yaml                  # 测试矩阵定义
    └── expected/                    # 对账基准（行数、哈希计数）
reports/                             # 输出：perf-report.md 与原始 JSON
```

---

## 4. 关键接口与结构体定义

### 4.1 指标采集（Go）

```go
// agent/internal/stats/stats.go
package stats

import (
	"sync"
	"sync/atomic"
	"time"
)

// FileSample 单文件耗时样本（Worker 回报一条，主进程记录）
type FileSample struct {
	Path      string  `msgpack:"path"`
	DiskNo    int     `msgpack:"disk_no"`
	Kind      string  `msgpack:"kind"` // image / video
	SizeBytes int64   `msgpack:"size_bytes"`
	ReadMs    float64 `msgpack:"read_ms"`  // 读盘+SHA-512 阶段
	DecodeMs  float64 `msgpack:"decode_ms"` // DLL 解码 + 感知哈希阶段（视频含 ffmpeg 等待）
	TotalMs   float64 `msgpack:"total_ms"`
	Stage     string  `msgpack:"stage"` // phase1 / phase2
	OK        bool    `msgpack:"ok"`
}

// DiskCounter 每盘原子计数（IO 调度器内嵌，零锁）
type DiskCounter struct {
	DiskNo     int
	BytesRead  atomic.Int64
	FilesDone  atomic.Int64
	BusyNanos  atomic.Int64 // 有至少一条流在读盘的时间
	StreamHigh atomic.Int32 // 并发流水位峰值
}

// WorkerCounter Worker 池监督计数
type WorkerCounter struct {
	Spawned     atomic.Int64
	Crashed     atomic.Int64
	Active      atomic.Int32
	RestartMaxMs atomic.Int64 // 崩溃→补位最大耗时
}

// Snapshot 每秒生成一份，写 stats.log 并经 StatsReport 上报
type Snapshot struct {
	Ts          time.Time        `json:"ts" msgpack:"ts"`
	Disks       []DiskSnap       `json:"disks" msgpack:"disks"`
	CPUFrac     float64          `json:"cpu_frac" msgpack:"cpu_frac"`     // 进程组 CPU / 机器总 CPU
	RSSBytes    uint64           `json:"rss_bytes" msgpack:"rss_bytes"`
	Handles     uint32           `json:"handles" msgpack:"handles"`
	Workers     WorkerSnap       `json:"workers" msgpack:"workers"`
	BacklogBts  int64            `json:"backlog_bytes" msgpack:"backlog_bytes"` // 背压待算字节
	Latency     LatencySnap      `json:"latency" msgpack:"latency"`
}

type DiskSnap struct {
	DiskNo      int     `json:"disk_no" msgpack:"disk_no"`
	ReadBps     float64 `json:"read_bps" msgpack:"read_bps"` // 本秒读带宽
	BusyFrac    float64 `json:"busy_frac" msgpack:"busy_frac"`
	StreamHigh  int32   `json:"stream_high" msgpack:"stream_high"`
}

type WorkerSnap struct {
	Active  int32 `json:"active" msgpack:"active"`
	Crashed int64 `json:"crashed" msgpack:"crashed"`
	Alive   int64 `json:"alive" msgpack:"alive"` // Spawned - Crashed(净存活次数比)
}

// LatencySnap 单文件耗时分布（HDR 式固定桶，避免引入第三方库）
type LatencySnap struct {
	Count  int64     `json:"count" msgpack:"count"`
	P50Ms  float64   `json:"p50_ms" msgpack:"p50_ms"`
	P90Ms  float64   `json:"p90_ms" msgpack:"p90_ms"`
	P99Ms  float64   `json:"p99_ms" msgpack:"p99_ms"`
	MaxMs  float64   `json:"max_ms" msgpack:"max_ms"`
}

// Collector 主进程单例
type Collector struct {
	mu      sync.Mutex
	disks   map[int]*DiskCounter
	workers WorkerCounter
	// latency 环形缓冲：保留最近 1<<20 个样本的耗时(ms, 0.1ms 精度)
	latBuf  []uint32
	latHead int
	latN    int
	cpu     *CPUSampler // cpu_windows.go
}

func New(disks []int, cpu *CPUSampler) *Collector {
	c := &Collector{
		disks:  make(map[int]*DiskCounter, len(disks)),
		latBuf: make([]uint32, 1<<20),
		cpu:    cpu,
	}
	for _, d := range disks {
		c.disks[d] = &DiskCounter{DiskNo: d}
	}
	return c
}

func (c *Collector) Disk(diskNo int) *DiskCounter { return c.disks[diskNo] }
func (c *Collector) Workers() *WorkerCounter      { return &c.workers }

// RecordFile 每完成一个文件调用一次；O(1)，采集开销可忽略
func (c *Collector) RecordFile(s FileSample) {
	c.mu.Lock()
	c.latBuf[c.latHead] = uint32(s.TotalMs * 10)
	c.latHead = (c.latHead + 1) & (len(c.latBuf) - 1)
	if c.latN < len(c.latBuf) {
		c.latN++
	}
	c.mu.Unlock()
	if dc, ok := c.disks[s.DiskNo]; ok {
		dc.FilesDone.Add(1)
	}
}

// quantile 从环形缓冲计算分位数（复制+部分排序，每秒最多一次）
func (c *Collector) quantile(q float64) float64 {
	c.mu.Lock()
	n := c.latN
	tmp := make([]uint32, n)
	copy(tmp, c.latBuf[:n])
	c.mu.Unlock()
	if n == 0 {
		return 0
	}
	// 简单插入排序分桶：样本量大时用 quickselect；此处用 sort.Slice 足够（1s 一次）
	sortUint32(tmp)
	idx := int(q * float64(n-1))
	return float64(tmp[idx]) / 10
}

func sortUint32(a []uint32) {
	// 标准库 sort，避免依赖
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
```

```go
// agent/internal/stats/cpu_windows.go
package stats

import (
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

// CPUSampler 采样"主进程 + 全部 Worker"合计 CPU 占比
type CPUSampler struct {
	mu        sync.Mutex
	pids      map[uint32]struct{}
	lastTimes map[uint32]int64 // 100ns 单位的 kernel+user 时间
	lastWall  time.Time
	cores     int
	frac      float64
}

func NewCPUSampler(cores int) *CPUSampler {
	return &CPUSampler{
		pids:      map[uint32]struct{}{uint32(windows.GetCurrentProcessId()): {}},
		lastTimes: make(map[uint32]int64),
		cores:     cores,
	}
}

// TrackPID Worker 诞生/重生时登记
func (s *CPUSampler) TrackPID(pid uint32) {
	s.mu.Lock()
	s.pids[pid] = struct{}{}
	s.mu.Unlock()
}

// UntrackPID Worker 死亡时移除
func (s *CPUSampler) UntrackPID(pid uint32) {
	s.mu.Lock()
	delete(s.pids, pid)
	delete(s.lastTimes, pid)
	s.mu.Unlock()
}

// Sample 每秒调用一次，返回进程组 CPU / 机器总 CPU
func (s *CPUSampler) Sample() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.lastWall.IsZero() {
		s.lastWall = now
		for pid := range s.pids {
			s.lastTimes[pid] = procTime100ns(pid)
		}
		return 0
	}
	wallDelta := float64(now.Sub(s.lastWall).Nanoseconds())
	var used int64
	for pid := range s.pids {
		t := procTime100ns(pid)
		if prev, ok := s.lastTimes[pid]; ok && t >= prev {
			used += (t - prev) * 100 // 100ns → ns
		}
		s.lastTimes[pid] = t
	}
	s.lastWall = now
	if wallDelta <= 0 {
		return s.frac
	}
	s.frac = float64(used) / wallDelta / float64(s.cores)
	return s.frac
}

func procTime100ns(pid uint32) int64 {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return -1
	}
	defer windows.CloseHandle(h)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return -1
	}
	return int64(kernel.HighDateTime)<<32 | int64(kernel.LowDateTime) +
		int64(user.HighDateTime)<<32 | int64(user.LowDateTime)
}
```

```go
// agent/internal/stats/sink.go
package stats

import (
	"encoding/json"
	"log/slog"
	"time"
)

// Run 每秒产出 Snapshot：写 stats.log，并保留最近 300 份供 StatsReport 拉取
func (c *Collector) Run(log *slog.Logger, stop <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	prevBytes := map[int]int64{}
	prevBusy := map[int]int64{}
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			snap := Snapshot{Ts: time.Now(), CPUFrac: c.cpu.Sample()}
			for no, dc := range c.disks {
				b := dc.BytesRead.Load()
				busy := dc.BusyNanos.Load()
				snap.Disks = append(snap.Disks, DiskSnap{
					DiskNo:     no,
					ReadBps:    float64(b-prevBytes[no]),
					BusyFrac:   float64(busy-prevBusy[no]) / 1e9,
					StreamHigh: dc.StreamHigh.Load(),
				})
				prevBytes[no] = b
				prevBusy[no] = busy
			}
			snap.Workers = WorkerSnap{
				Active:  c.workers.Active.Load(),
				Crashed: c.workers.Crashed.Load(),
			}
			snap.Workers.Alive = c.workers.Spawned.Load() - c.workers.Crashed.Load()
			snap.Latency = LatencySnap{
				Count: int64(c.latN),
				P50Ms: c.quantile(0.50),
				P90Ms: c.quantile(0.90),
				P99Ms: c.quantile(0.99),
				MaxMs: c.quantile(1.0),
			}
			bs, err := json.Marshal(snap)
			if err == nil {
				log.Info("stats", "line", string(bs))
			}
		}
	}
}
```

pprof 挂载（`agent/cmd/agent/main.go` 改动示意，完整可编译）：

```go
// pprof：默认关闭；--pprof=:16060 开启，仅 loopback，扫描稳态窗口采 60s CPU profile
package main

import (
	"net"
	"net/http"
	_ "net/http/pprof"
)

// startPprof 在独立 goroutine 启动；bind 必须带 loopback，禁止暴露到局域网
func startPprof(addr string) {
	if addr == "" {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1"+addr)
	if err != nil {
		return
	}
	go func() { _ = http.Serve(ln, http.DefaultServeMux) }()
}
// 采集命令（调优机本地执行）：
//   go tool pprof -seconds 60 http://127.0.0.1:16060/debug/pprof/profile
//   go tool pprof -http=:18080 http://127.0.0.1:16080/debug/pprof/heap
```

### 4.2 浸泡语料构建（Go）

```go
// agent/cmd/corpusgen/main.go
// 语料构成（总数 N 默认 1,000,000，可用 -total 调整；分布见下表）
//   正常图片 55% | 正常视频 5% | 精确重复图片 10% | 精确重复视频 2%
//   近似重复图片 8%（重编码/缩放） | 损坏文件 12% | 超大文件 3% | 非媒体干扰 5%
package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Spec struct {
	Kind    string // normal_img / normal_vid / dup_img / dup_vid / near_img / corrupt / oversize / noise
	Count   int
	Ext     string
	Bytes   int64 // corrupt/oversize/noise 用
	SrcSeed int   // dup/near 引用的种子文件序号
}

func main() {
	root := flag.String("root", `D:\soak-corpus`, "语料根目录")
	total := flag.Int("total", 1000000, "文件总数")
	ffmpeg := flag.String("ffmpeg", `ffmpeg`, "ffmpeg 路径")
	flag.Parse()

	specs := plan(*total)
	if err := os.MkdirAll(*root, 0o755); err != nil {
		fatal(err)
	}
	manifest := filepath.Join(*root, "corpus-manifest.json")
	mf, err := os.Create(manifest)
	if err != nil {
		fatal(err)
	}
	defer mf.Close()
	fmt.Fprintln(mf, "[")

	seedIdx := 0
	first := true
	for i, sp := range specs {
		dir := filepath.Join(*root, fmt.Sprintf("dir-%04d", i/2000)) // 2000 文件/目录，保证目录序局部性
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatal(err)
		}
		name := filepath.Join(dir, fmt.Sprintf("f-%07d%s", i, sp.Ext))
		var shaNote string
		switch sp.Kind {
		case "normal_img":
			genImage(name, 256+randN(1800), 256+randN(1800), false)
		case "normal_vid":
			genVideo(*ffmpeg, name, 2+randN(28), false)
		case "dup_img", "dup_vid", "near_img":
			// 由种子复制/重编码；种子即 specs 中 SrcSeed 指向的 normal 文件
			seed := filepath.Join(*root, fmt.Sprintf("dir-%04d", sp.SrcSeed/2000),
				fmt.Sprintf("f-%07d%s", sp.SrcSeed, extOf(specs, sp.SrcSeed)))
			if sp.Kind == "near_img" {
				reencode(seed, name) // 质量 85 重编码 → 内容近似、字节不同
			} else {
				copyFile(seed, name) // 字节级复制 → SHA-512 相同
				shaNote = "same-as:" + seed
			}
		case "corrupt":
			genCorrupt(name, sp.Bytes, sp.Ext)
		case "oversize":
			genCorrupt(name, sp.Bytes, sp.Ext) // 合法头 + 填充，尺寸超限（图片>256MB 阈值或视频>2GB）
		case "noise":
			genCorrupt(name, sp.Bytes, sp.Ext) // 随机字节，扩展名为 .txt/.log 等
		}
		if !first {
			fmt.Fprintln(mf, ",")
		}
		first = false
		fmt.Fprintf(mf, `{"idx":%d,"path":%q,"kind":%q,"note":%q}`, i, name, sp.Kind, shaNote)
		seedIdx++
	}
	fmt.Fprintln(mf, "\n]")
	fmt.Printf("corpus done: %d files at %s (seeds=%d)\n", *total, *root, seedIdx)
}

func plan(total int) []Spec {
	pct := []struct {
		kind string
		p    int
		ext  string
	}{
		{"normal_img", 55, ".jpg"}, {"normal_vid", 5, ".mp4"},
		{"dup_img", 10, ".jpg"}, {"dup_vid", 2, ".mp4"},
		{"near_img", 8, ".jpg"}, {"corrupt", 12, ".jpg"},
		{"oversize", 3, ".jpg"}, {"noise", 5, ".txt"},
	}
	out := make([]Spec, 0, total)
	for _, p := range pct {
		n := total * p.p / 100
		for i := 0; i < n; i++ {
			sp := Spec{Kind: p.kind, Ext: p.ext, Bytes: 4096 + int64(randN(1<<20))}
			if p.kind == "dup_img" || p.kind == "dup_vid" || p.kind == "near_img" {
				sp.SrcSeed = randN(total * 55 / 100) // 只引用正常图片/视频种子区
			}
			if p.kind == "oversize" {
				sp.Bytes = 300 << 20 // 300MB，超过图片内存驻留阈值 256MB
			}
			if p.kind == "corrupt" && i%3 == 0 {
				sp.Ext = ".mp4"
			}
			out = append(out, sp)
		}
	}
	// 补齐到 total
	for len(out) < total {
		out = append(out, Spec{Kind: "normal_img", Ext: ".jpg"})
	}
	return out
}

func genImage(path string, w, h int, _ bool) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8((x * 7) ^ y), uint8((y * 13) ^ x), uint8(x + y), 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	if filepath.Ext(path) == ".png" {
		fatal(png.Encode(f, img))
	} else {
		fatal(jpeg.Encode(f, img, &jpeg.Options{Quality: 92}))
	}
}

func genVideo(ffmpeg, path string, sec int, _ bool) {
	// 2s~30s 的 testsrc2 视频，码率低以保证语料构建速度
	cmd := exec.Command(ffmpeg, "-y", "-f", "lavfi",
		"-i", fmt.Sprintf("testsrc2=size=640x360:rate=24:duration=%d", sec),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "30", "-pix_fmt", "yuv420p", path)
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Run(); err != nil {
		fatal(err)
	}
}

func reencode(seed, dst string) {
	f, err := os.Open(seed)
	if err != nil {
		fatal(err)
	}
	img, err := jpeg.Decode(f)
	f.Close()
	if err != nil {
		fatal(err)
	}
	out, err := os.Create(dst)
	if err != nil {
		fatal(err)
	}
	defer out.Close()
	fatal(jpeg.Encode(out, img, &jpeg.Options{Quality: 85}))
}

func copyFile(src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		fatal(err)
	}
	fatal(os.WriteFile(dst, b, 0o644))
}

// genCorrupt：先写合法文件头再截断/填充随机字节，覆盖"能解析头但体损坏"与"完全随机"两类
func genCorrupt(path string, size int64, ext string) {
	f, err := os.Create(path)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	switch ext {
	case ".jpg", ".jpeg":
		_, _ = f.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00})
	case ".png":
		_, _ = f.Write([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A})
	case ".mp4":
		_, _ = f.Write([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
	}
	buf := make([]byte, 1<<20)
	_, _ = rand.Read(buf)
	remain := size
	for remain > 0 {
		n := int64(len(buf))
		if remain < n {
			n = remain
		}
		if _, err := f.Write(buf[:n]); err != nil {
			fatal(err)
		}
		remain -= n
	}
}

func randN(max int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return int(time.Now().UnixNano() % int64(max))
	}
	return int(n.Int64())
}

func extOf(specs []Spec, i int) string {
	if i < len(specs) {
		return specs[i].Ext
	}
	return ".jpg"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "corpusgen:", err)
	os.Exit(1)
}
```

### 4.3 同步压测工具（Go）

```go
// agent/cmd/benchsync/main.go
// 目标：在百万级积压下压测 PostgreSQL 写入，扫描批量大小与并发 Agent 数，
// 验证 5min/5万行 默认策略的稳定性与幂等性。不改业务表：写入独立表 sync_bench。
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Result struct {
	Batch    int     `json:"batch"`
	Agents   int     `json:"agents"`
	Rows     int     `json:"rows"`
	Elapsed  float64 `json:"elapsed_s"`
	RowsPerS float64 `json:"rows_per_s"`
	Retries  int64   `json:"retries"`
	OK       bool    `json:"ok"`
}

func main() {
	dsn := flag.String("dsn", os.Getenv("PG_DSN"), "PostgreSQL DSN")
	total := flag.Int("rows", 1000000, "总上行行数")
	batch := flag.Int("batch", 50000, "每批行数")
	agents := flag.Int("agents", 2, "并发模拟 Agent 数")
	out := flag.String("out", "benchsync-result.json", "结果输出")
	flag.Parse()

	db, err := sql.Open("pgx", *dsn)
	must(err)
	defer db.Close()
	must(db.Ping())
	ensureTable(db)

	start := time.Now()
	var retries int64
	ctx := context.Background()
	base := *total / *agents
	done := make(chan error, *agents)
	for a := 0; a < *agents; a++ {
		go func(agentID int) {
			rowsPerAgent := base
			if agentID == *agents-1 {
				rowsPerAgent += *total - base**agents // 末位 Agent 承担余数，保证总行数 == total
			}
			var r int64
			for sent := 0; sent < rowsPerAgent; sent += *batch {
				n := *batch
				if sent+n > rowsPerAgent {
					n = rowsPerAgent - sent
				}
				if err := upsertBatch(ctx, db, agentID, sent, n); err != nil {
					r++ // 模拟 sync_queue 重发：整批重试一次
					if err2 := upsertBatch(ctx, db, agentID, sent, n); err2 != nil {
						done <- err2
						return
					}
				}
			}
			retries += r
			done <- nil
		}(a)
	}
	var runErr error
	for a := 0; a < *agents; a++ {
		if e := <-done; e != nil {
			runErr = e
		}
	}
	elapsed := time.Since(start).Seconds()

	// 对账：行数必须等于 total（幂等：重复主键被 UPDATE 而非插入）
	var cnt int
	must(db.QueryRow(`SELECT count(*) FROM sync_bench`).Scan(&cnt))
	res := Result{
		Batch: *batch, Agents: *agents, Rows: *total,
		Elapsed: elapsed, RowsPerS: float64(*total) / elapsed,
		Retries: retries, OK: runErr == nil && cnt == *total,
	}
	bs, _ := jsonMarshalIndent(res)
	must(os.WriteFile(*out, bs, 0o644))
	fmt.Printf("batch=%d agents=%d -> %.0f rows/s, ok=%v (db rows=%d, expect=%d)\n",
		*batch, *agents, res.RowsPerS, res.OK, cnt, *total)
}

func ensureTable(db *sql.DB) {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS sync_bench (
  machine_id  INT          NOT NULL,
  row_seq     INT          NOT NULL,
  sha512      BYTEA        NOT NULL,
  payload     JSONB        NOT NULL,
  updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
  PRIMARY KEY (machine_id, row_seq)
)`)
	must(err)
	_, err = db.Exec(`TRUNCATE sync_bench`)
	must(err)
}

// upsertBatch 与业务同步同语义：自然键 ON CONFLICT UPDATE，单事务多行 INSERT
func upsertBatch(ctx context.Context, db *sql.DB, agentID, startSeq, n int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO sync_bench (machine_id, row_seq, sha512, payload)
VALUES ($1, $2, $3, $4)
ON CONFLICT (machine_id, row_seq)
DO UPDATE SET sha512 = EXCLUDED.sha512, payload = EXCLUDED.payload, updated_at = now()`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	sha := make([]byte, 64)
	for i := 0; i < n; i++ {
		if _, err := rand.Read(sha); err != nil {
			_ = tx.Rollback()
			return err
		}
		payload := fmt.Sprintf(`{"w":1920,"h":1080,"pdq_quality":%d,"agent":%d}`, i%100, agentID)
		if _, err := stmt.ExecContext(ctx, agentID, startSeq+i, sha, payload); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func jsonMarshalIndent(v Result) ([]byte, error) {
	return []byte(fmt.Sprintf(
		"{\n  \"batch\": %d,\n  \"agents\": %d,\n  \"rows\": %d,\n  \"elapsed_s\": %.3f,\n  \"rows_per_s\": %.1f,\n  \"retries\": %d,\n  \"ok\": %v\n}\n",
		v.Batch, v.Agents, v.Rows, v.Elapsed, v.RowsPerS, v.Retries, v.OK)), nil
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchsync:", err)
		os.Exit(1)
	}
}
```

> 注：`github.com/jackc/pgx/v5` 应与 M1 同步器实际使用的驱动保持一致；若 M1 用的是 `lib/pq`，此处同改。DDL 仅新增压测表 `sync_bench`，业务表零改动。

### 4.4 协议追加消息（msgpack，语义与架构计划 §7 一致）

仅追加两类消息，帧格式不变（`[4B 大端长度][msgpack body]`），不改动任何已有消息：

```
GUI → Agent : StatsQuery{window_s}                    // 拉取最近 window_s 秒（≤300）的 Snapshot 序列
Agent → GUI : StatsReport{task_id, snapshots[]}       // Snapshot 结构同 §4.1
              SoakStatus{phase, files_done, files_total, crashes, errors}  // 浸泡进度（可选）
```

- `StatsQuery/StatsReport` 只读，不影响任务流；背压/重连语义复用既有实现。
- 心跳仍 15s 默认；压测期间 GUI 每 5s 拉一次 `StatsReport` 入报告数据源。

### 4.5 浸泡编排器（Go）

```go
// agent/cmd/soakrun/main.go
// 编排：全量×3 + 增量×10（每轮增量前改写 0.5% 文件 mtime + 新增 0.1% 文件 + 删除 0.05% 文件），
// 全程监控主进程存活；中途注入崩溃（kill 一个 Worker）与损坏文件，验证 crash.log/errors.log。
package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Round struct {
	Kind      string    `json:"kind"` // full / incr
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Files     int64     `json:"files"`
	Crashes   int64     `json:"crashes"`
	Errors    int64     `json:"errors"`
	MainAlive bool      `json:"main_alive"`
}

func main() {
	agentExe := `agent.exe`
	guiExe := `gui.exe` // GUI 负责任务下发；编排器经 GUI CLI 触发扫描并轮询 TaskDone
	corpus := `D:\soak-corpus`
	agentDir := `C:\agent-data`
	log := mustCreate(`reports\soak-result.jsonl`)
	defer log.Close()

	agent := exec.Command(agentExe, `--data`, agentDir, `--pprof=:16060`)
	agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
	must(agent.Start())
	mainPID := agent.Process.Pid
	defer func() { _ = agent.Process.Kill() }()
	time.Sleep(3 * time.Second)

	var rounds []Round
	// 3 轮全量
	for i := 0; i < 3; i++ {
		r := runRound(guiExe, corpus, "full", mainPID)
		if i == 1 {
			injectCrash(agentDir) // 第二轮全量中途杀一个 Worker
		}
		r.EndedAt = time.Now()
		rounds = append(rounds, r)
		writeLine(log, rounds[len(rounds)-1])
	}
	// 10 轮增量
	for i := 0; i < 10; i++ {
		mutateCorpus(corpus)
		r := runRound(guiExe, corpus, "incr", mainPID)
		r.EndedAt = time.Now()
		rounds = append(rounds, r)
		writeLine(log, rounds[len(rounds)-1])
	}

	// 终判：主进程仍存活 + 日志对账
	alive := procAlive(mainPID)
	audit := exec.Command(`go`, `run`, `tools\audit_logs.go`, `--data`, agentDir)
	audit.Stdout = os.Stdout
	auditErr := audit.Run()
	fmt.Printf("soak done: main_alive=%v audit_err=%v rounds=%d\n", alive, auditErr != nil, len(rounds))
	if !alive || auditErr != nil {
		os.Exit(1)
	}
}

// runRound 经 GUI CLI 下发扫描并阻塞至 TaskDone，统计该轮 crash/error 增量
func runRound(guiExe, corpus, kind string, mainPID int) Round {
	r := Round{Kind: kind, StartedAt: time.Now()}
	crash0, err0 := countLines(`C:\agent-data\logs\crash.log`), countLines(`C:\agent-data\logs\errors.log`)
	cmd := exec.Command(guiExe, `scan`, `--roots`, corpus, `--phase`, `1`, `--wait`)
	must(cmd.Run())
	crash1, err1 := countLines(`C:\agent-data\logs\crash.log`), countLines(`C:\agent-data\logs\errors.log`)
	r.Crashes = crash1 - crash0
	r.Errors = err1 - err0
	r.MainAlive = procAlive(mainPID)
	return r
}

// injectCrash 杀掉一个 Worker 进程（命令行含 --worker 标记）
func injectCrash(agentDir string) {
	out, _ := exec.Command(`wmic`, `process`, `where`,
		`"CommandLine like '%--worker%' and ExecutablePath like '%agent%'"`,
		`get`, `ProcessId`).Output()
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "ProcessId" {
			continue
		}
		_ = exec.Command(`taskkill`, `/PID`, line, `/F`).Run()
		break // 只杀一个
	}
}

// mutateCorpus 增量语料变化：0.5% 改 mtime、0.1% 新增、0.05% 删除
func mutateCorpus(corpus string) {
	var files []string
	_ = filepath.Walk(corpus, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && !strings.HasSuffix(p, "corpus-manifest.json") {
			files = append(files, p)
		}
		return nil
	})
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	rnd.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
	nTouch, nDel := len(files)/200, len(files)/2000
	for i := 0; i < nTouch && i < len(files); i++ {
		now := time.Now()
		_ = os.Chtimes(files[i], now, now)
	}
	for i := nTouch; i < nTouch+nDel && i < len(files); i++ {
		_ = os.Remove(files[i])
	}
	// 新增 0.1%：复制现存文件（精确重复）
	for i := 0; i < len(files)/1000 && i < len(files); i++ {
		dst := files[i] + fmt.Sprintf(".incr%d.copy", time.Now().Unix())
		b, err := os.ReadFile(files[i])
		if err == nil {
			_ = os.WriteFile(dst, b, 0o644)
		}
	}
}

func countLines(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return int64(strings.Count(string(b), "\n"))
}

func procAlive(pid int) bool {
	err := exec.Command(`tasklist`, `/FI`, fmt.Sprintf("PID eq %d", pid), `/NH`).Run()
	if err != nil {
		return false
	}
	out, _ := exec.Command(`tasklist`, `/FI`, fmt.Sprintf("PID eq %d", pid), `/NH`).Output()
	return strings.Contains(string(out), fmt.Sprintf("%d", pid))
}

func mustCreate(path string) *os.File {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.Create(path)
	must(err)
	return f
}

func writeLine(f *os.File, r Round) {
	bs, _ := json.Marshal(r)
	_, _ = f.Write(append(bs, '\n'))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "soakrun:", err)
		os.Exit(1)
	}
}
```

### 4.6 一筛压测工具与盘基准脚本

```go
// agent/cmd/benchscreen/main.go
// 直接调用 GUI 侧 band 倒排实现（与 M3 同一份代码），生成可控重复率的合成 PDQ-256 数据集
package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"time"
)

// BandIndex 与 M3 落地实现同签名；此处声明接口以说明压测驱动方式。
// 真实工程中 benchscreen 直接 import gui/internal/screen 包，下面为对齐用最小复刻：
type BandIndex struct {
	bands  int                 // PDQ-256 分 4 个 64bit band
	tables []map[uint64][]int  // 每 band 一张 hash → 行号列表 倒排表
}

func NewBandIndex(bands int) *BandIndex {
	b := &BandIndex{bands: bands, tables: make([]map[uint64][]int, bands)}
	for i := range b.tables {
		b.tables[i] = make(map[uint64][]int)
	}
	return b
}

// Insert 256bit = 4×64bit；第 i 个 band 取第 i 个 64bit 段
func (b *BandIndex) Insert(row int, pdq [32]byte) {
	for i := 0; i < b.bands; i++ {
		k := binary.BigEndian.Uint64(pdq[i*8 : i*8+8])
		b.tables[i][k] = append(b.tables[i][k], row)
	}
}

// Candidates 返回同 band 桶大小 ≥2 的候选行对数（不做 O(n²) 展开，仅计数与抽样验证）
func (b *BandIndex) Candidates() (pairs int64, buckets int64) {
	for _, t := range b.tables {
		for _, rows := range t {
			if len(rows) >= 2 {
				buckets++
				pairs += int64(len(rows)) * int64(len(rows)-1) / 2
			}
		}
	}
	return
}

func main() {
	n := flag.Int("n", 1000000, "PDQ 条数")
	dupRate := flag.Float64("dup", 0.02, "近似重复比例（翻转 ≤8 bit 的近邻）")
	out := flag.String("out", "benchscreen-result.json", "结果输出")
	flag.Parse()

	data := make([][32]byte, *n)
	for i := 0; i < *n; i++ {
		if _, err := rand.Read(data[i][:]); err != nil {
			fatal(err)
		}
	}
	// 注入近邻：复制已有行并随机翻转 1~8 bit（模拟汉明距离 ≤ T1 的真实候选）
	nDup := int(float64(*n) * *dupRate)
	for i := 0; i < nDup; i++ {
		src := int(binary.BigEndian.Uint32(data[i][:4]) % uint32(*n))
		copy(data[*n-1-i][:], data[src][:])
		flips := 1 + i%8
		for f := 0; f < flips; f++ {
			byteIdx := (i + f*7) % 32
			data[*n-1-i][byteIdx] ^= 1 << uint(f%8)
		}
	}

	start := time.Now()
	idx := NewBandIndex(4)
	for i := 0; i < *n; i++ {
		idx.Insert(i, data[i])
	}
	insertMs := time.Since(start).Milliseconds()

	start = time.Now()
	pairs, buckets := idx.Candidates()
	scanMs := time.Since(start).Milliseconds()

	res := fmt.Sprintf("{\n  \"n\": %d,\n  \"dup_rate\": %.4f,\n  \"insert_ms\": %d,\n  \"scan_ms\": %d,\n  \"total_ms\": %d,\n  \"buckets\": %d,\n  \"candidate_pairs\": %d\n}\n",
		*n, *dupRate, insertMs, scanMs, insertMs+scanMs, buckets, pairs)
	must(os.WriteFile(*out, []byte(res), 0o644))
	fmt.Print(res)
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "benchscreen:", err); os.Exit(1) }
func must(err error)  { fatal(err) }
```

```powershell
# agent/tools/fio-baseline.ps1 — 每块盘空载顺序读带宽基准（4MB 块，与 HDD 读块对齐）
# 用法：pwsh tools/fio-baseline.ps1 -Disks D,E,F -Out reports/fio-baseline.json
param([string[]]$Disks, [string]$Out = "reports/fio-baseline.json")
$results = @()
foreach ($d in $Disks) {
    $testfile = "${d}:\fio-baseline.tmp"
    # 顺序读：iodepth=1 单流，4MB 块，直读 8GB 文件
    fio --name=seqread --filename=$testfile --size=8G --bs=4M --rw=read `
        --iodepth=1 --numjobs=1 --direct=1 --runtime=30 --time_based `
        --output-format=json --output="${d}-fio-seq.json"
    Remove-Item $testfile -ErrorAction SilentlyContinue
    $j = Get-Content "${d}-fio-seq.json" | ConvertFrom-Json
    $bw = $j.jobs[0].read.bw_bytes
    $results += [pscustomobject]@{ disk = $d; seq_read_bps = $bw; ts = (Get-Date).ToString("o") }
}
$results | ConvertTo-Json | Set-Content $Out
Write-Host "baseline written to $Out"
```

```powershell
# agent/tools/perfmon-collect.ps1 — Windows Performance Monitor 计数器采集
# 采集：每盘读带宽/读忙时间、Agent+Worker CPU、可用内存；1s 采样，输出 CSV
# 用法：pwsh tools/perfmon-collect.ps1 -DurationMin 60 -Out reports/perfmon.csv
param([int]$DurationMin = 60, [string]$Out = "reports/perfmon.csv")
$counters = @(
    '\PhysicalDisk(*)\Disk Read Bytes/sec',
    '\PhysicalDisk(*)\% Disk Read Time',
    '\Process(agent*)\% Processor Time',
    '\Memory\Available MBytes'
)
logman create counter m6perf -c $counters -si 1 -f csv -o $Out | Out-Null
logman start m6perf | Out-Null
Start-Sleep -Seconds ($DurationMin * 60)
logman stop m6perf | Out-Null
logman delete m6perf | Out-Null
Write-Host "perfmon csv at $Out"
```

---

## 5. 数据模型与配置项

### 5.1 数据模型

业务表结构零变更（以架构计划 §6 为准）。M6 仅新增：

```sql
-- 中心 PostgreSQL：同步压测专用表（benchsync 自建自清，与业务表隔离）
CREATE TABLE IF NOT EXISTS sync_bench (
  machine_id  INT          NOT NULL,
  row_seq     INT          NOT NULL,
  sha512      BYTEA        NOT NULL,
  payload     JSONB        NOT NULL,
  updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
  PRIMARY KEY (machine_id, row_seq)
);
```

调优机本地落盘：`stats.log`（JSON 行，lumberjack 滚动，单文件 ≤100MB，保留 10 份）、`reports/*.json`（各压测工具原始输出）。

### 5.2 配置项表

`config.agent.yaml` 新增 `tuning` 节；未列参数一律以架构计划 §9 默认参数表为准，本表只包含 M6 可调/新增项。**"调优后建议值"列在压测完成后回填，未回填前一律使用默认值。**

| 配置键 | 默认值 | 含义 | 调优后建议值 |
|---|---|---|---|
| `tuning.hdd.streams_per_disk` | 2 | HDD 每盘并发顺序流（可调范围 1~2，架构计划 §4.3） | （待回填） |
| `tuning.hdd.chunk_bytes` | 4194304 (4MB) | HDD 读块，与 SHA-512 流式块对齐 | （待回填） |
| `tuning.ssd.streams_per_disk` | 6 | SSD 每盘并发流（可调范围 4~8） | （待回填） |
| `tuning.ssd.chunk_bytes` | 1048576 (1MB) | SSD 读块 | （待回填） |
| `tuning.scheduler.dir_order` | true | 按目录序（path 字典序）取任务 | true |
| `tuning.backpressure.pending_bytes` | 2147483648 (2GB) | 背压：待算字节超阈值暂停读盘 | （待回填：512MB/1GB/2GB/4GB 扫描后选定） |
| `tuning.worker.count` | 0（=CPU 核数） | Worker 数，0 表示按核数 | （待回填） |
| `tuning.stats.enabled` | true | stats 采集与 stats.log | true |
| `tuning.stats.interval_ms` | 1000 | Snapshot 间隔 | 1000 |
| `tuning.pprof_addr` | ""（关闭） | pprof 监听地址，仅 loopback | 调优期 `:16060` |
| `syncer.batch_rows` | 50000 | 单次上行批量行数（架构计划 §9：5万行） | （待回填：1k/5k/10k/50k 扫描后选定） |
| `syncer.period_s` | 300 | 上行周期 5min | 300 |
| `screen.bench.n` | 1000000 | 一筛压测数据规模 | 1000000 |
| `soak.full_rounds` | 3 | 浸泡全量轮数 | 3 |
| `soak.incr_rounds` | 10 | 浸泡增量轮数 | 10 |
| `soak.corpus_root` | `D:\soak-corpus` | 浸泡语料根 | 按调优机盘位调整 |

---

## 6. 测试与验收用例

### 6.1 前置：盘基准建档（所有性能用例的参照系）

- 步骤：调优机空闲 → `pwsh tools/fio-baseline.ps1 -Disks D,E,F` → 记录每盘 `seq_read_bps`。
- 通过标准：基准值稳定（连测 3 次偏差 <5%）；HDD 典型 150~250MB/s，NVMe SSD ≥1GB/s，异常值先查盘健康再压测。

### 6.2 TC-IO-01：HDD 顺序带宽占用（对应 A1）

- 步骤：全量扫描语料 ≥20 万文件（HDD 盘）；`perfmon-collect.ps1` 同步采集；从 CSV 提取扫描稳态段（剔除首尾各 60s）`\PhysicalDisk(N)\Disk Read Bytes/sec` 均值。
- 通过标准：每块 HDD `实测均值 / fio 基准 ≥ 0.80`；`\% Disk Read Time` ≥ 90%。
- 失败处理：降至 1 条流/盘重测（排除磁头抖动）；查背压是否频繁触发（stats `backlog_bytes` 曲线贴顶说明下游算不动，转 TC-CPU 决策树）。

### 6.3 TC-IO-02：目录序 vs 随机序对照

- 步骤：同一 HDD、同一 10 万文件语料，`tuning.scheduler.dir_order` 开/关各一轮全量，比较稳态读带宽。
- 通过标准：目录序 ≥ 随机序 × 1.15（HDD 物理邻近收益显著）；若差异 <5% 说明盘缓存/分布已优，记录结论即可。
- 失败处理：检查调度器取任务 SQL 是否真按 `path` 排序、任务分桶是否按物理盘号。

### 6.4 TC-IO-03：背压阈值扫描

- 矩阵：`pending_bytes ∈ {512MB, 1GB, 2GB, 4GB}` × `hdd.streams ∈ {1, 2}`，各跑 10 万文件。
- 记录：稳态吞吐、RSS 峰值、`backlog_bytes` 贴顶时间占比。
- 通过标准：选定值满足 ① 吞吐不低于矩阵最优值的 97% ② RSS 峰值 ≤ `pending_bytes × 1.2 + 1GB`。
- 回填 §5.2 建议值。

### 6.5 TC-CPU-01：CPU 占用与读算重叠（对应 A2）

- 步骤：Worker=核数跑全量扫描稳态 10min；stats 自采 `cpu_frac` 均值；`perfmon` `\Process(agent*)\% Processor Time` 交叉验证（注意该计数器按单核 100% 归一，需除以核数）。
- 通过标准：稳态 `cpu_frac ≥ 0.85`；IO 忙（`busy_frac ≥ 0.8`）的时间片中 CPU 忙占比 ≥ 90%（证明读算重叠生效）。
- 决策树（不达标时依次执行，每步重测）：
  1. `cpu_frac` 低且 `disk busy` 低 → 调度饿死：查任务分发锁/队列空转（pprof goroutine dump）。
  2. `cpu_frac` 低且 `disk busy` 高 → IO-bound：HDD 加流无效（上限 2），接受为 IO 瓶颈场景，但混合盘整体仍需 ≥85%。
  3. pprof 显示 GC >10% → 查每文件分配（msgpack 复用 buffer、4MB 读块用 `sync.Pool`）。
  4. pprof 显示 SQLite 写热点 → 批量事务提交（M1 应已做；此处只验证不重构）。
  5. cgo 边界开销显著 → 记录数据，提交架构评审（不自行改 DLL）。

### 6.6 TC-SYNC-01：批量大小扫描（对应 A6）

- 矩阵：`batch ∈ {1000, 5000, 10000, 50000}` × `agents ∈ {1, 2, 4}`，`--rows 1000000`，每组合 3 次取中位。
- 通过标准：每组 `ok=true`（对账行数 = 100 万，零丢失零重复）；选定默认值吞吐 ≥ 矩阵最优值的 95%，且单批事务时长 ≤ 30s（避免长事务阻塞）。
- 回填 `syncer.batch_rows` 建议值。

### 6.7 TC-SYNC-02：5min/5万行策略回归 + 断点重发

- 步骤：真实 Agent 本地库造 100 万行积压（`sync_queue`），按默认 `syncer.period_s=300 / batch_rows=50000` 连跑 3 个周期；第 2 个周期中 `taskkill` 一次到 PostgreSQL 的连接（`pg_terminate_backend`）。
- 通过标准：3 周期内积压清零；重发后中心库行数/主键与本地 100% 一致（`ON CONFLICT` 幂等，无重复行）；`agent.log` 有断线重试记录。

### 6.8 TC-SCREEN-01：百万级一筛耗时（对应 A7）

- 步骤：`benchscreen -n 1000000 -dup 0.02`；同时记录进程峰值内存。
- 通过标准：`total_ms < 10000`（GUI 机内存 ≥16GB）；`candidate_pairs` 量级与注入重复率自洽（同量级，不爆炸）；规模 10 万/50 万档耗时近似线性。
- 备注：压测用最小复刻 `BandIndex` 先跑通管线；正式结果必须以 GUI 侧 M3 实现（`benchscreen` import 该包）为准，两者结果偏差 >20% 时以 M3 实现为准并排查复刻版。

### 6.9 TC-SOAK-01：百万文件浸泡（对应 A3/A4/A5）

- 步骤：`corpusgen -total 1000000` 构建语料（分布见 §4.2 头注）→ 分布到 HDD×2 + SSD×1 → `soakrun` 执行 3 全量 + 10 增量（≥24h，语料不足时多轮复用同一语料）。
- 通过标准：
  - A3：主进程全程存活，`soak-result.jsonl` 每轮 `main_alive=true`。
  - A4：第 2 轮全量注入的 Worker 崩溃在 `crash.log` 恰有 ≥1 行 `{ts, pid, file, exit_code}`；stats 显示补位耗时 ≤5s；该轮扫描正常完成。
  - A5：`audit_logs.go` 对账通过 —— 语料清单中 `corrupt/oversize` 文件每个在每轮 `errors.log` 恰有一行（首轮全量）或不出现（增量轮未触碰），且 SQLite `files.status ∈ {failed, partial}` 与 `error` 字段一致。
  - 语料中 `normal` 文件一阶段特征覆盖率 ≥ 99.9%（扣除增量轮新增未扫部分）。
- 失败处理：主进程崩溃即中止浸泡，保留现场（pprof heap/goroutine dump、`agent.log` 尾部 1000 行、SQLite 快照）作为 P0 缺陷。

### 6.10 测试矩阵汇总

| 维度 | 取值 | 用例 |
|---|---|---|
| 盘类型 | HDD×2 / SSD×1 | 全部 IO 用例 |
| HDD 流/盘 | 1 / 2 | TC-IO-01/03 |
| SSD 流/盘 | 4 / 6 / 8 | TC-IO-03 扩展 |
| 背压 | 512MB / 1GB / 2GB / 4GB | TC-IO-03 |
| 任务顺序 | 目录序 / 随机 | TC-IO-02 |
| Worker 数 | 核数×{0.5, 1, 1.5} | TC-CPU-01 扩展 |
| 同步批量 | 1k / 5k / 10k / 50k 行 | TC-SYNC-01 |
| 同步并发 Agent | 1 / 2 / 4 | TC-SYNC-01 |
| 一筛规模 | 10万 / 50万 / 100万 | TC-SCREEN-01 |
| 浸泡轮次 | 全量×3 + 增量×10 | TC-SOAK-01 |

---

## 7. 报告模板（`reports/perf-report.md` 骨架）

`perfreport` 工具聚合 `stats.log`、各 `*-result.json`、fio 基准与 perfmon CSV 生成：

```markdown
# M6 压测报告 — <日期> / <调优机标识>

## 1. 环境
- 机器：CPU <型号/核数> / RAM <GB> / OS <版本>
- 盘：<盘符→类型→fio 顺序读基准 MB/s>
- 版本：Agent <git sha> / mediacore.dll <版本> / PostgreSQL 16.x
- 配置：默认值 + tuning 覆盖项清单

## 2. 验收结论
| 指标 | 标准 | 实测 | 结论 |
|---|---|---|---|
| A1 HDD 顺序带宽 | ≥80% | D: xx% E: xx% | PASS/FAIL |
| A2 CPU 占用 | ≥85% | xx% | PASS/FAIL |
| A3 主进程零崩溃 | 0 | 0 | PASS/FAIL |
| A4 崩溃恢复 | ≤5s 补位 | xs | PASS/FAIL |
| A5 日志对账 | 100% | xx/xx | PASS/FAIL |
| A6 同步吞吐/幂等 | 对账一致 | xxxx rows/s | PASS/FAIL |
| A7 一筛 100 万 | <10s | xs | PASS/FAIL |

## 3. IO 调优矩阵（每盘吞吐曲线图 + 表格）
## 4. CPU 分析与 pprof 热点 Top10
## 5. 同步压测矩阵（批量×并发 → rows/s，含重试次数）
## 6. 一筛耗时与内存
## 7. 浸泡测试摘要（轮次表、crash/errors 统计、覆盖率）
## 8. 单文件耗时分布（P50/P90/P99/Max，按 image/video 分列）
## 9. 最终建议默认值（与架构计划 §9 对照，差异项加粗说明理由）
## 10. 遗留问题与风险
```

---

## 8. 风险与注意事项

1. **语料构建耗时与磁盘空间**：100 万文件语料约需 0.8~1.5TB（含 3% 超大文件）。`corpusgen` 的 `normal_vid` 走 ffmpeg 单进程较慢，可并行起 4~8 个实例分段构建（`-total` 切分后合并 manifest）。超大文件用稀疏写入（`genCorrupt` 顺序写即可，避免真解码）。
2. **pprof 仅限调优机**：`tuning.pprof_addr` 生产必须为空；绑定地址强制 loopback（代码已硬编码 `127.0.0.1` 前缀）。
3. **stats 采样开销**：`quantile` 每秒钟对 ≤100 万样本排序一次，单次约 50~100ms，在独立 goroutine 执行、不阻塞采集；若实测影响 CPU 指标，降为每 5s 一次分位计算。
4. **背压与内存联动**：`pending_bytes` 上调时必须同步核算 图片内存驻留阈值 256MB × Worker 数的极端驻留，防 OOM；调优矩阵中 RSS 超限的组合直接淘汰。
5. **压测表与业务表隔离**：`sync_bench` 只能由 `benchsync` 建/清；严禁把压测流量打入业务特征表，污染 GUI 分析数据。
6. **perfmon 计数器口径**：`\Process(*)\% Processor Time` 按单核 100% 归一，多核合计需除以核数再与 stats `cpu_frac` 对比；`\PhysicalDisk` 实例名与盘符映射以 `IOCTL_STORAGE_GET_DEVICE_NUMBER` 的盘号为准，避免 A1 张冠李戴。
7. **浸泡中断恢复**：浸泡 ≥24h，调优机断电/重启后 `soakrun` 从 `soak-result.jsonl` 最后一轮续跑，已完成轮次不重跑；主进程崩溃（A3 失败）则整体标 FAIL，修缺陷后从头重新浸泡。
8. **fio 为唯一引入的外部工具**：仅用于盘基准，不进入产品代码；若环境禁装，可用 `diskspd` 平替，参数语义相同（4MB 顺序读、direct IO）。

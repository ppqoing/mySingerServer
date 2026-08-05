# M1 骨架 — 详细实施文档

> 依据：`docs/architecture-plan.md` v1.2（下称 plan）。本文档不改变 plan 的任何选型、默认参数、协议语义与数据模型；所有参数默认值与 plan 第 9 节一致。Everything 的纯 Go 动态调用决策见 ADR-0001。
> 里程碑目标（plan 第 10 节）：**双机普扫，精确重复组在 GUI 正确汇总**。
> 验收范围修订（2026-07-27）：项目所有者明确决定不执行第二台独立 Windows 主机验收。下文 T4-2、T8-6、T9-1 的第二主机部分按范围豁免关闭；该标记表示“不再要求”，不表示第二主机实测通过。

---

## 1. 目标与范围

### 1.1 交付物

M1 交付三个可部署单元与一套联调验收：

| 单元 | 内容 | 形态 |
|---|---|---|
| **Agent 主进程** | TCP 服务端（长度前缀 + msgpack 帧、15s 心跳、任务级 ACK、断线任务续跑/重连续传）；Everything SDK 通过 `x/sys/windows` 按需动态调用（失败回退目录遍历，见 ADR-0001）；物理盘号映射（盘符 → Volume GUID → `IOCTL_STORAGE_GET_DEVICE_NUMBER` + 寻道惩罚属性区分 HDD/SSD）；SQLite 本地库（modernc.org/sqlite，WAL，plan 6.1 DDL）；SHA-512 流式计算（Go `crypto/sha512`，4MB 块，按盘并发调度）；定时上行同步 PostgreSQL（5min / 5 万行，`ON CONFLICT` 幂等） | `agent.exe`（CGO_ENABLED=0，windows/amd64） |
| **GUI 独立进程** | 到多台 Agent 的 TCP 连接池（指数退避重连、心跳）；ScanTask 下发；TaskProgress / FeatureResult / TaskDone / Error 接收；内嵌 Web 页面（agent 状态、任务进度、精确重复组） | `gui.exe`（CGO_ENABLED=0） |
| **PostgreSQL 16 中心库** | plan 6.2 全部 DDL（files / image_features / video_features / video_frames / dup_groups / dup_members / scan_tasks） | DDL 脚本 + docker-compose（可选） |

### 1.2 M1 明确不做什么

以下属于后续里程碑，M1 **不实现、不预留半成品代码**（协议结构体与 DDL 除外——为保证前向兼容，M1 一次性定义完整）：

- **不实现** `mediacore.dll` 与任何解码、PDQ-256、pHash、Sobel、视频抽帧（M2/M4）；SHA-512 暂由 Go 标准库在 Agent 主进程内计算，DLL 化与 Worker 子进程化留给 M2（为此 M1 把计算抽象为 `Hasher` 接口，见 4.5）。
- **不实现** Worker 进程池、崩溃监督、单文件看门狗（M2）。`CrashNotice` 消息仅定义协议，Agent M1 不产生、GUI M1 收到只记日志。
- **不实现** 一筛（band 倒排、汉明距离、剪枝）（M3）；GUI 的精确重复组用中心库实时 SQL 分组得出，`dup_groups` / `dup_members` 结果表 M1 只建表不写入（写入从 M3 分析管线开始）。
- **不实现** Phase2Task / DeleteTask 的处理逻辑（M4/M5）。Agent M1 收到这两类消息回复 `Error{stage:"proto", msg:"unsupported in M1"}`，不崩溃、不断连。
- **不实现** 提权删除 Helper、delete.log（M5）。
- **不实现** 视频缩略图缓存目录与 ffprobe/ffmpeg 调用（M2）。
- **不做** 盘级背压限流（待算字节阈值暂停读盘）的完整实现（M6 调优）；M1 用每盘固定并发流数（HDD 2 / SSD 6）天然限流。
- **不支持** Linux/macOS（`diskmap` 与 Everything 包装仅 Windows；代码用 build tag 隔离，但不提供其他平台实现）。
- **不做** TLS 与鉴权（局域网内网直连，plan 第 7 节无此要求；如需公网部署再议）。

### 1.3 M1 内的口径约定（plan 未细化处，本文档补足，不冲突）

- `FeatureResult` 采用**批量流式回传**：每帧最多 512 条或 200ms 刷一次，帧内带单调递增 `seq`，GUI 可据此发现丢帧（丢帧不补发——数据以 SQLite/中心库为准，回传只做展示）。
- **断线续传**：Agent 侧任务不随 GUI 断线而取消（计算结果本就要落本地库并上行）；GUI 重连后以相同 `task_id` 重发 `ScanTask`，Agent 发现该任务仍在运行则回 `TaskAck{reason:"resumed"}` 并重新绑定回传通道，进度与 FeatureResult 从当前位置继续。
- **同步触发条件**：严格按 plan 6.3 —— 周期 5min 或积压 5 万行。为便于验收，同步周期是配置项（`sync.interval_s`），验收环境可调小到 10s，不改变语义。
- **文件分类**：按扩展名把文件分为 image / video / other 三类（默认扩展名表见 5.3）。M1 对**全部三类文件都算 SHA-512**（精确去重需要），分类只决定 `missing_mask` 中一阶段特征位（PDQ/缩略图 PDQ 留给 M2）。
- `phase1_done` 的判定与 plan 6.1 注释一致（sha/pdq/thumb 齐才算 1），M1 只清 SHA-512 位，因此 M1 结束后媒体文件 `phase1_done=0`、"other" 类文件 `phase1_done=1`，属预期。

---

## 2. 任务分解（Checklist）

> 每项粒度到可单独验收。依赖关系自上而下，同组内可并行。

### T1 协议栈（`internal/proto`，Agent/GUI 共用）

- [x] T1-1 定义全部 msgpack 消息结构体与消息类型常量（4.1），含 M2+ 保留消息（Phase2Task/DeleteTask/DeleteReport/CrashNotice/ConfigPush/StatsQuery/StatsReport）。
- [x] T1-2 实现帧编解码 `Conn`：`[4B 大端长度][msgpack envelope]`，最大帧 16MB，并发写安全；默认 30s 写 deadline 防止不读对端永久占锁（4.2，v1.2 覆盖旧值）。
- [x] T1-3 实现 `Heartbeat`（每 15s 发 Ping）与读循环约定（每次读前 `SetReadDeadline = now + 45s`；收到 Ping 必回 Pong）。
- [x] T1-4 单元测试：帧 roundtrip、边界帧长（0 / 超 16MB 拒绝）、并发写不串帧、垃圾字节返回错误而非 panic。

### T2 Agent 主进程骨架

- [x] T2-1 `agent.json` 配置加载与默认值（5.2），`--config` 命令行参数。
- [x] T2-2 slog JSON 日志：`agent.log`（lumberjack 滚动）、`errors.log` 一行一报错（8 节日志规范的 M1 子集）。
- [x] T2-3 TCP 服务端：监听、每连接一个读循环、连接建立即主动发 `Hello`、Ping/Pong、45s 读超时断连。
- [x] T2-4 ScanTask 受理：`TaskAck`（accepted / resumed / already_done / rejected）；同 `task_id` 仅在完整信封一致时幂等，参数变化拒绝；ACK 成功写出后才启动扫描；任务不随连接断开取消，重连后重绑定回传通道。
- [x] T2-5 任务执行完毕发 `TaskDone{stats}`；任务状态在内存保留 10 分钟供重连查询；already_done ACK 携带最终统计。
- [x] T2-6 收到 Phase2Task / DeleteTask / ConfigPush / 未知类型：回 `Error{stage:"proto"}`，连接保持。

### T3 Everything SDK 枚举

- [x] T3-1 `third_party/everything_sdk` 就位：Everything64.dll（voidtools SDK 1.4+）；按 ADR-0001 无需导入库。
- [x] T3-2 纯 Go 动态包装 `internal/enum`：`EverythingEnumerator`（探测、MatchPath 按 root 枚举全路径 + size + mtime，跳过目录项）（4.3）。
- [x] T3-3 `WalkerEnumerator` 回退实现（`filepath.WalkDir`，长路径 `\\?\` 前缀与短路径统一展开；目录访问与元数据错误上抛为扫描级失败，不把不完整枚举误报成功）。
- [x] T3-4 启动与运行期探测：Everything DLL/IPC/查询/根结果不可用 → 告警日志 + 该根切换 Walker（plan 11 节风险缓解）。
- [x] T3-5 Walker 自动化覆盖中文文件与空目录（[测试](../../internal/enum/enumerator_test.go)），真实非空 Everything 索引与 Walker 一致；目标机真实 437 UTF-16 字符 Unicode 路径经 Walker/ResilientEnumerator 枚举验证通过（[验收证据](../acceptance/2026-07-27-m1-longpath-certutil.md)）。

### T4 物理盘号映射

- [x] T4-1 `internal/diskmap`：`MountPointOf`（任意路径 → 卷挂载点）与 `Resolve`（挂载点 → Volume GUID → `IOCTL_STORAGE_GET_DEVICE_NUMBER` → `DeviceNumber` 作为 `disk_no`；`IOCTL_STORAGE_QUERY_PROPERTY` 寻道惩罚属性 → `IsSSD`）（4.4）。
- [x] T4-2 当前主机 C/D/F/G/H/I 六个本地卷已与 `Get-Partition` / `Get-Disk` / `Get-PhysicalDisk` 全量对拍，盘号、分区号和 SSD 判定一致（[验收证据](../acceptance/2026-07-27-m1-diskmap.md)）；第二台验收主机按 2026-07-27 项目所有者决定范围豁免，未实测。
- [x] T4-3 寻道惩罚查询失败时保守按 HDD（2 流）处理、暴露 `MediaTypeKnown=false` 并记告警日志。

### T5 SQLite 本地库

- [x] T5-1 `internal/store`：打开库（WAL + synchronous=NORMAL + busy_timeout=5000），幂等执行 plan 6.1 全量 DDL与旧库迁移（4.6）。
- [x] T5-2 枚举落库 `UpsertEnumerated`（万行一事务；size+mtime 未变且 sha512 已有 → 保持原状，否则重置 pending 并置 SHA-512 缺失位）。
- [x] T5-3 哈希结果回写 `ApplyHashResults`（500 行一事务；同事务写带单调 generation 的 `sync_queue`）。
- [x] T5-4 缺失字段剪枝查询 `PendingSnapshot`：只取 `missing_mask` 含 SHA-512 位的文件，按 `disk_no` 分桶、桶内按路径排序（目录序，plan 4.3）。
- [x] T5-5 单元测试：DDL/迁移幂等；重复枚举无重复行；mtime 变化重新 pending；sync_queue 去重且并发新代际不会被旧同步清除。

### T6 SHA-512 精确去重计算

- [x] T6-1 `Hasher` 接口 + `GoHasher` 实现：`crypto/sha512` 流式 4MB 块（4.5）。
- [x] T6-2 盘级调度：每物理盘按介质类型起 2（HDD）/ 6（SSD）条并发哈希流，桶内目录序取任务。
- [x] T6-3 失败处理：打不开 / 读失败 → 写 `errors.log` 一行、发 `Error` 消息、库标 `status='failed'`，整轮不中断；批次落库失败不得伪报成功。
- [x] T6-4 Go 标准库已知向量与跨 4MB 块自动化通过；目标机 437 字符 Unicode 路径文件的 GoHasher 与 `certutil -hashfile ... SHA512` 摘要完全一致；另以真实 Agent 完成 3GiB 文件流式哈希（[对拍证据](../acceptance/2026-07-27-m1-longpath-certutil.md)、[大文件扫描证据](../acceptance/2026-07-27-m1-postgres-outage.md)）。

### T7 PostgreSQL 中心库与上行同步

- [x] T7-1 中心库 DDL 脚本 `deploy/central.sql`（plan 6.2/v1.2 全量，4.7.1），在真实 PG 16 容器执行成功。
- [x] T7-2 `internal/syncer`：5min 周期 + 积压 ≥5 万行触发；每批 5000 行一事务；files 表 `ON CONFLICT (machine_id,path) DO UPDATE` 幂等上行；仅按未变化 generation 标记同步（4.7.2）。
- [x] T7-3 真实 Agent/GUI + PG 16 验收通过：PG 停止后扫描完成、本地 generation 队列保留；PG 恢复后 7.4s 内自动清队列并上行相同 SHA-512（[验收证据](../acceptance/2026-07-27-m1-postgres-outage.md)）。
- [x] T7-4 真实 PG 集成测试：同一批数据同步两次，中心库行数不变、无重复；pgx 与 SMALLINT 边界已覆盖。

### T8 GUI 独立进程

- [x] T8-1 `gui.json` 配置：监听地址、PG DSN、Agent 列表（machine_id + addr）。
- [x] T8-2 Agent 连接池：每 Agent 一个连接协程，断线指数退避重连（1s 起步 ×2，封顶 30s），15s 心跳，45s 读超时；连接状态实时可查。
- [x] T8-3 消息分发与恢复：进度/结果/完成/错误按单调状态机更新任务注册表；首次完整 ScanTask 信封必须成功写 PG 后才允许发送，同 UUID 不同信封返回冲突；GUI 重启恢复并在 Hello 后自动重发。
- [x] T8-4 HTTP API：`/api/agents`、`/api/scan`、`/api/tasks`、`/api/dup_groups`、`/api/dup_groups/{sha512}`（4.8.3）。
- [x] T8-5 内嵌 Web 页面（embed.FS，单页）：Agent 在线状态、扫描任务下发表单、任务进度/跳过/失败、精确重复组表（可展开成员）（4.8.4）。
- [x] T8-6 本机双 Agent + GUI kill/restart 自动恢复黑盒已通过；两台独立 Windows 主机上的 Agent kill/restart 手工验收按 2026-07-27 项目所有者决定范围豁免，未实测。

### T9 双机联调验收

- [x] T9-1 两台独立 Windows 主机的 AC-1 ~ AC-10 验收按 2026-07-27 项目所有者决定从当前交付范围移除；保留本机双 Agent 自动化证据，不宣称第二主机通过。

---

## 3. 目录与文件结构

单一 Go module（`module dedup`），Agent 与 GUI 共享 `internal/proto`。`internal/enum` 通过 `x/sys/windows` 动态调用 Everything SDK；Agent 与 GUI 均以 `CGO_ENABLED=0` 构建。

```
mySingerServer/
├── go.mod                         # module dedup，Go 1.22+
├── go.sum
├── cmd/
│   ├── agent/main.go              # Agent 入口（--config）
│   └── gui/main.go                # GUI 入口（--config）
├── internal/
│   ├── proto/
│   │   ├── message.go             # 全部 msgpack 消息结构体 + 类型常量 + Decode
│   │   ├── conn.go                # 帧编解码 Conn + Heartbeat
│   │   └── conn_test.go
│   ├── config/
│   │   ├── agent.go               # AgentConfig + 默认值 + Load
│   │   └── gui.go                 # GUIConfig + 默认值 + Load
│   ├── enum/
│   │   ├── enumerator.go          # Enumerator 接口 + FileRecord + longPath
│   │   ├── everything_windows.go  # x/sys/windows 动态加载 Everything64.dll
│   │   ├── path_windows.go        # 短路径规范化为长路径
│   │   ├── resilient.go           # Everything 失败/空结果时回退且去重
│   │   └── walker.go              # WalkerEnumerator（纯 Go 回退）
│   ├── diskmap/
│   │   └── diskmap_windows.go     # 盘符→GUID→IOCTL（build: windows）
│   ├── store/
│   │   ├── db.go                  # Open/Close + DDL 迁移
│   │   ├── ddl.go                 # plan 6.1 DDL 常量
│   │   ├── files.go               # UpsertEnumerated / PendingSnapshot / ApplyHashResults
│   │   ├── syncq.go               # sync_queue 读写
│   │   └── store_test.go
│   ├── agent/
│   │   ├── server.go              # TCP 服务端 + 连接读循环
│   │   ├── scan.go                # ScanManager：枚举→落库→剪枝→盘级调度→回传
│   │   ├── hasher.go              # Hasher 接口 + GoHasher（4MB 块）
│   │   ├── classify.go            # 扩展名分类 + MissingBase
│   │   └── logging.go             # agent.log / errors.log 初始化
│   ├── syncer/
│   │   └── syncer.go              # 定时上行（5min/5万行，ON CONFLICT 幂等）
│   ├── centraldb/
│   │   └── pg.go                  # pgxpool 封装（Agent syncer 与 GUI 共用）
│   └── gui/
│       ├── pool.go                # AgentConn 连接池 + 重连 + 分发
│       ├── tasks.go               # 任务注册表（内存）
│       ├── httpapi.go             # REST API
│       ├── web.go                 // embed.FS 静态页
│       └── web/index.html         # 单页（内联 JS/CSS）
├── deploy/
│   ├── central.sql                # plan 6.2 中心库 DDL
│   ├── agent.example.json
│   └── gui.example.json
└── docs/
    ├── architecture-plan.md
    ├── todolist.md
    └── details/M1-skeleton.md     # 本文档
```

Go 依赖（`go.mod`）：`modernc.org/sqlite`、`github.com/vmihailenco/msgpack/v5`、`github.com/jackc/pgx/v5`（含 `pgxpool`）、`golang.org/x/sys`、`github.com/google/uuid`、`gopkg.in/natefinch/lumberjack.v2`。

选型冻结（v1.2）：msgpack 必须使用 map（具名字段）编码（`github.com/vmihailenco/msgpack/v5` 默认即 map 编码），协议演进只允许追加字段，禁止数组编码、禁止改序、禁止复用字段名；PostgreSQL 驱动钉死 `github.com/jackc/pgx/v5`。

---

## 4. 关键接口与结构体定义

### 4.1 msgpack 消息（`internal/proto/message.go`）

语义与 plan 第 7 节一致。帧内统一包一层 `Envelope{t: 消息类型, b: 消息体原始 msgpack 字节}`，接收方先解 Envelope 再按类型解体。**M1 实现的处理**：Ping/Pong/Hello/ScanTask/TaskAck/TaskProgress/FeatureResult/TaskDone/Error；其余类型 M1 仅定义（前向兼容），Agent 收到回 `Error{stage:"proto"}`。

```go
package proto

// 消息类型。1~9 连接管理；10~19 GUI→Agent；20~29 Agent→GUI。
const (
	MsgPing  uint8 = 1
	MsgPong  uint8 = 2
	MsgHello uint8 = 3

	MsgScanTask   uint8 = 10
	MsgTaskAck    uint8 = 11
	MsgPhase2Task uint8 = 12 // M4 实现处理
	MsgDeleteTask uint8 = 13 // M5 实现处理
	MsgConfigPush uint8 = 14 // 预留

	MsgTaskProgress  uint8 = 20
	MsgFeatureResult uint8 = 21
	MsgTaskDone      uint8 = 22
	MsgError         uint8 = 23
	MsgCrashNotice   uint8 = 24 // M2 实现处理
	MsgDeleteReport  uint8 = 25 // M5 实现处理
)

// ProtocolVersion 随不兼容变更递增；Hello 携带，双方不一致时拒绝连接。
const ProtocolVersion = 1

// 字段位掩码（missing_mask / Phase2Item.FieldsMask 共用）。M1 只用 FieldSHA512。
const (
	FieldSHA512     uint32 = 1 << 0 // 精确哈希（所有类型文件）
	FieldPDQ256     uint32 = 1 << 1 // 图片一阶段 PDQ-256
	FieldThumb      uint32 = 1 << 2 // 视频一阶段：缩略图 + 缩略图 PDQ-256
	FieldPHashParts uint32 = 1 << 3 // 二阶段：分区 pHash
	FieldSobelHist  uint32 = 1 << 4 // 二阶段：Sobel 结构块直方图
	FieldVideo6F    uint32 = 1 << 5 // 二阶段：视频 6 帧
)

// files.status 取值（plan 6.1）。
const (
	StatusPending = "pending"
	StatusDone    = "done"
	StatusPartial = "partial"
	StatusFailed  = "failed"
	StatusCrash   = "crash"
)

// ---------- 连接管理 ----------

type Ping struct {
	TS int64 `msgpack:"ts"` // Unix 毫秒，回 Pong 时原样带回
}

type Pong struct {
	TS int64 `msgpack:"ts"`
}

// Hello 由 Agent 在连接建立后主动发送，GUI 校验 machine_id 与配置一致。
type Hello struct {
	Version   int    `msgpack:"version"`
	MachineID string `msgpack:"machine_id"`
	Hostname  string `msgpack:"hostname"`
	PID       int    `msgpack:"pid"`
}

// ---------- GUI → Agent ----------

// ScanTask 下发普扫（phase=1）或（M4 起）带 fields 要求的任务。
type ScanTask struct {
	TaskID  string      `msgpack:"task_id"` // GUI 生成的 UUID；断线重连用同一 task_id 续传
	Roots   []string    `msgpack:"roots"`   // 本机绝对路径，如 ["D:\\media", "E:\\"]
	Phase   uint8       `msgpack:"phase"`   // 1=普扫；2=二阶段（M1 只接受 1）
	Options ScanOptions `msgpack:"options"`
}

type ScanOptions struct {
	Rescan     bool     `msgpack:"rescan"`               // true=忽略剪枝，全部重算 SHA-512
	Extensions []string `msgpack:"extensions,omitempty"` // 非空时只处理这些扩展名（小写带点，如 ".jpg"）
}

type TaskAck struct {
	TaskID   string `msgpack:"task_id"`
	Accepted bool   `msgpack:"accepted"`
	Reason   string `msgpack:"reason"` // accepted / resumed / already_done / rejected:<原因>
	Total    int64  `msgpack:"total"`  // 预计处理总数；-1=尚未枚举完未知
}

// Phase2Item / Phase2Task：M4 按需补算（plan 7 节）。M1 仅定义。
type Phase2Item struct {
	Path       string `msgpack:"path"`
	FieldsMask uint32 `msgpack:"fields_mask"` // FieldPHashParts|FieldSobelHist|FieldVideo6F
}

type Phase2Task struct {
	TaskID string       `msgpack:"task_id"`
	Items  []Phase2Item `msgpack:"items"`
}

type DeleteTask struct {
	TaskID  string   `msgpack:"task_id"`
	Entries []string `msgpack:"entries"`
}

type ConfigPush struct {
	KV map[string]string `msgpack:"kv"`
}

// ---------- Agent → GUI ----------

type TaskProgress struct {
	TaskID string  `msgpack:"task_id"`
	Done   int64   `msgpack:"done"`  // 已处理（含失败）
	Total  int64   `msgpack:"total"` // 本轮需处理数（剪枝后）
	Speed  float64 `msgpack:"speed"` // 最近 10s 平均，文件/秒
}

// FeatureItem 是单文件单阶段结果。M1 仅填 Path/SHA512/Size/MTime/Status/Err；
// M2 起按 phase 追加 PDQ/宽高/时长/缩略图等字段（msgpack 缺省字段兼容）。
type FeatureItem struct {
	Path   string `msgpack:"path"`
	SHA512 string `msgpack:"sha512,omitempty"` // hex，128 字符；失败为空
	Size   int64  `msgpack:"size"`
	MTime  int64  `msgpack:"mtime"` // Unix 秒
	Status string `msgpack:"status"`
	Err    string `msgpack:"err,omitempty"`
}

// FeatureResult 批量流式回传：每帧 ≤512 条或 200ms 刷一次；Seq 从 1 单调递增。
type FeatureResult struct {
	TaskID string        `msgpack:"task_id"`
	Seq    uint64        `msgpack:"seq"`
	Items  []FeatureItem `msgpack:"items"`
}

type TaskStats struct {
	Total     int64 `msgpack:"total"`      // 枚举到的文件总数
	Done      int64 `msgpack:"done"`       // 本轮实际计算数
	Skipped   int64 `msgpack:"skipped"`    // 剪枝跳过数
	Failed    int64 `msgpack:"failed"`     // 计算失败数
	ElapsedMS int64 `msgpack:"elapsed_ms"`
}

type TaskDone struct {
	TaskID string    `msgpack:"task_id"`
	Stats  TaskStats `msgpack:"stats"`
}

// Error 单行错误上报（同时写 Agent 本地 errors.log）。Stage: enum/hash/sync/proto。
type Error struct {
	TaskID string `msgpack:"task_id,omitempty"`
	Path   string `msgpack:"path,omitempty"`
	Stage  string `msgpack:"stage"`
	Msg    string `msgpack:"msg"`
}

// CrashNotice：M2 起 Worker/ffmpeg 崩溃时上报（plan 7 节）。M1 仅定义。
type CrashNotice struct {
	PID      int    `msgpack:"pid"`
	Path     string `msgpack:"path"`
	ExitCode int    `msgpack:"exit_code"`
}

type DeleteResult struct {
	Path string `msgpack:"path"`
	OK   bool   `msgpack:"ok"`
	Err  string `msgpack:"err,omitempty"`
}

type DeleteReport struct {
	TaskID  string         `msgpack:"task_id"`
	Entries []DeleteResult `msgpack:"entries"`
}
```

消息解码入口（同文件）：

```go
package proto

import (
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// Decode 按消息类型把 Envelope.Body 解成具体结构体指针。
func Decode(msgType uint8, body []byte) (any, error) {
	var v any
	switch msgType {
	case MsgPing:
		v = &Ping{}
	case MsgPong:
		v = &Pong{}
	case MsgHello:
		v = &Hello{}
	case MsgScanTask:
		v = &ScanTask{}
	case MsgTaskAck:
		v = &TaskAck{}
	case MsgPhase2Task:
		v = &Phase2Task{}
	case MsgDeleteTask:
		v = &DeleteTask{}
	case MsgConfigPush:
		v = &ConfigPush{}
	case MsgTaskProgress:
		v = &TaskProgress{}
	case MsgFeatureResult:
		v = &FeatureResult{}
	case MsgTaskDone:
		v = &TaskDone{}
	case MsgError:
		v = &Error{}
	case MsgCrashNotice:
		v = &CrashNotice{}
	case MsgDeleteReport:
		v = &DeleteReport{}
	default:
		return nil, fmt.Errorf("proto: unknown message type %d", msgType)
	}
	if err := msgpack.Unmarshal(body, v); err != nil {
		return nil, fmt.Errorf("proto: decode type=%d: %w", msgType, err)
	}
	return v, nil
}
```

### 4.2 帧协议与连接（`internal/proto/conn.go`）

线格式：`[4B 大端长度 N][msgpack(Envelope)]`，N = envelope 编码后字节数，上限 16MB（防对端异常撑爆内存）。Envelope 用 msgpack map `{t, b}`，`b` 为消息体的原始 msgpack 字节（二次解码，免 union 类型体操）。每次写帧默认设置 30 秒 deadline，避免持续发心跳但不读取的对端永久占住写锁。

```go
package proto

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// MaxFrameSize 帧上限 16MB（FeatureResult 批量帧远小于此，余量充足）。
const MaxFrameSize = 16 << 20

var ErrFrameTooLarge = errors.New("proto: frame length invalid or exceeds 16MB")

type envelope struct {
	Type uint8              `msgpack:"t"`
	Body msgpack.RawMessage `msgpack:"b"`
}

// Conn 是一条已建立连接上的帧读写器。WriteFrame 并发安全；ReadFrame 只能单 goroutine 调用。
type Conn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
	wm sync.Mutex
	wt time.Duration
}

func NewConn(nc net.Conn) *Conn {
	return &Conn{
		nc: nc,
		r:  bufio.NewReaderSize(nc, 64<<10),
		w:  bufio.NewWriterSize(nc, 256<<10),
		wt: 30 * time.Second,
	}
}

func (c *Conn) Close() error                  { return c.nc.Close() }
func (c *Conn) RemoteAddr() net.Addr          { return c.nc.RemoteAddr() }
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.nc.SetReadDeadline(t)
}

// WriteFrame 编码并发送一帧，并发安全。
func (c *Conn) WriteFrame(msgType uint8, v any) error {
	body, err := msgpack.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto: marshal body type=%d: %w", msgType, err)
	}
	payload, err := msgpack.Marshal(envelope{Type: msgType, Body: msgpack.RawMessage(body)})
	if err != nil {
		return fmt.Errorf("proto: marshal envelope: %w", err)
	}
	if len(payload) == 0 || len(payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	c.wm.Lock()
	defer c.wm.Unlock()
	if c.wt > 0 {
		if err := c.nc.SetWriteDeadline(time.Now().Add(c.wt)); err != nil {
			return err
		}
		defer c.nc.SetWriteDeadline(time.Time{})
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(payload); err != nil {
		return err
	}
	return c.w.Flush()
}

// ReadFrame 读取一帧，返回消息类型与消息体原始字节（交 Decode 二次解码）。
func (c *Conn) ReadFrame() (uint8, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrameSize {
		return 0, nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return 0, nil, err
	}
	var env envelope
	if err := msgpack.Unmarshal(buf, &env); err != nil {
		return 0, nil, fmt.Errorf("proto: bad envelope: %w", err)
	}
	return env.Type, env.Body, nil
}

// Heartbeat 每 interval 发一帧 Ping，连接任一侧各跑一个（双向心跳，plan 7 节）。
// 判活不依赖 Pong：读循环每次读前 SetReadDeadline(now + 3*interval)，任何帧都算存活。
func Heartbeat(ctx context.Context, c *Conn, interval time.Duration) {
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.WriteFrame(MsgPing, &Ping{TS: time.Now().UnixMilli()}); err != nil {
				return
			}
		}
	}
}
```

---

### 4.3 Everything SDK 动态枚举（`internal/enum`）

要求目标机运行 Everything ≥ 1.4（plan 2 节）。Windows 实现通过 `golang.org/x/sys/windows` 在运行期按需 `LoadDLL`/`FindProc`，保持官方 `__stdcall` C ABI，但 `agent.exe` 以 `CGO_ENABLED=0` 构建且不需要 MinGW/导入库。`Everything64.dll` 必须与 `agent.exe` 同目录、位于配置路径或 PATH。DLL/IPC 不可用、查询失败或目标根返回空结果时，`ResilientEnumerator` 回退 `WalkerEnumerator` 并告警；详见 ADR-0001。

#### 4.3.1 枚举器抽象（`internal/enum/enumerator.go`）

```go
package enum

import (
	"path/filepath"
	"strings"
)

// FileRecord 是枚举产出的一条文件记录。
type FileRecord struct {
	Path  string // 全路径（不含 \\?\ 前缀）
	Size  int64  // 字节；-1 表示未知
	MTime int64  // 修改时间，Unix 秒
}

// Enumerator 枚举 roots 下的全部文件。visit 在枚举协程内同步调用，勿长时间阻塞。
type Enumerator interface {
	Name() string
	// Available 探测该枚举器当前可用；返回 nil 才允许调用 Enum。
	Available() error
	// Enum 枚举 root（本机绝对路径）下全部文件；visit 返回非 nil 则中止并透传。
	Enum(root string, visit func(FileRecord) error) error
}

// longPath 把超长路径转为 \\?\ 前缀形式（Windows API 260 字符限制）。
func longPath(p string) string {
	if len(p) < 248 || strings.HasPrefix(p, `\\?\`) {
		return p
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`)
	}
	return `\\?\` + abs
}
```

#### 4.3.2 ABI 动态绑定（`internal/enum/everything_windows.go`）

实现按候选路径加载 `Everything64.dll`，并逐一解析以下官方导出：`Everything_SetSearchW`、`Everything_SetMatchPath`、`Everything_SetRequestFlags`、`Everything_QueryW`、`Everything_GetNumResults`、`Everything_GetResultFullPathNameW`、`Everything_GetResultSize`、`Everything_GetResultDateModified`、`Everything_IsFolderResult`、`Everything_GetLastError`。任一必要导出缺失都视为不可用并触发回退，不在构建期链接 DLL。

`LARGE_INTEGER` 与 `FILETIME` 直接按 Windows ABI 向 `Proc.Call` 传入指针；FILETIME 用 `windows.Filetime.Nanoseconds()` 转 Unix 秒。动态加载方案、构建参数和回退边界由 [ADR-0001](../adr/0001-load-everything-sdk-with-pure-go-windows-calls.md) 固化。

#### 4.3.3 查询与回退语义

1. 先用 `GetLongPathNameW` 将存在的 8.3 短路径根规范化为长路径；
2. `Everything_SetMatchPath(TRUE)` 后以带引号的完整根路径查询，避免空格被拆词；
3. 只接收文件结果并返回完整路径、大小、修改时间；
4. Everything 报错或返回零条时用 Walker 重新枚举；已经成功回调的路径去重；
5. `visit` 自身返回的错误属于下游失败，直接透传，不能再回退后重复回调。

> 注意：`Everything_QueryW(TRUE)` 会把全部结果一次性拷入客户端内存。百万级文件（均长 200 字符路径）峰值约几百 MB，M1 接受（plan 目标单机百万级）；若实测过高，M6 再用 `Everything_SetOffset/SetMax` 视窗分页，协议与接口不变（见 7 节风险 R-7）。

#### 4.3.4 回退遍历（`internal/enum/walker.go`）

```go
package enum

import (
	"io/fs"
	"path/filepath"
)

// WalkerEnumerator 是 Everything 不可用时的回退实现（plan 11 节）。
type WalkerEnumerator struct{}

func (WalkerEnumerator) Name() string  { return "walker" }
func (WalkerEnumerator) Available() error { return nil }

func (WalkerEnumerator) Enum(root string, visit func(FileRecord) error) error {
	canonicalRoot, err := canonicalExistingPath(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(longPath(canonicalRoot), func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr // 不吞目录访问错误，避免把不完整枚举误报为成功
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // 跳过 reparse point / 设备文件等
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return visit(FileRecord{Path: cleanPath(p), Size: info.Size(), MTime: info.ModTime().Unix()})
	})
}
```

### 4.4 物理盘号映射（`internal/diskmap/diskmap_windows.go`）

链路：任意路径 →（`GetVolumePathNameW`）→ 卷挂载点 →（`GetVolumeNameForVolumeMountPointW`）→ Volume GUID → 打开卷句柄 →（`IOCTL_STORAGE_GET_DEVICE_NUMBER`）→ `DeviceNumber`（即 `PhysicalDrive<N>` 的 N，落库为 `disk_no`）；HDD/SSD 用（`IOCTL_STORAGE_QUERY_PROPERTY` + `StorageDeviceSeekPenaltyProperty`）寻道惩罚属性判定（plan 2、4.3 节）。纯 Go 经 `golang.org/x/sys/windows`，无 cgo。

```go
//go:build windows

package diskmap

import (
	"encoding/binary"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 常量（wdm.h / winioctl.h）。
const (
	ioctlStorageGetDeviceNumber      = 0x002D1080 // CTL_CODE(IOCTL_STORAGE_BASE, 0x420, BUFFERED, ANY)
	ioctlStorageQueryProperty        = 0x002D1400 // CTL_CODE(IOCTL_STORAGE_BASE, 0x500, BUFFERED, ANY)
	storageDeviceSeekPenaltyProperty = 7          // StorageDeviceSeekPenaltyProperty
	propertyStandardQuery            = 0          // PropertyStandardQuery
)

// Info 是一块物理盘的信息。
type Info struct {
	MountPoint      string // 卷挂载点，如 `D:\`
	VolumeGUID      string // `\\?\Volume{GUID}\`
	DeviceType      uint32 // STORAGE_DEVICE_NUMBER.DeviceType
	DeviceNumber    uint32 // STORAGE_DEVICE_NUMBER.DeviceNumber，落库 disk_no
	PartitionNumber uint32
	IsSSD           bool // 寻道惩罚=false → SSD；查询失败保守置 false(=HDD 处理)
}

var (
	kernel32                               = windows.NewLazySystemDLL("kernel32.dll")
	procGetVolumePathNameW                 = kernel32.NewProc("GetVolumePathNameW")
	procGetVolumeNameForVolumeMountPointW  = kernel32.NewProc("GetVolumeNameForVolumeMountPointW")
)

// MountPointOf 返回 path 所在卷的挂载点（如 `D:\`；挂载到目录的卷返回该目录）。
func MountPointOf(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, 1024)
	r1, _, e1 := procGetVolumePathNameW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	if r1 == 0 {
		return "", fmt.Errorf("diskmap: GetVolumePathNameW(%s): %w", path, e1)
	}
	return windows.UTF16ToString(buf), nil
}

// Resolve 把卷挂载点解析到物理盘信息。
func Resolve(mountPoint string) (*Info, error) {
	if !strings.HasSuffix(mountPoint, `\`) {
		mountPoint += `\`
	}
	mp, err := windows.UTF16PtrFromString(mountPoint)
	if err != nil {
		return nil, err
	}
	gbuf := make([]uint16, 128)
	r1, _, e1 := procGetVolumeNameForVolumeMountPointW.Call(
		uintptr(unsafe.Pointer(mp)),
		uintptr(unsafe.Pointer(&gbuf[0])),
		uintptr(len(gbuf)),
	)
	if r1 == 0 {
		return nil, fmt.Errorf("diskmap: GetVolumeNameForVolumeMountPointW(%s): %w", mountPoint, e1)
	}
	guid := windows.UTF16ToString(gbuf) // `\\?\Volume{GUID}\`

	// 打开卷句柄（去尾反斜杠；0 访问权限即可做这两次 IOCTL，均为 FILE_ANY_ACCESS）。
	op, err := windows.UTF16PtrFromString(strings.TrimSuffix(guid, `\`))
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(op, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("diskmap: open volume %s: %w", guid, err)
	}
	defer windows.CloseHandle(h)

	// STORAGE_DEVICE_NUMBER{DeviceType,DeviceNumber,PartitionNumber}，3×uint32。
	var sdn [12]byte
	var ret uint32
	if err := windows.DeviceIoControl(h, ioctlStorageGetDeviceNumber, nil, 0,
		&sdn[0], uint32(len(sdn)), &ret, nil); err != nil {
		return nil, fmt.Errorf("diskmap: IOCTL_STORAGE_GET_DEVICE_NUMBER: %w", err)
	}
	info := &Info{
		MountPoint:      mountPoint,
		VolumeGUID:      guid,
		DeviceType:      binary.LittleEndian.Uint32(sdn[0:4]),
		DeviceNumber:    binary.LittleEndian.Uint32(sdn[4:8]),
		PartitionNumber: binary.LittleEndian.Uint32(sdn[8:12]),
	}

	// STORAGE_PROPERTY_QUERY{PropertyId,QueryType,AdditionalParameters[1]} → 12 字节。
	var qry [12]byte
	binary.LittleEndian.PutUint32(qry[0:4], storageDeviceSeekPenaltyProperty)
	binary.LittleEndian.PutUint32(qry[4:8], propertyStandardQuery)
	// DEVICE_SEEK_PENALTY_DESCRIPTOR{Version,Size,IncursSeekPenalty} → 12 字节。
	var desc [12]byte
	if err := windows.DeviceIoControl(h, ioctlStorageQueryProperty, &qry[0], uint32(len(qry)),
		&desc[0], uint32(len(desc)), &ret, nil); err != nil {
		info.IsSSD = false // 查询失败保守按 HDD（2 条顺序流），见任务 T4-3
		return info, nil
	}
	info.IsSSD = desc[8] == 0 // IncursSeekPenalty == FALSE → SSD
	return info, nil
}
```

### 4.5 SHA-512 计算与文件分类（`internal/agent/hasher.go`、`classify.go`）

M1 在 Agent 主进程内用 Go 标准库计算（任务既定口径）；`Hasher` 接口是 M2 切换到 Worker 子进程 + DLL 的接缝，M2 替换实现不改调度代码。

```go
package agent

import (
	"crypto/sha512"
	"encoding/hex"
	"io"
	"os"
)

// HashBlockSize 读块 4MB，与 HDD 读块默认值对齐（plan 9 节）。
const HashBlockSize = 4 << 20

// Hasher 计算单个文件的 SHA-512。M1 用 GoHasher；M2 换成 Worker 进程实现。
type Hasher interface {
	HashFile(path string) (sha512Hex string, err error)
}

// GoHasher 流式 4MB 块计算 SHA-512，返回 128 字符 hex。
type GoHasher struct{}

func (GoHasher) HashFile(path string) (string, error) {
	f, err := os.Open(longPathPrefix(path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha512.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, HashBlockSize)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// longPathPrefix 与 internal/enum.longPath 同规则，Agent 侧单独保留一份避免包循环。
func longPathPrefix(p string) string {
	if len(p) < 248 || len(p) >= 4 && p[:4] == `\\?\` {
		return p
	}
	return `\\?\` + p
}
```

```go
package agent

import (
	"path/filepath"
	"strings"

	"dedup/internal/proto"
)

// 默认扩展名表（可在 agent.json 的 scan.image_exts / scan.video_exts 覆盖）。
var (
	defaultImageExts = []string{".jpg", ".jpeg", ".png", ".webp", ".bmp", ".gif", ".tif", ".tiff"}
	defaultVideoExts = []string{".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".ts", ".m2ts", ".mpg", ".mpeg", ".webm", ".3gp"}
)

// MediaKind 返回 image / video / other。
func MediaKind(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	for _, e := range defaultImageExts {
		if ext == e {
			return "image"
		}
	}
	for _, e := range defaultVideoExts {
		if ext == e {
			return "video"
		}
	}
	return "other"
}

// MissingBase 返回该文件一阶段应算字段的基础缺失位掩码（M1 只有 FieldSHA512 会被清除）。
func MissingBase(path string) uint32 {
	switch MediaKind(path) {
	case "image":
		return proto.FieldSHA512 | proto.FieldPDQ256
	case "video":
		return proto.FieldSHA512 | proto.FieldThumb
	default:
		return proto.FieldSHA512
	}
}
```

---

### 4.6 SQLite 本地库（`internal/store`）

DDL 严格按 plan 6.1，只补足列类型、约束与索引。`mtime`/`updated_at` 均为 Unix 秒；`sha512` 存 128 字符 hex；`pdq256`/`thumb_pdq256` 为 32 字节 BLOB（M2 起写入）。`sync_queue.row_pk`：files 表用 `CAST(id AS TEXT)`，特征表用 `sha512`。

#### 4.6.1 DDL（`internal/store/ddl.go`）

```go
package store

// ddl 对应 plan 6.1 全部五张表；通过 `PRAGMA user_version` 做版本化迁移。
const ddl = `
CREATE TABLE IF NOT EXISTS files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    machine_id   TEXT    NOT NULL,
    disk_no      INTEGER NOT NULL DEFAULT -1,
    path         TEXT    NOT NULL,
    size         INTEGER NOT NULL DEFAULT -1,
    mtime        INTEGER NOT NULL DEFAULT 0,
    sha512       TEXT,
    phase1_done  INTEGER NOT NULL DEFAULT 0,  -- sha/pdq/thumb 是否齐
    phase2_done  INTEGER NOT NULL DEFAULT 0,  -- phash/sobel/6帧 是否齐
    -- status: 'deleted' 由 M5 删除组件写入；M3/M4 分析侧与 GUI 查询须统一排除该状态
    status       TEXT    NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','done','partial','failed','crash','deleted')),
    missing_mask INTEGER NOT NULL DEFAULT 0,
    error        TEXT,
    updated_at   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (machine_id, path)
);
CREATE INDEX IF NOT EXISTS idx_files_sha512      ON files (sha512);
CREATE INDEX IF NOT EXISTS idx_files_status      ON files (status);
CREATE INDEX IF NOT EXISTS idx_files_disk_status ON files (disk_no, status);

CREATE TABLE IF NOT EXISTS image_features (
    sha512       TEXT PRIMARY KEY,
    width        INTEGER NOT NULL DEFAULT 0,
    height       INTEGER NOT NULL DEFAULT 0,
    pdq256       BLOB,                      -- 32 字节，一阶段
    pdq_quality  INTEGER NOT NULL DEFAULT 0,
    phash_parts  BLOB,                      -- 二阶段（可空）
    sobel_hist   BLOB                       -- 二阶段（可空）
);

CREATE TABLE IF NOT EXISTS video_features (
    sha512        TEXT PRIMARY KEY,
    duration_ms   INTEGER NOT NULL DEFAULT 0,
    thumb_path    TEXT,
    thumb_pdq256  BLOB,                     -- 32 字节，一阶段
    thumb_quality INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS video_frames (
    sha512      TEXT    NOT NULL,
    frame_idx   INTEGER NOT NULL,
    pdq256      BLOB,
    phash_parts BLOB,                       -- 二阶段（可空）
    sobel_hist  BLOB,                       -- 二阶段（可空）
    PRIMARY KEY (sha512, frame_idx)
);

CREATE TABLE IF NOT EXISTS sync_queue (
    table_name  TEXT    NOT NULL,
    row_pk      TEXT    NOT NULL,
    synced      INTEGER NOT NULL DEFAULT 0,
    enqueued_at INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (table_name, row_pk)
);
CREATE INDEX IF NOT EXISTS idx_sync_queue_pending ON sync_queue (synced);

PRAGMA user_version = 1;
`
```

#### 4.6.2 打开库与核心读写（`internal/store/db.go`、`files.go`、`syncq.go`）

```go
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// DB 包装本地 SQLite。单连接 + WAL + busy_timeout：写事务由调用方批量合并，避免 SQLITE_BUSY。
type DB struct {
	db *sql.DB
}

// Open 打开（不存在则创建）本地库并幂等执行 DDL。
func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"+
		"&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(ddl); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &DB{db: db}, nil
}

func (d *DB) Close() error { return d.db.Close() }
```

```go
package store

import (
	"context"
	"fmt"
	"time"
)

// FileRow 对应 files 表一行。
type FileRow struct {
	ID          int64
	MachineID   string
	DiskNo      int64
	Path        string
	Size        int64
	MTime       int64
	SHA512      *string // NULL = 未算
	Phase1Done  bool
	Phase2Done  bool
	Status      string
	MissingMask uint32
	Error       *string
	UpdatedAt   int64
}

// EnumUpsert 是枚举落库的一行输入；MissingBase 由分类器给出。
type EnumUpsert struct {
	MachineID   string
	DiskNo      int64
	Path        string
	Size        int64
	MTime       int64
	MissingBase uint32
}

// 枚举落库：新文件按 MissingBase 置缺失位；已存在文件 size+mtime 未变且 sha512 已有
// → 保持原状（剪枝，plan 4.4-1）；否则重置 pending 并补置 SHA-512 缺失位。
const upsertEnumeratedSQL = `
INSERT INTO files (machine_id, disk_no, path, size, mtime, status, missing_mask, updated_at)
VALUES (?1, ?2, ?3, ?4, ?5, 'pending', ?6, ?7)
ON CONFLICT (machine_id, path) DO UPDATE SET
    disk_no = excluded.disk_no,
    missing_mask = CASE
        WHEN files.size = excluded.size AND files.mtime = excluded.mtime
             AND files.sha512 IS NOT NULL
        THEN files.missing_mask
        ELSE files.missing_mask | excluded.missing_mask END,
    status = CASE
        WHEN files.size = excluded.size AND files.mtime = excluded.mtime
             AND files.sha512 IS NOT NULL
        THEN files.status
        ELSE 'pending' END,
    size       = excluded.size,
    mtime      = excluded.mtime,
    updated_at = excluded.updated_at;`

// UpsertEnumerated 批量落库（建议每次 ≤10000 行，一个事务）。
func (d *DB) UpsertEnumerated(ctx context.Context, recs []EnumUpsert) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, upsertEnumeratedSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().Unix()
	for _, r := range recs {
		if _, err := stmt.ExecContext(ctx, r.MachineID, r.DiskNo, r.Path, r.Size,
			r.MTime, r.MissingBase, now); err != nil {
			return fmt.Errorf("store: upsert %s: %w", r.Path, err)
		}
	}
	return tx.Commit()
}

// PendingSnapshot 取本轮待算文件：SHA-512 缺失位仍在、状态允许重试，
// 按 disk_no 分桶、桶内按路径排序（目录序，plan 4.3）。
func (d *DB) PendingSnapshot(ctx context.Context, machineID string) (map[int64][]string, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT disk_no, path FROM files
		WHERE machine_id = ?1
		  AND status IN ('pending','failed','crash')
		  AND (missing_mask & 1) != 0
		ORDER BY disk_no, path;`, machineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var disk int64
		var p string
		if err := rows.Scan(&disk, &p); err != nil {
			return nil, err
		}
		out[disk] = append(out[disk], p)
	}
	return out, rows.Err()
}

// HashResult 是单文件哈希结果；Err 非空表示失败。
type HashResult struct {
	Path   string
	SHA512 string // 成功时 128 字符 hex
	Size   int64
	MTime  int64
	Err    string
}

// 成功：清 SHA-512 位；phase1_done = 一阶段三位（bit0~2）全清时置 1（M1 媒体文件不会置 1，预期）。
const markHashOKSQL = `
UPDATE files SET sha512 = ?3, status = 'done', error = NULL,
    missing_mask = missing_mask & ~1,
    phase1_done  = CASE WHEN ((missing_mask & ~1) & 7) = 0 THEN 1 ELSE 0 END,
    updated_at   = ?4
WHERE machine_id = ?1 AND path = ?2;`

// 失败：保留缺失位（下轮可补算），记 error，状态 failed。
const markHashFailSQL = `
UPDATE files SET status = 'failed', error = ?3, updated_at = ?4
WHERE machine_id = ?1 AND path = ?2;`

// 同事务把该行挂入上行队列（重复入队自动去重并重置 synced）。
const enqueueFilesSyncSQL = `
INSERT INTO sync_queue (table_name, row_pk, synced, enqueued_at)
SELECT 'files', CAST(id AS TEXT), 0, ?3 FROM files WHERE machine_id = ?1 AND path = ?2
ON CONFLICT (table_name, row_pk) DO UPDATE
    SET synced = 0, enqueued_at = excluded.enqueued_at;`

// ApplyHashResults 批量回写哈希结果（建议 500 行一事务）。
func (d *DB) ApplyHashResults(ctx context.Context, machineID string, results []HashResult) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, r := range results {
		var err error
		if r.Err == "" {
			_, err = tx.ExecContext(ctx, markHashOKSQL, machineID, r.Path, r.SHA512, now)
		} else {
			_, err = tx.ExecContext(ctx, markHashFailSQL, machineID, r.Path, r.Err, now)
		}
		if err != nil {
			return fmt.Errorf("store: apply %s: %w", r.Path, err)
		}
		if _, err := tx.ExecContext(ctx, enqueueFilesSyncSQL, machineID, r.Path, now); err != nil {
			return fmt.Errorf("store: enqueue %s: %w", r.Path, err)
		}
	}
	return tx.Commit()
}

// LoadFilesByIDs 按本地 id 取整行（同步器用）。ids 为空返回空切片。
func (d *DB) LoadFilesByIDs(ctx context.Context, ids []string) ([]FileRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := `SELECT id, machine_id, disk_no, path, size, mtime, sha512, phase1_done,
	             phase2_done, status, missing_mask, error, updated_at
	      FROM files WHERE id IN (`
	for i := range ids {
		if i > 0 {
			q += ","
		}
		q += "?"
	}
	q += ");"
	args := make([]any, len(ids))
	for i, s := range ids {
		args[i] = s
	}
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileRow
	for rows.Next() {
		var r FileRow
		var phase1, phase2 int64
		if err := rows.Scan(&r.ID, &r.MachineID, &r.DiskNo, &r.Path, &r.Size, &r.MTime,
			&r.SHA512, &phase1, &phase2, &r.Status, &r.MissingMask, &r.Error, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Phase1Done = phase1 != 0
		r.Phase2Done = phase2 != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
```

```go
package store

import (
	"context"
	"fmt"
)

// PendingSyncRowPKs 取某表待上行主键（按入队时间，limit 一批）。
func (d *DB) PendingSyncRowPKs(ctx context.Context, table string, limit int) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT row_pk FROM sync_queue
		WHERE synced = 0 AND table_name = ?1
		ORDER BY enqueued_at LIMIT ?2;`, table, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			return nil, err
		}
		out = append(out, pk)
	}
	return out, rows.Err()
}

// PendingSyncCount 返回全部待上行行数（5 万行触发条件用）。
func (d *DB) PendingSyncCount(ctx context.Context) (int64, error) {
	var n int64
	err := d.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sync_queue WHERE synced = 0;`).Scan(&n)
	return n, err
}

// MarkSynced 上行成功后标记（同一事务外也可，失败重发无副作用）。
func (d *DB) MarkSynced(ctx context.Context, table string, pks []string) error {
	if len(pks) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, pk := range pks {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sync_queue SET synced = 1 WHERE table_name = ?1 AND row_pk = ?2;`,
			table, pk); err != nil {
			return fmt.Errorf("store: mark synced %s/%s: %w", table, pk, err)
		}
	}
	return tx.Commit()
}
```

### 4.7 PostgreSQL 中心库与上行同步

#### 4.7.1 中心库 DDL（`deploy/central.sql`，严格按 plan 6.2）

```sql
-- 多机器媒体文件去重系统 · 中心库（PostgreSQL 16）
-- 对应 architecture-plan v1.1 第 6.2 节：同构表 + machine_id 维度 + 结果表。

CREATE TABLE IF NOT EXISTS files (
    id           BIGSERIAL PRIMARY KEY,
    machine_id   TEXT   NOT NULL,
    disk_no      INTEGER NOT NULL DEFAULT -1,
    path         TEXT   NOT NULL,
    size         BIGINT NOT NULL DEFAULT -1,
    mtime        BIGINT NOT NULL DEFAULT 0,
    sha512       TEXT,
    phase1_done  SMALLINT NOT NULL DEFAULT 0,
    phase2_done  SMALLINT NOT NULL DEFAULT 0,
    status       TEXT   NOT NULL DEFAULT 'pending',
    missing_mask INTEGER NOT NULL DEFAULT 0,
    error        TEXT,
    updated_at   BIGINT NOT NULL DEFAULT 0,       -- Agent 侧 Unix 秒
    synced_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (machine_id, path)
);
-- 精确分组与特征主键索引（部分索引，未算出的行不进索引）
CREATE INDEX IF NOT EXISTS idx_files_sha512 ON files (sha512) WHERE sha512 IS NOT NULL;

CREATE TABLE IF NOT EXISTS image_features (
    sha512       TEXT PRIMARY KEY,
    width        INTEGER NOT NULL DEFAULT 0,
    height       INTEGER NOT NULL DEFAULT 0,
    pdq256       BYTEA,
    pdq_quality  INTEGER NOT NULL DEFAULT 0,
    phash_parts  BYTEA,
    sobel_hist   BYTEA,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS video_features (
    sha512        TEXT PRIMARY KEY,
    duration_ms   BIGINT NOT NULL DEFAULT 0,
    thumb_path    TEXT,                        -- 各 Agent 本机路径，仅供参考
    thumb_pdq256  BYTEA,
    thumb_quality INTEGER NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS video_frames (
    sha512      TEXT    NOT NULL,
    frame_idx   INTEGER NOT NULL,
    pdq256      BYTEA,
    phash_parts BYTEA,
    sobel_hist  BYTEA,
    PRIMARY KEY (sha512, frame_idx)
);

-- 结果表（plan 6.2）。M1 只建表；dup_groups/dup_members 从 M3 分析管线开始写入，
-- scan_tasks 由 GUI 在 M1 写入。
CREATE TABLE IF NOT EXISTS dup_groups (
    id                     BIGSERIAL PRIMARY KEY,
    -- kind: exact/image/video 为确认组(M4 复筛后)，image_candidate/video_candidate 为一筛候选(M3 写入)
    kind                   TEXT NOT NULL CHECK (kind IN ('exact','image','video','image_candidate','video_candidate')),
    representative_file_id BIGINT REFERENCES files (id),
    member_count           INTEGER NOT NULL DEFAULT 0,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS dup_members (
    group_id   BIGINT NOT NULL REFERENCES dup_groups (id) ON DELETE CASCADE,
    file_id    BIGINT NOT NULL REFERENCES files (id),
    score_json JSONB,                          -- 各级分数明细
    PRIMARY KEY (group_id, file_id)
);

CREATE TABLE IF NOT EXISTS scan_tasks (
    id         TEXT PRIMARY KEY,               -- GUI 生成的 task_id（UUID）
    machine_id TEXT NOT NULL,
    phase      INTEGER NOT NULL,
    target     JSONB NOT NULL,                 -- {"roots":[...]}
    status     TEXT NOT NULL,                  -- running / done / failed
    stats_json JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

#### 4.7.2 files 表上行 upsert（`internal/syncer` 使用的 SQL）

自然键 `(machine_id, path)` 幂等（plan 6.3）：

```sql
INSERT INTO files (machine_id, disk_no, path, size, mtime, sha512,
                   phase1_done, phase2_done, status, missing_mask, error, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (machine_id, path) DO UPDATE SET
    disk_no      = EXCLUDED.disk_no,
    size         = EXCLUDED.size,
    mtime        = EXCLUDED.mtime,
    sha512       = EXCLUDED.sha512,
    phase1_done  = EXCLUDED.phase1_done,
    phase2_done  = EXCLUDED.phase2_done,
    status       = EXCLUDED.status,
    missing_mask = EXCLUDED.missing_mask,
    error        = EXCLUDED.error,
    updated_at   = EXCLUDED.updated_at,
    synced_at    = now();
```

特征表 upsert（M2 起启用，M1 随代码一并交付但无数据流经；同 sha512 多机上行幂等合流，只覆盖非空字段）：

```sql
INSERT INTO image_features (sha512, width, height, pdq256, pdq_quality, phash_parts, sobel_hist)
VALUES ($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (sha512) DO UPDATE SET
    width       = CASE WHEN EXCLUDED.width  > 0 THEN EXCLUDED.width  ELSE image_features.width  END,
    height      = CASE WHEN EXCLUDED.height > 0 THEN EXCLUDED.height ELSE image_features.height END,
    pdq256      = COALESCE(EXCLUDED.pdq256,      image_features.pdq256),
    pdq_quality = CASE WHEN EXCLUDED.pdq_quality > 0 THEN EXCLUDED.pdq_quality
                       ELSE image_features.pdq_quality END,
    phash_parts = COALESCE(EXCLUDED.phash_parts, image_features.phash_parts),
    sobel_hist  = COALESCE(EXCLUDED.sobel_hist,  image_features.sobel_hist),
    updated_at  = now();

INSERT INTO video_features (sha512, duration_ms, thumb_path, thumb_pdq256, thumb_quality)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (sha512) DO UPDATE SET
    duration_ms   = CASE WHEN EXCLUDED.duration_ms > 0 THEN EXCLUDED.duration_ms
                         ELSE video_features.duration_ms END,
    thumb_path    = COALESCE(EXCLUDED.thumb_path,   video_features.thumb_path),
    thumb_pdq256  = COALESCE(EXCLUDED.thumb_pdq256, video_features.thumb_pdq256),
    thumb_quality = CASE WHEN EXCLUDED.thumb_quality > 0 THEN EXCLUDED.thumb_quality
                         ELSE video_features.thumb_quality END,
    updated_at    = now();

INSERT INTO video_frames (sha512, frame_idx, pdq256, phash_parts, sobel_hist)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT (sha512, frame_idx) DO UPDATE SET
    pdq256      = COALESCE(EXCLUDED.pdq256,      video_frames.pdq256),
    phash_parts = COALESCE(EXCLUDED.phash_parts, video_frames.phash_parts),
    sobel_hist  = COALESCE(EXCLUDED.sobel_hist,  video_frames.sobel_hist);
```

#### 4.7.3 同步器（`internal/syncer/syncer.go`）

触发：每 `interval`（默认 5min）一次；另每 30s 检查积压 ≥ `trigger_rows`（默认 5 万）立即触发（plan 6.3）。失败整批留 `sync_queue` 下轮重发；PG 不可达不影响扫描链路。

```go
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/store"
)

// Config 默认值：Interval=5min、TriggerRows=50000、UpsertBatch=5000（plan 6.3）。
type Config struct {
	Interval    time.Duration
	TriggerRows int64
	UpsertBatch int
}

type Syncer struct {
	local *store.DB
	pg    *pgxpool.Pool
	cfg   Config
	log   *slog.Logger
}

func New(local *store.DB, pg *pgxpool.Pool, cfg Config, log *slog.Logger) *Syncer {
	return &Syncer{local: local, pg: pg, cfg: cfg, log: log}
}

const upsertFilesPG = `INSERT INTO files (machine_id, disk_no, path, size, mtime, sha512,
	phase1_done, phase2_done, status, missing_mask, error, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
ON CONFLICT (machine_id, path) DO UPDATE SET
	disk_no = EXCLUDED.disk_no, size = EXCLUDED.size, mtime = EXCLUDED.mtime,
	sha512 = EXCLUDED.sha512, phase1_done = EXCLUDED.phase1_done,
	phase2_done = EXCLUDED.phase2_done, status = EXCLUDED.status,
	missing_mask = EXCLUDED.missing_mask, error = EXCLUDED.error,
	updated_at = EXCLUDED.updated_at, synced_at = now();`

// Run 阻塞运行直到 ctx 取消。
func (s *Syncer) Run(ctx context.Context) {
	period := time.NewTicker(s.cfg.Interval)
	check := time.NewTicker(30 * time.Second)
	defer period.Stop()
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-period.C:
			s.syncOnce(ctx)
		case <-check.C:
			if n, err := s.local.PendingSyncCount(ctx); err == nil && n >= s.cfg.TriggerRows {
				s.log.Info("sync: backlog trigger", "pending", n)
				s.syncOnce(ctx)
			}
		}
	}
}

// syncOnce 把 files 表待上行数据分批推到中心库；特征表 M2 起按同模式扩展。
func (s *Syncer) syncOnce(ctx context.Context) {
	for {
		pks, err := s.local.PendingSyncRowPKs(ctx, "files", s.cfg.UpsertBatch)
		if err != nil {
			s.log.Error("sync: read queue", "err", err)
			return
		}
		if len(pks) == 0 {
			return
		}
		if err := s.syncFilesBatch(ctx, pks); err != nil {
			s.log.Error("sync: batch failed, retry next round", "err", err, "rows", len(pks))
			return // 留在队列下轮重发（plan 6.3）
		}
	}
}

func (s *Syncer) syncFilesBatch(ctx context.Context, pks []string) error {
	rows, err := s.local.LoadFilesByIDs(ctx, pks)
	if err != nil {
		return err
	}
	tx, err := s.pg.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(upsertFilesPG,
			r.MachineID, r.DiskNo, r.Path, r.Size, r.MTime, r.SHA512,
			r.Phase1Done, r.Phase2Done, r.Status, r.MissingMask, r.Error, r.UpdatedAt)
	}
	br := tx.SendBatch(ctx, batch)
	for range rows {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return fmt.Errorf("sync: upsert files: %w", err)
		}
	}
	if err := br.Close(); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return s.local.MarkSynced(ctx, "files", pks)
}
```

---

### 4.8 Agent 扫描调度与服务端（`internal/agent`）

#### 4.8.1 扫描管理器（`internal/agent/scan.go`）

流程：枚举（Everything/回退）→ 万行批事务落库 → 剪枝取待算快照（按盘分桶、目录序）→ 每盘 HDD 2 / SSD 6 条并发流哈希 → 结果 500 行批事务回写（同事务挂 sync_queue）→ 批量 FeatureResult / 周期 TaskProgress / 结束 TaskDone。任务不随 GUI 断线取消（数据落库与上行不受影响），同 `task_id` 重连重绑定回传通道（断点续传，见 1.3）。

```go
package agent

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"dedup/internal/config"
	"dedup/internal/diskmap"
	"dedup/internal/enum"
	"dedup/internal/proto"
	"dedup/internal/store"
)

// Sender 绑定到一条 GUI 连接的帧发送函数。
type Sender func(msgType uint8, v any) error

// ScanManager 管理本机全部扫描任务。
type ScanManager struct {
	cfg    *config.AgentConfig
	st     *store.DB
	enumr  enum.Enumerator
	hasher Hasher
	log    *slog.Logger
	errLog *slog.Logger // errors.log：一文件一失败一行

	mu     sync.Mutex
	tasks  map[string]*ScanState
	disks  map[int64]bool // disk_no → isSSD（本轮解析缓存；重启即重建，规避盘号重排）
}

// ScanState 是单个任务的运行态。
type ScanState struct {
	Task   proto.ScanTask
	Status string // running / done
	Stats  proto.TaskStats

	mu     sync.Mutex
	sender Sender // nil = GUI 离线，只落库不回传
	seq    uint64

	total    atomic.Int64 // 剪枝后待算总数
	done     atomic.Int64 // 已处理（含失败）
	failed   atomic.Int64
	speedWin *speedWindow
}

func NewScanManager(cfg *config.AgentConfig, st *store.DB, enumr enum.Enumerator,
	h Hasher, log, errLog *slog.Logger) *ScanManager {
	return &ScanManager{
		cfg: cfg, st: st, enumr: enumr, hasher: h,
		log: log, errLog: errLog,
		tasks: map[string]*ScanState{}, disks: map[int64]bool{},
	}
}

// Handle 受理 ScanTask（M1 只接受 phase=1），幂等：同 task_id 运行中 → resumed 并重绑回传。
func (m *ScanManager) Handle(task proto.ScanTask, sender Sender) proto.TaskAck {
	if task.Phase != 1 {
		return proto.TaskAck{TaskID: task.TaskID, Accepted: false, Reason: "rejected:only phase=1 in M1", Total: -1}
	}
	if len(task.Roots) == 0 {
		return proto.TaskAck{TaskID: task.TaskID, Accepted: false, Reason: "rejected:empty roots", Total: -1}
	}
	m.mu.Lock()
	if st, ok := m.tasks[task.TaskID]; ok {
		st.mu.Lock()
		st.sender = sender
		status, stats := st.Status, st.Stats
		st.mu.Unlock()
		m.mu.Unlock()
		if status == "done" {
			return proto.TaskAck{TaskID: task.TaskID, Accepted: true, Reason: "already_done", Total: stats.Total}
		}
		return proto.TaskAck{TaskID: task.TaskID, Accepted: true, Reason: "resumed", Total: st.total.Load()}
	}
	st := &ScanState{Task: task, Status: "running", sender: sender, speedWin: newSpeedWindow(10 * time.Second)}
	m.tasks[task.TaskID] = st
	m.mu.Unlock()
	go m.run(st)
	return proto.TaskAck{TaskID: task.TaskID, Accepted: true, Reason: "accepted", Total: -1}
}

// send 向当前绑定连接回传；发送失败自动解绑（任务继续跑）。
func (st *ScanState) send(msgType uint8, v any) {
	st.mu.Lock()
	s := st.sender
	st.mu.Unlock()
	if s == nil {
		return
	}
	if err := s(msgType, v); err != nil {
		st.mu.Lock()
		st.sender = nil
		st.mu.Unlock()
	}
}

func (m *ScanManager) run(st *ScanState) {
	started := time.Now()
	ctx := context.Background() // 任务生命周期独立于 GUI 连接（断点续传，1.3 节）

	// ---- 1. 枚举 + 落库 ----
	var enumerated int64
	for _, root := range st.Task.Roots {
		diskNo, err := m.resolveDisk(root)
		if err != nil {
			m.reportErr(st, root, "enum", err)
			continue
		}
		buf := make([]store.EnumUpsert, 0, 10000)
		flush := func() {
			if len(buf) == 0 {
				return
			}
			if err := m.st.UpsertEnumerated(ctx, buf); err != nil {
				m.reportErr(st, "", "enum", err)
			}
			buf = buf[:0]
		}
		err = m.enumr.Enum(root, func(rec enum.FileRecord) error {
			if len(st.Task.Options.Extensions) > 0 && !extIn(rec.Path, st.Task.Options.Extensions) {
				return nil
			}
			enumerated++
			base := MissingBase(rec.Path)
			if st.Task.Options.Rescan {
				base |= proto.FieldSHA512 // 强制重算
			}
			buf = append(buf, store.EnumUpsert{
				MachineID: m.cfg.MachineID, DiskNo: diskNo,
				Path: rec.Path, Size: rec.Size, MTime: rec.MTime, MissingBase: base,
			})
			if len(buf) >= cap(buf) {
				flush()
			}
			return nil
		})
		flush()
		if err != nil {
			m.reportErr(st, root, "enum", err)
		}
	}

	// ---- 2. 剪枝快照 ----
	pending, err := m.st.PendingSnapshot(ctx, m.cfg.MachineID)
	if err != nil {
		m.reportErr(st, "", "enum", err)
		m.finish(st, started, enumerated)
		return
	}
	var total int64
	for _, ps := range pending {
		total += int64(len(ps))
	}
	st.total.Store(total)
	st.send(proto.MsgTaskProgress, &proto.TaskProgress{
		TaskID: st.Task.TaskID, Done: 0, Total: total, Speed: 0})

	// ---- 3. 盘级并发哈希 ----
	results := make(chan store.HashResult, 1024)
	var wg sync.WaitGroup
	for diskNo, paths := range pending {
		streams := m.cfg.Scan.HDDStreams
		if m.isSSD(diskNo) {
			streams = m.cfg.Scan.SSDStreams
		}
		wg.Add(1)
		go func(diskNo int64, paths []string, streams int) {
			defer wg.Done()
			m.hashDisk(st, paths, streams, results)
		}(diskNo, paths, streams)
	}
	writerDone := make(chan struct{})
	go m.resultWriter(st, results, writerDone)
	progressDone := make(chan struct{})
	go m.progressLoop(st, progressDone)

	wg.Wait()
	close(results)
	<-writerDone
	close(progressDone)
	m.finish(st, started, enumerated)
}

// resolveDisk 解析 root 所在物理盘，缓存 disk_no → 介质类型。
func (m *ScanManager) resolveDisk(root string) (int64, error) {
	mp, err := diskmap.MountPointOf(root)
	if err != nil {
		return -1, err
	}
	info, err := diskmap.Resolve(mp)
	if err != nil {
		return -1, err
	}
	diskNo := int64(info.DeviceNumber)
	m.mu.Lock()
	m.disks[diskNo] = info.IsSSD
	m.mu.Unlock()
	return diskNo, nil
}

// isSSD 读盘介质缓存（加锁，多任务并发安全）；未知盘按 HDD（false）保守处理。
func (m *ScanManager) isSSD(diskNo int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disks[diskNo]
}

// hashDisk 单盘 streams 条并发流，按传入顺序（目录序）取文件。
func (m *ScanManager) hashDisk(st *ScanState, paths []string, streams int, out chan<- store.HashResult) {
	sem := make(chan struct{}, streams)
	var wg sync.WaitGroup
	for _, p := range paths {
		sem <- struct{}{}
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			defer func() { <-sem }()
			res := store.HashResult{Path: p}
			sha, err := m.hasher.HashFile(p)
			if err != nil {
				res.Err = err.Error()
				st.failed.Add(1)
				m.reportErr(st, p, "hash", err)
			} else {
				res.SHA512 = sha
			}
			st.speedWin.Add(1)
			st.done.Add(1)
			out <- res
		}(p)
	}
	wg.Wait()
}

// resultWriter 500 行一批落库 + 组 FeatureResult 帧（≤512 条或 200ms）。
func (m *ScanManager) resultWriter(st *ScanState, in <-chan store.HashResult, done chan<- struct{}) {
	defer close(done)
	tk := time.NewTicker(200 * time.Millisecond)
	defer tk.Stop()
	buf := make([]store.HashResult, 0, 512)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := m.st.ApplyHashResults(context.Background(), m.cfg.MachineID, buf); err != nil {
			m.log.Error("scan: apply results", "err", err)
		}
		items := make([]proto.FeatureItem, len(buf))
		for i, r := range buf {
			items[i] = proto.FeatureItem{
				Path: r.Path, SHA512: r.SHA512, Size: r.Size, MTime: r.MTime,
				Status: proto.StatusDone, Err: r.Err,
			}
			if r.Err != "" {
				items[i].Status = proto.StatusFailed
			}
		}
		st.mu.Lock()
		st.seq++
		seq := st.seq
		st.mu.Unlock()
		st.send(proto.MsgFeatureResult, &proto.FeatureResult{TaskID: st.Task.TaskID, Seq: seq, Items: items})
		buf = buf[:0]
	}
	for {
		select {
		case r, ok := <-in:
			if !ok {
				flush()
				return
			}
			buf = append(buf, r)
			if len(buf) >= 512 {
				flush()
			}
		case <-tk.C:
			flush()
		}
	}
}

// progressLoop 每秒上报一次进度。
func (m *ScanManager) progressLoop(st *ScanState, done <-chan struct{}) {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-done:
			return
		case <-tk.C:
			st.send(proto.MsgTaskProgress, &proto.TaskProgress{
				TaskID: st.Task.TaskID,
				Done:   st.done.Load(),
				Total:  st.total.Load(),
				Speed:  st.speedWin.Rate(),
			})
		}
	}
}

func (m *ScanManager) finish(st *ScanState, started time.Time, enumerated int64) {
	st.mu.Lock()
	st.Status = "done"
	st.Stats = proto.TaskStats{
		Total:     enumerated,
		Done:      st.done.Load(),
		Skipped:   enumerated - st.done.Load(),
		Failed:    st.failed.Load(),
		ElapsedMS: time.Since(started).Milliseconds(),
	}
	stats := st.Stats
	st.mu.Unlock()
	st.send(proto.MsgTaskDone, &proto.TaskDone{TaskID: st.Task.TaskID, Stats: stats})
	m.log.Info("scan done", "task_id", st.Task.TaskID, "stats", stats)
	// 完成的任务保留 10 分钟，供重连后的 GUI 查询/幂等 Ack。
	time.AfterFunc(10*time.Minute, func() {
		m.mu.Lock()
		delete(m.tasks, st.Task.TaskID)
		m.mu.Unlock()
	})
}

// reportErr 一行一报错（errors.log）+ 上报 GUI（plan 8 节）。
func (m *ScanManager) reportErr(st *ScanState, path, stage string, err error) {
	m.errLog.Error("file error", "path", path, "stage", stage, "err", err.Error())
	st.send(proto.MsgError, &proto.Error{
		TaskID: st.Task.TaskID, Path: path, Stage: stage, Msg: err.Error()})
}

func extIn(path string, exts []string) bool {
	ext := ""
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			ext = path[i:]
			break
		}
		if path[i] == '\\' || path[i] == '/' {
			break
		}
	}
	for _, e := range exts {
		if len(ext) == len(e) && equalFoldASCII(ext, e) {
			return true
		}
	}
	return false
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// speedWindow 滑动窗口速率统计（文件/秒）。
type speedWindow struct {
	mu   sync.Mutex
	win  time.Duration
	evts []time.Time
}

func newSpeedWindow(win time.Duration) *speedWindow { return &speedWindow{win: win} }

func (s *speedWindow) Add(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for i := int64(0); i < n; i++ {
		s.evts = append(s.evts, now)
	}
	cut := now.Add(-s.win)
	k := 0
	for _, t := range s.evts {
		if t.After(cut) {
			s.evts[k] = t
			k++
		}
	}
	s.evts = s.evts[:k]
}

func (s *speedWindow) Rate() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return float64(len(s.evts)) / s.win.Seconds()
}
```

#### 4.8.2 TCP 服务端（`internal/agent/server.go`）

```go
package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"dedup/internal/config"
	"dedup/internal/proto"
)

// Server 是 Agent 的 TCP 服务端：每连接一个读循环，建立即发 Hello。
type Server struct {
	cfg *config.AgentConfig
	sm  *ScanManager
	log *slog.Logger
}

func NewServer(cfg *config.AgentConfig, sm *ScanManager, log *slog.Logger) *Server {
	return &Server{cfg: cfg, sm: sm, log: log}
}

// heartbeat 与读超时：15s 心跳，45s 无帧判死（plan 7、9 节）。
func (s *Server) heartbeat() time.Duration { return time.Duration(s.cfg.Proto.HeartbeatS) * time.Second }

func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return err
	}
	s.log.Info("agent listening", "addr", s.cfg.ListenAddr)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.log.Error("accept", "err", err)
			continue
		}
		go s.handleConn(ctx, nc)
	}
}

func (s *Server) handleConn(ctx context.Context, nc net.Conn) {
	c := proto.NewConn(nc)
	defer c.Close()
	remote := nc.RemoteAddr().String()
	host, _ := os.Hostname()
	if err := c.WriteFrame(proto.MsgHello, &proto.Hello{
		Version:   proto.ProtocolVersion,
		MachineID: s.cfg.MachineID,
		Hostname:  host,
		PID:       os.Getpid(),
	}); err != nil {
		return
	}
	s.log.Info("gui connected", "remote", remote)
	defer s.log.Info("gui disconnected", "remote", remote)

	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	go proto.Heartbeat(hbCtx, c, s.heartbeat())

	sender := func(msgType uint8, v any) error { return c.WriteFrame(msgType, v) }
	for {
		c.SetReadDeadline(time.Now().Add(3 * s.heartbeat()))
		msgType, body, err := c.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				s.log.Warn("conn closed", "remote", remote, "err", err)
			}
			return
		}
		msg, err := proto.Decode(msgType, body)
		if err != nil {
			sender(proto.MsgError, &proto.Error{Stage: "proto", Msg: err.Error()})
			continue
		}
		switch m := msg.(type) {
		case *proto.Ping:
			sender(proto.MsgPong, &proto.Pong{TS: m.TS})
		case *proto.ScanTask:
			ack := s.sm.Handle(*m, sender)
			sender(proto.MsgTaskAck, &ack)
		default:
			// M1 未实现的消息类型（Phase2Task/DeleteTask/ConfigPush 等）：不崩不断连。
			sender(proto.MsgError, &proto.Error{Stage: "proto", Msg: "unsupported in M1"})
		}
	}
}
```

### 4.9 GUI 独立进程（`internal/gui`）

#### 4.9.1 Agent 连接池（`internal/gui/pool.go`）

```go
package gui

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"dedup/internal/config"
	"dedup/internal/proto"
)

// AgentStatus 是连接池对外的状态视图。
type AgentStatus struct {
	MachineID string `json:"machine_id"`
	Addr      string `json:"addr"`
	Online    bool   `json:"online"`
	LastErr   string `json:"last_err,omitempty"`
}

// AgentConn 维护到一台 Agent 的长连接（断线指数退避重连：1s×2 封顶 30s）。
type AgentConn struct {
	ep  config.AgentEndpoint
	log *slog.Logger
	on  func(machineID string, conn *AgentConn, msg any) // 消息分发回调

	mu      sync.Mutex
	conn    *proto.Conn
	online  bool
	lastErr string
}

func newAgentConn(ep config.AgentEndpoint, log *slog.Logger,
	on func(string, *AgentConn, any)) *AgentConn {
	return &AgentConn{ep: ep, log: log, on: on}
}

// Run 阻塞运行拨号循环直到 ctx 取消。
func (a *AgentConn) Run(ctx context.Context, heartbeat time.Duration) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := a.runOnce(ctx, heartbeat)
		a.setOffline(err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (a *AgentConn) runOnce(ctx context.Context, heartbeat time.Duration) error {
	nc, err := net.DialTimeout("tcp", a.ep.Addr, 10*time.Second)
	if err != nil {
		return err
	}
	c := proto.NewConn(nc)
	defer c.Close()

	// 首帧必须是 Hello，并校验 machine_id 与协议版本。
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	msgType, body, err := c.ReadFrame()
	if err != nil {
		return err
	}
	msg, err := proto.Decode(msgType, body)
	if err != nil {
		return err
	}
	hello, ok := msg.(*proto.Hello)
	if !ok {
		return fmt.Errorf("expect Hello, got type=%d", msgType)
	}
	if hello.Version != proto.ProtocolVersion {
		return fmt.Errorf("protocol version mismatch: agent=%d gui=%d", hello.Version, proto.ProtocolVersion)
	}
	if hello.MachineID != a.ep.MachineID {
		return fmt.Errorf("machine_id mismatch: config=%s agent=%s", a.ep.MachineID, hello.MachineID)
	}

	a.setOnline(c)
	a.log.Info("agent connected", "machine_id", a.ep.MachineID, "addr", a.ep.Addr)
	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	go proto.Heartbeat(hbCtx, c, heartbeat)

	for {
		c.SetReadDeadline(time.Now().Add(3 * heartbeat))
		msgType, body, err := c.ReadFrame()
		if err != nil {
			return err
		}
		msg, err := proto.Decode(msgType, body)
		if err != nil {
			continue
		}
		if p, ok := msg.(*proto.Ping); ok {
			a.Send(proto.MsgPong, &proto.Pong{TS: p.TS})
			continue
		}
		a.on(a.ep.MachineID, a, msg)
	}
}

// Send 经当前连接发帧；离线返回错误。
func (a *AgentConn) Send(msgType uint8, v any) error {
	a.mu.Lock()
	c := a.conn
	a.mu.Unlock()
	if c == nil {
		return fmt.Errorf("agent %s offline", a.ep.MachineID)
	}
	return c.WriteFrame(msgType, v)
}

func (a *AgentConn) setOnline(c *proto.Conn) {
	a.mu.Lock()
	a.conn, a.online, a.lastErr = c, true, ""
	a.mu.Unlock()
}

func (a *AgentConn) setOffline(err error) {
	a.mu.Lock()
	a.conn, a.online = nil, false
	if err != nil {
		a.lastErr = err.Error()
	}
	a.mu.Unlock()
}

// Pool 是全部 Agent 连接的注册表。
type Pool struct {
	conns map[string]*AgentConn // machine_id → conn
}

func NewPool(eps []config.AgentEndpoint, log *slog.Logger,
	on func(string, *AgentConn, any)) *Pool {
	p := &Pool{conns: map[string]*AgentConn{}}
	for _, ep := range eps {
		p.conns[ep.MachineID] = newAgentConn(ep, log, on)
	}
	return p
}

func (p *Pool) Start(ctx context.Context, heartbeat time.Duration) {
	for _, c := range p.conns {
		go c.Run(ctx, heartbeat)
	}
}

func (p *Pool) Send(machineID string, msgType uint8, v any) error {
	c, ok := p.conns[machineID]
	if !ok {
		return fmt.Errorf("unknown agent %q", machineID)
	}
	return c.Send(msgType, v)
}

func (p *Pool) Status() []AgentStatus {
	out := make([]AgentStatus, 0, len(p.conns))
	for _, c := range p.conns {
		c.mu.Lock()
		out = append(out, AgentStatus{
			MachineID: c.ep.MachineID, Addr: c.ep.Addr, Online: c.online, LastErr: c.lastErr,
		})
		c.mu.Unlock()
	}
	return out
}
```

#### 4.9.2 任务注册表（`internal/gui/tasks.go`）

```go
package gui

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/proto"
)

// TaskInfo 是 GUI 侧一个任务的实时状态（内存态，Web 轮询展示用）。
type TaskInfo struct {
	TaskID    string              `json:"task_id"`
	MachineID string              `json:"machine_id"`
	Phase     int                 `json:"phase"`
	Roots     []string            `json:"roots"`
	Status    string              `json:"status"` // sent / acked / running / done / failed
	AckReason string              `json:"ack_reason,omitempty"`
	Done      int64               `json:"done"`
	Total     int64               `json:"total"`
	Speed     float64             `json:"speed"`
	LastErr   string              `json:"last_err,omitempty"`
	Recent    []proto.FeatureItem `json:"recent"` // 最近 50 条结果
	UpdatedAt time.Time           `json:"updated_at"`
}

// TaskRegistry 内存任务表；TaskDone 时落中心库 scan_tasks。
type TaskRegistry struct {
	mu   sync.Mutex
	byID map[string]*TaskInfo
	pg   *pgxpool.Pool
	log  *slog.Logger
}

func NewTaskRegistry(pg *pgxpool.Pool, log *slog.Logger) *TaskRegistry {
	return &TaskRegistry{byID: map[string]*TaskInfo{}, pg: pg, log: log}
}

func (r *TaskRegistry) Register(t *TaskInfo) {
	r.mu.Lock()
	r.byID[t.TaskID] = t
	r.mu.Unlock()
	r.upsertScanTask(t, nil)
}

// Dispatch 处理 Agent 回传消息（pool 回调）。
func (r *TaskRegistry) Dispatch(machineID string, msg any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch m := msg.(type) {
	case *proto.TaskAck:
		if t, ok := r.byID[m.TaskID]; ok {
			t.Status = "acked"
			t.AckReason = m.Reason
			t.Total = m.Total
			t.UpdatedAt = time.Now()
			if !m.Accepted {
				t.Status = "failed"
				t.LastErr = m.Reason
			}
		}
	case *proto.TaskProgress:
		if t, ok := r.byID[m.TaskID]; ok {
			t.Status = "running"
			t.Done, t.Total, t.Speed = m.Done, m.Total, m.Speed
			t.UpdatedAt = time.Now()
		}
	case *proto.FeatureResult:
		if t, ok := r.byID[m.TaskID]; ok {
			t.Recent = append(t.Recent, m.Items...)
			if len(t.Recent) > 50 {
				t.Recent = t.Recent[len(t.Recent)-50:]
			}
			t.UpdatedAt = time.Now()
		}
	case *proto.TaskDone:
		if t, ok := r.byID[m.TaskID]; ok {
			t.Status = "done"
			t.Done = m.Stats.Done
			t.UpdatedAt = time.Now()
			r.upsertScanTask(t, &m.Stats)
		}
	case *proto.Error:
		if m.TaskID != "" {
			if t, ok := r.byID[m.TaskID]; ok {
				t.LastErr = m.Msg
				t.UpdatedAt = time.Now()
			}
		}
		r.log.Warn("agent error", "machine", machineID, "task", m.TaskID,
			"stage", m.Stage, "path", m.Path, "msg", m.Msg)
	}
}

// List 返回全部任务（按更新时间倒序）。
func (r *TaskRegistry) List() []*TaskInfo {
	r.mu.Lock()
	out := make([]*TaskInfo, 0, len(r.byID))
	for _, t := range r.byID {
		out = append(out, t)
	}
	r.mu.Unlock()
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].UpdatedAt.After(out[i].UpdatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// upsertScanTask 把任务状态写入中心库 scan_tasks（plan 6.2）。失败仅记日志。
func (r *TaskRegistry) upsertScanTask(t *TaskInfo, stats *proto.TaskStats) {
	if r.pg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var statsJSON any
	if stats != nil {
		statsJSON = map[string]int64{
			"total": stats.Total, "done": stats.Done, "skipped": stats.Skipped,
			"failed": stats.Failed, "elapsed_ms": stats.ElapsedMS,
		}
	}
	_, err := r.pg.Exec(ctx, `
		INSERT INTO scan_tasks (id, machine_id, phase, target, status, stats_json)
		VALUES ($1,$2,$3, jsonb_build_object('roots', to_jsonb($4::text[])), $5, $6)
		ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status,
			stats_json = COALESCE(EXCLUDED.stats_json, scan_tasks.stats_json),
			updated_at = now();`,
		t.TaskID, t.MachineID, t.Phase, t.Roots, t.Status, statsJSON)
	if err != nil {
		r.log.Error("upsert scan_tasks", "err", err)
	}
}
```

#### 4.9.3 HTTP API（`internal/gui/httpapi.go`）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/agents` | 全部 Agent 连接状态 |
| POST | `/api/scan` | 下发 ScanTask；body `{"machine_id":"A","roots":["D:\\media"],"phase":1,"rescan":false}` → `{"task_id":"..."}` |
| GET | `/api/tasks` | 全部任务实时状态 |
| GET | `/api/dup_groups?limit=100&offset=0` | 精确重复组（中心库实时分组） |
| GET | `/api/dup_groups/{sha512}` | 某组的全部成员（跨机器路径列表） |
| GET | `/` | 内嵌 Web 页面 |

```go
package gui

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/proto"
)

// API 是 GUI 的 HTTP 层。
type API struct {
	pool  *Pool
	tasks *TaskRegistry
	pg    *pgxpool.Pool
}

func NewAPI(pool *Pool, tasks *TaskRegistry, pg *pgxpool.Pool) *API {
	return &API{pool: pool, tasks: tasks, pg: pg}
}

// Routes 注册全部路由（含内嵌静态页）。
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/agents", a.handleAgents)
	mux.HandleFunc("POST /api/scan", a.handleScan)
	mux.HandleFunc("GET /api/tasks", a.handleTasks)
	mux.HandleFunc("GET /api/dup_groups", a.handleDupGroups)
	mux.HandleFunc("GET /api/dup_groups/{sha512}", a.handleDupMembers)
	mux.Handle("GET /", http.FileServerFS(webFS()))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *API) handleAgents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.pool.Status())
}

type scanRequest struct {
	MachineID string   `json:"machine_id"`
	Roots     []string `json:"roots"`
	Phase     uint8    `json:"phase"`
	Rescan    bool     `json:"rescan"`
}

func (a *API) handleScan(w http.ResponseWriter, r *http.Request) {
	var req scanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.MachineID == "" || len(req.Roots) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "machine_id and roots required"})
		return
	}
	if req.Phase == 0 {
		req.Phase = 1
	}
	taskID := uuid.NewString()
	task := &proto.ScanTask{
		TaskID:  taskID,
		Roots:   req.Roots,
		Phase:   req.Phase,
		Options: proto.ScanOptions{Rescan: req.Rescan},
	}
	if err := a.pool.Send(req.MachineID, proto.MsgScanTask, task); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	a.tasks.Register(&TaskInfo{
		TaskID: taskID, MachineID: req.MachineID, Phase: int(req.Phase),
		Roots: req.Roots, Status: "sent", UpdatedAt: time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]string{"task_id": taskID})
}

func (a *API) handleTasks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.tasks.List())
}

// DupGroup 精确重复组视图（按组字节数降序，大头排前）。
type DupGroup struct {
	SHA512      string `json:"sha512"`
	MemberCount int64  `json:"member_count"`
	TotalBytes  int64  `json:"total_bytes"` // 含全部副本
	WastedBytes int64  `json:"wasted_bytes"` // (n-1) × size
	Machines    int64  `json:"machines"`
}

func (a *API) handleDupGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	rows, err := a.pg.Query(r.Context(), `
		SELECT sha512, count(*) AS members, sum(size) AS total_bytes,
		       (count(*)-1) * max(size) AS wasted_bytes,
		       count(DISTINCT machine_id) AS machines
		FROM files
		WHERE sha512 IS NOT NULL
		GROUP BY sha512
		HAVING count(*) > 1
		ORDER BY wasted_bytes DESC
		LIMIT $1 OFFSET $2;`, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []DupGroup{}
	for rows.Next() {
		var g DupGroup
		if err := rows.Scan(&g.SHA512, &g.MemberCount, &g.TotalBytes, &g.WastedBytes, &g.Machines); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, g)
	}
	writeJSON(w, http.StatusOK, out)
}

// DupMember 组内成员（跨机器路径）。
type DupMember struct {
	MachineID string `json:"machine_id"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	MTime     int64  `json:"mtime"`
}

func (a *API) handleDupMembers(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha512")
	rows, err := a.pg.Query(r.Context(), `
		SELECT machine_id, path, size, mtime FROM files
		WHERE sha512 = $1
		ORDER BY machine_id, path;`, sha)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	out := []DupMember{}
	for rows.Next() {
		var m DupMember
		if err := rows.Scan(&m.MachineID, &m.Path, &m.Size, &m.MTime); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, out)
}
```

#### 4.9.4 内嵌 Web 页面（`internal/gui/web.go` + `web/index.html`）

```go
package gui

import (
	"embed"
	"io/fs"
)

//go:embed web
var webContent embed.FS

// webFS 返回静态页文件系统（根即 index.html 所在目录）。
func webFS() fs.FS {
	sub, err := fs.Sub(webContent, "web")
	if err != nil {
		panic(err)
	}
	return sub
}
```

`web/index.html`（单页、无外部依赖；2s 轮询）：

```html
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>媒体去重 · M1</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 24px; color: #222; }
  h2 { margin-top: 28px; font-size: 17px; }
  table { border-collapse: collapse; width: 100%; font-size: 13px; }
  th, td { border: 1px solid #ddd; padding: 5px 8px; text-align: left; }
  th { background: #f5f5f5; }
  .on { color: #0a0; font-weight: 600; } .off { color: #c00; font-weight: 600; }
  input, select, button, textarea { font-size: 13px; padding: 4px 6px; }
  .mono { font-family: ui-monospace, Consolas, monospace; font-size: 12px; }
  .err { color: #c00; }
</style>
</head>
<body>
<h1>多机器媒体文件去重 — M1 骨架</h1>

<h2>Agent 连接状态</h2>
<table id="agents"><thead><tr><th>machine_id</th><th>地址</th><th>状态</th><th>最近错误</th></tr></thead><tbody></tbody></table>

<h2>下发扫描任务</h2>
<div>
  <select id="machine"></select>
  <input id="roots" size="60" placeholder='扫描根路径，多个用 | 分隔，如 D:\media|E:\photos'>
  <label><input type="checkbox" id="rescan"> 强制重算</label>
  <button onclick="startScan()">开始普扫</button>
  <span id="scanMsg"></span>
</div>

<h2>任务进度</h2>
<table id="tasks"><thead><tr>
  <th>task_id</th><th>机器</th><th>状态</th><th>进度</th><th>速度(文件/s)</th><th>最近错误</th>
</tr></thead><tbody></tbody></table>

<h2>精确重复组（SHA-512 一致，plan 5.1）</h2>
<table id="groups"><thead><tr>
  <th>sha512</th><th>副本数</th><th>机器数</th><th>总字节</th><th>浪费字节</th>
</tr></thead><tbody></tbody></table>
<div id="members"></div>

<script>
async function j(u, opt) { const r = await fetch(u, opt); return r.json(); }
function fmtBytes(n) {
  const u = ['B','KB','MB','GB','TB']; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(1) + ' ' + u[i];
}

async function refreshAgents() {
  const list = await j('/api/agents');
  const tb = document.querySelector('#agents tbody'); tb.innerHTML = '';
  const sel = document.getElementById('machine'); sel.innerHTML = '';
  for (const a of list) {
    tb.insertAdjacentHTML('beforeend',
      `<tr><td>${a.machine_id}</td><td>${a.addr}</td>` +
      `<td class="${a.online ? 'on' : 'off'}">${a.online ? '在线' : '离线'}</td>` +
      `<td class="err">${a.last_err || ''}</td></tr>`);
    if (a.online) sel.insertAdjacentHTML('beforeend',
      `<option value="${a.machine_id}">${a.machine_id}</option>`);
  }
}

async function refreshTasks() {
  const list = await j('/api/tasks');
  const tb = document.querySelector('#tasks tbody'); tb.innerHTML = '';
  for (const t of list) {
    tb.insertAdjacentHTML('beforeend',
      `<tr><td class="mono">${t.task_id.slice(0,8)}</td><td>${t.machine_id}</td>` +
      `<td>${t.status}${t.ack_reason ? '(' + t.ack_reason + ')' : ''}</td>` +
      `<td>${t.done}/${t.total < 0 ? '?' : t.total}</td>` +
      `<td>${t.speed ? t.speed.toFixed(1) : ''}</td>` +
      `<td class="err">${t.last_err || ''}</td></tr>`);
  }
}

async function refreshGroups() {
  const list = await j('/api/dup_groups?limit=200');
  const tb = document.querySelector('#groups tbody'); tb.innerHTML = '';
  for (const g of list) {
    tb.insertAdjacentHTML('beforeend',
      `<tr onclick="showMembers('${g.sha512}')" style="cursor:pointer">` +
      `<td class="mono">${g.sha512.slice(0,16)}…</td><td>${g.member_count}</td>` +
      `<td>${g.machines}</td><td>${fmtBytes(g.total_bytes)}</td>` +
      `<td>${fmtBytes(g.wasted_bytes)}</td></tr>`);
  }
}

async function showMembers(sha) {
  const list = await j('/api/dup_groups/' + sha);
  let h = `<h3>组 ${sha.slice(0,16)}… 的 ${list.length} 个副本</h3><table><thead>` +
    `<tr><th>机器</th><th>路径</th><th>大小</th><th>修改时间</th></tr></thead><tbody>`;
  for (const m of list) {
    h += `<tr><td>${m.machine_id}</td><td class="mono">${m.path}</td>` +
      `<td>${fmtBytes(m.size)}</td><td>${new Date(m.mtime*1000).toLocaleString()}</td></tr>`;
  }
  document.getElementById('members').innerHTML = h + '</tbody></table>';
}

async function startScan() {
  const machine = document.getElementById('machine').value;
  const roots = document.getElementById('roots').value.split('|').map(s => s.trim()).filter(Boolean);
  const rescan = document.getElementById('rescan').checked;
  const r = await j('/api/scan', {
    method: 'POST', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({machine_id: machine, roots, phase: 1, rescan})
  });
  document.getElementById('scanMsg').textContent =
    r.task_id ? '已下发 task_id=' + r.task_id.slice(0,8) : ('失败: ' + r.error);
  setTimeout(refreshTasks, 500);
}

function tick() { refreshAgents(); refreshTasks(); refreshGroups(); }
tick(); setInterval(tick, 2000);
</script>
</body>
</html>
```

---

### 4.10 配置与进程入口

#### 4.10.1 Agent 配置（`internal/config/agent.go`，`deploy/agent.example.json`）

```go
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// AgentConfig 对应 agent.json；默认值与 plan 9 节一致。
type AgentConfig struct {
	MachineID  string      `json:"machine_id"`  // 必填，全系统唯一
	ListenAddr string      `json:"listen_addr"` // 默认 0.0.0.0:9101
	DataDir    string      `json:"data_dir"`    // agent.db 与日志目录，默认 ./data
	PGDSN      string      `json:"pg_dsn"`      // 中心库 DSN
	UseEverything bool     `json:"use_everything"` // 默认 true；不可用时自动回退 walker
	Scan       ScanConfig  `json:"scan"`
	Sync       SyncConfig  `json:"sync"`
	Proto      ProtoConfig `json:"proto"`
}

type ScanConfig struct {
	HDDReadBlockMB     int      `json:"hdd_read_block_mb"`      // 默认 4（=哈希块 4MB）
	HDDStreams         int      `json:"hdd_streams_per_disk"`   // 默认 2
	SSDStreams         int      `json:"ssd_streams_per_disk"`   // 默认 6
	ImageMemResidentMB int      `json:"image_mem_resident_mb"`  // 默认 256（M2 用）
	ImageTimeoutS      int      `json:"image_timeout_s"`        // 默认 30（M2 用）
	VideoTimeoutS      int      `json:"video_timeout_s"`        // 默认 120（M2 用）
	ImageExts          []string `json:"image_exts"`             // 空=用内置默认表
	VideoExts          []string `json:"video_exts"`             // 空=用内置默认表
}

type SyncConfig struct {
	IntervalS   int `json:"interval_s"`    // 默认 300（5min）
	TriggerRows int `json:"trigger_rows"`  // 默认 50000
	UpsertBatch int `json:"upsert_batch"`  // 默认 5000/事务
}

type ProtoConfig struct {
	HeartbeatS int `json:"heartbeat_s"` // 默认 15
}

func (c *AgentConfig) SyncInterval() time.Duration {
	return time.Duration(c.Sync.IntervalS) * time.Second
}

// DefaultAgent 返回带全部默认值的配置。
func DefaultAgent() *AgentConfig {
	return &AgentConfig{
		ListenAddr:    "0.0.0.0:9101",
		DataDir:       "./data",
		UseEverything: true,
		Scan: ScanConfig{
			HDDReadBlockMB: 4, HDDStreams: 2, SSDStreams: 6,
			ImageMemResidentMB: 256, ImageTimeoutS: 30, VideoTimeoutS: 120,
		},
		Sync:  SyncConfig{IntervalS: 300, TriggerRows: 50000, UpsertBatch: 5000},
		Proto: ProtoConfig{HeartbeatS: 15},
	}
}

// LoadAgent 读 JSON 覆盖默认值；machine_id 与 pg_dsn 必填。
func LoadAgent(path string) (*AgentConfig, error) {
	cfg := DefaultAgent()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if cfg.MachineID == "" {
		return nil, fmt.Errorf("config: machine_id required")
	}
	if cfg.PGDSN == "" {
		return nil, fmt.Errorf("config: pg_dsn required")
	}
	return cfg, nil
}
```

#### 4.10.2 GUI 配置（`internal/config/gui.go`，`deploy/gui.example.json`）

```go
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// GUIConfig 对应 gui.json。
type GUIConfig struct {
	ListenAddr  string          `json:"listen_addr"`  // Web 监听，默认 127.0.0.1:8080
	PGDSN       string          `json:"pg_dsn"`       // 中心库 DSN（分析数据源，plan 3 节）
	Agents      []AgentEndpoint `json:"agents"`       // 直连的 Agent 列表
	HeartbeatS  int             `json:"heartbeat_s"`  // 默认 15
}

// AgentEndpoint 是一台 Agent 的直连端点。
type AgentEndpoint struct {
	MachineID string `json:"machine_id"` // 与 agent.json 一致，Hello 校验
	Addr      string `json:"addr"`       // host:9101
}

// DefaultGUI 返回带默认值的配置。
func DefaultGUI() *GUIConfig {
	return &GUIConfig{ListenAddr: "127.0.0.1:8080", HeartbeatS: 15}
}

// LoadGUI 读 JSON 覆盖默认值；pg_dsn 与 agents 必填。
func LoadGUI(path string) (*GUIConfig, error) {
	cfg := DefaultGUI()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if cfg.PGDSN == "" || len(cfg.Agents) == 0 {
		return nil, fmt.Errorf("config: pg_dsn and agents required")
	}
	return cfg, nil
}
```

#### 4.10.3 Agent 入口（`cmd/agent/main.go`）

```go
package main

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/natefinch/lumberjack.v2"

	"dedup/internal/agent"
	"dedup/internal/config"
	"dedup/internal/enum"
	"dedup/internal/store"
	"dedup/internal/syncer"
)

func main() {
	cfgPath := flag.String("config", "agent.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.LoadAgent(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		slog.Error("mkdir data dir", "err", err)
		os.Exit(1)
	}

	// agent.log：JSON 行 + lumberjack 滚动（plan 8 节）；同时输出到控制台便于调试。
	logFile := &lumberjack.Logger{
		Filename:   filepath.Join(cfg.DataDir, "agent.log"),
		MaxSize:    100, MaxBackups: 5, MaxAge: 30, Compress: true, // MB / 天
	}
	log := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, logFile), nil))
	// errors.log：一文件一失败一行。
	errFile := &lumberjack.Logger{
		Filename:   filepath.Join(cfg.DataDir, "errors.log"),
		MaxSize:    100, MaxBackups: 5, MaxAge: 30, Compress: true,
	}
	errLog := slog.New(slog.NewJSONHandler(errFile, nil))

	st, err := store.Open(filepath.Join(cfg.DataDir, "agent.db"))
	if err != nil {
		log.Error("open sqlite", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// 枚举器：Everything 优先，IPC 不可用回退 walker 并告警（plan 11 节）。
	var enumr enum.Enumerator = enum.NewEverythingEnumerator()
	if cfg.UseEverything {
		if err := enumr.Available(); err != nil {
			log.Warn("everything unavailable, fallback to walker", "err", err)
			enumr = enum.WalkerEnumerator{}
		}
	} else {
		enumr = enum.WalkerEnumerator{}
	}
	log.Info("enumerator ready", "name", enumr.Name())

	// 中心库：连不上只告警，同步器每轮重试，扫描链路不受影响（T7-3）。
	pg, err := pgxpool.New(context.Background(), cfg.PGDSN)
	if err != nil {
		log.Error("parse pg dsn", "err", err)
		os.Exit(1)
	}
	defer pg.Close()
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := pg.Ping(pingCtx); err != nil {
		log.Warn("postgres unreachable at startup, syncer will retry", "err", err)
	}
	cancel()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sync := syncer.New(st, pg, syncer.Config{
		Interval:    cfg.SyncInterval(),
		TriggerRows: int64(cfg.Sync.TriggerRows),
		UpsertBatch: cfg.Sync.UpsertBatch,
	}, log)
	go sync.Run(ctx)

	sm := agent.NewScanManager(cfg, st, enumr, agent.GoHasher{}, log, errLog)
	srv := agent.NewServer(cfg, sm, log)
	if err := srv.ListenAndServe(ctx); err != nil {
		log.Error("server exited", "err", err)
		os.Exit(1)
	}
}
```

#### 4.10.4 GUI 入口（`cmd/gui/main.go`）

```go
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dedup/internal/config"
	"dedup/internal/gui"
)

func main() {
	cfgPath := flag.String("config", "gui.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.LoadGUI(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	pg, err := pgxpool.New(context.Background(), cfg.PGDSN)
	if err != nil {
		log.Error("parse pg dsn", "err", err)
		os.Exit(1)
	}
	defer pg.Close()
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := pg.Ping(pingCtx); err != nil {
		log.Error("postgres unreachable", "err", err)
		os.Exit(1) // GUI 的分析数据源只有中心库，连不上无法工作（plan 3 节）
	}
	cancel()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tasks := gui.NewTaskRegistry(pg, log)
	pool := gui.NewPool(cfg.Agents, log, func(machineID string, _ *gui.AgentConn, msg any) {
		tasks.Dispatch(machineID, msg)
	})
	pool.Start(ctx, time.Duration(cfg.HeartbeatS)*time.Second)

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: gui.NewAPI(pool, tasks, pg).Routes()}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shCtx)
	}()
	log.Info("gui listening", "addr", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server exited", "err", err)
		os.Exit(1)
	}
}
```

> 注：`NewPool` 内部完成 `AgentConn` 的构造与启动，`main` 仅在分发回调签名中引用导出类型 `*gui.AgentConn`，4.9.1 代码按原样即可编译。

---

## 5. 数据模型与配置项

### 5.1 状态机与字段语义（files 表，Agent 本地与中心库同构）

| 字段 | 语义 | M1 写入方 |
|---|---|---|
| `status` | `pending` 待算 / `done` 本轮请求字段全部成功 / `partial` 部分字段失败（M2 起多字段才有意义）/ `failed` 全部失败 / `crash` Worker 崩溃（M2 起） | 枚举置 `pending`，哈希成功置 `done`，失败置 `failed` |
| `missing_mask` | 缺失字段位（4.1 常量）。图片初始 `0b000011`（SHA+PDQ），视频 `0b000101`（SHA+缩略图），其他 `0b000001` | 枚举置基础位；哈希成功清 bit0 |
| `phase1_done` | 一阶段三位（bit0~2）全清时置 1 | M1 媒体文件保持 0，other 类置 1（预期） |
| `phase2_done` | 二阶段三位（bit3~5）全清时置 1 | M1 保持 0 |
| `disk_no` | `IOCTL_STORAGE_GET_DEVICE_NUMBER` 的 `DeviceNumber`；未解析成功为 -1 | 枚举时写入 |
| `mtime` / `updated_at` | 文件修改时间 / 行更新时间，均为 Unix 秒 | — |

### 5.2 配置项表（默认值严格对齐 plan 9 节；"M1 使用"标注本里程碑是否生效）

| 配置键（agent.json） | 默认值 | plan 参数 | M1 使用 |
|---|---|---|---|
| `scan.hdd_read_block_mb` | 4 | HDD 读块 4MB | 是（哈希块对齐） |
| `scan.hdd_streams_per_disk` | 2 | HDD 并发流/盘 2 | 是 |
| `scan.ssd_streams_per_disk` | 6 | SSD 并发流/盘 6 | 是 |
| —（Worker 数） | CPU 核数 | Worker 数 | 否（M2 Worker 池） |
| `scan.image_mem_resident_mb` | 256 | 图片内存驻留阈值 | 否（M2 解码用） |
| `scan.image_timeout_s` | 30 | 图片单文件超时 | 否（M2 看门狗） |
| `scan.video_timeout_s` | 120 | 视频单文件超时 | 否（M2 看门狗） |
| `sync.interval_s` | 300 | 同步周期 5min | 是 |
| `sync.trigger_rows` | 50000 | 同步积压 5 万行 | 是 |
| `sync.upsert_batch` | 5000 | —（实现细节） | 是 |
| `proto.heartbeat_s` | 15 | TCP 心跳 15s | 是 |
| —（PDQ 阈值 T1=31、长宽比 10%、T2=80%、T3=0.85、时长差 2s、T4=0.8） | 见 plan 9 节 | 一筛/二阶段 | 否（M3/M4，届时入 GUI 配置） |

| 配置键（gui.json） | 默认值 | 说明 |
|---|---|---|
| `listen_addr` | `127.0.0.1:8080` | Web/HTTP 监听 |
| `pg_dsn` | 必填 | 中心库 DSN |
| `agents[].machine_id` / `agents[].addr` | 必填 | Agent 列表（Hello 校验 machine_id） |
| `heartbeat_s` | 15 | 心跳周期 |
| —（重连退避） | 1s×2 封顶 30s | 代码内常量（plan 7 节"指数退避"） |

### 5.3 默认扩展名表（`scan.image_exts` / `scan.video_exts` 可覆盖）

- image：`.jpg .jpeg .png .webp .bmp .gif .tif .tiff`
- video：`.mp4 .mkv .avi .mov .wmv .flv .ts .m2ts .mpg .mpeg .webm .3gp`
- 其余一律 `other`（M1 只算 SHA-512，M2 起不参与特征计算）。

---

## 6. 测试与验收

### 6.1 单元测试（`go test ./...`，CI 可跑的部分）

| # | 包 | 用例 | 通过标准 |
|---|---|---|---|
| UT-1 | `internal/proto` | 帧 roundtrip：全部消息类型 encode→decode 字段一致 | 全部通过 |
| UT-2 | `internal/proto` | 帧边界：长度=0 或 >16MB 返回 `ErrFrameTooLarge`；随机垃圾字节返回 error 不 panic | 全部通过 |
| UT-3 | `internal/proto` | 16 goroutine 各写 1000 帧，对端逐帧解码无串帧（每条消息校验和正确） | 全部通过 |
| UT-4 | `internal/agent` | SHA-512 已知向量：空文件、`"abc"`、10MB 随机数据（跨 4MB 块）与 `crypto/sha512` 直接算一致；再与 `certutil -hashfile <f> SHA512` 对拍一个真实文件 | hex 全等 |
| UT-5 | `internal/agent` | `MediaKind` / `MissingBase`：`.JPG`（大写）→ image `0b000011`；`.mkv` → video `0b000101`；`.txt` → other `0b000001` | 全等 |
| UT-6 | `internal/store` | DDL 二次执行幂等；同一语料枚举两次行数不增；改 mtime 后该行重新 `pending` 且 bit0 置位；sha 不变时 `status`/`missing_mask` 保持 | 断言通过 |
| UT-7 | `internal/store` | `ApplyHashResults` 成功路径清 bit0、other 类 `phase1_done=1`、图片保持 0；失败路径置 `failed` 且保留 bit0；`sync_queue` 同行重复入队仍一行 | 断言通过 |
| UT-8 | `internal/enum` | 长路径辅助：≥248 字符路径加 `\\?\` 前缀 | 断言通过 |
| UT-9 | `internal/gui` | `TaskRegistry.Dispatch`：Ack/Progress/FeatureResult(>50 条截断)/Done/Error 状态流转正确 | 断言通过 |

### 6.2 集成测试（单机，需 Windows + Everything + PG 16）

| # | 用例 | 步骤 | 通过标准 |
|---|---|---|---|
| IT-1 | Everything 枚举 | 准备含中文名、空格名、>260 字符长路径、空目录的目录树；跑 Everything 与 Walker 两种枚举 | 两者文件数一致；长路径记录完整 |
| IT-2 | 盘号映射 | 对机器每个盘符执行 `diskmap.Resolve` | `DeviceNumber` 与 `Get-Disk` 的 Number 一致；SSD 判定与 `Get-PhysicalDisk` 的 MediaType 一致 |
| IT-3 | 同步幂等 | 本地造 1000 行 files，跑 `syncer.syncOnce` 两次 | 中心库 `files` 恰好 1000 行；`sync_queue` 全部 `synced=1` |
| IT-4 | PG 不可达降级 | 停掉 PG 启动 Agent，下发小任务，重启 PG | Agent 不退出、任务正常完成；PG 恢复后 ≤ 一个周期数据到齐 |
| IT-5 | Everything 不可用回退 | 退出 Everything 进程后启动 Agent | 日志出现 `fallback to walker` 告警；枚举结果与 IT-1 一致 |

### 6.3 双机联调验收（里程碑验收标准：双机普扫，精确重复组在 GUI 正确汇总）

**环境**：机器 A、机器 B（Windows，均运行 Everything ≥1.4 并完成索引）；机器 C 或 A/B 之一跑 PostgreSQL 16（已执行 `deploy/central.sql`）与 GUI。验收用 `sync.interval_s=10`（仅加速，不改语义，见 1.3）。

**语料**（两台机器分别放置）：

| 文件 | 内容 | 位置 |
|---|---|---|
| `dup1.bin` ×3 | 同一随机 10MB 内容 | A:`D:\mtest\a\dup1.bin`，A:`D:\mtest\b\copy.bin`，B:`E:\mtest\dup1.bin` |
| `dup2.jpg` ×2 | 同一真实 JPEG 内容（改名不同） | A:`D:\mtest\photo.jpg`，B:`E:\mtest\img_001.jpg` |
| `dup3.mp4` ×2 | 同一视频内容 | A:`D:\mtest\v.mp4`，B:`E:\mtest\video.mp4` |
| `empty.dat` ×2 | 0 字节 | A、B 各一 |
| `uniq-a.bin` / `uniq-b.bin` | 互不相同的随机 5MB | A、B 各一 |
| `big.bin` | 300MB 随机（跨 4MB 块×75） | A |
| `锁定的文件.bin` | 任意内容，测试时以 `cmd /c "copy /y nul ..."` 后用另一进程独占锁（或验收时改为无权限目录） | B |

**用例**：

| # | 用例 | 操作 | 通过标准 |
|---|---|---|---|
| AC-1 | 建连与心跳 | 启动 Agent A/B 与 GUI；静置 60s | Web"Agent 连接状态"两台在线；`agent.log` 显示连接建立；Wireshark/日志可见 ≤15s 周期 Ping/Pong；60s 内无断连 |
| AC-2 | 双机普扫 | Web 页分别对 A（`D:\mtest`）、B（`E:\mtest`）下发 phase=1 任务 | 两任务均收到 `TaskAck{accepted}`；进度持续增长至 `TaskDone`；`stats.total`=各自语料文件数；`failed` 计数与语料中不可读文件数一致 |
| AC-3 | 精确重复组汇总 | 任务完成 → 等待 ≤2 个同步周期 → 打开"精确重复组"页 | 恰好 4 组：dup1(3 副本/2 机)、dup2(2/2)、dup3(2/2)、empty(2/2)；uniq/big 不出现；展开 dup1 组成员为语料表中的 3 条路径 |
| AC-4 | 中心库一致性 | `psql` 执行 `SELECT count(*), count(DISTINCT sha512) FROM files WHERE sha512 IS NOT NULL;` | 行数 = 两机语料文件总数；distinct sha = 语料中不同内容种数；与 GUI 展示一致 |
| AC-5 | 哈希正确性 | 中心库查 dup1 组的 sha512，与 `certutil -hashfile D:\mtest\a\dup1.bin SHA512` 对比 | 完全一致（小写 hex） |
| AC-6 | 断线重连 | 扫描中途 `taskkill` 机器 B 的 Agent；30s 后重启 | Web 在 45s 内显示 B 离线；重启后 ≤60s 自动重连；Agent 本地库已算数据不丢（重启后直接 `TaskAck` 新任务时剪枝跳过已算文件） |
| AC-7 | 断点续传 | 扫描中途重启 GUI（不杀 Agent）；GUI 起来后用**同一 task_id** 重发 ScanTask | Agent 回 `TaskAck{resumed}`；进度从当前值继续而非从 0；最终 `TaskDone` 计数正确 |
| AC-8 | 坏文件不中断 | 语料中含锁定/不可读文件 | 任务正常 `TaskDone`；`errors.log` 恰有一行对应记录；GUI 任务上 `last_err` 可见；其余文件全部算完（plan 8 节坏文件原则） |
| AC-9 | 幂等重扫 | AC-2 完成后对 A 原样再发一次普扫（不带 rescan） | `stats.skipped` = 上次成功文件数、`done` = 0（或仅新增/变更文件数）；中心库行数不变 |
| AC-10 | 协议健壮性 | 用 `nc`/自写脚本向 Agent 端口发送 4 字节超大长度前缀（如 0x7FFFFFFF）与垃圾字节 | Agent 关闭该连接、主进程存活、`agent.log` 有 warn；正常任务不受影响 |

**一键核对 SQL**（psql）：

```sql
-- 精确重复组（应与 GUI 展示一致）
SELECT sha512, count(*) AS members, count(DISTINCT machine_id) AS machines
FROM files WHERE sha512 IS NOT NULL
GROUP BY sha512 HAVING count(*) > 1
ORDER BY members DESC;

-- 失败文件（应等于语料中不可读文件）
SELECT machine_id, path, error FROM files WHERE status = 'failed';

-- 任务台账
SELECT id, machine_id, phase, status, stats_json FROM scan_tasks ORDER BY created_at;
```

---

## 7. 风险与注意事项

| # | 风险/注意点 | 说明与缓解 |
|---|---|---|
| R-1 | **Everything 未运行/索引不全** | 启动探测 + 回退 walker（plan 11 节）。另外注意：新拷贝的文件可能尚未进 Everything 索引（索引基于 USN 日志通常秒级，但大批量拷贝后可能滞后）。验收语料拷贝完成后等待 ≥30s 再扫描；生产环境可在 ScanOptions 后续加 `force_walker`。 |
| R-2 | **DLL 分发** | Agent 与 GUI 均以 `CGO_ENABLED=0, GOOS=windows, GOARCH=amd64` 构建；Everything64.dll 为 voidtools SDK 组件，随 agent.exe 同目录分发（注意其再分发许可）。M2 的 worker/mediacore 可独立引入 cgo，不影响 Agent。构建步骤见附录 A。 |
| R-3 | **`DeviceNumber` 不稳定** | 物理盘号在增删盘/部分重启场景下可能重排。M1 口径：`disk_no` 只作当轮扫描的调度分桶键，内存缓存、重启重建；**不**用 `disk_no` 做跨轮次的文件归属判断（剪枝只认 size+mtime+sha512）。 |
| R-4 | **0 字节文件成组** | 空文件 sha512 恒等，会形成一个天然大"精确重复组"。属正确行为；GUI 按 `wasted_bytes` 排序时 0 字节组自然沉底。M3 起分析侧可考虑过滤 size=0。 |
| R-5 | **长路径与路径表示** | Everything 返回长路径；Go 的 `os.Open`/`filepath.WalkDir` 在 >MAX_PATH 时需 `\\?\` 前缀（已实现 `longPath`）。入库统一存**不带前缀**的形式（前缀只在打开文件时临时加），避免中心库同路径两种写法。 |
| R-6 | **SQLite 写并发** | WAL + 单连接 + 批量事务（枚举 1 万行/事务、结果 500 行/事务）是本设计吞吐前提；勿在多 goroutine 中各自开写事务（会 SQLITE_BUSY 打转）。`-wal` 文件高峰可能达数百 MB，磁盘预留空间。 |
| R-7 | **Everything 查询内存峰值** | `QueryW(TRUE)` 一次拷回全部结果，百万级约数百 MB。M1 接受；M6 若实测超标，改 `Everything_SetOffset/SetMax` 视窗分页（接口不变）。 |
| R-8 | **PG 上行乱序与覆盖** | 同一行短时间内多次变更只入队一次（主键去重），上行的是**读时点的最新行**，符合"最终一致"语义；两机同 sha512 的特征行合流依赖 `ON CONFLICT` 只覆盖非空字段（4.7.2），M2 接入特征表时不得改成裸覆盖。 |
| R-9 | **协议演进** | 消息体新增字段必须只加不改（msgpack map 按字段名解码，老端忽略未知键）；破坏性变更升 `ProtocolVersion`，Hello 校验拒绝混连。`FeatureItem` 在 M2 会扩展 PDQ/宽高/时长字段，GUI 解析代码要用 `omitempty` 容忍缺省。 |
| R-10 | **时间口径** | `mtime`/`updated_at` 一律 Unix 秒（UTC）；FILETIME 转换已含时区无关的 11644473600 秒偏移。中心库 `synced_at`/`created_at` 用 TIMESTAMPTZ + `now()`。 |

---

## 附录 A：构建与部署

### A.1 工具链

- Go 1.22+（`go version` 确认）。
- voidtools Everything SDK 1.4+（`Everything-SDK.zip`），目标机安装并运行 Everything 1.4+。

### A.2 Everything DLL 分发

无需生成导入库。`internal/enum/everything_windows.go` 在运行期通过 `x/sys/windows.LoadDLL`/`FindProc` 解析 Everything SDK；将 SDK 的 `Everything64.dll` 放在 `agent.exe` 同目录即可。

### A.3 构建与运行

```bash
# 中心库（任选其一）：本机 PG16 或
docker run -d --name dedup-pg -p 5432:5432 \
  -e POSTGRES_PASSWORD=dedup postgres:16
psql "postgres://postgres:dedup@127.0.0.1:5432/postgres" -c "CREATE DATABASE dedup;"
psql "postgres://postgres:dedup@127.0.0.1:5432/dedup" -f deploy/central.sql

# Agent（纯 Go；windows/amd64）
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -o bin/agent.exe ./cmd/agent
copy third_party\everything_sdk\Everything64.dll bin\

# GUI（纯 Go）
set CGO_ENABLED=0
go build -o bin/gui.exe ./cmd/gui

# 配置
copy deploy\agent.example.json bin\agent.json   # 改 machine_id / pg_dsn
copy deploy\gui.example.json bin\gui.json       # 改 pg_dsn / agents 列表

# 运行
bin\agent.exe --config bin\agent.json           # 机器 A、B 各一
bin\gui.exe --config bin\gui.json               # 机器 C（或 A/B 之一）
# 浏览器打开 http://127.0.0.1:8080
```

### A.4 单元/集成测试命令

```bash
go test ./...                 # 单元测试
go vet ./...
golangci-lint run             # 如已安装
```

---

## 附：与 plan 的对应关系速查

| plan 章节 | 本文档章节 |
|---|---|
| 2 选型（Go/Everything/SQLite/PG/msgpack） | 4.1~4.7、附录 A |
| 3 总体架构（GUI 直连多 Agent） | 4.8、4.9 |
| 4.3 盘级 IO 调度 | 4.4、4.8.1（盘级分桶 + HDD2/SSD6 流） |
| 4.4 剪枝（size+mtime+sha 跳过） | 4.6.2 upsert/PendingSnapshot |
| 6.1 / 6.2 数据模型 | 4.6.1、4.7.1 |
| 6.3 同步策略（5min/5万行、ON CONFLICT） | 4.7.2、4.7.3 |
| 7 通信协议（帧格式/心跳/消息类型） | 4.1、4.2 |
| 8 日志规范（M1 子集：agent.log/errors.log） | 4.10.3、4.8.1 reportErr |
| 9 默认参数表 | 5.2 |
| 10 M1 验收标准 | 6.3 AC-1~AC-10 |
| 11 风险（Everything 回退等） | 4.3、7 节 |

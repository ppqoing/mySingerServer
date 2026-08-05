# M5 删除组件 — 详细实施文档

> 依据：`docs/architecture-plan.md` v1.1（§2 选型表"删除组件"行、§3 总体架构、§4.1 进程模型、§5.3 输出与删除、§7 协议 `DeleteTask`/`DeleteReport`、§8 日志规范 `delete.log`、§10 里程碑 M5、§11 风险表"删除误操作"）。
> 里程碑验收标准（plan §10）：**勾选删除端到端走通，只读文件可删**。
> 依赖：M1（TCP 协议帧、Agent 骨架、slog+lumberjack 日志组件、SQLite 层）。与 M2~M4 可并行开发。
> 说明：本文代码骨架按 Go 1.22 + 文中所列依赖编写，首次构建以 `go mod tidy` 实际解析为准。

---

## 1. 目标与范围

### 1.1 目标

在各机器上交付一个**独立提权 Helper**（`helper.exe`，manifest `requireAdministrator`，由用户手动启动并确认一次 UAC 后以管理员权限常驻运行），实现从 GUI 勾选重复组成员到文件删除的端到端链路：

1. GUI 勾选重复组成员 → **二次确认**（弹窗核对摘要）→ `DeleteTask` 经 TCP 发到目标机器 Agent；
2. Agent（普通权限）**原样转发**到本机命名管道 `\\.\pipe\dedup-delete`；
3. Helper（管理员权限）逐项执行：查询文件属性（`FILE_ATTRIBUTE_READONLY`）→ 只读则改可写 → 删除 → 逐项回执；
4. 回执经 Agent 返回 GUI，并由 Agent 逐项写 `delete.log` 审计（一行一次：时间、路径、结果、错误）；
5. 可选**软删模式**：不物理删除，移入本盘回收目录 `<卷>:\$DedupRecycle\`；
6. 安全约束：Helper 只接受本机命名管道连接、只执行带二次确认标记的清单、逐项做路径白名单校验防越权删除；
7. 权限隔离：Agent 主进程 / Worker / ffmpeg 全程普通权限，只有 `helper.exe` 由用户手动提权启动并常驻运行；Agent 不承担提权启动职责。

### 1.2 端到端时序

```
用户 ──▶ GUI(Web 页面): 勾选重复组成员 → 点"删除所选"（第一次确认）
GUI  ──▶ GUI 服务端:  POST /api/delete/prepare  → 返回摘要(数量/总大小/样本路径) + confirm_token
用户 ──▶ 确认弹窗:   核对摘要、选择模式(软删/硬删, 默认软删) → 点"确认删除"（第二次确认）
GUI 服务端:          校验 confirm_token(60s 内、一次性) → 按机器拆分清单
      ── TCP ──▶     DeleteTask{task_id, entries[], options{mode, confirmed:true}} → 各目标 Agent
Agent:               分块(≤2000 项/帧) → 连接常驻 Helper(管道不可达则任务失败, 返回明确错误提示 Helper 未启动)
      ── 管道 ──▶    DeleteTask 帧原样写入 \\.\pipe\dedup-delete
Helper:              逐项 Lstat → 白名单/拒绝前缀/UNC/reparse/目录 校验
                     → 只读? 清除 FILE_ATTRIBUTE_READONLY → 硬删(os.Remove) 或 软删(移入本盘回收目录)
      ── 管道 ──▶    DeleteReport 回执帧(逐项 path/ok/err_code/readonly_cleared/recycled_to)
Agent:               逐项写 delete.log → SQLite files.status='deleted' + sync_queue → 回执经 TCP 转发 GUI
GUI:                 展示进度与最终结果；中心库上行后组展示排除已删成员
```

### 1.3 不做什么

- **不使用 Windows 回收站**（`SHFileOperation`/`IFileOperation`）：提权进程操作回收站按操作者上下文落盘、跨盘行为不一致，且拿不到稳定的"原路径 → 回收位置"映射用于审计与还原。以自研"本盘回收目录"软删替代。
- **不做目录递归删除**：清单只含文件；目标是目录一律拒绝（`E_BAD_PATH`）。
- **不夺取所有权、不改写 ACL**（不启用 `SeTakeOwnershipPrivilege`）：权限不足只在回执中如实报错。
- **不强杀占用进程**，不删除 reparse point（junction/symlink）及其指向的目标。
- **不做回收目录的自动清理/容量管理**：本期只写不删，清理策略由人工或后续版本处理。
- **不开放远程管道访问**：DACL 显式拒绝 NETWORK 登录。
- **不做跨机代删**：Agent 只处理本机盘符路径，UNC 路径一律拒绝。
- **GUI 分析侧的 `dup_groups` 清理/重算归 GUI 模块**：M5 只保证 `files.status='deleted'` 随同步上行，GUI 展示时排除该状态。

---

## 2. 任务分解

### 2.1 协议与消息（`internal/proto`）

- [x] P1 定义删除相关 msgpack 消息：`DeleteTaskMsg` / `DeleteReportMsg` / `HelloMsg` / `ShutdownMsg`（对 plan §7 `DeleteTask`/`DeleteReport` 的向后兼容扩展：msgpack map 增加 `seq`、`last_seq`、`options`、`stats` 字段，原有 `task_id`、`entries[]{path,ok,err}` 语义不变）。
- [x] P2 定义删除模式常量 `ModeHard`/`ModeSoft` 与全部稳定错误码（`E_NOT_FOUND` 等 12 个，见 4.1）。
- [x] P3 帧编解码工具 `WriteFrame`/`ReadFrameRaw`/`Decode`：`[4B 大端长度][msgpack body]`，帧上限 16MB。**若 M1 已实现同名工具则直接复用 M1，禁止出现第二份实现。**

### 2.2 Helper（`cmd/helper` + `internal/helper`）

- [x] H1 `helper.manifest`（`requireAdministrator`）+ `rsrc` 嵌入构建，产出单文件 `helper.exe`。
- [x] H2 `helper.json` 配置加载：白名单/拒绝前缀/默认模式/回收目录名等，含默认值与启动校验（`allowed_roots` 为空直接拒绝启动）。
- [x] H3 命名管道服务端：`winio.ListenPipe`，SDDL 限制本机访问（拒绝 NETWORK，允许 SYSTEM/Administrators/启动用户）。
- [x] H4 单实例：命名互斥体 `Local\DedupDeleteHelperMutex`，第二个实例立即退出。
- [x] H5 路径校验器：拒绝 UNC/设备路径、非绝对路径、目录、reparse point；白名单前缀命中 + 拒绝前缀排除（盘符边界安全，大小写不敏感）。
- [x] H6 硬删执行：`Lstat` 取属性 → 只读则 `SetFileAttributes` 清 `FILE_ATTRIBUTE_READONLY`（保留其余属性位）→ `os.Remove` → 单项回执。
- [x] H7 软删执行：计算 `<卷>:\$DedupRecycle\<task_id>\<卷内相对路径>`（冲突追加 `_N` 后缀）→ `os.MkdirAll` → `os.Rename` → 回执带 `recycled_to`。
- [x] H8 回执组装：逐项独立、单项失败不中断整块；`confirmed=false` 整块拒绝（`E_NOT_CONFIRMED`）；未知模式/禁用硬删整块拒绝（`E_BAD_MODE`）；超 `max_entries_per_frame` 整块拒绝。
- [x] H9 连接处理：接入即回 `HelloMsg`；逐帧读（读超时 120s/帧，写超时 60s）；`Shutdown` 消息退出；串行处理连接。
- [x] H10 常驻运行：启动后持续服务、不自动退出；仅收到 `Shutdown` 消息或进程被终止时退出。
- [x] H11 `helper.log`：slog JSON + lumberjack，记启动、白名单生效、连接、安全拒绝、任务摘要（**逐项审计在 Agent 侧 `delete.log`**，见 A4）。

### 2.3 Agent 转发器（`internal/agent/delete`）

- [x] A1 `ensureHelper`：仅按 500ms 超时拨管道；不可达即返回明确错误（逐项回执 `E_HELPER_LOST`，err 文案提示"Helper 未运行，请以管理员权限启动 helper.exe"）；不含提权启动逻辑。
- [x] A2 分块：单任务条目按 ≤2000 项/帧拆分，填 `seq`/`last_seq`。
- [x] A3 帧转发：单连接依次写任务帧、读回执帧（回执读超时 10min）；管道中途故障时，当前及后续分块全部按 `E_HELPER_LOST` 生成回执，任务不悬挂。
- [x] A4 逐项审计：每条回执项写一行 `delete.log`（时间、task_id、路径、结果、错误码、`readonly_cleared`、`recycled_to`）。
- [x] A5 本地库联动：成功项 `files.status='deleted'` + 写 `sync_queue`，随 M1 同步器上行中心库。
- [x] A6 回执经 M1 TCP 连接转发回 GUI（`DeleteReport`）。
- [x] A7 Helper 会话管理：Agent 不常驻持有连接，任务结束即断开连接；Helper 常驻运行，不随连接断开而退出。

### 2.4 GUI（GUI 模块内，本文只定契约与确认令牌逻辑）

- [x] G1 重复组展示页支持勾选成员（跨组多选）。
- [x] G2 `POST /api/delete/prepare`：入参按机器分组的清单，返回摘要（文件数、总大小、每机器数量、前 20 条样本路径）+ `confirm_token`（60s TTL、一次性、绑定清单摘要）。
- [x] G3 确认弹窗：展示摘要、模式单选（软删默认选中/硬删）、红色警示文案；"确认删除"为第二次确认。
- [x] G4 `POST /api/delete/execute`：校验并消费 `confirm_token` → 按机器拆分 → 向各 Agent 发 `DeleteTask{confirmed:true, mode}`（服务端只在持有有效 token 时才置 `confirmed=true`）。
- [x] G5 进度与结果展示：按 `(task_id, seq)` 聚合 `DeleteReport`；已删成员在组展示中排除（中心库 `status='deleted'` 上行后）。

### 2.5 测试与验收

- [x] T1 通过 §6 全部验收用例（TC-01 ~ TC-12），其中 TC-01/02/03/04/05/06 为里程碑必过项。
- [x] T2 `internal/helper` 单测：路径校验矩阵（白名单内/外、UNC、目录、junction）、回收目标命名冲突。
- [x] T3 `internal/proto` 单测：消息编解码往返、超 16MB 帧拒绝。

---

## 3. 目录与文件结构

```
mySingerServer/
├─ cmd/
│  └─ helper/                        # 【M5 新增】提权 Helper 独立 exe
│     ├─ main.go                     # 入口: 单实例/配置/日志/管道服务
│     ├─ helper.manifest             # requireAdministrator manifest
│     └─ rsrc.syso                   # 生成物: rsrc 把 manifest 嵌入 exe(可入库或构建期生成)
├─ internal/
│  ├─ proto/
│  │  ├─ frame.go                    # 【M1 已有则复用】帧编解码: WriteFrame/ReadFrameRaw/Decode
│  │  └─ delete.go                   # 【M5 新增】删除消息、模式、错误码
│  ├─ helper/                        # 【M5 新增】Helper 全部业务逻辑(不依赖 main, 可单测)
│  │  ├─ config.go                   # helper.json 加载与校验
│  │  ├─ validate.go                 # 路径白名单/拒绝前缀/reparse/目录 校验
│  │  ├─ delete.go                   # 逐项执行: 只读处理/硬删/软删/回执组装
│  │  └─ server.go                   # 命名管道服务端(SDDL/常驻连接循环/Shutdown 退出)
│  ├─ agent/
│  │  └─ delete/
│  │     └─ forwarder.go             # 【M5 新增】Agent 转发器: 分块/在线检测/审计/库联动
│  └─ gui/
│     └─ delete.go                   # 【M5 新增】prepare/execute 接口与确认令牌(GUI 模块内)
├─ bin/
│  └─ helper.exe                     # 构建产物(不入库), 部署时与 helper.json 同目录
└─ docs/details/M5-delete.md         # 本文
```

部署约定：`helper.exe` 与同目录的 `helper.json` 一起分发到每台目标机器（与 `agent.exe` 同目录）；`helper.json` 需用 NTFS ACL 限制为仅管理员可写（普通用户可篡改白名单 = 安全边界失守）。

M5 新增第三方依赖（`go get` 引入；msgpack 实现必须与 M1 TCP 层保持同一选型，若 M1 已定其他库则以 M1 为准）：

```bash
go get github.com/Microsoft/go-winio@latest      # Windows 命名管道
go get golang.org/x/sys@latest                   # Token/SDDL
go get github.com/vmihailenco/msgpack/v5@latest  # msgpack 编解码(与 M1 统一)
go get gopkg.in/natefinch/lumberjack.v2@latest   # 日志滚动(plan §8)
```

以下代码假设 `go.mod` 模块名为 `dedup`（`module dedup`），包内引用路径为 `dedup/internal/...`。

---

## 4. 关键接口与结构体定义

### 4.1 消息定义与帧编解码（`internal/proto`）

帧格式与 plan §7 完全一致：`[4B 大端长度][msgpack body]`，帧上限 16MB。命名管道与 TCP 使用**同一份帧编解码与消息定义**——这就是 plan §5.3 所说"Agent 原样转发"的实现含义：Agent 把 TCP 收到的 `DeleteTask` 体直接按帧写入管道，Helper 回执体直接按帧写回 TCP。

```go
// internal/proto/delete.go
package proto

// 消息类型(与 plan §7 命名一致; Hello/Shutdown 为管道内部消息)
const (
	MsgHello        = "Hello"
	MsgDeleteTask   = "DeleteTask"
	MsgDeleteReport = "DeleteReport"
	MsgShutdown     = "Shutdown"
)

// 删除模式
const (
	ModeHard = "hard" // 物理删除
	ModeSoft = "soft" // 移入本盘回收目录
)

// 回执稳定错误码(err_code)。err 字段为人读文本(含 win32 原始错误), 不用于程序判断。
const (
	ErrNotFound      = "E_NOT_FOUND"      // 文件不存在
	ErrBadPath       = "E_BAD_PATH"       // 空路径/非绝对路径/UNC/目录/分块超限
	ErrPathDenied    = "E_PATH_DENIED"    // 未命中白名单或命中拒绝前缀
	ErrNotConfirmed  = "E_NOT_CONFIRMED"  // 缺少 GUI 二次确认标记
	ErrReadonly      = "E_READONLY"       // 只读属性清除失败
	ErrAccessDenied  = "E_ACCESS_DENIED"  // win32 ERROR_ACCESS_DENIED(权限不足)
	ErrDeleteFailed  = "E_DELETE_FAILED"  // 硬删失败(其余原因)
	ErrRecycleFailed = "E_RECYCLE_FAILED" // 软删(移入回收目录)失败(其余原因)
	ErrInUse         = "E_IN_USE"         // 文件被占用(共享冲突)
	ErrReparse       = "E_REPARSE"        // junction/symlink/reparse point, 拒绝处理
	ErrBadMode       = "E_BAD_MODE"       // 未知模式 / hard 被配置禁用
	ErrHelperLost    = "E_HELPER_LOST"    // Agent 侧生成: 管道/Helper 不可达
)

// TypeHeader 用于先读 type 再决定解码目标。
type TypeHeader struct {
	Type string `msgpack:"type"`
}

// DeleteOptions 为 plan §7 DeleteTask 的扩展字段(缺省零值 = 未确认 + 由 Helper 配置决定模式)。
type DeleteOptions struct {
	Mode      string `msgpack:"mode"`      // "hard" | "soft"; 空 = helper.json default_mode
	Confirmed bool   `msgpack:"confirmed"` // GUI 二次确认后置 true; Helper 对 false 整块拒绝
}

type DeleteEntry struct {
	Path string `msgpack:"path"`
}

// DeleteTaskMsg 对 plan §7 DeleteTask{task_id, entries[]{path}} 的向后兼容扩展:
// 新增 seq/last_seq(分块) 与 options。msgpack map 加字段, 旧字段语义不变。
type DeleteTaskMsg struct {
	Type    string         `msgpack:"type"`     // "DeleteTask"
	TaskID  string         `msgpack:"task_id"`  // UUID, GUI 生成, 同一任务各分块相同
	Seq     int            `msgpack:"seq"`      // 分块序号, 从 0 开始
	LastSeq int            `msgpack:"last_seq"` // 最后一个分块序号; 单分块任务 = 0
	Options DeleteOptions  `msgpack:"options"`
	Entries []DeleteEntry  `msgpack:"entries"`
}

// DeleteResultEntry 对应 plan §7 DeleteReport 的 entries[]{path, ok, err}, 扩展 err_code 等审计字段。
type DeleteResultEntry struct {
	Path            string `msgpack:"path"`
	OK              bool   `msgpack:"ok"`
	ErrCode         string `msgpack:"err_code,omitempty"`
	Err             string `msgpack:"err,omitempty"`
	ReadonlyCleared bool   `msgpack:"readonly_cleared,omitempty"` // 本次是否清除了只读属性
	RecycledTo      string `msgpack:"recycled_to,omitempty"`      // 软删目标完整路径(还原映射)
}

type DeleteStats struct {
	Total  int `msgpack:"total"`
	OK     int `msgpack:"ok"`
	Failed int `msgpack:"failed"`
}

// DeleteReportMsg 一回执帧对应一任务分块(seq 一一对应)。
type DeleteReportMsg struct {
	Type    string              `msgpack:"type"` // "DeleteReport"
	TaskID  string              `msgpack:"task_id"`
	Seq     int                 `msgpack:"seq"`
	Stats   DeleteStats         `msgpack:"stats"`
	Entries []DeleteResultEntry `msgpack:"entries"`
}

// HelloMsg: Helper 接受连接后立刻下发, 用于版本/存活确认。
type HelloMsg struct {
	Type    string `msgpack:"type"` // "Hello"
	Version string `msgpack:"version"`
	PID     int    `msgpack:"pid"`
}

// ShutdownMsg: Agent 请求 Helper 处理完当前帧后退出(用于部署/升级)。
type ShutdownMsg struct {
	Type string `msgpack:"type"` // "Shutdown"
}
```

```go
// internal/proto/frame.go
// 注意: 若 M1 已实现本文件(同函数签名), M5 直接复用, 不得重复定义。
package proto

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/vmihailenco/msgpack/v5"
)

// MaxFrameSize 帧上限, 与 TCP 层一致(plan §7 帧格式)。
const MaxFrameSize = 16 << 20 // 16MB

// WriteFrame 编码 v 并写入一帧: [4B 大端长度][msgpack body]。
func WriteFrame(w *bufio.Writer, v any) error {
	body, err := msgpack.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto: marshal: %w", err)
	}
	if len(body) > MaxFrameSize {
		return fmt.Errorf("proto: frame too large: %d > %d", len(body), MaxFrameSize)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return w.Flush()
}

// ReadFrameRaw 读取一帧返回原始字节, 由调用方按 type 解码。
func ReadFrameRaw(r *bufio.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrameSize {
		return nil, fmt.Errorf("proto: bad frame length: %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// Decode 把帧体解码到具体消息结构。
func Decode(raw []byte, v any) error {
	return msgpack.Unmarshal(raw, v)
}
```

### 4.2 Helper 配置（`internal/helper/config.go` + `helper.json`）

`helper.json` 示例（与 `helper.exe` 同目录，NTFS ACL 仅管理员可写）：

```json
{
  "pipe_name": "\\\\.\\pipe\\dedup-delete",
  "allowed_roots": ["E:\\", "F:\\Media"],
  "denied_roots": ["E:\\System Volume Information", "E:\\$RECYCLE.BIN"],
  "default_mode": "soft",
  "allow_hard_delete": true,
  "recycle_dir_name": "$DedupRecycle",
  "max_entries_per_frame": 2000,
  "frame_read_timeout_sec": 120,
  "log_dir": ""
}
```

```go
// internal/helper/config.go
package helper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dedup/internal/proto"
)

const (
	DefaultPipeName       = `\\.\pipe\dedup-delete`
	DefaultRecycleDirName = `$DedupRecycle`
)

// Config 为 Helper 本地安全策略。只允许来自本地配置文件, 不接受任何网络下发。
type Config struct {
	PipeName            string   `json:"pipe_name"`             // 命名管道名
	AllowedRoots        []string `json:"allowed_roots"`         // 路径白名单根前缀(强制, 空=拒绝启动)
	DeniedRoots         []string `json:"denied_roots"`          // 拒绝前缀(优先排除, 可空)
	DefaultMode         string   `json:"default_mode"`          // 任务未带 mode 时的默认: hard|soft
	AllowHardDelete     bool     `json:"allow_hard_delete"`     // false 时拒绝一切 hard 任务
	RecycleDirName      string   `json:"recycle_dir_name"`      // 每盘根下的回收目录名
	MaxEntriesPerFrame  int      `json:"max_entries_per_frame"` // 单帧条目上限
	FrameReadTimeoutSec int      `json:"frame_read_timeout_sec"`// 管道单帧读超时
	LogDir              string   `json:"log_dir"`               // 空 = exe 目录下 logs\
}

func DefaultConfig() Config {
	return Config{
		PipeName:            DefaultPipeName,
		DefaultMode:         proto.ModeSoft,
		AllowHardDelete:     true,
		RecycleDirName:      DefaultRecycleDirName,
		MaxEntriesPerFrame:  2000,
		FrameReadTimeoutSec: 120,
	}
}

// LoadConfig 读取配置并做启动校验; 任何一项不安全即拒绝启动。
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config %s: %w", path, err)
	}
	if len(cfg.AllowedRoots) == 0 {
		return cfg, fmt.Errorf("allowed_roots 不能为空: 白名单是强制安全约束")
	}
	if cfg.DefaultMode != proto.ModeHard && cfg.DefaultMode != proto.ModeSoft {
		return cfg, fmt.Errorf("default_mode 必须是 hard 或 soft")
	}
	if cfg.MaxEntriesPerFrame <= 0 {
		cfg.MaxEntriesPerFrame = 2000
	}
	if cfg.FrameReadTimeoutSec <= 0 {
		cfg.FrameReadTimeoutSec = 120
	}
	if cfg.AllowedRoots, err = normalizeRoots(cfg.AllowedRoots); err != nil {
		return cfg, err
	}
	if cfg.DeniedRoots, err = normalizeRoots(cfg.DeniedRoots); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// normalizeRoots 统一为: 小写 + filepath.Clean + 以 "\" 结尾。
// 结尾带 "\" 后做 HasPrefix 比较天然保证目录边界(防止 E:\Media 放行 E:\Media2)。
func normalizeRoots(roots []string) ([]string, error) {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if strings.HasPrefix(r, `\\`) {
			return nil, fmt.Errorf("配置不允许 UNC/设备路径: %s", r)
		}
		if filepath.VolumeName(r) == "" {
			return nil, fmt.Errorf("配置项必须是带盘符的绝对路径: %s", r)
		}
		r = filepath.Clean(r)
		if !strings.HasSuffix(r, `\`) {
			r += `\`
		}
		out = append(out, strings.ToLower(r))
	}
	return out, nil
}
```

### 4.3 路径白名单校验（`internal/helper/validate.go`）

校验顺序固定，任一不通过即拒绝该项（不影响同块其他项）：

1. 非空、拒绝 `\\` 前缀（UNC/设备路径）、必须带盘符的绝对路径；
2. `filepath.Clean` 规范化后做白名单前缀匹配，再做拒绝前缀排除；
3. `os.Lstat`（**不用 `os.Stat`**——Stat 会跟随 junction 解析到白名单外目标，Lstat 看的是链接本身）；
4. 先拒绝 reparse point，再拒绝目录（目录 junction 的 `IsDir()` 同为 true，顺序反了会把 junction 误报为 `E_BAD_PATH`）。

```go
// internal/helper/validate.go
package helper

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"dedup/internal/proto"
)

// validate 对单条路径做全部安全校验, 返回规范化路径与文件属性。
// Helper 绝不处理未通过校验的路径。
func (c Config) validate(p string) (clean string, attrs uint32, errCode string, err error) {
	if p == "" {
		return "", 0, proto.ErrBadPath, fmt.Errorf("空路径")
	}
	// 只接受本机盘符路径: 拒绝 UNC(\\server\share)、设备路径与 \\?\ 扩展前缀。
	// Go 的 os 包对超长路径会自动加 \\?\ 前缀, 无需调用方处理。
	if strings.HasPrefix(p, `\\`) {
		return "", 0, proto.ErrBadPath, fmt.Errorf("拒绝 UNC/设备路径: %s", p)
	}
	if !filepath.IsAbs(p) || filepath.VolumeName(p) == "" {
		return "", 0, proto.ErrBadPath, fmt.Errorf("非本机绝对路径: %s", p)
	}
	clean = filepath.Clean(p)
	lower := strings.ToLower(clean)

	inAllowed := false
	for _, root := range c.AllowedRoots {
		if strings.HasPrefix(lower, root) {
			inAllowed = true
			break
		}
	}
	if !inAllowed {
		return "", 0, proto.ErrPathDenied, fmt.Errorf("路径不在白名单内: %s", p)
	}
	for _, root := range c.DeniedRoots {
		if strings.HasPrefix(lower, root) {
			return "", 0, proto.ErrPathDenied, fmt.Errorf("路径命中拒绝前缀: %s", p)
		}
	}

	// Lstat 而非 Stat: 不跟随 junction/symlink, 防止借链接逃逸白名单。
	fi, err := os.Lstat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, proto.ErrNotFound, fmt.Errorf("文件不存在: %s", p)
		}
		return "", 0, proto.ErrBadPath, fmt.Errorf("lstat %s: %w", p, err)
	}
	attrs = fi.Sys().(*syscall.Win32FileAttributeData).FileAttributes
	// reparse 检查必须先于目录检查: 目录 junction 的 IsDir 同样为 true,
	// 若先判目录会把它误报为 E_BAD_PATH 而非更准确的 E_REPARSE。
	if attrs&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", 0, proto.ErrReparse, fmt.Errorf("reparse point/junction/symlink, 拒绝处理: %s", p)
	}
	if fi.IsDir() {
		return "", 0, proto.ErrBadPath, fmt.Errorf("目标是目录, 删除组件只处理文件: %s", p)
	}
	return clean, attrs, "", nil
}
```

### 4.4 删除执行：只读处理 + 软删（`internal/helper/delete.go`）

逐项独立执行，单项失败不中断同块其余项；整块级前置检查（确认标记、模式合法性）失败则整块同码回执。

```go
// internal/helper/delete.go
package helper

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"dedup/internal/proto"
)

type Deleter struct {
	cfg Config
	log *slog.Logger
}

func NewDeleter(cfg Config, log *slog.Logger) *Deleter {
	return &Deleter{cfg: cfg, log: log}
}

// ExecuteTask 执行一个任务分块并组装回执。
func (d *Deleter) ExecuteTask(t *proto.DeleteTaskMsg) *proto.DeleteReportMsg {
	rep := &proto.DeleteReportMsg{Type: proto.MsgDeleteReport, TaskID: t.TaskID, Seq: t.Seq}
	rep.Stats.Total = len(t.Entries)

	// 二次确认标记缺失: 整块拒绝(防绕过 GUI 确认流程的意外/测试调用)
	if !t.Options.Confirmed {
		d.log.Warn("reject chunk: not confirmed", "task_id", t.TaskID, "seq", t.Seq)
		return fillAll(rep, t.Entries, proto.ErrNotConfirmed, "缺少 GUI 二次确认标记")
	}

	mode := t.Options.Mode
	if mode == "" {
		mode = d.cfg.DefaultMode
	}
	if mode != proto.ModeHard && mode != proto.ModeSoft {
		return fillAll(rep, t.Entries, proto.ErrBadMode, "未知删除模式: "+mode)
	}
	if mode == proto.ModeHard && !d.cfg.AllowHardDelete {
		return fillAll(rep, t.Entries, proto.ErrBadMode, "配置已禁用物理删除(allow_hard_delete=false)")
	}

	for _, e := range t.Entries {
		r := d.executeOne(mode, t.TaskID, e.Path)
		if r.OK {
			rep.Stats.OK++
		} else {
			rep.Stats.Failed++
		}
		rep.Entries = append(rep.Entries, r)
	}
	d.log.Info("chunk done", "task_id", t.TaskID, "seq", t.Seq, "mode", mode,
		"ok", rep.Stats.OK, "failed", rep.Stats.Failed)
	return rep
}

func fillAll(rep *proto.DeleteReportMsg, entries []proto.DeleteEntry, code, msg string) *proto.DeleteReportMsg {
	rep.Stats.Failed = rep.Stats.Total
	for _, e := range entries {
		rep.Entries = append(rep.Entries, proto.DeleteResultEntry{
			Path: e.Path, ErrCode: code, Err: msg,
		})
	}
	return rep
}

func (d *Deleter) executeOne(mode, taskID, path string) proto.DeleteResultEntry {
	res := proto.DeleteResultEntry{Path: path}
	clean, attrs, code, err := d.cfg.validate(path)
	if err != nil {
		res.ErrCode, res.Err = code, err.Error()
		if code == proto.ErrPathDenied || code == proto.ErrReparse {
			d.log.Warn("security reject", "path", path, "code", code, "err", err)
		}
		return res
	}
	if mode == proto.ModeSoft {
		return d.softDelete(res, clean, taskID)
	}
	return d.hardDelete(res, clean, attrs)
}

// hardDelete 物理删除: 只读则先清 FILE_ATTRIBUTE_READONLY(保留其余属性位)再删。
func (d *Deleter) hardDelete(res proto.DeleteResultEntry, clean string, attrs uint32) proto.DeleteResultEntry {
	if attrs&syscall.FILE_ATTRIBUTE_READONLY != 0 {
		if err := clearReadonly(clean, attrs); err != nil {
			res.ErrCode, res.Err = proto.ErrReadonly, err.Error()
			return res
		}
		res.ReadonlyCleared = true
	}
	if err := os.Remove(clean); err != nil {
		res.ErrCode, res.Err = classifyWinErr(err, proto.ErrDeleteFailed), err.Error()
		return res
	}
	res.OK = true
	return res
}

func clearReadonly(path string, attrs uint32) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return syscall.SetFileAttributes(p, attrs&^uint32(syscall.FILE_ATTRIBUTE_READONLY))
}

// softDelete 软删: 移入本盘回收目录, 不物理删除。同卷 rename, 保留原 ACL/属性。
func (d *Deleter) softDelete(res proto.DeleteResultEntry, clean, taskID string) proto.DeleteResultEntry {
	dst, err := d.recycleTarget(clean, taskID)
	if err != nil {
		res.ErrCode, res.Err = proto.ErrRecycleFailed, err.Error()
		return res
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		res.ErrCode, res.Err = proto.ErrRecycleFailed, err.Error()
		return res
	}
	if err := os.Rename(clean, dst); err != nil {
		res.ErrCode, res.Err = classifyWinErr(err, proto.ErrRecycleFailed), err.Error()
		return res
	}
	res.OK = true
	res.RecycledTo = dst
	return res
}

// recycleTarget 生成回收目标: <卷>:\$DedupRecycle\<taskID>\<原卷内相对路径>, 冲突追加 _N。
func (d *Deleter) recycleTarget(src, taskID string) (string, error) {
	vol := filepath.VolumeName(src) // 例如 "E:"
	rel := strings.TrimPrefix(src, vol+`\`)
	if rel == src {
		return "", fmt.Errorf("无法计算卷内相对路径: %s", src)
	}
	base := filepath.Join(vol+`\`, d.cfg.RecycleDirName, sanitizeTaskID(taskID), rel)
	dst := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			return dst, nil
		}
		if i > 9999 {
			return "", fmt.Errorf("回收目标命名冲突过多: %s", base)
		}
		ext := filepath.Ext(base)
		dst = strings.TrimSuffix(base, ext) + fmt.Sprintf("_%d", i) + ext
	}
}

func sanitizeTaskID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "notask"
	}
	return b.String()
}

// classifyWinErr 把 win32 错误归类为稳定错误码; 兜底返回 fallback。
func classifyWinErr(err error, fallback string) string {
	switch {
	case errors.Is(err, syscall.ERROR_FILE_NOT_FOUND), errors.Is(err, syscall.ERROR_PATH_NOT_FOUND):
		return proto.ErrNotFound
	case errors.Is(err, syscall.ERROR_SHARING_VIOLATION):
		return proto.ErrInUse
	case errors.Is(err, syscall.ERROR_ACCESS_DENIED):
		return proto.ErrAccessDenied
	default:
		return fallback
	}
}
```

### 4.5 命名管道服务端与 ACL（`internal/helper/server.go`）

管道 DACL 用 SDDL 显式构造，这是"Helper 只接受本机命名管道"的强制点：

```
D:(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;<启动用户SID>)
   └拒绝NETWORK    └允许SYSTEM   └允许Administrators   └允许启动Helper的用户(=Agent运行账户)
```

Helper 由用户经 UAC 提权启动，其进程 Token 用户与该用户（即 Agent 运行账户）相同，因此把"启动用户 SID"写入 DACL 即可同时放行普通权限的 Agent、挡住其他本地用户与一切网络登录。

```go
// internal/helper/server.go
package helper

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"dedup/internal/proto"
)

// Version Helper 版本, 随 HelloMsg 上报。
const Version = "0.5.0"

type Server struct {
	cfg Config
	log *slog.Logger
	d   *Deleter
	ln  net.Listener
}

func NewServer(cfg Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log, d: NewDeleter(cfg, log)}
}

// buildPipeSDDL 生成管道 DACL: 拒绝 NETWORK(本机only), 允许 SYSTEM/Administrators/启动用户。
func buildPipeSDDL(userSID string) string {
	return "D:(D;;GA;;;NU)(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + userSID + ")"
}

// currentUserSID 取 Helper 进程 Token 用户 SID(= 经 UAC 提权的启动用户)。
func currentUserSID() (string, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}

// Listen 创建带 ACL 的命名管道服务端。
func (s *Server) Listen() error {
	sid, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("get current user sid: %w", err)
	}
	ln, err := winio.ListenPipe(s.cfg.PipeName, &winio.PipeConfig{
		SecurityDescriptor: buildPipeSDDL(sid),
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	if err != nil {
		return fmt.Errorf("listen pipe %s: %w", s.cfg.PipeName, err)
	}
	s.ln = ln
	s.log.Info("pipe listening", "pipe", s.cfg.PipeName, "user_sid", sid,
		"allowed_roots", s.cfg.AllowedRoots, "denied_roots", s.cfg.DeniedRoots)
	return nil
}

// Run 常驻并串行处理连接；仅 Shutdown 或进程终止结束服务。
func (s *Server) Run() error {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return fmt.Errorf("accept pipe: %w", err)
		}
		if s.handleConn(c) {
			s.log.Info("shutdown requested, exit")
			return nil
		}
	}
}

// handleConn 处理一条 Agent 连接直到断开; 返回 true 表示收到 Shutdown。
func (s *Server) handleConn(c net.Conn) bool {
	defer c.Close()
	s.log.Info("agent connected", "remote", c.RemoteAddr())
	br := bufio.NewReaderSize(c, 64*1024)
	bw := bufio.NewWriterSize(c, 64*1024)
	if err := proto.WriteFrame(bw, proto.HelloMsg{
		Type: proto.MsgHello, Version: Version, PID: os.Getpid(),
	}); err != nil {
		s.log.Warn("write hello failed", "err", err)
		return false
	}
	for {
		_ = c.SetReadDeadline(time.Now().Add(time.Duration(s.cfg.FrameReadTimeoutSec) * time.Second))
		raw, err := proto.ReadFrameRaw(br)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded) {
				s.log.Warn("read frame failed", "err", err)
			}
			return false
		}
		var hdr proto.TypeHeader
		if err := proto.Decode(raw, &hdr); err != nil {
			s.log.Warn("decode frame header failed", "err", err)
			return false
		}
		switch hdr.Type {
		case proto.MsgDeleteTask:
			var t proto.DeleteTaskMsg
			if err := proto.Decode(raw, &t); err != nil {
				s.log.Warn("decode DeleteTask failed", "err", err)
				return false
			}
			if len(t.Entries) > s.cfg.MaxEntriesPerFrame {
				s.log.Warn("oversize chunk rejected", "task_id", t.TaskID, "seq", t.Seq, "n", len(t.Entries))
				rep := &proto.DeleteReportMsg{Type: proto.MsgDeleteReport, TaskID: t.TaskID, Seq: t.Seq}
				rep.Stats.Total = len(t.Entries) // 必须先于 fillAll 设置, fillAll 依赖 Total
				rep = fillAll(rep, t.Entries, proto.ErrBadPath, "分块超过 max_entries_per_frame")
				if !s.writeReport(c, bw, rep) {
					return false
				}
				continue
			}
			if !s.writeReport(c, bw, s.d.ExecuteTask(&t)) {
				return false
			}
		case proto.MsgShutdown:
			return true
		default:
			s.log.Warn("unknown frame type ignored", "type", hdr.Type)
		}
	}
}

func (s *Server) writeReport(c net.Conn, bw *bufio.Writer, rep *proto.DeleteReportMsg) bool {
	_ = c.SetWriteDeadline(time.Now().Add(60 * time.Second))
	if err := proto.WriteFrame(bw, rep); err != nil {
		s.log.Warn("write report failed", "err", err)
		return false
	}
	return true
}

func (s *Server) Close() {
	if s.ln != nil {
		_ = s.ln.Close()
	}
}
```

### 4.6 Helper 入口与手动提权启动（`cmd/helper`）

**启动方式**：`helper.exe` 内嵌 `requireAdministrator` manifest，由用户在**目标机器控制台手动启动**并确认一次 UAC，随后常驻运行。Agent 只连接已运行的 Helper，不负责启动、提权或自动重启。Helper 不注册服务、不自启。

```xml
<!-- cmd/helper/helper.manifest -->
<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity version="1.0.0.0" processorArchitecture="*" name="dedup-helper" type="win32"/>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="requireAdministrator" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
    </application>
  </compatibility>
</assembly>
```

```go
// cmd/helper/main.go
//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"gopkg.in/natefinch/lumberjack.v2"

	"dedup/internal/helper"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "helper fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	// 1. 单实例: 命名管道允许同名多服务端实例, 必须用互斥体挡住第二个 Helper。
	mutexName, err := windows.UTF16PtrFromString(`Local\DedupDeleteHelperMutex`)
	if err != nil {
		return err
	}
	h, err := windows.CreateMutex(nil, false, mutexName)
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return fmt.Errorf("helper 已在运行, 本实例退出")
		}
		return fmt.Errorf("create mutex: %w", err)
	}
	defer windows.CloseHandle(h)

	// 2. 配置(本地安全策略, 只从 exe 同目录 helper.json 读取)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exeDir := filepath.Dir(exe)
	cfg, err := helper.LoadConfig(filepath.Join(exeDir, "helper.json"))
	if err != nil {
		return err
	}

	// 3. helper.log: 只记生命周期与安全事件; 逐项审计 delete.log 在 Agent 侧(plan §8)。
	logDir := cfg.LogDir
	if logDir == "" {
		logDir = filepath.Join(exeDir, "logs")
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	logFile := &lumberjack.Logger{
		Filename:   filepath.Join(logDir, "helper.log"),
		MaxSize:    32, // MB
		MaxBackups: 5,
		MaxAge:     90, // 天
		Compress:   true,
	}
	defer logFile.Close()
	log := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stdout, logFile), nil))

	// 4. 提权自检(manifest 应已保证; 未提权只告警, 让逐项回执如实报错)
	if !windows.GetCurrentProcessToken().IsElevated() {
		log.Warn("helper 未以管理员权限运行, 删除可能因权限不足失败")
	}

	// 5. 管道服务
	srv := helper.NewServer(cfg, log)
	if err := srv.Listen(); err != nil {
		return err
	}
	defer srv.Close()
	log.Info("helper started", "version", helper.Version, "pid", os.Getpid())
	return srv.Run()
}
```

构建（`rsrc` 把 manifest 嵌入 exe；`.syso` 由 go build 自动链接）：

```bash
go install github.com/akavel/rsrc@latest
cd cmd/helper
rsrc -manifest helper.manifest -o rsrc.syso
cd ../..
go build -o bin/helper.exe ./cmd/helper
```

### 4.7 Agent 转发器（`internal/agent/delete/forwarder.go`）

职责：接收 TCP `DeleteTask` → 分块 → 检查 Helper 是否在线 → 帧转发 → 审计 + 本地库联动 → 回执转 GUI。Helper 不可达时立即返回明确错误，由用户手动启动后重试；Agent 不启动、不提权、不自动重启 Helper。`Store`/`GUIUplink` 两个接口由 M1 的 SQLite 层与 TCP 服务端实现，本文给出签名与语义。

```go
// internal/agent/delete/forwarder.go
//go:build windows

package delete

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/Microsoft/go-winio"

	"dedup/internal/proto"
)

const (
	DefaultChunkSize  = 2000               // 单帧条目上限(与 helper max_entries_per_frame 对齐)
	pipeDialTimeout   = 500 * time.Millisecond
	helloTimeout      = 5 * time.Second
	reportReadTimeout = 10 * time.Minute   // 单分块回执读超时(大块硬删足够)
)

// Store 由 M1 的 SQLite 层实现。
type Store interface {
	// MarkFilesDeleted 把删除成功的路径标记 files.status='deleted' 并写 sync_queue。
	MarkFilesDeleted(paths []string, deletedAt time.Time) error
}

// GUIUplink 由 M1 的 TCP 服务端实现: 把回执发回 GUI。
type GUIUplink interface {
	SendToGUI(v any) error
}

type Config struct {
	PipeName  string // 默认 \\.\pipe\dedup-delete
	ChunkSize int    // 默认 2000
}

type Forwarder struct {
	cfg Config
	log *slog.Logger // delete.log 专用 logger(M1 日志组件: slog JSON + lumberjack)
	st  Store
	up  GUIUplink
}

func NewForwarder(cfg Config, log *slog.Logger, st Store, up GUIUplink) *Forwarder {
	if cfg.PipeName == "" {
		cfg.PipeName = `\\.\pipe\dedup-delete`
	}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = DefaultChunkSize
	}
	return &Forwarder{cfg: cfg, log: log, st: st, up: up}
}

// pipeConn 持有连接及其缓冲(hello 帧可能预读)。
type pipeConn struct {
	net.Conn
	BR *bufio.Reader
	BW *bufio.Writer
}

// HandleDeleteTask 处理 GUI 下发的删除任务。任何故障都收敛为逐项回执, 任务绝不悬挂。
func (f *Forwarder) HandleDeleteTask(t *proto.DeleteTaskMsg) {
	chunks := chunkEntries(t.Entries, f.cfg.ChunkSize)
	lastSeq := len(chunks) - 1

	conn, err := f.ensureHelper()
	if err != nil {
		f.failRemaining(t.TaskID, 0, chunks, err)
		return
	}
	defer conn.Close()

	for seq, entries := range chunks {
		sub := &proto.DeleteTaskMsg{
			Type:    proto.MsgDeleteTask,
			TaskID:  t.TaskID,
			Seq:     seq,
			LastSeq: lastSeq,
			Options: t.Options, // mode/confirmed 原样透传
			Entries: entries,
		}
		if err := proto.WriteFrame(conn.BW, sub); err != nil {
			f.failRemaining(t.TaskID, seq, chunks, fmt.Errorf("write pipe: %w", err))
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(reportReadTimeout))
		raw, err := proto.ReadFrameRaw(conn.BR)
		if err != nil {
			f.failRemaining(t.TaskID, seq, chunks, fmt.Errorf("read pipe: %w", err))
			return
		}
		var rep proto.DeleteReportMsg
		if err := proto.Decode(raw, &rep); err != nil {
			f.failRemaining(t.TaskID, seq, chunks, fmt.Errorf("decode report: %w", err))
			return
		}
		f.deliverReport(&rep)
	}
}

// ensureHelper 只连接已由用户手动启动的 Helper。
func (f *Forwarder) ensureHelper() (*pipeConn, error) {
	conn, err := f.dial()
	if err != nil {
		return nil, fmt.Errorf(
			"Helper 未运行，请以管理员权限手动启动 helper.exe: %w", err,
		)
	}
	return conn, nil
}

// dial 连接管道并消费首帧 Hello。
func (f *Forwarder) dial() (*pipeConn, error) {
	timeout := pipeDialTimeout
	c, err := winio.DialPipe(f.cfg.PipeName, &timeout)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(c, 64*1024)
	_ = c.SetReadDeadline(time.Now().Add(helloTimeout))
	raw, err := proto.ReadFrameRaw(br)
	if err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("read hello: %w", err)
	}
	var h proto.HelloMsg
	if err := proto.Decode(raw, &h); err != nil || h.Type != proto.MsgHello {
		_ = c.Close()
		return nil, fmt.Errorf("bad hello frame")
	}
	_ = c.SetReadDeadline(time.Time{})
	f.log.Info("helper connected", "helper_version", h.Version, "helper_pid", h.PID)
	return &pipeConn{Conn: c, BR: br, BW: bufio.NewWriterSize(c, 64*1024)}, nil
}

// deliverReport 审计 + 本地库联动 + 转发 GUI。
func (f *Forwarder) deliverReport(rep *proto.DeleteReportMsg) {
	deleted := make([]string, 0, len(rep.Entries))
	for _, e := range rep.Entries {
		// delete.log 审计: 一行一次(plan §8)
		f.log.Info("delete",
			"task_id", rep.TaskID, "seq", rep.Seq,
			"path", e.Path, "ok", e.OK,
			"err_code", e.ErrCode, "err", e.Err,
			"readonly_cleared", e.ReadonlyCleared,
			"recycled_to", e.RecycledTo,
		)
		if e.OK {
			deleted = append(deleted, e.Path)
		}
	}
	if len(deleted) > 0 {
		if err := f.st.MarkFilesDeleted(deleted, time.Now()); err != nil {
			f.log.Error("mark files deleted failed", "task_id", rep.TaskID, "err", err)
		}
	}
	if err := f.up.SendToGUI(rep); err != nil {
		f.log.Error("forward report to GUI failed", "task_id", rep.TaskID, "err", err)
	}
}

// failRemaining 为 [fromSeq, lastSeq] 全部条目生成 E_HELPER_LOST 回执。
func (f *Forwarder) failRemaining(taskID string, fromSeq int, chunks [][]proto.DeleteEntry, cause error) {
	f.log.Error("helper unavailable", "task_id", taskID, "from_seq", fromSeq, "err", cause)
	for s := fromSeq; s < len(chunks); s++ {
		rep := &proto.DeleteReportMsg{Type: proto.MsgDeleteReport, TaskID: taskID, Seq: s}
		rep.Stats.Total = len(chunks[s])
		rep.Stats.Failed = len(chunks[s])
		for _, e := range chunks[s] {
			rep.Entries = append(rep.Entries, proto.DeleteResultEntry{
				Path: e.Path, ErrCode: proto.ErrHelperLost, Err: cause.Error(),
			})
		}
		f.deliverReport(rep)
	}
}

func chunkEntries(entries []proto.DeleteEntry, size int) [][]proto.DeleteEntry {
	if len(entries) == 0 {
		return [][]proto.DeleteEntry{{}} // 空任务也回一帧空回执, 便于 GUI 闭环
	}
	var out [][]proto.DeleteEntry
	for i := 0; i < len(entries); i += size {
		j := i + size
		if j > len(entries) {
			j = len(entries)
		}
		out = append(out, entries[i:j])
	}
	return out
}
```

### 4.8 GUI 接口契约与确认令牌（`internal/gui/delete.go`）

GUI 为 Web 页面（plan §3）。两次确认的第一次是"删除所选"按钮，第二次是确认弹窗的"确认删除"按钮。服务端用一次性 `confirm_token` 把"弹窗真实展示过"与 `confirmed=true` 绑定：**只有持有效 token 的 execute 请求才会在 `DeleteTask` 中置 `confirmed=true`**。

```
POST /api/delete/prepare
  请求:  { "items": [ {"agent_id":"A","paths":["E:/media/a.jpg", ...]}, ... ] }
  响应:  { "confirm_token": "9f3c…", "expires_in": 60,
           "summary": { "files": 123, "bytes": 45678901,
                        "per_agent": {"A": 100, "B": 23},
                        "sample": ["E:/media/a.jpg", ...(前 20 条)] } }

POST /api/delete/execute
  请求:  { "confirm_token": "9f3c…", "mode": "soft" }        // mode: soft|hard, 前端默认 soft
  响应:  202 { "task_id": "…", "dispatched": {"A": 100, "B": 23} }
         400 token 过期/不匹配; 409 token 已使用
```

回执推送（SSE/WebSocket，GUI 模块内部机制自定）：服务端按 `(task_id, agent_id, seq)` 聚合各 Agent 转来的 `DeleteReport`，前端展示进度条与失败清单。

```go
// internal/gui/delete.go
package gui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ConfirmEntry 一次删除中属于某台 Agent 的路径子集。
type ConfirmEntry struct {
	AgentID string   `json:"agent_id"`
	Paths   []string `json:"paths"`
}

type pendingDelete struct {
	digest string
	items  []ConfirmEntry
	expire time.Time
}

// ConfirmStore 二次确认令牌: 60s TTL、一次性、与清单内容绑定。
type ConfirmStore struct {
	mu   sync.Mutex
	ttl  time.Duration
	pend map[string]pendingDelete
}

func NewConfirmStore(ttl time.Duration) *ConfirmStore {
	return &ConfirmStore{ttl: ttl, pend: make(map[string]pendingDelete)}
}

// Prepare 生成确认令牌。digest 绑定清单内容, execute 时清单被篡改即校验失败。
func (s *ConfirmStore) Prepare(items []ConfirmEntry) (token string, err error) {
	digest, err := digestItems(items)
	if err != nil {
		return "", err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token = hex.EncodeToString(buf)
	s.mu.Lock()
	s.pend[token] = pendingDelete{digest: digest, items: items, expire: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return token, nil
}

// Consume 校验并消费令牌(单用后失效); 返回 prepare 时登记的清单。
func (s *ConfirmStore) Consume(token string) ([]ConfirmEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pend[token]
	if !ok {
		return nil, fmt.Errorf("confirm_token 无效")
	}
	delete(s.pend, token) // 一次性
	if time.Now().After(p.expire) {
		return nil, fmt.Errorf("confirm_token 已过期")
	}
	return p.items, nil
}

func digestItems(items []ConfirmEntry) (string, error) {
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
```

execute 处理流程（同一 `internal/gui/delete.go` 文件内，需额外 `import "dedup/internal/proto"`；与 M1 连接池对接处为接口）：

```go
// AgentPool 由 M1 TCP 连接池实现。
type AgentPool interface {
	Send(agentID string, v any) error
}

// execute 校验 token → 按机器拆分 → 置 confirmed=true 下发。
func executeDelete(cs *ConfirmStore, pool AgentPool, taskID, token, mode string) (map[string]int, error) {
	items, err := cs.Consume(token)
	if err != nil {
		return nil, err
	}
	if mode != "hard" && mode != "soft" {
		return nil, fmt.Errorf("mode 必须是 hard 或 soft")
	}
	dispatched := make(map[string]int, len(items))
	for _, it := range items {
		entries := make([]proto.DeleteEntry, 0, len(it.Paths))
		for _, p := range it.Paths {
			entries = append(entries, proto.DeleteEntry{Path: p})
		}
		task := &proto.DeleteTaskMsg{
			Type:    proto.MsgDeleteTask,
			TaskID:  taskID,
			Seq:     0,
			LastSeq: 0, // Agent 转发器按 ChunkSize 重新分块时会覆盖 seq/last_seq
			Options: proto.DeleteOptions{Mode: mode, Confirmed: true},
			Entries: entries,
		}
		if err := pool.Send(it.AgentID, task); err != nil {
			return dispatched, fmt.Errorf("下发到 Agent %s 失败: %w", it.AgentID, err)
		}
		dispatched[it.AgentID] = len(entries)
	}
	return dispatched, nil
}
```

> 约束：GUI 单任务条目数建议 ≤ 20000（msgpack 体约 5MB，远低于 16MB 帧上限）；更大清单由 GUI 拆成多个任务串行下发。

---

## 5. 数据模型与配置项

### 5.1 SQLite 变更（Agent 本地库，增量）

无 DDL 变更。`files` 表沿用 plan §6.1 / M1 定义，仅 `status` 取值域新增 `'deleted'`（应用层约定，与 `done/partial/failed/crash` 并列）。`MarkFilesDeleted` 语义：

```sql
-- 逐条(或批量 IN)执行, 包在一个事务里
UPDATE files SET status='deleted', updated_at=:ts
 WHERE machine_id=:mid AND path=:path;

-- 复用 M1 同步队列, 随 5min/5万行 周期上行中心库
INSERT INTO sync_queue(table_name, row_pk, synced) VALUES ('files', :pk, 0);
```

- 中心库按 `(machine_id, path)` 自然键 `ON CONFLICT UPDATE` 后，`files.status='deleted'` 在中心可见；GUI 分析与组展示一律排除该状态。
- **特征表（`image_features`/`video_features`/`video_frames`）不动**：特征以 SHA-512 为主键，同一内容的其他副本仍在使用。
- Everything 索引由系统自行更新，下轮枚举自然不再看到已删文件，无需 M5 干预。

### 5.2 Helper 配置项表（`helper.json`）

| 字段 | 默认值 | 说明 |
|---|---|---|
| `pipe_name` | `\\.\pipe\dedup-delete` | 命名管道名；Agent 转发器配置必须一致 |
| `allowed_roots` | （无，必填） | 路径白名单根前缀，空则拒绝启动。**部署时按该机实际数据盘配置** |
| `denied_roots` | `[]` | 拒绝前缀（在白名单内再排除），建议含 `System Volume Information`、`$RECYCLE.BIN` |
| `default_mode` | `soft` | 任务未显式携带 `mode` 时的默认模式；GUI 总是显式下发，此项仅为兼容兜底 |
| `allow_hard_delete` | `true` | `false` 时一切 hard 任务以 `E_BAD_MODE` 拒绝（只准软删的机器用） |
| `recycle_dir_name` | `$DedupRecycle` | 软删回收目录名，位于每盘根下 |
| `max_entries_per_frame` | `2000` | 单帧条目上限，与 Agent `ChunkSize` 对齐 |
| `frame_read_timeout_sec` | `120` | 管道单帧读超时 |
| `log_dir` | `""`（= exe 目录下 `logs\`） | `helper.log` 目录 |

### 5.3 运行参数表

plan §9 原有参数**不变**（TCP 心跳 15s 等由 M1 TCP 层负责）。M5 新增参数（全部可配）：

| 参数 | 默认值 | 位置 | 说明 |
|---|---|---|---|
| 命名管道 | `\\.\pipe\dedup-delete` | helper.json / Agent 配置 | 本机唯一通道 |
| 管道帧上限 | 16MB | `proto.MaxFrameSize` | 与 TCP 帧一致 |
| Agent 分块大小 | 2000 项/帧 | Agent `ChunkSize` | 路径按 120B 估算单帧约 500KB |
| 管道拨号超时 | 500ms | 转发器常量 | 判定 Helper 是否在线 |
| Hello 读超时 | 5s | 转发器常量 | 连接后首帧 |
| 回执读超时 | 10min | 转发器常量 | 单分块执行+回传上限 |
| 回执写超时 | 60s | Helper 常量 | 管道写 |
| `confirm_token` TTL | 60s | GUI `ConfirmStore` | 一次性、与清单绑定 |
| GUI 单任务条目建议上限 | 20000 | GUI 约定 | 超出拆成多任务 |

### 5.4 日志格式

**`delete.log`（Agent 侧，审计，一行一次，slog JSON + lumberjack，plan §8）**：

```json
{"time":"2026-07-26T15:44:01.234Z","level":"INFO","msg":"delete","task_id":"9f3c1a2e","seq":0,"path":"E:/media/a.jpg","ok":true,"err_code":"","err":"","readonly_cleared":true,"recycled_to":""}
{"time":"2026-07-26T15:44:01.241Z","level":"INFO","msg":"delete","task_id":"9f3c1a2e","seq":0,"path":"E:/media/b.jpg","ok":false,"err_code":"E_NOT_FOUND","err":"文件不存在: E:/media/b.jpg","readonly_cleared":false,"recycled_to":""}
{"time":"2026-07-26T15:44:02.007Z","level":"INFO","msg":"delete","task_id":"9f3c1a2e","seq":0,"path":"E:/media/c.mkv","ok":true,"err_code":"","err":"","readonly_cleared":false,"recycled_to":"E:\\$DedupRecycle\\9f3c1a2e\\media\\c.mkv"}
```

**`helper.log`（Helper 侧，生命周期与安全事件）**：

```json
{"time":"…","level":"INFO","msg":"helper started","version":"0.5.0","pid":8128}
{"time":"…","level":"INFO","msg":"pipe listening","pipe":"\\\\.\\pipe\\dedup-delete","user_sid":"S-1-5-21-…","allowed_roots":["e:\\"],"denied_roots":["e:\\system volume information\\"]}
{"time":"…","level":"WARN","msg":"security reject","path":"C:\\Windows\\System32\\x.dll","code":"E_PATH_DENIED","err":"路径不在白名单内: C:\\Windows\\System32\\x.dll"}
{"time":"…","level":"INFO","msg":"chunk done","task_id":"9f3c1a2e","seq":0,"mode":"soft","ok":99,"failed":1}
{"time":"…","level":"INFO","msg":"shutdown requested, exit"}
```

---

## 6. 测试与验收用例

### 6.1 环境准备

单机即可验证核心链路（GUI、Agent、Helper 同机）；TC-09 涉及手动 Helper 生命周期，TC-10 涉及跨会话或网络访问。准备：

```bat
:: 测试数据(图片/视频各若干, 内容随意)
mkdir E:\dedup-test\media
copy <样本> E:\dedup-test\media\a.jpg
copy <样本> E:\dedup-test\media\b.jpg
copy <样本> E:\dedup-test\media\c.mkv

:: TC-01 用: 只读文件
attrib +R E:\dedup-test\media\b.jpg

:: helper.json 白名单只放行测试目录
::   "allowed_roots": ["E:\\dedup-test"]
```

执行前先构建：`go build -o bin/agent.exe ./cmd/agent`、`go build -o bin/helper.exe ./cmd/helper`（manifest 已按 4.6 嵌入），GUI 以开发模式起 Web 页面。

### 6.2 用例

| # | 用例 | 步骤 | 通过标准 |
|---|---|---|---|
| TC-01 | **端到端硬删（含只读文件）** | GUI 构造含 `a.jpg`、`b.jpg`(只读) 的删除清单 → 两次确认 → mode=hard | 两文件均不存在；`DeleteReport` 全部 `ok=true`；`b.jpg` 回执 `readonly_cleared=true`；`delete.log` 两行审计齐全；SQLite `files.status='deleted'`；GUI 组内成员消失 |
| TC-02 | **不存在文件** | 清单混入一条 `E:\dedup-test\media\ghost.jpg`（不存在） | 该项 `ok=false, err_code=E_NOT_FOUND`；其余项正常删除；任务完整闭环；审计逐行正确 |
| TC-03 | **权限不足 / 文件被占用** | (a) PowerShell 持句柄：`$fs=[IO.File]::Open('E:\dedup-test\media\c.mkv','Open','Read','None')` 后删除该文件；(b) `icacls E:\dedup-test\media\d.jpg /deny Administrators:(D)` 后删除（文件非只读） | (a) `err_code=E_IN_USE`；(b) `err_code=E_ACCESS_DENIED`（或清只读失败时 `E_READONLY`）；其余项不受影响；释放句柄 / `icacls /remove:d` 恢复后重删成功 |
| TC-04 | **软删模式** | 两次确认时 mode=soft，删 `a.jpg`、`c.mkv` | 源路径消失；`E:\$DedupRecycle\<task_id>\media\a.jpg`、`...\c.mkv` 存在且内容一致（哈希校验）；回执 `recycled_to` 指向实际位置；同名冲突时追加 `_1` 后缀；`delete.log` 含映射 |
| TC-05 | **白名单越权拒绝** | 清单含 `C:\Windows\notepad.exe`、白名单外盘符路径、白名单内但命中 `denied_roots` 的路径各一 | 全部 `err_code=E_PATH_DENIED`；目标文件完好；`helper.log` 有 `security reject` 记录 |
| TC-06 | **缺少二次确认标记** | 绕过 GUI，直接用脚本向管道发 `DeleteTask{confirmed:false}`（以 Agent 账户本机发送） | 整块 `err_code=E_NOT_CONFIRMED`；无任何文件被删；`helper.log` 有拒绝记录 |
| TC-07 | **reparse point 拒绝** | `mklink /J E:\dedup-test\link C:\Windows` 后清单含 `E:\dedup-test\link` 及白名单内正常文件 | junction 项 `err_code=E_REPARSE`；junction 本身与其指向目标均完好；正常文件删除成功 |
| TC-08 | **大批量分块** | 构造 5000 个文件的清单一次性下发 | Agent 拆为 3 帧（2000+2000+1000，`last_seq=2`）；全部回执收齐、`seq` 与请求一一对应；无帧超 16MB；GUI 进度按分块推进 |
| TC-09 | **Helper 未运行时失败闭合** | 确认无 helper.exe 进程 → 发起删除 → 用户以管理员权限手动启动 Helper → 重试同一操作 | 首次全部条目 `err_code=E_HELPER_LOST`，`err` 明确提示手动启动 Helper，且 Agent 不弹 UAC、不启动 Helper；手动启动后重试成功 |
| TC-10 | **管道 ACL** | (a) 另一本地用户会话中 `helper.exe`-client 模拟拨管（或普通进程用非授权账户 `DialPipe`）；(b) 从网络邻居访问 `\\<机器名>\pipe\dedup-delete` | (a) 拒绝访问（Access Denied）；(b) 拒绝（NETWORK deny ACE）；合法 Agent 不受影响 |
| TC-11 | **常驻与 Shutdown 退出** | 手动启动 Helper，完成任务后保持空闲，再发送 `Shutdown` | 空闲期间 Helper 与管道持续存在；收到 `Shutdown` 后 Helper 退出、管道消失；Agent 不触发自动重启 |
| TC-12 | **任务中 Helper 死亡** | 大任务执行中 `taskkill /F /IM helper.exe` | 当前及剩余分块全部 `E_HELPER_LOST` 回执，任务闭环不悬挂；Agent 写 `delete.log` 并存活；后续任务继续明确失败且不自动重放，直到用户手动重启 Helper 后新任务成功 |

### 6.3 里程碑验收映射

plan §10 M5 验收标准"**勾选删除端到端走通，只读文件可删**"由 TC-01 直接覆盖；TC-02/03/04/05/06 为必过回归项（对应"不存在文件 / 权限不足 / 软删模式 / 安全约束"四类基本盘）；TC-07~TC-12 为健壮性项，首次交付建议全过。

---

## 7. 风险与注意事项

| 风险 / 注意点 | 说明与缓解 |
|---|---|
| Helper 手动提权启动 | 用户需在目标机器控制台手动启动 Helper 并确认 UAC；Agent 不弹 UAC、不启动或自动重启 Helper。建议在开始删除任务前启动并保持常驻 |
| 管理员进程常驻 | Helper 串行处理并持续监听，仅收到 `Shutdown` 或进程被终止时退出；不注册服务、不开机自启 |
| `helper.json` 被普通用户篡改 = 白名单失守 | 部署清单必须含 NTFS ACL（仅 Administrators 可写）；Helper 启动时把生效白名单写入 `helper.log` 便于审计核对 |
| 白名单配置过宽（如整盘 `C:\`） | 启动校验只保证非空，语义宽度靠部署审查；文档与示例均引导只放行数据盘/数据目录；`denied_roots` 作为第二道闸 |
| 确认标记不是密码学边界 | `confirmed` 防的是"意外/测试调用绕过确认流程"；真正的边界是管道 DACL（仅本机指定用户）+ 白名单 + 本地配置文件。同账户恶意进程理论上可伪造清单——接受此残余风险（与同账户删除文件能力等价） |
| 回收目录膨胀 | 软删只移不清，磁盘占用不变；`delete.log` 的 `recycled_to` 映射支持人工还原或清理；自动清理策略留待后续版本 |
| 回收目录内文件名冲突 | 同任务按"原卷内相对路径"落位天然隔离目录；同名追加 `_N`；跨任务以 `task_id` 子目录隔离 |
| 长路径 | Go `os` 包自动加 `\\?\` 前缀处理 >260 字符路径；输入校验拒绝调用方自带 `\\?\` 前缀的路径，保证白名单比较发生在规范化常规路径上 |
| 中心库滞后 | `status='deleted'` 依赖 5min 同步周期上行；同步前 GUI 组展示可能仍含已删文件——可接受，GUI 收到 `DeleteReport` 即可先在界面临时标记 |
| msgpack 库选型一致性 | 本文代码用 `github.com/vmihailenco/msgpack/v5`；若 M1 已定其他实现，M5 跟随 M1，消息字段名/语义不变 |
| Agent 删除期间被扫描任务引用同一文件 | 删除与扫描并发时，Worker 读到的文件可能随即消失：按 M1/M2 既有"坏文件原则"处理（读失败 → `errors.log` 一行 → 下轮补算），M5 不做额外加锁 |

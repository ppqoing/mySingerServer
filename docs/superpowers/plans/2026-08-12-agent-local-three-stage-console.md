# Agent 本机三筛控制台 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Compute 便携包在没有 Manager、没有 PostgreSQL 时，也能通过 NodeTray 本机 Web 界面完成扫描、一筛、二筛、三筛、结果审核和受控删除；配置 PostgreSQL 后，本机结果异步同步到中心库。

**Architecture:** `agent.exe` 继续独占 SQLite、分析任务和 Worker 调度，`nodetray.exe` 只通过 Agent TCP Socket 调用本机业务接口并承载 Wails/Web UI。Manager 继续使用同一 Socket 下发远程任务；Agent 控制命名管道彻底移除，Helper 的安全删除命名管道保留。所有本机分析按 generation 原子发布，PostgreSQL 通过 SQLite outbox 幂等同步。

**Tech Stack:** Go、TCP + msgpack、自带 SQLite（modernc.org/sqlite）、PostgreSQL 16（pgx）、React + TypeScript + Wails、PowerShell 7 便携发布脚本。

## Global Constraints

- 所有行为变更严格执行 RED → GREEN → 回归 → 小提交；不要在一个提交中混入相邻任务。
- 不直接修改或清理现有未跟踪目录 `.codex-temp/`，不使用 `git add -A`。
- `agent.exe` 是 `agent.db` 的唯一所有者；NodeTray 和前端不得打开 SQLite。
- NodeTray 只能管理当前机器；本机查询必须固定 `machine_id` 和 `scope=local`。
- PostgreSQL 不可用不得阻止 Agent 监听、本机界面、扫描、分析、审核或删除。
- 图片扫描不生成缩略图；图片审核预览只能按文件 ID 请求、内存缩放、不得落盘。
- Manager 现有扫描与旧合并式 `Phase2Task` 保持兼容；新版二筛和三筛可分别下发。
- Agent 控制命名管道必须完全移除；`nodectl` 中 Helper 管道继续使用。
- 只有 Helper 明确报告物理删除成功时才标记 `deleted`。失败或不确定结果不得修改文件状态。
- 文件标记 `deleted` 后必须保留 SHA-512、图片/视频特征、哈希索引关联、分阶段分数、重复组历史、审核决定和删除审计。
- 默认候选和当前分组查询排除 `deleted`，历史与审计查询仍能通过文件 ID、SHA-512 关联原记录。
- 便携发布仍为 ZIP；不生成 MSI 或其他安装包。

---

## Task 1：扩展兼容的 Socket 协议

**Files:**

- Modify: `internal/proto/message.go`
- Modify: `internal/proto/message_test.go`
- Create: `internal/proto/local.go`
- Create: `internal/proto/local_test.go`

- [ ] **Step 1: 写协议 RED 测试**

  增加并先运行以下测试：

  - `TestDecodeClientAuthAndLocalEnvelope`
  - `TestPhase2TaskStageTwoAcceptsOnlyPHashFields`
  - `TestPhase2TaskStageThreeAcceptsOnlySobelFields`
  - `TestLegacyPhase2TaskStillAcceptsCombinedFields`
  - `TestLocalEnvelopeRejectsOversizedPayloadAndUnknownOperation`

  固定协议增量：

  ```go
  const (
      MsgClientAuth       uint8 = 5
      MsgClientAuthResult uint8 = 6
      MsgLocalRequest     uint8 = 30
      MsgLocalResponse    uint8 = 31
      MsgLocalEvent       uint8 = 32
  )

  const (
      ScreenStageLegacy uint8 = 0
      ScreenStageTwo    uint8 = 2
      ScreenStageThree  uint8 = 3
  )

  const (
      FieldVideo6FPHash uint32 = 1 << 8
      FieldVideo6FSobel uint32 = 1 << 9
  )

  type Phase2Task struct {
      TaskID string       `msgpack:"task_id"`
      Stage  uint8        `msgpack:"stage,omitempty"`
      Items  []Phase2Item `msgpack:"items"`
  }
  ```

  本机信封使用稳定操作名和请求 ID：

  ```go
  type ClientAuth struct {
      Role    string `msgpack:"role"`
      Token   string `msgpack:"token"`
      Version int    `msgpack:"version"`
  }

  type LocalRequest struct {
      RequestID string `msgpack:"request_id"`
      Operation string `msgpack:"operation"`
      Payload   []byte `msgpack:"payload,omitempty"`
  }

  type LocalResponse struct {
      RequestID string `msgpack:"request_id"`
      OK        bool   `msgpack:"ok"`
      ErrorCode string `msgpack:"error_code,omitempty"`
      Payload   []byte `msgpack:"payload,omitempty"`
  }
  ```

  命令常量至少固定 `local.status.get`、`local.config.get`、`local.config.validate`、`local.config.save`、`local.task.create`、`local.task.list`、`local.task.cancel`、`local.task.retry`、`local.analysis.start`、`local.analysis.status`、`local.groups.list`、`local.groups.detail`、`local.review.save`、`local.delete.prepare`、`local.delete.execute`、`local.delete.status`、`local.preview.image`、`local.shutdown`。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/proto -run 'ClientAuth|LocalEnvelope|Phase2TaskStage|LegacyPhase2'`

  Expected: FAIL，原因是新消息、阶段和字段掩码尚未定义。

- [ ] **Step 3: 实现最小协议和严格校验**

  - 保留 `ProtocolVersion=1`，只追加消息和字段，不复用旧编号。
  - `Stage=0` 继续接受原图片 `FieldPHashParts|FieldSobelHist` 和视频 `FieldVideo6F`。
  - `Stage=2` 图片只接受 `FieldPHashParts`，视频只接受 `FieldVideo6FPHash`。
  - `Stage=3` 图片只接受 `FieldSobelHist`，视频只接受 `FieldVideo6FSobel`。
  - 本机信封 payload 上限固定为 4 MiB；未知操作返回稳定错误码 `unsupported_operation`。
  - `Decode` 覆盖全部新增消息。

- [ ] **Step 4: 运行 GREEN 和协议全包回归**

  Run: `go test -count=1 ./internal/proto`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/proto/message.go internal/proto/message_test.go internal/proto/local.go internal/proto/local_test.go
  git diff --cached --check
  git commit -m "feat: add staged local agent protocol"
  ```

---

## Task 2：创建受保护的本机控制令牌并认证 NodeTray

**Files:**

- Create: `internal/localcontrol/token.go`
- Create: `internal/localcontrol/token_windows.go`
- Create: `internal/localcontrol/token_stub.go`
- Create: `internal/localcontrol/token_test.go`
- Create: `internal/localcontrol/token_windows_test.go`
- Modify: `internal/agent/server.go`
- Modify: `internal/agent/server_test.go`

- [ ] **Step 1: 写令牌和鉴权 RED 测试**

  覆盖：

  - 首次创建 32 字节随机令牌并用 base64url 编码；并发首次创建最终读取同一令牌。
  - Windows 文件 DACL 受保护，只允许当前用户、Administrators 和 SYSTEM；不得继承 Everyone/Users 写权限。
  - 错误令牌使用恒定时间比较并返回 `unauthorized`。
  - 非回环来源即使令牌正确也返回 `local_only`。
  - 未认证 Manager 连接仍能提交原有扫描/Phase2 消息，但不能发送 `MsgLocalRequest` 或 `MsgShutdown`。
  - 令牌不出现在日志、错误正文或 `LocalResponse`。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/localcontrol ./internal/agent -run 'Token|ClientAuth|LocalOnly|ManagerCompatibility|ShutdownAuthorization'`

  Expected: FAIL，原因是令牌存储和连接角色状态不存在。

- [ ] **Step 3: 实现令牌文件和 Agent 会话状态**

  使用如下边界：

  ```go
  type TokenStore interface {
      LoadOrCreate(path string) (string, error)
  }

  type LocalHandler interface {
      HandleLocal(context.Context, proto.LocalRequest) proto.LocalResponse
  }

  func (s *Server) SetLocalControl(token string, handler LocalHandler)
  ```

  - 令牌路径由便携数据根解析为 `data/local-control.token`，不写入配置模板或命令行。
  - Windows 创建时先建立受保护安全描述符，再写入内容；原子替换也必须重新应用同一 DACL。
  - `handleConn` 在发送 `Hello` 后允许 `MsgClientAuth`；连接会话保存 `role=nodetray` 状态。
  - 本机请求必须同时满足回环地址、角色、协议版本和令牌。
  - 未注入业务 handler 时返回 `local_unavailable`，但 Agent 继续服务 Manager。

- [ ] **Step 4: 运行 GREEN、Windows ACL 和 Agent 回归**

  Run: `go test -count=1 ./internal/localcontrol ./internal/agent`

  Run: `$env:GOOS='windows'; $env:GOARCH='amd64'; go test -count=1 ./internal/localcontrol ./internal/agent; Remove-Item Env:GOOS,Env:GOARCH`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/localcontrol internal/agent/server.go internal/agent/server_test.go
  git diff --cached --check
  git commit -m "feat: authenticate local nodetray socket sessions"
  ```

---

## Task 3：用 Socket 替换 Agent 控制命名管道

**Files:**

- Create: `internal/nodetray/agentclient/client.go`
- Create: `internal/nodetray/agentclient/client_test.go`
- Create: `internal/nodetray/agentclient/controller.go`
- Create: `internal/nodetray/agentclient/controller_test.go`
- Create: `internal/agentinstance/singleinstance_windows.go`
- Create: `internal/agentinstance/singleinstance_stub.go`
- Modify: `internal/nodetray/production/adapters.go`
- Modify: `internal/nodetray/production/adapters_test.go`
- Modify: `internal/nodectl/pipe_windows.go`
- Modify: `internal/nodectl/pipe_windows_test.go`
- Modify: `internal/nodectl/pipe_stub.go`
- Modify: `internal/nodectl/pipe_stub_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`
- Delete: `internal/agentcontrol/service.go`
- Delete: `internal/agentcontrol/service_test.go`
- Delete: `internal/agentcontrol/provider.go`
- Delete: `internal/agentcontrol/provider_test.go`
- Delete: `internal/agentcontrol/singleinstance_windows.go`
- Delete: `internal/agentcontrol/singleinstance_stub.go`

- [ ] **Step 1: 写 Socket 客户端和“无 Agent 管道”RED 测试**

  客户端合同：

  ```go
  type Client struct { /* 单 readLoop、请求表、串行写 */ }

  func Dial(ctx context.Context, endpoint, token, machineID string) (*Client, error)
  func (c *Client) Call(ctx context.Context, operation string, request, response any) error
  func (c *Client) Close() error
  ```

  测试必须证明：

  - 客户端先读取 `Hello`，校验 machine ID，再发送 `ClientAuth`。
  - 并发 `Call` 由 request ID 正确配对；只有一个协程调用 `ReadFrame`。
  - 连接中断时所有未完成调用收到 `agent_disconnected`，重连后可再次查询。
  - `NewAgentController` 使用 `127.0.0.1:<agent-port>`，不使用 `nodectl.AgentPipeName()`。
  - `nodectl` 只剩 `HelperPipeName()`；Helper controller 回归仍通过。
  - Agent 单实例锁迁移到 `agentinstance` 后语义不变。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/nodetray/agentclient ./internal/nodetray/production ./internal/nodectl ./cmd/agent -run 'AgentClient|AgentController|HelperPipe|SingleInstance|NoAgentPipe'`

  Expected: FAIL，原因是 NodeTray 仍拨 Agent 命名管道。

- [ ] **Step 3: 实现 TCP 客户端和生命周期最小命令**

  - 将 Agent 状态、Worker 状态、Agent 配置读取/校验/保存和 `local.shutdown` 接入 Task 2 的本机 handler；配置保存由 Agent 自己原子写入并返回是否需要重启。
  - NodeTray 从已解析 Agent 配置取监听端口，但连接地址固定改为回环；配置为 `0.0.0.0:9101` 时拨 `127.0.0.1:9101`。
  - 保留 NodeTray 已有 PID、启动时间、最终 EXE 路径可信认领；Socket 优雅停止失败后才能使用该认领做强制停止。
  - 移除 Agent control service 的启动/等待/关闭组合；Helper 管道代码不改协议。
  - 日志不输出控制令牌。

- [ ] **Step 4: 运行 GREEN 和相关回归**

  Run: `go test -count=1 ./internal/nodetray/agentclient ./internal/nodetray/production ./internal/nodectl ./cmd/agent`

  Run: `go test -count=1 ./nodetray ./internal/nodetray/...`

  Expected: PASS，并且 `rg -n "AgentPipeName|agentcontrol" cmd internal` 没有结果。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- cmd/agent internal/agentcontrol internal/agentinstance internal/nodectl internal/nodetray/agentclient internal/nodetray/production
  git diff --cached --check
  git commit -m "refactor: move agent control to tcp socket"
  ```

---

## Task 4：增加本机任务、三筛、审核、删除和 outbox 的 SQLite 模型

**Files:**

- Modify: `internal/store/ddl.go`
- Modify: `internal/store/db.go`
- Create: `internal/store/local_tasks.go`
- Create: `internal/store/local_tasks_test.go`
- Create: `internal/store/local_analysis.go`
- Create: `internal/store/local_analysis_test.go`
- Create: `internal/store/local_review.go`
- Create: `internal/store/local_review_test.go`
- Create: `internal/store/local_outbox.go`
- Create: `internal/store/local_outbox_test.go`

- [ ] **Step 1: 写迁移和仓储 RED 测试**

  固定表和关键约束：

  - `local_tasks`：任务 ID、来源、类型、阶段、状态、信封摘要、进度、统计、安全错误、时间。
  - `local_analysis_runs`：`machine_id`、generation、状态、发布时间。
  - `local_pair_scores`：一筛、二筛、三筛 JSON、最终 verdict、两端 SHA/文件 ID。
  - `local_dup_groups`、`local_dup_members`：run/generation 下的本机分组历史。
  - `local_current_analysis`：每个 machine ID 只指向一个已完整发布 run。
  - `local_reviews`：明确的 keep/delete/undecided 决定。
  - `local_delete_batches`、`local_delete_items`：确认摘要、执行结果、错误码、uncertain 和审计时间。
  - `local_outbox`：自增 sequence、topic、entity key、generation、payload、ack 时间和重试信息。

  测试必须证明：

  - 新库和旧库升级得到相同 schema version。
  - 同一 task ID + 同一信封幂等；同一 task ID + 不同信封返回 `task_conflict`。
  - running 任务重启后进入 `waiting_recovery`，终态任务不变。
  - 发布新 generation 在一个事务中切换 current；注入失败后旧 current 不变。
  - 本机当前查询自动带 `machine_id`，不会读出另一机器的结果。
  - outbox 的 `(topic, entity_key, generation)` 幂等。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/store -run 'LocalTask|LocalAnalysis|LocalReview|LocalOutbox|Migration'`

  Expected: FAIL，原因是本机表和仓储不存在。

- [ ] **Step 3: 实现 schema 与窄接口**

  主要接口固定为：

  ```go
  func (d *DB) CreateOrLoadLocalTask(ctx context.Context, in LocalTaskCreate) (LocalTask, error)
  func (d *DB) RecoverLocalTasks(ctx context.Context, machineID string) ([]LocalTask, error)
  func (d *DB) BeginLocalAnalysis(ctx context.Context, machineID, taskID string) (LocalAnalysisRun, error)
  func (d *DB) PublishLocalAnalysis(ctx context.Context, runID string) error
  func (d *DB) SaveLocalReview(ctx context.Context, review LocalReview) error
  func (d *DB) EnqueueLocalEvent(ctx context.Context, event LocalOutboxEvent) error
  ```

  DDL 使用显式 `CHECK`、外键和索引；查询分页必须有稳定顺序和最大 page size。

- [ ] **Step 4: 运行 GREEN 和 Store 全包回归**

  Run: `go test -count=1 ./internal/store`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/store
  git diff --cached --check
  git commit -m "feat: persist local dedup workflow in sqlite"
  ```

---

## Task 5：扫描默认生成完整一筛基础特征，图片不生成缩略图

**Files:**

- Modify: `internal/agent/scan.go`
- Modify: `internal/agent/scan_test.go`
- Modify: `internal/agent/pool_router.go`
- Modify: `internal/agent/pool_router_test.go`
- Modify: `internal/worker/messages.go`
- Modify: `internal/worker/messages_test.go`
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/main_test.go`
- Modify: `internal/store/mask.go`
- Modify: `internal/store/mask_test.go`

- [ ] **Step 1: 写基础特征合同 RED 测试**

  测试固定：

  - 所有扫描文件默认请求 SHA-512。
  - 图片默认请求 PDQ-256、quality、width、height，不请求 `FieldThumb`，结果 `ThumbPath` 必须为空。
  - 视频默认请求 duration、contact sheet/thumbnail、thumbnail PDQ、quality 和尺寸。
  - 必需字段缺失时状态为 partial，不能设置一筛 ready。
  - 文件未变化且特征完整时复用；`Rescan=true` 强制重算。
  - 已删除文件恢复到原路径后，重新枚举会进入 pending 并重新核验，而不是沿用 deleted。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/agent ./internal/worker ./internal/store ./cmd/worker -run 'DefaultStageOne|ImageNoThumbnail|VideoBaseFeatures|RestoredDeleted|Rescan'`

  Expected: FAIL，至少图片掩码和恢复 deleted 的合同不满足。

- [ ] **Step 3: 实现固定的一筛掩码与缓存规则**

  - 将“一筛必需字段”集中到一个纯函数，UI 和协议不能关闭这些字段。
  - 图片解码只返回 PDQ、质量和尺寸；不创建缩略图路径。
  - 视频沿用受控 thumbnail/contact-sheet 缓存。
  - `UpsertEnumerated` 对 `status=deleted` 的同路径新实体始终清空旧完成标志并重新计算，但保留旧 SHA/特征历史供审计，直到新身份完成后再建立当前关联。

- [ ] **Step 4: 运行 GREEN 和扫描回归**

  Run: `go test -count=1 ./internal/agent ./internal/worker ./internal/store ./cmd/worker`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/agent/scan.go internal/agent/scan_test.go internal/agent/pool_router.go internal/agent/pool_router_test.go internal/worker/messages.go internal/worker/messages_test.go cmd/worker internal/store/mask.go internal/store/mask_test.go internal/store/files.go internal/store/store_test.go
  git diff --cached --check
  git commit -m "feat: compute required first-screen features by default"
  ```

---

## Task 6：让 Worker 分别计算二筛和三筛特征

**Files:**

- Modify: `internal/agent/phase2.go`
- Modify: `internal/agent/phase2_test.go`
- Modify: `internal/agent/phase2_loopback_test.go`
- Modify: `internal/worker/messages.go`
- Modify: `internal/worker/messages_test.go`
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/main_test.go`
- Modify: `internal/store/features.go`
- Modify: `internal/store/features_test.go`

- [ ] **Step 1: 写分阶段计算 RED 测试**

  覆盖：

  - 图片 Stage 2 只运行 3×3 pHash，不运行 Sobel。
  - 图片 Stage 3 只运行 Sobel，不重复运行 pHash。
  - 视频 Stage 2 的 6 帧只返回 `PHashParts`；Stage 3 的 6 帧只返回 `SobelHist`。
  - Stage 2 不通过的本地候选不会产生 Stage 3 Worker job。
  - Manager 可分别发送 Stage 2 和 Stage 3，并收到对应 `FieldsDone`。
  - 旧 `Stage=0` 合并任务仍生成原有完整结果。
  - 请求前后都核验 machine ID、SHA-512、size、mtime；变化时返回 `stale`。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/agent ./internal/worker ./cmd/worker ./internal/store -run 'StageTwo|StageThree|LegacyCombined|StaleIdentity|VideoSixFrame'`

  Expected: FAIL，原因是现有视频 `FieldVideo6F` 不能区分 pHash 与 Sobel。

- [ ] **Step 3: 实现按阶段路由和特征合并**

  - Worker job 明确携带 stage 和字段掩码。
  - SQLite 更新使用 `COALESCE`/按字段合并，Stage 2 写入不能清空 Sobel，Stage 3 写入不能清空 pHash。
  - `Phase2Manager` 对相同 task ID + 信封幂等，对信封冲突拒绝。
  - Manager 来源和 Local 来源进入同一 Worker 池，来源字段必须进入日志统计但不得包含敏感路径全文。

- [ ] **Step 4: 运行 GREEN 和计算回归**

  Run: `go test -count=1 ./internal/agent ./internal/worker ./cmd/worker ./internal/store`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/agent/phase2.go internal/agent/phase2_test.go internal/agent/phase2_loopback_test.go internal/worker/messages.go internal/worker/messages_test.go cmd/worker internal/store/features.go internal/store/features_test.go
  git diff --cached --check
  git commit -m "feat: split second and third screen computation"
  ```

---

## Task 7：复用一筛算法生成严格本机候选

**Files:**

- Modify: `internal/firstscreen/analyzer.go`
- Modify: `internal/firstscreen/analyzer_test.go`
- Create: `internal/firstscreen/source.go`
- Create: `internal/firstscreen/local_test.go`
- Create: `internal/store/firstscreen.go`
- Create: `internal/store/firstscreen_test.go`
- Create: `internal/localanalysis/stage1.go`
- Create: `internal/localanalysis/stage1_test.go`

- [ ] **Step 1: 写存储无关一筛 RED 测试**

  定义存储边界：

  ```go
  type CandidateSource interface {
      StreamActiveFiles(context.Context, string, func(firstscreen.File) error) error
      LoadImageFeatures(context.Context, []string) (map[string]firstscreen.ImageFeature, error)
      LoadVideoFeatures(context.Context, []string) (map[string]firstscreen.VideoFeature, error)
  }

  type CandidateSink interface {
      ReplaceStageOne(context.Context, string, firstscreen.Result) error
  }
  ```

  测试证明：

  - 精确 SHA 组直接 verdict=`yes`。
  - 图片候选使用 PDQ 倒排、长宽比、质量和 Hamming。
  - 视频候选使用 duration window、thumb PDQ 和 Hamming。
  - 不同 machine ID 永远不组成本机候选。
  - `status=deleted` 行不会进入新候选，但历史 generation 仍可查询。
  - 现有 PostgreSQL Analyzer 的输出不变。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/firstscreen ./internal/store ./internal/localanalysis -run 'Local|CandidateSource|DeletedExcluded|ExactVerdict'`

  Expected: FAIL，原因是 `firstscreen` 仍绑定 PostgreSQL store。

- [ ] **Step 3: 提取纯算法边界并接入 SQLite**

  - 保留现有 PostgreSQL constructor；新增以接口注入的 constructor。
  - `store.DB` 的 active 查询强制 `machine_id=? AND status!='deleted'`。
  - 一筛结果写入 Task 4 创建的 run/generation，不直接切换 current。

- [ ] **Step 4: 运行 GREEN 和一筛全包回归**

  Run: `go test -count=1 ./internal/firstscreen ./internal/store ./internal/localanalysis`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/firstscreen internal/store/firstscreen.go internal/store/firstscreen_test.go internal/localanalysis
  git diff --cached --check
  git commit -m "feat: generate local first-screen candidates"
  ```

---

## Task 8：执行二筛、三筛并原子发布本机重复组

**Files:**

- Modify: `internal/phase2/judge.go`
- Modify: `internal/phase2/judge_test.go`
- Modify: `internal/phase2/groups.go`
- Modify: `internal/phase2/groups_test.go`
- Create: `internal/localanalysis/engine.go`
- Create: `internal/localanalysis/engine_test.go`
- Create: `internal/localanalysis/groups.go`
- Create: `internal/localanalysis/groups_test.go`

- [ ] **Step 1: 写三筛编排 RED 测试**

  把现有合并判定拆成可组合纯函数：

  ```go
  func JudgeImageStage2(a, b []byte, cfg Config) StageScore
  func JudgeImageStage3(a, b []byte, cfg Config) StageScore
  func JudgeVideoStage2(a, b []proto.FrameFeature, cfg Config) StageScore
  func JudgeVideoStage3(a, b []proto.FrameFeature, cfg Config) StageScore
  ```

  固定默认值：二筛每区 Hamming `<=10`、通过比例 `>=0.80`；图片三筛 Sobel cosine `>=0.85`；视频 6 帧、至少 4 帧有效，平均 `>=0.80` 或至少 4 帧通过。

  测试证明：

  - Stage 2 fail 的 pair 不调度 Stage 3。
  - Stage 3 输出 `yes/no/inconclusive`，原因稳定可展示。
  - 只有 exact 和 `yes` 进入当前重复组。
  - 分析失败、取消或 publish 注入失败时，旧 current generation 继续可见。
  - union-find 输入顺序变化时组 ID/代表项选择仍确定。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/phase2 ./internal/localanalysis -run 'Stage2|Stage3|Inconclusive|AtomicPublish|DeterministicGroup'`

  Expected: FAIL，原因是当前 Judge 一次完成 pHash 和 Sobel，且本机编排器不存在。

- [ ] **Step 3: 实现兼容 wrapper 与本机 Engine**

  - 现有 `JudgeImagePair`/`JudgeVideoPair` 组合调用新函数，保持 Manager 行为。
  - `Engine.Run(taskID)` 顺序执行候选 → Stage 2 → Stage 3 → 分组 → `PublishLocalAnalysis`。
  - 特征计算通过 Task 6 的 Worker 抽象；不得在 Agent 内加载 native media DLL。
  - 每阶段落统计并写 Task 4 outbox；中途停止时不发布半成品。

- [ ] **Step 4: 运行 GREEN 和算法回归**

  Run: `go test -count=1 ./internal/phase2 ./internal/localanalysis ./internal/firstscreen`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/phase2/judge.go internal/phase2/judge_test.go internal/phase2/groups.go internal/phase2/groups_test.go internal/localanalysis
  git diff --cached --check
  git commit -m "feat: run and publish local three-stage analysis"
  ```

---

## Task 9：增加可恢复本机任务与公平调度

**Files:**

- Create: `internal/localtask/service.go`
- Create: `internal/localtask/service_test.go`
- Create: `internal/localtask/scheduler.go`
- Create: `internal/localtask/scheduler_test.go`
- Create: `internal/agent/local_handler.go`
- Create: `internal/agent/local_handler_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`

- [ ] **Step 1: 写任务生命周期 RED 测试**

  固定服务接口：

  ```go
  type Service interface {
      Create(context.Context, CreateRequest) (Task, error)
      List(context.Context, ListRequest) (Page[Task], error)
      Cancel(context.Context, string) error
      Retry(context.Context, string) (Task, error)
      Resume(context.Context) error
  }
  ```

  测试覆盖：

  - 本机可创建“只扫描”和“扫描后自动三筛”任务。
  - 相同 request/task ID 幂等，信封冲突拒绝。
  - Agent 重启恢复 `waiting_recovery`，从已完成阶段继续。
  - local/manager 与 scan/stage2/stage3 按来源+阶段轮转；任一来源不能长期占满 Worker。
  - 状态和 Socket heartbeat 在大量任务下仍能及时响应。
  - 连接断开不取消已受理任务。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/localtask ./internal/agent ./cmd/agent -run 'LocalTask|Recovery|FairScheduler|DisconnectDoesNotCancel'`

  Expected: FAIL，原因是本机任务服务与调度器不存在。

- [ ] **Step 3: 实现服务并接入 Agent 本机命令**

  - `local.task.*` 与 `local.analysis.*` 只接受 Task 2 已认证的 NodeTray 会话。
  - `cmd/agent` 在监听 Socket 前完成 SQLite migration 和任务恢复注册，但恢复执行在监听成功后异步开始。
  - PostgreSQL 初始化失败只更新同步健康状态，不影响本机 task service。

- [ ] **Step 4: 运行 GREEN 和 Agent 回归**

  Run: `go test -count=1 ./internal/localtask ./internal/agent ./cmd/agent`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/localtask internal/agent/local_handler.go internal/agent/local_handler_test.go cmd/agent
  git diff --cached --check
  git commit -m "feat: schedule recoverable local agent tasks"
  ```

---

## Task 10：增加本机结果查询、审核和不落盘图片预览

**Files:**

- Create: `internal/localreview/service.go`
- Create: `internal/localreview/service_test.go`
- Create: `internal/localpreview/service.go`
- Create: `internal/localpreview/service_test.go`
- Modify: `internal/worker/messages.go`
- Modify: `internal/worker/messages_test.go`
- Modify: `cmd/worker/main.go`
- Modify: `cmd/worker/main_test.go`
- Modify: `internal/agent/local_handler.go`
- Modify: `internal/agent/local_handler_test.go`

- [ ] **Step 1: 写查询、审核和预览 RED 测试**

  覆盖：

  - 组列表按 exact/image/video/inconclusive、路径、文件名、大小、审核状态筛选并稳定分页。
  - 默认所有成员 `undecided`；提交审核必须明确一个或多个 keep，delete 只能来自 exact 或 verdict=`yes`。
  - `local.preview.image` 只接受数据库文件 ID，不接受路径。
  - Agent 校验 machine ID、active 状态、SHA、size、mtime 后才调 Worker 生成内存 JPEG/WebP 预览。
  - 预览响应有尺寸和 4 MiB 上限；原图 stale、已删除、非图片、跨机器都拒绝。
  - 临时目录和源目录中没有新增图片缩略图文件。
  - 视频审核继续读取扫描阶段已有缩略图，不重建图片预览缓存。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/localreview ./internal/localpreview ./internal/agent ./internal/worker ./cmd/worker -run 'GroupQuery|Review|ImagePreview|NoPreviewFile|StalePreview'`

  Expected: FAIL，原因是查询服务和 Worker 内存预览 job 不存在。

- [ ] **Step 3: 实现窄查询服务和内存预览 Worker job**

  - 分页请求最大 200 条；错误只返回稳定码和安全摘要。
  - Worker 只把编码后的预览字节返回 Agent，不写文件。
  - 审核提交与 outbox 写入同一 SQLite 事务；未提交的前端勾选不写 PostgreSQL outbox。

- [ ] **Step 4: 运行 GREEN 和 Worker 回归**

  Run: `go test -count=1 ./internal/localreview ./internal/localpreview ./internal/agent ./internal/worker ./cmd/worker`

  Expected: PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/localreview internal/localpreview internal/agent/local_handler.go internal/agent/local_handler_test.go internal/worker/messages.go internal/worker/messages_test.go cmd/worker
  git diff --cached --check
  git commit -m "feat: review local results with memory previews"
  ```

---

## Task 11：实现审核绑定删除和保留哈希的 deleted 状态同步

**Files:**

- Create: `internal/localdelete/service.go`
- Create: `internal/localdelete/service_test.go`
- Modify: `internal/agent/delete/forwarder.go`
- Modify: `internal/agent/delete/forwarder_test.go`
- Modify: `internal/agent/local_handler.go`
- Modify: `internal/agent/local_handler_test.go`
- Modify: `internal/store/files.go`
- Modify: `internal/store/store_test.go`
- Modify: `internal/store/local_review.go`
- Modify: `internal/store/local_review_test.go`
- Modify: `internal/firstscreen/store.go`
- Modify: `internal/firstscreen/store_integration_test.go`

- [ ] **Step 1: 写删除状态机和数据保留 RED 测试**

  固定两阶段接口：

  ```go
  type Service interface {
      Prepare(context.Context, DeleteSelection) (DeletePreview, error)
      Execute(context.Context, DeleteExecution) (DeleteBatch, error)
      Status(context.Context, string) (DeleteBatch, error)
  }
  ```

  RED 必须逐项证明：

  - Prepare 只接受已提交审核中标记 delete 的 exact/yes 成员，并返回完整路径、数量、总大小、选择摘要和一次性短期令牌。
  - Execute 再次核验 machine ID、path、SHA-512、size、mtime、审核 generation 和选择摘要；令牌使用一次后失效，Agent/NodeTray 重启后失效。
  - Helper `OK=true && Uncertain=false` 才进入成功分支。
  - Helper 失败、断连、超时、`Uncertain=true` 时，`files.status` 保持原值，并保存失败/不确定审计。
  - 成功分支在一个 SQLite 事务中写删除结果、`files.status='deleted'`、文件同步队列和本地删除 outbox。
  - 成功后原 `files.sha512` 不变；`image_features`、`video_features`、`video_frames`、`local_pair_scores`、`local_dup_members`、`local_reviews` 记录数和内容不变。
  - `idx_files_sha512` 仍可按原 SHA 找到 deleted 行；默认 active/当前组查询排除该行；历史/审计查询仍返回它。
  - 同批部分成功时只标记明确成功的文件，其他文件保持原状态。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/localdelete ./internal/agent/delete ./internal/agent ./internal/store ./internal/firstscreen -run 'DeletePrepare|DeleteExecute|DeletedRetention|Uncertain|PartialDelete|DeletedExcluded'`

  Expected: FAIL，原因是现有 `MarkDeleted` 尚未与审核、删除审计和本地 outbox 形成单一事务。

- [ ] **Step 3: 实现绑定审核的删除事务**

  将现有 `MarkDeleted` 收窄为事务内部方法，并增加明确入口：

  ```go
  type DeletionResult struct {
      FileID    int64
      MachineID string
      Path      string
      SHA512    string
      BatchID   string
      OK        bool
      Uncertain bool
      ErrorCode string
  }

  func (d *DB) CommitDeletionResults(
      ctx context.Context,
      batchID string,
      results []DeletionResult,
  ) error
  ```

  事务只 UPDATE 文件状态，不对任何哈希或特征表执行 DELETE/NULL，不依赖级联清理。PostgreSQL 同步事件携带 file ID、machine ID、status=`deleted`、原 SHA 和删除批次 ID。

- [ ] **Step 4: 运行 GREEN 和删除回归**

  Run: `go test -count=1 ./internal/localdelete ./internal/agent/delete ./internal/agent ./internal/store ./internal/firstscreen`

  Expected: PASS。

  额外静态证据：

  Run: `rg -n "DELETE FROM (image_features|video_features|video_frames|local_pair_scores|local_dup_members)|sha512\s*=\s*NULL" internal deploy`

  Expected: 本任务生产路径没有命中。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/localdelete internal/agent/delete internal/agent/local_handler.go internal/agent/local_handler_test.go internal/store/files.go internal/store/store_test.go internal/store/local_review.go internal/store/local_review_test.go internal/firstscreen/store.go internal/firstscreen/store_integration_test.go
  git diff --cached --check
  git commit -m "feat: retain hashes when local files are deleted"
  ```

---

## Task 12：异步同步本机结果与 deleted 状态到 PostgreSQL

**Files:**

- Modify: `deploy/central.sql`
- Create: `internal/store/local_sync.go`
- Create: `internal/store/local_sync_test.go`
- Modify: `internal/syncer/syncer.go`
- Modify: `internal/syncer/syncer_test.go`
- Modify: `internal/syncer/postgres_integration_test.go`
- Modify: `cmd/agent/main.go`
- Modify: `cmd/agent/main_test.go`

- [ ] **Step 1: 写中心 schema 与离线恢复 RED 测试**

  PostgreSQL 新增独立本机 scope 表：

  - `local_analysis_runs`
  - `local_pair_scores`
  - `local_dup_groups`
  - `local_dup_members`
  - `local_task_events`
  - `local_review_decisions`
  - `local_delete_results`

  每张表都包含 `machine_id`、本机 scope/分析 run 或 task key，并有幂等唯一键。测试证明：

  - PostgreSQL 离线时本地任务完成，outbox 保留且 Agent 健康仅显示 sync degraded。
  - 恢复连接后按 sequence 补传；远端提交后、本地 ack 前失败可安全重传。
  - 本机结果写 `scope=local`，不会覆盖现有跨机器 `dup_groups/pair_scores`。
  - 本机文件删除成功后远端 `files.status='deleted'`，但 `files.sha512`、`image_features`、`video_features`、`video_frames` 和本机历史表不变。
  - 删除失败/不确定时远端不收到 deleted 事件。
  - 同一 machine/run/entity/generation 重放不生成重复行。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/syncer ./internal/store ./cmd/agent -run 'LocalOutbox|LocalScope|OfflineRecovery|DeletedRemoteRetention|IdempotentReplay'`

  Expected: FAIL，原因是远端事务接口只支持原始文件/特征表。

- [ ] **Step 3: 扩展 outbox loader 和 PostgreSQL 事务**

  - `RemoteTx` 增加按 topic 批量 UPSERT 的本机事件方法。
  - 远端 deleted 文件更新继续使用 `ON CONFLICT(machine_id,path) DO UPDATE SET status=EXCLUDED.status`，同时显式保留已有非空 SHA/特征值。
  - 本机事件和原始文件同步可在同一远端事务提交；任一项失败均不 ack 本地 outbox。
  - Agent 启动不 `Ping` 阻塞监听；syncer 后台指数退避并报告安全摘要。

- [ ] **Step 4: 运行 GREEN、PostgreSQL 集成和无 PG 回归**

  Run: `go test -count=1 ./internal/syncer ./internal/store ./cmd/agent`

  Precondition: 当前 shell 已设置只用于验收的 `$env:TEST_POSTGRES_DSN`；未设置时不要运行该集成命令。

  Run: `go test -count=1 ./internal/syncer -run 'Postgres|DeletedRemoteRetention|LocalScope'`

  Expected: 配置测试库时 PASS；没有测试库时集成用例明确 SKIP，单元用例仍 PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- deploy/central.sql internal/store/local_sync.go internal/store/local_sync_test.go internal/syncer cmd/agent
  git diff --cached --check
  git commit -m "feat: sync local analysis and deletion state"
  ```

---

## Task 13：把本机闭环接入 NodeTray Wails/Web 界面

**Files:**

- Modify: `internal/nodetray/traymodel/model.go`
- Modify: `internal/nodetray/traymodel/model_test.go`
- Modify: `internal/nodetray/app/service.go`
- Modify: `internal/nodetray/app/service_test.go`
- Modify: `nodetray/app.go`
- Modify: `nodetray/app_test.go`
- Modify: `nodetray/composition.go`
- Modify: `nodetray/composition_test.go`
- Modify: `nodetray/frontend/src/components/AppShell.tsx`
- Modify: `nodetray/frontend/src/components/AppShell.test.tsx`
- Modify: `nodetray/frontend/src/App.tsx`
- Modify: `nodetray/frontend/src/App.test.tsx`
- Create: `nodetray/frontend/src/api/localAgent.ts`
- Create: `nodetray/frontend/src/api/localAgent.test.ts`
- Create: `nodetray/frontend/src/pages/LocalTasksPage.tsx`
- Create: `nodetray/frontend/src/pages/LocalTasksPage.test.tsx`
- Create: `nodetray/frontend/src/pages/AnalysisPage.tsx`
- Create: `nodetray/frontend/src/pages/AnalysisPage.test.tsx`
- Create: `nodetray/frontend/src/pages/ReviewPage.tsx`
- Create: `nodetray/frontend/src/pages/ReviewPage.test.tsx`
- Create: `nodetray/frontend/src/pages/DeleteHistoryPage.tsx`
- Create: `nodetray/frontend/src/pages/DeleteHistoryPage.test.tsx`

- [ ] **Step 1: 写 Wails binding 和页面 RED 测试**

  Backend 只转发 typed DTO：

  ```go
  func (b *Backend) CreateLocalTask(req traymodel.LocalTaskCreate) traymodel.LocalTaskResult
  func (b *Backend) ListLocalTasks(req traymodel.PageRequest) traymodel.LocalTaskPage
  func (b *Backend) StartLocalAnalysis(req traymodel.LocalAnalysisStart) traymodel.OperationResult
  func (b *Backend) ListLocalGroups(req traymodel.LocalGroupQuery) traymodel.LocalGroupPage
  func (b *Backend) SaveLocalReview(req traymodel.LocalReviewSave) traymodel.OperationResult
  func (b *Backend) PrepareLocalDelete(req traymodel.LocalDeletePrepare) traymodel.LocalDeletePreview
  func (b *Backend) ExecuteLocalDelete(req traymodel.LocalDeleteExecute) traymodel.LocalDeleteBatch
  func (b *Backend) GetLocalImagePreview(fileID int64) traymodel.ImagePreview
  ```

  前端测试证明：

  - 导航包含“本地任务、去重分析、结果审核、删除记录”，不显示其他机器/Agent 连接页。
  - 可选择目录并创建扫描或自动三筛任务；一筛基础特征没有关闭开关。
  - 页面显示来源、阶段、状态、速度、失败数、耗时和同步状态。
  - 审核按四类结果展示一/二/三筛分数；默认没有删除勾选。
  - 图片预览使用内存响应，不构造 `file://` 或任意路径请求。
  - 删除先显示预览，再发送一次性 token；失败/不确定项不显示为已删除。
  - PostgreSQL 错误只显示“同步暂不可用”，页面和本机操作仍可用。

- [ ] **Step 2: 运行 RED**

  Run: `go test -count=1 ./internal/nodetray/app ./internal/nodetray/traymodel ./nodetray -run 'LocalTask|LocalAnalysis|LocalReview|LocalDelete|ImagePreview'`

  Run: `Push-Location nodetray/frontend; npm.cmd test -- --run; Pop-Location`

  Expected: FAIL，原因是 binding、API 和页面尚不存在。

- [ ] **Step 3: 实现 Socket-backed Service 和四个页面**

  - `trayapp.Service` 依赖 Task 3 的 `agentclient.Client`，不得依赖 `store.DB`；Agent 运行时的配置读取、校验和保存也通过 `local.config.*`，NodeTray 不直接改 Agent 业务配置。
  - 页面查询全部分页；长任务通过轮询或 `MsgLocalEvent` 刷新，断线后按 task ID 恢复。
  - 敏感路径只在已授权审核/删除视图显示；令牌和 DSN 不进入前端状态。
  - 保留现有 Agent/Helper/程序设置页面，新增业务页面不破坏生命周期操作。

- [ ] **Step 4: 生成 Wails bindings/embed 并运行 GREEN**

  Run: `go test -count=1 ./internal/nodetray/app ./internal/nodetray/traymodel ./nodetray`

  Run: `Push-Location nodetray/frontend; npm.cmd test -- --run; npm.cmd run lint -- --quiet; npm.cmd run build; Pop-Location`

  Run: `go test -count=1 ./nodetray ./internal/nodetray/...`

  Expected: PASS；生成资源已更新且 `git diff --check` PASS。

- [ ] **Step 5: 提交**

  ```powershell
  git add -- internal/nodetray nodetray
  git diff --cached --check
  git commit -m "feat: add local dedup console to nodetray"
  ```

---

## Task 14：端到端验收、文档和 Compute ZIP 发布

**Files:**

- Modify: `scripts/build.ps1`
- Modify: `scripts/package-node-release.ps1`
- Modify: `scripts/test-package-node-release.ps1`
- Modify: `scripts/test-package-portable-release.ps1`
- Create: `scripts/test-agent-local-console.ps1`
- Modify: `README.md`
- Modify: `deploy/agent.example.json`

- [ ] **Step 1: 写发布和本机闭环 RED 合同**

  `scripts/test-agent-local-console.ps1` 使用唯一临时目录、空闲端口和测试数据库，证明：

  1. 解压 Compute ZIP 后直接启动 NodeTray/Agent，不配置 PostgreSQL。
  2. 创建包含重复图片、相似图片、重复视频和普通文件的本机扫描任务。
  3. 等待一筛基础特征完成，并断言图片目录和数据目录没有图片缩略图。
  4. 运行二筛、三筛，检查 stage 统计与最终组。
  5. 审核选定一个重复文件，走删除预览和软删除。
  6. Helper 明确成功后，SQLite 文件状态为 deleted；SHA、特征、索引、分数、组历史和审核记录仍存在。
  7. 注入失败/uncertain 删除，文件状态不变。
  8. 启用测试 PostgreSQL 后等待 outbox 清空，中心 files 标记 deleted 且哈希/特征/历史仍存在。
  9. Manager 分别发送 Stage 2/3 测试任务并收到结果。
  10. 关闭 Manager/PostgreSQL 后，本机新任务继续工作。

  所有测试进程必须记录 PID；`finally` 只停止本脚本启动且最终 EXE 路径仍属于测试解压目录的进程。

- [ ] **Step 2: 运行 RED 发布合同**

  Run: `pwsh -NoProfile -File scripts/test-package-node-release.ps1`

  Run: `pwsh -NoProfile -File scripts/test-package-portable-release.ps1`

  Run: `pwsh -NoProfile -File scripts/test-agent-local-console.ps1 -StageDir artifacts/stage`

  Expected: 首次因新本机闭环尚未被构建/打包或验收脚本尚不存在而 FAIL。

- [ ] **Step 3: 更新构建、包合同和中文文档**

  - Compute ZIP 只包含所需 EXE、Everything/FFmpeg/native 依赖、配置模板、许可证、启动脚本和 release manifest；不放真实控制令牌或预建 SQLite。
  - `agent.example.json` 说明 PostgreSQL 可选、Socket 监听端口之外的错误不影响 NodeTray 打开。
  - README 用中文说明本机闭环、DSN 示例、图片不存缩略图、删除状态/哈希保留、PG 离线重试和 Manager 分阶段任务。
  - 不生成安装包。

- [ ] **Step 4: 运行静态与单元总门禁**

  Run: `gofmt -w (rg --files cmd internal nodetray -g '*.go')`

  Run: `go test -p=1 -count=1 ./...`

  Run: `go vet ./cmd/agent ./cmd/worker ./cmd/nodetray ./internal/agent/... ./internal/localcontrol/... ./internal/localtask/... ./internal/localanalysis/... ./internal/localreview/... ./internal/localdelete/... ./internal/store/... ./internal/syncer/...`

  Run: `Push-Location nodetray/frontend; npm.cmd test -- --run; npm.cmd run lint -- --quiet; npm.cmd run build; Pop-Location`

  Run: `pwsh -NoProfile -File scripts/test-node-tray-supply-chain.ps1`

  Run: `pwsh -NoProfile -File scripts/test-package-node-release.ps1`

  Run: `pwsh -NoProfile -File scripts/test-package-portable-release.ps1`

  Expected: 全部 PASS。

- [ ] **Step 5: 构建并执行 Windows 运行验收**

  Run: `pwsh -NoProfile -File scripts/build.ps1`

  Precondition: 当前 shell 已设置只用于验收的 `$env:TEST_POSTGRES_DSN`；脚本必须拒绝空值和非测试库标识。

  Run: `pwsh -NoProfile -File scripts/test-agent-local-console.ps1 -StageDir artifacts/stage -PostgresDSN $env:TEST_POSTGRES_DSN`

  Expected: 本机闭环和 PostgreSQL 补传全部 PASS。Everything 首次索引允许一直等待，除非用户主动取消。

- [ ] **Step 6: 生成便携 ZIP 并核验内容与哈希**

  Run: `pwsh -NoProfile -File scripts/package-portable-release.ps1 -StageDir artifacts/stage -OutputDir artifacts/releases -ReleaseId (Get-Date -Format yyyyMMdd-HHmmss) -SourceRevision (git rev-parse HEAD)`

  核验 Compute ZIP：

  - 只有运行所需可执行文件、配置模板、启动脚本、依赖、许可证和 manifest。
  - 不含 `gui.exe`、真实 `agent.json`、真实 DSN、`local-control.token`、预建 `agent.db`。
  - `.sha256` 与 ZIP 一致；解压后二次运行验收 PASS。

- [ ] **Step 7: 提交发布链变更**

  ```powershell
  git add -- scripts/build.ps1 scripts/package-node-release.ps1 scripts/test-package-node-release.ps1 scripts/test-package-portable-release.ps1 scripts/test-agent-local-console.ps1 README.md deploy/agent.example.json
  git diff --cached --check
  git commit -m "build: validate local compute console release"
  ```

---

## 最终验收清单

- [ ] NodeTray 与 Agent 的所有控制和业务通信都走 TCP Socket；Agent 命名管道代码不存在，Helper 管道正常。
- [ ] 不配置 PostgreSQL 和 Manager 时，本机扫描、一筛、二筛、三筛、审核、删除闭环可用。
- [ ] 一筛特征默认计算；图片没有磁盘缩略图，图片预览只在内存中生成。
- [ ] Agent 同时接受 Manager 分别下发的 Stage 2 与 Stage 3，以及旧合并任务。
- [ ] 本机分析只包含当前 machine ID，generation 发布失败时旧结果不受影响。
- [ ] PostgreSQL 离线不阻断本机工作，恢复后 outbox 幂等补传。
- [ ] 只有物理删除明确成功的文件被标记 `deleted`；失败/uncertain 文件状态不变。
- [ ] SQLite 与 PostgreSQL 的 deleted 文件仍保留 SHA-512、特征、哈希索引关联、分数、组历史、审核决定和删除审计。
- [ ] 默认当前查询排除 deleted，历史和审计查询仍可关联 deleted 文件。
- [ ] 最终只发布便携 ZIP，包内容合同、SHA-256 和解压运行验收通过。

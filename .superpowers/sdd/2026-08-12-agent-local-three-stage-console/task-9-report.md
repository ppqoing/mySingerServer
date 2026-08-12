# Task 9 实现报告：可恢复本机任务与公平 Worker 调度

## Status

PASS。本机扫描任务、扫描后自动三筛任务、恢复、取消、重试、NodeTray Socket 命令、公平 Worker 调度及 Agent 生产接线已完成。SQLite 仍仅由 Agent 打开；本任务未实现查询审核、删除、PG outbox sink 或 Web UI。

## 基线

- 工作树：`D:\code\mySingerServer\.worktrees\portable-dual-package`
- 分支：`codex/portable-dual-package`
- 基线：`8c5dfe1a38effbbacd242be834eea4f2b167267c`
- 初始脏项仅 `?? .codex-temp/`；全程未读取、修改、删除或暂存该目录。

## RED → GREEN

| 合同组 | 有效 RED | GREEN |
| --- | --- | --- |
| Protocol | `go test -count=1 ./internal/proto -run 'LocalTask'` 编译失败，缺少 `LocalTaskCreateRequest`、Task/分页/重试 DTO | DTO 使用既有 `MsgLocalRequest/Response`，验证 task ID、roots、模式、扩展名和重复项；msgpack 往返通过 |
| Store envelope / lifecycle | `go test -count=1 ./internal/store -run 'LocalTask'` 编译失败，缺 `Envelope`、Transition/List/Cancel/Retry | opaque envelope 迁移、digest+bytes 冲突、machine 分页、Store 固定状态图、stage/progress 单调、total 固定、Cancel 幂等、同 task ID Retry 通过 |
| Service / recovery | `go test -count=1 ./internal/localtask -run 'LocalTask|Recovery|Disconnect'` 编译失败，缺 Service/编码/恢复实现 | Create 幂等、连接无关后台 context、PrepareRecovery/Resume 两阶段、旧空 envelope fail-closed、持久 stage 恢复通过 |
| Fair scheduler | brief 聚焦 RED 编译失败，缺 `NewFairScheduler`、queue key、closed error | bounded 单调度 goroutine按 source+stage 轮转，关闭/背压释放，Results/Crashes/Metrics 原样代理 |
| PoolRouter adapter / handler | Agent 聚焦 RED 编译失败，缺 `NewLocalStageWorker`、`NewLocalTaskHandler` | NextJobID→Register→fair Submit→terminal；取消清 route；task/analysis 命令复用认证 NodeTray gate，真实 Socket 未认证拒绝且 Ping 及时 |
| cmd lifecycle / PG | 新测试先要求 stage-aware runner、listen 前 Prepare/ready 后 Resume、PG parse fail-degraded | scan-only 终态 stage 1；auto 终态 stage 3；stage 1/2 恢复不重扫；listen 前无 PG Ping，parse 失败只降级 sync health |

## 调度不变量

- 一个 `FairScheduler` 同时注入 PoolRouter、ScanManager 与 Phase2Manager，是 scan、manager、local 的唯一底层 `Submit` 入口。
- 队列 key 为 `source + screen_stage`；只在活跃 key 间 round-robin，每个 key 内 FIFO。
- 队列有固定容量，只有一个 dispatch goroutine，不按任务创建无限 goroutine；关闭会释放排队和当前 Submit 调用。
- PoolRouter 仍是进程级 Results/Crashes 的唯一消费者；本地 StageWorker 只等待私有 terminal。

## 恢复、连接、认证和 PG 边界

- `PrepareRecovery` 在 listener 创建前完成 migration 后的 `running → waiting_recovery` 注册；`Resume` 仅在 `agent listening` 后异步触发。
- scan-only 扫描完成落 stage 1；auto-analysis 调用 Task 8 Engine 完成一筛候选、二筛、三筛和 publish 后落 stage 3。
- stage 1/2 恢复均跳过 scan；Task 8 Engine 当前以幂等方式重跑分析整体，成功后落 stage 3，恢复粒度不细到 Engine 内单 pair。
- Socket 连接生命周期不拥有已受理任务；连接断开只取消请求 context，不取消 Service 的后台 task context。
- `local.task.*` / `local.analysis.*` 继续由 Task 2 Server gate 限定 loopback、role=nodetray、protocol version 和恒定时间 token 校验。
- DSN parse 失败只记录安全摘要和 degraded sync health；listen 前不做网络 Ping。有效 pool 的连接/重试由异步 syncer 负责。
- 新日志不记录 roots、媒体路径、token 或 DSN。

## 最终测试原始摘要

```text
ok   dedup/internal/proto       0.172s
ok   dedup/internal/store       1.637s
ok   dedup/internal/localtask   0.058s
ok   dedup/internal/agent       0.714s
ok   dedup/cmd/agent            0.087s

ok   dedup/internal/proto       1.708s  (race)
ok   dedup/internal/store      10.688s  (race)
ok   dedup/internal/localtask   1.230s  (race)
ok   dedup/internal/agent       2.632s  (race)
ok   dedup/cmd/agent            1.254s  (race)
```

`git diff --check`：PASS，仅 LF→CRLF 工作区提示，无 whitespace error。

## 文件

- `internal/localtask/service.go`, `service_test.go`, `scheduler.go`, `scheduler_test.go`
- `internal/agent/local_handler.go`, `local_handler_test.go`
- `cmd/agent/main.go`, `main_test.go`
- `internal/proto/local.go`, `local_test.go`
- `internal/store/ddl.go`, `db.go`, `local_tasks.go`, `local_tasks_test.go`
- `internal/store/local_analysis_test.go`：仅给既有 helper 补确定性非空 envelope
- 本报告

## Commit

- 消息：`feat: schedule recoverable local agent tasks`
- 哈希：本报告与实现位于同一固定消息提交，最终哈希见任务回执。

## Concerns

- 未执行真实媒体/native DLL 的完整本机三筛运行验收；本任务证据覆盖生产组合、TCP/认证行为、SQLite、调度、恢复及 race。
- Stage 2 恢复会安全重跑 Task 8 幂等 Engine，而不是从 Engine 内部逐 pair 的 Stage 3 精确续跑。

---

# Task 9 修复轮次 1/5（2026-08-13）

基线：`602657e19b201550a6156b1898a2b5dcdd2070dc`。本轮严格保留并忽略 `?? .codex-temp/`。

## A–G RED → GREEN

- A 真公平：RED 的生产式 1024-buffer backend 测试证明旧 wrapper 会先向底层 FIFO 灌入 scan 洪水；GREEN 后 scheduler 自持有界 backlog，底层 in-flight 上限为 `max(1, Metrics().ReadyWorkers)`，每个 `source+stage` key 内 FIFO、active key 间稳定 RR。scheduler 是 backend Results/Crashes 的唯一消费者，按 JobID 精确释放并原样转发，PoolRouter 只消费公开转发通道。
- B cancel/close：RED 中可取消提交在排队/底层背压时不能退出，旧 Close 也可能遗留 dispatch/forwarder；GREEN 后提供 `SubmitContext`、`StopAccepting`、`Shutdown(ctx)`。排队取消会移除任务；关闭顺序为停止接收、backend StopAccepting、清 pending、等待已提交 terminal 完整转发并释放、停止 forwarder。`LocalStageWorker` 优先使用可取消提交且及时取消 route。
- C Cancel→Retry：RED 覆盖 Cancel 后立即 Retry、并发两个 Retry，以及 failed terminal 已写入但旧 attempt 尚未 defer 注销窗口；其中真实失败为 `failed terminal was not persisted`，定位到普通 runner 错误遗漏 `status=failed`。GREEN 后 active attempt 含 cancel/done/superseded 身份，Store retry 是线性化点，旧 attempt 先标记 superseded、取消并等待退出，旧终态不能覆盖新 attempt，并发 Retry 最多一次 launch，普通错误正确落 `failed`。
- D idempotency：RED 为同 envelope 仅持久 Stage 改变时误报 `ErrLocalTaskConflict`；GREEN 后身份只比较 machine/source/type/digest/envelope，Stage 1/2/3 均加载同一任务，真正 envelope 冲突仍拒绝。
- E durable recovery/checkpoint：RED 覆盖 pending 崩溃窗口未恢复、旧 pending 空 envelope 未 fail-closed，以及 Engine 缺少 Stage2 checkpoint；GREEN 后 pending/running/waiting 全部恢复，空 envelope 稳定失败。`RunWithProgress` 在全部 Stage2 save/outbox 完成后、任何 Stage3 提交前回调 2，callback 失败不进入 Stage3；cmd runner 正常持久顺序为 0→1→2→3。
- F slice alias：RED 在 Create 返回后修改调用方 Roots/Extensions，runner/响应被同步污染；GREEN 后 Create、Retry、Resume 都从 persisted Envelope 重新解码 canonical 副本，响应、后台任务和 DB 一致。
- G 日志：RED 用含 user/host/db/query/password 的非法 DSN 以及 root/mount/cause 变体检出明文；GREEN 后 Everything fallback 与 unknown media 只记录 `path_id`、设备号和固定 safe code，PG 配置失败只记录 `postgres_config_invalid`，不记录错误或 DSN。

## Engine、关闭与生产边界

- `Engine.Run` 保持兼容并委托 `RunWithProgress`；Stage2 callback 顺序由事件/outbox 与无 Stage3 job 的断言锁定。
- Agent 关闭严格为 Phase2 drain/cancel → fair scheduler `Shutdown(ctx)` → backend WorkerPool `Close`；scheduler 不会在 inflight terminal 尚未公开转发时停掉 forwarder。
- 公平 wrapper 仍是 Scan、Manager Phase2、Local Analysis 共用的唯一底层 Submit 入口；心跳与状态命令仍绕过计算队列。
- 本轮未修改 `worker.Pool`，无新增白名单需求。

## 本轮文件

- `internal/localtask/scheduler.go`, `scheduler_test.go`, `service.go`, `service_test.go`
- `internal/agent/local_handler.go`, `local_handler_test.go`, `scan.go`, `scan_test.go`
- `internal/localanalysis/engine.go`, `engine_test.go`
- `internal/store/local_tasks.go`, `local_tasks_test.go`
- `cmd/agent/main.go`, `main_test.go`
- 本报告

## 本轮验证与 Commit

- focused 公平/恢复/取消/日志测试：PASS；高并发关键测试重复 20 次：PASS。
- 六包全量最终原始摘要：

```text
ok  dedup/internal/proto          0.191s
ok  dedup/internal/store          1.652s
ok  dedup/internal/localtask      0.275s
ok  dedup/internal/localanalysis  0.048s
ok  dedup/internal/agent          0.732s
ok  dedup/cmd/agent               0.100s
```

- 五包 race 最终原始摘要：

```text
ok  dedup/internal/store          10.991s
ok  dedup/internal/localtask       1.611s
ok  dedup/internal/localanalysis   1.071s
ok  dedup/internal/agent           2.708s
ok  dedup/cmd/agent                1.277s
```

- 固定提交消息：`fix: enforce fair recoverable local scheduling`；最终哈希见任务回执。

## 本轮 Concerns

- `ReadyWorkers` 是当前可用容量快照；为避免 backend FIFO 隐藏竞争，低值时仍保守限制为 1，容量增长会在 terminal/新提交唤醒后继续调度。
- Stage 2 恢复继续安全重跑完整幂等 Engine（不重扫），没有实现 pair 粒度的 Stage 3 续点。
- 未做真实媒体/native DLL 与真实 PostgreSQL 服务运行验收；本轮覆盖单测、race、生产组合顺序和敏感日志边界。

# Bug 排查报告（2026-08-14）

## 排查方法与环境说明

- **前端（webui）**：动态验证——vitest 205/205 全部通过，`tsc --noEmit` 无类型错误，`eslint` 报 1 error + 1 warning（见§四）。
- **后端（Go）**：系统 Go 工具链缺失（项目脚本默认的便携 Go `C:\Users\Administrator\AppData\Local\Temp\go1.26.5-portable` 已被系统清理），无法编译运行，Go 部分为**纯静态审查**。覆盖 `internal/gui`、`internal/agent`、`internal/proto`、`internal/firstscreen`、`internal/phase2`、`internal/enum`、`internal/store`、`internal/worker`、`internal/local*`、`internal/wproc`、`internal/machineid` 等包及 `cmd/` 入口。所有条目均基于代码证据，高危项的证据链经二次人工逐环复核；建议恢复 Go 工具链后运行 `go test ./...` 与集成测试复验。
- 与 UI 审查报告（`2026-08-14-webui-ux-audit.md`）的关系：本报告聚焦代码级 bug；UI 报告中"交互逻辑不正确"类条目（如删除 409 死锁）在此做了代码级确证，不重复罗列。

---

## 一、高危

### B1. 本地分析 mtime 单位错配：本地"扫描+分析"任务 stage2/3 必然失败

**证据链**（已逐环人工复核）：

1. `internal/localanalysis/engine.go:54`：`fileMetadata` 返回 `info.ModTime().UnixMilli()`（**毫秒**）。
2. `engine.go:248`：构造 `worker.JobMsg{Phase: worker.Phase2, MTimeMS: mtimeMS}`。
3. `internal/worker/pool.go:587-590`：`mtime := job.MTimeUnix; if job.Phase == Phase2 { mtime = job.MTimeMS }`——Phase2 任务把**毫秒**存入 `AnalysisResult.MTime`。
4. `internal/store/analysis.go:78`：`storedSize != result.Size || storedMTime != result.MTime` → 返回 `ErrStale`。
5. 而 `files.mtime` 由扫描写入，来自 `internal/enum/walker.go:39` 的 `info.ModTime().Unix()`（**秒**），Everything 路径同样是 Unix 秒（`everything_windows.go:240`）。

**后果**：毫秒（~1.7e12）与秒（~1.7e9）恒不相等 → `SaveAnalysis` 恒返回 `ErrStale` → `pool.go:561-563` 将结果覆写为 `Errors:[{Stage:"stale"}]` → `engine.go:262-264` 见 Errors 非空即报 `worker stage %d failed` → **整个本地分析 run 失败**。本地分析二/三阶段（相似图片/视频判定）实质不可用。

**修复建议**：最小修复是 `engine.go:54` 改用 `info.ModTime().Unix()`（秒）与 files 表一致；但更根本的是按 B3 统一 `MTimeMS` 字段语义，一并消解。

---

## 二、中危

### 后端：gui 包

| # | 位置 | 问题与后果 | 修复建议 |
|---|------|-----------|----------|
| B3 | `internal/phase2/dispatcher.go:399` + `internal/wproc/pipeline_phase2.go:285` | **`Phase2Item.MTimeMS` 语义分裂，集中式 phase2 每个候选文件都被全量重哈希**：dispatcher 把 PG `files.mtime`（秒）装入名为 `MTimeMS` 的字段；worker 端按毫秒比较（`info.ModTime().UnixMilli() == job.MTimeMS`，已复核）。比较恒 false → 图片走重哈希分支（`pipeline_phase2.go:114-120`）、视频走 `confirmPhase2KnownSHA`——GB 级视频全文件 SHA-512。正确性由重哈希兜底，但失去元数据快速路径，I/O 与 CPU 开销数量级上升。注意三处消费点假设互相矛盾：dispatcher 发秒、worker 比毫秒、store 比秒 | 统一字段语义为毫秒（dispatcher 端 `copy.MTime*1000` 并理清存储层），或改名 `MTimeUnix` 全链路用秒；改后 B1 同步消解 |
| B4 | `internal/phase2/dispatcher.go:479-484` | **`duration_ms = 0` 的视频候选阻断全部 phase2 派发**：上游允许 `duration_ms >= 0`（`store/features.go:317`、`store/analysis.go:330`），一筛只要求 `IS NOT NULL`，但 dispatcher `validateSelectedIdentity` 把 `<= 0` 当整体错误返回 → `BuildTasks` 失败 → 所有机器、所有候选的派发全部中止，且数据不变每轮重试必复现，phase2 管道停摆 | 对该 pair `continue`（记日志/计数）而非整体报错；或一筛 `LoadVideoFeatures` 增加 `duration_ms > 0` 过滤 |
| B5 | `internal/gui/groups.go:587-592` | **`member_size` 传空值触发除零 panic**：`parsePositiveDecimal("", 0)` 对空串返回 `0, nil`，校验 `size > 500` 放行 0，随后 `page-1 > maxInt64/int64(size)` 除零。net/http recover 后连接被掐断。触发：`GET /api/groups/{id}?member_page=1&member_size=`（空值路径漏网，`member_size=0` 反而被拒） | 校验改为 `size < 1 || size > 500`，与错误文案 "in 1..500" 一致 |
| B6 | `delete_http.go:183-186` + `DeleteDialog.tsx:298-316` | **409 "confirmation already used" 死锁（UI 报告 H/删除 3.1 的代码级确证）**：token 一次性（`delete.go:134`），重复 Execute 返回 409；前端 `isExpiredConfirmation` 只认 400，409 落入通用失败分支回到 confirming 且沿用死 token，按钮重新可用 → 再点永远 409，且对话框无"重新准备"入口。更糟场景：首次 Execute 已成功但响应丢失，任务在后台执行而前端丢失 task_id | 前端把 409 consumed 并入过期分支（强制重新 prepare）；或服务端对已消费 token 幂等返回原 taskId |
| B7 | `internal/gui/delete.go:520-522` | **DeleteService.tasks 只增不减，无界内存泄漏**：全文件无 `delete(s.tasks, ...)`；每个任务持有全部待删路径与 report 明细。另：终态结算只在 `Status`/`HandleReport` 被调用时发生（`delete.go:583,667`），无人轮询且 agent 不回报的任务连 12 分钟 deadline 结算都不触发，一直非终态 | 终态后延时清理（保留 N 分钟供轮询再删除），或定期扫描清理过期终态任务 |
| B8 | `internal/gui/tasks.go:119-149,214-222,369-376` | **Send 失败后同 task_id 重试导致内存/DB 状态永久不一致**：`Register` 同 envelope 分支不检查终态直接放行重发；但 agent 的 Ack/Progress 被 `isTerminalTaskStatus` 吞掉，任务长期显示 failed；`TaskDone` 能把内存翻成 done，DB upsert 的 CASE 却保留 failed 终态 → **DB 永久记录 failed，内存为 done**。当前 webui 不传 task_id（每次新 UUID），仅自定义客户端/重试中间件可达 | `Register` 对已终态任务返回 409，或重试 Send 成功后重置内存/DB 状态为 sent |

### 后端：agent 包

| # | 位置 | 问题与后果 | 修复建议 |
|---|------|-----------|----------|
| B9 | `internal/machineid/machineid.go:49-83` + `cmd/agent/main.go:160-164,197-199` | **机器唯一 ID 在 WMI 瞬态故障时静默漂移**：`Resolve` 用"本次可用源的子集"算 SHA-256，仅全缺才报错。WMI 重启/超时导致 CPU 或主板源缺失时，启动仅记 warning，算出**不同的**节点 ID。后果：① agent.db 中按 machine_id 作用域的数据全部不可见（相当于全量重扫）；② 单实例互斥名由 machineID 派生，退化身份与正常身份互斥名不同，**双实例可并行运行**；③ 同步到 PG 出现两台"机器" | 首次成功计算后持久化 ID，后续启动优先采用持久化值、校验失败时告警；或强制要求注册表 MachineGUID 可用才启动 |
| B10 | `internal/agent/filesystem_browser_windows.go:123-127` | **单个目录条目属性读取失败导致整目录浏览失败**：任一条目 `GetFileAttributes` 失败（权限拒绝/坏 reparse point/脱机占位文件）即整体报错 return，GUI 无法浏览本身可读的目录（如 `C:\`），用户无法选择扫描根目录 | 条目级失败跳过或标记 `Selectable=false` 附加错误，仅 `ReadDir` 失败才整体报错 |
| B11 | `internal/agent/server.go:283-316` + `internal/agent/delete/forwarder.go:153-222` | **LocalRequest 在连接读循环中同步执行，长操作冻结整条连接**：`local.delete.execute` 每 chunk 最长等 `report_timeout_s`（默认 600 秒），期间读循环停转：入方向 Ping 不应答、后续 LocalRequest 全部排队。对端（nodetray）若按 Pong 及时性判断存活，长删除会被判连接死亡，删除结果丢失关联 | 读循环只做校验与派发，`HandleLocal` 放独立 goroutine 执行后按 RequestID 异步回包（响应本就按 ID 关联） |

### 后端：其余

| # | 位置 | 问题与后果 | 修复建议 |
|---|------|-----------|----------|
| B12 | `cmd/gui/operational_runtime.go:236-253` | **待确认**：`phase2Dispatcher`/`phase2Router` 为 nil 时走 `resources.tasks.Dispatch` 分支，`routeAgentMessage` 被跳过，`proto.DeleteReport` 永远送不到 `deleteService.HandleReport` → 删除任务静默等到 12 分钟 deadline 全判 helper_lost/uncertain。需排查这两个字段为 nil 的启动路径是否真实存在 | 若为不可达路径则加注释/断言；若可达则修复接线 |

---

## 三、低危

### gui 包

| # | 位置 | 问题 |
|---|------|------|
| B13 | `httpapi.go:103-105` | `GET /api/delete/tasks`（无 task_id）必 404 的死路由，无调用方；要么删除要么实现列表语义（UI 报告 H5 的关联项） |
| B14 | `httpapi.go:247-256,142-152`、`config_http.go:100-110` | 非删除类 JSON 接口无请求体大小限制（删除接口有 1MB `MaxBytesReader`），无认证监听下有 DoS 面 |
| B15 | `runtime_host.go:128` | 每个请求重建整个路由 mux（新建 2 个 ServeMux + FileServer），纯性能浪费 |

### agent 包

| # | 位置 | 问题 |
|---|------|------|
| B16 | `internal/agent/server.go:363-366` + `scan.go:222-228` | ScanTask 的 ack 写失败永久毒化该 task_id：`Prepare` 已登记 running，`start()` 未调用，扫描永不启动也永不 finish（retention 不调度），同 task_id 重发被 envelope mismatch 拒绝，直到进程重启。phase2 分支在 ack 失败时会 start()，两分支行为不一致 |
| B17 | `internal/agent/pool_router.go:126-145,158-177` | JobID 命中但字段不匹配的结果仅记日志保留路由，`<-terminal` 永远等不到值，goroutine 与路由表项泄漏直到 pool 关闭 |
| B18 | `internal/agent/scan.go:677`、`phase2.go:649-661` | 等待 worker terminal 无 agent 侧兜底超时；pool 出现"不回结果也不关通道"的 bug 时 goroutine 永久挂起且无日志 |
| B19 | `internal/agent/server.go:132-139` | 非 ctx 取消的 Accept 错误仅 log 后 continue，fd 耗尽等持续错误下 100% CPU 热循环 |
| B20 | `internal/agent/server.go:254-261` | 浏览闸被占时每个新请求都 `go func()` 异步回 browse_busy，洪泛请求堆积大量阻塞在写锁上的 goroutine |
| B21 | `cmd/agent/main.go:342-371` | 进行中的扫描不纳入优雅退出：SIGINT 后不等扫描 goroutine，resultWriter 可能打在已关闭的 SQLite 上，最后一批（≤512 条）hash 结果丢失（失败后保持 pending，重启重扫可自愈） |
| B22 | `internal/agent/scan.go:204` vs `server.go:364-365` | resume 任务 bind sender 在 ack 写出之前，`MsgFeatureResult` 可能抢在 `MsgTaskAck` 前到达 GUI |

### 分析管道 / 其他

| # | 位置 | 问题 |
|---|------|------|
| B23 | `internal/phase2/judge.go:298-329` vs `:139` | 本地与集中式视频二筛判定算法不一致：集中式 AvgSim 分母含 stage2 未通过帧（按 0 惩罚），本地对所有有效帧求均值不惩罚；同一对视频两条路径可能得出不同 verdict，结论不可互验 |
| B24 | `internal/enum/walker.go:26-28` | walker 单目录读错（权限不足/消失）即中止整个枚举，扫描根下一个不可读子目录（如 `System Volume Information`）会使整个 fallback 扫描失败。应对 walkErr 计数跳过（至少对目录），仅根目录错误致命 |
| B25 | `internal/firstscreen/analyzer.go:76` | `Analyzer.Run` 不做配置校验（对照 `source.go:61` 有）：绕过 GUI 校验的路径下负 `HammingMax` 静默产生零候选对、`AspectTolerance<0` 使候选全灭，无任何报错 |
| B26 | `internal/config/agent.go:266-296` | `ValidateAgent` 未校验 Scan 段的 `HDDReadBlockMB`/`ImageMemResidentMB`/`ImageTimeoutS`/`VideoTimeoutS`：`image_timeout_s=0` 立即超时全部失败；`hdd_read_block_mb` 负值/超大可能分配 panic 或 OOM |
| B27 | `internal/localtask/scheduler.go:202-204` vs `215-218` | FairScheduler 转发循环对 results/crashes 关闭处理不对称：关闭顺序颠倒时 crash 记录不再转发，inflight job 永不释放，Shutdown 卡到 ctx 超时 |

---

## 四、前端动态检查结果

- vitest：**17 个测试文件、205 个用例全部通过**（含删除两阶段确认、代表保护、幂等重试等关键路径）。
- `tsc --noEmit`：**无错误**。
- eslint：**1 error + 1 warning**，均在 `webui/src/features/scans/RemotePathBrowser.tsx`：
  - `:56` error `react-hooks/set-state-in-effect`：effect 体内同步 `setSelectedPath("")` 触发级联渲染（即 UI 报告扫描 C 类 19"重开对话框残留旧目录"的代码现场，修复时应一并清空 entries）。
  - `:59` warning `react-hooks/exhaustive-deps`：effect 缺少依赖 `showHidden`——勾选"显示隐藏文件"后重新打开对话框，新值可能不生效（闭包捕获旧值），属真实行为 bug。

---

## 五、建议修复顺序

1. **B1 + B3（mtime 单位统一）**：一个字段语义修复同时消解"本地分析必然失败"（功能级）和"集中式 phase2 全量重哈希"（性能级）两个 bug，优先级最高。修后补 `engine + 真 Pool + 真 SQLite` 的本地 stage2 集成测试。
2. **B4（duration=0 阻断派发）**：单行级修复（continue 代替整体报错），解除 phase2 管道停摆风险。
3. **B5（除零 panic）**：单行校验修复。
4. **B6（409 死锁）**：前端一个分支修复即可，服务端幂等改造随后。
5. **B9（机器 ID 持久化）**：涉及节点身份稳定性，改前需设计评审。
6. **B7、B8、B10、B11** 中危收尾；低危项随邻近改动顺带处理。

## 六、已排除的疑点（核查后确认无 bug，节选）

- `pool.go` 写路径互斥锁、identity claim/release 配对；`analysis.go` 状态机读写均在锁内；`filesystem_browser.go` pending 双路径无泄漏；delete 报告状态机序列号循环有界。
- `firstscreen/store.go ReplaceResults` 事务边界与 keyset 游标；`phase2/store.go upsertScore` 幂等写入与冲突检测；unionfind 组件归并与代表选择；syncer 代际精确匹配防并发覆盖。
- `agentinstance` 互斥检测（x/sys v0.30.0 wrapper 语义核实）；phase2 ack 失败后 detach→start 为断线 resume 设计；WMI COM 资源释放符合 go-ole 约定。
- 删除接口已限 1MB body；`handleRestartHealth` 的 restart_token 仅为新旧实例交接比对，不授权操作。


---

## 七、修复结果（2026-08-14）

### 已修复（均带回归测试并验证通过）

| 编号 | 修复内容 | 涉及文件 |
|------|---------|---------|
| B1+B3 | mtime 单位统一为 Unix 秒：`engine.go`（`UnixMilli()`→`Unix()`）、`pipeline_phase2.go:285`、`pipeline_session.go:374` 三处比较改为秒。字段名 `MTimeMS` 保留不动——vmihailenco/msgpack 按字段名做 map key 编码，改名会破坏 manager↔agent 线上兼容；改为在 `proto.Phase2Item` 与 `worker.JobMsg` 两个协议结构体上加注"实际承载 Unix 秒"的注释。测试夹具与漂移测试同步到秒级精度（`TestPhase2ImageDetectsSubMillisecondSourceDrift` 更名并改为 1 秒漂移）；新增 `TestEngineDefaultFileMetadataUsesUnixSeconds` 钉住单位契约 | `internal/localanalysis/engine.go`、`internal/wproc/pipeline_phase2.go`、`internal/wproc/pipeline_session.go`、`internal/proto/message.go`、`internal/worker/messages.go` 及测试 |
| B4 | `BuildTasks` 对时长缺失的视频对 `continue` 跳过，不再整体报错；`validateSelectedIdentity` 移除时长检查并精简闲置参数。测试改写为"坏对跳过 + 同批好对正常派发" | `internal/phase2/dispatcher.go`、`dispatcher_test.go` |
| B5 | 校验改为 `size < 1 \|\| size > 500`；回归用例覆盖 `member_size=` 空值（修复前触发除零 panic） | `internal/gui/groups.go`、`groups_test.go` |
| B6 | `isExpiredConfirmation` 识别 409 "already used"，强制回到"重新准备"，文案区分为"该确认已被使用，请重新准备"；新增 409 回归测试 | `webui/.../DeleteDialog.tsx`、`DeleteDialog.test.tsx` |
| B7 | `deleteTaskState` 增加 `terminalAt`，终态后保留 30 分钟（`deleteTerminalRetention`）供轮询，随后由 `Status`/`Execute` 触发的 `pruneTasksLocked` 回收；prune 时先按需做 deadline 结算，无人轮询的任务也能终态化并被回收。两个回归测试（保留窗口内可查/过期回收、无人轮询任务按需结算+回收） | `internal/gui/delete.go`、`delete_test.go` |
| B8 | `Register` 同 envelope 且现状为 failed 时重置为 sent（清 LastErr/AckReason），DB 用专用 `UPDATE ... WHERE status='failed'` 强制重置（通用 upsert 的 CASE 保留终态，无法复用）；回归测试验证重置后 Ack/Progress 恢复流转 | `internal/gui/tasks.go`、`tasks_test.go` |
| B10 | 条目级 `GetFileAttributes` 失败改为跳过该条目，不再中止整目录浏览；测试改写为"坏条目跳过、好条目保留"（`TestFilesystemBrowserSkipsEntriesWithAttributeErrors`） | `internal/agent/filesystem_browser_windows.go` 及测试 |
| 前端 eslint | `RemotePathBrowser` 重开/换 Agent 时的状态残留改为渲染期同步重置（React 认可的 prev-props 调整模式，顺带修复 UI 审查报告"扫描 C 类 19"的残留旧目录问题）；effect 补依赖、`set-state-in-effect` 消除。eslint 0 error 0 warning | `webui/.../RemotePathBrowser.tsx` |

### 验证记录

- `go vet`（七个改动包）：干净。
- `go test ./internal/gui/ ./internal/phase2/ ./internal/agent/ ./internal/localanalysis/ ./internal/worker/ ./internal/proto/`：全部通过。
- `go test ./internal/wproc/`：通过；唯一失败 `TestContactSheetCachePath` 是本机临时目录无 8.3 短路径名导致的预存环境失败（断言期望 `ADMINI~1` 短路径形式），与本次改动无关（diff 不触及 contact sheet 代码）。
- 全量 `go test ./cmd/... ./internal/... ./nodetray/... ./integration/...`：除以下预存环境失败外全部通过——`cmd/helper`（缺 `.superpowers\tmp` 目录、build.ps1 要求 videocore stage 布局、缺 `rsrc_windows_amd64.syso` 构建产物）、`internal/shared/finalpath`（同属 8.3 短路径）、`integration`（缺 `C:\vcpkg`、build 脚本报文契约测试）。这些包与本次 diff 无交集。
- webui：vitest **206/206** 通过（较基线 +1，为新增 409 回归用例）；`tsc --noEmit` 无错误；`eslint` 0 问题。
- 环境备注：Go 工具链已恢复到 `C:\Users\Administrator\AppData\Local\Temp\go1.26.6-portable`（go1.26.6）；跑 wproc 测试需 PATH 包含 `videocore/build/Release` 与 `bin`（DLL 依赖），否则测试二进制报 0xc0000135。

### 本轮未修（建议后续单独排期）

- **B9 机器唯一 ID 漂移**：涉及节点身份语义，需先定持久化方案（建议：首次计算落盘，后续启动优先读持久值），做设计评审后再改。
- **B11 LocalRequest 同步执行冻结连接**：协议读循环异步化重构，风险较高，建议单独任务并配并发回归测试。
- **B12**：已核查——`RestorePhase2` 内 dispatcher/router 无条件赋值且失败即中止启动（`operational_runtime.go:108-113`），nil 分支为不可达的防御性代码，**非 bug，无需改动**。
- **B13–B27 低危项**：本轮未动，随邻近改动顺带处理。

---

## 八、双主机真实媒体联调复核（2026-08-18，最新）

### 8.1 结论

本轮总体结论为 **FAIL（存在 P0 阻断）**。双 Agent 连接、身份认领、真实目录枚举、进度上报、取消收口、PostgreSQL 传输及精确重复分组链路均能运行；但当前生产构建下，**图片阶段一系统性失败，视频阶段一会触发 Go/cgo panic 并使 Worker 退出**。因此真实媒体的相似分析、预览、复核与删除均不能作为通过项。

本节是对当前工作区和当前构建的最新结论；优先级高于本文前面 2026-08-14 的静态结论。未对用户真实媒体执行删除，也未写入媒体目录。

### 8.2 联调基线

| 项目 | 本机 | 远程主机 |
|---|---|---|
| 主机 | `DESKTOP-NSKLQ2S` / `192.168.1.17` | SSH 别名 `codex-192-168-1-6` / `192.168.1.6` |
| 真实测试根 | `I:\MiddleDir\11111111` | `D:\tmp\-------2-4` |
| 枚举基线 | 47,150 个文件，553,002,427,192 字节 | 7,290 个文件，320,879,193,712 字节 |
| Agent 身份 | `node-08d2...45d` | `node-3fc8...4e3e` |
| 控制通道 | `127.0.0.1:9102` | `192.168.1.6:9101`，局域网直连通过 |

- 源码基线：`main`，HEAD `5abea7418f6b51fcf79898c8ef5ce58451cb0d5c`；工作区原本已有大量未提交改动，本轮没有改业务代码。
- Worker 按 `scripts/build.ps1` 的正式参数重建：`CGO_ENABLED=1`、MinGW GCC、`-tags nodynamic`。两端部署同一 Worker，SHA-256 为 `73649B58316EEEEA12D5C920BC72671C5317F2A3C50D7068544B3D71167339CA`。
- Agent SHA-256：`6BD81EAD8D5C185C18816BCDA920283FCF9F3AA89E075F4A81B6338065F077C6`；`videocore.dll` SHA-256：`DEFEE34D2A5352D34AEDA9178061372DFA186DB33D4745C231DEC0937BD71C52`。
- 隔离 PostgreSQL 16 容器健康。远程主机直连本机 5432 被本机防火墙阻断；本轮使用 SSH 反向隧道 `远程 127.0.0.1:15432 -> 本机 127.0.0.1:5432` 完成同步。该项属于测试环境边界，不判为项目逻辑 Bug。

### 8.3 按优先级排序的缺陷

| 优先级 | 编号 | 结论 | 影响与证据 |
|---|---|---|---|
| **P0** | DH-P0-01 | **正式 Worker 构建会选中旧 MediaCore stub，导致全部图片阶段一失败** | 本机真实任务处理 42 个近期图片样本全部失败；远程任务处理 94 个图片样本全部失败。统一错误为 `mediacore: cgo Windows binding unavailable`，随后因 SHA 长度为 0 再触发 `store: SHA-512 must be exactly 64 bytes, got 0`。两任务 `files_done=0`、`decode_calls=0`。失败图片无法进入 SQLite/PG 特征链路。 |
| **P0** | DH-P0-02 | **视频 contact-sheet 的 cgo 传参违反 Go 指针规则，Worker 直接 panic/退出** | 本机真实任务抽到的 8 个 MP4 为 **8/8 crash**，两个 Worker 交替退出并重生，`crash.log` 均为 `exit_code=2`。用同一生产管线对其中一个真实 MP4 做只读直达诊断，稳定得到 `panic: runtime error: argument of cgo function has Go pointer to unpinned Go pointer`，堆栈落在 `videocore.cgoBridge.analyze`。 |
| **P0（静态确认）** | DH-P0-03 | **“扫描 + 本地分析”未把请求 Roots 传入分析阶段，可能把同机器其他目录纳入复核/删除候选** | `agentLocalTaskRunner.Run` 只把 `request.Roots` 传给扫描；随后调用 `analysis.RunWithProgress(ctx, taskID, ...)` 时没有 Roots。`CandidateAnalyzer.Run` 再按 machine ID 调用 `StreamActiveFiles`，读取该机器全部 active 文件。本轮因 DH-P0-01/02 无法走到有效真实分析；为避免扩大作用域，未做真实删除验证。 |
| **P0（静态确认）** | DH-P0-04 | **本地删除先发生物理删除，后提交 SQLite 结果，存在不可原子恢复窗口** | `internal/localdelete/service.go:175-186` 先调用 Helper 执行删除，收齐报告后才调用 `CommitDeletionResults`。若 Agent 在两步之间崩溃，或 SQLite 提交失败，媒体已删除但 DB 仍是 active，后续界面、同步和重复分析会继续把它当作存在。真实媒体删除未执行。 |
| **P1** | DH-P1-01 | **Manager 重启后终态扫描历史从 `/api/tasks` 消失** | 重启前完成任务可见；重启后 `/api/tasks` 返回空数组，但 PostgreSQL `scan_tasks` 仍保留该任务。根因是 `TaskRegistry.Restore` 只恢复 `sent/acked/running`，HTTP 列表又只读内存。新任务产生后只显示重启后的任务，历史审计断层。 |
| **P2** | DH-P2-01 | **`/api/runtime/status` 的 Agent 状态永久停留在初始化快照，与 `/api/agents` 矛盾** | 同一时刻 `/api/agents` 显示两端 `online=true, claimed`，而 `/api/runtime/status` 显示两端 `online=false, pending` 且 machine_id 为空。`RuntimeHost` 构造时写入离线快照，此后没有用连接池实时状态刷新 `status.Agents`。会误导启动页、监控或恢复逻辑。 |

#### DH-P0-01 根因链

1. `internal/wproc/run.go:152-153` 在生产 session pipeline 下仍把阶段一图片单独路由到旧的 `processImageWithDeps`。
2. `internal/wproc/pipeline.go:54,58` 分别调用旧 `mediacore.NewSHA512()` 和 `mediacore.ImagePhase1()`。
3. 真实 binding 仅在 build tag `cgo && windows && legacy_mediacore` 下编译；默认 stub 的条件包含 `!legacy_mediacore`，并固定返回 `ErrUnavailable`。
4. 正式构建脚本 `scripts/build.ps1:386-387` 只传 `-tags nodynamic`，没有 `legacy_mediacore`，所以当前正式 Worker 必然选中 stub。
5. Worker 返回空 SHA 后，Agent 仍尝试保存分析结果，存储层再次以“SHA 必须为 64 字节”拒绝，最终图片既失败又无法持久化。

这不是缺 DLL 或远程部署遗漏；使用正式构建参数在本机重新编译后仍完全复现。

#### DH-P0-02 根因链

1. 阶段一视频需要生成 contact sheet，`processMediaWithDeps` 为 native 分析请求设置临时 JPEG 路径。
2. `internal/wproc/videocore/bindings.go:211-232` 把 UTF-16 路径保存在 Go slice `temporaryPath`，再把 slice 数据指针写入 Go 栈上的 `C.vc_analysis_request`。
3. `bindings.go:234-239` 随后把 `&nativeRequest` 传给 C。此时传入的是“包含 Go 指针的 Go 内存”，违反 cgo 指针规则，Go 运行时在进入 `C.vc_media_analyze` 前直接 panic。
4. `wproc.Run` 没有恢复该 panic；进程退出码为 2，Agent 只能记录笼统的 `exit_code`，然后重生 Worker。单靠 Agent 日志看不到真正错误，直达诊断才暴露 panic 栈。

建议把临时 UTF-16 路径放入 C 分配的内存，或提供 C 侧包装函数在调用期组装 `vc_analysis_request`；同时增加一个使用非空 `TempJPEGPath` 的 Windows+cgo 回归测试，并断言正式 `-tags nodynamic` Worker 能处理视频。

### 8.4 动态联调结果矩阵

| 场景 | 结果 | 证据/边界 |
|---|---|---|
| SSH 与双机 Agent 握手 | **PASS** | 两个不同 machine ID 均被 Manager 认领，远程 9101 局域网直连通过。 |
| Everything 枚举 | **PASS** | 两端均进入 Everything 枚举；远程真实目录得到 7,290 总数。本机真实目录扫描期间目录有并发变化，任务总数为 47,153，比预枚举多 3，属于活目录漂移。 |
| 进度上报 | **PASS** | 远程从未知总量进入 `total=7290` 并连续上报进度；本机枚举后进入 `total=47153`。 |
| 取消与终态收口 | **PASS** | 远程取消后 `done=94, skipped=7196`；本机取消后 `done=163, skipped=46990`。两端均在约 1 秒内进入 `status=failed, ack_reason=cancelled`，没有残留运行任务。 |
| 图片阶段一 | **FAIL / P0** | 远程 94/94 失败；本机近期图片 42/42 失败，系统性错误见 DH-P0-01。 |
| 视频阶段一 | **FAIL / P0** | 本机 8/8 Worker crash，见 DH-P0-02。远程任务在取消前尚未枚举到视频，但部署的是同一 Worker 哈希。 |
| PostgreSQL 同步 | **PARTIAL** | 3 条任务记录均落库；`files` 共 10 行，其中本机 8 行为 crash 且无 SHA，另 2 行为早期隔离样本的 partial。`image_features=0`、`video_features=0`。传输通道可用，但真实特征被上游 P0 阻断。 |
| 集中式一筛 | **PARTIAL** | 对 2 条隔离样本可完成精确分组：`files_scanned=2`、`exact_groups=1`、`groups_written=1`；真实图片/视频相似筛选因特征为 0 无法验收。 |
| 图片预览 | **BLOCKED** | 预览要求 PG 中存在有效图片 SHA；DH-P0-01 使真实图片无 SHA、无 image_features，不能到达预览链路。 |
| 本地分析/复核 | **BLOCKED** | 两个阶段一 P0 使有效特征为 0；同时存在 DH-P0-03 Roots 作用域风险。 |
| 删除 | **BLOCKED（安全边界）** | 未对两个真实媒体目录调用删除接口；上游无有效复核结果，且 DH-P0-03/04 未修前不应开展真实删除验收。 |

### 8.5 修复与复验顺序

1. **先修 DH-P0-01 与 DH-P0-02**：正式构建必须使用同一条可用的 VideoCore 图片/视频阶段一实现；新增“正式构建参数 + 真 Worker + 图片 + 非空 contact-sheet 路径”的黑盒门禁。
2. **再修 DH-P0-03 与 DH-P0-04**：把 Roots/扫描快照明确绑定到 local analysis run；删除采用可恢复的 prepare/journal 状态，至少在物理操作前持久化 intent，并能在崩溃后对账收口。
3. **修 DH-P1-01**：终态任务列表直接分页查询 PostgreSQL，或在 Restore 中按有界时间窗恢复终态；不能只依赖进程内 map。
4. **修 DH-P2-01**：`/api/runtime/status` 在线阶段复用 `Pool.Status()` 快照，数据库不可用阶段才回退到初始化离线状态。
5. 使用相同两个真实目录复验，验收门槛为：不再出现 `ErrUnavailable`、cgo pointer panic 或 Worker 系统性重生；图片与视频均产生非零成功数和特征数；单文件损坏需作为文件级错误而不能杀死 Worker。
6. P0 全部通过后，再用**单独构造的隔离副本**做 Roots 隔离、预览、复核、删除与崩溃恢复测试；真实媒体目录继续保持只读。

### 8.6 本轮未判定为 Bug 的环境项

- 本机 Windows 防火墙阻止远程直连 5432；创建临时防火墙规则因无管理员权限失败，未留下规则。SSH 反向隧道验证了应用层 PostgreSQL 同步本身可用。
- Everything 便携实例首次建立 IPC/索引需要等待；就绪后真实目录枚举成功，不能把启动等待误判为扫描逻辑故障。
- 本机真实目录在扫描期间文件总数增加 3，属于测试目录本身并发变化；当前证据不足以判定为重复枚举。

### 8.7 修复后结论（2026-08-18）

本节覆盖 8.1～8.5 的旧结论。六个已确认缺陷均已按最小方案修复；复验时另发现并修复一个 Worker 协议兼容缺陷。最终结论为：

- **构建与自动测试：PASS**。
- **双主机真实媒体只读扫描：PASS**。本机 `I:\MiddleDir\11111111`、远程 `D:\tmp\-------2-4` 均产生有效图片/视频结果，未再出现图片管线不可用或 cgo pointer panic。
- **故障视频隔离副本：PASS**。不能解码 contact sheet 的单文件会返回 `partial`，不再关闭 Worker IPC 或触发 Worker 重生。
- **真实媒体删除：未执行**。删除事务修复由 SQLite/服务层回归测试验证；为保护真实媒体，未把两个真实目录用于删除验收。

### 8.8 按优先级的修复结果

| 优先级 | 编号 | 状态 | 最小修复与复验证据 |
|---|---|---|---|
| **P0** | DH-P0-01 | **已修复** | 阶段一图片统一走 Session VideoCore，不再走默认不可用的 legacy MediaCore；同时把 native 图片宽高补入 ABI 和 Worker 结果，满足 Agent 协议校验。远程受控目录 3 张图片、2 个视频完成 **5/5、失败 0、crashes 0**；真实目录也产生非零图片和视频特征。 |
| **P0** | DH-P0-02 | **已修复** | 非空 `TempJPEGPath` 的 UTF-16 数据改为在 C 内存中分配、复制并释放，避免“Go 内存中包含 Go 指针再传 C”。Windows+cgo 回归测试确认返回 native 文件错误而不是 pointer panic。 |
| **P0** | DH-P0-03 | **已修复** | local task 将请求 Roots 的副本贯穿到 `RunWithProgressForRoots`、stage1 和候选流；根作用域过滤在持久化前生效。未引入额外缓存或后台观察器。Roots 复制、非法根、越界路径、stage1 过滤测试全部通过。 |
| **P0** | DH-P0-04 | **已修复** | 增加 `BeginDeletionBatch`：Helper 物理操作前先持久化 pending journal，Helper 返回后再原子提交结果；intent 写失败时不调用 Helper，结果提交失败时保留 intent 供后续对账。没有增加后台 reconciler。 |
| **P0（复验派生）** | DH-P0-05 | **已修复** | VideoCore 对坏视频返回的字段掩码可能为 `0xc0`；Agent 协议要求每个 `FieldError.Field` 只能有一个 bit。Worker 现在仅把掩码拆成单 bit 错误（64、128）。两端原先触发 `pipe_eof` 的 4 个故障视频均变为文件级 `partial`，两端 Agent 日志均为 `crashes=0`。 |
| **P1** | DH-P1-01 | **已修复** | `TaskRegistry.Restore` 恢复全部活跃任务和最新 200 个终态扫描，并应用 `stats_json`。Manager 重启后，本轮 3 个终态任务的状态、完成数、失败数和耗时均原样恢复。 |
| **P2** | DH-P2-01 | **已修复** | `/api/runtime/status` 每个请求直接读取已安装 Pool 的实时快照；仅无 Pool 时回退离线状态，不增加状态缓存。Manager 重启前后，该接口与 `/api/agents` 均一致显示两个 Agent `online=true, claimed`。 |

### 8.9 双主机动态复验

| 场景 | 结果 | 证据 |
|---|---|---|
| 本机真实根阶段一扫描 | **PASS** | 任务 `b0452dd7-5ffd-4e5c-963c-4b33352c1830` 在取消收口时 `done=176, skipped=46977, failed=4`；成功结果同时包含图片宽高和视频 duration/contact sheet。4 个失败来自特定视频 contact-sheet 解码失败，不再代表系统性图片/视频管线失败。 |
| 远程真实根阶段一扫描 | **PASS** | 任务 `5feb5384-df5e-49ca-8db2-3296a6e7e468` 在取消收口时 `done=388, skipped=6902, failed=3`；大量图片成功并带非零宽高，视频成功结果带 duration/contact sheet。 |
| 远程受控小目录 | **PASS** | 任务 `be7b45b3-a016-4396-9c09-e2fb2ffb3e85`：3 张图片 + 2 个视频，`done=5/5, failed=0, crashes=0`，`thumb_generated=1, singleflight_hits=2`。 |
| 本机故障视频隔离副本 | **PASS** | 任务 `2c77bac2-924d-4a1c-90ea-99a3651cbec1`：2/2 以 `partial` 完成，错误位分别为 64、128；Agent 记录 `crashes=0`，不再出现 `pipe_eof`。 |
| 远程故障视频隔离副本 | **PASS** | 任务 `e5e156c6-570b-46dc-948d-2bef4bd2c15b`：2/2 以 `partial` 完成，错误位分别为 64、128；Agent 记录 `crashes=0`，耗时 147012 ms。 |
| Manager 重启恢复 | **PASS** | 重启后上述本机故障任务、远程故障任务、远程受控任务均从 PostgreSQL 恢复为终态，统计未丢失。 |
| 双 Agent 实时状态 | **PASS** | `/api/agents` 与 `/api/runtime/status` 同时显示本机 `127.0.0.1:9102`、远程 `192.168.1.6:9101` 在线且已认领，数据库状态为 connected。 |
| PostgreSQL 同步 | **PASS** | 隔离库最终为 `files=573`、`image_features=536`、`video_features=20`、有效 SHA-512 `566`；本机 178 条、远程 395 条。 |
| Roots 隔离与删除 journal | **代码/测试 PASS，真实删除未执行** | Roots 越界和删除事务窗口均有定向回归；真实媒体保持只读。若做破坏性验收，应继续只对另行复制的隔离目录执行。 |

远程 Agent 在一次重启后曾停留在 `acked`，日志显示原因是便携 Everything 的数据库/IPC 尚未就绪；按“便携 Everything 先启动、Agent 后启动”的顺序后 7 秒进入 `everything enumerator ready`，任务正常执行。该现象没有复现为业务代码故障，也未触碰远程已安装的 `D:\Everything-1.4.1.1028.x64` 实例。

### 8.10 最终构建与测试

- 正式 Stage 3：`.tmp\dualhost-20260818\fixed-stage3-20260818`。
- VideoCore：**18/18** 通过；ABI 精确导出：**10/10**；原生依赖闭包：**6/6**。
- 受影响包：`internal/wproc`、`internal/worker`、`cmd/agent`、`internal/localanalysis`、`internal/localdelete`、`internal/store`、`internal/gui` 全部通过。
- 全仓：`go test -p=1 -tags nodynamic -count=1 ./...` **全部通过**。`cmd/helper` 在本机耗时 329.518 秒但最终通过，`integration` 用时 76.075 秒并通过。
- 定向测试首次运行时，既有 `TestFirstScreenHTTPCancelStopsRunningAnalysis` 出现一次取消时序抖动；该用例单独复现通过，整组重跑通过，全仓测试再次通过。本轮没有为一次性抖动增加状态缓存或防御分支。

最终产物 SHA-256：

| 文件 | SHA-256 |
|---|---|
| `agent.exe` | `7C798E4262F705EB3C7F630E1CBC6CCC7593A9E0A6C13BA7E92290FED72DB565` |
| `gui.exe` | `F43098C1CD768A4EC68E55C852F9418CF7B1CEC0B708C1B404727CA935EA0523` |
| `helper.exe` | `540C0E2847CC8B8C6B42B3758FA3C699FF9012446A4BC10A459F6A3BA2357CD6` |
| `worker.exe` | `3837B7F766D719DDED72EFC41E85E28142AE0C40EA5319869482EC02C6D6F979` |
| `videocore.dll` | `C36E6D85B01B62B2A7E0D636AA8FCFF7AD38938AE247493E583CDEDE64D4652C` |

### 8.11 关键回归测试

- 图片 Session 路由与 native 宽高：`TestServeDispatchesPhase1ImageThroughSessionPipeline`、`TestSessionPipelinePhase1ImagePublishesNativeDimensions`。
- cgo 临时 JPEG 路径：`TestCGOAnalyzeTempPathReturnsNativeErrorWithoutPointerPanic`。
- 单 bit 协议错误：`TestSessionPipelineFileErrorUsesSingleFieldBits`，并同步覆盖 stale 组合掩码。
- Roots 作用域：`TestEngineRunWithProgressForRootsForwardsCopiedRoots`、`TestRootScopedCandidateSourceStreamsOnlyFilesWithinRoots`、`TestLocalStageOneRunForRootsFiltersBeforePersisting`。
- 删除 journal：`TestDeleteExecutePersistsIntentBeforeHelper`、`TestDeleteExecuteDoesNotCallHelperWhenIntentFails`、`TestDeleteExecuteKeepsIntentWhenResultCommitFails`、`TestBeginDeletionBatchPersistsPendingJournal`。
- 任务恢复：`TestTaskRegistryRestoresPendingScanEnvelopeWhenIntegrationEnabled`、`TestTaskRegistryRestoresOnlyLatestTwoHundredTerminalScansWhenIntegrationEnabled`，并有本轮 Manager 真重启验证。
- 实时状态：`TestRuntimeHostReportsLiveAgentStatusFromInstalledPool`，并有本轮双 Agent API 对照验证。

### 8.12 仍保留的验收边界

- 两个真实媒体根目录仅用于读取、枚举、哈希与特征分析；没有执行删除，也没有改写源媒体。
- 对故障视频返回 `video frame decode failed` 是文件级结果，不应写成媒体处理成功；本轮修复的验收点是错误隔离后 Worker 保持存活。
- 删除崩溃恢复目前由 journal 事务测试覆盖；如需做进程中断级动态验收，应使用新的隔离副本，分别在 Helper 返回前后中止 Agent 并核对 SQLite journal，不能使用真实媒体根。

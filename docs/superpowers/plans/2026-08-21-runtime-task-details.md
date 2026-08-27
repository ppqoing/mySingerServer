# Rust V2 运行时任务阶段与详情实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 为扫描、本地分析、跨机器分析、删除和同步建立统一但不持久化的运行时任务详情；Desktop 每 2 秒刷新摘要与当前选中详情，终态、断线和重连通过主动事件立即更新，任务中心显示阶段、进度、Worker、物理盘和最近失败。

**架构：** Node 与 Desktop 各拥有进程内 `RuntimeTaskRegistry`。Node registry 观察扫描、本地分析、二筛和删除；Desktop registry 观察跨机器编排和同步。协议只传 Node 运行快照；现有 SQLite `TaskSummary` 继续服务恢复门禁。Transport 的 `request_id=0` 事件通道负责终态和连接事件，不把高频进度推送当作事件流。

**技术栈：** Rust 1.97.1、Tokio watch/broadcast、Prost、Slint 1.17.1、现有 `ClientConnection::next_event()`。

**规格：** `docs/superpowers/specs/2026-08-21-node-runtime-scheduling-and-task-details-design.md`

**全局约束：** 先完成远程配置与 I/O 两个子计划。运行详情不写 SQLite、PostgreSQL、TOML 或日志文件；Node/Desktop 重启清空详情。SQLite 只恢复未完成工作并创建新的“恢复任务”，不恢复旧阶段历史。完成任务可以在当前进程的“已完成”页查看，但进程重启后不从 SQLite 回填。每 2 秒一个固定 tick；终态/断线/重连立即更新。只运行列出的相关测试，Cargo 输出固定 `C:\tmp\rust-v2-node-runtime-target`。

---

### 任务 1：定义运行时任务协议并保持持久 TaskSummary 独立

**Files:**
- Modify: `proto/node.proto`
- Modify: `crates/protocol/src/lib.rs`
- Create: `crates/protocol/tests/runtime_tasks_wire.rs`

**Interfaces:**
- Produces: `ListRuntimeTasks`、`GetRuntimeTaskDetails`、`RuntimeTaskSummary`、`RuntimeTaskDetails`、`RuntimeStageDetails`、`RuntimeWorkerDetails`、`RuntimeFailureDetails`、`RuntimeTaskChanged`。

- [ ] **Step 1: 写 descriptor RED**

测试要求运行消息包含 machine ID、状态、并行阶段摘要、总体计数；阶段包含单位/已完成/总数是否已知/失败/跳过/速度/耗时/ETA；Worker 包含 slot、可选 PID、stage、path、physical disk、完成文件数和速度；失败最多 20 条。断言既有 `TaskSummary` 字段不增加路径、Worker 或速度。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-protocol --test runtime_tasks_wire --locked -- --test-threads=1
```

Expected: FAIL，运行时消息不存在。

- [ ] **Step 3: 实现消息和枚举**

```proto
enum RuntimeStageState {
  RUNTIME_STAGE_STATE_UNSPECIFIED = 0;
  RUNTIME_STAGE_WAITING = 1;
  RUNTIME_STAGE_RUNNING = 2;
  RUNTIME_STAGE_COMPLETED = 3;
  RUNTIME_STAGE_FAILED = 4;
  RUNTIME_STAGE_SKIPPED = 5;
}

message GetRuntimeTaskDetails { string runtime_task_id = 1; }
message RuntimeTaskChanged { string runtime_task_id = 1; string state = 2; }
```

`RuntimeStageDetails.total_known` 区分未知总数，不能用 0 冒充已知总数。Envelope 使用配置/故障消息之后的新字段号，`TaskEvent` 保留兼容现有持久任务边界。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-protocol --test runtime_tasks_wire --locked -- --test-threads=1
git add -- proto/node.proto crates/protocol/src/lib.rs crates/protocol/tests/runtime_tasks_wire.rs
git commit -m "feat: define runtime task detail protocol"
```

Expected: PASS；持久任务和临时详情字段没有混合。

---

### 任务 2：实现 Node 进程内 RuntimeTaskRegistry

**Files:**
- Create: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Create: `crates/node-engine/tests/runtime_tasks.rs`

**Interfaces:**
- Produces: `RuntimeTaskRegistry`、`RuntimeTaskReporter`、稳定阶段/Worker/失败更新；broadcast 终态事件。

- [ ] **Step 1: 写 registry RED**

使用暂停时钟覆盖：创建摘要、多个阶段同时 Running、未知总数、滑动速度、ETA、Worker slot 更新、失败队列只保留最近 20、终态只广播一次、Node registry 重新创建后为空。

```rust
let task = registry.begin(RuntimeTaskKind::Scan, machine_id, "扫描");
task.stage("read_md5").running(40, Some(100));
task.stage("probe_stage1").running(18, Some(40));
assert_eq!(registry.list()[0].stage_summary, "读取与 MD5 / 媒体探测与一筛并行");
```

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test runtime_tasks --locked -- --test-threads=1
```

Expected: FAIL，registry 不存在。

- [ ] **Step 3: 实现内存模型**

Registry 用 `Arc<RwLock<...>>`，reporter 只携带 task ID。阶段 ID 固定英文，显示名固定中文。速度用最近最多 10 秒的单调时钟差分，ETA 只在 total_known 且速度大于 0 时提供。失败结构不含持久 fault 禁止字段之外的额外数据库信息。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test runtime_tasks --locked -- --test-threads=1
git add -- crates/node-engine/src/runtime_tasks.rs crates/node-engine/src/lib.rs crates/node-engine/tests/runtime_tasks.rs
git commit -m "feat: track node runtime tasks in memory"
```

Expected: PASS；测试重建 registry 后没有旧详情。

---

### 任务 3：为扫描、读取和 Worker 流水线发布真实阶段

**Files:**
- Modify: `crates/node-engine/src/scan/engine.rs`
- Modify: `crates/node-engine/src/scan/pipeline.rs`
- Modify: `crates/node-engine/src/io/scheduler.rs`
- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Create: `crates/node-engine/tests/scan_runtime_details.rs`

**Interfaces:**
- Consumes: pipeline 真实通道与 Worker 状态。
- Produces: 扫描六阶段、每盘/Worker/文件/字节进度和失败详情。

- [ ] **Step 1: 写扫描详情 RED**

可控两盘/两 Worker fixture 锁定阶段：`prepare`、`enumerate`、`cache_lookup`、`read_md5`、`probe_stage1`、`persist_finalize`。阻塞读取与 Worker 时断言两个阶段同时 Running；读取进度同时包含完成文件/总文件和完成字节；Worker 行展示 slot、当前 path 和物理盘；文件失败进入最近失败。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test scan_runtime_details --locked -- --test-threads=1
```

Expected: FAIL，扫描没有 reporter。

- [ ] **Step 3: 接入真实状态点**

枚举完成后设置总文件/字节；缓存命中推进 cache 和总体完成；每个 block 推进 read bytes；WorkerPool dispatch/response 更新 slot；writer commit 后推进 stage1/persist。取消先置 terminal，再停止新派发；迟到结果不能覆盖 Cancelled。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test scan_runtime_details --locked -- --test-threads=1
git add -- crates/node-engine/src/scan/engine.rs crates/node-engine/src/scan/pipeline.rs crates/node-engine/src/io/scheduler.rs crates/node-engine/src/worker/pool.rs crates/node-engine/src/actor.rs crates/node-engine/tests/scan_runtime_details.rs
git commit -m "feat: report scan pipeline details"
```

Expected: PASS；阶段摘要来自真实并行状态。

---

### 任务 4：为本地分析、二筛和删除发布完整阶段

**Files:**
- Modify: `crates/node-engine/src/analysis/mod.rs`
- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Modify: `crates/node-engine/src/analysis/exact.rs`
- Modify: `crates/node-engine/src/analysis/image.rs`
- Modify: `crates/node-engine/src/analysis/video.rs`
- Modify: `crates/node-engine/src/analysis/grouping.rs`
- Modify: `crates/node-engine/src/delete.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Create: `crates/node-engine/tests/analysis_runtime_details.rs`
- Create: `crates/node-engine/tests/delete_runtime_details.rs`

**Interfaces:**
- Produces: 本地分析六阶段；节点二筛 Worker 详情；删除四阶段。

- [ ] **Step 1: 写分析和删除 RED**

本地分析锁定 `freeze_inputs/load_features/stage1_candidates/fill_stage2/cluster/save_results`。删除锁定 `revalidate_selection/dispatch_nodes/delete_items/summarize`。测试每阶段单位分别为 files/candidate_pairs/delete_items；partial/failure 准确落在所属阶段。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test analysis_runtime_details --locked -- --test-threads=1
cargo test -p dedup-node-engine --test delete_runtime_details --locked -- --test-threads=1
```

Expected: FAIL，引擎没有 reporter 参数。

- [ ] **Step 3: 接入 reporter**

使用具体 `RuntimeTaskReporter`，不创建只有一个生产实现的空 trait。阶段推进点与当前事务/批次边界一致；删除仍严格使用已确认集合，遥测不得重新计算或扩大删除范围。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test analysis_runtime_details --locked -- --test-threads=1
cargo test -p dedup-node-engine --test delete_runtime_details --locked -- --test-threads=1
git add -- crates/node-engine/src/analysis/mod.rs crates/node-engine/src/analysis/phase2.rs crates/node-engine/src/analysis/exact.rs crates/node-engine/src/analysis/image.rs crates/node-engine/src/analysis/video.rs crates/node-engine/src/analysis/grouping.rs crates/node-engine/src/delete.rs crates/node-engine/src/actor.rs crates/node-engine/tests/analysis_runtime_details.rs crates/node-engine/tests/delete_runtime_details.rs
git commit -m "feat: report node analysis and delete stages"
```

Expected: 两条命令 PASS；删除确认链没有变化。

---

### 任务 5：服务运行快照并推送终态事件

**Files:**
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/server.rs`
- Modify: `crates/node-engine/tests/node_actor.rs`
- Modify: `crates/node-engine/tests/node_server.rs`
- Modify: `crates/desktop-core/src/node_session.rs`
- Modify: `crates/desktop-core/tests/node_session.rs`

**Interfaces:**
- Consumes: Node registry 和 broadcast receiver。
- Produces: 分页 summary、按 ID details、`request_id=0 RuntimeTaskChanged`。

- [ ] **Step 1: 写协议行为 RED**

list 只返回当前进程 registry；details 未找到返回 NotFound；terminal 只推一次；普通 2 秒进度不主动推送；单连接 writer 能交错写 response/event 且 response request ID 不变；断线结束 event reader。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test node_server runtime_events --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test node_session runtime_tasks --locked -- --test-threads=1
```

Expected: FAIL，server 只有响应通道。

- [ ] **Step 3: 实现单 writer 事件合流**

`NodeRequestHandler` 增加 registry event subscription；`serve_connection` 的唯一 writer 从 response mpsc 和 broadcast receiver select，事件 envelope 的 request_id 固定 0。`NodeSession` 增加 list/details 和 `next_runtime_event()`，不创建第二条 TCP 连接。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test node_server runtime_events --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test node_session runtime_tasks --locked -- --test-threads=1
git add -- crates/node-engine/src/actor.rs crates/node-engine/src/server.rs crates/node-engine/tests/node_actor.rs crates/node-engine/tests/node_server.rs crates/desktop-core/src/node_session.rs crates/desktop-core/tests/node_session.rs
git commit -m "feat: stream node runtime task events"
```

Expected: PASS；高频进度仍由轮询取得。

---

### 任务 6：实现 Desktop 临时 registry 并覆盖跨机器、同步和删除

**Files:**
- Create: `crates/desktop-core/src/runtime_tasks.rs`
- Modify: `crates/desktop-core/src/lib.rs`
- Modify: `crates/desktop-core/src/analysis/mod.rs`
- Modify: `crates/desktop-core/src/sync.rs`
- Modify: `crates/desktop-core/src/app.rs`
- Create: `crates/desktop-core/tests/runtime_tasks.rs`
- Create: `crates/desktop-core/tests/controller_runtime_tasks.rs`

**Interfaces:**
- Produces: Desktop-owned cross-analysis/delete/sync details and unified summary key。

- [ ] **Step 1: 写 Desktop registry RED**

锁定跨机器七阶段、删除四阶段、同步四阶段。同步以 `SyncProgress` 的 ACK/incremental/snapshot/caught-up 更新；跨机器 coordinator poll 更新候选/节点/二筛单位；同一机器只能有一个同步 task，重复触发合并而非显示多行。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test runtime_tasks --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked -- --test-threads=1
```

Expected: FAIL，Desktop registry 和 instrumentation 不存在。

- [ ] **Step 3: 实现统一 key 和阶段**

```rust
pub enum RuntimeTaskOwner { Node { node_index: usize }, Desktop }
pub struct RuntimeTaskKey { pub owner: RuntimeTaskOwner, pub id: String }
```

Node summary 转成统一 view 时用已握手 machine ID；Desktop 任务使用涉及机器 ID 列表。删除阶段只观察现有 prepare/confirm/execute，不创建新删除命令。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test runtime_tasks --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked -- --test-threads=1
git add -- crates/desktop-core/src/runtime_tasks.rs crates/desktop-core/src/lib.rs crates/desktop-core/src/analysis/mod.rs crates/desktop-core/src/sync.rs crates/desktop-core/src/app.rs crates/desktop-core/tests/runtime_tasks.rs crates/desktop-core/tests/controller_runtime_tasks.rs
git commit -m "feat: track desktop runtime tasks"
```

Expected: PASS；Desktop restart 后 registry 为空。

---

### 任务 7：加入 2 秒 tick、选中详情和立即事件监督器

**Files:**
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-core/src/view_state.rs`
- Modify: `crates/desktop-core/tests/controller_runtime_tasks.rs`
- Modify: `crates/desktop-core/tests/controller_reconnect.rs`

**Interfaces:**
- Produces: `UiCommand::SelectRuntimeTask`、`UiEvent::RuntimeTasksChanged`、2 秒 tick、stale detail 状态。

- [ ] **Step 1: 写暂停时钟 RED**

Tokio start_paused 测试断言：1.999 秒不刷新，2.000 秒列 summary；只对选中 Node task 调 details；切换选择立即拉一次；terminal event 不等 tick；断线立即标记 stale 并保留最后详情；重连立即 list；重复或过期 session event 不覆盖新机器。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test controller_reconnect runtime_events --locked -- --test-threads=1
```

Expected: FAIL，控制器没有 2 秒 runtime tick 或 event supervisor。

- [ ] **Step 3: 实现控制循环**

增加 `runtime_ticks = interval(Duration::from_secs(2))` 且 `MissedTickBehavior::Delay`。每个已连接 Node 只启动一个 event listener，结果带 session generation 和 machine ID 回到 controller。详情失败保留旧数据并设置 `stale=true` 与错误文本。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test controller_runtime_tasks --locked -- --test-threads=1
cargo test -p dedup-desktop-core --test controller_reconnect runtime_events --locked -- --test-threads=1
git add -- crates/desktop-core/src/app.rs crates/desktop-core/src/view_state.rs crates/desktop-core/tests/controller_runtime_tasks.rs crates/desktop-core/tests/controller_reconnect.rs
git commit -m "feat: refresh task details every two seconds"
```

Expected: PASS；没有更快的隐式轮询。

---

### 任务 8：建立恢复任务而不恢复旧运行详情

**Files:**
- Modify: `crates/node-store/src/tasks.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-store/tests/task_recovery.rs`
- Create: `crates/node-engine/tests/runtime_recovery.rs`

**Interfaces:**
- Produces: 新 runtime ID 的“恢复任务”和 `recovery_validate` 阶段。

- [ ] **Step 1: 写重启 RED**

准备 SQLite 中 queued/running/failed/completed 四种持久任务；重开 Node 后仅未完成任务产生新 runtime task，ID 不等于旧 task ID，历史阶段/Worker/失败为空，从“恢复与校验”开始；completed/failed 不进入当前 registry；Worker crash 已失败 item 不重排。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test runtime_recovery --locked -- --test-threads=1
```

Expected: FAIL，actor 只恢复持久 item，没有新 runtime 包装。

- [ ] **Step 3: 实现恢复入口**

Node 启动枚举 `has_active_computation_tasks` 的真实项，为每个恢复批次创建新的 runtime task。旧 SQLite task ID 只作为恢复输入关联，不暴露为 runtime ID。校验和重新排队完成后进入实际阶段。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test runtime_recovery --locked -- --test-threads=1
cargo test -p dedup-node-store --test task_recovery --locked -- --test-threads=1
git add -- crates/node-store/src/tasks.rs crates/node-engine/src/actor.rs crates/node-engine/src/runtime_tasks.rs crates/node-store/tests/task_recovery.rs crates/node-engine/tests/runtime_recovery.rs
git commit -m "feat: expose fresh runtime recovery tasks"
```

Expected: PASS；旧详情没有持久化或复原。

---

### 任务 9：扩展 UI 模型和绑定

**Files:**
- Modify: `crates/desktop-ui/ui/theme.slint`
- Modify: `crates/desktop-ui/ui/app.slint`
- Modify: `crates/desktop-ui/src/models.rs`
- Modify: `crates/desktop-ui/src/bindings.rs`
- Modify: `crates/desktop-ui/tests/bindings_contract.rs`

**Interfaces:**
- Produces: `UiRuntimeStageRow`、`UiRuntimeWorkerRow`、`UiRuntimeFailureRow`、任务选择回调和 stale 状态。

- [ ] **Step 1: 写模型转换 RED**

断言阶段未知总数显示 `已完成 / —`；速度按单位格式化；ETA 缺失显示 `—`；Worker PID 缺失显示 slot；失败只显示最近 20；机器唯一 ID 不用节点索引代替；选择 callback 精确发送 owner/id 一次。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-ui --test bindings_contract runtime_task_details --locked -- --test-threads=1
```

Expected: FAIL，Slint 结构和 callback 不存在。

- [ ] **Step 3: 实现映射**

`UiTaskRow` 增加 `runtime-id`、`owner-kind`、`machine-id` 和 `stale`。详情模型作为三个 `ModelRc<VecModel<...>>` 整体替换；绑定不得读取后端或启动 timer。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-ui --test bindings_contract runtime_task_details --locked -- --test-threads=1
git add -- crates/desktop-ui/ui/theme.slint crates/desktop-ui/ui/app.slint crates/desktop-ui/src/models.rs crates/desktop-ui/src/bindings.rs crates/desktop-ui/tests/bindings_contract.rs
git commit -m "feat: bind runtime task detail models"
```

Expected: PASS；绑定只转发一次。

---

### 任务 10：重建任务中心双栏详情

**Files:**
- Modify: `crates/desktop-ui/ui/pages/task-center-page.slint`
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`
- Modify: `crates/desktop-ui/tests/visual_preview.rs`

**Interfaces:**
- Consumes: 运行摘要、阶段、Worker、失败模型。
- Produces: 35%/65% 双栏、选中行为、stale 提示、完整阶段详情。

- [ ] **Step 1: 写真实行为 RED**

用 generated MainWindow 和 pointer 断言：点 task 只选择自身并转发一次；阶段列表显示全部阶段和同时运行；Worker 列有 slot/PID/path/disk/speed；失败最多 20；stale 保留画面并显示“数据已过期”；取消仍只对运行中 Node task转发原参数。

- [ ] **Step 2: 写 1440/1080 几何 RED**

两尺寸都要求左栏 32%..38%、右栏 62%..68%、不重叠、各自 ScrollView；长 machine ID/path elide 但完整 accessible label；阶段、Worker 和失败区可纵向滚动。

- [ ] **Step 3: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-ui --test window_contract runtime_task_details --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout runtime_task_details --locked -- --test-threads=1
```

Expected: FAIL，右栏仍显示“运行明细未接入”。

- [ ] **Step 4: 实现页面并生成相关预览**

移除禁用空态。右栏顺序固定：摘要 → 全部阶段 → 当前 Worker → 最近失败。没有 Worker/失败时显示紧凑真实空态，不伪造数据。只生成 `04-tasks.png` 的 1440×900 和 1080×700 预览供人工检查。

- [ ] **Step 5: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-ui --test window_contract runtime_task_details --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout runtime_task_details --locked -- --test-threads=1
$env:RUST_V2_PREVIEW_VIEWS='04-tasks'
cargo test -p dedup-desktop-ui --test visual_preview render_all_views --locked -- --test-threads=1
Remove-Item Env:RUST_V2_PREVIEW_VIEWS
git add -- crates/desktop-ui/ui/pages/task-center-page.slint crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/offscreen_layout.rs crates/desktop-ui/tests/visual_preview.rs
git commit -m "feat: show full runtime task details"
```

Expected: 两个行为/几何测试和相关预览 PASS；不运行其他页面测试。

---

### 任务 11：完成运行任务定向集成门禁

**Files:**
- Create: `crates/desktop-core/tests/runtime_tasks_e2e.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Verifies: Node pipeline → protocol → 2 秒 controller → Slint view model 的真实链路。

- [ ] **Step 1: 写本地 TCP 集成测试**

真实临时 SQLite、NodeServer、两个 controlled Worker 和 Desktop controller；扫描运行时观察并行阶段与两个 Worker，2 秒 tick 后选中详情一致，终态 event 立即到达，断线后 stale，重启后旧详情不恢复并出现新 recovery task。

- [ ] **Step 2: 运行门禁**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test runtime_tasks_e2e --locked -- --test-threads=1 --nocapture
cargo check -p dedup-node-engine -p dedup-desktop-core -p dedup-desktop-ui -p desktop -p node --locked
```

Expected: PASS；输出 tick 次数、terminal event 和恢复 runtime ID。

- [ ] **Step 3: 更新架构并提交**

`AGENTS.md` 记录 Node/Desktop registry 所有权、2 秒轮询、主动 terminal event、stale 保留和持久 TaskSummary 分工。

```powershell
git diff --check
git add -- crates/desktop-core/tests/runtime_tasks_e2e.rs AGENTS.md
git commit -m "test: verify runtime task details end to end"
```

Expected: 无空白错误；不运行 workspace 全量测试。

# Rust V2 Node 远程配置与自重启实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**目标：** 让 Desktop 按机器唯一 ID 加载远程 Node 配置，并通过唯一的“保存并重启”动作把配置原子写入 Node 本地；Node 创建替代进程、响应成功、退出并由新进程等待旧进程结束后重新上线。

**架构：** `NodeConfig` 只承载原始配置值；`NodeConfigRepository` 负责 `bootstrap.toml`、相对路径解析和原子写入；Node actor 只处理协议和版本冲突，应用入口拥有进程替换生命周期。Desktop 只通过 `NodeSession` 读写快照，不直接访问远程文件。

**技术栈：** Rust 1.97.1、Tokio 1.53.1、Prost 0.14.4、Slint 1.17.1、Windows API 0.62、TOML、SHA-256。

**规格：** `docs/superpowers/specs/2026-08-21-node-runtime-scheduling-and-task-details-design.md`

**全局约束：** 本计划是四个子计划的第 1 个；后续顺序为 `node-io-worker-pipeline` → `runtime-task-details` → `real-media-runtime-acceptance`。不增加认证或 TLS；只支持本机物理磁盘路径；相对路径以 `node.exe` 目录解析且响应原样返回；不迁移或删除旧数据；只运行本计划列出的定向测试。Cargo 输出固定到 `C:\tmp\rust-v2-node-runtime-target`。工作树已有 UI、Everything 和发布改动，任何提交都只能精确暂存当前任务 Files 列表。

---

### 任务 1：扩展 Node 配置强类型与验证边界

**Files:**
- Modify: `crates/core/src/config.rs`
- Create: `crates/core/tests/node_config.rs`

**Interfaces:**
- Consumes: `EnumeratorKind`、逻辑 CPU 数。
- Produces: `NodePathsConfig`、`DiskReadConfig`、`WorkerMode`、`WorkerConfig`、扩展后的 `NodeConfig::validate()` 和 `effective_worker_count()`。

- [ ] **Step 1: 写配置默认值和拒绝边界 RED**

测试固定以下值：Everything、HDD=1、SSD=2、未知盘=1、总读取线程=4、块大小=4 MiB、超时=3 秒、重试=2、Worker 自动模式、保留 1 核。覆盖端口 0、线程 0、块大小不在 `64 KiB..=64 MiB`、超时不在 `1..=60`、重试大于 10、手动 Worker 为 0 的拒绝。

```rust
#[test]
fn defaults_match_the_approved_node_runtime_contract() {
    let config = NodeConfig::default();
    assert_eq!(config.enumerator, EnumeratorKind::Everything);
    assert_eq!(config.read.hdd_threads_per_disk, 1);
    assert_eq!(config.read.ssd_threads_per_disk, 2);
    assert_eq!(config.read.unknown_threads_per_disk, 1);
    assert_eq!(config.read.total_threads, 4);
    assert_eq!(config.read.block_size_bytes, 4 * 1024 * 1024);
    assert_eq!(config.read.block_timeout_seconds, 3);
    assert_eq!(config.read.block_retries, 2);
}
```

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-core --test node_config --locked -- --test-threads=1
```

Expected: FAIL，缺少新配置类型和字段。

- [ ] **Step 3: 实现配置模型**

```rust
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerMode { Automatic, Manual }

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(default)]
pub struct DiskReadConfig {
    pub hdd_threads_per_disk: usize,
    pub ssd_threads_per_disk: usize,
    pub unknown_threads_per_disk: usize,
    pub total_threads: usize,
    pub block_size_bytes: usize,
    pub block_timeout_seconds: u64,
    pub block_retries: u32,
}
```

`NodePathsConfig` 保存四个原始字符串：`data_path`、`config_path`、`log_path`、`cache_path`。`WorkerConfig` 保存模式、保留核心和手动数量；`effective_worker_count(logical_cpus)` 在自动模式返回 `logical_cpus.saturating_sub(reserved).max(1)`。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-core --test node_config --locked -- --test-threads=1
git add -- crates/core/src/config.rs crates/core/tests/node_config.rs
git commit -m "feat: define node runtime configuration"
```

Expected: PASS；提交仅包含两个配置文件。

---

### 任务 2：实现 bootstrap、路径语义和原子配置仓库

**Files:**
- Modify: `crates/windows/Cargo.toml`
- Modify: `crates/windows/src/lib.rs`
- Modify: `crates/windows/src/app_layout.rs`
- Create: `crates/windows/src/local_path.rs`
- Create: `crates/windows/tests/local_path.rs`
- Create: `crates/node-engine/src/config_repository.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Create: `crates/node-engine/tests/config_repository.rs`

**Interfaces:**
- Consumes: `AppLayout::executable_dir()`、原始路径字符串、`NodeConfig`。
- Produces: `LocalNodePath::validate()`、`NodeConfigRepository::{load,snapshot,save_if_version}`、固定 `bootstrap.toml`。

- [ ] **Step 1: 写路径与故障窗口 RED**

测试使用临时“node.exe 目录”覆盖：相对路径原样 round-trip、实际访问解析到 exe 目录；绝对本地路径通过；UNC 和 `GetDriveTypeW == DRIVE_REMOTE` 拒绝；修改配置路径后旧配置仍存在；目标配置写失败时 bootstrap 和当前配置均不改变；旧摘要保存返回冲突。

```rust
let loaded = repository.load().unwrap();
assert_eq!(loaded.config.paths.cache_path, r"data\node\cache");
assert_eq!(
    loaded.resolved.cache_path,
    executable_dir.join(r"data\node\cache")
);
```

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test config_repository --locked -- --test-threads=1
```

Expected: FAIL，配置仓库和本机路径验证接口不存在。

- [ ] **Step 3: 实现本机路径验证**

给 `windows` 依赖增加 `Win32_Storage_FileSystem`。`LocalNodePath::validate(executable_dir, raw)`：拒绝空值、UNC、远程盘；相对路径只在内部 `executable_dir.join(raw)`，结构体同时保留 `raw` 与 `resolved`。此任务只判定本地盘，物理磁盘编号和介质类型由第 2 子计划扩展。

- [ ] **Step 4: 实现两文件原子替换**

`bootstrap.toml` 固定放在 `node.exe` 同目录，只含：

```toml
config_path = "data/node/config.toml"
```

`save_if_version` 先重新读取当前摘要，再按“目标配置临时文件 → flush/sync_all → rename → bootstrap 临时文件 → flush/sync_all → rename”的顺序执行。新配置路径不同则保留旧文件；任一步失败不删除旧文件。摘要固定为完整配置 TOML 的 SHA-256 小写十六进制。

- [ ] **Step 5: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test config_repository --locked -- --test-threads=1
cargo test -p dedup-windows --test local_path --locked -- --test-threads=1
git add -- crates/windows/Cargo.toml crates/windows/src/lib.rs crates/windows/src/app_layout.rs crates/windows/src/local_path.rs crates/windows/tests/local_path.rs crates/node-engine/src/config_repository.rs crates/node-engine/src/lib.rs crates/node-engine/tests/config_repository.rs
git commit -m "feat: persist node config through bootstrap"
```

Expected: 两条命令 PASS；没有旧数据迁移或清理行为。

---

### 任务 3：增加配置协议并把版本固定到 V3

**Files:**
- Modify: `proto/node.proto`
- Modify: `crates/protocol/src/lib.rs`
- Modify: `crates/protocol/src/convert.rs`
- Create: `crates/protocol/tests/node_config_wire.rs`

**Interfaces:**
- Produces: `GetNodeConfig`、`NodeConfigSnapshot`、`SaveNodeConfigAndRestart`、`NodeRestartAccepted`；`PROTOCOL_VERSION = 3`。

- [ ] **Step 1: 写 descriptor 和转换 RED**

测试 round-trip 全部配置字段、机器 ID、版本摘要、逻辑 CPU 和有效 Worker；descriptor 必须含四个新消息且不含认证、密钥或 TLS 字段。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-protocol --test node_config_wire --locked -- --test-threads=1
```

Expected: FAIL，生成类型不存在。

- [ ] **Step 3: 增加消息**

```proto
message GetNodeConfig {}
message NodeConfigSnapshot {
  string machine_id = 1;
  string version_sha256 = 2;
  NodeConfigValue config = 3;
  uint32 logical_cpu_count = 4;
  uint32 effective_worker_count = 5;
}
message SaveNodeConfigAndRestart {
  string expected_version_sha256 = 1;
  NodeConfigValue config = 2;
}
message NodeRestartAccepted {
  string machine_id = 1;
  string saved_version_sha256 = 2;
}
```

为 Envelope 使用字段号 `37..40`；已有字段号不复用。内部 `NodeConfigValue` 明确列出基础、四路径、七个读取字段和四个 Worker 字段。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-protocol --test node_config_wire --locked -- --test-threads=1
git add -- proto/node.proto crates/protocol/src/lib.rs crates/protocol/src/convert.rs crates/protocol/tests/node_config_wire.rs
git commit -m "feat: add remote node config protocol"
```

Expected: PASS，握手协议版本为 3。

---

### 任务 4：在 Node actor 中处理加载、冲突和保存请求

**Files:**
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/server.rs`
- Create: `crates/node-engine/src/host_control.rs`
- Modify: `crates/node-engine/src/lib.rs`
- Modify: `crates/node-engine/tests/node_actor.rs`
- Modify: `crates/node-engine/tests/node_server.rs`

**Interfaces:**
- Consumes: `NodeConfigRepository`、`NodeHostControl`。
- Produces: 原样配置快照；Conflict 错误；只有落盘和替代进程创建都成功才返回 `NodeRestartAccepted`。

- [ ] **Step 1: 写 actor RED**

fake repository 记录加载/保存次数；fake host control 记录 `prepare_replacement(saved_version)`。断言旧摘要不调用 host；路径或字段失败不调用 host；创建替代进程失败返回 Internal 且旧 Node 不退出；成功路径只准备一次。

- [ ] **Step 2: 写响应刷出顺序 RED**

`node_server` 使用记录型 handler，断言 `NodeRestartAccepted` 已完整写给 client 后才调用：

```rust
handler.response_flushed(request_id).await;
```

连接写失败时不提交退出。

- [ ] **Step 3: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test node_actor remote_config --locked -- --test-threads=1
cargo test -p dedup-node-engine --test node_server restart_response --locked -- --test-threads=1
```

Expected: FAIL，actor 分支和刷出确认不存在。

- [ ] **Step 4: 实现协议边界**

`NodeHostControl` 只公开两个可测试动作：`prepare_replacement()` 在响应前创建新进程，`commit_exit_after_response()` 在 server 写成功后通知宿主退出。Actor 不直接调用 `std::process::exit`。

- [ ] **Step 5: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-node-engine --test node_actor remote_config --locked -- --test-threads=1
cargo test -p dedup-node-engine --test node_server restart_response --locked -- --test-threads=1
git add -- crates/node-engine/src/actor.rs crates/node-engine/src/server.rs crates/node-engine/src/host_control.rs crates/node-engine/src/lib.rs crates/node-engine/tests/node_actor.rs crates/node-engine/tests/node_server.rs
git commit -m "feat: serve versioned node configuration"
```

Expected: 两条命令 PASS；保存响应和退出次序由行为测试锁定。

---

### 任务 5：实现 node.exe 替代进程和等待旧进程退出

**Files:**
- Modify: `Cargo.lock`
- Modify: `apps/node/Cargo.toml`
- Modify: `apps/node/build.rs`
- Modify: `crates/windows/Cargo.toml`
- Modify: `crates/windows/src/lib.rs`
- Create: `crates/windows/src/process_lifecycle.rs`
- Create: `crates/windows/tests/process_lifecycle.rs`
- Modify: `apps/node/src/main.rs`
- Create: `apps/node/tests/restart_lifecycle.rs`
- Create: `tests/windows/Test-RustV2NodeUac.ps1`

**Interfaces:**
- Produces: `spawn_replacement_node(executable, parent_pid)`、`wait_for_process_exit(pid)`、`--wait-for-parent <PID>`。

- [ ] **Step 1: 写子进程握手 RED**

测试 helper 模式启动一个可控父进程，断言新进程在父进程仍活着时不创建数据库/监听，父进程退出后才继续；无法创建替代进程时旧运行循环仍接收命令。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p node --test restart_lifecycle --locked -- --test-threads=1
```

Expected: FAIL，命令行和 Windows 等待函数不存在。

- [ ] **Step 3: 锁定管理员启动清单**

`apps/node/build.rs` 用 `winresource` 嵌入 `requestedExecutionLevel level="requireAdministrator"`；`Test-RustV2NodeUac.ps1` 用 Windows SDK `mt.exe` 从 release `node.exe` 读取资源并断言该值。已有未提交 UAC 草稿必须原地验证，不能覆盖或丢弃。

- [ ] **Step 4: 实现宿主生命周期**

新进程命令固定为当前 `node.exe --wait-for-parent <当前 PID>`。启动后旧进程继续到配置响应刷出；收到 `commit_exit_after_response` 后有序 shutdown NodeRuntime、托盘事件循环和日志 writer。新进程使用 `OpenProcess(SYNCHRONIZE)` + `WaitForSingleObject(INFINITE)` 等待旧 PID；PID 已不存在视为可继续启动。

- [ ] **Step 5: 让启动顺序受 bootstrap 控制**

`main` 必须先加载 bootstrap/config，再按解析后的日志路径初始化日志，再启动 NodeRuntime。`AppLayout` 不再把 node 数据/日志/cache 固定为唯一生产路径；固定路径只作为新 bootstrap 的默认值。WorkerPool 启动数量必须调用 `config.worker.effective_worker_count(logical_cpu_count)`；不得继续读取已删除的顶层 `worker_count`。

- [ ] **Step 6: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p node --test restart_lifecycle --locked -- --test-threads=1
cargo test -p dedup-windows --test process_lifecycle --locked -- --test-threads=1
cargo build -p node --release --locked --target x86_64-pc-windows-msvc
& .\tests\windows\Test-RustV2NodeUac.ps1 -NodeExe 'C:\tmp\rust-v2-node-runtime-target\x86_64-pc-windows-msvc\release\node.exe'
git add -- Cargo.lock apps/node/Cargo.toml apps/node/build.rs crates/windows/Cargo.toml crates/windows/src/lib.rs crates/windows/src/process_lifecycle.rs crates/windows/tests/process_lifecycle.rs apps/node/src/main.rs apps/node/tests/restart_lifecycle.rs tests/windows/Test-RustV2NodeUac.ps1
git commit -m "feat: restart node after remote config save"
```

Expected: 两个 Rust 测试和 UAC 资源测试 PASS；旧 Node 只有在响应完成后退出。

---

### 任务 6：接入 Desktop 会话、重连验证和控制器状态

**Files:**
- Modify: `crates/desktop-core/src/node_session.rs`
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-core/src/view_state.rs`
- Create: `crates/desktop-core/tests/node_config_controller.rs`

**Interfaces:**
- Consumes: `NodeConfigSnapshot`、Node 机器唯一 ID。
- Produces: `UiCommand::{LoadNodeConfig,SaveNodeConfigAndRestart}`、`UiEvent::NodeConfigChanged`、保存阶段状态。

- [ ] **Step 1: 写控制器 RED**

覆盖：按节点索引发送但以已握手机器 ID 归属；离线拒绝；加载后保存携带原摘要；收到 Accepted 后移除旧会话；按机器 ID 等待重连；重连后自动加载并比对 saved version；超时保留“等待重连”错误；切换节点清空未保存表单。

- [ ] **Step 2: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test node_config_controller --locked -- --test-threads=1
```

Expected: FAIL，新命令、事件和会话方法不存在。

- [ ] **Step 3: 实现会话与状态机**

```rust
pub enum NodeConfigSavePhase {
    Idle, Validating, Saving, Restarting, WaitingForReconnect, Verifying, Completed, Failed,
}
```

`NodeSession` 增加 `get_node_config()` 与 `save_node_config_and_restart()`。控制器不写远程文件；重连仍复用现有 endpoint，但必须确认返回的机器 ID 等于保存目标。

- [ ] **Step 4: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test node_config_controller --locked -- --test-threads=1
git add -- crates/desktop-core/src/node_session.rs crates/desktop-core/src/app.rs crates/desktop-core/src/view_state.rs crates/desktop-core/tests/node_config_controller.rs
git commit -m "feat: manage remote node config lifecycle"
```

Expected: PASS；保存状态严格为“校验→保存→重启→等待重连→验证”。

---

### 任务 7：重建设置页 Node 配置区与真实绑定

**Files:**
- Modify: `crates/desktop-ui/ui/theme.slint`
- Modify: `crates/desktop-ui/ui/app.slint`
- Modify: `crates/desktop-ui/ui/pages/settings-workspace.slint`
- Modify: `crates/desktop-ui/src/models.rs`
- Modify: `crates/desktop-ui/src/bindings.rs`
- Modify: `crates/desktop-ui/tests/window_contract.rs`
- Modify: `crates/desktop-ui/tests/bindings_contract.rs`
- Modify: `crates/desktop-ui/tests/offscreen_layout.rs`

**Interfaces:**
- Consumes: `UiNodeRow.machine-id`、远程配置快照和保存阶段。
- Produces: 节点选择、加载配置、保存并重启、路径/读取/Worker 字段；日志诊断区为下一子计划预留真实模型入口。

- [ ] **Step 1: 写 Slint 行为 RED**

使用真实 generated callback 断言：节点选择项显示完整机器唯一 ID；离线节点禁用两个动作；加载只发一次 `LoadNodeConfig`；唯一保存按钮文案为“保存并重启”且只发一次；切换节点清空已加载配置和扫描路径选择；旧“保存设置”继续只保存 Desktop 配置，不冒充 Node 保存。

- [ ] **Step 2: 写最小窗口几何 RED**

1080×700 下，节点选择、加载、保存并重启、四路径、七个读取字段和 Worker 字段必须位于“节点服务”自身 ScrollView 中且可滚动到达，不能覆盖 190px 二级菜单。

- [ ] **Step 3: 运行 RED**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-ui --test bindings_contract remote_node_config --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract remote_node_config --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout remote_node_config --locked -- --test-threads=1
```

Expected: FAIL，根属性、回调和配置区不存在。

- [ ] **Step 4: 实现 UI 和绑定**

节点选择显示 `节点名称 · machine-id · address · online/offline`。加载成功前表单禁用；内容未变化、离线或保存进行中时“保存并重启”禁用。路径提示明确“相对路径按 node.exe 所在目录解析；旧数据不迁移；不支持网络盘”。自动 Worker 模式禁用手动数量，手动模式禁用保留核心。

- [ ] **Step 5: 运行 GREEN 并提交**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-ui --test bindings_contract remote_node_config --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test window_contract remote_node_config --locked -- --test-threads=1
cargo test -p dedup-desktop-ui --test offscreen_layout remote_node_config --locked -- --test-threads=1
cargo check -p dedup-desktop-ui -p desktop -p node --locked
git add -- crates/desktop-ui/ui/theme.slint crates/desktop-ui/ui/app.slint crates/desktop-ui/ui/pages/settings-workspace.slint crates/desktop-ui/src/models.rs crates/desktop-ui/src/bindings.rs crates/desktop-ui/tests/window_contract.rs crates/desktop-ui/tests/bindings_contract.rs crates/desktop-ui/tests/offscreen_layout.rs
git commit -m "feat: edit node configuration from desktop"
```

Expected: 三个定向测试和 check PASS；不运行 workspace 全量测试。

---

### 任务 8：完成配置子系统定向集成门禁

**Files:**
- Create: `crates/desktop-core/tests/node_config_e2e.rs`
- Modify: `AGENTS.md`

**Interfaces:**
- Verifies: Desktop → TCP → Node repository → restart accepted → machine ID 重连 → reload verify。

- [ ] **Step 1: 写临时目录双进程测试**

测试用真实 TCP、临时 exe 布局和 helper Node host，保存相对路径与绝对路径各一次；断言响应仍保留原字符串，新 bootstrap 指向新配置，旧配置存在，替代进程启动一次。

- [ ] **Step 2: 运行集成测试**

```powershell
$env:CARGO_TARGET_DIR='C:\tmp\rust-v2-node-runtime-target'
cargo test -p dedup-desktop-core --test node_config_e2e --locked -- --test-threads=1 --nocapture
```

Expected: PASS；输出机器 ID、旧/新版本摘要和重连验证成功。

- [ ] **Step 3: 更新架构文档并提交**

在 `AGENTS.md` 更新协议 V3、bootstrap 所有权、Node 配置保存故障窗口和 Desktop 重连验证，不记录实施流水账。

```powershell
git diff --check
git add -- crates/desktop-core/tests/node_config_e2e.rs AGENTS.md
git commit -m "test: verify node config restart boundary"
```

Expected: `git diff --check` 无错误；提交后只剩实施前已存在的脏文件和保护文件。

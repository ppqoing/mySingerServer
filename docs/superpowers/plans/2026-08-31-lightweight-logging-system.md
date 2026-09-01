# Rust V2 轻量日志系统 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不引入日志平台和新持久化模型的前提下，确保所有可捕获的生产异常和操作错误最终至少记录一次，并让 `node.exe`、`worker.exe`、`desktop.exe` 的本地日志能够直接回答“哪个进程、哪个任务、哪个阶段、哪个文件、哪个 Worker、为什么失败、是否继续运行”。

**Architecture:** 保留现有 `tracing`、`tracing-subscriber` 和 `dedup_core::logging::SizeRotatingWriter`。三个进程继续写各自的 UTF-8 单行文本日志，以稳定的 `event` 字段和少量关联字段形成可检索契约；错误只在拥有最终处理结果的进程、任务或资源边界记录一次，向上传播的中间层不重复写。正常日志尚未初始化或不可写时退回 `%TEMP%\mySingerServer\logs\<process>-emergency.log`；panic hook 为避免 logger 重入而始终直接写该应急文件，Worker 原生崩溃由 Node 监督器记录。默认只记录 `INFO` 及以上，临时排障时用 `RUST_LOG` 打开 `DEBUG`。

**Tech Stack:** Rust 1.97.1、`tracing 0.1`、`tracing-subscriber 0.3`、同步大小滚动文件 writer、PowerShell 本地排障。

**Spec:** `AGENTS.md` 的 Rust V2 日志、任务、Worker 和数据所有权约束，以及本计划的“日志契约”。

## Global Constraints

- 不新增 Elasticsearch、Loki、OpenTelemetry、日志数据库、远程上传、压缩归档或后台日志线程。
- 不修改 Protobuf、SQLite/PostgreSQL schema、Node/Desktop 配置格式或 UI 页面。
- 保留现有日志目录、文件名和滚动规则：单文件 20 MiB，包含当前文件在内保留 10 个滚动文件。
- `worker.exe` 的 stdout 永远只传四字节长度分帧的 `WorkerEnvelope`；所有诊断仍只写文件。
- 日志格式保持人可读的单行文本，不切换 JSON/JSONL；机器检索依靠稳定英文 `event` 和字段名。
- 默认过滤级别固定为 `INFO`；只允许通过进程环境变量 `RUST_LOG` 临时提升到 `DEBUG/TRACE`，不得把 WARN/ERROR 关闭，不增加配置项。
- `INFO` 不记录逐文件成功、每次调度选择或高频进度；这些只在 `DEBUG` 中按需记录。
- 不记录 PostgreSQL DSN、密码、令牌、确认值、完整配置原文或媒体内容。
- Node 本机的单文件失败和 Worker 崩溃日志允许记录完整 `display_path`，因为现场诊断必须定位实际文件；Desktop 不复制该路径到中心日志。
- 日志写入失败不能改变已经提交的任务状态；数据库/任务状态失败也不能被“只写日志”替代。
- 所有可捕获的生产 `Err`、panic 和 Join 失败必须最终写入正常日志或应急日志；只有未产生错误值、并以 `Option`/领域枚举/显式状态表达的正常分支可以不写错误日志。
- Rust panic 必须由进程级 panic hook 记录线程、位置和消息；Tokio/线程 Join 错误必须由持有 JoinHandle 的边界记录。
- `.ok()`、`let _ =` 和忽略 `send/join/cleanup` 结果不得用于吞掉真实错误；外部 API 若用 `Err` 表达预期关闭或幂等 NotFound，也必须显式匹配并至少写一条低频 `expected_condition`，不能只靠注释丢弃错误值。
- 同一错误只记录一次根因；错误随后被转换成 UI、协议或任务终态时只附带关联 ID，不重复输出同一错误文本。
- 正常日志目录和 `%TEMP%` 应急目录同时不可写、操作系统强制终止进程、断电、内核终止以及 Node/Desktop 自身的不可恢复原生崩溃无法保证持久落盘；Worker 原生退出只能由仍存活的 Node 记录，验收报告必须如实标注这个物理边界。
- 新增或修改的方法、字段、常量和测试辅助类型使用中文注释说明职责和行为。
- 实施前必须重新检查目标 worktree 的分支、dirty state 和当前代码；不得 reset、clean 或覆盖用户未提交内容。

---

## 1. 当前基础与最小差距

现有实现已经具备可复用基础：

- `crates/core/src/logging.rs` 已提供同步 `SizeRotatingWriter` 和 20 MiB × 10 滚动测试。
- `node.exe` 写 `node.log`，`desktop.exe` 写 `desktop.log`，`worker.exe` 写 `worker-<pid>.log`。
- Node 托盘已经可以打开日志目录。
- `RuntimeTaskReporter::finish` 已写任务终态汇总。
- 基础计算已经记录单文件失败和 Worker 崩溃的任务、文件、PID、退出码、阶段字段。

本计划只补以下差距：

1. 三个入口没有统一的默认过滤策略，容易写入过多调试信息或产生行为差异。
2. 缺少稳定的 `event` 值，排障依赖中文消息全文搜索。
3. 进程启动/退出、任务开始、依赖降级等关键边界不完整。
4. 顶层 `?`、后台 Tokio/线程任务、传输读写循环和 UI 命令错误存在返回或显示但未写日志的路径。
5. 生产源码中存在大量 `let _ =`、`.ok()` 和未检查的 channel/Join 结果，尚未区分正常控制流与真实错误。
6. 日志初始化失败和初始化前错误没有固定应急落盘路径，panic 也没有统一记录入口。
7. 没有一份面向实际使用者的日志路径、字段和 PowerShell 检索说明。

## 2. 日志契约

### 2.1 文件布局

| 进程 | 当前文件 | 主要内容 |
|---|---|---|
| Node | `data/node/logs/node.log` | 节点生命周期、任务终态、Worker 监督、扫描降级、数据库降级、单文件失败 |
| Worker | `data/node/logs/worker-<pid>.log` | Worker 启动、FFmpeg 初始化、协议循环结束、进程内错误 |
| Desktop | `data/desktop/logs/desktop.log` | 管理端生命周期、节点连接、同步和中心库错误 |

正常运行目录不增加 `errors.log`、`audit.log` 或按任务拆分文件；唯一新增文件是 primary 不可用时的 `%TEMP%` 应急日志。一个事件只在最接近故障权威边界的进程记录一次，避免多份日志相互矛盾。

### 2.2 级别语义

| 级别 | 使用条件 | 示例 |
|---|---|---|
| `ERROR` | 进程/组件崩溃、当前操作因基础设施错误无法继续，或内部不变量破坏 | Worker 崩溃、SQLite 提交失败、WorkerPool 无法补建、Node 启动失败 |
| `WARN` | 已降级或局部失败，但主流程仍能继续 | Everything 回退 Walker、PostgreSQL 降级、单文件失败后继续 |
| `INFO` | 低频生命周期和终态汇总 | 进程启动/退出、任务开始/完成、Worker Ready |
| `DEBUG` | 临时排障所需的高频内部状态 | 单次调度选择、缓存命中细节、队列状态变化 |

同一故障不得先写 `WARN` 再以相同内容重复写 `ERROR`。Worker 进程崩溃本身固定记录一次 `ERROR`；若成功补位，不再追加通用“任务失败”错误。只有补位或持久化另行失败时，才为该独立基础设施错误再写一条事件。

### 2.3 稳定事件

| `event` | 级别 | 最少字段 |
|---|---:|---|
| `process_started` | INFO | `process`, `pid`, `version` |
| `process_stopped` | INFO | `process`, `pid`, `reason` |
| `process_failed` | ERROR | `process`, `pid`, `operation`, `error` |
| `process_panicked` | ERROR | `process`, `pid`, `thread`, `source_file`, `source_line`, `panic_message` |
| `background_task_failed` | ERROR | `component`, `task_name`, `operation`, `error` |
| `request_rejected` | WARN | `component`, `request_id`, `reason` |
| `request_failed` | ERROR | `component`, `request_id`, `operation`, `error` |
| `transport_connection_failed` | WARN | `peer`, `operation`, `error` |
| `transport_listener_failed` | ERROR | `listen_address`, `operation`, `error` |
| `configuration_rejected` | WARN | `process`, `setting`, `fallback`, `error` |
| `expected_condition` | INFO | `component`, `operation`, `reason`, `error` |
| `diagnostic_sink_failed` | ERROR | `process`, `primary_path`, `fallback_path`, `error` |
| `worker_ready` | INFO | `worker_pid` |
| `runtime_task_started` | INFO | `runtime_task_id`, `task_kind`, `machine_id`, `restored` |
| `runtime_task_terminal` | INFO | `runtime_task_id`, `task_kind`, `state`, `overall_completed`, `overall_failed`, `overall_skipped` |
| `file_failed` | WARN | `task_id`, `item_id`, `stage`, `display_path`, `error` |
| `worker_crashed` | ERROR | `task_id`, `item_id`, `worker_pid`, `worker_exit_code`, `crash_stage`, `display_path`, `error` |
| `dependency_fallback` | WARN | `dependency`, `fallback`, `error` |
| `central_store_degraded` | WARN | `operation`, `fallback`, `error` |
| `invariant_failed` | ERROR | `component`, `operation`, `error` |

约束：

- `event` 值和字段名使用稳定英文蛇形命名，中文消息只用于人读说明。
- 已有业务 ID 直接复用，不新建 trace/span ID。
- 不存在的 PID、退出码使用 tracing 的 `?Option<T>` 输出，不使用 `0` 伪装未知值。
- 同一任务终态只写一条 `runtime_task_terminal`。
- 错误消息保留原始 `Display` 文本，但不得把 DSN 或配置原文拼入错误上下文。

### 2.4 错误完整性契约

Rust 没有通用“异常”对象。本计划所称“可捕获错误”包含：

- 生产路径返回的 `Result::Err`；
- Tokio task、阻塞 task 和 OS 线程的 Join 错误；
- panic hook 能观察到的 Rust panic；
- Node 能观察到的 Worker 启动失败、管道失败、异常退出和退出码；
- TCP/Protobuf、文件系统、SQLite、PostgreSQL、FFmpeg、Windows API 与 UI 命令边界返回的错误。

每个错误必须选择且只能选择以下一种归属：

| 处理方式 | 记录位置 | 要求 |
|---|---|---|
| 当前层完全处理并继续 | 当前层的权威处理边界 | 写 WARN/ERROR，包含后续动作或降级结果 |
| 当前层只增加上下文并向上传播 | 最终接收 `Err` 的进程/任务/请求边界 | 中间层不写，避免同一根因重复 |
| 转成协议错误、UI 错误或任务失败 | 转换发生前 | 先写日志，再发送不含敏感信息的用户错误 |
| 进程初始化前失败 | `%TEMP%` 应急日志 | 写 `process_failed`；应急日志也失败时输出 stderr 并返回原错误 |
| Rust panic | `%TEMP%` 应急日志中的进程 panic hook | 绕过 tracing 直接写 `process_panicked`，随后保留默认 hook 行为 |
| Worker 原生崩溃 | Node WorkerPool/任务归并边界 | 写 `worker_crashed`，Worker 自身不假装能够捕获原生堆损坏 |
| 未产生错误值的正常控制流 | 不写错误日志 | 仅限 `Option::None`、领域枚举、显式 EOF/取消状态和已验证的缓存 miss；代码旁写中文说明 |
| 外部 API 以 `Err` 表达预期状态 | 当前层的权威处理边界 | 写一次 INFO `expected_condition`，保留原始错误与关闭/NotFound 等具体原因，不得静默丢弃 |

以下规则用于防止“看似处理、实际丢失”：

1. `main` 必须把初始化后的所有 `Err` 记录后再返回；初始化日志本身失败时写应急日志。
2. 所有拥有 JoinHandle 的结构必须在正常关闭时 `await/join` 并检查结果；Drop 中主动 `abort` 属于显式取消动作，但该动作或随后的 join 若返回错误仍按本契约记录。
3. detached task 必须在 task 内捕获顶层 `Result` 并写 `background_task_failed`，或把结果送回一个会检查它的 owner。
4. transport read/write loop 必须保留真实 Frame/Decode/IO 错误，不能只以 `break` 丢失原因。
5. `UiEvent::Error`、协议 ErrorEnvelope、`RuntimeTaskState::Failed` 和 `file_faults` 是错误结果，不替代日志；产生这些结果的权威边界必须已有对应事件。
6. `let _ =`、`.ok()`、`is_err()` 后直接 `break` 以及空 `Err(_)` 分支逐项审计；任何实际 `Err` 都改为显式匹配，并在当前层或最终 owner 记录。
7. 日志内容自身写入失败时，writer 把原日志行和 primary 错误写入应急文件；若两个 sink 都失败，只能把错误返回/写 stderr，不能声称已经持久化。

### 2.5 明确不做

- 不做日志搜索 UI、日志导出压缩包、自动上传和告警通知。
- 不做每个阶段的独立日志文件。
- 不做全链路分布式 trace，也不跨 Node/Desktop 传播日志关联 ID。
- 不使用 `catch_unwind` 让已经 panic 的业务流程继续运行；panic hook 只负责留下诊断，既有 unwind/abort 语义保持不变。
- 不实现 Windows minidump、Vectored Exception Handler 或内核级崩溃捕获；Worker 原生异常继续由父 Node 的进程退出事件覆盖。
- 不把高频流水线指标逐条写日志；运行时 Registry 和现有验收报告继续承担指标展示。
- 本轮不改变 `worker-<pid>.log` 的历史清理策略；若以后观察到磁盘增长，再用真实数据单独设计全局保留上限。

## 3. 文件职责映射

- Modify: `crates/core/Cargo.toml` —— 让已有 logging 模块复用 `tracing` 和 `tracing-subscriber::EnvFilter`。
- Modify: `crates/core/src/logging.rs` —— 提供默认过滤器、正常/应急 sink 状态、panic hook 和日志写入失败回退，保留现有滚动边界。
- Modify: `apps/node/src/main.rs` —— 最早安装 panic hook，接入统一过滤器，补 Node 启动、顶层失败、线程 Join 和退出事件。
- Modify: `apps/node/src/restart_lifecycle.rs` —— 配置加载/替换进程启动错误返回到顶层日志边界，不在中间层重复记录。
- Modify: `apps/worker/src/main.rs` —— 最早安装 panic hook，接入统一过滤器，确保 FFmpeg、协议循环和顶层错误返回前记录。
- Modify: `apps/worker/src/protocol_loop.rs` —— 为现有 Ready 日志增加稳定事件字段。
- Modify: `apps/desktop/src/main.rs` —— 最早安装 panic hook，接入统一过滤器，补 Desktop 顶层、事件 task、Shutdown 和退出错误。
- Modify: `crates/transport/Cargo.toml`、`crates/transport/src/connection.rs` —— 保留 TCP 读写循环的真实结束原因并交给连接 owner 记录。
- Modify: `crates/node-engine/src/server.rs` —— 记录请求拒绝、连接写失败和 listener 终止错误，正常断开不升级为错误。
- Modify: `crates/node-engine/src/actor.rs` —— 检查后台任务、持久终态、reply/send 和 Shutdown/Join 结果。
- Modify: `crates/node-engine/src/worker/process.rs`、`crates/node-engine/src/worker/pool.rs` —— 保留 stop/terminate 的次级错误并在 Worker owner 边界记录。
- Modify: `crates/desktop-core/Cargo.toml`、`crates/desktop-core/src/app.rs` —— 记录 controller、数据库检查、同步 worker 和 runtime watcher 的后台错误。
- Modify: `crates/desktop-ui/Cargo.toml`、`crates/desktop-ui/src/bindings.rs` —— UI 命令发送失败同时写日志并显示给用户，禁止记录数据库密码。
- Modify: `crates/node-engine/src/runtime_tasks.rs` —— 固定运行任务开始和终态事件。
- Modify: `crates/node-engine/src/scan/base_compute.rs` —— 统一单文件失败和 Worker 崩溃字段、级别与事件值。
- Modify: `crates/node-engine/src/scan/everything.rs` —— 固定 Everything 回退事件。
- Modify: `crates/node-engine/src/central_cache.rs` —— 固定中心库降级事件。
- Modify: `crates/node-engine/tests/runtime_tasks.rs` —— 验证任务日志恰好一次且字段完整。
- Modify: `apps/worker/tests/worker_protocol_process.rs` —— 验证 Worker Ready 只写文件且不污染协议 stdout。
- Create: `docs/operations/rust-v2-log-error-inventory.md` —— 先列出所有进程、请求、后台 task/thread、Worker item 和 UI callback 的最终错误出口，再逐项列出生产 `let _`、`.ok()`、Join/send/cleanup 等结果的归属与记录事件。
- Create: `docs/operations/rust-v2-logging.md` —— 记录日志位置、级别、字段、排障命令和敏感信息边界。

---

### Task 1: 建立过滤器、应急日志和 panic 捕获基础

**Files:**

- Modify: `crates/core/Cargo.toml`
- Modify: `crates/core/src/logging.rs`
- Create: `crates/core/tests/process_diagnostics.rs`

**Interfaces:**

- Produces: `dedup_core::logging::log_filter(value: Option<&str>) -> Result<tracing_subscriber::EnvFilter, LogFilterError>`
- Produces: `dedup_core::logging::log_filter_from_env() -> Result<tracing_subscriber::EnvFilter, LogFilterError>`
- Produces: `ProcessDiagnostics::new(process: &'static str) -> ProcessDiagnostics`
- Produces: `#[doc(hidden)] ProcessDiagnostics::with_emergency_path(process: &'static str, path: impl Into<PathBuf>) -> ProcessDiagnostics`，只供隔离测试注入路径。
- Produces: `ProcessDiagnostics::install_panic_hook(&self)`、`ProcessDiagnostics::mark_primary_ready(&self)`。
- Produces: `ProcessDiagnostics::record_warning(...)` 与 `ProcessDiagnostics::record_error(...)`；两者都接收稳定 `event`、`operation` 和原始 `Display` 错误。
- Produces: `FallbackLogWriter<W>::new(primary: W, primary_path: impl Into<PathBuf>, diagnostics: ProcessDiagnostics) -> FallbackLogWriter<W>`。
- Preserves: `SizeRotatingWriter::production(...)`、20 MiB × 10 轮转和同步写入语义。

- [ ] **Step 1: 写过滤、应急单行和 primary 失败回退的 RED 测试**

在 `crates/core/src/logging.rs` 增加三个测试：

```rust
#[test]
fn log_filter_defaults_to_info_and_accepts_explicit_directive() {
    assert_eq!(super::log_filter(None).unwrap().to_string(), "info");
    assert_eq!(super::log_filter(Some("")).unwrap().to_string(), "info");
    let explicit = super::log_filter(Some("dedup_node_engine=debug,info")).unwrap().to_string();
    assert!(explicit.contains("dedup_node_engine=debug"));
    assert!(explicit.contains("info"));
    assert!(super::log_filter(Some("[invalid")).is_err());
    assert!(super::log_filter(Some("off")).is_err());
    assert!(super::log_filter(Some("error")).is_err());
}

#[test]
fn emergency_log_is_single_line_and_contains_process_context() {
    let directory = tempfile::tempdir().unwrap();
    let diagnostics = ProcessDiagnostics::with_emergency_path("node", directory.path().join("node-emergency.log"));
    diagnostics.record_error("process_failed", "load_config", "第一行\n第二行");
    let log = std::fs::read_to_string(directory.path().join("node-emergency.log")).unwrap();
    assert_eq!(log.lines().count(), 1);
    assert!(log.contains("event=\"process_failed\""));
    assert!(log.contains("operation=\"load_config\""));
    assert!(log.contains("process=\"node\""));
    assert!(log.contains("第一行\\n第二行"));
}

#[test]
fn primary_write_failure_replays_original_line_to_emergency_log() {
    let directory = tempfile::tempdir().unwrap();
    let diagnostics = ProcessDiagnostics::with_emergency_path("worker", directory.path().join("worker-emergency.log"));
    let mut writer = FallbackLogWriter::new(AlwaysFailWriter, "primary.log", diagnostics);
    writer.write_all(b"event=\"worker_crashed\" error=\"boom\"\n").unwrap();
    let log = std::fs::read_to_string(directory.path().join("worker-emergency.log")).unwrap();
    assert!(log.contains("event=\"diagnostic_sink_failed\""));
    assert!(log.contains("worker_crashed"));
    assert!(log.contains("boom"));
}
```

`AlwaysFailWriter` 是测试内 `Write` 实现，`write` 和 `flush` 都返回固定 `io::ErrorKind::Other`。

- [ ] **Step 2: 运行测试确认接口缺失**

Run: `cargo test -p dedup-core logging`

Expected: FAIL，缺少 filter、`ProcessDiagnostics` 和 `FallbackLogWriter`。

- [ ] **Step 3: 实现默认过滤和进程诊断句柄**

`crates/core/Cargo.toml` 增加 `tracing.workspace = true` 与 `tracing-subscriber.workspace = true`。`log_filter` 先固定 `info`，再只接受 `info`、`debug`、`trace`、`<target>=debug` 和 `<target>=trace` 指令；`off/warn/error`、非法 target 和非 Unicode `RUST_LOG` 返回 `LogFilterError`。三个入口在安装 panic hook 后调用它，错误时先以 `configuration_rejected` 写应急日志，再继续使用固定 `info`，从而既不吞配置错误，也不允许环境变量关闭错误记录。诊断句柄只保存进程名、应急路径和一个共享 ready 标记：

```rust
/// 创建至少保留 INFO 的过滤器；非法或试图降级的指令显式返回错误。
pub fn log_filter(value: Option<&str>) -> Result<tracing_subscriber::EnvFilter, LogFilterError> {
    let mut filter = tracing_subscriber::EnvFilter::new("info");
    for directive in validated_directives(value)? {
        filter = filter.add_directive(directive.parse().map_err(LogFilterError::from)?);
    }
    Ok(filter)
}

/// 从 RUST_LOG 读取临时排障级别；变量缺失是默认状态，非法值返回错误。
pub fn log_filter_from_env() -> Result<tracing_subscriber::EnvFilter, LogFilterError> {
    match std::env::var("RUST_LOG") {
        Ok(value) => log_filter(Some(&value)),
        Err(std::env::VarError::NotPresent) => log_filter(None),
        Err(error) => Err(LogFilterError::Environment(error)),
    }
}

#[derive(Clone)]
pub struct ProcessDiagnostics {
    process: &'static str,
    emergency_path: Arc<PathBuf>,
    primary_ready: Arc<AtomicBool>,
}

impl ProcessDiagnostics {
    /// 为当前进程创建固定的 TEMP 应急日志位置。
    pub fn new(process: &'static str) -> Self {
        Self::with_emergency_path(
            process,
            std::env::temp_dir()
                .join("mySingerServer")
                .join("logs")
                .join(format!("{process}-emergency.log")),
        )
    }

    /// 仅供隔离行为测试把应急日志固定到临时目录。
    #[doc(hidden)]
    pub fn with_emergency_path(process: &'static str, path: impl Into<PathBuf>) -> Self {
        Self {
            process,
            emergency_path: Arc::new(path.into()),
            primary_ready: Arc::new(AtomicBool::new(false)),
        }
    }

    /// 正常 subscriber 安装成功后切换到 tracing；此前错误直接写应急文件。
    pub fn mark_primary_ready(&self) {
        self.primary_ready.store(true, Ordering::Release);
    }
}
```

`validated_directives` 对每个指令执行完整白名单校验，不把解析错误转成 `Option`。`record_warning`/`record_error` 共用一个私有单行写入函数：ready 前分别以 `level=WARN/ERROR` 把 `ts_unix_ms`、`event`、`process`、`pid`、`operation` 和经过换行转义的 `error` 追加到应急文件，ready 后分别调用 `tracing::warn!`/`tracing::error!`。应急目录创建或追加失败时调用 `eprintln!`，但调用方仍返回原始业务错误。

- [ ] **Step 4: 实现 primary writer 失败回退**

`FallbackLogWriter<W: Write>` 正常委托给 primary。primary 的 `write/flush` 返回错误时，使用不经过 tracing 的 `OpenOptions::append` 把 primary 错误和原日志行写到应急文件；应急写入成功则把原日志行视为已写入，应急写入也失败则返回 primary 错误。禁止从 fallback 再调用 tracing，避免递归。

- [ ] **Step 5: 安装 panic hook 并用隔离子进程验证**

`ProcessDiagnostics::install_panic_hook` 必须保存原 hook。新 hook 绕过 tracing，直接向应急文件记录 `event="process_panicked"`、进程、PID、线程名、源文件、行号和 panic payload，避免 panic 由 primary writer 引起时递归进入 logger；记录后调用原 hook，不使用 `catch_unwind` 继续业务流程。

在 `crates/core/Cargo.toml` 增加 harnessless `process_diagnostics` test。父测试进程复制自身为临时子进程，通过 `DEDUP_DIAGNOSTIC_TEST_PATH` 传递临时路径；子进程用 `with_emergency_path` 创建 diagnostics、安装 hook 后执行 `panic!("panic-log-sentinel")`。父进程断言子进程非零退出，且应急日志恰好一条 `process_panicked`，包含 sentinel、线程和源位置。这样不会在并行 Rust test harness 中替换全局 hook。

- [ ] **Step 6: 运行 logging 全部测试**

Run: `cargo test -p dedup-core logging`

Run: `cargo test -p dedup-core --test process_diagnostics`

Expected: PASS；原有轮转、默认 filter、应急单行、primary 回退和隔离 panic 落盘全部通过。

---

### Task 2: 覆盖三个进程、后台任务和传输错误边界

**Files:**

- Modify: `apps/node/src/main.rs`
- Modify: `apps/node/src/restart_lifecycle.rs`
- Modify: `apps/worker/src/main.rs`
- Modify: `apps/desktop/src/main.rs`
- Modify: `crates/transport/Cargo.toml`
- Modify: `crates/transport/src/connection.rs`
- Modify: `crates/transport/src/lib.rs`
- Modify: `crates/node-engine/src/server.rs`
- Modify: `crates/node-engine/src/actor.rs`
- Modify: `crates/node-engine/src/worker/process.rs`
- Modify: `crates/node-engine/src/worker/pool.rs`
- Modify: `crates/desktop-core/Cargo.toml`
- Modify: `crates/desktop-core/src/app.rs`
- Modify: `crates/desktop-ui/Cargo.toml`
- Modify: `crates/desktop-ui/src/bindings.rs`

**Interfaces:**

- Consumes: Task 1 的 `ProcessDiagnostics`、`FallbackLogWriter` 和 `log_filter_from_env`。
- Preserves: Node/Worker/Desktop 的公开接口、Worker stdout 协议、网络错误响应和任务状态机。
- Produces: 顶层 `process_failed/process_panicked`、后台 `background_task_failed`、请求与 transport 稳定事件。

- [ ] **Step 1: 写进程和后台错误边界的 RED 测试**

增加或扩展测试，固定以下行为：

- 非法或非 Unicode `RUST_LOG` 在 subscriber 初始化前恰好写一条 WARN `configuration_rejected` 到应急日志，随后进程以 INFO 继续启动。
- transport 收到无效 Protobuf 时恰好写一条 `transport_connection_failed`，包含 peer 和 `operation="decode"`；显式 EOF/取消状态不写错误日志，底层若以 `Err` 返回预期关闭则写一条 INFO `expected_condition`。
- Node 首帧不是 Hello 时写 `request_rejected`，错误响应写失败时写 `transport_connection_failed`。
- Desktop controller 命令返回 `Err` 时，既发送 `UiEvent::Error`，也写一条 `request_failed`；日志不得包含测试 DSN 密码哨兵。
- Tokio task 返回 JoinError 或 OS 线程 join 失败时写 `background_task_failed`，不能只转换成字符串返回。

测试使用线程局部 tracing subscriber 和 `SharedLogBuffer`，断言每个注入错误事件只出现一次。

- [ ] **Step 2: 三个入口改成统一的最外层错误包装**

每个 `main` 在读取配置、解析 `RUST_LOG`、加载 FFmpeg 或启动 runtime 前创建 diagnostics 并安装 panic hook；过滤值无效时先用 `record_warning` 写 `configuration_rejected` 并回退固定 INFO，正常 subscriber 创建后调用 `mark_primary_ready`。入口结构固定为：

```rust
fn main() -> Result<(), Box<dyn std::error::Error>> {
    let diagnostics = ProcessDiagnostics::new("node");
    diagnostics.install_panic_hook();
    match run(&diagnostics) {
        Ok(()) => Ok(()),
        Err(error) => {
            diagnostics.record_error("process_failed", "run", error.as_ref());
            Err(error)
        }
    }
}
```

Worker 使用 `Box<dyn Error + Send + Sync>`，Desktop/Node 保持现有错误类型。`run` 内不得在每个 `?` 前重复记录；最外层负责未处理错误。

- [ ] **Step 3: 三个 subscriber 使用 fallback writer 并写生命周期事件**

三个进程的 subscriber 都加入统一 EnvFilter，并用 `FallbackLogWriter<SizeRotatingWriter>` 包装 primary；Node 的 `CloseableLogWriter` 改为持有该 wrapper，但仍在退出前同步 flush/close。日志初始化成功后写 `process_started`，正常退出前写 `process_stopped`。Worker stdout 不得发生任何日志写入。

- [ ] **Step 4: 检查所有 owned task/thread 的结束结果**

- Node runtime OS 线程必须检查 `join()`；错误由最外层 `process_failed` 记录。
- Desktop `event_task` 超时后先 `abort()`，再 await 并区分主动取消与 panic；panic/JoinError 写 `background_task_failed`。
- `DesktopApp::start` 的 controller task、同步 worker、runtime watcher 以及 Node 的 server/background task：若 owner 会 await，owner 检查结果；若设计上 detached，则 task 自己匹配顶层结果并记录。
- 显式 Shutdown/取消状态不写错误日志；channel API 若实际返回 Send/Recv/Join `Err`，即使原因是 owner 已结束，也写一次 INFO `expected_condition` 并在代码旁说明归属。

- [ ] **Step 5: 保留 transport 真实错误而不是无条件 break**

`ClientConnection::from_stream` 在 split 前冻结 peer。read/write loop 显式匹配 Frame、Decode、IO 和正常 EOF：已经建立的连接发生故障时只在 transport loop 写一次 `transport_connection_failed`，随后 `fail_all`；上层收到派生的 `ConnectionClosed` 只更新会话/UI，不重复记录同一根因。`TcpStream::connect` 在 loop 建立前失败时由调用它的 NodeSession/Desktop 记录。事件接收端正常关闭只结束事件投递。Node `serve_connection_io` 对握手拒绝、写错误、request task JoinError 和 `response_flushed` 失败分别记录稳定事件，listener `accept` 失败写 `transport_listener_failed` 后返回原 `ServerError`。

- [ ] **Step 6: 覆盖 UI、同步和 Worker 次级错误**

- `UiEvent::Error` 发送前写一次 `request_failed`；UI 命令队列失败同时写日志和 `last_error`。
- 数据库诊断、节点同步和 runtime watcher 的后台错误在各自 owner 边界记录；result channel 已因正常关闭而消失只写 DEBUG。
- `WorkerProcess::stop_after_failure` 不再用 `.ok()` 丢弃 terminate 错误；返回“原始错误 + 收束错误”给 WorkerPool，由 Pool 记录一次包含 PID/slot/item 的事件。
- `Drop::start_kill` 失败仅在进程仍可能存活时写 WARN；进程已退出的 InvalidInput 视为正常幂等结果。

- [ ] **Step 7: 运行边界定向测试**

Run: `cargo test -p dedup-transport`

Run: `cargo test -p dedup-node-engine server`

Run: `cargo test -p dedup-desktop-core controller`

Run: `cargo check -p node -p worker -p desktop`

Expected: PASS；每个注入错误恰好一条日志，正常断开/取消不产生错误日志，三进程编译通过。

---

### Task 3: 固定运行任务开始与终态日志

**Files:**

- Modify: `crates/node-engine/src/runtime_tasks.rs`
- Modify: `crates/node-engine/tests/runtime_tasks.rs`

**Interfaces:**

- Preserves: `RuntimeTaskRegistry::begin(...) -> RuntimeTaskReporter`
- Preserves: `RuntimeTaskRegistry::begin_restored(...) -> RuntimeTaskReporter`
- Preserves: `RuntimeTaskReporter::finish(RuntimeTaskState) -> Result<(), RuntimeTaskError>`
- Produces: 每个运行任务一条 `runtime_task_started` 和恰好一条 `runtime_task_terminal`。

- [ ] **Step 1: 把现有终态测试扩展成完整契约 RED**

将 `terminal_transition_writes_one_structured_log` 扩展为同时断言：

```rust
assert_eq!(log.matches("event=\"runtime_task_started\"").count(), 1);
assert_eq!(log.matches("event=\"runtime_task_terminal\"").count(), 1);
assert!(log.contains("task_kind=\"base_compute\""));
assert!(log.contains("restored=false"));
assert!(log.contains("state=\"completed\""));
assert!(log.contains("overall_completed=7"));
assert!(log.contains("overall_failed=1"));
assert!(log.contains("overall_skipped=1"));
```

另加一个 `begin_restored` 测试，断言开始事件使用持久任务 ID 且 `restored=true`：

```rust
#[tokio::test]
async fn restored_task_start_log_uses_persistent_identity() {
    let output = SharedLogBuffer::default();
    let subscriber = tracing_subscriber::fmt()
        .without_time()
        .with_ansi(false)
        .with_target(false)
        .with_writer(output.clone())
        .finish();
    let _guard = tracing::subscriber::set_default(subscriber);
    let task_id = dedup_core::TaskId::new();
    let task_text = task_id.as_uuid().to_string();
    let task = dedup_node_store::TaskSnapshot {
        task_id,
        kind: "scan".into(),
        status: dedup_node_store::TaskStatus::Queued,
        event_seq: 1,
        total_items: 2,
        succeeded: 1,
        failed: 0,
        cancelled: 0,
        outbox_high_seq: 0,
    };
    let registry = RuntimeTaskRegistry::new();
    registry
        .begin_restored(
            RuntimeTaskKind::BaseCompute,
            MachineId::from_sha256([0x85; 32]),
            "恢复日志",
            &task,
            &[],
            100,
        )
        .await;
    drop(_guard);

    let log = output.text();
    assert!(log.contains(&format!("runtime_task_id=\"{task_text}\"")));
    assert!(log.contains("event=\"runtime_task_started\""));
    assert!(log.contains("restored=true"));
}
```

- [ ] **Step 2: 运行测试确认事件字段缺失**

Run: `cargo test -p dedup-node-engine --test runtime_tasks terminal_transition_writes_one_structured_log -- --exact`

Expected: FAIL，现有日志没有稳定 `event`、`task_kind` 和 `restored` 字段。

- [ ] **Step 3: 在创建和恢复边界写开始事件**

任务插入 Registry 后、返回 Reporter 前写一条：

```rust
tracing::info!(
    event = "runtime_task_started",
    runtime_task_id = %task_id,
    task_kind = kind.as_str(),
    machine_id = %machine_id.as_str(),
    restored = false,
    "运行任务已开始"
);
```

`begin_restored` 使用 `restored=true`。不得记录高频进度或完整任务输入。

- [ ] **Step 4: 扩充现有终态事件而不新增第二条日志**

在 `finish` 获取锁期间一并复制 `task.kind`、`task.machine_id` 和 `task.overall_total`，随后把现有日志改为：

```rust
tracing::info!(
    event = "runtime_task_terminal",
    runtime_task_id = %self.task_id,
    task_kind = kind.as_str(),
    machine_id = %machine_id,
    state = state.as_str(),
    overall_completed,
    overall_total = ?overall_total,
    overall_failed,
    overall_skipped,
    has_pipeline_metrics,
    "运行任务进入终态"
);
```

重复 `finish` 仍必须在写日志前返回 `RuntimeTaskError::Terminal`。

- [ ] **Step 5: 运行任务日志测试**

Run: `cargo test -p dedup-node-engine --test runtime_tasks`

Expected: PASS；开始和终态各一条，重复终态无第二条日志。

---

### Task 4: 统一 Worker、文件失败和降级事件

**Files:**

- Modify: `apps/worker/src/protocol_loop.rs`
- Modify: `apps/worker/tests/worker_protocol_process.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/scan/everything.rs`
- Modify: `crates/node-engine/src/central_cache.rs`
- Test: 复用 `crates/node-engine/src/scan/base_compute.rs` 内现有日志捕获测试

**Interfaces:**

- Preserves: `WorkerEvent`、Worker IPC、任务状态和数据库写入顺序。
- Produces: `worker_ready`、`file_failed`、`worker_crashed`、`dependency_fallback`、`central_store_degraded` 五类稳定事件。

- [ ] **Step 1: 先扩展现有日志测试为 RED**

在 `worker_crash_log_contains_full_path_and_process_context` 增加：

```rust
assert!(log.contains("event=\"worker_crashed\""));
assert!(log.contains("task_id=\"task-log\""));
assert!(log.contains("item_id=\"item-log\""));
assert!(log.contains("worker_pid=Some(4102)"));
assert!(log.contains("worker_exit_code=Some(-1073740940)"));
assert!(log.contains(r"崩溃样本.mp4"));
```

在 Worker 进程测试的隔离子进程中，用 `DEDUP_WORKER_TEST_LOG` 指定临时日志文件；`run_child_protocol` 只为测试安装一个 file-only subscriber：

```rust
if let Some(path) = env::var_os("DEDUP_WORKER_TEST_LOG") {
    let file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)?;
    tracing_subscriber::fmt()
        .without_time()
        .with_ansi(false)
        .with_target(false)
        .with_writer(std::sync::Mutex::new(file))
        .try_init()?;
}
```

父进程在 `WorkerPool::start` 返回后读取该临时文件，断言 `event="worker_ready"`。现有测试仍通过 WorkerPool 解码 stdout 的 Protobuf 帧，从而证明日志没有污染协议。这个测试 subscriber 只服务隔离子进程，不替代生产 `SizeRotatingWriter`。

- [ ] **Step 2: 运行测试确认稳定事件缺失**

Run: `cargo test -p dedup-node-engine --features test-hooks worker_crash_log_contains_full_path_and_process_context -- --exact`

Run: `cargo test -p worker --test worker_protocol_process`

Expected: 至少一个 FAIL，原因是稳定 `event` 字段尚未完整接入。

- [ ] **Step 3: 为现有日志增加字段并统一级别**

- Worker Ready：增加 `event="worker_ready"`、`worker_pid`。
- 单文件读取/媒体许可/计算失败并继续：统一为 `WARN`，增加 `event="file_failed"`、`task_id`、`item_id`、`stage`、`display_path`、`error`。
- Worker 崩溃：保留现有 `ERROR`，增加 `event="worker_crashed"`；成功补位时不重复写通用错误，无法补位时只为“补位失败”再写独立 `ERROR`。
- Everything 回退：`WARN` + `event="dependency_fallback"`、`dependency="everything"`、`fallback="windows_walker"`。
- PostgreSQL 缓存不可用且降级本地：`WARN` + `event="central_store_degraded"`、`fallback="sqlite_only"`。

不得改变 `file_faults` 事务、Worker 补位、任务继续或取消行为。

- [ ] **Step 4: 防止重复记录同一故障**

沿调用链检查每类故障的权威边界：

- Worker 具体文件崩溃由 `log_worker_crash` 记录一次；上层只有任务真的停止时才再写基础设施 `ERROR`。
- Everything 启动失败和“整次回退”合并为一条最终回退事件；低层错误通过字段附带，不重复写相同 WARN。
- PostgreSQL 同一次操作只在决定降级的位置写一次 WARN。

- [ ] **Step 5: 运行定向回归**

Run: `cargo test -p dedup-node-engine --test runtime_tasks`

Run: `cargo test -p dedup-node-engine --features test-hooks worker_crash_log_contains_full_path_and_process_context -- --exact`

Run: `cargo test -p worker --test worker_protocol_process`

Expected: PASS；协议、任务状态和既有 Worker 生命周期测试不变。

---

### Task 5: 完成错误吞没审计、使用说明和轻量验收

**Files:**

- Create: `docs/operations/rust-v2-log-error-inventory.md`
- Create: `docs/operations/rust-v2-logging.md`
- Verify: 所有 workspace 生产 `lib` 和 `bin` target；测试/示例只检查会进入正式验收包的入口。

**Interfaces:**

- Consumes: Tasks 1–4 的日志路径、错误边界、过滤规则和稳定事件表。
- Produces: 零未分类错误结果的审计清单，以及一页可直接复制命令的本地排障说明。

- [ ] **Step 1: 建立生产错误结果清单**

先在 `docs/operations/rust-v2-log-error-inventory.md` 建立“最终错误出口表”，固定列为：

| 执行根 | owner | 最终结果/异常 | 记录 event | 恰好一次策略 | 行为测试 |
|---|---|---|---|---|---|

执行根必须完整覆盖三个 `main/run`、每个 `tokio::spawn`/`spawn_blocking`/`thread::spawn`、TCP listener/connection/request、UI callback/command、Worker item/session/protocol loop/process monitor、重启生命周期和含 fallible cleanup 的 Drop owner。每个执行根都必须说明 `Err`、JoinError 和 panic 最终由谁记录；不得只写“上层处理”。

然后对 `apps` 和 `crates` 的非测试生产代码执行三组搜索：

```powershell
rg -n --glob '*.rs' --glob '!**/tests/**' --glob '!target/**' `
  'let _ =|\.ok\(\)|\.is_err\(\)|Err\(_\)|tokio::(task::)?spawn|spawn_blocking|thread::(spawn|Builder)|JoinSet|\.spawn\(|\.join\(\)|\.abort\(\)' `
  apps crates

rg -n --glob '*.rs' --glob '!**/tests/**' --glob '!target/**' `
  'return Err|Err\(|map_err|\?|unwrap\(|expect\(|panic!|catch_unwind|process::exit|process::abort' `
  apps crates

rg -n --glob '*.rs' --glob '!**/tests/**' --glob '!target/**' `
  'send\(|try_send\(|recv\(|try_recv\(|flush\(|shutdown\(|terminate\(|start_kill\(' `
  apps crates
```

第一组和第三组的每个 fallible 命中都写入同一文档的“结果消费表”，使用固定列：

| 位置 | fallible 操作 | 分类 | 最终记录边界/event | 不写错误日志的原因 | 验证 |
|---|---|---|---|---|---|

分类只能是 `真实错误`、`向上传播`、`预期 Err`、`非错误状态/非 Result`。`真实错误` 必须指向具体 WARN/ERROR 事件和行为测试；`向上传播` 必须指向最终记录者；`预期 Err` 必须指向 INFO `expected_condition`；只有没有错误值的 `Option`/枚举/显式状态才能归为 `非错误状态/非 Result`。第二组用于逐个执行根追踪 `?`/`Err`/panic 的最终出口，不要求把每个中间 `?` 重复抄入表，但不得存在到不了已登记 owner 的传播链。不得留下空白、笼统“忽略”或“不会失败”。

- [ ] **Step 2: 消除所有未分类的错误吞没**

按清单修改生产代码：

- `let _ = fallible_call()` 改为显式 `match/if let Err`；真实错误记录，预期 `Err` 写 `expected_condition`，没有错误值的正常状态写中文注释。
- 生产代码不使用 `.ok()` 抹掉 `Result` 的错误载荷；真正可选的值改为显式 `match` 并返回 `Option`/领域枚举，IO、DB、网络、解析、任务 Join、进程终止和状态写入错误必须保留原因。
- `is_err()` 后直接 `break/return` 必须先保存并处理具体 error。
- `Err(_)` 必须绑定变量；即使是 receiver 关闭或幂等 NotFound，也写一次 `expected_condition` 后再继续。
- detached task 要么返回结果给 owner，要么在 task 内记录顶层错误。

然后运行生产 target lint：

Run: `cargo clippy --workspace --lib --bins -- -D unused_must_use -D clippy::let_underscore_must_use`

Expected: PASS。若 Clippy 命中预期状态，也必须改成显式分支，不能用 `drop(result)` 或 `#[allow]` 绕过审计。

- [ ] **Step 3: 写日志使用说明**

文档只包含以下内容：

1. 三类日志的实际目录和滚动文件命名。
2. 默认 INFO 及临时启用 DEBUG 的 PowerShell 命令：

```powershell
$env:RUST_LOG = 'info,dedup_node_engine=debug'
```

3. 按事件和任务检索的命令：

```powershell
rg 'event="(worker_crashed|file_failed|runtime_task_terminal)"' .\data\node\logs
rg 'task_id="<任务ID>"|runtime_task_id="<运行任务ID>"' .\data\node\logs
rg 'event="(dependency_fallback|central_store_degraded)"' .\data\node\logs
```

4. 需要提交问题时应提供的最小信息：问题时间、Node 日志、相关 Worker PID 日志、任务 ID；无需上传数据库和媒体文件。
5. 应急日志 `%TEMP%\mySingerServer\logs\<process>-emergency.log` 的用途和清理方式。
6. 明确禁止公开 DSN、密码和含隐私的完整路径日志。
7. 明确哪些情况无法保证落盘：两个 sink 同时不可写、断电、OS 强杀和 Node/Desktop 自身原生崩溃；Worker 原生退出依赖 Node 仍然存活。

- [ ] **Step 4: 执行格式、lint 和定向测试**

Run: `cargo fmt --all -- --check`

Run: `cargo test -p dedup-core logging`

Run: `cargo test -p dedup-core --test process_diagnostics`

Run: `cargo test -p dedup-transport`

Run: `cargo test -p dedup-node-engine --test runtime_tasks`

Run: `cargo test -p worker --test worker_protocol_process`

Run: `cargo check -p node -p worker -p desktop`

Run: `cargo clippy --workspace --lib --bins -- -D unused_must_use -D clippy::let_underscore_must_use`

Expected: 全部 PASS。

- [ ] **Step 5: 做一次隔离目录错误矩阵烟雾验收**

使用测试夹具或现有 runtime acceptance 入口，在临时目录启动一轮最小任务；不得使用正式 `I:\Tool` 数据目录。检查：

- Node、Worker、Desktop 各自产生日志文件。
- 配置/日志初始化前注入一个错误后，应急日志出现一条 `process_failed`。
- 子进程注入 Rust panic 后出现一条 `process_panicked`，包含线程、源位置和消息。
- primary writer 注入失败后，原事件与 `diagnostic_sink_failed` 出现在应急文件。
- 默认 INFO 下没有逐文件成功或调度 DEBUG 洪泛。
- 任务开始与终态能用同一个 `runtime_task_id` 关联。
- 注入一次可恢复 Worker 退出后，Node 日志只有一条对应 `worker_crashed`，字段包含任务、文件、PID、退出码和阶段，后续文件继续。
- 一次 Everything 或 PostgreSQL 降级只产生一条最终 WARN。
- Worker stdout 仍可完整解码为 `WorkerEnvelope`。
- TCP 无效帧、后台 task 失败、UI 命令失败、SQLite 写失败各自出现一条对应事件；同一根因不得在相邻层重复。
- 注入 receiver 正常关闭和幂等 NotFound 的 `Err` 后，各自出现一条 INFO `expected_condition`；用 `Option`/枚举表达的缓存 miss 不伪装成错误。

- [ ] **Step 6: 核对敏感信息、文件上限和完整性账本**

用 `rg` 检查真实测试日志不包含测试 DSN、密码或完整配置原文；使用小上限 writer 单元测试确认轮转数量不回归。最后逐行复核两张清单：每个执行根都有最终 owner，每个 `真实错误` 有事件与测试，每个 `向上传播` 能到达记录者，每个 `预期 Err` 有 `expected_condition`，每个 `非错误状态/非 Result` 都确实没有错误载荷；未分类数量必须为 0。

## 4. 完成标准

1. 每个可捕获的生产 `Err`、panic 和 Join 失败都能在错误清单中追踪到一条实际日志；只有没有错误载荷的 `Option`/枚举/显式状态可以作为不写错误日志的正常控制流，未分类数量为 0。
2. 三个进程的顶层 `Err`、Rust panic、后台 task/thread Join 错误、传输错误和 Worker 异常退出均有行为测试。
3. 正常日志不可写时自动使用 `%TEMP%` 应急日志；两个 sink 同时不可写时返回/显示原错误且不虚报已落盘。
4. 三个进程默认只记录 INFO/WARN/ERROR，`RUST_LOG` 可以临时打开模块 DEBUG/TRACE，但 `off/warn/error` 不能关闭错误记录。
5. 日志目录、文件名、20 MiB × 10 滚动和 Worker stdout 协议边界保持不变。
6. 进程、任务、文件失败、Worker 崩溃、Everything 回退和中心库降级都有稳定 `event` 字段。
7. 任一 Worker 文件故障能通过 `task_id/item_id/worker_pid/display_path/stage` 在 Node 日志中直接定位。
8. 同一任务只有一条开始事件和一条终态事件；同一根因不在相邻层重复记录。
9. INFO 不写高频进度和逐文件成功日志。
10. 日志不包含 DSN、密码、令牌、确认值和配置原文。
11. 格式、定向测试、workspace 生产 lint、三进程编译和隔离错误矩阵验收全部通过；未执行的真实运行验收必须明确标记为未验证。


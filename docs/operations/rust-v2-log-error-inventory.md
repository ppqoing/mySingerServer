# Rust V2 日志错误完整性清单

本清单覆盖 Windows x64 正式 `lib`/`bin` target。目标是让每个可捕获的生产 `Err`、panic 和
Join 失败都能追踪到一个最终记录 owner；中间层只增加上下文并向上传播，不重复记录根因。
`#[cfg(test)]`、单元测试和不进入安装包的 `examples` 单独列在末尾，不冒充生产覆盖。

## 归属规则

1. 当前层决定继续、降级或丢弃结果时，当前层记录一次。
2. 当前层用 `?` 或 `map_err` 向上传播时，最终进程、任务、请求或资源 owner 记录。
3. 正常关闭、主动取消、receiver 先结束和幂等 NotFound 仍显式消费 `Err`，记录
   `expected_condition`。
4. Tokio/线程 JoinHandle 必须被 owner 检查；detached 任务由观察器消费 JoinError。
5. `Option::None`、缓存 miss、二分查找未命中、原子 compare-exchange 未命中等没有错误载荷的
   状态不写错误日志。

## 最终错误出口表

| 执行根 | owner | 最终结果/异常 | 记录 event | 恰好一次策略 | 行为验证 |
|---|---|---|---|---|---|
| `node.exe main/run` | Node 进程入口 | 初始化、配置、runtime 线程、运行期顶层 `Err` | `process_failed`、`process_panicked`、`process_stopped` | panic hook 直写应急文件；普通错误只在 main 记录 | `process_diagnostics`、三进程 `cargo check` |
| Node runtime OS 线程 | `apps/node::run` | 线程 panic/Join 失败 | `process_panicked`，随后 `process_failed` | main 检查 `join()`，不由线程外再复制业务根因 | Node 编译与 panic hook 测试 |
| `worker.exe main/run` | Worker 进程入口 | 日志、FFmpeg、协议循环顶层 `Err` | `process_failed`、`process_panicked` | stdout 仅保留 Protobuf；日志只写文件 | `worker_protocol_process` |
| Worker 请求流水线 | Node 文件任务 owner | 探测、解码、联系表和载荷错误 | Node 持久终态 `file_failed` | Worker 用结构化 `WorkerFailure` 向上传播，不重复写同一根因；FFI 中无法传播的原始 IO 例外地在回调边界记录 | Worker 协议进程测试、Node 基础计算日志测试 |
| `desktop.exe main/run` | Desktop 进程入口 | 配置、GUI、event task、Shutdown 错误 | `process_failed`、`request_failed`、`background_task_failed` | event task 由 `settle_event_task` 唯一 join | Desktop main 单元测试、三进程 check |
| TCP client read/write loop | Transport 观察器 | Frame/Decode/IO、loop panic | `transport_connection_failed`、`background_task_failed` | 连接错误由原 loop 记录一次；观察器只记录 JoinError | `dedup-transport` 测试 |
| Transport pending response | `PendingRequests` | oneshot receiver 已关闭 | `expected_condition` | send 失败只在 pending owner 记录 | `dedup-transport` 测试 |
| Node TCP listener | `NodeServer::serve_until` / `NodeRuntime` | accept、listener、server JoinError | `transport_listener_failed`、`background_task_failed` | server 返回值由 NodeRuntime await；不在 accept 中间层重复 | `node_server` |
| Node connection/request JoinSet | NodeServer | 无效首帧、Busy、请求 panic/取消 | `request_rejected`、`request_failed`、`expected_condition`、`background_task_failed` | JoinSet drain 区分 owner abort 与 panic | `node_server`、transport 测试 |
| Node actor | `NodeRuntime` | actor panic、server/actor JoinError | `background_task_failed` | `NodeRuntime::shutdown` 持有并 await 两个 handle | Node server/runtime tests |
| Node 媒体后台作业 | EngineState observer | 作业 panic、归还 Pool/完成通知失败 | `background_task_failed`、`expected_condition` | 作业 handle 由 observer 唯一 join；业务失败由任务终态记录 | runtime task tests |
| 磁盘读取调度 actor | `DiskReadScheduler` observer | actor panic | `background_task_failed` | observer 消费 JoinError；命令 receiver 正常关闭是 actor 正常返回 | workspace lint/check |
| 扫描枚举 `spawn_blocking` | ScanEngine | 枚举 `Err`、JoinError、取消 | `file_failed`/任务错误最终边界，`expected_condition` | `join_enumeration` 返回到 ScanEngine，任务 owner 最终记录 | scan engine tests |
| 扫描并行读取 JoinSet | ScanEngine drain | 单文件 ReadFailure、JoinError、取消 | `file_failed`、`background_task_failed`、`expected_condition` | 正常归并持久化；错误收束逐项检查 | scan engine tests、workspace tests |
| Hash/媒体许可 JoinSet | BaseCompute drain | ReadFailure、JoinError、取消 | `file_failed`、`background_task_failed`、`expected_condition` | 正常项由主循环处理；错误退出后的剩余项由 drain 处理 | base compute tests |
| 缓存 resolver actor | BaseCompute owner | resolver JoinError、引用泄漏 panic | `background_task_failed`、`process_panicked` | handle 由基础计算关闭路径 await；内部引用不变量直接 panic | cache resolver tests |
| PostgreSQL 缓存查询 JoinSet | CacheResolver | 查询失败、降级取消、JoinError | `central_store_degraded`、`expected_condition`、`background_task_failed` | 首个根因写一次降级；同批取消只写预期条件 | cache resolver tests |
| BaseStore OS 线程 | `BaseStoreActor::close` | 调用/持久化错误、线程 panic | 业务错误向任务 owner 传播；JoinError 为 `background_task_failed` | close 使用 `spawn_blocking` join 并返回原 store | base persistence tests |
| WorkerPool actor/outbox | Pool task observer | actor/outbox panic、receiver 关闭 | `background_task_failed`、`expected_condition` | observer 只检查 JoinError；send 错误由 outbox owner 处理 | WorkerPool tests |
| Worker slot task | slot observer | 进程管道错误、slot panic | `worker_crashed`/基础设施事件、`background_task_failed` | slot 事件带 PID/退出码；observer 只补 JoinError | WorkerPool 与 crash 日志测试 |
| Worker 子进程 | WorkerPool / WorkerProcess | 启动、Ready 超时、管道、异常退出、terminate 错误 | `worker_crashed`、`request_failed`、`expected_condition` | Node 是进程监督权威；计划终止不伪装崩溃 | Worker protocol/process tests |
| FFmpeg AVIO FFI 回调 | `decode.rs` FFI 边界 | read/seek IO、非法范围、回调 panic | `file_failed`、`request_rejected`、`invariant_failed`、`background_task_failed` | C ABI 只能返回错误码，因此在返回前记录；panic 不跨 FFI | media-ffmpeg tests、workspace check |
| CentralStore PG driver | `CentralStore` | connection future 错误、abort JoinError | `background_task_failed`、`expected_condition` | 连接体记录驱动错误；owned handle 在校验失败/Drop 时被观察 | central-store tests |
| 临时数据库诊断 driver | `inspect_database` | schema、连接、JoinError | `request_failed`、`background_task_failed`、`expected_condition` | schema 失败仍返回逐表诊断，但先记录根因 | desktop/central-store tests |
| Desktop controller | `DesktopApp` / desktop main | controller panic/JoinError | `background_task_failed` | main 关闭时 `wait_for_controller` 唯一 join | desktop-core tests |
| Desktop DB 诊断 task | `observe_background_task` | 请求错误、JoinError | `request_failed`、`background_task_failed` | task 内记录业务 `Err`，观察器只记录 JoinError | desktop-core tests |
| Desktop 节点同步 worker | `NodeSyncWorker::drop` observer | 同步错误、abort JoinError | `central_store_degraded`、`request_failed`、`expected_condition` | 同步结果发回 controller；Drop 的主动 abort 单独标记预期 | desktop-core tests |
| Desktop runtime watcher | `NodeRuntimeWatcher::drop` observer | 传输失败、abort JoinError | `request_failed`、`expected_condition`、`background_task_failed` | 断线结果发回 controller；旧代 watcher 主动取消 | desktop-core tests |
| Desktop 并行重连 JoinSet | controller | connect `Err`、JoinError | `request_failed`、`background_task_failed` | 每次连接结果只由 controller 归并一次 | desktop-core tests |
| Slint UI callback/command | `bindings::send` 和 controller | 输入校验、try_send、command `Err` | `request_rejected`、`request_failed`、`expected_condition` | 草稿编辑仅表示 dirty；真正保存/命令执行时记录具体错误 | desktop-ui check、controller tests |
| Windows 回收站 STA 线程 | `move_to_recycle_bin` 调用方 | COM `Err`、线程 panic | 向删除任务 owner 传播，最终 `file_failed`/任务错误 | `join()` 错误转 `io::Error`；panic hook另保留 panic | windows/node-engine tests |
| Node 替代进程生命周期 | node main / host control | spawn、等待父 PID、参数解析错误 | `process_failed`、`request_failed` | Windows 边界保留原始 `io::Error` 向最终 owner 传播 | restart lifecycle tests/check |
| 磁盘满清理与可再生产物 cleanup | `DiskFullCleaner` | resolver、metadata、remove、锁中毒、SQLite cleanup | `file_failed`、`expected_condition`、`invariant_failed`、`background_task_failed` | NotFound 与真实 IO 错误分开；DB cleanup 继续向上传播 | disk-full cleanup tests |
| RuntimeTaskRegistry 广播 | Registry/Reporter | receiver 关闭、终态持久更新失败 | `expected_condition`、`background_task_failed`、`runtime_task_terminal` | 终态事件只在成功进入终态后一次 | `runtime_tasks` |

## 结果消费表

下表按相同 owner 和相同处理策略合并连续命中；每一组内的生产 fallible 调用使用同一最终边界。

| 位置 | fallible 操作 | 分类 | 最终记录边界/event | 不写错误日志的原因 | 验证 |
|---|---|---|---|---|---|
| `apps/{node,worker,desktop}/src/main.rs` | 顶层 `?`、subscriber 初始化、runtime/GUI 运行 | 向上传播 | 各 main 的 `process_failed`；初始化前走应急日志 | — | process diagnostics + check |
| `apps/node/src/restart_lifecycle.rs`、`dedup-windows::process_lifecycle` | 参数解析、spawn、父进程等待 | 向上传播 | node main/host request 的 `process_failed` 或 `request_failed` | — | Node check/tests |
| `crates/transport/{frame,priority_writer,connection}.rs` | read/write/flush/send/decode | 真实错误 | `transport_connection_failed` 或 request 返回到最终 owner | — | transport tests |
| `crates/transport/pending.rs` | oneshot `send` | 预期 Err | `expected_condition` | — | transport tests |
| `crates/node-engine/src/server.rs` | accept、writer、response send、JoinSet | 真实错误/预期 Err | listener/connection/request 对应事件 | — | node_server |
| `crates/node-engine/src/actor.rs` | actor command/reply、后台 task、shutdown/join | 真实错误/预期 Err | `request_failed`、`background_task_failed`、`expected_condition` | — | runtime/node tests |
| `crates/node-engine/src/{analysis,delete,contact_sheet_cache,config_repository}.rs` | store/report/send/cleanup | 向上传播或真实错误 | 任务/request owner；旁路 cleanup 用 diagnostics helper | — | node-engine tests + lint |
| `crates/node-engine/src/disk_full_cleanup.rs` | mutex、磁盘解析、metadata、remove_file | 真实错误/预期 Err | cleanup 内四类稳定事件 | — | disk-full tests |
| `crates/node-engine/src/io/scheduler.rs` | command send、oneshot、actor join | 向上传播/预期 Err/真实错误 | caller、`expected_condition`、observer | — | scheduler tests |
| `crates/node-engine/src/scan/{pipeline,engine}.rs` | spawn_blocking、JoinSet、channel、文件读取 | 真实错误/向上传播/预期 Err | ScanEngine 和任务 owner | — | scan tests |
| `crates/node-engine/src/scan/base_compute.rs` | JoinSet、cache send/recv、Worker dispatch、persist | 真实错误/向上传播/预期 Err | `file_failed`、`worker_crashed`、任务/request owner | — | base compute tests |
| `crates/node-engine/src/scan/base_persistence.rs` | std channel、thread join、persist ack | 真实错误/向上传播 | BaseStore owner 和 Node 后台 owner | — | persistence tests |
| `crates/node-engine/src/scan/cache_resolver.rs` | remote JoinSet、abort、channel、Arc unwrap | 真实错误/预期 Err | resolver 的降级、expected、background 事件 | — | resolver tests |
| `crates/node-engine/src/worker/{process,pool}.rs` | process send/wait/terminate、actor/slot send/Join | 真实错误/预期 Err | WorkerPool/Node 文件任务 owner | — | WorkerPool/process tests |
| `crates/node-engine/src/worker/pipeline.rs` | 联系表 read/decode、媒体流水线 | 真实错误 | 回退时 `dependency_fallback`；终态 `file_failed` | — | Worker tests |
| `crates/media-ffmpeg/src/{loader,decode}.rs` | DLL、FFI read/seek、`catch_unwind` | 真实错误 | loader request owner；FFI 返回前直接记录 | — | media-ffmpeg tests/check |
| `crates/windows/{job,overlapped_reader,storage_device,smbios}.rs` | Windows API、IO、取消、转换 | 真实错误/预期 Err/向上传播 | 当前资源边界或上层任务 owner | — | windows tests/check |
| `crates/windows/src/shell.rs:38-40` | `HRESULT::ok()` 后 `?` | 向上传播 | 删除任务 owner | 这是 Windows HRESULT 到 `Result` 的转换，不是 `Result::ok()` 丢失错误 | windows/node tests |
| `crates/central-store/src/*.rs` | PG query/转换/connection Join | 向上传播/真实错误 | Desktop/Node request owner或连接 observer | — | central-store tests |
| `crates/desktop-core/src/{app,sync,node_session}.rs` | controller、同步、重连、event send、JoinSet | 真实错误/预期 Err | controller/observer 对应事件 | — | desktop-core tests |
| `crates/desktop-ui/src/{bindings,models}.rs` | 输入转换、command try_send、wire enum | 真实错误 | `request_rejected`/`request_failed` | 编辑中的暂态无效字段只标 dirty；保存时记录 | desktop-ui check |
| `crates/core/src/logging.rs` | primary flush/write/rotate、emergency write | 真实错误 | `diagnostic_sink_failed`，双 sink 失败返回/stderr | — | logging + process diagnostics tests |
| `binary_search(...).is_ok()`、原子 compare-exchange、`Option::is_*` | 集合/状态判断 | 非错误状态/非 Result | 不记录 | 没有 Error 载荷 | 单元测试/lint |
| `#[cfg(test)]` 与平台不适用分支中的 `let _`/`is_err()` | 测试断言、未使用普通值 | 非错误状态/非生产 | 不进入正式 `lib`/`bin` | 不属于生产执行根 | production clippy |
| Slint `include_modules!` 生成代码 | 生成器内部未使用 Result | 生成代码边界 | 手写 UI callback 和 app main 记录可观察错误 | 仅对生成模块局部 allow，不放宽手写 Rust | desktop-ui/node clippy |

## 静态搜索结论

使用以下命令检查正式源码：

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

截至本次实现：Windows 正式 `lib`/`bin` 中没有未分类的 `Result` 丢弃。搜索仍会显示以下已分类
文本命中：Windows `HRESULT::ok()` 的向上传播转换、`#[cfg(test)]` 内断言/夹具、不适用平台中对
普通 `Metadata` 值的占位，以及 `examples/runtime_acceptance.rs` 开发验收工具。它们不进入三个正式
可执行文件；正式 target 还由以下 lint 阻止新增未消费结果：

```powershell
cargo clippy --workspace --lib --bins --all-features -- `
  -D unused_must_use -D clippy::let_underscore_must_use
```

全量测试使用以下固定命令；`--test-threads=1` 避免少量修改进程环境变量的验收用例互相覆盖，
`-j 1` 控制 Windows 链接阶段的内存和页面文件占用：

```powershell
cargo test --workspace --all-features -j 1 -- --test-threads=1
```

`cargo fmt`、上述全量测试、生产 lint 和三进程编译的最终运行结果记录在本次任务交付说明中。

## 无法捕获的物理边界

正常日志与应急目录同时不可写、断电、OS/内核强杀、进程内存损坏导致的不可恢复原生崩溃，无法
保证持久化。Worker 原生退出由仍存活的 Node 监督并写 `worker_crashed`；如果 Node 同时死亡，
本系统不能假装已经捕获该错误。

# Rust V2 日志使用说明

本项目使用本地 UTF-8 单行文本日志，不依赖日志服务器。`node.exe`、`worker.exe` 和
`desktop.exe` 共用同步大小滚动 writer：单个文件最多 20 MiB，包含当前文件在内保留
10 个文件。日志写入失败不会改变已经提交的任务或数据库状态。

## 日志位置

| 进程 | 默认当前日志 | 滚动文件 | 内容 |
|---|---|---|---|
| Node | `data/node/logs/node.log` | `node.1.log` 至 `node.9.log` | 节点、任务、扫描、Worker 监督和本地数据库错误 |
| Worker | `data/node/logs/worker-<PID>.log` | `worker-<PID>.1.log` 至 `.9.log` | FFmpeg、单文件计算和 Worker 协议错误 |
| Desktop | `data/desktop/logs/desktop.log` | `desktop.1.log` 至 `desktop.9.log` | 节点连接、同步、中心数据库和界面命令错误 |

Node 的实际日志目录来自已验证的 Node 配置；相对路径以可执行文件目录解析。Desktop 和
Worker 也从各自可执行文件布局解析目录，不使用当前工作目录作为回退。

正常日志尚未初始化或当前日志不可写时，诊断会写入：

```text
%TEMP%\mySingerServer\logs\node-emergency.log
%TEMP%\mySingerServer\logs\worker-emergency.log
%TEMP%\mySingerServer\logs\desktop-emergency.log
```

应急日志主要保存 `process_failed`、`process_panicked` 和 `diagnostic_sink_failed`。确认对应
进程已经停止后可以直接删除；下次需要时会自动重建。

## 级别和临时调试

- `ERROR`：当前操作无法继续、后台任务 panic/Join 失败或内部不变量破坏。
- `WARN`：单文件失败、依赖降级或局部失败，主流程仍可继续。
- `INFO`：进程和任务生命周期，以及正常关闭、取消、NotFound 等预期条件。
- `DEBUG`/`TRACE`：临时排障细节；默认不写高频调度和逐文件成功信息。

默认级别固定为 `INFO`。在启动目标进程前，可在同一个 PowerShell 窗口临时打开模块调试：

```powershell
$env:RUST_LOG = 'info,dedup_node_engine=debug'
```

也可使用全局 `debug`、`trace`，或为其它 Rust target 指定 `target=debug/trace`。为了确保错误
不会被关闭，`off`、`warn`、`error` 和会降低详细度的 target 指令会被拒绝，并回退 `INFO`。
关闭临时调试：

```powershell
Remove-Item Env:RUST_LOG -ErrorAction SilentlyContinue
```

## 常用事件

| `event` | 含义 | 常用关联字段 |
|---|---|---|
| `process_started` / `process_stopped` | 进程正常生命周期 | `process`、`pid`、`reason` |
| `process_failed` / `process_panicked` | 顶层失败或 Rust panic | `process`、`operation`、`thread`、`error` |
| `runtime_task_started` / `runtime_task_terminal` | 运行任务开始和唯一终态 | `runtime_task_id`、`task_kind`、`state` |
| `file_failed` | 单个文件失败但任务可继续 | `task_id`、`item_id`、`stage`、`display_path` |
| `worker_ready` / `worker_crashed` | Worker 就绪或异常退出 | `worker_pid`、`worker_exit_code`、`crash_stage` |
| `dependency_fallback` | Everything、联系表等依赖回退 | `dependency`、`fallback`、`error` |
| `central_store_degraded` | PostgreSQL 不可用，降级 SQLite-only | `operation`、`fallback`、`error` |
| `transport_connection_failed` | TCP 分帧、读写或解码失败 | `peer`、`operation`、`error` |
| `background_task_failed` | Tokio/线程后台执行根失败 | `component`、`task_name`、`operation` |
| `request_failed` / `request_rejected` | 请求执行失败或输入被拒绝 | `component`、`request_id`、`operation` |
| `expected_condition` | 已显式处理的正常关闭、取消或幂等缺失 | `component`、`operation`、`reason` |
| `invariant_failed` | 代码内部约束被破坏 | `component`、`operation`、`error` |

## PowerShell 检索

在安装目录执行：

```powershell
rg 'event="(worker_crashed|file_failed|runtime_task_terminal)"' .\data\node\logs
rg 'task_id="<任务ID>"|runtime_task_id="<运行任务ID>"' .\data\node\logs
rg 'event="(dependency_fallback|central_store_degraded)"' .\data\node\logs
rg 'event="(process_failed|process_panicked|background_task_failed|invariant_failed)"' .\data
```

查找一个 Worker 的上下文：

```powershell
$workerPid = 12345
rg "worker_pid=$workerPid|PID $workerPid" .\data\node\logs
Get-Content ".\data\node\logs\worker-$workerPid.log" -Tail 200
```

## 提交问题所需信息

请提供：

1. 问题发生的大致时间和操作；
2. `node.log` 及相关滚动文件；
3. 日志中出现的任务 ID；
4. 若涉及 Worker，提供对应 PID 的 `worker-<PID>.log`；
5. 若涉及管理端连接或中心同步，提供 `desktop.log`。

通常不需要上传数据库、配置原文或媒体文件。Node 的本地 `file_failed`/`worker_crashed` 允许
记录完整路径用于定位故障，因此分享前应检查并遮盖隐私路径。严禁公开 PostgreSQL DSN、密码、
令牌、确认值或完整配置原文。

## 可保证与不可保证的边界

系统会记录所有能到达 Rust 处理边界的 `Err`、Rust panic、Tokio/线程 Join 失败、Worker 启动与
异常退出，以及文件、网络、数据库、FFmpeg 和 Windows API 返回的操作错误。中间层只向上传播
的错误由最终 owner 记录，避免同一根因重复。

以下物理情况无法保证落盘：正常日志和 `%TEMP%` 应急目录同时不可写、断电、操作系统强制终止、
内核终止，以及 Node/Desktop 自身的不可恢复原生崩溃。Worker 原生崩溃只有在 Node 仍存活时，
才能由 Node 的 `worker_crashed` 事件记录。

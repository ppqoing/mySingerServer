# 三类计算任务验收记录

本记录只填写实际执行证据。自动化测试、静态发布验证、本机真实媒体和远端真实媒体分别记录，
不得互相推导通过。真实媒体根只读使用：本机 `I:\tmp`，远端 `D:\tmp\-------2-4`。

## 环境

| 项目 | 记录 |
|---|---|
| 日期 | 2026-08-23 |
| 源码目录 | `D:\code\mySingerServer\.worktrees\rust-v2-media-dedup` |
| Rust 工具链 | `1.97.1-x86_64-pc-windows-msvc` |
| 本机执行目录 | `I:\Tool\mySingerServer-rust-v2-win-x64`；2026-08-23 15:16 已部署修复后的 `node.exe`，Node 与 23 个 Worker 已启动，127.0.0.1:39091 实测可连接 |
| 远端主机 | `codex-192-168-1-6` |
| PostgreSQL | `mysingerserver-rust-v2-postgres-schema3`，127.0.0.1:15440，schema 3，22 表；旧 schema 1 容器保留未修改 |

## 相关自动化门禁

| 验证点 | 命令 | 结果 | 证据 |
|---|---|---|---|
| V4 配置与 Worker 协议 | `cargo test -p dedup-protocol --test node_config_wire --test worker_base_compute_wire --test runtime_tasks_wire --locked -- --test-threads=1` | 通过 | 12 项通过 |
| Worker 同句柄与 AVIO | Node 两项 Worker 会话测试、Windows 可复用句柄、FFmpeg custom AVIO | 通过 | 4 项通过 |
| 基础缓存与连续调度 | `base_compute_pipeline`、`scan_cache`、`scan_parallelism` | 通过 | 18 项通过 |
| 二次缓存与缩略图复用 | `stage2_thumbnail_cache`、`analysis_runtime_details` | 通过 | 7 项通过 |
| 阶段持久化与恢复 | `task_stages`、`task_recovery`、`file_faults` | 通过 | 9 项通过 |
| 三类任务与 2 秒进度 | `runtime_tasks`、`runtime_recovery`、`three_task_pipeline` | 通过 | 7 项通过；组合链路二次运行零 Worker 派发 |
| Desktop 清单编排 | `duplicate_list_tasks`、`runtime_tasks`、`cross_analysis`、`cross_phase2` | 通过 | 14 项通过，含 1 项真实 TCP + PostgreSQL |
| 设置与任务详情 UI | `cargo test -p dedup-desktop-ui --test bindings_contract --test window_contract --locked -- --test-threads=1` | 通过 | 35 项通过 |
| 进度条和详情布局 | 两项 `offscreen_layout` 精确用例 | 通过 | 2 项通过；进度从左向右、详情双尺寸布局 |

## 本机基础计算阶段切换回归

| 验证点 | 结果 | 证据 |
|---|---|---|
| 原发布包真实失败边界 | 已复现并定位 | `I:\Tool` 的 SQLite 中枚举阶段为 `14786/14786 completed`，缓存查询与基础计算均未开始，任务项为 0；运行界面将两个等待阶段显示为失败 |
| 根因 | 已确认 | 枚举边界和 `BaseComputeEngine` 入口对同一总数重复调用 `freeze_base_compute_totals_nowait`，第二次返回 `StageRegression`，基础缓存阶段因此无法启动 |
| 回归测试 RED | 符合预期失败 | `freezing_the_same_base_compute_totals_twice_is_idempotent` 在修复前返回 `StageRegression` |
| 相关测试 GREEN | 通过 | `runtime_tasks` 6 项、`base_compute_pipeline` 4 项，共 10 项通过 |
| 部署边界 | 通过 | 仅覆盖 `I:\Tool\mySingerServer-rust-v2-win-x64\node.exe`，源/目标 SHA-256 均为 `39698A9A96B95B54A1ED8808DDC477C22A5E6C4116C143BC4E5EF4458E333965`；保留 `config` 与 `data` |
| 新任务阶段切换验收 | 通过原故障边界，任务继续运行 | 新任务 `01a02d7d-d8ea-7fd3-a89b-9781692537a0`：枚举 `14786/14786 completed`，基础缓存查询 `14786/14786 completed`，基础特征进入 `running`；任务项 14786，采样时已成功 112、失败 0、跳过 0。该证据只证明阶段切换修复，不冒充最终半小时终态验收 |

## 本机缓存进度与单文件失败回归

| 验证点 | 结果 | 证据 |
|---|---|---|
| 现场失败边界 | 已复现并定位 | 发布目录任务 `01a02d84-a23e-7e81-bed0-e01291fe9df2`：枚举和缓存查询均为 `14786/14786`，基础特征在成功 395 项后整任务失败；SQLite 仍有 14379 queued、12 running，且没有文件失败、`file_faults` 或错误日志，证明文件处理错误越过了单项边界且 Actor 丢弃了任务错误文本 |
| 缓存查询中间进度 RED | 符合预期失败 | 第二个路径查询批次暂停时，修复前运行时阶段仍为 `0/1001`，未发布首批 1000 项完成进度 |
| 单文件继续执行 RED | 符合预期失败 | 首文件返回错误响应类型后，修复前后续文件 1 秒内未获调度，整次 `BaseComputeEngine` 提前返回错误 |
| 相关测试 GREEN | 通过 | `base_compute_pipeline` 6 项通过；其中首批完成时运行时与 SQLite 均为 `1000/1001`，单文件异常后下一文件成功、任务以 1 failed + 1 succeeded 完成 |
| 实现边界 | 已修复，待发布目录真实复测 | 路径缓存按最多 1000 项完整处理后更新并持久化；文件级协议/解析/缩略图/续算错误写 Node 日志和任务失败详情后继续；SQLite/WorkerPool 基础设施错误仍终止任务，并保留完整错误文本 |

## PostgreSQL 与 schema

| 验证点 | 结果 | 证据 |
|---|---|---|
| 空库执行 `schema/central-v2.sql` | 通过 | 新容器和新持久化卷由 `New-RustV2PostgresContainer.ps1` 创建；脚本行为测试通过 |
| schema 3、固定表和数据条数诊断 | 通过 | `mysingerserver-rust-v2-central-schema-3|3|22` |
| SQLite-only 模式不连接 PostgreSQL | 通过 | Node 组合链路未配置 PostgreSQL，三类任务与缓存复用完成 |
| PostgreSQL 阶段与跨机编排 | 通过 | `dedup-central-store task_stages` 1 项；真实 TCP + PostgreSQL 1 项 |

## 最终半小时真实媒体验收

开始前确认相关自动化门禁已通过，且测试不会修改、删除或移动真实媒体。

| 项目 | 本机 | 远端 |
|---|---|---|
| 媒体根 | `I:\tmp` | `D:\tmp\-------2-4` |
| 开始/结束时间 | 未开始；2026-08-23 两次 UAC 均返回用户取消 | 由用户按发布包手动验收 |
| 基础计算终态 | 未产生运行样本 | 待填写 |
| 重复文件清单终态 | 待填写 | 待填写 |
| 二次特征终态 | 待填写 | 待填写 |
| Worker/CPU/磁盘观察 | 待填写 | 待填写 |
| 缓存和缩略图复用 | 待填写 | 待填写 |
| 崩溃、超时和跳过记录 | 待填写 | 待填写 |
| 日志/数据库/截图证据 | 两个隔离目录仅生成 `media-before.json`；未启动 Node，不算验收通过 | 待填写 |

## 发布包

| 项目 | 记录 |
|---|---|
| ZIP 路径 | `dist-rust-v2/mySingerServer-rust-v2-win-x64.zip` |
| 文件大小 | 69,696,748 字节 |
| SHA-256 | `E3B8867856B141862F59B5877C9B1115183B50AFD2ADC87D7CC2FC31CAC209BB` |
| `verify-release.ps1` | 通过；2026-08-23 缓存进度与单文件失败修复后重新构建，输出 `PACKAGE_PASS` 与 `RUST_V2_RELEASE_BUILD_PASS` |

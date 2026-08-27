# 2026-08-23 CPU/I/O 分阶段流水线旧架构基线

## 范围

本记录冻结 Task 0 开始时 `BaseComputeEngine::run_existing` 的行为与同机夹具数值，供后续 CPU/I/O 分阶段流水线改造对照。夹具通过真实 `BaseComputeEngine`、真实 `NodeStore` SQLite 单写者和真实 `WorkerPool` 控制通道运行；不读取、创建或修改真实媒体文件。

## 固定夹具

- 随机种子：`0x20260823C0DE0000`。夹具不消费随机数，种子仅标识不可变清单。
- Worker 槽位：2；磁盘读取许可：SSD 2。
- 路径缓存等待：25 ms；Worker 媒体解码等待：6 ms。
- 清单：`cached-small.bin`（4 KiB，路径缓存命中）、`small-miss.bin`（8 KiB，缓存缺失）、`large-content-hit.bin`（64 MiB，Hash 后内容缓存命中）、`large-miss.bin`（96 MiB，缓存缺失）。
- Hash、远程缓存和 Worker 解码均为可控边界替身；结果仍经真实 Node SQLite 持久化。`NeverRead` 明确拒绝 Node 直接读文件，保证 Hash 事件来自 Worker 两步会话。

## 行为门禁

`mixed_baseline_fixture_observes_cache_blocked_worker_idle_and_persistence` 使用通知闸门暂停真实 `lookup_paths` 调用，并在放行前等待 30 ms。该测试证明旧架构在路径缓存查询完成前没有派发任何 Worker Hash；放行后完成 3 个 Hash 会话，其中 1 个内容缓存命中且只有 2 个实际媒体解码任务，最终 4 个任务项均由 SQLite 标为完成。

`decode_and_persist_ms` 从第一个 Worker 收到续算命令开始，到 `BaseComputeEngine::run_existing` 返回、读取 SQLite 并确认任务状态为 `Completed` 后才结束。回归夹具额外在最终 outbox ACK 前等待 80 ms，并断言该跨度覆盖这段等待；因此它不会把“`CompleteBase` 控制命令已送入 WorkerPool”误当成“Node 已持久化完成”。

## 原始基准输出

运行命令：

```powershell
Remove-Item Env:CC -ErrorAction SilentlyContinue
Remove-Item Env:CXX -ErrorAction SilentlyContinue
$env:CARGO_TARGET_DIR = 'C:\tmp\rust-v2-visual-fidelity-target'
cargo bench -p dedup-node-engine --bench base_compute_pipeline
```

退出码：`0`。

```text
seed=0x20260823C0DE0000
files=4
cache_hits=2
hash_sessions=3
media_decode_jobs=2
cache_wait_ms=32.255
worker_idle_before_hash_ms=32.766
decode_and_persist_ms=61.838
elapsed_ms=110.144
throughput_files_per_second=36.316
worker_idle_while_cache_waits=false
persisted_completed=true
```

`worker_idle_while_cache_waits=false` 是固定时长 bench 的非闸门模式标识；“缓存等待时 Worker 空闲”由上节的闸门行为测试验证。后续改造必须沿用相同种子、清单、槽位与等待参数后再比较阶段重叠、空闲时间和吞吐，且不能将这一次受控替身的数值解释为真实媒体/磁盘吞吐。

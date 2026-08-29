# Task 7B2D2C1：瞬态任务文件的 Media Worker 派发边界

## 范围

本提交新增 `task_file_media_compute`，只接收已经封闭且完成 Hash/缓存分类的
`TaskFileBaseComputePending`，把已知 MD5、仍缺少基础媒体字段的 Base 行交给现有
`WorkerPool`。本阶段负责 dispatcher Media permit、Worker Started/SourceComplete/
Completed/Crashed 事件和取消收束；完成/失败结果以拥有型对象返回，任务文件仍保持 `P`。

本提交不接 actor 主循环、Worker 实现、SQLite stage1 写入、taskless persist ACK、
任务 finalize、恢复或任务表。

## 实现

- Media 阶段只调用 `TaskDispatchAdmission::media_only()`；dispatcher 已取得的 permit
  直接随 Worker dispatch 使用，不二次申请读取许可，并以 `worker_capacity` 限制活动项。
- 每个 Media 行沿 `TaskFileIdentity` 取得原始 `TaskFileBaseContext`，物理盘值直接使用
  冻结 lane 生成，不从任务文件名反解析。缺少本地 `content_id` 时通过同一
  `BaseStoreHandle::upsert_content_and_location` 补齐内容/位置，仍不写任务表或 stage1。
- Started 只有在 run、item 和完整 `WorkerFileIdentity`（含阶段）匹配时才登记 slot；
  `BaseSourceReadComplete` 只有 run、item、slot 匹配时释放 permit。Completed 未收到源
  读取完成事件时返回当前项协议失败；Crashed 只返回当前项文件失败，外层可在 ACK 后
  决定 `F`，基础设施失败则携带 pending 返回任务级错误。
- Media 完成结果包含原身份、任务行、上下文、Worker 响应和 slot；失败结果包含同样的
  原始上下文与诊断。普通完成/失败路径只收集结果，不把 TSV 原位改成 `C/F`。
- 取消会停止派发、调用 `WorkerPool::cancel_task`、释放许可并 abandon 精确在途身份，
  保持 `P` 并返回可 discard 的 pending。Hash 行存在时，Media-first 运行返回明确
  `HashPending` 与剩余 Hash 数量，不把阻塞当成完成。

## 审查修复

- Worker 槽位退出时先等待旧 driver 收束并尝试安装 replacement；driver 或 replacement
  失败只发送任务级 `InfrastructureFailure`，不先发送 `Crashed`，避免上层先把当前项
  错误置为 `F` 后又收到基础设施错误。replacement 工厂现在返回错误，由统一事件边界
  决定事件顺序。
- 每个 Media 行都通过 `upsert_content_and_location` 幂等补写当前位置，即使上下文已有
  `content_id` 也不跳过；返回的 `ContentKey` 必须与任务行的 MD5/文件大小一致，并把
  返回的内容 ID 更新回上下文。

## TDD 与验证

先以缺少 `run_task_file_media_compute` 的编译失败固定 RED，随后实现最小边界并增加真实
行为测试。测试通过 `ControlledWorkerPool` 和真实 `TaskFileDispatcher` 驱动，不使用源码
文本匹配：

| 行为 | 结果 |
|---|---:|
| permit 在 SourceComplete 前保持、匹配后释放 | 通过 |
| Hash→Media continuation 保持同一 identity | 通过 |
| Media-first 后显式返回 HashPending/remaining=1 | 通过 |
| Worker 崩溃只影响当前项、另一 lane 继续完成 | 通过 |
| foreign/mismatched Started/SourceComplete 不释放 permit | 通过 |
| 取消保持 P 且 pending 可 discard | 通过 |
| replacement 失败先发 InfrastructureFailure，不发送 Crashed | 通过 |
| 已有 content_id 的 Media 行仍补写当前 files 位置并校验 ContentKey | 通过 |
| `cargo test -p dedup-node-engine --lib task_file_media_compute --features test-hooks --locked -- --test-threads=1` | 6/6 通过 |
| `cargo test -p dedup-node-engine --lib worker::pool::tests --locked -- --test-threads=1` | 12/12 通过 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 82/82 通过 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

focused 编译曾因共享 `C:\tmp\rust-v2-core-scope-target` 的锁文件 ACL 返回拒绝访问，
随后使用独立可再生目录 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1` 完成验证。
测试期间 C/D 可用空间约 13.49/11.95 GiB，未触发 10 GiB 停止线；未触碰 `I:\Tool`。

## 后续边界

Task 7B2D2C2 仍需消费本模块的 completed/failure 拥有型结果，校验 Worker payload，
执行 taskless stage1 写入并等待 persist ACK，随后才允许 dispatcher 行迁移为 `C/F`。
本提交不能单独宣称瞬态任务已完成端到端计算，也未进行真实媒体、打包或部署。

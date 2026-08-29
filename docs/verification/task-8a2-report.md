# Task8A2：Worker 二筛源读取完成事件

## 目标

让 Worker 在二筛所需的原媒体、联系表读取和解码全部完成、相关文件句柄释放后，先发送已经定义的
`Stage2SourceReadComplete`；事件之后只使用已拥有的内存帧执行 CPU 特征计算，不再访问原路径。

## 修改范围

- `apps/worker/src/protocol_loop.rs`：二筛协议请求拆成源读取和 CPU 特征两个阶段，固定发送顺序：
  `Started → Decode → prepare（源文件/联系表 IO）→ Stage2SourceReadComplete → Feature → ResultWait → Stage2Result`。
- `crates/node-engine/src/worker/pipeline.rs`：增加拥有型 `PreparedStage2Compute`，拆分 `prepare_stage2_input` 和
  `finish_stage2_input`。前者读取并解码原数据，后者只消费 RGB 内存帧；联系表命中、损坏重建和直接媒体解码都在
  事件前完成。
- `crates/node-engine/src/worker/mod.rs`：导出协议循环使用的准备结果类型。
- `apps/worker/tests/worker_protocol_process.rs`：增加真实 Worker 进程行为门禁。解码器在源读取 gate 中暂停，
  放行后断言先收到 `Stage2SourceReadComplete`，随后删除源文件，再验证仍能得到 Stage2 特征结果且 Worker
  槽位保持占用直到终态。

未修改协议定义、WorkerPool、NodeStore、phase2、actor 或 scan task-file 模块。旧的同步
`handle_worker_request` 仍复用同一准备/完成逻辑，保持既有一次响应接口兼容；真实进程协议循环使用新增的中间事件。

## TDD 证据

### RED

命令：

```text
cargo test -p worker --test worker_protocol_process --locked -- --test-threads=1
```

旧实现能进入真实二筛解码，但没有发送 `Stage2SourceReadComplete`。在释放源读取 gate 后，测试等待该事件
超时，实际失败为：

```text
Error: Elapsed(())
```

这次失败证明测试捕获的是缺失协议事件，而不是静态源码变更。之后共享工作树曾被其他任务的 `actor.rs`
非 `Send` 中间态阻塞过一次编译；该次编译错误不计入本任务 RED。

### GREEN

主代理在 actor 编译修复后独立复跑同一命令，结果为：

```text
WORKER_PROTOCOL_PROCESS_PASS
```

真实进程行为通过：源读取 gate 未释放时没有提前事件；释放后先收到源读取完成事件；删除源文件后仍完成
CPU 特征并返回 `Stage2Result`；Worker 直到终态才释放槽位。

## 静态核对

- `rustfmt --edition 2024 --check`（本任务四个 Rust 文件）：通过。
- `git diff --check`（本任务四个 Rust 文件）：通过。
- `finish_stage2_input` 及 `finish_stage2_features` 不接收或读取路径，只处理拥有型 RGB 数据。
- `Stage2SourceReadComplete` 为非终态事件；WorkerPool 的既有生命周期继续持有 Worker/CPU 所有权直到结果终态。

## 状态与后续

Task8A2 的实现和真实进程协议门禁已完成。本任务未提交、未打包、未部署，也未触碰 `I:\Tool`。主代理仍需按
整体变更范围运行 Node/Worker pipeline 回归；本报告不替代更大范围回归和真实媒体验收。

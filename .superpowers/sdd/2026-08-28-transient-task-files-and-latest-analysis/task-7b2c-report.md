# Task 7B2C：缓存分类到冻结 lane TSV 生产器

## 范围

本次新增 `BaseTaskProducer`，把已经完成本地/远端选择且已导入本地 SQLite 的基础缓存
结果转换为瞬态 TSV 任务行和当前扫描清单。生产器只负责分类、去重、按冻结物理盘 lane
批量追加和 seal；不迁移 BaseCompute 主循环、actor、Worker 或 NodeStore，也不实现任务恢复、
TaskCatalog、分析、删除、分页、`.idx`、磁盘满清理或真实媒体跑测。

## TDD 证据

先新增真实行为测试并在旧实现运行：

```text
cargo test -p dedup-node-engine --test base_task_producer --locked -- --test-threads=1
exit 1
```

旧实现因 `BaseTaskInput` 和 `BaseTaskProducer` 接口不存在而在集成测试编译阶段失败；失败
对应目标行为缺口，没有把源码文本检查当作测试。

实现后的 focused 行为测试为 8/8 通过，覆盖三项两 lane 分类、全命中不建 TSV、合法零值
特征、重复路径去重、未导入缓存拒绝、ContentKey 冲突、文件大小/lane 冲突和超过 1,000
项拒绝。测试通过真实 `TaskFileDispatcher` 读取行，不直接检查实现源码。

## 窄审查修复

复审新增两项 Important 后继续按 RED→GREEN 执行：

- 原 `seal(self)` 在封闭失败时消费整个生产器，且没有生产端 discard 入口。新增可选的
  dispatcher 所有权槽，改为 `seal(&mut self)`；只有 seal 成功才移交 dispatcher，失败时仍由
  生产器持有，并可调用 `discard` 删除精确运行目录。
- 原生产器只在逐 lane 的 `append_batch` 内校验行，后续 lane 的非法 UTF-8 显示路径会让前一
  lane 先发布。`TransientTaskFileSet` 暴露 crate 内复用的完整任务行校验，生产器在任何 lane
  写入前预校验本批全部行。

新增真实测试：后一个 lane 含非法 UTF-8 时所有 lane 均没有发布；append 失败和 seal 失败后
均可由同一 owner 精确 discard。旧实现分别以缺少 `discard` 接口编译失败和前一 lane 已存在
任务文件的运行断言固定 RED；修复后 focused 测试为 11/11 通过。

## 实现结果

- `BaseTaskInput` 固定携带 `PlannedScannedPath`、缓存记录、联系表有效性和强制重算标记；
  `BaseComputeDecision::for_cache` 与 `classify_cache_completeness` 是唯一缺失位来源。
- 任意 `Some(BaseCacheRecord)` 都必须带本地 `content_id`，且内容大小必须与枚举结果一致；
  未导入的远端快照在任何文件发布前拒绝。
- 完整命中只加入 `seen_paths/resolved_files` 并递增 `cache_hits`，不创建任务行或 lane 文件。
  已知 MD5 的部分命中只写真实 `TaskWorkMask::for_base(false, missing_parts)`；路径未命中
  只写 `needs_md5` 行。
- 同批和跨批规范路径使用稳定排序去重；同路径的文件大小、冻结 lane 或分类/ContentKey
  不一致均作为任务级输入错误。输入校验完成后才开始发布，超大批次不会部分发布。
- 非空 lane 按任务文件名稳定排序，每个 lane 每批只调用一次 `append_batch`，lane 内保留
  输入顺序；全命中不注册空 lane。`seal` 返回原 dispatcher 和 `BaseTaskManifest`，不复制
  调度器状态。

## 修改文件与 SHA-256

| 文件 | SHA-256 |
|---|---|
| `crates/node-engine/src/scan/base_task_producer.rs` | `97C36795C098F4304ED5537031A4C2AF82E8F162EF7AFEDC7ECD8F963A26AB04` |
| `crates/node-engine/src/scan/mod.rs` | `BF5FC3A83447BC5151BE82CCAB15712A486B47D7E1C61E1D8350575839FBE88A` |
| `crates/node-engine/src/task_files.rs` | `47857F52F0278080779FB52F9CDC7DED111A740BF17AFF00F9DA7943A1CAD5A8` |
| `crates/node-engine/tests/base_task_producer.rs` | `B5233378631D426C89C64EDEC6EBF84B7B3255E0B71857C4174B0A0E1E0C78F4` |

## 验证

所有 Cargo 命令均使用 `C:\tmp\rust-v2-core-scope-target`、关闭 incremental/debug 信息并
清除 MinGW 编译环境变量。执行期间 C/D 可用空间约 14.72/11.95 GiB，未触发清理规则。

| 验证 | 结果 |
|---|---:|
| `cargo test -p dedup-node-engine --test base_task_producer --locked -- --test-threads=1` | 11/11 通过 |
| `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1` | 18/18 通过 |
| `cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1` | 25/25 通过 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 66/66 通过；仅有既有 dead-code 警告 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

## 风险与后续边界

本提交只提供 Task 7B 后续接入所需的生产 substrate。Worker 成功结果加入
`resolved_files`、SQLite ACK 后原位标记 `C`、取消/失败收束和 actor 生命周期仍由后续 Task
7B2/7C 接入。跨 lane 的真实文件追加若在后一个 lane 发生 IO 错误，底层 dispatcher 会按
既有语义毒化本次 run；本模块不尝试跨文件回滚。未运行真实媒体、未打包、未部署、未访问
`I:\Tool`。

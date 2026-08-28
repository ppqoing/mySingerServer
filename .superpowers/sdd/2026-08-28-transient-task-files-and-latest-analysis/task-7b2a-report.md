# Task 7B2A：无任务表的一筛提交事务

## 结果

Task 7B2A 已完成。NodeStore 新增 `commit_scan_stage1_taskless`，为瞬时 TSV 任务提供不依赖
`tasks`、`task_items`、`task_stages` 的一筛提交边界。该接口使用 SQLite `Immediate` 事务，
在同一事务内更新内容媒体类型和 `base_complete`，按既有合并规则写入图片/视频一筛与联系表，
并为内容和每个特征写入与提交值一致的同步 outbox。成功返回本次事务最后一个 outbox 序号。

既有 `commit_scan_stage1_guarded` 继续使用原任务身份门禁；其特征写入逻辑已提取为共享事务
辅助函数，避免两套 SQL 漂移。taskless 入口不读取、不插入、不更新任务表。

## TDD 证据

先新增真实行为测试并执行旧实现：

```text
cargo test -p dedup-node-store --test taskless_stage1 --locked -- --test-threads=1
exit 1
```

旧实现因 `NodeStore::commit_scan_stage1_taskless` 尚不存在而编译失败；测试调用的是预期公开
行为接口，没有使用源码文本检测代替行为断言。

实现后验证结果：

- `taskless_stage1`：4/4 通过。
- `dedup-node-store` 全量：56/56 通过，既有 guarded 一筛和 outbox 行为均通过。
- `cargo fmt --all -- --check`：通过。
- `git diff --check`：通过。

所有 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，关闭增量/debug 信息并清除了 MinGW
编译环境变量。执行期间未触碰 `I:\Tool`，未运行真实媒体、未打包或部署。

## 已覆盖行为

- 完整图片一筛和联系表可在没有任何任务行时提交；SQLite 完整缓存与内容/特征 outbox 均可读取。
- 视频元数据、六个固定槽位和联系表在一个事务内提交；四个成功槽位形成完整视频一筛。
- 末尾出现非法二筛 `FeatureWrite` 时，内容状态、特征、outbox 和任务表均回滚。
- 合法 `Quality=0` 被保留；后续宽高为 0、其余字段为空的部分写入不会覆盖已有有效字段。
- 返回序号等于事务提交后的 outbox 高水位；无 taskless 任务表记录产生。

## 实现边界

- 修改 `crates/node-store/src/features.rs`：新增公开 taskless 入口，并抽取一筛内容/特征/outbox
  事务辅助函数；原 guarded API 只在辅助调用后执行原有任务项成功收尾。
- 新增 `crates/node-store/tests/taskless_stage1.rs`：覆盖图片、视频、回滚和合并语义。
- 未修改 NodeEngine、BaseCompute、actor、协议、任务恢复、分析、删除、分页或数据库 schema。

## 后续风险与范围

该接口只提供 NodeStore 事务边界；Task 7B 后续仍需由 BaseCompute 在 SQLite ACK 成功后调用，
并由 actor 负责瞬时 TSV 生命周期和完成扫描清单收尾。本提交不宣称生产 BaseCompute 已迁移，
也不替代 Task 7C 的当前 run 清理或真实媒体验收。

# Task 13B：中心当前事实边界

## 结果

- 中心成员窗口不再读取 `review_marks`。`CentralGroupMember.review` 固定从当前窗口初始化为 `Undecided`，复核决定由 Desktop 当前进程状态维护。
- 同步仍兼容解码旧 `deletion_tombstone` 载荷，但投影层直接忽略该历史项，不执行 PostgreSQL `INSERT` 或 `UPSERT`。
- `file(active=false)` 仍按正常文件事实写入，并作为删除后的唯一中心当前状态。
- 全量快照保留 `deletion_tombstones` 表名和协议页面映射以兼容旧快照，但不再清理、写入或读取该表；旧表和旧 `save_review_mark` API 暂留给兼容测试，新的 Desktop 产品路径不得调用。

## TDD 证据

先增加真实同步语义测试，旧实现编译失败，缺少当前事实判定边界：

```text
error[E0599]: no method named `writes_current_fact` found for enum `DecodedChange`
```

随后增加最小的 `DecodedChange::writes_current_fact` 门禁，并让历史墓碑/联系表在 `apply_change` 入口直接返回；新增单元行为测试通过：

```text
cargo test -p dedup-central-store --lib legacy_tombstone_is_not_a_current_fact_mutation --locked -- --test-threads=1
1 passed; 0 failed
```

新增 `transient_history` PostgreSQL 行为夹具覆盖：

- 已保存 `review_marks` 后重新读取成员，所有兼容字段仍为 `Undecided`；
- 同一同步批次包含 `file(active=false)` 和旧墓碑载荷时，文件位置失活且墓碑表不新增行；
- 旧墓碑载荷仍能通过协议解码；
- 测试使用唯一机器、内容、位置和分析运行 ID，结束后按外键顺序清理。

当前机器未设置 `DEDUP_TEST_POSTGRES_URL`，Docker PostgreSQL 服务也不可用，因此该真实数据库夹具明确保持 `ignored`，未伪报 PostgreSQL PASS。

中心库全量回归：

```text
cargo test -p dedup-central-store --locked -- --test-threads=1
3 unit tests passed; 0 failed
1 public contract test passed; 0 failed
3 PostgreSQL tests ignored because DEDUP_TEST_POSTGRES_URL is unavailable
```

格式与边界检查：

```text
rustfmt --edition 2024 --check crates/central-store/src/content.rs crates/central-store/src/analysis.rs crates/central-store/tests/transient_history.rs  PASS
git diff --check -- crates/central-store/src/content.rs crates/central-store/src/analysis.rs crates/central-store/tests/transient_history.rs  PASS
```

## 修改范围与未执行项

修改文件：

- `crates/central-store/src/analysis.rs`
- `crates/central-store/src/content.rs`
- `crates/central-store/tests/transient_history.rs`
- 本报告

未修改 Desktop、NodeEngine、NodeStore、协议 schema 或 PostgreSQL DDL；未删除旧历史表和旧 API；未提交、未打包、未部署、未触碰 `I:\Tool`。

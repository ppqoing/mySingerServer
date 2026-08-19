# 最终修复 1 报告：冻结用户确认的删除集合

## 基线与范围

- 分支：`codex/rust-v2-media-dedup`
- 修复前 HEAD：`710721892a6f2b0a2d4ff379030905a7eadca88b`
- 修复前唯一脏项：未跟踪 `crates/desktop-core/tests/physical_two_hosts_e2e.rs`
- 该未跟踪文件未修改、未删除、未暂存；`MainWindow` 21 个回调和 UI 文件均未修改。
- 未新增 Protobuf 消息、字段、数据库字段或后端命令；复用现有 `CreateDeleteBatch.items` 表达精确确认集合。

## 根因与 RED

执行链确认如下：

1. `DesktopApp::LoadedMembersContext` 只保存当前最多 200 行成员页。
2. 旧 `PrepareDelete` 只用当前页计算摘要。
3. 旧本地 `ConfirmDelete` 只向节点发送 `group_ids`，节点 actor 和 SQLite 再按整组查询全部持久 Delete。
4. 旧中心 `create_delete_plan` 同样按 `group_id` 查询全部 Delete，再按机器派发。

先新增真实 `DesktopApp -> NodeSession -> TCP/Protobuf` 回归测试：同组 201 个成员，第一页包含 Keep 和 Delete，末页包含另一个 Delete，UI 当前只加载末页。首次运行：

```text
cargo test -p dedup-desktop-core --test delete_scope \
  cross_page_confirmation_and_execution_use_the_same_complete_set -- --exact --nocapture

FAILED
assertion left == right failed
left: 1
right: 2
```

失败准确命中跨页摘要少报，不是编译错误、源码文本断言或测试夹具错误。

## 接口裁决与实现

- `PrepareDelete` 从空游标开始分页读取当前组的完整成员；本地走现有节点成员分页，中心走现有 PostgreSQL 成员分页。
- 摘要从完整组计算，并把其中活动且明确 Delete 的 `group_id + LocationKey + ContentKey` 冻结在 `PreparedDeleteContext`。
- `PrepareDelete` 不调用 `CreateDeleteBatch`；回归测试在确认事件后明确断言尚未出现执行 RPC。
- `ConfirmDelete` 才把同一冻结集合写入现有 `CreateDeleteBatch.items`。本地请求带 `analysis_run_id`；中心外部批次不带该 ID，因此节点 actor 无需新协议字段即可区分两条路径。
- 节点 actor 拒绝本地 `group_ids` 范围请求；SQLite `NodeStore` 只为显式 `ConfirmedDeleteItem` 建批次。
- SQLite 和 PostgreSQL 均逐项复验：成员属于指定运行与组、review 仍为 Delete、组成员与位置仍 active、当前位置内容仍匹配确认时 MD5 和大小；每组选定集合之外仍有活动 Keep。
- 存储事务不再按组扩大集合。新增测试在冻结一项后再把同组另一项标记为 Delete，最终批次仍只有原确认位置；错误 ContentKey 在文件操作前被拒绝。
- 删除执行器继续在实际文件操作前复验活动位置、磁盘大小和流式 MD5；默认回收站与永久删除模式未改变。
- 中心完整摘要额外覆盖历史页离线 Delete：完整集合计数为 2，在线门禁禁止执行并给出在线警告。

## GREEN 与回归

所有命令运行前均清除了当前 PowerShell 进程的 `CC`、`CXX`。

- `cargo test -p dedup-desktop-core --test delete_scope --test review_delete --locked -- --test-threads=1`：7 passed。
- `cargo test -p dedup-node-store --test delete_group_update --locked -- --test-threads=1`：4 passed。
- `cargo test -p dedup-node-engine --test delete --locked -- --test-threads=1`：5 passed。
- `cargo test -p dedup-protocol --locked -- --test-threads=1`：3 passed，doc tests 0 failed。
- `cargo test -p dedup-desktop-core --locked -- --test-threads=1`：28 passed，0 failed，16 ignored；其中 `delete_scope` 在完整 crate 顺序中通过。
- `cargo fmt --all -- --check`：通过。
- `cargo check -p dedup-node-store -p dedup-node-engine -p dedup-desktop-core --all-targets --locked`：通过。
- `cargo clippy -p dedup-node-store -p dedup-node-engine -p dedup-desktop-core --all-targets --locked -- -D warnings`：通过。
- `git diff --check`：通过，仅有 Git 的 LF/CRLF 提示，无 whitespace error。

## 验收边界

- 中心冻结集合专用真实 PostgreSQL 测试 `central_delete_plan_never_expands_beyond_confirmed_locations` 已加入并通过 `--no-run` 编译。
- 当前环境未设置 `DEDUP_TEST_POSTGRES_URL`，Docker daemon 也不可用，因此该真实 PostgreSQL 用例本轮为 `BLOCKED`，没有冒充运行 PASS。
- 本地完整跨页链、节点存储复验、节点实际删除前身份复验、中心历史页离线门禁、协议 crate 和相关 crate check/clippy 均有本轮实际通过证据。

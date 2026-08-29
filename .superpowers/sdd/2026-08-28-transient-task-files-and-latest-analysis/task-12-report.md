# Task 12 实施报告：Desktop 中心结果滑动窗口

## 结论

Task 12 已把 Desktop 结果浏览收敛为“已完成 PostgreSQL 中心分析”这一条事实来源，并删除 Desktop 启动、读取、复核 Node 本地分析的产品路径。Slint 不再暴露本地分析入口、游标、上一页、下一页或加载更多；组和成员只提交 `start_index + visible_count`，Core 内部使用中心游标和有限检查点填充当前窗口。

本阶段仍临时保留中心复核/删除的 PostgreSQL 兼容写入，Task 13 再改成当前进程复核状态和瞬态 TSV 删除队列。本次没有修改 Node actor、NodeStore、协议或中心 schema，也没有增加历史、恢复、JSON、`.idx` 或 TaskCatalog。

## TDD 证据

### RED 1：旧窗口追加历史

命令：

```powershell
cargo test -p dedup-desktop-core --test review_delete result_window_replaces_previous_rows_instead_of_appending_history --locked -- --test-threads=1
```

旧实现真实失败：`left=2 right=1`。`PagedWindow` 把第二个窗口追加到第一个窗口，无法满足滑动窗口整体替换。

### RED 2：扫描页仍暴露本地分析入口

命令：

```powershell
cargo test -p dedup-desktop-ui --test offscreen_layout scan_page_does_not_expose_local_analysis_start --locked -- --test-threads=1
```

旧实现真实失败：`扫描页不应暴露 Desktop 本地分析入口`。

### RED 3：结果页仍暴露游标分页控件

命令：

```powershell
cargo test -p dedup-desktop-ui --test offscreen_layout result_workspace_does_not_expose_cursor_pagination_controls --locked -- --test-threads=1
```

旧实现真实失败：`结果工作区不应通过游标显示下一页控件`。

### RED 4：真实滚动始终请求第 0 行

命令：

```powershell
cargo test -p dedup-desktop-ui --test offscreen_layout result_scroll_requests_positive_group_and_member_windows --locked -- --test-threads=1
```

真实 MainWindow 向下滚动后失败：`向下滚动组表必须请求正数起始行，实际=[0]`。根因是 Slint `ScrollView.viewport-y` 向下滚动为负数，新代码错误地按正数换算。

最小修复只在组表和成员表把 `-viewport-y` 换算为全局可见行，并保留半窗预读。修复后同一测试通过，组表和成员表都发出正数起始窗口。

## 已落地结构

### Desktop Core

- `ResultScope` 只保留中心分析运行，不再表达 Node 本地结果。
- 新增 `ResultWindowRequest`：只包含运行 ID、组类型、`start_index` 和 `visible_count`。
- 新增 `ResultWindowState<T>`：每次响应整体替换 `items`，同时携带 `total_rows/loading/stale`。
- 新增 `CentralResultWindowCache`：单次最多向 UI 物化 200 行，组与成员共用的内部游标检查点总数最多 8 个，不缓存历史行对象。
- `RequestGroupWindow/RequestMemberWindow` 在读取前核对中心运行必须为 `Completed`；其他状态不发布可复核结果。
- 换运行、换组、空结果和错误响应不会拼接旧窗口；活动请求身份不匹配时丢弃响应。
- 中心连接失效后保留旧窗口并标记 `stale`；滚动与有效文件预览仍可使用，复核和删除写入被拒绝。
- 删除 `NodeSession` 的本地分析创建、运行查询、组/成员读取和本地复核接口。
- 保留 `PrepareAnalysisInput`、`DispatchStage2`、`CrossAnalysisCoordinator`、同步高水位、预览与外部删除执行。

### Desktop UI

- 扫描页删除“创建本地分析”入口。
- 结果页删除 cursor、上一页、下一页和“加载更多”。
- 组表和成员表使用头尾占位保持全局滚动高度，滚动时请求有限行范围；返回模型整体替换。
- `loading` 显示中心窗口加载提示；`stale` 显示“文件库已变化，结果只读”。
- `stale/loading` 时禁用保留、删除、快捷复核和删除确认，但不禁用滚动及已有在线成员预览。

## 验证结果

统一使用目标：

```text
C:\tmp\rust-v2-core-scope-target-task7b2d2c1
```

并清除继承的 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`。

| 命令 | 结果 |
|---|---:|
| `cargo test -p dedup-desktop-core --test review_delete --locked -- --test-threads=1` | 7 passed |
| `cargo test -p dedup-desktop-core --test delete_scope --locked -- --test-threads=1` | 1 passed |
| `cargo test -p dedup-desktop-core --test local_node_e2e --locked -- --test-threads=1` | 1 ignored；缺少真实测试包 |
| `cargo test -p dedup-desktop-core --test cross_phase2 --locked -- --test-threads=1` | 3 passed |
| `cargo test -p dedup-desktop-core --lib --locked -- --test-threads=1` | 6 passed |
| `cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1` | 15 passed |
| `cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1` | 21 passed |
| `cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1` | 20 passed |
| `cargo fmt --all -- --check` | passed |
| `git diff --check` | passed |

合计 73 项通过、0 项失败、1 项因未提供 `DEDUP_TEST_PACKAGE_ROOT` 或 release staging 明确 ignored。ignored 项没有计为通过。

## 审查修复

初次聚焦审查没有 Critical，发现两项 Important：滚动事件会立即排队中心查询，可能挤满串行命令通道；旧运行的成员窗口响应也可能在切换运行或删除成功后覆盖新状态。

- 滚动请求增加 80 毫秒单次防抖，只提交最新可见窗口；永久去重键同时包含运行、类别和组，切换上下文后相同窗口仍会重新查询。旧实现的真实 MainWindow 行为记录到 3 次连续请求，修复后只发最后 1 次。
- Core 为组和成员请求保存完整作用域。成员请求在查询前、查询后都核对运行、类别、组和请求身份；删除成功立即使组/成员窗口及在途请求失效。旧实现中 R1 的迟到成员响应会在切换 R2 后继续发布，修复后不再发布旧响应。
- 第一轮修复后复审确认迟到响应已经关闭，但指出 80 毫秒 UI 防抖不能阻止慢 PostgreSQL 查询期间继续填满公共 64 槽通道。最终修复在 Core 前增加持续排空的轻量命令路由：组窗口和成员窗口各自只有一个 latest-only 槽，普通命令保持独立顺序通道。控制循环暂停时分别发送 96 条组窗口和 96 条成员窗口，真实行为测试证明公共通道不阻塞、每类只执行最后一条，Refresh 和 SaveReview 均未丢失。
- 最终合并后重新运行 Desktop Core/UI 相关套件，共 73 项通过；格式和差异检查通过。

## 文件清单

- `crates/desktop-core/src/app.rs`
- `crates/desktop-core/src/node_session.rs`
- `crates/desktop-core/src/results.rs`
- `crates/desktop-core/src/review.rs`
- `crates/desktop-core/tests/delete_scope.rs`
- `crates/desktop-core/tests/local_node_e2e.rs`
- `crates/desktop-core/tests/review_delete.rs`
- `crates/desktop-ui/src/bindings.rs`
- `crates/desktop-ui/src/models.rs`
- `crates/desktop-ui/tests/bindings_contract.rs`
- `crates/desktop-ui/tests/offscreen_layout.rs`
- `crates/desktop-ui/tests/window_contract.rs`
- `crates/desktop-ui/ui/app.slint`
- `crates/desktop-ui/ui/components/group-table.slint`
- `crates/desktop-ui/ui/components/member-list.slint`
- `crates/desktop-ui/ui/pages/duplicate-workspace.slint`
- `crates/desktop-ui/ui/pages/review-delete-workspace.slint`
- `crates/desktop-ui/ui/pages/scan-page.slint`

## 已知边界

- `local_node_e2e` 真实包场景未运行，因为本机本轮没有提供测试 release root；保留明确 ignored 证据。
- 当前为得到精确 `total_rows`，窗口读取会从最近检查点继续遍历中心游标到末尾，但只物化当前最多 200 行，内存不会累加历史结果。若后续真实 PostgreSQL 大结果验收显示读取量成为瓶颈，可在不改变 UI 契约的前提下增加按类别/组计数查询。
- 构建仍显示 NodeEngine 既有 unused/dead-code 警告，本任务未扩大范围清理。
- 未运行真实媒体、未打包、未部署、未触碰 `I:\Tool`。

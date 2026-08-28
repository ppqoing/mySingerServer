# Task 7A 实施报告：扫描清单单事务收尾

## 结果

Task 7A 已完成。NodeStore 新增 `finalize_scan_manifest`，把本轮已见路径、完整解析路径、活动位置失效、file outbox、高水位和 `library_revision` 收束到一个 SQLite 业务事务中。当前只改动 NodeStore 边界，不接入 NodeEngine、任务文件或运行时恢复。

## TDD 证据

先新增行为测试并执行缺失接口的 RED：

```text
cargo test -p dedup-node-store --test inventory_finalize --locked -- --test-threads=1
exit 1
```

失败原因是 `ResolvedScanFile`、`ScanFinalizeInput` 和 `finalize_scan_manifest` 尚不存在；没有使用源码文本检测代替行为断言。

复审补充的 no-op 行为也先执行 RED：完全相同的活动关系重复收尾时，旧实现错误地产生第 4 条 file outbox；修复后不再重复写出。TEMP 清单的 1,001 行边界测试同时验证了跨批次的末尾路径不会被失活。

实现后 GREEN 与回归结果：

- `inventory_finalize`：8/8 通过。
- `outbox`：6/6 通过。
- `dedup-node-store` 全量：56/56 通过（4 个单元测试和全部集成测试）。
- `cargo fmt --all -- --check`：通过。
- `git diff --check`：通过。

所有 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，并关闭增量/debug 信息、清除了 MinGW 编译环境变量。执行期间 C 盘约 15.69--15.71 GiB、D 盘约 10.24 GiB，未触发清理线。

## 已覆盖行为

- 完整解析项在收尾事务内写入活动位置，并产生 file outbox；返回的高水位等于同一事务提交后的 SQLite 高水位。
- `seen_paths` 包含读取失败路径时，该路径不会被误失活。
- `D:\A` 只按路径组件失活，`D:\AB` 和 `D:\A2` 保持活动。
- 根、已见路径和解析项按规范路径排序去重；同一路径对应不同内容键直接拒绝。
- 解析内容必须存在且 `base_complete=1`，缺失或占位内容不能伪造位置关系。
- outbox 触发器注入中途失败时，新位置、旧位置失活、outbox 和 revision 整体回滚。
- 成功收尾只由调用方主动调用；取消、枚举失败和任务级失败没有收尾参数，仍由调用方跳过该接口。

## 实现边界

- 新增 `crates/node-store/src/inventory.rs`，导出 `ResolvedScanFile`、`ScanFinalizeInput`、`ScanFinalizeResult`。
- 使用连接级 TEMP 清单表保存本次输入，单批最多 1,000 行；清空和装载 TEMP 清单、正式表变更、失效 outbox、版本高水位和 revision 在同一显式事务中完成，避免逐行隐式提交。
- 未修改 `content.rs`、`outbox.rs`、任务表 API、NodeEngine、actor、协议、生产目录或 `I:\Tool`。

## 后续风险与范围

Task 7B 仍需把 BaseCompute 的生产路径从 SQLite 任务项迁移到瞬态 TSV，并在全部 ACK 收束后调用本接口；Task 7C 再接入当前进程快照和 run 目录清理。本提交不宣称这些后续工作已完成，也未进行真实媒体、打包或部署测试。

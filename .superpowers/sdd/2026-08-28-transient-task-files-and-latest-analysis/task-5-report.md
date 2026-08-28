# Task 5：枚举前冻结扫描根与物理盘 lane

## 结果

Task 5 已完成。节点在第一次 Everything/Windows Walker 枚举前，对本轮全部扫描根做规范化、排序、去重和物理存储解析，生成只读 `ScanDiskPlan`。枚举结果按 `NormalizedPath::is_within` 选择组件最深根，并关联 `TaskDiskLane`；同一物理盘合并，复合盘保留全部底层盘编号，混合介质类型降为 `Unknown` 并采用保守额度。

生产 actor 在枚举前建立计划。任一根解析失败都返回稳定错误 `SCAN_ROOT_STORAGE_RESOLVE_FAILED`，不调用枚举器，也不进入扫描收尾。`ScheduledFileReader` 的 Hash 和 Media 许可均从同一冻结计划取得物理盘集合、介质类型和本轮逐盘额度，不再在读取阶段调用 Windows 存储解析器或维护路径位置缓存。

## 代码范围

- 新增 `crates/node-engine/src/scan/root_plan.rs`：根计划、lane 合并、组件边界匹配和配置额度冻结。
- 修改 `crates/node-engine/src/scan/mod.rs`、`actor.rs`：导出计划类型，并在生产枚举前建立计划、失败时跳过枚举。
- 修改 `crates/node-engine/src/scan/pipeline.rs`：读取器消费冻结 lane；Hash/Stage1 使用只读物理盘身份接口，读取期旧消费接口已经删除。
- 修改 `crates/node-engine/src/scan/base_compute.rs`、`scan/engine.rs`：改用冻结 lane 身份，避免重复解析。
- 修改 `crates/windows/src/storage_device.rs`：补充从已验证物理盘编号和类型构造调度位置的边界 API。
- 修改 `crates/node-engine/tests/scan_roots.rs`：新增调用顺序、解析失败、根边界、同盘/复合/Unknown、配置额度和 Hash/Media 同 lane 行为测试。

本任务未修改 TSV、协议、SQLite schema、UI、唯一 `DiskReadScheduler` 的许可算法，也未实现跨盘加权 dispatcher；未运行真实媒体、打包、部署或访问 `I:\Tool`。

## TDD 与验证证据

先在旧实现运行根计划测试，因计划类型不存在而真实失败（RED）；实现后首次 GREEN 通过。随后补充 Hash/Media 不增加 resolver 调用的行为测试。完整回归期间发现旧 `take_physical_disk_id` 消费语义被无状态实现破坏，定向测试真实失败；最终改为 `physical_disk_id` 只读接口，并删除读取期旧消费接口，定向和全量均通过。

使用 `CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`、关闭增量和 debug info，并清除 MinGW 环境变量，结果如下：

| 命令 | 结果 |
|---|---:|
| `cargo test -p dedup-windows --test storage_device --locked -- --test-threads=1` | 5/5 通过 |
| `cargo test -p dedup-node-engine --features test-hooks --test scan_roots --locked -- --test-threads=1` | 9/9 通过 |
| `cargo test -p dedup-node-engine --test enumerators --locked -- --test-threads=1` | 4/4 通过 |
| `cargo test -p dedup-node-engine --test disk_scheduler --locked -- --test-threads=1` | 27/27 通过 |
| `cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1` | 59/59 通过 |
| `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1` | 66/66 通过 |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

验证期间 C 盘约 17.68 GiB、D 盘约 10.24 GiB 可用，未触发清理停止线。

## Follow-up 审查修复

针对审查反馈补齐了三项边界：

1. `DiskReadScheduler` 新增带冻结逐盘额度的许可入口。混合类型 lane 即使对外表现为 `Unknown`，也会把 HDD/SSD/Unknown 配置的最小值传入唯一 scheduler；复合盘的每个底层物理盘都用该有效上限。真实测试持有前五个 permit 时第六个阻塞，释放后才放行。
2. 删除读取器的 `LaneSource::System`、普通 `ScheduledFileReader::new` 和读取期 `resolve_storage_location(path)`。生产读取器只能从枚举行生成的冻结 lane 映射创建；受控 resolver 仅保留在命名明确的测试构造中。
3. actor 共用“先建立计划、再调用枚举器”的边界，把 `PlannedScannedPath` 列表交给读取器建立不可变的 `NormalizedPath → TaskDiskLane` 精确索引。Hash 和 Media 都只查该索引，不重新按根归属匹配。

Follow-up 验证：scan_roots 11/11、storage_device 5/5、enumerators 4/4、disk_scheduler 27/27、base_compute_pipeline 59/59、node-engine lib 66/66；格式和 diff 检查通过。混合复合盘 permit、exact planned lane 及 resolver/枚举真实顺序测试均通过。

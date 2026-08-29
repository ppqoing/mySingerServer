# Task 7D3A：瞬态扫描运行器与清单收尾

## 范围

本步新增 `task_file_scan_run`，把已经落地的批量缓存分类、TSV 任务生产、Hash/Media
交错计算、taskless SQLite 持久化、清单事务和精确运行目录清理组合为一个完整扫描入口。

本步不切换 Node actor，不增加 `TaskCatalog`、任务恢复、历史任务、分析、删除、分页或
磁盘满清理，也不触碰正式部署目录。

## 实现

- 每次扫描只创建 `<data>/runtime/<task-id>` 的精确任务文件集合；路径缓存按最多 1,000
  项批量查询，完整命中只进入结果清单，不写 TSV。
- 任务文件只保存真实缺失项，Hash 与 Media 共用冻结物理盘 lane 和同一 dispatcher；
  任务行只有在 SQLite ACK 后才从 `P` 迁移为 `C/F`。
- 取消、缓存准备失败、协调器失败和 SQLite writer 收束失败均不调用扫描清单事务，并尝试
  删除当前精确 run；清理或 join 失败会合并到任务级错误中，不被静默吞掉。
- 协调器完成后保留取消令牌直到 writer join 完成；清单事务前再次检查，末端取消与
  writer join 失败都会先取得 completed owner 并精确清理，不能通过 `?` 提前泄漏目录。
- 正常完成时先关闭持久化句柄并 join 唯一 task-local SQLite writer，再调用
  `finalize_scan_manifest`，随后删除当前精确 run，最后才返回当前进程
  `CompletedScanSnapshot`。
- 清单事务返回真实 `outbox_high_seq` 和 `library_revision`。提交后再发布最终 file outbox；
  中心库失败只记录一次降级告警并保留 SQLite outbox，不回滚本地成功。
- 清单事务拒绝输入时同样精确清理当前 run；不会删除 runtime 根或其它任务目录。

## TDD 与验证

行为测试先以缺失实现或未清理 run 固定 RED，随后最小实现转为 GREEN：

| 行为 | 结果 |
|---|---:|
| 完整缓存命中直接收尾，不启动 Hash/Worker | 通过 |
| 预取消删除精确 run，不推进 revision | 通过 |
| pending 取消不提交清单 | 通过 |
| 无效清单整体回滚并删除精确 run | 通过 |
| 单文件失败仍保留既有活动位置，成功邻项进入快照 | 通过 |
| 成功清单先提交 revision，再删除精确 run | 通过 |
| 协调结束后取消仍阻止清单收尾并删除精确 run | 通过 |
| SQLite writer join 失败仍删除精确 run | 通过 |
| `task_file_scan_run` 模块测试 | 8/8 通过 |
| `dedup-node-engine --features test-hooks --lib` | 120/120 通过 |
| `dedup-node-store --test inventory_finalize` | 8/8 通过 |
| `cargo fmt --all`、`git diff --check` | 通过 |

测试复用 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`，并清除继承的
`CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`。验证时 C/D 可用空间约
14.47/11.95 GiB，未触发 10 GiB 停止线；未运行真实媒体、未打包、未部署、未触碰
`I:\Tool`。

窄审查首轮发现“末端取消漏检”和“writer join 失败目录泄漏”两项 Important，均已用上述
行为测试固定并修复；同一审查人定点复核结果为 PASS，无剩余 Critical/Important。

## 后续边界

下一步只把 Node actor 的 `BackgroundJob::Scan` 切换到本入口：生产 runtime 根固定为
`data_path/runtime`，扫描身份与旧 Stage2 任务身份分开，运行态只由
`RuntimeTaskRegistry` 发布，并在 actor 内存中只保留最近一次成功的
`CompletedScanSnapshot`。取消、关机和失败不得再写旧 `tasks/task_items/task_stages`。

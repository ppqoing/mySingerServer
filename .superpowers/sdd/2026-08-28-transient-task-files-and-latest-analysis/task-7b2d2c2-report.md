# Task 7B2D2C2：瞬态任务文件 Media 结果持久化边界

## 范围

本提交新增 `task_file_media_persistence`，消费 Media 阶段已经返回的
`completed/file_failures` 拥有型结果，执行不依赖任务表的 stage1/失败持久化，
并把状态迁移严格放在 SQLite ACK 之后。本阶段不接 actor 主循环、Worker 派发、
外层 Hash/Media 交错协调或任务 finalize。

新增模块当前生产逻辑约 739 行，行为测试约 684 行；`scan/mod.rs` 只增加模块声明和
窄 re-export，没有修改 SQLite schema、任务表或 actor 生产逻辑。

## 实现

- Completed 先校验 `TaskFileIdentity`、Worker `task_id/item_id`、16 字节 MD5、
  payload、probe 和 stage1 缺失字段，并严格校验媒体类型、尺寸/时长、Quality、
  图片单槽位、视频六槽位唯一性以及联系表 JPEG。协议错误、MD5 不匹配和缺失
  content_id 只把当前项转成失败持久化，不影响其它已返回项；非法输出不会把
  `base_complete` 写成真。
- 成功结果在同一 NodeStore actor 内加载并校验 `content_id/ContentKey`，构造
  `Stage1Output`，复用现有 stage1 写入及视频联系表 publish/confirm/rollback，
  最后调用 `commit_scan_stage1_taskless`。没有调用 guarded、reserve、claim、
  save_stage、任务表或 finalize API。
- 文件失败使用 `BasePersistMessage::new_task_file`，在返回 Failed ACK 前先以同一
  actor 操作写入 SQLite `file_faults`（当前 schema 的 WorkerCrash 类别）；故障表
  写失败即返回任务级错误，TSV 保持 `P`。队列满时消费已发送消息的 ACK 后重试，
  队列关闭或 actor/SQLite/联系表事务错误返回带 pending 所有权的任务级错误，
  剩余行保持 `P`。
- 联系表 partial 发布不再因旧 final 存在而丢弃新结果：旧文件先移到本轮备份，
  新文件再 rename 到 final；提交确认删除备份，事务失败则删除新文件并恢复旧文件。
- 只有收到并校验对应 identity、worker slot、ContentKey、媒体类型和文件大小的
  ACK，才调用 dispatcher 的 `mark_completed/mark_failed`；成功项登记稳定排序的
  `ResolvedScanFile`，不增加 `cache_hits`。ACK 前 TSV、上下文和 dispatcher 状态均
  保持 pending。

## TDD 与验证

测试使用真实 `TaskFileDispatcher`、NodeStore actor 和 task 文件，不使用源码文本
匹配；先固定 ACK 门禁、taskless 写入和失败继续处理行为，再实现生产边界。

| 行为 | 结果 |
|---|---:|
| 图片成功：ACK 前 TSV 为 P，ACK 后为 C、登记 resolved、旧任务表为空 | 通过 |
| 同批一项成功、一项失败：ACK 后仅各自迁移为 C/F | 通过 |
| Worker/协议 MD5 不匹配转当前项 F | 通过 |
| 文件失败 ACK 后置 F，不写任务表/任务项/任务阶段 | 通过 |
| Worker 非法 Quality：仅写 file fault、TSV 置 F、`base_complete` 保持假 | 通过 |
| 视频联系表成功 publish，ACK 后确认并保存缓存路径 | 通过 |
| 损坏 final 被有效 partial 替换；引用失败回滚恢复旧文件 | 通过 |
| 联系表/Store 错误返回 pending，TSV 保持 P | 通过 |
| `cargo test -p dedup-node-engine --lib task_file_media_persistence --locked -- --test-threads=1` | 8/8 通过 |
| `cargo test -p dedup-node-engine --features test-hooks --lib task_file_media_persistence --locked -- --test-threads=1` | 9/9 通过 |
| `cargo test -p dedup-node-engine --features test-hooks --lib task_file_media_compute --locked -- --test-threads=1` | 7/7 通过 |
| `cargo test -p dedup-node-engine --features test-hooks --lib task_file_base_compute --locked -- --test-threads=1` | 8/8 通过 |
| `cargo test -p dedup-node-engine --features test-hooks --lib base_persistence --locked -- --test-threads=1` | 5/5 通过 |
| `cargo test -p dedup-node-store --test file_faults --locked -- --test-threads=1` | 2/2 通过 |
| `cargo test -p dedup-node-engine --features test-hooks --lib --locked -- --test-threads=1` | 未在本轮重复（沿用 D2C2 的 92/92 证据） |
| `cargo test -p dedup-node-engine --features test-hooks --lib base_task_producer --locked -- --test-threads=1` | 0 项，退出 0（lib 无同名测试） |
| `cargo test -p dedup-node-engine --features test-hooks --lib task_dispatch --locked -- --test-threads=1` | 0 项，退出 0（lib 无同名测试） |
| `cargo fmt --all -- --check` | 通过 |
| `git diff --check` | 通过 |

测试命令统一清除了继承的 `CC/CXX/AR/RANLIB/CFLAGS/CXXFLAGS/RUSTFLAGS/RUSTC_WRAPPER`，
并复用 `C:\tmp\rust-v2-core-scope-target-task7b2d2c1`。一次未清理环境变量的重试曾被
MinGW 编译缓存触发 MSVC `LNK2019`，清理后编译和测试正常。本轮测试期间 C/D 可用空间约
18.4/11.95 GiB，未触发 10 GiB 停止线；未触碰 `I:\Tool`。

## 未闭合边界

本提交只完成 Media 结果的 taskless persist/ACK 边界，尚未把 Hash pass、Media pass
和本模块接入统一外层交错循环；也未接 Node actor 的生产主循环、任务终态 finalize、
恢复逻辑、真实媒体跑测、打包或部署。后续 D2C3/D3 负责组合这些边界。

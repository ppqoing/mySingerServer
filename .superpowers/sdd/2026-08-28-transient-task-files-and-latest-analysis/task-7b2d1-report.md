# Task 7B2D1：Hash→Media 同行续算校验报告

## 范围

本次只调整 `TaskFileDispatcher` 续算校验和对应行为测试，不改磁盘调度权重、读取许可所有权、Actor 或 BaseCompute。

Hash 阶段完成后，原始 TSV 行继续保持原字节和 `P` 状态；调用方可以在内存中携带新的 `known_md5` 与真实基础媒体缺失位申请同一身份的 Media 许可。Media ACK 后仍只把该行状态字节改为 `C`。

## TDD 证据

- RED：在基线 `484950ba5931e3d1491765c5f973d7098626cf95` 上，新增行为测试
  `hash_continuation_accepts_derived_md5_and_media_mask_without_rewriting_tsv`，旧实现因“续算只允许仍需 MD5 的基础任务原始记录”拒绝派生记录，exit 1。
- GREEN：放宽 `validate_media_continuation` 仅允许内存派生的已知 MD5、非 `TASK_NEEDS_MD5` 且非空基础媒体缺失位；同一测试 1/1 通过。

## 实现结果

- 重新读取并验证完整原始 TSV 行：`run_id`、lane、在途 identity、偏移、长度、状态、item、工作类型、规范路径、显示路径和文件大小仍绑定原行。
- 派生记录仅允许 `known_md5=Some` 和至少一个基础媒体缺失位；非法工作类型、空 MD5、残留 `TASK_NEEDS_MD5`、空缺失掩码均拒绝。
- `continuation_claimed`、原行 `P`、在途集合和单行约束保持不变；不写回派生字段、不增加第二行。
- 更新既有续算测试夹具，使其显式使用 Hash 后派生 Media 记录。

## 验证

使用冻结环境：`CARGO_TARGET_DIR=C:\tmp\rust-v2-core-scope-target`、关闭增量和调试信息、清除外部 C/C++/Rust 编译器覆盖变量。

- `cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1`：19/19 通过。
- `cargo test -p dedup-node-engine --test transient_task_files --locked -- --test-threads=1`：25/25 通过。
- `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1`：69/69 通过。
- `cargo fmt --all -- --check`：通过。
- `git diff --check`：通过。

回归输出仅包含当前 B2B 任务文件 identity 尚未接入主循环的既有 dead-code 警告；没有测试失败或编译错误。

## 风险与后续

本次未接入 Actor/BaseCompute 的真实续算生产链，也未运行真实媒体、打包或部署；主循环接入仍由后续 Task7B2D/7C 完成。调度公平、许可额度和取消语义未修改。

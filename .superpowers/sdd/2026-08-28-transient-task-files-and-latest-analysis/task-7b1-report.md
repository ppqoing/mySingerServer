# Task 7B1：外部读取许可接缝

## 范围

本次只处理 `ScheduledFileReader` 与瞬态任务 dispatcher 之间的读取许可所有权：

- 保留 `PipelineFileReader::read`、`read_with_phase` 和 `acquire_media_permit` 原接口；
- 新增 `HashPermitReader`，由调用方交付已经取得的 `ScheduledReadPermit`；
- `ScheduledFileReader` 按冻结的 `TaskDiskLane` 实现 `TaskLanePermitProvider`；
- Hash 读取在完整 MD5 完成前持有同一个 permit，结果返回后由调用方通过
  `ReadProduct::lease` 显式释放；
- 复用既有 `DiskReadScheduler`、权重/逐盘额度和运行时 IO telemetry，不引入第二套调度状态。

没有修改 BaseCompute、actor、NodeStore、Worker、协议、真实媒体配置或部署文件。

## TDD 证据

先在旧实现运行：

```text
cargo test -p dedup-node-engine --test pipeline_permit dispatcher_permit_can_be_consumed_by_external_hash_read_without_second_scheduler_acquire --locked -- --test-threads=1
```

结果为编译失败，原因是旧实现同时缺少 `HashPermitReader`、
`ScheduledFileReader: TaskLanePermitProvider` 和 `read_with_permit`。该失败对应目标行为缺口，
不是测试运行时超时或环境错误。

实现后 focused 行为测试：

```text
cargo test -p dedup-node-engine --test pipeline_permit --locked -- --test-threads=1
6 passed, 0 failed
```

覆盖内容：

1. 全局/逐盘额度为 1 时，dispatcher 先交付 Hash permit，外部许可读取在不二次申请的情况下完成；
2. Hash permit 的 active/grant/release 各只发生一次，且结果返回后仍保持 active，直到显式 Drop；
3. Media permit 的 `media_io` 在持有期间为 1，Drop 后归零；
4. 取消等待会清除 waiting，任务文件字节不变，TSV 行继续保持 `P`；
5. provider 使用调用方交付的 `TaskDiskLane`，不根据路径重新解析物理盘；
6. 旧 `PipelineFileReader::read` 仍能取得许可并完成 Hash。

既有回归：

```text
cargo test -p dedup-node-engine --test scan_runtime_details scheduled_reader --locked -- --test-threads=1
1 passed, 0 failed

cargo test -p dedup-node-engine --test task_dispatch --locked -- --test-threads=1
18 passed, 0 failed

cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline scheduled_reader --locked -- --test-threads=1
2 passed, 0 failed
```

第一次运行 BaseCompute 过滤测试时遗漏 `--features test-hooks`，只产生既有测试夹具的编译错误；
补齐 feature 后通过，不属于产品失败。

## 实现要点

`HashPermitReader::read_with_permit` 只把外部 permit 移入一个完整 MD5 的
`spawn_blocking` 闭包，内部不调用旧 `read`、`read_with_phase`、
`acquire_scheduled_permit` 或 scheduler。旧 `read` 路径取得 permit 后复用同一内部读取函数，
因此兼容路径与新路径的实际读取生命周期一致。

新的 provider 直接把冻结 lane 转成 `DiskReadLane`，调用唯一 scheduler 的 `acquire_lane`；
waiting、active、resource 和逐盘 grant/release 都由已有 RAII 包装完成。原有
`SchedulerTaskLanePermitProvider`（裸 `DiskReadPermit`）保持不变，供旧 dispatcher/测试使用。

## 验证

```text
cargo fmt --all -- --check 通过
git diff --check 通过
```

本任务未运行真实媒体、未打包、未部署、未访问 `I:\Tool`。下一步由 Task 7B2 将该接口接入
BaseCompute 的 TSV dispatcher；接入时必须把 dispatcher 交付的 permit 传给
`HashPermitReader`，不得在读取前再次调用旧 Hash 读取入口。

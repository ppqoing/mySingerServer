# Task 2：GUI 退出后双 Node 只读终态观察器

## 范围与行为

- 新增 `crates/desktop-core/examples/physical_two_host_observer.rs`，通过
  `DEDUP_OBSERVER_FIRST_ENDPOINT`、`DEDUP_OBSERVER_SECOND_ENDPOINT`、
  `DEDUP_OBSERVER_EVIDENCE_DIR` 和 `DEDUP_OBSERVER_OUTPUT_FILE` 接收输入。
- 证据目录必须已存在且会先 canonicalize；输出名只能是单个普通文件名，使用
  `create_new` 写入该目录，拒绝覆盖和 `..` 路径。
- 一次只拥有一台 Node 的 TCP 收发半边：先完成并同步释放第一会话，才 connect 第二端点。
  会话只发送 `Hello`、`NodeStatus`、`ListTasks`、`ListRuntimeTasks`、
  `GetRuntimeTaskDetails`，不使用 Desktop GUI 会话，也不发送扫描、同步、分析、删除或配置命令。
- NDJSON 固定写 `observer_start`、每节点 `node_snapshot`、失败时
  `observer_error`、末尾 `observer_result`。任务页没有创建时间或最新排序语义，
  `latest_persistent_task` 明确 `available=false`；持久任务阶段也明确不可用。
  运行任务详情存在时输出阶段、资源和 `disk_reads`；详情缺失时输出缺失原因。
- `NODE_BUSY` 写稳定代码 `node_busy` 和诊断“节点正被 GUI 的唯一管理连接占用；请完全退出 GUI 后再观察”，
  不再连接第二端点。

## RED → GREEN 证据

1. RED：新增 loopback 真实帧测试后运行
   `cargo test -p dedup-desktop-core --test physical_two_host_observer_contract --locked -- --test-threads=1`；
   失败原因符合预期：`examples/physical_two_host_observer.rs` 尚不存在。
2. 初次 GREEN 前测试帧解码缺少直接 `prost` 依赖；补入 Desktop Core 依赖并离线更新 lockfile，随后同一
   loopback 测试通过。强化“第二 accept 必须在第一会话关闭后”后，原先通过的实现暴露 transport 后台
   Drop 延迟；改为 observer 自己拥有 TCP 收发半边后重新通过。
3. 最终命令：

   ```powershell
   $env:CARGO_TARGET_DIR='C:\tmp\rust-v2-physical-two-host-observer-target'
   cargo test -p dedup-desktop-core --test physical_two_host_observer_contract --locked -- --test-threads=1
   cargo check -p dedup-desktop-core --example physical_two_host_observer --locked
   cargo test -p dedup-desktop-core --test physical_two_hosts_e2e --locked --no-run
   cargo fmt --all -- --check
   git diff --check
   ```

   结果：观察器契约 `2 passed, 0 failed`；example check 通过；物理双主机测试仅编译通过；格式和 diff
   检查通过。

## 环境与限制

- 基线：`a65f881b34a31118614554a725a8ede692f80800`。初检 C 盘剩余 19.97 GiB、D 盘 14.77 GiB。
- 共享 target `C:\tmp\rust-v2-core-scope-target-task7b2d2c1` 的构建锁拒绝访问，改用规定的可再生
  `C:\tmp\rust-v2-physical-two-host-observer-target`；没有清理任何目录。
- 编译仍打印既有 `dedup-node-engine` 17 条 unused 警告，观察器与其契约测试无新增警告。
- 未启动 GUI、未连接物理 Node、未读取 `I:\Tool` 或真实媒体；observer 也未加入正式发布 ZIP 配置。

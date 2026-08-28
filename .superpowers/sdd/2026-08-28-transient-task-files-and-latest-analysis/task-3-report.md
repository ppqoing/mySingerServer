# Task 3 执行报告

## 状态

Task 3 已完成。本任务把配置、协议和桌面诊断边界收敛到主功能所需的最小实现：

- 协议版本保持 `5`；普通 `SaveNodeConfig` / `NodeConfigSaved` 复用原配置载荷槽位 `39/40`。
- 配置仓库只读 `bootstrap.toml`，保存时校验版本和路径后原子替换现有 `config.toml`；不再写 journal、恢复文件或 bootstrap。
- Node 启动和运行时统一使用配置仓库已解析的 data、cache、log、config 路径，生产入口不回退到 `AppLayout` 默认路径。
- 删除重启协商、宿主替换、响应 flush 重试、独立文件故障协议和 UI 管理入口；NodeStore 的 `file_faults` 表及 Worker 崩溃写入保留。
- 中心库保留 schema ID、必需列校验和同步/分析事实，删除逐表诊断与 COUNT 投影。
- Desktop 配置保存改为保存完成后由后续 Node 启动读取，不触发远程 Node 重启、恢复或任务重连。

## 行为验证

所有 Cargo 命令均使用 `C:\tmp\rust-v2-core-scope-target`，关闭 incremental/debug info，并清除 MinGW 编译环境变量。

- `cargo check -p dedup-protocol -p dedup-node-engine -p dedup-desktop-core -p dedup-desktop-ui -p node --locked`：通过。
- `cargo test -p dedup-protocol --test node_config_wire --locked -- --test-threads=1`：8/8 通过，确认 V5、39/40 槽位及旧管理载荷移除。
- `cargo test -p dedup-central-store --lib --locked -- --test-threads=1`：2/2 通过，覆盖 schema ID 不匹配和必需列缺失。
- `cargo test -p dedup-central-store --test public_contract --locked -- --test-threads=1`：1/1 通过。
- `cargo test -p dedup-desktop-core --test central_schema --locked -- --test-threads=1`：1/1 通过，1 项 PostgreSQL 环境测试按既有条件忽略。
- `cargo test -p dedup-node-engine --lib --locked -- --test-threads=1`：62/62 通过。
- `cargo test -p dedup-node-engine --test config_repository --locked -- --test-threads=1`：8/8 通过，覆盖路径解析、CAS 冲突、控制路径拒绝、原子替换、失败保留旧配置和临时文件清理。
- `cargo test -p dedup-node-engine --test node_actor --locked -- --test-threads=1`：7/7 通过。
- `cargo test -p dedup-node-engine --test node_server --locked -- --test-threads=1`：3/3 通过。
- `cargo test -p dedup-desktop-core --test node_config_controller --locked -- --test-threads=1`：3/3 通过。
- `cargo test -p dedup-desktop-core --test node_config_e2e --locked -- --test-threads=1`：2/2 通过。
- `cargo test -p node --test restart_lifecycle --locked -- --test-threads=1`：2/2 通过。
- `cargo test -p dedup-desktop-ui --test window_contract --locked -- --test-threads=1`：21/21 通过。
- `cargo test -p dedup-desktop-ui --test bindings_contract --locked -- --test-threads=1`：15/15 通过。
- `cargo test -p dedup-desktop-ui --test offscreen_layout --locked -- --test-threads=1`：16/16 通过，确认移除诊断动作栏后设置页仍可达。
- `cargo fmt --all`、`git diff --check`：通过。

## 改动范围

- `proto/node.proto` 与协议测试：收窄配置和管理载荷，保持现有编号和版本。
- `crates/node-engine`：删除重启/恢复及故障管理接口，简化配置仓库和 actor/server；保留配置路径解析并传入 NodeRuntime。
- `apps/node`：删除父进程重启生命周期，传递仓库解析路径；启动测试改为首次初始化和既有文件不改写。
- `crates/central-store`：保留 schema 校验，移除诊断查询模型和投影。
- `crates/desktop-core`、`crates/desktop-ui`：适配普通配置保存、schema 状态显示和精简设置页；删除文件故障及表诊断状态。
- 测试：改为真实文件、TCP/actor、配置保存和 Slint MainWindow 行为门禁，未使用源码文本匹配。

## 未包含

本任务未改变 NodeStore 的故障记录生产写入、扫描/计算/同步/分析主链路，也未加入任务恢复或重启恢复逻辑；未运行真实媒体、打包、部署或触碰 `I:\Tool`。历史设计文档中的旧架构描述由上层任务统一更新。

## Fix round 1：保存前机器身份校验

独立审查发现配置快照按节点索引保存；同一索引断线重连到另一物理机器时，即使版本摘要相同，旧快照也可能被发送到新会话。新增真实 TCP 重连行为测试：先加载机器 A，再让同一 endpoint 重连机器 B，断言保存失败且 B 未收到 `SaveNodeConfig`。

- RED：`cargo test -p dedup-desktop-core --test node_config_controller save_rejects_same_index_after_reconnect_to_another_machine --locked -- --test-threads=1`，稳定失败于 `Completed` 而非预期 `Failed`。
- 修复：`save_node_config` 在发起请求前比较快照 `machine_id` 与当前 `NodeSession` 的机器 ID；不一致时返回包含两端身份的明确错误，不进入 Saving 请求，不写入新节点。
- GREEN：上述定向测试 1/1 通过，B 端保存请求为 0；`node_config_controller` 4/4、`node_config_e2e` 2/2、`cargo check -p dedup-desktop-core --locked`、`cargo fmt --all -- --check`、`git diff --check` 均通过。

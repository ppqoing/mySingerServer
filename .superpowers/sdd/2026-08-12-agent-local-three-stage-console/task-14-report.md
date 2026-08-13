# Task 14 中文执行报告

## 结果

- Compute 与 Manager 最终仅发布便携 ZIP，不生成安装包；两个包都包含运行所需可执行文件、模板、启动脚本、依赖、许可证、manifest 与 SHA-256 sidecar。
- Compute 新增 `Start-Compute.ps1`，可解压到任意可写目录后启动 NodeTray；包内不含 `gui.exe`、真实 `agent.json`、`agent.db` 或 `local-control.token`。
- PostgreSQL 改为计算节点的可选同步目标：空 `pg_dsn` 不创建连接池、不阻断 Agent，本机 SQLite、扫描、三阶段分析、审核和删除仍可工作；NodeTray 表单允许数据库保持未配置。
- Agent 与 Worker 正式构建固定使用 `-tags nodynamic`，确保图片预览使用已验证的内存受限后端，不依赖宿主机动态 WebP DLL。
- `agent.example.json` 默认不携带 DSN；README 已说明八个 NodeTray 页签、本机闭环、DSN 示例、图片不落盘缩略图、删除后保留哈希/特征/历史、PostgreSQL 补传与 Manager 分阶段任务。

## TDD 与合同证据

- RED：空 `pg_dsn` 原先被 `ValidateAgent` 拒绝；空数据库表单也无法往返。最小实现后 `MissingDSN|PostgresToRemainUnconfigured|EmptyPostgres` 聚焦测试转绿。
- RED：Compute 包原合同没有启动脚本，也没有显式拒绝真实 SQLite/控制令牌；更新生产打包脚本与合同后转绿。
- RED：构建脚本仍引用已移除的 `internal/agentcontrol`，并且 Agent 未固定 `nodynamic`；更新为 TCP `internal/nodetray/agentclient` 与正式 tag 后供应链门禁转绿。
- 新增 `scripts/test-agent-local-console.ps1`，对 stage 必需文件、空 PostgreSQL 模板、监听端口及 Agent 命名管道残留做 fail-closed 静态核验；该脚本明确不删除用户文件。

## 新鲜验证

- `go test -count=1 ./internal/config ./internal/nodetray/config ./cmd/agent`：PASS。
- 同三包 `go test -race -count=1`：PASS。
- Task 14 指定受影响包 `go vet`（将计划中不存在的 `./cmd/nodetray` 更正为实际包 `./nodetray`）：PASS。
- NodeTray `npm.cmd test -- --run`：24 个测试文件、115 个测试 PASS；`npm.cmd run lint -- --quiet`：PASS。
- `scripts/test-node-tray-supply-chain.ps1`：PASS。
- `scripts/test-package-node-release.ps1`：PASS，Compute 合同 22 个文件。
- `scripts/test-package-portable-release.ps1`：PASS，Compute 与 Manager 双包合同通过。
- `scripts/test-agent-local-console.ps1 -StageDir artifacts/stage-local-console-20260813`：静态合同 PASS，PostgreSQL 未配置。
- Visual Studio 2022 + `C:\vcpkg` + Go 1.26.5 + WinLibs 实际 Windows 构建：PASS；原生 CTest 18/18、依赖闭包、WebView2、Wails/NodeTray 与六个 EXE 均完成，stage 为 `artifacts/stage-local-console-20260813`。
- `git diff --check`：PASS。

## 完整门禁与运行边界

- `go test -p=1 -count=1` 的显式源码包全集为 PARTIAL：绝大多数包通过，但 `cmd/helper` 两项测试仍依赖未显式传入的 VideoCore stage/资源清理夹具；`internal/helper` 的受限令牌用例在本机返回 `CreateRestrictedToken: The parameter is incorrect`。这三项不是 Task 14 变更引入，实际完整 Windows 构建已通过。
- 直接 `go test ./...` 的包发现还会遇到用户现有 `artifacts/releases/MySingerServer-Compute/data/agent` ACL 拒绝；未修改该目录或 ACL。
- 新 stage 的无 PostgreSQL独立启动验收为 PARTIAL：机器上已有用户 Agent PID 405300 持有全局单实例；本任务未停止或接管该进程。
- PostgreSQL 集成补传为 PARTIAL：当前未设置 `TEST_POSTGRES_DSN`，因此没有连接真实测试库。
- 含重复媒体、Everything 首次长索引、真实 Helper 删除成功/失败/uncertain 和 Manager Stage 2/3 的破坏性端到端验收为 PARTIAL：当前没有操作员提供的可丢弃媒体目录，验收脚本不会删除用户文件。

## 发布候选

- 已使用上述实际 stage 生成并验证 Compute/Manager 候选 ZIP；最终提交后会复用同一已验证 stage，仅用最终提交号重写 manifest 并生成最终 ZIP 与 SHA-256。

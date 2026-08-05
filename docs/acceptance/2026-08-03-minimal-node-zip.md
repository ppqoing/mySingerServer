# mySingerServer 最小媒体节点 ZIP 验收记录

日期：2026-08-03  
目标：Windows x64 媒体节点生产部署 ZIP  
源码标识：`N/A_NO_GIT_METADATA`（当前目录无 Git 元数据）

## 1. 最终发布物

| 项目 | 值 |
|---|---|
| ZIP | `artifacts/releases/MySingerServer-node-win-x64-20260803.zip` |
| ZIP 大小 | `83,466,561` 字节 |
| ZIP SHA-256 | `956c18fdba448dbd8c297c6753ba03b27753f50d4d8a2d6a5a739f5be4b7a55e` |
| 侧车哈希 | `artifacts/releases/MySingerServer-node-win-x64-20260803.zip.sha256` |
| ZIP 顶层目录 | `MySingerServer/`，且只有一个顶层目录 |
| 包内文件数 | 20 |
| 发布清单条目 | 19（按设计不含清单自身） |
| 原生依赖闭包 | 6 个 DLL（含 `videocore.dll`） |

## 2. 构建门禁

本次使用全新 stage：

```text
.tmp/node-release-full-stage-20260803-002
```

构建/测试结果：

- VideoCore CMake Release 构建成功。
- VideoCore CTest：18/18 通过。
- VideoCore 导出：10/10 精确匹配。
- 递归原生依赖闭包：PASS，6 个 DLL。
- Web UI：13 个测试文件、152 个测试通过。
- Web UI ESLint：通过。
- Web UI production build：通过。
- 节点控制包：`internal/nodectl`、`internal/agentcontrol`、`internal/helpercontrol` 全部通过。
- NodeTray 前端：18 个测试文件、86 个测试通过。
- NodeTray Go 包：全部通过。
- NodeTray Wails production build：通过。
- NodeTray PE：amd64。
- NodeTray 执行级别：`asInvoker`。
- WebView2 Bootstrapper Authenticode：`Valid`。

### 2.1 受控提权环境差异

完整构建首次在需要写入 `C:\vcpkg` 缓存的受控提权环境中运行时，命名管道 ACL 字符串把当前用户 SID 规范化为 `LA`，导致测试对原始 SID 字符串的精确比较失败。相同 `TestPipeACL` 及三个节点控制包在普通用户令牌下重新运行后全部通过。

因此没有修改、跳过或削弱 ACL 测试和产品 ACL。后续流程复用了本轮成功生成并已通过 18/18 CTest 的 VideoCore，在普通令牌下构建 Go/NodeTray 组件。

## 3. 打包合同

新增并执行：

```powershell
.\scripts\test-package-node-release.ps1
```

结果：`NODE RELEASE PACKAGE CONTRACT PASS files=16`。

合同覆盖：

- 只复制固定节点白名单和 `native-dependencies.json` 指定的 DLL。
- 不把 full stage 中的 `gui.exe`、`agent.json`、`helper.json`、`gui.json` 带入 ZIP。
- 原生依赖文件被修改、与清单 SHA-256 不一致时拒绝打包。
- Agent 示例包含密码型 PostgreSQL DSN 时拒绝打包。
- Helper 示例包含真实 `allowed_roots` 时拒绝打包。
- Helper 在发布清单中记录为随包提供、默认关闭、需要管理员权限。
- ZIP 只有一个 `MySingerServer/` 顶层目录。
- 侧车 SHA-256 与 ZIP 重新计算值一致。

## 4. 最终 ZIP 独立复核

正式 ZIP 重新解压到新的独立目录：

```text
.tmp/node-release-final-verify-20260803-002
```

独立复核结果：`PASS`。

检查项：

- 20 个实际文件与 19 个 `release-manifest.json` 条目逐项匹配。
- 每个清单条目的相对路径、字节大小和 SHA-256 重新计算一致。
- 6 个 `native-dependencies.json` DLL 的 SHA-256 重新计算一致。
- `gui.exe`、旧 `mediacore.dll`、FFmpeg CLI 和真实运行 JSON 均不存在。
- `agent.example.json` 无密码型 DSN。
- `helper.example.json` 的 `allowed_roots` 为空。
- 11 个应为 x64 的 EXE/DLL 均为 amd64。
- `MicrosoftEdgeWebview2Setup.exe` Authenticode 为 `Valid`。
- `Everything64.dll` Authenticode 为 `Valid`。
- NodeTray：显式 `asInvoker`。
- Helper：显式 `requireAdministrator`。
- Agent/Worker：无嵌入应用清单，使用 Windows 默认权限。
- 四个自有 EXE 的 Authenticode 均为 `NotSigned`，未误报为已签名。
- ZIP 侧车哈希再次匹配。

## 5. 包内容与排除项

包内包含：

- `nodetray.exe`
- `agent.exe`
- `worker.exe`
- `helper.exe`（默认关闭，需要管理员权限）
- `videocore.dll`
- 递归 FFmpeg DLL 闭包
- `Everything64.dll`
- `MicrosoftEdgeWebview2Setup.exe`
- `agent.example.json`
- `helper.example.json`
- `native-dependencies.json`
- `release-manifest.json`
- `README-节点部署.md`
- FFmpeg/WebView2 许可和声明

明确排除：

- `gui.exe`
- `mediacore.dll`、`libmediacore.a`
- `ffmpeg.exe`、`ffprobe.exe`、`ffplay.exe`
- `agent.json`、`helper.json`、`gui.json`
- 源代码、测试、构建缓存、PDB/导入库
- 当前机器的 PostgreSQL DSN、密码、令牌和真实媒体目录

## 6. 部署方法

将 ZIP 中完整 `MySingerServer` 文件夹复制到 `C:\Program Files\`，最终路径必须为：

```text
C:\Program Files\MySingerServer\nodetray.exe
```

启动后通过 NodeTray 图形界面交互式配置，不直接修改生产 JSON。Helper 为可选组件，默认关闭；启用或启动 Helper 时需要管理员权限。

## 7. 未执行的动态验收

本次没有启动、停止或重启 NodeTray、Agent、Worker、Helper，没有运行 WebView2 安装器，也没有修改注册表、计划任务或服务。

因此本记录不声明以下项目已通过：

- 目标机器实际部署和 UAC 流程。
- PostgreSQL 实际连接与同步。
- 真实媒体目录处理。
- Everything IPC/Walker 回退运行时行为。
- Helper soft/hard 删除运行时行为。
- 开机启动。
- 24 小时驻留长测。

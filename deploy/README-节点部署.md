# mySingerServer 媒体节点部署说明

本压缩包用于在 Windows x64 机器上部署 mySingerServer 媒体节点，包括托盘管理程序、Agent、Worker、可选 Helper、VideoCore 和所需原生运行库。

## 快速部署

1. 将压缩包内完整的 `MySingerServer-Compute` 文件夹解压到任意本地、当前用户可写的目录，例如 `D:\Apps\MySingerServer-Compute`。不要只复制单个 EXE，也不要从压缩包内直接运行。
2. 不能解压到 UNC 网络路径（如 `\\server\share\...`）；可使用本地磁盘或可移动磁盘。
3. 确认 `nodetray.exe`、所有同级 EXE/DLL、`data` 目录和许可证目录都保留在同一个包根目录。新包不会预建 `data\helper`。
4. 启动 `nodetray.exe`。
5. NodeTray 会自动安全导入包内默认配置；在托盘界面的配置页签中交互式填写 Agent 参数并保存。不要直接修改生产 JSON 配置文件。
6. 根据需要选择自动或手动启动，并决定是否启用开机启动。

机器 ID 由 CPU ID、主板序列号和 Windows MachineGuid 自动计算为 `node-<sha256>`，无需填写；NodeTray 只在概览页显示该只读 ID。

## 组件说明

- `nodetray.exe`：常驻托盘管理界面，用于配置、启动、停止和重启节点组件。
- `agent.exe`：媒体节点 Agent。
- `worker.exe`：调用 `videocore.dll` 完成媒体计算。
- `helper.exe`：可选删除辅助组件，随包提供但默认不启用，也不会由 Agent 自动启动。Helper 需要管理员权限，只有用户显式启用或启动时才可能出现 UAC 提示。
- `Everything.exe`、`Everything64.dll`：Everything 1.4 后台客户端和 SDK 运行库。启用 `use_everything` 后，Agent 会在 IPC 不可用时自动执行同目录的 `Everything.exe -startup`，并让扫描等待索引数据库完成加载。程序不会安装或管理 Windows Everything 服务。
- `MicrosoftEdgeWebview2Setup.exe`：WebView2 Runtime 官方引导程序。只有目标机缺少 WebView2 时才由用户手动运行。

## Helper 安全配置

Helper 必须与 Agent 位于同一台机器，建议由同一账号运行，并需要管理员权限。只在确实需要删除辅助功能时启用，并在界面中把 `allowed_roots` 配置为明确、窄范围的媒体目录。默认采用 soft 删除；是否允许硬删除由用户在配置界面中明确决定。

首次解压后即可在 NodeTray 界面中查看和修改 `data\helper\helper.json`。保存 Agent、Helper 与 NodeTray 配置都不需要管理员权限；只有真正启动 `helper.exe` 时 Windows 才会显示 UAC 提示。

当包根允许普通用户写入时，启用 Helper 会带来额外的提权风险：管理员任务会从该包根启动 `helper.exe`。只应在受信任的本地目录中启用它，并限制该目录的写入权限；移动、替换或允许不受信任用户写入包根后，应先禁用并重新检查 Helper 配置和任务。

## 便携数据、配置和日志目录

| 用途 | 目录 |
|---|---|
| 程序文件 | 包根目录（`nodetray.exe` 同级） |
| Agent 配置与日志 | `data\agent\` 与 `data\agent\logs\` |
| Helper 配置与日志 | `data\helper\` 与 `data\helper\logs\` |
| NodeTray 设置与 WebView2 数据 | `data\nodetray\` 与 `data\nodetray\webview2\` |

包内预置 `data\agent\agent.json`、`data\helper\helper.json` 和 `data\nodetray\tray.json`；Agent 的 Worker 参数唯一位于 `data\agent\agent.json` 的 `worker` 段，不会生成 `worker.json`。所有默认运行路径都相对包根，不依赖系统安装目录或用户配置目录。

如需移动整个计算包，请先通过 NodeTray 停止组件，再完整移动整个目录。移动后在 NodeTray 中检查并修复登录启动；若曾启用 Helper，也要检查并重新保存其任务配置，使其指向新目录。不要混用旧目录中的 `data`。

## 哈希验证

发布目录中的 `.zip.sha256` 文件记录整个 ZIP 的 SHA-256。可使用 PowerShell 验证：

```powershell
Get-FileHash -Algorithm SHA256 .\MySingerServer-compute-win-x64-*.zip
```

计算结果应与对应 `.zip.sha256` 文件中的 64 位哈希完全一致。包内 `release-manifest.json` 还记录了各发布文件的大小和 SHA-256。

## 基础排查

- NodeTray 无法启动：确认程序位于完整的本地包根目录，而非 UNC 路径，并确认系统已安装 WebView2 Runtime。
- Agent 无法连接：检查 Agent 监听地址、中央 GUI 配置的 Agent 地址、状态页上报 ID、PostgreSQL 连接和 Windows 防火墙；不要在聊天、日志截图或问题报告中公开 DSN、密码或令牌。
- 路径扫描长期等待：确认任务管理器中存在 `Everything.exe`，并查看 Agent 日志中的 Everything IPC/索引等待状态；首次建立大型索引时扫描会持续等待，不会自动切换为普通目录遍历。
- Helper 无法启动：确认已显式启用、配置了有效的窄范围 `allowed_roots`，并检查 Helper 日志目录。
- 需要卸载：先通过 NodeTray 停止 Agent、Worker 和 Helper，再删除整个包根目录；包内运行期配置和日志会随该目录一并移除。

## 验收边界

本发布包在生成时仅进行静态构建、白名单裁剪、依赖闭包、文件清单和 ZIP 哈希复核。它不代表已在当前目标机上启动组件、连接 PostgreSQL、处理真实媒体目录或完成长时间驻留测试。

# mySingerServer 媒体节点部署说明

本压缩包用于在 Windows x64 机器上部署 mySingerServer 媒体节点，包括托盘管理程序、Agent、Worker、可选 Helper、VideoCore 和所需原生运行库。

## 快速部署

1. 将压缩包内完整的 `MySingerServer` 文件夹解压或复制到 `C:\Program Files\`。
2. 确认最终程序路径为 `C:\Program Files\MySingerServer\nodetray.exe`。不要只复制单个 EXE，也不要从其他目录直接运行。
3. 启动 `nodetray.exe`。
4. 在托盘界面的配置页签中交互式填写 Agent 参数并保存。不要直接修改生产 JSON 配置文件。
5. 根据需要选择自动或手动启动，并决定是否启用开机启动。

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

## 配置和日志目录

| 用途 | 目录 |
|---|---|
| 程序文件 | `C:\Program Files\MySingerServer\` |
| Agent 配置与日志 | `C:\ProgramData\MySingerServer\Node\` |
| Helper 配置与日志 | `C:\ProgramData\MySingerServer\Helper\` |
| NodeTray 用户设置 | `%LOCALAPPDATA%\MySingerServer\NodeTray\` |

包内 `agent.example.json` 和 `helper.example.json` 仅为脱敏示例。实际配置由 NodeTray 界面生成并保存到上述运行期目录。

## 哈希验证

发布目录中的 `.zip.sha256` 文件记录整个 ZIP 的 SHA-256。可使用 PowerShell 验证：

```powershell
Get-FileHash -Algorithm SHA256 .\MySingerServer-node-win-x64-*.zip
```

计算结果应与对应 `.zip.sha256` 文件中的 64 位哈希完全一致。包内 `release-manifest.json` 还记录了各发布文件的大小和 SHA-256。

## 基础排查

- NodeTray 无法启动：确认程序位于固定安装路径，并确认系统已安装 WebView2 Runtime。
- Agent 无法连接：检查 Agent 监听地址、中央 GUI 配置的 Agent 地址、状态页上报 ID、PostgreSQL 连接和 Windows 防火墙；不要在聊天、日志截图或问题报告中公开 DSN、密码或令牌。
- 路径扫描长期等待：确认任务管理器中存在 `Everything.exe`，并查看 Agent 日志中的 Everything IPC/索引等待状态；首次建立大型索引时扫描会持续等待，不会自动切换为普通目录遍历。
- Helper 无法启动：确认已显式启用、配置了有效的窄范围 `allowed_roots`，并检查 Helper 日志目录。
- 需要卸载：先通过 NodeTray 停止 Agent、Worker 和 Helper，再移除程序目录；运行期配置和日志不会因删除程序目录而自动清除。

## 验收边界

本发布包在生成时仅进行静态构建、白名单裁剪、依赖闭包、文件清单和 ZIP 哈希复核。它不代表已在当前目标机上启动组件、连接 PostgreSQL、处理真实媒体目录或完成长时间驻留测试。

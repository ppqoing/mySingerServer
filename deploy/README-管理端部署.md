# MySingerServer 管理端便携包

本压缩包只包含管理端 `gui.exe`，不包含计算节点、Everything、本地 PostgreSQL 或任何媒体处理组件。解压到 Windows 电脑的可写目录即可启动并配置；PostgreSQL 与 Agent 可在启动后再连接。

## 配置与启动

1. 可直接双击 `gui.exe`，或在 PowerShell 中执行 `./Start-Manager.ps1`。首次双击时，程序会在同目录自动生成 `gui.json`。
2. `gui.example.json` 仅供参考，无需手工复制。请在设置页填写外部 PostgreSQL 的连接地址和可访问的 Agent 地址。即使 PostgreSQL 或 Agent 暂时不可用，程序仍可启动并进入设置页。
3. 保存设置后，程序会自动重启以应用新配置。配置中如需密码，请仅写入本机的 `gui.json`，不要提交或分发它。

如果不希望自动打开浏览器，可执行：

```powershell
./Start-Manager.ps1 -no-browser
```

管理端所需的 PostgreSQL 是外部服务，本包不会安装、启动或保存本地数据库。运行日志固定写入 `<gui.exe 所在目录>\data\logs\gui.log`，不能通过 `gui.json` 改变；请确保解压目录允许当前用户写入。

## 完整性验证

同目录的 `.zip.sha256` 记录整个 ZIP 的 SHA-256。解压前可执行：

```powershell
(Get-FileHash .\MySingerServer-manager-win-x64-*.zip -Algorithm SHA256).Hash
```

结果应与对应的 `.zip.sha256` 文件中记录的哈希一致。包内 `release-manifest.json` 还记录每个发布文件的大小和 SHA-256。

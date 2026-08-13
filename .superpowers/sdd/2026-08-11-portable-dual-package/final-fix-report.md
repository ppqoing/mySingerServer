# 完全便携双发布包最终修复报告

## 结论

状态：`DONE_WITH_CONCERNS`

指定的 Critical、Important 和直接冲突文档均已修复，且使用新鲜 RED/GREEN 与完整指定门禁验证。`DONE_WITH_CONCERNS` 仅表示固定 MinGW 工具链和真实运行环境仍不可用，不表示存在已知未修复的本轮源码缺陷。

- 基线：`6ec15b63179a5fbeee208833f8cac2013209d248`
- 工作树：`D:\code\mySingerServer\.worktrees\portable-dual-package`
- 分支：`codex/portable-dual-package`
- 未下载工具链，未复用旧 stage，未生成伪最终 ZIP。
- 未跟踪 `.codex-temp/` 未暂存。

## RED / GREEN 证据

### 1. Compute 不预建 Helper 目录

RED：先修改 Compute 精确文件合同，要求 ZIP 不含 `data/helper/.gitkeep` 且解压后不存在 `data\helper`。旧实现失败，差异明确为：

```text
data/helper/.gitkeep =>
ASSERTION_FAILED: ZIP file list differs
```

GREEN：`package-node-release.ps1` 只预置 `data\agent` 和 `data\nodetray`。解压合同通过，共 21 个实际文件。Windows 跨层测试按解压后的目录形状创建普通用户可写根，通过 `production.ResolvePortableLayout` 调用生产 `config.NewStore`；Store 初始化前后均不存在 `data\helper`。现有 Helper owner、受保护 DACL 和普通用户 mutation 权限拒绝测试保持通过，没有削弱 ACL 检查。

### 2. GUI 使用最终 EXE 路径

RED：新增 Windows alias/最终路径与最终 UNC 注入测试，并新增共享最终路径包测试；旧代码分别因 `resolveGUIExecutablePath`、`ResolveExisting` 不存在而编译失败。

GREEN：新增 `internal/shared/finalpath.ResolveExisting`。Windows 使用 `CreateFile` 打开现有映像，并由 `GetFinalPathNameByHandle` 返回最终对象路径；`cmd/gui` 在计算默认配置和日志根前调用该边界。NodeTray 进程身份检查也复用同一实现，删除重复代码。测试证明：

- 启动别名 `D:\junction\manager\gui.exe` 最终解析到 `E:\portable\MySingerServer-Manager\gui.exe` 后，配置与日志均使用最终根；
- 映射盘别名最终解析为 `\\server\share\...\gui.exe` 时，运行时路径解析拒绝 UNC；
- 显式 `-config` 仍按既有当前工作目录绝对化语义覆盖默认配置；
- `cmd/gui` 与共享最终路径包均可交叉编译为 `linux/amd64`，非 Windows 构建保持可编译。

### 3. Manager 模板 PostgreSQL 端点 fail closed

RED：新增 LAN PostgreSQL IP、内部 DNS 和错误 scheme 用例。旧实现实际生成了 `postgres-lan-host` Manager ZIP，随后测试失败为：

```text
ASSERTION_FAILED: unsafe manager template was accepted: postgres-lan-host
```

GREEN：模板只接受 `postgres` 或 `postgresql` scheme，host 只接受 `127.0.0.1` 或 `localhost`。密码、敏感 query key 和非回环 Agent 的原有拒绝逻辑保持不变。`postgresql://dedup@localhost:5432/dedup` 正向占位合同通过。

### 4. 原子发布回滚删除绑定文件身份

RED：在旧代码的“哈希完成、按路径删除之前”hook 中执行确定性替换：把已验证 ZIP 重命名为 `.published-original`，再在原路径写入用户文件并正常返回。旧实现删除了替换后的用户文件，失败为：

```text
ASSERTION_FAILED: rollback deleted the user file that replaced the verified path
```

GREEN：回滚为每个已发布文件调用 `CreateFileW`，请求 `GENERIC_READ | DELETE`，共享 `FILE_SHARE_READ | FILE_SHARE_DELETE` 而不共享写入；SHA-256 通过同一持续打开的 `FileStream` 计算，再在同一句柄上调用 `SetFileInformationByHandle(FileDispositionInfo)` 请求删除。Microsoft 文档要求此操作在 `CreateFile` 时请求 `DELETE` 权限，并明确删除在句柄关闭时作用于该句柄指向的文件对象：

- [SetFileInformationByHandle](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-setfileinformationbyhandle)
- [FILE_DISPOSITION_INFO](https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-file_disposition_info)

确定性竞态 GREEN 证明：hook 可以把锁定对象重命名并在原路径放入用户文件；关闭 lease 后 `.published-original` 被删除，而原路径用户文件内容保持不变。句柄打开、hook、删除或关闭失败均追加 `cleanup_warnings` 并继续处理后续项。既有“哈希不符保留用户修改”“单项 cleanup 失败不停止回滚”“候选清理 warning 不替换原发布错误”合同均继续通过。

## 文档修正

- Manager 推荐直接双击 `gui.exe`；`Start-Manager.ps1` 明确为在 PowerShell 中运行。
- Compute 完整解压后先启动 `nodetray.exe`，Agent 与 Helper 生产配置由 NodeTray UI 生成；不再指导手工准备 `helper.json`。
- Compute 设计、计划和部署说明明确：发布包不包含 `data\helper`、`.gitkeep` 或空 ZIP 目录项，Helper 目录由提权写入器首次安全创建。
- Manager 日志固定为 `<gui.exe 所在目录>\data\logs\gui.log`，`gui.json` 不能改变该位置。
- 计划与设计同步写入 GUI 最终路径、Manager DSN scheme/host 白名单和句柄绑定回滚合同。

## 新鲜验证

以下命令均在本修复工作树运行并返回退出码 0：

1. Go 测试（10 个受影响包）：

```powershell
go test -count=1 `
  .\internal\nodetray\production .\internal\nodetray\config `
  .\internal\nodetray\windows\loginstart .\internal\nodetray\windows\task `
  .\internal\nodetray\process .\internal\shared\finalpath `
  .\nodetray .\cmd\gui .\internal\gui .\internal\config
```

2. Go vet（8 个受影响包）：

```powershell
go vet .\internal\nodetray\production .\internal\nodetray\config `
  .\internal\nodetray\process .\internal\shared\finalpath `
  .\nodetray .\cmd\gui .\internal\gui .\internal\config
```

3. 非 Windows 编译边界：`GOOS=linux GOARCH=amd64 CGO_ENABLED=0` 下，`cmd/gui` 与 `internal/shared/finalpath` 的 `go test -c` 均通过。

4. 发布与供应链合同：

```text
NODE RELEASE PACKAGE CONTRACT PASS files=21
MANAGER RELEASE PACKAGE CONTRACT PASS files=5
PORTABLE RELEASE PACKAGE CONTRACT PASS
NODETRAY_SUPPLY_CHAIN_GATE_PASS
```

5. `git diff --check`：PASS。

## Concerns / 验收边界

- `BLOCKED_TOOLCHAIN_MISSING`：固定路径下的 `gcc.exe`、`windres.exe`、`dlltool.exe` 均不存在；`artifacts/portable-dual-stage-20260811` 和两个 `portable-20260811` ZIP 目标也不存在。本轮没有运行完整构建或最终双包生成，不复用旧 stage，不下载替代工具链。
- `BLOCKED_RUNTIME_ACCEPTANCE`：未实际完成 GUI 双击/浏览器、UAC、Helper 首次提权写入与计划任务、外部 PostgreSQL、远端 Agent、Everything 首次索引和真实媒体扫描。
- 本轮按要求只运行受影响 Go 测试/vet、三个发布合同、供应链与差异检查；没有把未运行的全仓测试或真实设备验收声明为 PASS。

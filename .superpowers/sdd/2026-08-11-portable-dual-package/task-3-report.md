# Task 3 - GUI 便携运行时报告

## 改动

- 新增 `cmd/gui/runtime_paths.go`：以 GUI EXE 的最终绝对路径目录为根目录；默认读取 `gui.json`，日志固定写入 `data\\logs\\gui.log`，拒绝 UNC 根目录。
- 显式 `-config` 继续按既有语义转换为绝对路径；`-config` 的 flag 默认值改为空，以区分默认与显式覆盖。
- 新增滚动文件日志和控制台多路输出；配置读取失败会先写入便携日志。
- 监听流程改为先 `net.Listen`、再可选打开经校验的本机 URL、最后 `Serve(net.Listener)`；`-no-browser` 会抑制浏览器。
- Windows 使用 Shell 打开 HTTP URL，并通过稳定中文 MessageBox 摘要报告失败；非 Windows 保持无 UI 通知和不支持浏览器启动的适配器。
- 启动通知不会传递原始错误、DSN 或私有路径。

## RED

先新增路径、日志、URL、监听顺序、`-no-browser` 与启动失败通知测试。运行：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\cmd\gui -run 'RuntimePaths|RuntimeLogger|Browser|StartupFailure'
```

结果为 FAIL：`resolveGUIRuntimePaths`、`newGUIRuntimeLogger`、`localBrowserURL`、监听/浏览器适配器和启动包装器尚不存在。首次受限环境还拒绝了 Go 临时编译缓存；获得执行许可后，失败原因已确认是所需功能缺失。

## GREEN 和验证

上述聚焦测试在最小实现后通过：`ok dedup/cmd/gui`。

完整 GUI 回归与静态检查通过：

```powershell
& 'C:\tmp\go1.26.5\go\bin\go.exe' test -count=1 .\cmd\gui .\internal\gui .\internal\config
& 'C:\tmp\go1.26.5\go\bin\go.exe' vet .\cmd\gui .\internal\gui .\internal\config
git diff --check
```

结果：三个测试包均为 `ok`，`vet` 和 `git diff --check` 均以退出码 0 完成。

## Concerns

- 未在真实 Windows 桌面环境点击 MessageBox 或验证默认浏览器实际启动；自动化测试仅验证调用顺序和错误边界。
- 未验证真实 PostgreSQL、真实配置和长期服务关闭流程；这些不属于本 Task 3 的单元/静态验证范围。

## 追加修正：启动失败日志

审查发现 logger 创建后的 PostgreSQL 解析/连通性与任务恢复路径并不都写入本次便携日志。先新增 `TestGUIPingFailureIsLoggedBeforeInteractiveNotification`：有效 GUI 配置指向不可达 PostgreSQL 时，旧实现的 RED 为通知前无法读取含 `ping postgres` 的 `data\\logs\\gui.log`。

GREEN 将 logger 创建后的启动失败统一经 `guiStartupFailure` 记录稳定阶段名和内部 error，覆盖配置服务、DSN 解析、PostgreSQL Ping、扫描任务恢复、Phase 2 恢复与重筛恢复；监听绑定失败也在返回前记录。通知仍为固定脱敏中文摘要。新增测试通过。

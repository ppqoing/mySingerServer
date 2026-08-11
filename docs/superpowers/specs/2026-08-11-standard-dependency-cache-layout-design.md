# Go、npm、CMake 标准依赖与缓存目录设计

## 目标

清理仓库内可再生成的构建缓存，并让构建脚本使用 Go、npm、CMake/vcpkg 在 Windows 上的标准依赖及缓存路径，避免后续构建在源码目录中重复创建多套缓存。

本设计不建立 `mySingerServer` 专属的公共缓存根目录，也不修改系统级环境变量。构建脚本读取工具本身的配置，使同一 Windows 用户下的不同仓库和工作树复用工具链标准缓存。

## 标准路径

当前机器解析出的标准路径如下：

| 用途 | 标准路径 | 解析方式 |
|---|---|---|
| Go 工作区 | `C:\Users\Administrator\go` | `go env GOPATH` |
| Go 模块缓存 | `C:\Users\Administrator\go\pkg\mod` | `go env GOMODCACHE` |
| Go 编译缓存 | `C:\Users\Administrator\AppData\Local\go-build` | `go env GOCACHE` |
| npm 下载缓存 | `C:\Users\Administrator\AppData\Local\npm-cache` | `npm config get cache` |
| npm 全局工具 | `C:\Users\Administrator\AppData\Roaming\npm` | `npm prefix -g` |
| vcpkg 根目录 | `C:\vcpkg` | 项目现有构建参数默认值 |
| vcpkg 已安装库 | `C:\vcpkg\installed` | vcpkg 标准目录 |
| vcpkg 下载缓存 | `C:\vcpkg\downloads` | vcpkg 标准目录 |

路径不得以硬编码用户名作为实现依据。Go 和 npm 路径分别通过命令查询；vcpkg 允许现有 `-VcpkgRoot` 参数覆盖，默认保持 `C:\vcpkg`。

## 构建配置

### Go

- 删除构建、测试和验收脚本中指向仓库 `.tmp`、`.codex-temp`、`.superpowers` 或 `artifacts` 的持久 `GOCACHE`、`GOMODCACHE`、`GOPATH` 覆盖。
- 默认继承 `go env GOPATH`、`go env GOMODCACHE` 和 `go env GOCACHE`。
- Windows 下需要避免并行命令争用同一 Go 缓存的验证流程改为顺序执行。
- 确实需要隔离缓存的故障注入测试只能使用系统临时目录，并在 `finally` 中回收，不得将隔离缓存留在仓库。
- Go 可执行文件仍按现有解析规则寻找；本设计不安装、升级或修改系统 `PATH`。

### npm

- 不在仓库中配置 `NPM_CONFIG_CACHE`，构建脚本使用 `npm config get cache` 返回的用户级标准缓存。
- 不把应用依赖改成 npm 全局安装。`webui/node_modules` 和 `nodetray/frontend/node_modules` 继续遵循 npm 的项目级模块解析规则。
- `npm ci` 仍以各自的 `package-lock.json` 为唯一依赖锁定来源。
- 前端构建输出仍属于项目产物，不作为公共依赖移动。

### CMake 与 vcpkg

- CMake 继续通过 `C:\vcpkg\scripts\buildsystems\vcpkg.cmake` 解析 C/C++ 包。
- libjpeg-turbo、PNG、WebP 等包统一来自 `C:\vcpkg\installed`，下载内容复用 `C:\vcpkg\downloads`。
- 删除任何指向仓库内 vcpkg 下载缓存或安装树的配置。
- `videocore/build` 和 `mediacore/build` 是可再生成的构建树，不是依赖库；本轮清理后，正常构建仍可按脚本约定重新创建。
- CMake 可执行文件继续由现有解析逻辑从 `PATH` 或 vcpkg 工具缓存中查找。

## 固定版本原生运行库

FFmpeg、Everything、Everything SDK 和 WebView2 引导程序具有项目固定版本、许可证、来源与 SHA-256 校验要求，不能冒充 Go、npm 或 vcpkg 的标准缓存。

- 本轮不改变这些运行库的版本和供应链语义。
- 清单、许可证和来源记录继续保留在仓库。
- 发布包仍只复制运行时必需文件。
- 如果以后要把这些固定二进制迁出 Git，应另立供应链设计，包含下载源、离线构建、校验失败处理和版本升级流程。

## 清理范围

清理只覆盖能够由标准工具链或现有脚本重新生成的内容：

- `.tmp` 中的 Go 编译缓存、Go 模块缓存、构建树、测试临时目录和过期发布暂存。
- `.codex-temp` 中的 Go 缓存、构建暂存、已结束进程日志和测试临时目录。
- `.superpowers/tmp` 与 `.superpowers/runtime` 中已结束任务留下的构建缓存和运行副本。
- `artifacts` 下名称明确的 Go/CMake 临时缓存目录。
- 在确认没有相关构建进程运行后，删除 `videocore/build` 和 `mediacore/build`。
- 保留 `webui/node_modules` 和 `nodetray/frontend/node_modules`；它们是 npm 标准的项目级应用依赖，不是下载缓存。

以下内容不得作为构建缓存删除：

- `artifacts/releases` 中的正式发布包。
- `.superpowers/evidence` 中的验收证据。
- `.worktrees` 中的 Git 工作树；如以后移除，必须使用 Git worktree 流程单独确认。
- Docker 镜像、构建缓存、容器和 PostgreSQL 数据卷。
- Git 已跟踪文件、未跟踪的源文件或脚本、现有未提交修改。
- `third_party` 中固定版本依赖及其供应链文件。

清理前必须检查相关构建进程是否仍在运行；删除目标必须解析为仓库内明确目录，禁止使用未解析变量、广泛通配符或递归删除仓库根目录。

## Git 忽略规则

- 将 `.codex-temp/` 加入根 `.gitignore`，防止后续缓存重新显示为未跟踪内容。
- 继续保留 `.tmp/`、`.superpowers/`、`.worktrees/`、`artifacts/`、CMake 构建树和前端 `node_modules` 的现有忽略规则。
- 只暂存本设计和后续实施明确修改的文件，不使用 `git add -A`。

## 验证与成功标准

实施完成后必须满足：

1. 仓库内不再存在持久 Go 模块缓存或 Go 编译缓存配置。
2. `go env GOPATH GOMODCACHE GOCACHE` 返回用户级标准路径。
3. `npm config get cache` 返回用户级标准缓存，两个前端仍能按锁文件执行构建。
4. CMake 构建使用 `C:\vcpkg` 工具链及其 `installed`、`downloads` 目录。
5. 运行一次代表性 Go、前端和 CMake 构建后，仓库中不会重新出现多套命名不同的 Go 缓存。
6. 正式发布包、验收证据、Git 工作树、数据库数据和用户未提交文件保持不变。
7. 清理前后记录仓库逻辑大小，明确实际释放空间。

## 失败处理

- 标准 Go、npm 或 vcpkg 路径不可用时立即停止，不自动下载或改写系统环境变量。
- 依赖缺失时报告具体命令和路径，不回退到仓库内新建缓存。
- 清理目标正在被进程使用时跳过该目标并报告，不强制终止未知进程。
- 任一代表性构建失败时保留标准缓存和诊断输出，停止后续非必要删除，不宣称迁移完成。

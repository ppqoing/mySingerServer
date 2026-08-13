# Everything 自动启动与就绪等待设计

## 目标

当 Agent 配置启用 `use_everything` 时，确保发布包携带完整的 Everything 运行文件；如果 Everything 客户端尚未运行，Agent 自动启动发布目录中的 `Everything.exe`，并让扫描任务一直等待到 Everything IPC 可用且索引数据库加载完成后再开始枚举路径。

## 范围

- 构建阶段复制并校验 `Everything.exe`、`Everything64.dll`、许可证和来源清单。
- 节点发布 ZIP 包含上述文件，并将它们纳入发布清单和 SHA-256 校验。
- Agent 只启动 Everything 后台客户端，不安装、启动或修改 Windows 服务，不触发 UAC。
- `use_everything=false` 时保持现有 Walker 行为。
- 不改变扫描协议、路径过滤、哈希、Worker 或数据库逻辑。

## 运行文件与供应链

在 `third_party/everything` 保存官方 Everything 1.4 x64 非 Lite 可执行文件、许可证和固定来源清单。现有 SDK DLL 继续由 `third_party/everything_sdk/Everything64.dll` 提供。

`scripts/build.ps1` 必须把下列文件复制到新建的阶段发布目录，并在缺失时立即失败：

- `Everything.exe`
- `Everything64.dll`
- `licenses/everything-LICENSE.txt`
- `licenses/everything-NOTICE.md`

`scripts/package-node-release.ps1` 必须把相同文件写入节点 ZIP，并由 `release-manifest.json` 记录相对路径、大小和 SHA-256。构建与打包测试必须精确断言这些文件存在。

## Agent 启动与扫描门控

Agent 不在主监听服务启动前同步等待 Everything。NodeTray 对 Agent 有 30 秒启动就绪限制；如果把首次索引等待放在监听之前，会把正常的长时间建索引误判为 Agent 启动失败。

采用扫描门控：

1. Agent 正常创建日志、数据库连接、Worker、业务监听和控制端点。
2. `use_everything=true` 时创建一个可等待的 Everything 枚举器，并在后台开始就绪检查。
3. 如果 SDK 已可用且数据库已加载，立即开放扫描门控，不启动新进程。
4. 如果 SDK 表明 Everything IPC 不可用，使用 Agent 同目录下的 `Everything.exe -startup` 启动后台客户端。
5. 启动成功后按条件轮询，不设置总超时；只有 SDK IPC 可用且 Everything 数据库已加载时才开放扫描门控。
6. 扫描任务到达时等待该门控，门控开放后继续使用 Everything 枚举。
7. Agent 收到关闭或重启信号时取消等待，使扫描和进程退出链不会被无限等待阻塞。

就绪判断必须使用 SDK 的数据库加载状态，而不能用固定 `Sleep` 或“查询结果非空”代替。空索引也可能是已经完成加载的合法状态。

## 错误处理

- `Everything.exe` 缺失或创建进程失败：记录明确错误并使用 Walker，避免整个 Agent 无法工作。
- `Everything64.dll` 缺失、导出损坏或版本不兼容：记录明确错误并使用 Walker，不尝试靠启动 EXE 修复 DLL 问题。
- Everything 进程已成功启动但仍在首次索引：默认一直等待，每 30 秒记录一次等待状态，不回退 Walker。
- 等待期间 Agent 被停止：返回取消错误并完成正常关闭。
- Everything 已就绪后，现有单根查询失败或返回空结果的 Walker 兜底语义保持不变。

## 测试

按 TDD 实施，至少覆盖：

- SDK 已就绪时不启动 `Everything.exe`。
- IPC 不可用时只启动一次 `Everything.exe -startup`。
- 启动后在数据库未加载期间持续等待，不因时间流逝回退。
- 数据库加载完成后释放等待中的扫描。
- Agent 上下文取消后等待立即结束。
- EXE 缺失、启动失败、DLL 不可用时回退 Walker。
- 构建阶段目录和节点 ZIP 同时包含 Everything EXE、DLL、许可证与 NOTICE，并被发布清单覆盖。

## 验收边界

自动化测试验证依赖选择、等待状态机、取消行为和发布文件契约。真实 Windows 机器上的首次索引耗时、托盘行为、IPC 建连和实际路径扫描需要运行发布产物后进行动态验收；未执行时不得标记为通过。

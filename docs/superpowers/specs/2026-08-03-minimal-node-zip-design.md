# mySingerServer 最小媒体节点发布 ZIP 设计

日期：2026-08-03  
状态：待用户书面确认后实施  
目标平台：Windows x64

## 1. 目标

生成一个可用于部署单台媒体节点的最小生产压缩包。压缩包保留现有 NodeTray 的固定安装目录和安全边界，不引入任意目录便携模式，不运行安装程序，也不携带当前机器的真实配置或凭据。

发布包应满足以下使用流程：

1. 用户将压缩包中的顶层 `MySingerServer` 文件夹解压到 `C:\Program Files\`。
2. 最终托盘程序路径必须为 `C:\Program Files\MySingerServer\nodetray.exe`。
3. 用户启动 `nodetray.exe`，通过图形界面交互式填写、保存并应用 Agent/Helper 配置。
4. Helper 随包提供，但默认不启用、不自动启动；只有用户在界面中显式启用后才参与节点删除辅助流程。

## 2. 非目标

本次不做以下工作：

- 不实现任意目录运行或绿色便携模式。
- 不改变 NodeTray、Agent、Worker、Helper 或 VideoCore 的业务行为。
- 不改变既有固定安装路径、配置目录、日志目录和权限模型。
- 不包含旧版 `mediacore.dll`，也不恢复旧 DLL 兼容路径。
- 不包含 `ffmpeg.exe`、`ffprobe.exe`、`ffplay.exe` 或其他 FFmpeg 命令行程序。
- 不包含中心端 `gui.exe`、源代码、测试代码、构建缓存或测试证据。
- 不包含现有 `bin` 目录中的真实 JSON、PostgreSQL DSN、密码、令牌或其他凭据。
- 不执行 EXE/DLL，不触发 UAC、注册表、计划任务、服务、网络连接或图形界面动态验收。
- 不把本次静态打包结果表述为完整动态验收通过。

## 3. 采用方案

采用“全量 fresh stage 构建 + 节点发布白名单裁剪”的两阶段方案：

1. 使用现有 `scripts/build.ps1` 在全新的临时目录中完成正式构建，复用其 VideoCore、递归 FFmpeg DLL 依赖闭包、WebView2 供应链和发布清单校验。
2. 从已通过构建校验的完整 stage 中，只按明确白名单复制媒体节点所需文件到独立的最小发布目录。
3. 为裁剪后的节点目录重新生成专属 `release-manifest.json`，不得沿用仍包含 `gui.exe` 等全量文件记录的原清单。
4. 对最小目录完成静态校验后再创建 ZIP，并重新解压到新的临时目录核对文件清单和哈希。

不直接压缩当前 `bin`，因为该目录含旧版和开发期产物，不能代表本次 VideoCore/NodeTray 生产发布边界。也不直接压缩仅含 NodeTray 的既有 Task 9 stage，因为它缺少 Agent、Worker、Helper、VideoCore 及其原生运行时闭包，不能构成可运行媒体节点。

## 4. 发布物命名与目录结构

正式发布目录：

```text
artifacts/releases/
├── MySingerServer-node-win-x64-20260803.zip
└── MySingerServer-node-win-x64-20260803.zip.sha256
```

ZIP 内必须只有一个顶层目录：

```text
MySingerServer/
├── nodetray.exe
├── agent.exe
├── worker.exe
├── helper.exe
├── videocore.dll
├── Everything64.dll
├── MicrosoftEdgeWebview2Setup.exe
├── agent.example.json
├── helper.example.json
├── native-dependencies.json
├── release-manifest.json
├── README-节点部署.md
├── licenses/
│   ├── ffmpeg-LICENSE.txt
│   ├── ffmpeg-NOTICE.md
│   └── webview2-NOTICE.md
└── <native-dependencies.json 记录的递归 FFmpeg 运行时 DLL 闭包>
```

FFmpeg DLL 不在设计文档中写死文件名。打包脚本必须以 fresh stage 的 `native-dependencies.json` 为权威来源复制完整递归依赖闭包，以避免 vcpkg/FFmpeg 版本更新后漏包或残留无关 DLL。

## 5. 文件白名单与排除规则

### 5.1 固定白名单

节点包只允许包含：

- 四个节点进程：`nodetray.exe`、`agent.exe`、`worker.exe`、`helper.exe`。
- 当前算法 DLL：`videocore.dll`。
- 文件枚举运行时：`Everything64.dll`。
- NodeTray 首次运行可能需要的官方 WebView2 Bootstrapper：`MicrosoftEdgeWebview2Setup.exe`。打包和校验过程不运行它。
- 经过脱敏的 `agent.example.json` 和 `helper.example.json`。
- `native-dependencies.json` 指定的 FFmpeg 原生 DLL 闭包。
- 本节点包自己的发布清单、中文部署说明和必要第三方许可/声明。

### 5.2 强制排除

发布前必须递归确认以下文件不存在：

- `gui.exe`。
- `mediacore.dll`、`libmediacore.a` 或其他旧版 MediaCore 产物。
- `ffmpeg.exe`、`ffprobe.exe`、`ffplay.exe`。
- `agent.json`、`helper.json`、`tray-settings.json` 或其他真实运行配置。
- `.pdb`、`.lib`、`.a`、`.obj`、`.o`、构建日志、测试报告、缓存目录。
- 源代码、Git 元数据和开发文档全集。

文件复制采用白名单，不采用“复制全部后删除黑名单”。出现未知文件时直接失败，不自动纳入发布包。

## 6. 配置与敏感信息边界

包内只放仓库中的脱敏示例配置：

- `deploy/agent.example.json` → `agent.example.json`
- `deploy/helper.example.json` → `helper.example.json`

打包校验必须确认：

- 示例中不存在真实 PostgreSQL DSN、密码、令牌、私钥或当前机器专属值。
- 不读取或复制 `bin/*.json`、`ProgramData`、`LocalAppData` 或用户目录中的运行配置。
- 中文部署说明明确要求通过 NodeTray 界面交互式修改配置，不指导用户直接编辑生产 JSON。

运行期目录继续使用现有约定：

| 用途 | 固定目录 |
|---|---|
| 程序文件 | `C:\Program Files\MySingerServer\` |
| Agent 配置与日志 | `C:\ProgramData\MySingerServer\Node\` |
| Helper 配置与日志 | `C:\ProgramData\MySingerServer\Helper\` |
| NodeTray 用户设置 | `%LOCALAPPDATA%\MySingerServer\NodeTray\` |

## 7. 中文部署说明内容

包内 `README-节点部署.md` 至少说明：

1. 适用范围和 Windows x64 要求。
2. 将完整 `MySingerServer` 文件夹复制到 `C:\Program Files\`，不可只复制单个 EXE。
3. 启动 `nodetray.exe` 后通过页签交互式配置 Agent；Helper 为可选组件，默认关闭。
4. WebView2 缺失时可由用户手动运行随包 Bootstrapper；程序不会在本次打包过程中代替用户安装。
5. Everything 服务不可用时 Agent 会按现有逻辑回退文件遍历，但 `Everything64.dll` 仍随包提供。
6. 配置、启动、停止、重启、开机启动和 Helper 删除辅助功能的现有界面入口。
7. 日志/配置位置、基础故障排查和完整卸载前需先停止组件的提示。
8. 包哈希验证方法及“当前发布仅完成静态构建/打包校验”的边界。

## 8. 发布清单与哈希

### 8.1 包内清单

裁剪后的 `release-manifest.json` 必须记录：

- 清单 schema 版本。
- 产品、目标平台、构建日期和源码提交标识；当前工作目录没有 Git 元数据时写入 `N/A_NO_GIT_METADATA`。
- 每个发布文件的相对路径、字节大小和 SHA-256。
- `native-dependencies.json` 的相对路径和 SHA-256。
- 固定安装根 `C:\Program Files\MySingerServer\`。
- 明确标记 Helper 为“随包提供、默认不启用”。
- 明确标记这是节点裁剪包而非中心 GUI 发布包。

由于清单无法安全地包含自身哈希，`release-manifest.json` 的文件条目覆盖除自身之外的全部 ZIP 内容；整个 ZIP 的 SHA-256 由同目录的 `.zip.sha256` 侧车文件提供。

### 8.2 ZIP 侧车哈希

`MySingerServer-node-win-x64-20260803.zip.sha256` 使用 UTF-8 无 BOM 文本，格式为：

```text
<64 位小写 SHA-256>  MySingerServer-node-win-x64-20260803.zip
```

## 9. 静态验收标准

只有以下检查全部通过才生成并保留正式 ZIP：

1. **fresh stage**：完整构建输出目录在构建前不存在，且现有构建脚本全部通过。
2. **白名单**：最小目录中的每个文件都能由本设计的固定白名单、许可文件或原生依赖清单解释。
3. **排除项**：强制排除的程序、旧 DLL、真实配置和开发文件均不存在。
4. **原生依赖闭包**：`videocore.dll`/`worker.exe` 需要的非系统 DLL 均已包含，并与 `native-dependencies.json` 一致；不得以 FFmpeg CLI 替代 DLL。
5. **架构**：四个 EXE、`videocore.dll`、FFmpeg DLL 和 `Everything64.dll` 均为预期 Windows x64 产物。
6. **NodeTray 供应链**：WebView2 Bootstrapper 的大小、SHA-256 和 Microsoft Authenticode 继续通过既有校验。
7. **权限清单**：NodeTray 保持显式 `asInvoker`；Agent/Worker 保持 Windows 默认权限；Helper 按既有删除安全边界保持显式 `requireAdministrator`。打包过程不得改变这些权限模型，也不得运行 Helper 或触发 UAC。
8. **签名报告**：如自有二进制未签名，验收报告必须明确写 `NotSigned`，不得误报为已签名。
9. **敏感信息扫描**：示例配置、清单和中文说明不含 DSN、密码、令牌或私钥内容。
10. **清单复核**：按实际文件重新计算大小和 SHA-256，与 `release-manifest.json` 逐项一致。
11. **ZIP 复核**：ZIP 只含一个顶层 `MySingerServer` 目录；解压到新的临时目录后，文件列表和哈希与打包前完全一致。
12. **侧车哈希**：重新计算 ZIP SHA-256，与 `.zip.sha256` 一致。
13. **只读执行边界**：验收期间不启动、停止或重启任何组件，不运行 WebView2 安装程序，不触发 UAC。

如果任何一步失败，脚本应以非零退出，不发布或覆盖正式命名的 ZIP。构建和临时裁剪目录保留用于人工排查，避免自动删除可能有价值的证据。

## 10. 实施产物

实施阶段预计新增：

- 一个可重复执行的节点 ZIP 打包脚本，接受已经验证的 fresh full stage 和输出目录，完成白名单裁剪、清单生成、ZIP 创建与静态复核。
- 对打包脚本的基础静态/合同测试，覆盖白名单、排除项、敏感配置和 ZIP 结构。
- 包内中文 `README-节点部署.md`。
- 本次实际构建与打包验收报告。
- 正式 ZIP 与 `.zip.sha256`。

本项目目录当前没有可用 Git 元数据，因此规格、计划、实现与验收中涉及提交号的位置统一使用 `N/A_NO_GIT_METADATA`，不伪造提交记录。

## 11. 完成判定

本任务的“完成”定义为：已生成上述正式 ZIP 和侧车哈希，所有第 9 节静态验收项通过，并报告准确的文件路径、大小、SHA-256、包含内容和未执行的动态验收范围。

它不代表目标媒体节点上已经安装、启动或连接 PostgreSQL，也不代表真实媒体目录、开机启动、Helper 删除辅助或 24 小时驻留长测已重新验收。

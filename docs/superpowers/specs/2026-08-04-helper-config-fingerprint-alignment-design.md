# Helper 配置指纹统一设计

日期：2026-08-04  
状态：已确认（2026-08-04）  
方案：A — Helper 使用 NodeTray canonical JSON 指纹

## 背景与根因

NodeTray 启动 Helper 后，会通过控制管道读取 Helper 报告的配置 SHA-256，并与自身保存的期望指纹比较。当前两端对同一个 `helper.json` 使用了不同的序列化字节：

- NodeTray 配置 Store 使用 `json.MarshalIndent(value, "", "  ")`，并在末尾追加一个换行；
- Helper 的 `effectiveHelperConfigSHA256` 使用 `json.Marshal`，得到紧凑 JSON 且没有末尾换行。

因此 Helper 可以成功加载配置并启动服务器，但控制握手必然报告 `control handshake config fingerprint does not match`，随后不能被 NodeTray 可信认领。现有 Helper 测试把紧凑 JSON 摘要写成期望值，未约束与 NodeTray 的 canonical 字节合同。

本机只读证据显示，Helper 日志已记录 `server_started`，随后 Helper 进程消失且 NodeTray 仍运行；这与握手拒绝路径一致。

## 目标

1. Helper 与 NodeTray、Agent 对有效配置使用完全相同的 canonical JSON 字节计算 SHA-256。
2. Helper 启动后报告的 `ConfigSHA256` 能通过 NodeTray 既有严格握手校验。
3. 用回归测试固定序列化合同，防止缩进、换行或紧凑编码再次漂移。
4. 生成可独立替换的 Windows x64 `helper.exe`，但不在本任务中直接部署到 `Program Files`。

## 非目标

- 不降低握手校验强度，不同时接受两种摘要。
- 不修改 `helper.json` 的字段、内容或保存位置。
- 不修改 Helper 启停、强制退出、计划任务、UAC 或进程认领流程。
- 不修改 Agent 指纹算法；Agent 已使用目标 canonical 合同。
- 不自动覆盖已安装文件，不启停当前 NodeTray、Agent 或 Helper。

## 设计

### 1. 统一 Helper 指纹算法

修改 `cmd/helper/main.go` 中的 `effectiveHelperConfigSHA256`：

1. 使用 `json.MarshalIndent(cfg, "", "  ")` 序列化已加载并验证的有效 Helper 配置；
2. 在返回字节末尾追加 `\n`；
3. 对完整字节计算 SHA-256，并继续返回小写十六进制字符串。

该实现与 `internal/nodetray/config/store.go` 的 `canonicalJSON` 以及 Agent 的 `effectiveConfigSHA256` 保持相同字节合同。

### 2. 回归测试

先修改或新增测试并确认 RED：

- 新增独立合同测试，使用非空、具有代表性的 Helper 配置，同时计算 NodeTray canonical 字节摘要，并断言 `effectiveHelperConfigSHA256` 与其相等；在生产代码修改前，该测试应因紧凑 JSON 与缩进 JSON 不同而失败。
- 更新 `runWith` 控制身份测试的期望摘要，使其由缩进 JSON加末尾换行计算，不再固定旧紧凑 JSON 字符串。
- 测试必须校验真实函数结果或控制状态，不仅断言序列化函数被调用。

实现最小生产改动后确认 GREEN，并运行 Helper、NodeTray 与相关控制握手测试。

### 3. 构建与产物

使用仓库固定 Go 工具链构建 Windows x64 Helper。产物写入新的独立目录，避免覆盖既有构建：

```text
artifacts/helper-config-fingerprint-fix/helper.exe
```

记录绝对路径、文件大小、修改时间和 SHA-256。构建阶段不复制到 `C:\Program Files\MySingerServer\helper.exe`。

## 数据流

```text
helper.json
  -> Helper 严格加载与验证
  -> MarshalIndent(两空格) + 换行
  -> SHA-256
  -> 控制状态 ConfigSHA256
  -> NodeTray 与 Store canonical SHA-256 严格比较
  -> 指纹相同后可信认领 Helper
```

## 错误处理

- JSON 编码失败仍由 `effectiveHelperConfigSHA256` 返回错误，Helper 启动失败并保留现有错误包装。
- 握手仍执行精确 SHA-256 相等比较；不增加回退、兼容摘要或忽略错误路径。
- 构建或测试失败时停止交付，不生成“已修复”结论。

## 验收标准

静态验收：

- 新合同测试在修复前按预期失败，修复后通过。
- `go test -count=1 ./cmd/helper ./internal/nodetray/... ./nodetray` 通过；若环境阻塞，必须单独记录，不能冒充通过。
- Windows x64 `helper.exe` 构建成功并记录 SHA-256。

动态验收：

- 本任务默认不部署、不启停，因此真实安装后点击“启动 Helper”仍记录为 `BLOCKED_NOT_RUN_DYNAMIC`。
- 后续只有在用户明确授权替换已安装 Helper 并启动验证后，才能确认本机动态问题已解决。

## 版本与交付边界

当前 checkout 没有 Git 元数据，设计文档和后续改动记录为 `N/A_NO_GIT_METADATA`；不初始化 Git，不伪造提交或分支状态。

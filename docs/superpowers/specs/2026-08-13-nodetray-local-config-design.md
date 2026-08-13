# NodeTray 本地配置读写设计

## 目标

NodeTray 直接管理便携包内 Agent、Helper 和自身配置文件。读取或保存配置不依赖 Agent/Helper Socket，也不请求管理员权限。只有启动 Helper 时才请求 UAC。

## 配置路径与发布内容

Compute ZIP 解压后必须直接包含以下可编辑配置：

- `data/agent/agent.json`
- `data/helper/helper.json`
- `data/nodetray/tray.json`

打包脚本分别把 `deploy/agent.default.json`、`deploy/helper.default.json`、`deploy/nodetray.default.json` 复制为上述实际配置文件。运行前不再需要用户手工创建或复制配置。

Helper 默认配置允许 `allowed_roots` 为空，以便 NodeTray 正常启动并打开配置界面；启动 Helper 前必须按现有 Helper 校验规则检查配置。未配置有效目录时拒绝启动 Helper，并返回稳定、可展示的配置错误。

## NodeTray 配置职责

### Agent

NodeTray 的 Agent 配置界面直接调用本地配置 Store：

1. 严格读取 `data/agent/agent.json`；
2. 使用现有 Agent 表单转换和共享校验规则；
3. 使用锁、同目录临时文件、原子替换和回读校验保存；
4. 覆盖前维护一份 `.last-good`；
5. 保存后更新 Supervisor 预期配置摘要；若 Agent 已运行，则展示“需要重启”。

NodeTray 不再通过 Agent Socket 执行 `local.config.get`、`local.config.validate` 或 `local.config.save`。Agent Socket 继续承担状态、任务、Worker 信息和生命周期控制。

### Helper

NodeTray 的 Helper 配置界面直接调用同一个本地配置 Store：

1. 严格读取 `data/helper/helper.json`；
2. 使用现有 Helper 表单转换和共享校验规则；
3. 使用与 Agent 相同的本地原子保存和 `.last-good` 语义；
4. 保存配置不调用提权客户端、不弹 UAC；
5. 保存后更新 Supervisor 预期配置摘要；若 Helper 已运行，则展示“需要重启”。

旧的提权写 Helper 配置动作暂时保留兼容，但 NodeTray 正常保存路径不再调用它。本次不做无关协议删除。

## Helper 启动与 UAC

- 手动启动 Helper：复用现有管理员启动器，启动时请求 UAC。
- 自动启动 Helper：NodeTray 启动后同样通过管理员启动入口请求 UAC，不再依赖“保存设置时安装提权计划任务”。
- 保存 NodeTray 设置不请求 UAC。
- 启用 Helper 或切换自动启动方式时只保存设置；真正启动时才提权。
- 启动前重新读取并验证 `data/helper/helper.json`，防止无效配置进入管理员进程。

Helper 可执行文件和配置均位于用户选择的便携目录。用户确认接受普通用户可编辑配置、启动时通过 UAC授权该配置的运行方式。

## 错误处理

- 配置不存在、JSON 非严格格式、校验失败或回读不一致：不启动对应组件，界面显示稳定错误摘要。
- 保存失败：保留原正式配置；不得产生半写文件。
- `.last-good` 只保存最近一次被替换的有效配置。
- 保存不成功时不得更新 Supervisor 预期摘要。
- Helper UAC 取消只影响本次启动，不回滚已经保存的普通配置或 NodeTray 设置。

## 兼容与范围

- 不改变 Agent 与 GUI、NodeTray 与 Agent 的任务/状态 Socket 协议。
- 不改变 Helper 的运行控制协议。
- 不删除旧 Socket 配置操作或旧提权写配置动作，只停止 NodeTray 生产接线调用。
- 不修改 PostgreSQL、扫描、去重或 Worker 计算行为。

## 验收

采用最小聚焦验证：

1. Agent 配置在 Agent 未运行时仍能由 NodeTray 读取、校验和保存。
2. Helper 配置在 Helper 未运行时仍能由 NodeTray 读取、校验和保存。
3. 保存 Helper 配置不调用 UAC/提权客户端。
4. 启动 Helper 才调用管理员启动入口；无效配置不会触发启动。
5. 保存后摘要与需要重启状态正确。
6. Compute ZIP 精确包含三份实际配置，不再包含仅供手工复制的 Helper 模板。

只运行受影响的 NodeTray 配置、应用服务、Windows 接线和 Compute 打包合同测试，不执行无关全仓回归。

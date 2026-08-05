# 节点硬件机器唯一 ID 与 GUI 动态发现设计

日期：2026-08-04  
状态：已确认（2026-08-04）  
方案：A — Windows 硬件信息规范化后计算完整 SHA-256

## 背景

当前 Agent 从 `agent.json` 读取人工配置的 `machine_id`，GUI 的每个 Agent endpoint 也同时配置 `machine_id` 与 `addr`。GUI 建立连接后会比较两端的值，不一致时拒绝连接，因此出现了 `machine_id mismatch: config=machine-a agent=1`。

机器身份不应由 GUI 和 Agent 分别维护。本设计改为由 Agent 根据本机 CPU ID、主板 ID 和 Windows 系统 ID 生成机器唯一 ID；GUI 只配置 Agent 地址，并在 Hello 握手成功后采用 Agent 上报的机器 ID。

本设计替代 `2026-08-04-gui-web-config-editor-design.md` 中关于 GUI Agent endpoint 必须编辑、校验 `machine_id` 的部分，其余 GUI 配置编辑合同保持不变。

## 目标

1. Agent、Helper 和 NodeTray 使用同一生成器得到稳定的机器唯一 ID，不再依赖专属 `machine_id` 配置或主机名。
2. 使用 CPU `ProcessorId`、主板 `SerialNumber` 和 Windows `MachineGuid` 共同计算身份。
3. 单项信息不可用时允许 Agent 使用其余有效信息启动；三项全部不可用时明确失败。
4. GUI endpoint 只保存连接地址，并在 Hello 后使用 Agent 上报的机器 ID。
5. 同一机器 ID 同时被多个连接上报时只允许一个连接参与调度。
6. 兼容现有包含 `machine_id` 的 Agent 和 GUI 配置，避免升级后因严格 JSON 解码而无法启动。

## 非目标

- 不允许用户在 Web 页面或配置文件中覆盖自动生成的机器 ID。
- 不把机器 ID 当作设备认证凭据，不增加登录、证书、TLS 或远程证明。
- 不为硬件更换后的旧身份自动迁移数据库历史数据。
- 不持久化 GUI 的“地址到机器 ID”缓存；GUI 每次启动后重新握手发现。
- 不增加复杂的硬件指纹审查或人工审批流程。

## 已确认的产品选择

- 系统 ID 使用 `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`。
- 某项 ID 读取失败或为无效占位值时，使用其余有效项继续计算并记录警告。
- 三项全部无效时阻止 Agent 启动。
- 使用完整 SHA-256，输出格式为 `node-` 加 64 位小写十六进制摘要。
- 重复机器 ID 采用“首个有效在线连接占用”规则；占用者离线后允许其他连接认领。

## 领域模型

### 机器唯一 ID

机器唯一 ID 是节点组件在启动时根据本机硬件与系统信息自动计算的稳定节点身份。Agent、Helper 和 NodeTray 使用同一个生成合同；Agent 通过 Hello 向 GUI 上报，GUI 不能配置或覆盖。

机器唯一 ID 用于：

- Agent 单实例范围；
- Worker 运行时配置；
- 扫描、同步和任务消息中的节点归属；
- GUI 在线节点索引和任务调度目标；
- 控制状态与日志中的节点标识。

### Agent endpoint

Agent endpoint 是 GUI 配置中的连接目标，只包含 `addr`。它回答“到哪里连接”，不再回答“连接对象是谁”。连接对象身份必须由成功握手后的 Hello 确定。

### 身份占用

GUI 中一个在线连接成功将其 Hello 机器 ID 注册到动态索引后，即占用该机器 ID。身份占用随连接生命周期存在，不写入配置文件。

## 机器 ID 生成合同

### 数据来源

Windows 实现读取：

| 字段 | 来源 | 原始属性 |
|---|---|---|
| CPU ID | WMI/CIM | `Win32_Processor.ProcessorId` |
| 主板 ID | WMI/CIM | `Win32_BaseBoard.SerialNumber` |
| 系统 ID | Windows 注册表 | `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid` |

读取接口必须可注入，以便单元测试直接提供固定输入和错误，不依赖测试机器的真实硬件。

### 规范化

每个原始值按相同顺序处理：

1. 去除首尾 Unicode 空白和 `NUL` 字符；
2. 转为大写；
3. 若为空或等于已知固件占位值，则视为不可用。

WMI 对 CPU 或主板返回多条记录时，逐条执行上述规范化和过滤，再去重、按字典序排序并使用 `|` 连接。摘要不得依赖 WMI 的枚举顺序；至少保留一条有效记录时，该字段即视为可用。

至少过滤以下不区分大小写的占位值：

- `UNKNOWN`
- `NONE`
- `DEFAULT STRING`
- `TO BE FILLED BY O.E.M.`
- `NOT SPECIFIED`
- 全部由 `0`、连字符或空白组成的值

不删除有效值内部的空格、连字符或其他字符，避免把不同厂商原始值错误合并。

### 摘要输入与输出

摘要输入使用带版本和固定字段名的 UTF-8 文本：

```text
mysingerserver-machine-id:v1
cpu=<规范化值或空值>
board=<规范化值或空值>
system=<规范化值或空值>
```

字段顺序、LF 换行和最后一行后的 LF 是合同的一部分。缺失字段保留为空行，从而使相同有效输入始终得到相同结果，并避免字段位置歧义。

对完整文本计算 SHA-256，输出：

```text
node-<64 位小写十六进制 SHA-256>
```

生成器返回机器 ID、每项是否可用以及读取警告。日志只记录字段是否可用和最终机器 ID，不记录三个原始值。

### 失败与稳定性

- 任意单项读取失败：记录该来源警告，继续使用其他项。
- 任意单项为占位值：按不可用处理并记录警告。
- 三项全部不可用：返回明确错误，Agent 在初始化其他组件前退出。
- CPU、主板或 Windows 安装身份发生变化时，生成的机器 ID会变化；本次不自动迁移旧身份数据。

## Agent 启动数据流

启动顺序调整为：

```text
加载 agent.json
  -> 读取并规范化本机身份来源
  -> 计算机器唯一 ID
  -> 写入仅存在于内存的运行时 MachineID
  -> 创建 Agent 单实例锁
  -> 初始化控制接口、Worker、数据库与同步组件
  -> 启动 Agent Server
  -> Hello 上报机器唯一 ID
```

机器 ID 必须在现有所有 `cfg.MachineID` 消费者初始化前生成。下游组件继续接收同一个运行时值，避免各组件分别计算导致规则漂移。

## Agent 配置迁移

`machine_id` 不再是有效的用户配置项。为兼容现有部署：

- `LoadAgent` 暂时接受旧 JSON 中的 `machine_id`；
- 读取到的旧值被忽略，不能覆盖自动生成值；
- 缺少 `machine_id` 的新配置合法；
- 其他未知字段仍按当前严格 JSON 规则拒绝；
- 配置模板、部署文档和示例移除 `machine_id`。

运行时机器 ID 与磁盘配置分离。旧字段只用于单向兼容解码，不得出现在新配置编码结果、Web 合同或业务读取路径中。

## Helper 与 NodeTray 统一身份

Helper 不再使用 `os.Hostname()` 作为控制接口身份，启动时调用同一机器身份生成器，并在 Helper 控制状态中上报相同的 `node-<sha256>`。

NodeTray 在生产组合初始化时只计算一次机器唯一 ID，并将同一结果用于：

- 概览页只读展示；
- Agent 控制器的预期机器 ID；
- Helper 控制器的预期机器 ID；
- Agent 和 Helper 的启动、停止、重启、状态查询与受管进程认领。

NodeTray 的 Agent 配置表单移除可编辑的机器 ID。保存 Agent 配置时不再更新控制器的预期身份，配置文件也不再写入 `machine_id`。身份读取失败时，NodeTray 生产组合明确失败，不以旧配置值或主机名回退。

Agent、Helper 和 NodeTray 的日志对缺失来源采用同一警告语义，只记录来源是否可用，不记录原始 CPU ID、主板序列号或 MachineGuid。

## GUI 配置与 Web 页面迁移

GUI endpoint 的新配置格式：

```json
{
  "agents": [
    { "addr": "192.168.1.101:9101" },
    { "addr": "192.168.1.102:9101" }
  ]
}
```

迁移规则：

- `LoadGUI` 暂时接受旧 endpoint 中的 `machine_id`，但忽略其值；
- `ValidateGUI` 不再要求或校验 endpoint `machine_id`；
- Agent 地址必须非空、合法且在列表内唯一；
- Web 配置 API 返回和接受的新 endpoint 只包含 `addr`；
- 旧配置经 Web 页面保存后，以规范 JSON 写回并移除旧 `machine_id`；
- GUI 设置页的 Agent 行只编辑地址，不再显示机器 ID 输入框。

## GUI 动态连接与身份索引

Pool 分为两个索引：

- endpoint 索引：按规范化 `addr` 保存待连接或已连接的 `AgentConn`；
- 身份索引：Hello 成功后按 Agent 上报的 `machine_id` 保存当前占用连接。

连接状态变化：

```text
按 addr 创建连接
  -> 连接中 / 未识别
  -> 收到合法 Hello
  -> 尝试占用 Hello.machine_id
     -> 成功：在线，可调度
     -> 已被其他在线连接占用：身份冲突，不可调度
  -> 连接断开
  -> 释放由该连接持有的身份占用
```

原有 `config machine_id != agent machine_id` 校验全部删除。Hello 中的机器 ID 仍必须非空且符合 `node-` 加 64 位小写十六进制格式；非法 Hello 不能注册身份或参与调度。

`Send(machineID)`、`IsOnline(machineID)` 和任务恢复逻辑统一查询身份索引。消息分发、状态回调、日志和任务归属使用 Hello 中的机器 ID，不使用 endpoint 地址代替身份。

## 重复身份规则

1. 第一个完成合法 Hello 并成功注册的在线连接占用机器 ID。
2. 后续上报相同 ID 的连接进入 `identity_conflict` 状态，不加入身份索引，也不参与任务调度。
3. 冲突连接保持 endpoint 连接循环，可显示错误，但不能替换仍在线的占用者。
4. 占用连接断开时，仅当身份索引仍指向该连接才释放，避免旧连接误删新占用。
5. 释放后，冲突连接在下一次握手或重连时可以重新认领；不增加复杂的主动抢占协调。

## Web 状态展示

Agent 状态页继续展示运行时状态，并增加清晰的身份阶段：

- Hello 前：机器 ID 显示“待识别”，同时显示配置地址；
- Hello 成功：显示 Agent 提供的机器唯一 ID；
- 身份冲突：显示该 ID、地址和“身份冲突”，不显示为在线可用；
- 断线：保留该 endpoint 最近一次已识别 ID 仅供当前进程内展示，但不视为身份占用。

配置页面与状态页面职责保持分离：配置页面编辑地址，状态页面展示 Agent 实际身份。

## 错误处理

- Agent 三项身份全部不可用：启动失败并返回 `machine identity unavailable` 类明确错误。
- WMI 单项查询失败或注册表项不存在：警告后按缺失项继续。
- GUI 收到空白或格式错误的机器 ID：该连接握手失败，不加入身份索引。
- 地址重复：配置保存或启动加载失败，并返回对应 `agents[i].addr` 字段错误。
- 身份重复：不修改磁盘配置，运行时标记冲突。
- 不向用户暴露 CPU ID、主板序列号或 MachineGuid 原文。

## 测试设计

### 身份生成单元测试

- 三项均有效时得到固定完整 SHA-256；
- 大小写、首尾空白和 `NUL` 规范化后结果一致；
- 每个已知占位值被过滤；
- 任意一项或两项缺失时仍生成稳定 ID并返回警告；
- 三项全部不可用时返回错误；
- 字段顺序、版本前缀和空字段均固定在摘要合同中。

### Agent 配置与启动测试

- 新配置不含 `machine_id` 时可加载；
- 旧配置含 `machine_id` 时可加载但旧值不进入运行时；
- 自动 ID 在单实例、Worker、控制接口和 Hello 初始化前注入；
- 身份生成失败时不启动下游组件。

### Helper 与 NodeTray 测试

- Helper 控制状态使用共享生成器返回的机器 ID，不再使用主机名；
- NodeTray 概览展示注入的机器 ID，Agent 和 Helper 控制器校验同一值；
- NodeTray Agent 表单、保存结果和生成的 `agent.json` 不包含 `machine_id`；
- 保存 Agent 配置不再调用运行时身份更新器；
- 身份生成失败时 NodeTray 生产组合不创建可用后端。

### GUI 配置测试

- endpoint 只含 `addr` 时可加载和保存；
- 旧 endpoint 的 `machine_id` 可读取但保存后被移除；
- 空地址、无效地址和重复地址被拒绝；
- Web API 合同不再要求或返回可编辑的 endpoint `machine_id`。

### Pool 测试

- Hello ID 成为状态、消息分发和 `Send` 的唯一身份；
- 配置地址与 Hello ID 不需要匹配；
- Hello 前 endpoint 不可调度；
- 非法 Hello ID 被拒绝；
- 重复身份首连接占用、后连接冲突；
- 占用者断线只释放自己的映射，其他连接随后可认领；
- 旧连接断开不会删除新连接已经取得的映射。

### React 测试

- GUI 设置页 Agent 行只编辑地址；
- 新增、删除、排序和保存地址列表正常；
- 状态页正确展示“待识别”、实际机器 ID 和“身份冲突”；
- 调度操作只对已识别且在线的机器 ID 可用。

## 验收标准

1. 删除 `agent.json` 的 `machine_id` 后 Agent 能启动并生成 `node-<sha256>` 身份。
2. 保留旧 `machine_id` 时 Agent 也能启动，且 Hello 上报值与旧配置无关。
3. GUI 只配置 `addr` 即可连接 Agent，状态页显示 Agent 上报的机器 ID。
4. 不再出现因 GUI 配置 ID 与 Agent ID 不同导致的 `machine_id mismatch`。
5. 两个 endpoint 上报同一 ID 时只有首个连接可调度；首个断线后另一个可重新认领。
6. Web 保存 GUI 配置后，endpoint 中不再写入 `machine_id`。
7. 受影响 Go 测试、Web 测试、lint、构建和嵌入资源校验通过。
8. Agent、Helper 和 NodeTray 控制状态使用同一个机器唯一 ID，NodeTray 不再提供机器 ID 编辑框。

## 实施约束

- 使用测试驱动方式先固定身份生成、配置兼容和动态索引合同。
- 这是个人项目，只进行与本变更直接相关的测试和一次最终集中验证，不增加额外安全审查或多层审批。
- 不启动、停止或重启当前真实 Agent/GUI；真实多机连接和硬件读取由用户在生成新二进制后手动验收。
- 当前 checkout 无 Git 元数据时，不初始化 Git，版本状态记录为 `N/A_NO_GIT_METADATA`。

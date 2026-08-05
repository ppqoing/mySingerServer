# GUI Web 完整配置编辑与多 Agent 管理设计

日期：2026-08-04  
状态：已确认（2026-08-04）  
方案：A — 类型化配置服务与结构化表单

## 背景

中央 GUI 已通过 `gui.json` 的 `agents` 数组支持连接多个 Agent，但当前 Web 的 Agent 页面只展示运行时连接状态，不能添加、编辑或删除节点，也不能维护 GUI 的其他配置。修改配置只能手工编辑 JSON 并重启 `gui.exe`。

本设计在现有 V4 React 工作台中增加独立“GUI 设置”工作区，通过类型化 HTTP API 读取、校验并保存启动时 `-config` 指定的完整配置文件。保存只更新磁盘配置；当前 HTTP 监听、PostgreSQL 连接和 Agent Pool 不热更新，用户手动重启 GUI 后生效。

## 目标

1. 在 Web 页面使用结构化表单编辑完整 GUI 配置。
2. 在 Agent 列表中添加、编辑和删除多台 Agent 的 `machine_id` 与 `addr`。
3. 后端复用 GUI 启动时的配置结构和校验规则，拒绝无法在重启后加载的配置。
4. 以 UTF-8 无 BOM 和 Windows 原子替换方式写回实际 `-config` 文件。
5. 保存成功后明确提示是否需要重启，不改变当前运行时连接池。

## 非目标

- 不在保存后热更新 Agent Pool、PostgreSQL 连接、HTTP 监听地址或分析参数。
- 不自动重启 `gui.exe`，不增加外部进程托管器。
- 不增加登录、鉴权、TLS、配置版本历史或多用户编辑协议。
- 不修改 Agent、Worker、Helper 或 NodeTray 的配置和生命周期。
- 不把尚未重启的新 Agent 配置显示为已经连接或已生效。

## 已确认的产品选择

- 编辑范围：完整 `gui.json`。
- 页面形式：结构化表单，不提供原始 JSON 编辑器。
- 页面位置：新增独立“GUI 设置”工作区。
- `pg_dsn`：单个密码输入框，默认隐藏，允许显示或隐藏。
- 生效方式：保存后手动重启 GUI；不热更新，不自动重启。

## 总体架构

新增 `GUIConfigService`，由 `cmd/gui` 在启动时注入：

- `-config` 解析得到的配置文件绝对路径；
- 启动时实际使用的 `GUIConfig`；
- 配置加载、字段校验、规范 JSON 编码和原子写入能力；
- 进程内保存互斥锁。

服务通过 `internal/gui` 暴露类型化配置接口，React 页面通过现有 `AppApi` 调用。数据流为：

```text
GUI 设置结构化表单
  -> PUT /api/config
  -> 解码完整 GUIConfig
  -> 共享 ValidateGUI
  -> 规范 JSON 编码（UTF-8 无 BOM）
  -> 同目录临时文件写入、关闭、重新读取验证
  -> Windows 原子替换 gui.json
  -> 与启动时运行配置比较
  -> 返回 saved / restart_required
  -> 页面显示保存结果和重启提示
```

当前 `http.Server`、PostgreSQL Pool、Agent Pool、分析服务和删除服务继续使用启动时配置，直到进程退出并重新启动。

## 配置模型与共享校验

现有 `internal/config.GUIConfig` 继续作为唯一配置模型。将 `LoadGUI` 内的语义校验抽成可复用的 `ValidateGUI`；文件启动加载和 Web 保存都调用同一入口，避免两套规则漂移。

校验范围：

- `listen_addr` 是有效的 `host/IP:port`，端口在有效范围内；
- `pg_dsn` 非空且可解析；
- `heartbeat_s` 为正数；
- `agents` 至少一项；
- 每个 Agent 的 `machine_id`、`addr` 必填；
- Agent `addr` 是有效的 `host/IP:port`；
- `machine_id` 在整个 Agent 列表中唯一；
- `firstscreen.*` 和 `phase2.*` 沿用当前边界，包括六帧合同、阈值范围、超时和最大任务分片数。

验证错误转换为稳定字段路径：

```json
{
  "error": "config_invalid",
  "fields": [
    {
      "field": "agents[1].machine_id",
      "code": "duplicate",
      "message": "机器标识不能重复"
    }
  ]
}
```

字段路径用于前端将错误放到对应控件，不依赖后端英文错误文本。`machine_id` 修改在重启后按旧节点移除、新节点加入处理；本功能不迁移历史任务或数据库身份。

## HTTP API

### `GET /api/config`

从当前 `-config` 文件重新加载并校验磁盘配置，成功返回：

```json
{
  "config": {
    "listen_addr": "127.0.0.1:18080",
    "pg_dsn": "postgres://dedup@127.0.0.1:5432/dedup?sslmode=prefer",
    "heartbeat_s": 15,
    "agents": [
      {
        "machine_id": "media-pc-1",
        "addr": "192.168.1.101:9101"
      }
    ],
    "firstscreen": {
      "hamming_max": 31,
      "aspect_tolerance": 0.1,
      "video_duration_window_ms": 2000,
      "image_quality_min": 50,
      "read_page_size": 50000,
      "group_insert_batch": 1000,
      "sha_resolve_chunk": 10000
    },
    "phase2": {
      "phash_pass_t2": 0.8,
      "phash_part_threshold": 10,
      "sobel_t3": 0.85,
      "video_frames": 6,
      "video_avg_t4": 0.8,
      "video_min_passed": 4,
      "video_min_valid": 4,
      "video_file_timeout_s": 120,
      "video_frame_command_timeout_s": 20,
      "image_file_timeout_s": 30,
      "task_shard_size": 5000,
      "auto_dispatch": true
    }
  },
  "restart_required": false
}
```

`restart_required` 通过磁盘配置的规范 JSON 与启动时运行配置的规范 JSON比较得出。配置文件缺失、不可读或已被外部修改为无效 JSON 时，返回稳定错误码，不泄露 `pg_dsn`。

成功响应会向当前 Web 客户端返回完整 `pg_dsn`，以便结构化表单继续编辑；密码输入框的隐藏只属于界面显示，不构成访问控制。本功能不改变现有“仅用于可信本机或可信局域网”的部署边界。

### `PUT /api/config`

接收完整配置对象。请求体限制为一个 JSON 文档，拒绝未知字段和尾随内容。验证失败返回 HTTP 400 和字段错误；保存冲突或写入失败返回稳定错误码。成功返回：

```json
{
  "saved": true,
  "restart_required": true
}
```

提交内容与当前磁盘规范配置相同时可返回 `saved=false`。只要磁盘配置与运行配置不同，`restart_required` 就保持 `true`。

## 保存语义

1. 使用进程内互斥锁串行化 `PUT /api/config`。
2. 完整解码并调用 `ValidateGUI`，校验通过前不创建正式文件。
3. 规范编码使用缩进 JSON、结尾换行和 UTF-8 无 BOM。
4. 临时文件必须与目标文件位于同一目录，文件名带随机后缀。
5. 写入后关闭并同步临时文件，再从临时文件重新加载和校验。
6. 使用 Windows `MoveFileEx` 的 `REPLACE_EXISTING | WRITE_THROUGH` 语义发布正式文件。
7. 原子替换失败时删除本次临时文件并保留原目标文件。

不增加配置历史或备份管理 UI。日志只记录配置保存结果、目标文件名、Agent 数量和是否需要重启，不记录请求体或 `pg_dsn`。

## Web 页面设计

导航新增“GUI 设置”。页面包含：

### 基本设置

- `listen_addr` 文本框；
- `heartbeat_s` 数字框。

### PostgreSQL

- `pg_dsn` 单行密码框；
- 默认隐藏；
- 显示/隐藏按钮只改变输入类型，不改变值。

### Agent 列表

- 每行包含 `machine_id`、`addr` 和删除按钮；
- 提供“添加 Agent”；
- 至少保留一行，或在保存时明确显示“至少配置一个 Agent”；
- 支持调整行顺序；排序只影响保存文件的顺序，不影响身份语义；
- 重复标识、空值和无效地址错误显示在具体行。

### 分析参数

- `firstscreen.*` 和 `phase2.*` 使用数字输入；
- `phase2.auto_dispatch` 使用开关；
- 固定六帧值仍展示但受现有合同约束。

### 页面状态

- 首次进入时加载磁盘配置；
- 修改后显示“有未保存更改”；
- “重新加载”放弃页面内修改并重新请求磁盘配置；
- 保存期间禁用重复提交；
- 保存失败保留用户输入并定位字段错误；
- 保存成功后显示固定提示“配置已保存，请手动重启 GUI 后生效”；
- 如果保存内容与运行配置一致，提示“配置已保存，当前无需重启”。

Agent 状态页继续只展示当前 `Pool.Status()`，不会混入磁盘中等待重启的新 Agent。

## 错误处理

- 请求不是合法 JSON、包含未知字段或尾随数据：HTTP 400。
- 字段校验失败：HTTP 400，返回稳定字段错误数组。
- 磁盘配置读取失败：HTTP 500，页面显示可重试错误。
- 临时文件写入、同步、重新验证或原子替换失败：HTTP 500，原配置保持不变。
- 并发保存由互斥锁串行处理；不增加版本号或乐观锁。
- 客户端中止请求不能把只完成一半的文件发布为正式配置。

## 测试设计

### Go 配置测试

- `LoadGUI` 与 `ValidateGUI` 接受相同合法配置；
- 重复 `machine_id`、无效监听地址、无效 Agent 地址、无效 DSN 和越界分析参数被拒绝；
- 字段错误路径稳定。

### 配置存储测试

- 保存使用传入的非默认 `-config` 路径；
- 输出为 UTF-8 无 BOM、合法完整 JSON；
- 临时文件校验或原子替换失败时原文件字节不变；
- 并发保存不会生成混合或截断文件；
- 相同配置与不同运行配置的 `restart_required` 结果正确；
- 保存结束后无临时文件残留。

### HTTP API 测试

- GET 返回完整配置和重启状态；
- PUT 成功、无变化、字段错误、未知字段、尾随 JSON、读写失败状态码与响应合同正确；
- 错误和日志中不包含 DSN。

### React 测试

- 独立导航入口和全部配置区渲染；
- 加载、错误、重新加载和未保存状态；
- Agent 行添加、编辑、删除、调整顺序；
- DSN 默认隐藏和显示切换；
- 字段错误绑定到具体控件；
- 保存成功后的重启提示；
- 保存后 Agent 状态页仍展示运行时状态。

### 回归门禁

- `npm test`、`npm run lint`、`npm run build`；
- `go test -count=1 ./internal/config ./internal/gui ./cmd/gui`；
- `scripts/build-web.ps1 -VerifyEmbedded`；
- 不用 `go test ./...` 掩盖与本功能无关的 Helper、原生工具或环境门禁，额外失败按实际状态单独报告。

## 验收标准

1. 用户可以在 Web 的“GUI 设置”工作区编辑并保存完整配置。
2. 用户可以添加、编辑、删除和排序多台 Agent。
3. 无效配置不会写入正式文件，错误定位到具体表单字段。
4. 保存文件为 UTF-8 无 BOM，GUI 重启后可直接加载。
5. 保存后不改变当前监听、数据库和 Agent Pool；页面明确提示手动重启。
6. 重启后 GUI 使用新 Agent 列表连接多台机器。
7. 当前 Agent 状态页不会把待重启配置伪装为运行状态。
8. 指定测试和内嵌 Web 构建验证通过；未执行的真实多机连接验收明确标记为未运行。

## 交付边界

当前 checkout 没有 Git 元数据，版本状态记为 `N/A_NO_GIT_METADATA`；不初始化 Git，不伪造提交或分支。实现与静态测试不自动重启当前 GUI，不连接或修改真实 Agent；真实多机器连通性在用户使用实际 IP、重启 GUI 后单独验收。

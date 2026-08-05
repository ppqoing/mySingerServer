# V4 百万级媒体去重 Web 重设计

日期：2026-07-31  
状态：已确认视觉方向，待实施计划  
范围：重设计 `mySingerServer` 现有 Go GUI 内嵌 Web 页面

## 1. 目标

将当前 `internal/gui/web/index.html` 的原生单页替换为 V4“流光智能助理”风格的 React 工作台，同时保留现有 Go 服务、HTTP API 和单二进制内嵌部署方式。

页面必须支持以下用户操作：

- 查看 Agent 在线状态与最近错误；
- 创建扫描任务并查看任务进度；
- 触发一筛分析并查看运行状态；
- 浏览精确重复、相似图片、相似视频三类重复组；
- 查看组内成员、代表文件与分项评分；
- 明确勾选待删除文件；
- 通过二次确认选择软删或硬删；
- 查看删除任务结果与失败原因；
- 在百万级文件、十万级重复组条件下保持可用的浏览和选择体验。

本次重构 Web 前端及其构建集成，并对 `/api/groups` 增加向后兼容的
筛选、排序、汇总字段和成员分页参数。不得修改媒体算法、Agent TCP
协议、数据库表结构或删除 Helper。

## 2. 已选方案

采用独立 React 源码工程 + Go 内嵌静态构建产物：

```text
webui/                         React + TypeScript + Vite 源码
  src/
  public/
    legacy.html
  tests/
  vite.config.ts
  package.json
       │
       │ npm run build
       ▼
internal/gui/web/              Vite 构建产物，继续由 go:embed web 嵌入
  index.html
  assets/*
```

选择该方案的原因：

- React 源码与 Go 业务逻辑边界清晰；
- 不改变 `internal/gui/web.go` 和 `http.FileServerFS` 的部署模型；
- 前端可独立运行、测试和构建；
- 生产仍是一个 Go 二进制，不依赖 CDN 或外部静态服务器；
- 选中的 V4 页面成为唯一生产界面，早期五版视觉探索不进入生产构建。

页面使用 React `HashRouter`，避免 Go `FileServerFS` 为路径路由增加 SPA fallback。

由于当前工作区没有 Git 元数据，首次替换前将原页面保存为
`webui/public/legacy.html`，构建后可通过 `/legacy.html` 回退查看。

## 3. 信息架构

顶部导航固定提供六个工作区：

1. 总览
2. Agent
3. 扫描任务
4. 一筛分析
5. 重复组
6. 删除审计

`/` 默认进入“总览”工作区，兼容入口 `/groups` 默认进入“重复组”工作区。
重复组内的路径筛选直接走服务端查询；其他实体沿用各自工作区的精确筛选，
不在前端伪造跨接口全局搜索结果。

### 3.1 重复组工作区

采用三栏高密度布局：

- 左栏：保存视图、重复类型、机器范围、空间区间、时间范围和状态筛选；
- 中栏：虚拟化重复组表格、服务端分页、排序、批量选择入口；
- 右栏：当前组详情、代表文件、成员评分、成员勾选和删除入口。

页面保留 V4 的渐变、半透明玻璃表面与柔和圆角，但主数据区域使用接近实体白色的背景，保证百万级审阅时的可读性。

### 3.2 其他工作区

- 总览：关键数量、当前任务、Agent 健康状态、可释放空间；
- Agent：在线状态、地址、最近错误、当前吞吐；
- 扫描任务：新建扫描表单、运行任务、完成任务与失败信息；
- 一筛分析：运行按钮、六阶段统计、耗时、最近错误；
- 删除审计：删除批次、成功/失败数量、错误码与文件级结果。

## 4. 百万级布局与数据策略

“支持百万级”不等于把全部文件放入浏览器。前端遵守以下限制：

- 重复组列表每次仅从服务端请求 100 条；
- 使用行虚拟化，DOM 仅保留可视区域及少量 overscan 行；
- 使用服务端筛选和排序，禁止先拉全量再在浏览器过滤；
- `/api/groups` 使用 `page + size` 并返回 `total`，前端固定每页 100 组；
- 列表接口新增可选 `q`、`machine`、`min_members` 和 `sort` 参数；
- 列表摘要新增 `total_bytes` 与 `wasted_bytes`；
- API 未来支持游标时只替换适配器内部实现，不修改组件；
- 组详情新增可选 `member_page + member_size`，React 页面固定每页 100 个成员；
- 前端对当前成员页做虚拟化，不一次创建全部成员 DOM；
- 搜索输入使用 300ms 防抖并取消过期请求；
- 同一筛选条件的最近分页结果保留有限 LRU 缓存；
- 页面不展示无法由 API 证明的总数；缺少 `total` 时显示“已加载数量”和“继续加载”；
- 紧凑密度为默认值，单行目标高度 44px；可切换 56px 舒适模式。

批量选择语义必须明确：

- 表头复选框只选择当前已加载页；
- “选择全部查询结果”只有后端提供不可歧义的查询快照或 token 时才启用；
- 未加载页、离线 Agent 和代表文件不得被隐式选中；
- 筛选条件变化时清空选择并给出提示。

## 5. React 模块边界

```text
src/
  app/
    App.tsx
    navigation.ts
  api/
    client.ts
    adapters.ts
    contracts.ts
    availability.ts
  features/
    overview/
    agents/
    scans/
    analysis/
    groups/
    deletion/
  components/
    AppShell/
    DataTable/
    EmptyState/
    ErrorState/
    LoadingState/
    ConfirmDialog/
  hooks/
    useDebouncedValue.ts
    usePagedQuery.ts
    useSelection.ts
  fixtures/
    demoData.ts
  styles/
    tokens.css
    global.css
  assets/
    aurora-surface.png
    media-placeholder.png
```

每个 feature 只依赖 `api/contracts.ts` 中的稳定前端模型。HTTP 返回值先经过 adapter 转换，组件不直接依赖 Go JSON 的偶然形状。

## 6. API 与可用性降级

### 6.1 当前已实现接口

- `GET /api/agents`
- `POST /api/scan`
- `GET /api/tasks`
- `GET /api/dup_groups?limit=&offset=`
- `GET /api/dup_groups/{sha512}`
- `POST /api/analysis/firstscreen/run`
- `GET /api/analysis/firstscreen/status`
- `GET /api/groups?kind=&page=&size=&q=&machine=&min_members=&sort=`
- `GET /api/groups/{id}?member_page=&member_size=`
- `POST /api/delete/prepare`
- `POST /api/delete/execute`
- `GET /api/delete/tasks/{task_id}`

以上接口均直接接入生产页面。三类重复组统一使用 `/api/groups`；
旧 `/api/dup_groups` 只保留为后端兼容接口，不再作为新页面主数据源。

删除服务暂不可用时会返回 503，前端保留用户选择并显示可重试错误，
不得使用模拟成功结果冒充生产删除。Fixtures 只用于测试和显式开发模式，
生产构建默认关闭。

## 7. 关键交互

### 7.1 扫描

1. 用户选择在线 Agent；
2. 输入一个或多个根路径；
3. 选择是否强制重算；
4. 提交后立即显示任务 ID；
5. 任务列表以 2 秒间隔轮询；
6. 页面失焦时降为 10 秒，任务全部终止态时停止轮询；
7. 发送失败保留表单内容并展示可复制错误。

### 7.2 一筛分析

1. 页面读取当前分析状态；
2. 运行中禁用重复触发；
3. 接口返回 409 时转为“已有分析正在运行”状态；
4. 运行时轮询状态；
5. 完成后展示统计和各阶段耗时；
6. 普通错误与 panic 文本均显示为失败，不清空上次成功统计。

### 7.3 重复组审阅

1. 用户通过筛选或全局搜索缩小结果集；
2. 中栏选择一个组；
3. 右栏按需加载成员；
4. 代表文件固定为不可选；
5. 每个成员展示机器、路径、大小及可用评分；
6. 用户明确勾选文件并加入删除清单；
7. 离线机器成员显示不可执行状态。

### 7.4 二次确认删除

1. 首次点击“检查删除清单”；
2. 前端调用 `/api/delete/prepare`；
3. 弹窗展示文件数、总大小、机器分布和样本路径；
4. 默认选择软删；
5. 硬删选项使用更强警示且需要再次明确选择；
6. 用户确认后调用 `/api/delete/execute`；
7. 按 task ID 展示进度和逐项结果；
8. confirm token 失效、Helper 不可达、文件占用、权限不足分别显示明确错误；
9. 成功项从当前组移除，失败项保留并可重试。

## 8. 视觉系统与生成素材

视觉采用已确认的 V4“流光智能助理”：

- 主色：靛蓝与紫色；
- 状态色：青绿成功、琥珀警告、玫红危险；
- 背景：低对比蓝紫青极光纹理；
- 表面：高透明度外层玻璃 + 高不透明度数据表格；
- 圆角：外层 16px、控件 8–10px；
- 动效：仅用于抽屉、弹窗、状态变化，持续 120–220ms；
- 危险操作不使用玻璃弱对比，必须使用实体危险色。

使用图像生成制作两项本地 UI 素材：

1. `aurora-surface.png`：无文字、低对比、可平铺或 cover 的蓝紫青极光背景；
2. `media-placeholder.png`：无品牌、无人物肖像的抽象媒体缩略图占位图。

素材必须本地打包；页面在素材加载失败时仍有 CSS 渐变后备。

## 9. 状态、错误与无障碍

每个数据区域必须有四种明确状态：

- loading：骨架屏；
- empty：说明当前筛选无结果并提供清除筛选；
- error：展示人读错误、重试按钮和可复制技术详情；
- ready：正常内容。

无障碍要求：

- 所有功能可用键盘完成；
- 可见焦点环；
- 复选框和危险按钮有明确文本标签；
- 弹窗锁定焦点并支持 Escape 返回；
- 正文和表格达到 WCAG AA 对比度；
- 不以颜色作为唯一状态提示；
- 支持 `prefers-reduced-motion`。

桌面优先，推荐宽度 1440px。1280px 及以上保留三栏；低于 1280px
时详情栏改为带遮罩、焦点约束和背景交互隔离的右侧抽屉。手机仅保证查看
和基础任务操作，不开放批量删除。

## 10. 测试策略

### 10.1 单元测试

- API adapter 对成功、空值、错误和未知字段的处理；
- 路径输入分隔与扫描请求构造；
- 分页和筛选状态；
- 选择语义；
- 删除 prepare/execute 状态机；
- 字节、速度、时间和评分格式化。

### 10.2 组件测试

- 表头全选只影响当前页；
- 筛选变化清空选择；
- 代表文件不可选择；
- 离线成员不可提交删除；
- confirm token 过期后要求重新 prepare；
- 删除服务返回 503 时保留选择、允许重试且不产生虚假成功；
- 键盘导航和弹窗焦点恢复。

### 10.3 集成与视觉验证

- 使用模拟 API 覆盖 Agent、扫描、一筛、三类重复组和删除完整流程；
- 生成 100 万文件规模下的合成分页元数据，验证浏览器只持有当前页；
- 运行前端 build、test 和 lint；
- 运行 `go test ./internal/gui ./cmd/gui`，确认静态资源替换不破坏 HTTP 服务；
- 启动本地 GUI，用浏览器检查 1440px、1280px 和窄屏布局；
- 检查控制台无错误、无失败请求风暴、无明显布局溢出。

## 11. 多 Agent 实施分工

实施阶段采用三个子 Agent 并行处理互不重叠的目录：

1. 工程与视觉 Agent：Vite/React 工程、设计令牌、应用壳、生成素材集成；
2. 数据与状态 Agent：API contracts、adapter、fixtures、分页与选择 hooks；
3. 功能组件 Agent：Agent、扫描、一筛、重复组、详情和删除交互组件。

主 Agent 负责：

- 生成 UI 素材；
- 合并三个工作流；
- 解决类型和构建集成问题；
- 构建到 `internal/gui/web/`；
- 运行全套验证；
- 做最终代码审查和交付。

子 Agent 不同时修改同一文件；共享契约先固定，再并行开发。

## 12. 明确不做

- 除 `/api/groups` 的向后兼容查询扩展外，不修改 M4/M5 后端行为、
  删除 Helper 或 TCP 消息；
- 不修改数据库或媒体分析算法；
- 不把百万条数据一次性加载到前端；
- 不引入远程字体、CDN、在线图片或运行时网络素材；
- 不保留五套生产主题；
- 不把视觉探索目录 `.superpowers/` 纳入生产构建。

## 13. 验收标准

完成时必须满足：

- 现有 Go GUI 打开后显示新的 V4 React 页面；
- 原页面仍可通过 `/legacy.html` 只读回退；
- 当前已实现的 Agent、扫描、任务、一筛、三类重复组和删除 API 均可操作；
- 后端服务返回不可用状态时有明确、诚实且可重试的错误界面；
- 三类重复组布局、评分详情、成员勾选和删除确认流程可连接真实 API 完成；
- 重复组列表采用服务端分页和虚拟化；
- 生产页面不依赖外网；
- 前端测试、构建和相关 Go 测试通过；
- 页面在 1440px 与 1280px 下无关键内容溢出；
- 删除确认默认软删，且不会隐式选择未加载文件。

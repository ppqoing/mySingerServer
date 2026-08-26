# WebUI 去重工具审查报告（2026-08-14）

## 审查范围与方法

审查对象：`webui/src` 前端（React+TS）全部七个功能页（概览、节点、扫描、分析、重复组、删除流程、设置）、应用框架组件，并对照 `webui/src/api/contracts.ts` 契约与 `internal/gui` 后端 HTTP 端点逐项核对。所有发现均基于实际代码确认，标注文件:行号。

问题分四类：

- **A 显示缺失**：契约/后端已提供数据，UI 未展示或展示不全
- **B 交互缺失**：用户合理期望的操作不存在
- **C 逻辑错误**：已有交互行为错误或不合理
- **D 功能缺口**：作为媒体去重工具应具备而缺失的能力

严重级别：**高**（功能缺失/逻辑错误，直接影响使用）、**中**（信息缺失/交互不便）、**低**（体验细节）。

统计：共 90+ 条发现，其中高级别 10 条（去重后）。

---

## 一、高优先级问题汇总（建议优先处理）

| # | 模块 | 问题 | 位置 |
|---|------|------|------|
| H1 | 重复组 | 窄屏（≤719px）下可以选择成员但**永远无法发起删除**：选择栏按钮 `!isMobile` 才渲染，抽屉内按钮要求 `isDrawer && onDelete`，而 `onDelete={isMobile ? undefined : requestDelete}`——两个入口在移动端同时消失。若"移动端不开放删除"是设计意图，则选择框也应一并禁用；否则是传值笔误。当前"能选不能删"两种解释下都不自洽 | `GroupsPage.tsx:518-530,540`、`GroupDetail.tsx:220-231` |
| H2 | 重复组 | **无缩略图/预览对比**：相似图片/视频组只能靠路径、大小和原始 JSON 评分盲选。详情里的"缩略图"是 `aria-hidden` 的静态占位。注意：agent 本机通道已有完整预览能力（`local.preview.image` 按 file_id 取图、`proto/local.go:41,422-448`、worker `PhasePreview` 管线），只服务 nodetray，**未暴露到 Web**。缺 gui.exe 的 HTTP 代理端点 + 前端展示 | `GroupDetail.tsx:119-123`、`internal/gui/httpapi.go`（无预览路由） |
| H3 | 重复组 | **无法指定保留哪个副本/变更代表文件**：代表文件由后端自动指派且受删除保护，UI 无任何"把此成员设为保留"操作。agent 本机通道已有 `local.review.save`（keep/delete/undecided，`proto/local.go:306-329`），同样未暴露到 Web | `GroupsPage.tsx:204-216`、契约无对应 API |
| H4 | 重复组 | **无按保留策略的批量自动选择**：没有"保留最新/最旧/最大/指定目录，自动选中其余副本"。面对大量组只能逐组手动勾选 | `GroupsPage.tsx` 整体 |
| H5 | 删除流程 | **删除任务无列表/历史**：`GET /api/delete/tasks` 路由已注册但 handler 无 task_id 必 404（死路由）；任务存内存 map，GUI 重启即丢；浏览器刷新后"删除审计"页丢失对进行中任务的跟踪。设计文档 §12.2/§18/§20 明确要求任务列表与刷新恢复 | `httpapi.go:103-104`、`delete_http.go:198-206`、`delete.go:320`、`App.tsx:19-21` |
| H6 | 扫描 | **无停止/取消扫描入口**：管理端 HTTP API 无取消路由，取消能力只存在于 agent 本机协议（`LocalOperationTaskCancel`），未延伸到管理端。误发长时间扫描无法中止 | `ScansPage.tsx:173-183`、`httpapi.go:86-96` |
| H7 | 概览 | **数据库错误码 `databaseErrorCode` 未展示**：后端区分四种错误码（未配置/认证失败/无法连接/不可用），页面一律显示"PostgreSQL 未连接，Manager 会继续尝试恢复连接"——"未配置"场景这句引导是误导，不填 DSN 永远不会恢复 | `OverviewPage.tsx:32-36`、`runtime_status.go:26-39` |
| H8 | 概览 | **仪表盘缺核心业务指标**：没有已扫描文件总量、重复组总数（exact/image/video 可从 `GroupPage.total` 获取但未使用）、可回收空间汇总（需后端加聚合端点）、分析检出统计。作为去重系统首页信息量不足 | `OverviewPage.tsx:49-55` |
| H9 | 扫描/分析 | **扫描→分析→看结果主流程无闭环**：扫描 done 后无"下一步：运行分析"引导，分析完成后无"查看重复组"入口，三个页面完全割裂，用户需自行知道切换到哪里 | `ScansPage.tsx`、`AnalysisPage.tsx` 整体 |
| H10 | 概览/框架 | 顶部状态栏**硬编码**"中央服务：正常"，数据库断开、重启中、节点全部离线时依然显示"正常"，属于错误的状态展示 | `AppShell.tsx:147` |

---

## 二、重复组页面（groups）——去重核心

### A 显示缺失

| 级别 | 位置 | 问题 |
|------|------|------|
| 中 | `GroupTable.tsx:32` | 组行只显示"N 台设备"，契约 `machines: string[]` 的完整分布未展示，连 title 悬浮提示都没有；窄屏（<44rem）该列被 CSS 整体隐藏（`GroupsPage.css:107`） |
| 中 | `GroupDetail.tsx:152,165`（`textScore()` 70-99 行） | 相似度 score 以原始 JSON 糊给用户：exact 组显示无意义的 `basis: sha512`；image/video 组显示含 `peer_sha512`（128 位 hex）等内部字段。应转为"与代表文件汉明距离 N / 相似度"等可读形式 |
| 中 | `GroupDetail.tsx:150,163` | 成员大小未人类化：`4,294,967,296 B`。列表已有 `byteText()`（`GroupTable.tsx:12-17`）做 KB/MB/GB 换算，详情未复用，同页两种格式不一致 |
| 低 | `GroupDetail.tsx:283,191` | 详情面板缺组级上下文：标题无组 ID，工具栏只有成员数/页码，看不到该组 kind、总容量、可回收量 |
| 低 | `GroupTable.tsx:30` + CSS 43-44 | 代表路径 ellipsis 截断后无 title，hover 也看不到全路径 |

### B 交互缺失

| 级别 | 位置 | 问题 |
|------|------|------|
| 中 | `GroupTable.tsx:71-75`、`GroupDetail.tsx:216-219` | 分页只有上一页/下一页。测试证实 total 可达 100 万组（约 1 万页），无跳页输入、无首页/末页 |
| 中 | `GroupDetail.tsx:149,158-168` | 无法复制文件路径："查看完整信息"弹窗展示全路径但无复制按钮；对远程 agent 上的文件，复制路径是用户定位文件的唯一手段 |
| 中 | `GroupsPage.tsx:505-513` | 组列表无手动刷新入口，也不轮询；分析写入新组后用户无任何方式刷新（只能改筛选/翻页间接触发） |
| 低 | `GroupsPage.tsx:73,175` | 每页条数硬编码 100，不可切换（测试表明是有意设计） |
| 低 | `GroupTable.tsx:57-64` | 排序仅 3 种降序（契约所限），表头是静态文本 div，不与列对齐、不可点击 |
| 低 | `VirtualTable.tsx` | 虚拟列表无方向键导航；虚拟化导致 Tab 只能到达已挂载的行 |

### C 逻辑错误

| 级别 | 位置 | 问题 |
|------|------|------|
| 高 | 见 H1 | 窄屏下能选不能删 |
| 中 | `usePagedGroups.ts:86-89,46-50` | 删除完成后 `reload()` 只强刷当前页，LRU 中其余至多 5 页缓存继续服役，翻页会看到删除前的陈旧数据；测试 `hooks.test.tsx:158` 固化了此行为，修复需同步改测试 |
| 中 | `useSelection.ts:17-25`、`GroupsPage.tsx:500-504` | Agent 离线时已选 id 在渲染期被静默过滤，"已选 1 项"直接变 "已选 0 项"且无任何通知（`setSelectionNotice` 未覆盖此路径） |
| 中 | `GroupFilters.tsx:82` → `GroupsPage.tsx:261-264` | "最少文件数"数字输入无防抖：路径搜索有 300ms 防抖，数字输入每次击键即发查询并清空选择/详情/页码，输入 "10" 会先以 "1" 发一次查询毁掉当前工作状态 |
| 低 | `GroupsPage.tsx:271-278` | 切换到另一组时静默丢弃原组选择（`setSelectionNotice(undefined)`），与筛选变化时的提示行为不一致 |
| 低 | `GroupTable.tsx:49` | 向用户暴露内部调试信息"行高估算：44px" |
| 低 | `GroupTable.tsx:42-45` | 加载新页期间分页器回退显示"第 1 / 1 页"，随后跳到真实值，视觉跳变 |
| 中 | `GroupsPage.tsx:140-147`、`GroupDetail.tsx:136` | 身份冲突（在线但未 claimed）的 Agent 成员徽章固定显示"Agent 离线"，把冲突误标为离线；设计 §8.1 明确冲突不能降级显示为普通离线 |

### D 功能缺口

| 级别 | 问题 |
|------|------|
| 高 | H2 预览对比、H3 指定保留、H4 策略批量选择 |
| 中 | 无组级一键操作：列表行只能"打开"，没有"选中本组除代表外全部成员并进入删除确认"的快捷操作；处理 N 个组需 N 次完整往返 |
| 中 | "全选"只覆盖当前成员页（`GroupsPage.tsx:280-286`，≤100 条），成员超 100 的组（如连拍照片集）无法一次选全可删成员。测试表明是有意 fail-safe，但仍是能力缺口 |
| 低 | 无全局可回收空间汇总（"当前筛选共可回收 X GB"），需后端聚合字段 |

---

## 三、扫描与分析（scans / analysis）

### A 显示缺失

| 级别 | 位置 | 问题 |
|------|------|------|
| 中 | `ScansPage.tsx:176-179` | `scanErrors` 列缺失：后端在 `scanErrors>0` 时把任务标为 failed（`tasks.go:287-291`），用户看到"failed"却看不到错误数量，无法区分"Agent 拒绝"与"扫描出错" |
| 中 | `ScansPage.tsx:177-180` | `ackReason` 未展示：任务变 done 时无法区分"真扫完"还是"已被去重跳过（already_done）" |
| 中 | `ScansPage.tsx:177-180` | `roots`/`rescan`/`phase` 未展示：看不出每个任务扫哪些目录、是否重扫 |
| 中 | `ScansPage.tsx` 整体 | `recent`（最近 50 条 FeatureItem，含 path/status/err，数据已到达前端）完全未展示：契约类型是 `unknown[]` 未结构化，UI 无错误明细视图 |
| 低 | `ScansPage.tsx:179` | `speed` 原始 float 直出（`0.8333333333333334`），无单位无格式化 |
| 低 | `ScansPage.tsx:179` + `tasks.go:183-194` | 运行中任务 `elapsedMs` 恒为 0（仅 TaskDone 时写入），表格运行期间显示"0.0 秒"有误导性，应显示"—" |
| 低 | `ScansPage.tsx:178` | 任务状态原始枚举直出（sent/acked/running），无中文文案；`updatedAt`/`lastSeq` 未展示，Agent 掉线停滞的任务与正常任务无区别 |
| 低 | `AnalysisPage.tsx:18,97-99` | `heapAllocBytes` 原始字节直出（label 就叫"堆内存（字节）"）；`stageElapsedMs` 毫秒直出且无阶段汇总 |

### B 交互缺失

| 级别 | 位置 | 问题 |
|------|------|------|
| 高 | 见 H6 | 无停止/取消扫描入口（后端同样缺失） |
| 中 | `ScansPage.tsx:168-184` | 任务列表无手动刷新；且全部任务终态后轮询永久停止（`isTerminal` 49-52 行），此后其他客户端产生的变化不可见，只能刷新整个页面 |
| 中 | `ScansPage.tsx` | 无"重扫失败项"：`rescan` 只能整根目录强制重扫，不能按失败清单重试 |
| 中 | `ScansPage.tsx` | 任务行不可点击，无任务详情/错误详情查看入口 |
| 中 | `RemotePathBrowser.tsx:79-82` | 无"返回上一级"按钮：契约 `parentPath` 已解码但从未使用，只能靠面包屑 |
| 中 | `RemotePathBrowser.tsx:91` | 浏览出错无重试按钮，唯一恢复方式是关闭重开 |
| 低 | `RemotePathBrowser.tsx` | 无刷新当前目录、无条目搜索/过滤（pageSize=100 下找目录只能逐页加载） |
| 低 | `AnalysisPage.tsx` | 无取消运行中分析的入口（后端 `AnalysisRunner.Run()` 也无上下文取消） |
| 低 | `AnalysisPage.tsx` | 无分析结果导出、无历史运行记录（后端只保留 `last`） |

### C 逻辑错误

| 级别 | 位置 | 问题 |
|------|------|------|
| 中 | `RemotePathBrowser.tsx:121-131` | 面包屑不支持 UNC 路径：`windowsBreadcrumbs` 只匹配 `^([a-zA-Z]:\\)`，而手工输入与 taskRoots 均支持 UNC；进入 `\\server\share\dir` 后面包屑整条消失，只能回根重来 |
| 中 | `RemotePathBrowser.tsx:51-59` | 重开对话框时残留旧目录内容：切换 Agent 后再打开会短暂看到**另一台机器**的目录列表 |
| 低 | `AnalysisPage.tsx:64-66,90` | 409"已有分析正在运行"alert 常驻不清：状态已回到"空闲"后该错误提示仍保留 |
| 低 | `ScansPage.tsx:79,155` | 旧成功消息与新错误并存："已创建任务：xxx"常驻，再次提交失败时与新错误 alert 同时显示 |
| 低 | `ScansPage.tsx:115-120` | "切换 Agent 后已清空待选根目录"是信息提示，却走 `role="alert"` 错误样式通道 |
| 低 | `ScansPage.tsx:58-89` | 相同 roots 重复提交无拦截：后端每次生成新 UUID，同机同路径可并发创建多个重复扫描任务 |
| 低 | `RemotePathBrowser.tsx:64-70,105-119` | 单击目录即进入，无"选中不进入"语义：`selectedPath || currentPath` 恒等于 currentPath（冗余）；想添加某目录必须先进去 |
| 低 | `RemotePathBrowser.tsx:73` | 对话框无 Esc 关闭、无背景点击关闭、`aria-modal` 但无初始焦点/焦点圈定 |
| 低 | `ScansPage.tsx:135-139` | Agent 下拉项不显示 `addr`，多连接去重后用户无法辨认实际选中的连接 |

### D 功能缺口

| 级别 | 问题 |
|------|------|
| 高 | H9 主流程无闭环 |
| 中 | 无任务历史管理与筛选：所有任务（含全部终态）混在一张无分页、无过滤、无搜索的表里，也无清理入口，长期使用后列表失控 |
| 中 | 无法发起 phase 2 扫描：`StartScanInput.phase` 与后端均支持，UI 硬编码 `phase: 1`；配置里有 `phase2.autoDispatch`，界面无任何手动入口或进度展示 |
| 中 | 进度可视化缺失：扫描只有 done/total 文本无进度条；分析运行中仅有"运行中"三个字（契约本身无进度字段，需后端补），长任务完全黑盒 |
| 低 | 失败/错误文件无清单导出，失败后排查只能靠 lastErr 一行文本 |

---

## 四、节点 / 概览 / 设置 / 应用框架

### A 显示缺失

| 级别 | 位置 | 问题 |
|------|------|------|
| 高 | 见 H7、H8、H10 | 数据库错误码、核心指标、假状态栏 |
| 中 | `OverviewPage.tsx`（全文未引用 `restarting`） | Manager 重启中状态（`restarting`/`recoveryURL`）无任何页面消费；由其他客户端触发重启时，本页只会把后端原始错误码 `server_shutting_down` 当错误文本弹出 |
| 中 | `OverviewPage.tsx:26,42` | Agent 卡片只统计 online，不汇总身份异常：`pending`/`conflict` 节点对扫描与删除是致命异常，仪表盘无"待识别 N / 身份冲突 N" |
| 中 | `OverviewPage.tsx:25,47` | 扫描卡片无失败与错误聚合：失败任务数、`scanErrors`、`failed` 文件数均未汇总 |
| 低 | `AgentsPage.tsx:18-22,55` | 身份冲突只显示"身份冲突"四字，无解释性文案告诉用户这意味着重复部署/配置错误 |
| 低 | `OverviewPage.tsx:37`、`AgentsPage.tsx:41`（根源 `client.ts:87-91`） | 后端原始错误码（如 `database_unavailable`）未经翻译直接作为用户可见错误文本 |

### B 交互缺失

| 级别 | 位置 | 问题 |
|------|------|------|
| 中 | `AgentsPage.tsx:44-63` | 节点无任何操作入口：离线节点不能触发重连，冲突/失效节点不能移除（契约级缺失，需后端配合） |
| 中 | `OverviewPage.tsx:38-56` | 概览卡片不可点击，无下钻跳转：整页唯一链接是数据库错误时的"打开 GUI 设置" |
| 中 | `GUISettingsPage.tsx:75,62-84` | 设置页无客户端校验：清空数字输入框 `Number("")===0`，心跳间隔、阈值静默变 0；非法输入得 `NaN`，JSON 序列化为 `null` 提交；`heartbeat_s` 可填负数 |
| 低 | `GUISettingsPage.tsx:251-252` | 校验失败无错误定位：约 25 个字段分五个区块，失败后不滚动/不聚焦到第一个 `aria-invalid` 字段，无顶部错误汇总 |
| 低 | `GUISettingsPage.tsx:371-407` | 配置项说明普遍缺失：仅 `videoFrames` 有 note，其余 18 个一筛/二筛参数无单位、范围、调整影响说明 |
| 低 | `GUISettingsPage.tsx:276,416` | 有未保存更改时无防丢保护：dirty 状态点"重新加载"或切换路由直接丢弃草稿 |
| 低 | `OverviewPage.tsx:28-57` | 概览页无手动刷新按钮（AgentsPage 有） |

### C 逻辑错误

| 级别 | 位置 | 问题 |
|------|------|------|
| 中 | `OverviewPage.tsx:14-26,42,47,52` | 数据库断开后业务轮询正确停止，但旧数据原样保留且无"已过期"标注——"PostgreSQL 未连接"警报下方仍显示断开前的"在线 X/共 Y"，用户可能把过期快照当成当前状态 |
| 低 | `OverviewPage.tsx:21-24` | 分析卡片轮询一旦空闲即永久停止（`isTerminal: !running`），其他标签页/客户端启动分析时本页永远停留在旧状态 |
| 低 | `AgentsPage.tsx:29` vs `OverviewPage.tsx:26` | "在线"统计口径两页不一致：AgentsPage 要求 `online && claimed`，OverviewPage 只要求 `online` |
| 低 | `AgentsPage.tsx:37` | 刷新按钮 `disabled={state.loading}` 随 2 秒后台轮询抖动禁用，文案在"刷新/正在刷新…"间闪烁 |
| 低 | `GUISettingsPage.tsx:236-237,254-256` | `recoveryURL` 为空/非法时 `new URL()` 抛 `TypeError: Invalid URL`，英文原始消息直接作为 pageError 显示 |
| 低 | `App.tsx:48` | 未知路由静默重定向到 /overview，输错地址无任何提示 |

---

## 五、删除流程（deletion）

### A 显示缺失

| 级别 | 位置 | 问题 |
|------|------|------|
| 中 | `delete_http.go:239-244` | `safeDeleteStatus` 把每条 `ErrorMessage`/`StateSyncErr` 替换为固定串 "delete item failed"，前端永远看不到真实失败原因。脱敏是刻意的（有测试），但与设计 §5.5"原始错误详情可查看"冲突 |
| 中 | `proto/message.go:461` vs `delete.go:240-248` | 软删回执含 `RecycledTo`（回收目标路径=还原映射），但状态响应不携带——软删后文件去了哪里 Web 完全不可见 |
| 低 | `DeleteStatusPanel.tsx:51-53` | 审计页机器表无序列级进度（`sequences` 未渲染；DeleteDialog 里有），设计 §12.2 要求展示 |
| 低 | `DeleteDialog.tsx:153-158` vs `DeleteStatusPanel.tsx:13-26` | 对话框内 errorCodes 无中文标签，审计页有完整 12 码映射，同一信息两处不一致 |
| 低 | `GroupsPage.tsx:515` | 选择摘要只有"已选 N 项"；设计 §11.3 要求显示文件数、总大小、涉及机器数（数据已具备） |
| 低 | `DeleteDialog.tsx:449`、`DeleteStatusPanel.tsx:96` | 任务 ID/长路径/错误详情无复制入口 |

### B 交互缺失

| 级别 | 位置 | 问题 |
|------|------|------|
| 高 | 见 H5 | 删除任务列表/历史 |
| 中 | `delete.go:287,785-800` | 删除中断恢复未实现：agent 断链只能等 12 分钟 deadline 全部判 `E_HELPER_LOST` uncertain，之后靠手动重试；设计 §12.1 的"稳定删除操作 ID + 重算剩余文件重派"未实施 |
| 低 | — | 软删无还原入口（配合 `RecycledTo` 缺失，软删可逆性在 Web 上完全体现不出） |
| 低 | `pool.go:391`、`agent/server.go:327` | `MsgStatsQuery`/`MsgConfigPush` agent 侧已实现，GUI 从不调用——agent 运行统计、配置推送无 Web 入口 |

### C 逻辑错误

| 级别 | 位置 | 问题 |
|------|------|------|
| 中 | `DeleteDialog.tsx:104-108,298-316` + `delete_http.go:183-186` | **409 token 已消费未区分**：`isExpiredConfirmation` 只认 400；token 被消费（网络重试/双击竞态）后返回 409，落入通用失败分支且仍持死 token——用户再点永远 409，死循环直到倒计时耗尽。应与过期同等处理，强制重新 prepare |
| 中 | `GroupsPage.tsx:411-433` + `usePagedGroups.ts:86-89` | 删除完成后组列表不保证刷新：`finishDelete` 只在 snapshot 存在且 scope 匹配且 agent 核验就绪时才 `reload()`，其余分支不刷新；且 `reload()` 只强刷当前页，其余缓存页仍旧 |
| 低 | `DeleteDialog.tsx:413-421` | 删除状态轮询一次失败即完全停止，瞬时网络抖动就冻结进度；设计 §5.3 要求有上限的退避自动恢复；与 DeleteStatusPanel 的策略不一致 |
| 低 | `GroupsPage.tsx:526-528` | 任务已到终态后按钮仍显示"查看进行中的删除任务"（关闭对话框才清除） |
| 低 | `DeleteStatusPanel.tsx:34-37,94` | 受控模式（App 传入 taskId）隐藏手动查询表单，无法查其他任务；`#/audit?task=` hash 入口无任何应用内导航使用，是死功能 |
| 低 | `DeleteDialog.tsx:229` | prepare 409 冲突直接透传英文 "delete selection conflict"；设计 §15.1 要求转换为中文状态文案 |

---

## 六、后端 API 能力盘点

### 端点清单（`internal/gui/httpapi.go:84-112` + `runtime_host.go:116-131`）

| 端点 | 前端使用情况 |
|------|------|
| GET `/api/agents` | 已用 |
| POST `/api/agents/{machine_id}/filesystem/browse` | 已用 |
| GET/PUT `/api/config` | 已用 |
| GET `/api/runtime/status` | 已用 |
| GET `/api/restart/health` | 已用（waitForManager） |
| POST `/api/scan`、GET `/api/tasks` | 已用 |
| GET `/api/dup_groups`、`/api/dup_groups/{sha512}` | **未用**（legacy 精确组接口，已被 `/api/groups?kind=exact` 取代） |
| GET `/api/groups`、`/api/groups/{id}` | 已用 |
| GET `/groups`（服务端 HTML 页） | webui 未用 |
| POST `/api/analysis/firstscreen/run`、GET `.../status` | 已用 |
| POST `/api/delete/prepare`、`/api/delete/execute` | 已用 |
| GET `/api/delete/tasks`、`/api/delete/tasks/{$}` | **死路由**（无 task_id 必 404） |
| GET `/api/delete/tasks/{task_id}` | 已用 |

### 已实现但未延伸到 Web 的能力（去重工具关键缺口）

1. **文件预览**：agent 本机通道 `local.preview.image` 按 file_id 取图 + worker 独立 PhasePreview 生成管线，只服务 nodetray。→ 补 gui.exe HTTP 代理端点 + 前端展示即可解锁 H2。
2. **保留标记**：agent 本机 `local.review.save` 支持 per-file keep/delete/undecided。→ 暴露后可解锁 H3。
3. **任务取消**：agent 本机 `local.task.cancel` 存在；manager→agent 协议本身无取消消息，加扫描取消需先扩协议。
4. **agent 统计/配置推送**：`MsgStatsQuery`/`MsgConfigPush` agent 侧已实现，GUI 从未调用。

---

## 七、去重工具功能缺口总清单（按建议优先级）

1. **文件预览/缩略图对比**（H2）——agent 侧能力已有，只差 GUI 代理 + UI，性价比最高
2. **保留标记/指定代表文件**（H3）——同上，agent 侧能力已有
3. **删除任务列表与历史持久化**（H5）——路由已是半残注册状态，设计文档明确要求
4. **保留策略批量自动选择**（H4）+ 组级一键操作——大批量去重的核心效率功能
5. **扫描任务取消**（H6）——需先扩 manager→agent 协议
6. **删除中断重算重派**——设计 §12.1 已规划未实施
7. **扫描→分析→看组主流程闭环引导**（H9）——纯前端可达
8. **概览仪表盘核心指标**（H8）——文件总量/组总数前端可取，可回收空间汇总需后端聚合端点
9. **数据库错误码与状态栏真实化**（H7/H10）——纯前端，低成本高收益
10. **软删还原路径可见性**（RecycledTo 透传 + 还原入口）

---

## 八、附注

- **被测试固化的可疑行为**（修复时需同步改测试）：缓存页陈旧（`hooks.test.tsx:158`）、离线静默清选择（`GroupsPage.test.tsx:909-934`）、全选仅限当前成员页（`GroupsPage.test.tsx:501-525`）。
- **实现正确、未列入问题的部分**：删除两阶段确认、token 倒计时与过期双路径处理、executing 锁窗、删除后重试项恢复（`deleteReview.ts`）、Modal/overlayStack 焦点约束、详情/列表请求竞态处理（abort + key 比对）、usePolling 停止条件、空 roots 双保险校验、taskRoots 规范化逻辑等，均有测试覆盖且实现正确。
- 已知部分缺口在仓库设计文档中列为"已规划未实施"（如 `docs/superpowers/specs/2026-08-13-compute-scan-throughput-progress-cancel-design.md` 针对 nodetray 本地任务的取消），本报告从 Web 管理端视角重新确认其影响。


---

## 九、改进方案（业界最佳实践对照）

方案按实施优先级分四档：**P0 核心去重能力**、**P1 主流程与数据正确性**、**P2 信息展示**、**P3 体验打磨**。每条标注对应的问题编号、业界参照做法和本项目具体落地路径。

### P0 核心去重能力

#### P0-1 文件预览与对比（对应 H2）

**业界做法**：dupeGuru Picture Edition 提供详情窗并排对比，Delta Values 模式把每个成员与参考文件的差异（大小 ±、属性不同处）高亮显示；Czkawka 相似图片模式内联缩略图预览，相似视频模式给出视频信息；Video Duplicate Finder 直接渲染视频缩略图条。共同点是：**判断"该不该删"永远基于看图，而不是看路径**。

**本项目落地**（agent 侧能力已存在，成本主要在通路）：

- 后端：新增 `GET /api/files/{fileId}/preview?machine=` 端点，按 machineId 路由代理到 agent 本机已有的 `local.preview.image`（`proto/local.go:41,422-448`）；视频复用 worker 的 PhasePreview 管线（`wproc/image_preview.go`）取关键帧。Agent 离线时返回 503 + 明确错误码。
- 前端：`GroupDetail.tsx:119-123` 的占位 span 替换为真实缩略图（懒加载 + 失败降级占位）；点击图片进入对比视图，并排展示两名成员的大小/mtime/差异字段（参照 dupeGuru Delta Values：与代表文件的差异高亮）。
- 同步删除占位 CSS（`GroupsPage.css:69-77`），符合设计 §2.2"无对应 API 的功能不以占位入口出现"。

#### P0-2 指定保留副本 / 变更代表文件（对应 H3）

**业界做法**：dupeGuru 用 Reference Folder（参考目录内文件自动作为保留方）+ Re-Prioritize（每组可按大小/路径等条件重选参考文件），且**参考文件永远不可被标记删除**——与本项目的代表保护机制完全同构，说明本项目保护方向正确，缺的只是"谁来当代表"的用户控制权。rmlint 则用路径顺序、mtime 等可配置规则判定 original。

**本项目落地**：

- 后端：新增 `POST /api/groups/{id}/representative`（body: fileId），更新组的 representative_file_id；或在 `prepareDelete` 入参允许 `keepIds` 覆盖默认代表。语义可复用 agent 本机已有的 `local.review.save`（keep/delete/undecided，`proto/local.go:306-329`）。
- 前端：成员行加"设为保留"操作，设置后刷新详情并给出 toast；`DeleteDialog` 确认页列出本次保留的文件，与删除清单一并展示。

#### P0-3 按保留策略批量自动选择（对应 H4、组级一键操作、跨页全选）

**业界做法**：Czkawka 提供选择助手（按最大/最小/最新/最旧一键标记）；Duplicate Cleaner 的 Selection Assistant 支持按日期、大小、路径、正则批量标记；dupeGuru 有 Power Marker。共同点是**把"保留规则"一次性应用到全部组，用户只负责抽查**。

**本项目落地**：

- 第一步（纯前端）：组详情工具栏加"自动选择"下拉——保留最新/最旧/最大/最短路径，基于 `members` 的 mtime/size/path 计算并勾选其余成员。
- 第二步（需后端）：跨页组与批量场景，新增 `POST /api/groups/select-by-strategy`（入参：筛选条件 + 策略，出参：fileIds），避免前端逐页拉取成员；组列表行加右键/行内快捷操作"保留代表，选中其余"。
- 与 P0-2 联动：策略选择后用户仍可手动微调，再进入删除确认。

#### P0-4 删除任务列表与持久化（对应 H5、概览 4-3、框架 3-6）

**业界做法**：后台任务中心模式——任务落库、列表页可查、刷新/重开页面后按列表恢复跟踪（GitHub Actions、云控制台操作记录均如此）。本项目设计文档 §12.2/§18/§20 已明确要求，属于补齐欠账。

**本项目落地**：

- 后端：`central.sql` 增 `delete_tasks` 表（参照已有 `scan_tasks` 表，:138）；`DeleteService.tasks` 内存 map 改为"内存缓存 + 落库"；实现 `GET /api/delete/tasks` 列表（当前是必 404 的死路由，`delete_http.go:198-206`）。
- 前端：删除审计页默认展示任务列表（进行中在前）；短期过渡方案先把 `activeDeleteTaskId` 持久化到 sessionStorage（`App.tsx:19-21`），刷新后可恢复单个任务跟踪。

### P1 主流程与数据正确性

#### P1-1 移动端删除入口对齐（对应 H1）

先确认设计 §17"移动端不开放批量删除"是否为最终意图，然后二选一，保持自洽：

- 若确认不开放：移动端同时隐藏成员复选框与选择栏（业界惯例：功能按形态裁剪时，其上游入口一并裁剪，如移动端隐藏批量操作则复选框不渲染）。
- 若是笔误：`GroupsPage.tsx:540` 改为 `onDelete={requestDelete}`，抽屉内按钮即恢复。

#### P1-2 删除确认 409 死循环（对应删除 C 类 3.1）

**业界做法**：幂等执行（Stripe Idempotency 模式）——同一确认令牌重复提交应返回同一个已受理任务，而不是报致命错误。

**本项目落地**：

- 后端：`executeDelete` 对已消费 token 直接返回首次受理的 taskId（幂等语义），而非 409。
- 前端（不依赖后端也可先修）：`isExpiredConfirmation`（`DeleteDialog.tsx:104-108`）扩展识别 409 "already used"，与过期同等处理——强制回到重新 prepare，杜绝死循环。

#### P1-3 删除后的列表缓存一致性（对应 groups 3.2、删除 3.3）

**业界做法**：React Query/SWR 的 mutation-then-invalidate 模式——写操作完成后整组查询缓存失效重取，而不是只刷当前页。

**本项目落地**：

- `usePagedGroups.ts` 增加 `invalidateAll()`：清空全部 LRU 缓存页并重取当前页；`finishDelete`（`GroupsPage.tsx:411-433`）的所有分支都调用它（当前 scope 不匹配/无 snapshot 时不刷新）。
- 同步更新固化旧行为的测试（`hooks.test.tsx:158`）。

#### P1-4 扫描/分析任务取消（对应 H6、分析 B 类 16）

- 协议层：参照 agent 本机已有的 `LocalOperationTaskCancel`（`agent/local_handler.go:263`）扩展 manager→agent 的 `MsgScanTaskCancel`；`docs/superpowers/specs/2026-08-13-compute-scan-throughput-progress-cancel-design.md` 中的取消链路设计可复用其结论。
- 后端：新增 `POST /api/tasks/{id}/cancel`；`AnalysisRunner.Run()` 接入 `context.Context` 支持取消。
- 前端：任务行加"停止"按钮，点击后进入 cancelling 中间态，终态前禁止重复点击。

#### P1-5 主流程闭环引导（对应 H9）

**业界做法**：向导式"下一步"CTA——每个阶段完成时明确告知下一步去哪（云产品"创建成功→前往配置"模式）。

**本项目落地**（纯前端即可起步）：

- 扫描任务进入 done 时，行内/页面顶部提示"扫描完成，下一步：运行一筛分析"（跳 `/analysis`）。
- 分析完成卡片显示"检出 N 个重复组，前往查看"（跳 `/groups`）。
- 概览页加流程状态条：扫描中 N → 待分析 → 待处理组 N，让主流程一眼可见。

#### P1-6 删除中断恢复（对应删除 B 类 2.4）

- 长期：实现设计 §12.1 的"稳定删除操作 ID + 重算剩余文件以新任务 ID 重派"。
- 短期：在审计页为 `uncertain`/`E_HELPER_LOST` 项提供"一键重试"入口——`deleteReview.ts` 的 `deriveDeleteRetryPlan` 已具备重试计划能力，缺的只是 UI 接线。

### P2 信息展示

#### P2-1 错误码映射层（对应 H7、概览 1-7、删除 3.6、设置 3-5）

**业界做法**：错误码→用户文案的集中映射层，每个已知错误码配套针对性引导动作（Windows 疑难解答、各云控制台均如此）。

**本项目落地**：新增 `api/errorText.ts` 统一映射，至少覆盖：

| 错误码 | 文案与引导 |
|--------|-----------|
| `postgres_not_configured` | "未配置数据库" + 跳转设置页填 DSN（当前"会继续尝试恢复"是误导） |
| `postgres_auth_failed` | "数据库认证失败，检查用户名密码" |
| `postgres_unreachable` | "无法连接数据库，检查网络与服务状态" |
| `server_shutting_down` / restarting | "Manager 正在重启，稍后自动恢复"横幅（消费 `RuntimeStatus.restarting`） |
| `delete selection conflict` | "选择冲突：部分文件已在其他删除任务中" |
| `delete task not found` | "任务不存在或已随 Manager 重启清除" |

`client.ts:87-91` 的原始 `body.error` 不再直接作为用户可见文案。

#### P2-2 顶部状态栏真实化（对应 H10）

`AppShell.tsx:147` 的硬编码"中央服务：正常"接入 `getRuntimeStatus`（低频轮询或与概览共享数据）：显示"数据库：正常/异常 + 在线节点 X/Y"，异常时红色样式并链接到概览页。

#### P2-3 概览仪表盘补全（对应 H8、概览 1-4/1-5/2-2/3-1）

**业界做法**：管理仪表盘 KPI 卡片 + 点击下钻 + 数据过期标注。

**本项目落地**：

- 卡片内容：已扫描文件数（`AnalysisStats.filesScanned`）、三类重复组总数（`listGroups` 的 `GroupPage.total`，exact/image/video 各发一次 size=1 请求即可取）、失败任务数、身份异常节点数（pending/conflict）。
- 可回收空间汇总需后端加聚合端点（如 `GET /api/groups/stats` 返回按筛选条件的 `SUM(wasted_bytes)`、`SUM(total_bytes)`）。
- 卡片可点击跳转 `/agents`、`/scans`、`/analysis`、`/groups`。
- 数据库断开时清空业务卡片或加"以下为断开前最后数据"水印标注（修 3-1）；分析卡片取消 `isTerminal: !running` 的永久停轮询，改空闲低频轮询（修 3-2）。

#### P2-4 扫描任务信息补全（对应扫描 A 类 1-8）

- 表格补列：`scanErrors`（>0 时红色）、`roots`（行内缩略 + title 全文）、`ackReason`（done 时区分"已完成"与"已跳过：already_done"）。
- 结构化 `recent`：契约从 `unknown[]` 改为 `FeatureItem[]`（path/status/err），任务行可展开查看错误明细——数据已到达前端，只差类型与 UI。
- 状态枚举中文化（sent→已下发、acked→已受理、running→运行中…）；`speed` 格式化为"N 文件/秒"；运行中 `elapsedMs` 显示"—"而非"0.0 秒"；补 `updatedAt` 列并对停滞任务（如 5 分钟无更新）高亮。

#### P2-5 score 可读化与选择摘要（对应 groups 1-2、删除 1.6）

- `textScore()`（`GroupDetail.tsx:70-99`）改为结构化解析：exact 组显示"内容完全一致"；image/video 组显示"与保留文件的差异：汉明距离 N"（可映射为 极小/小/中 三档），隐藏 `peer_sha512`、`quality_self` 等内部字段。
- 成员大小复用 `byteText()` 做 KB/MB/GB 换算，消除同页两种格式。
- 选择摘要按设计 §11.3 补齐："已选 N 项，共 X GB，涉及 Y 台设备"（数据在 `detail.members` 已具备）。

#### P2-6 软删去向可见（对应删除 1.3、2.6）

- 后端：`DeleteProblemItem`/状态响应携带软删回执中的 `RecycledTo`（`proto/message.go:461` 已有字段，只是没透传）。
- 前端：审计页展示"已移入回收目录：X"。
- 长期：参照 rmlint 的 undo 脚本思路，软删任务生成还原映射清单并支持导出，让软删真正可逆。

### P3 体验打磨

| 对应问题 | 方案 |
|----------|------|
| 分页无法跳页（groups 2.1） | 分页器加页码输入 + 首页/末页；参考 GitHub 分页模式 |
| 无法复制路径（groups 2.2、删除 1.7） | 成员行/详情弹窗/任务 ID 旁加复制按钮（`navigator.clipboard` + "已复制"瞬时反馈） |
| 列表无手动刷新（groups 2.3、扫描 10、概览 2-7） | 三页统一加刷新按钮；AgentsPage 刷新禁用态改为独立 `refreshing` 状态，消除随轮询抖动（修 3-4） |
| 筛选防抖不统一（groups 3.4） | 数字输入与路径搜索统一 300ms 防抖，或改为 Enter/失焦生效 |
| 选择丢失无通知（groups 3.3/3.5） | 离线清选择、切组清选择均走 `setSelectionNotice`："N 项因 Agent 离线被移除" |
| UNC 面包屑（扫描 18/13） | 面包屑复用 `taskRoots.ts` 的 UNC 规范化；同时用契约已有的 `parentPath` 加"返回上一级"按钮 |
| 路径浏览器残留（扫描 19/14/25） | 打开对话框时清空旧 entries 并显示加载态；补 Esc 关闭、初始焦点、错误重试按钮 |
| 表单校验（设置 2-3/2-4/2-6） | NumberField 加 min/max/step，空值保持非法态不静默变 0；提交失败聚焦第一个 `aria-invalid` 字段；dirty 状态离开/重载前二次确认 |
| 删除轮询一致性（删除 3.3） | DeleteDialog 轮询改为有上限的退避自动恢复，与 DeleteStatusPanel 策略对齐 |
| 文案一致性（删除 1.5、3.4/3.5、扫描 22） | DeleteDialog 内 errorCodes 补中文映射（复用 DeleteStatusPanel 的 12 码表）；终态后按钮文案改为"查看删除任务结果"；信息提示与错误 alert 分通道 |
| 分析页（扫描 8/20/17） | `heapAllocBytes` 人性化（MB）、`stageElapsedMs` 渲染为阶段耗时条；409 alert 随状态恢复清除；支持最近指标导出 JSON |
| 任务历史（扫描 28） | 任务表加状态筛选 + 终态折叠/清理入口 |
| phase 2（扫描 29） | 展示 `phase2.autoDispatch` 状态，提供手动触发入口与进度展示 |
| 死功能（删除 3.5） | `#/audit?task=` hash 入口接入应用内导航，或移除 |
| 调试信息外泄（groups 3.6/3.7） | 移除"行高估算：44px"；翻页加载中分页器保持显示上次真实页码 |

### 实施顺序建议

1. **第一批（纯前端、低风险、收益快）**：P1-1、P1-2 前端部分、P1-3、P1-5、P2-1、P2-2、P2-4、P2-5、P3 全档——不动协议和数据库，可快速消化掉报告里大部分中低级别问题。
2. **第二批（端点补充，无协议变更）**：P0-1（预览代理）、P0-4（任务表 + 列表端点）、P2-3 聚合端点、P2-6（RecycledTo 透传）。
3. **第三批（协议/语义变更）**：P0-2（代表指定）、P0-3 第二步（策略选择端点）、P1-4（取消链路）、P1-6（删除重派）。

---

## 十、参考来源

- [dupeGuru 官方文档 — Re-Prioritizing duplicates](https://dupeguru.voltaicideas.net/help/en/reprioritize.html)（参考文件重选机制）
- [dupeGuru 官方文档 — Results](https://dupeguru.voltaicideas.net/help/en/results.html)（参考文件不可标记删除的保护语义、Power Marker）
- [dupeGuru 官网](https://dupeguru.net/)（Reference Folder、Delta Values 模式）
- [Czkawka GitHub](https://github.com/qarmin/czkawka)（相似图片预览、相似视频、选择助手）
- [Linux Uprising — Czkawka 介绍](https://www.linuxuprising.com/2021/03/find-and-remove-duplicate-files-similar.html)（相似图片内联预览确认）
- [rmlint](https://github.com/sahib/rmlint)（original 判定规则、可导出的还原脚本思路）

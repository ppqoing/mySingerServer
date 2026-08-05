# V4 百万级 Web 重设计验收记录

日期：2026-07-31  
工作区：`D:\code\mySingerServer`

## 结论

- React V4 控制台已生成并嵌入 `internal/gui/web/`。
- 实际页面入口为 `/`（默认 `#/overview`）与 `/groups`（默认
  `#/groups`）；旧页面保留为 `/legacy.html` 与
  `/legacy-groups.html`。
- 浏览器验收使用只绑定 `127.0.0.1` 的内存夹具。删除演练只写入内存
  task，不调用真实 Helper、Agent 或文件系统删除。

## 自动化门禁

| 门禁 | 结果 |
| --- | --- |
| `npm --prefix webui test` | 13 files，152/152 |
| `npm --prefix webui run lint` | 通过 |
| `tsc --noEmit -p webui/tsconfig.json` | 通过 |
| `scripts/build-web.ps1` | 通过；53 modules |
| `scripts/build-web.ps1 -VerifyEmbedded` | 通过 |
| `node --test scripts/acceptance-web-fixture.test.mjs` | 13/13 |
| `node --check scripts/acceptance-web-fixture.mjs` | 通过 |
| `go test ./internal/gui ./cmd/gui -count=1` | 两个包均通过 |

最终嵌入资源：

| 资源 | 字节 | SHA-256 |
| --- | ---: | --- |
| `assets/main-BE3Mq3U5.js` | 324707 | `eac9390e74dac8b47ae0cc53b72d689ad0375e094ef68e22fad93d171ea81f50` |
| `assets/main-BQtgk0N6.css` | 15611 | `6790f987a8c59d412d67d0af6dd17cf912b6632461fefc813faa260c59bf7721` |
| `assets/aurora-surface-De-LCS6f.png` | 1622296 | `b33664e01f64d4a6cc3351964a79b2591765ae4e99efe9a75d2b8c7ef9b5253a` |
| `assets/media-placeholder-DU4hjsyh.png` | 1515482 | `49807aa8ef48398f78e15c94bf52c06a157c4099fa383e30ecb8f4a3246892ce` |

## 浏览器验收

入口：`http://127.0.0.1:4173/#/groups`  
响应标记：`X-Acceptance-Fixture: 1`

### 业务流程

- 六个工作区均可进入：总览、Agent、扫描任务、一筛分析、重复组、删除审计。
- 精确、图片、视频三类重复组分别使用独立 ID 区间。
- 列表显示总量 1,000,000，但 1440px 视口只保留 20 个组行 DOM。
- 组详情声明 1,000,000 个成员，但当前视口只保留 11 个成员 DOM。
- `min_members=1000000` 返回 4 个真实满足阈值的组。
- 代表文件与离线 Agent 文件复选框不可用；只允许明确勾选的在线非代表文件进入删除。
- 二次确认默认软删除；切换硬删除后显示不可恢复警告并把最终按钮改为
  “最终确认硬删除”。
- 内存夹具硬删除任务
  `11111111-2222-4333-8444-000000000001` 从进行中到已完成；
  审计页显示模式 `hard`、总数 1、成功 1。
- 新扫描任务从 `running` 到 `done`；一筛分析从空闲到运行中再回空闲。
- 浏览器 console warning/error：0。

浏览器验收完成后追加的删除恢复、分析确认、筛选清理、Modal
焦点顺序及后端代表文件修复没有改变布局；这些变更由最终 152 条前端测试、
13 条夹具测试、Go GUI 测试与最终嵌入构建覆盖，没有重新启动或重启验收
Fixture。

## 最终安全与百万级修复

- 筛选、类型、Agent、最少成员数与排序变化会立即清除旧详情和旧选择，
  搜索输入仍保留 300ms 防抖。
- 分析启动只有在后续状态请求成功确认后才解锁；409 会转为刷新已有任务，
  非重试错误不会被焦点/可见性事件重新触发。
- 删除结果按 `machineId + path` 映射回原始显式选择。后端的 `uncertain`
  已包含于 `failed`，因此 100 项中 99 成功、1 不确定时只保留该不确定项。
- 失败项所属 Agent 离线时保持不可选，持久化未协调快照；Agent 恢复后才
  恢复显式选择。关闭终态弹窗、离开再返回重复组页面也会自动回到原组。
- 代表文件以“同组且仍存活的成员”为准；列表、详情与删除准备使用同一
  稳定回退规则。列表先分页再查询代表文件，删除准备先按已选文件收窄候选组。
- 构建与 Fixture 同时验证 HTML 引用及 CSS `url(...)` 的本地资源闭包，
  拒绝远程、越界、缺失或空资源。
- Modal 的焦点链按 DOM 顺序建立，忙碌控件禁用、嵌套弹层与焦点恢复均有
  自动化覆盖。

### 响应式布局

| 视口 | 验收 |
| --- | --- |
| 1440×900 | 筛选/列表/详情三栏；文档无横向溢出 |
| 1280×800 | 详情仍为内联第三栏；0 个 dialog；无横向溢出 |
| 1024×768 | 详情成为 dialog 抽屉；背景 inert；body 锁滚动 |
| 1024→1280 | 嵌套完整信息保持顶层；关闭后 body 解锁，焦点回到“打开重复组 1” |
| 375×812 | 无横向溢出；隐藏批量删除入口；详情为抽屉 |

## 未计为通过的环境门禁

全仓 `go test ./... -count=1` 在 80.4 秒内完成，但不计为通过：
除 `cmd/helper` 的 3 条构建脚本测试需要 `M5_WINDRES` 或
`M5_CC`/`CC` 外，其余包（包括 77 秒集成测试）均通过。本机当前这些
变量为空，且找不到 `gcc`、`windres`。这不影响已通过的
`internal/gui` 与 `cmd/gui` Web/嵌入测试。

`npm ci` 仍报告 React Router 的 2 个 high 条目；当前前端仅使用客户端
`HashRouter`，不使用公告涉及的 unstable RSC API。本次没有用
`npm audit fix --force` 跨主要版本升级依赖。

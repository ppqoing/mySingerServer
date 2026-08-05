# 多机器媒体文件去重系统 — 架构选型与实施计划

> 版本：v1.2
> 变更（相对 v1.1）：
> ① Worker 池改为**双二进制**（`agent.exe` 不链接 mediacore.dll + `worker.exe` cgo 链接 DLL）——cgo 链接的 DLL 在进程启动时即加载，单 exe `--worker` 模式与"主进程不加载 DLL"互斥；
> ② msgpack 钉死 `github.com/vmihailenco/msgpack/v5` 且**必须 map（具名字段）编码**，协议演进只允许追加字段；
> ③ PostgreSQL Go 驱动钉死 `github.com/jackc/pgx/v5`；
> ④ `dup_groups.kind` 扩为 5 值（含 `image_candidate`/`video_candidate` 候选态），`files.status` 增 `'deleted'`（分析侧统一排除），新增 `pair_scores` 对级结果表；
> ⑤ 补充默认参数：PDQ Quality 下限 50、缩略图长边 256px、ffmpeg 单帧截图超时 60s、IPC 帧护栏 16MB、Worker 重生退避 500ms；
> ⑥ 协议追加 `StatsQuery`/`StatsReport`（M6 指标采集）；
> ⑦ 固化两条跨里程碑约定：PDQ-256 落库字节序、band 倒排召回上界；
> ⑧ 提权 Helper 改为**启动时即以管理员权限启动并常驻**（UAC 仅在启动那一刻出现一次），Agent 不再按需 runas 提权启动。
> ⑨ M1 落地决定：Everything SDK 改用 `x/sys/windows` 按需 `LoadDLL`/`FindProc`，保持相同 C ABI 与运行期回退语义，但 `agent.exe` 不再需要 cgo/MinGW；详见 [ADR-0001](adr/0001-load-everything-sdk-with-pure-go-windows-calls.md)。
> 默认决策（待确认）："云端" = 局域网自建中心存储（PostgreSQL）。如需公有云数据库，仅影响第 6、7 节局部内容。

---

## 1. 需求摘要

| 维度 | 要求 |
|---|---|
| 规模 | ≥2 台机器，单机 ≥3 块盘（HDD + SSD 混合），文件百万级 |
| 进程划分 | **GUI 独立进程**（可部署在任意机器），经 **TCP** 与多台机器的 **Agent（纯计算进程）** 通信；GUI 下发任务、做查重分析与重复统计；Agent 只负责取数与计算 |
| 一阶段（普扫全量） | 图片：SHA-512 + PDQ-256（附带产出宽/高、PDQ Quality，供剪枝）；视频：生成缩略图 + SHA-512 + 缩略图 PDQ-256（附带时长，供 ±2s 剪枝） |
| 二阶段（按需补算） | 仅对一筛（PDQ-256 汉明距离）命中的候选：图片补分区 pHash + Sobel 结构块直验；视频补 6 帧截图逐帧校验，按 6 帧平均值判定 |
| 精确去重 | SHA-512 一致判定为完全相同；所有特征数据以 SHA-512 为索引 |
| 删除 | **Helper 启动时即以管理员权限运行并常驻**；删除前校验只读属性，只读则改可写后删除 |
| 防崩溃 | 解码放入 Worker 进程池，Worker 崩溃不拖垮 Agent 主进程；Worker 对同一文件只读一次，一次流程算完本阶段所有缺失数据 |
| 文件列表 | Everything 枚举路径下全部文件，按物理磁盘落本地库 |
| 剪枝 | 扫描前比对数据库只算缺失字段；按路径判断本地缩略图是否已存在；视频时长绝对差 ≤ 2 秒剪枝；长宽比 10% 宽容度剪枝 |
| 日志 | 每个计算失败一行一条；崩溃检测单独写日志 |
| 优化 | 本地库暂存 + 定时同步中心；多进程吃满 CPU；HDD 4MB 大块读吃满 IO；SSD 多线程读吃满 IO；单文件损坏/超时/崩溃不中断整轮扫描 |

---

## 2. 选型结论（汇总）

| 层 | 选型 | 说明 |
|---|---|---|
| Agent / GUI 语言 | **Go 1.22+** | 单静态二进制部署；goroutine 调度 IO 与 TCP 连接；进程监督成熟 |
| 核心计算 | **C++17 DLL（`mediacore.dll`）**，仅 `worker.exe` 经 cgo 链接 | 解码 + 哈希 + 感知哈希全在 DLL 内，数据零拷贝 |
| 进程池 | **双二进制**：`agent.exe`（不链接 mediacore.dll）派生 **`worker.exe`（cgo 链接 DLL）** 子进程 | cgo 链接的 DLL 进程启动即加载，故必须独立 exe；解码崩溃只杀死 worker.exe，agent.exe 检测退出码、写日志、重生 |
| 图片解码 | libjpeg-turbo + libpng + libwebp + stb_image（内存缓冲解码） | 文件字节只读一次，DLL 从内存解码 |
| 视频解码 | ffmpeg/ffprobe CLI 子进程（带超时）；可选编译 libav 进 worker.exe | 缩略图与 6 帧截图均走子进程，天然崩溃隔离 |
| 哈希 | DLL 内 SHA-512（流式 4MB 块） | 与 HDD 读块对齐 |
| PDQ-256 | 移植 facebook ThreatExchange **PDQ C++ 实现**（含 Quality 指标） | 一、二阶段共用；落库字节序见 §5.1 约定 |
| pHash（分区）/ Sobel | DLL 内自实现，复用同一 u8 灰度面 | 仅二阶段按需调用 |
| 文件枚举 | **Everything SDK**（`Everything64.dll`，`x/sys/windows` 动态调用，按需 LoadLibrary），目标机需运行 Everything | 百万级秒级枚举；DLL 缺失/IPC 失败可回退 Walker |
| 物理磁盘映射 | 盘符/挂载点 → Volume GUID → `IOCTL_STORAGE_GET_DEVICE_NUMBER` | 任务按物理盘分队列；disk_no 仅作当轮调度分桶键，非稳定盘身份 |
| Agent 本地库 | **SQLite**（`modernc.org/sqlite`，纯 Go 实现） | WAL 模式，特征暂存 + 缺失字段剪枝 |
| 中心库 | **PostgreSQL 16**，Go 驱动钉死 **`pgx/v5`** | 各 Agent 定时上行汇总，GUI 分析的数据底座（"云端"） |
| 通信 | **自研 TCP 协议：长度前缀 + msgpack 帧**，GUI ↔ 各 Agent 直连，带心跳/断线重连/断点续传；msgpack 钉死 **`vmihailenco/msgpack/v5`，map（具名字段）编码** | 轻量、无代理；向后兼容仅靠追加字段；gRPC 为可替换选项 |
| 删除组件 | 各机器独立 **提权 Helper**（manifest `requireAdministrator`，**启动时即以管理员权限启动并常驻**），命名管道收清单 | 与计算进程权限隔离；UAC 仅在 Helper 启动时出现一次 |
| 日志 | Go `slog` JSON 行格式 + lumberjack 滚动；`errors.log` 一行一报错；`crash.log` 记崩溃 | — |

### 语言对比结论

- **Python**：算法现成但 GIL 下多进程编排笨重、部署需带解释器；已按你的决定排除。
- **.NET 8**：Windows 集成好但 PDQ-256 需移植，与 Go+C++ 方案叠加只增复杂度；排除。
- **Go + C++ DLL（选定）**：Go 管 IO 调度、TCP、进程监督、DB；C++ DLL 管解码与哈希。崩溃面全部收敛进可牺牲的 worker.exe 子进程，agent.exe 主进程不链接 mediacore.dll、零解码。

---

## 3. 总体架构

```
        ┌─────────────────────────────────────────────────────┐
        │ GUI 独立进程（任意一台机器，Go 单二进制 + Web 页面） │
        │  ├ TCP 客户端：同时直连 Agent A/B/N（心跳、重连）    │
        │  ├ 任务编排：普扫任务 → 一筛 → 按需下发二阶段任务    │
        │  ├ 查重分析：精确组 / 图片三级漏斗 / 视频判定        │
        │  └ 分析数据源：PostgreSQL（各 Agent 定时上行）       │
        └───┬───────────────┬───────────────┬─────────────────┘
            │ TCP（自研协议，长度前缀+msgpack，map 编码）
            ▼               ▼               ▼
   ┌───────────────┐ ┌───────────────┐ ┌───────────────┐
   │ Agent A（纯计算）│ Agent B       │ Agent N ...     │
   │ agent.exe：     │ │ （同构）       │                 │
   │  ├ Everything枚举│ │               │                 │
   │  ├ 盘级IO调度   │ │               │                 │
   │  ├ Worker池监督 │ │               │                 │
   │  ├ SQLite暂存   │ │               │                 │
   │  └ 定时上行中心库│ │               │                 │
   │  worker.exe×N(cgo+mediacore.dll)                    │
   │  ffmpeg子进程(视频)                │                 │
   │  提权Helper(常驻,启动即管理员)      │                 │
   └───────┬───────┘ └───────┬───────┘ └───────┬───────┘
           └────────┬────────┴────────┬────────┘
                    ▼ 定时批量上行（5min/5万行）
           ┌─────────────────┐
           │ PostgreSQL 中心库│ ◀── GUI 分析读取
           └─────────────────┘
```

职责边界：

- **Agent 只做计算**：枚举文件、读盘、算特征、存本地、上行。不做任何查重判定、不感知其他机器。
- **GUI 进程做全部"脑力活"**：下发扫描范围、收/读特征、一筛、按需生成二阶段任务再下发、出重复组、发起删除。
- agent.exe 不链接 mediacore.dll、不解码（Everything SDK 通过纯 Go Windows API 按需加载）→ 坏文件最多杀死 worker.exe，整轮扫描不中断。

---

## 4. Agent 内部设计

### 4.1 进程模型

```
agent.exe（主进程，不链接 mediacore.dll）
 ├── TCP 服务端：收 GUI 任务（扫描范围 + 阶段 + 字段位掩码），回进度与结果
 ├── 枚举器：x/sys/windows 动态调用 Everything SDK（按需 LoadLibrary）
 ├── 调度器：每物理盘一个 IO 队列 + Worker 池分发（背压限流）
 ├── worker.exe×N（N = CPU 核数）：独立二进制，cgo 链接 mediacore.dll
 ├── 同步器：定时批量上行中心库（独立周期任务，不占用计算链路）
 └── 删除 Helper：独立提权 exe，启动时即管理员权限并常驻，命名管道收清单
```

- 必须双二进制的原因：cgo 静态链接的 DLL 在进程启动时即加载，"单 exe `--worker` 模式"会让主进程也加载 mediacore.dll，破坏崩溃隔离。两者同仓构建但参数分离：`agent.exe` 使用 `CGO_ENABLED=0`，只有 `worker.exe` 使用 `CGO_ENABLED=1` 并 import mediacore DLL 绑定。
- Worker 与主进程：**命名管道 + 长度前缀 msgpack**（map 编码，与 TCP 协议同一套帧编解码）。
- 崩溃监督：管道 EOF / 非零退出码 / 心跳超时 → `crash.log` 写一行（时间、PID、正在处理的文件、退出码）→ 文件标记 `status=crash` → 退避 500ms 后重生 worker.exe，池子不断流。
- 单文件看门狗：图片 30s / 视频 120s（可配）超时 → kill 该 worker.exe 按崩溃处理。

### 4.2 Worker"只读一次"流水线（两阶段统一）

任务 = 文件路径 + **阶段** + 缺失字段位掩码。worker.exe 一次流程算完本阶段全部缺失字段：

**一阶段任务（普扫）**
1. 4MB 块流式读文件，边读边算 SHA-512；图片文件（≤内存阈值 256MB）字节驻留内存。
2. 查本地库（经 IPC 向主进程发 `sha_query`/`sha_reply`）：此 SHA-512 是否已有本阶段特征？有 → 直接复用返回（**single-flight，同 SHA 只解码一次**；owner 崩溃时等待者重试抢 owner）。
3. **图片**：内存字节交 DLL 解码 → u8 灰度面（BT.601）→ PDQ-256 + PDQ Quality；记录宽/高（长宽比剪枝用）。
4. **视频**：ffprobe 取时长（超时 15s）；ffmpeg 子进程取中点帧生成缩略图（长边 256px，灰度）→ DLL 算缩略图 PDQ-256；缩略图按 `sha1(path)` 落本地缓存（存在且源 mtime 未变则跳过生成）。
5. 结果一次回传：`{path, sha512, pdq256, quality, w, h | duration_ms, thumb_path, thumb_pdq256, error?}`。

**二阶段任务（一筛命中后按需下发）**
1. 图片：重新定位文件 → 读一次 → 解码 → 同一 u8 灰度面依次产出 **分区 pHash** 与 **Sobel 结构块直方图**（同 SHA 已有则 single-flight 复用；灰度面为 u8，跨里程碑不得改为 float 面）。
2. 视频：按时长均分 6 个时间点 (1/12,3/12,...,11/12) 逐帧截图 → 每帧走图片二阶段流程（PDQ-256 存档 + 分区 pHash + Sobel）。
3. 失败只标记当前字段，其余字段照常回传；错误随结果上报，主进程一行一条写 `errors.log`。

### 4.3 盘级 IO 调度

| 盘类型 | 识别 | 并发读流 | 读块 | 目标 |
|---|---|---|---|---|
| HDD | `IOCTL_STORAGE_GET_DEVICE_NUMBER` + 寻道惩罚属性 | 1~2 条顺序流/盘 | **4MB** | 避免磁头抖动，吃满顺序带宽 |
| SSD | 同上 | 4~8 条流/盘 | 1MB | 吃满随机 IOPS |

- 枚举结果按物理盘号分桶；调度器按目录序取任务（同目录物理邻近）。disk_no（DeviceNumber）跨重启可能重排，仅作当轮调度分桶键，剪枝只认 size+mtime+sha512。
- 背压：待算字节超阈值暂停读盘，防内存膨胀（与 256MB 驻留阈值 × Worker 数联动，联合约束在 M6 压测回填）。

### 4.4 剪枝（每轮任务执行前，Agent 本地完成）

1. 文件列表联查本地 SQLite 生成字段级缺失位掩码：size+mtime 未变且 sha512 已有 → 跳过哈希；本阶段特征已齐 → 整文件跳过。
2. 缩略图：按 `sha1(path)` 查缓存目录，存在且源 mtime 未变 → 跳过生成，只取已有缩略图补算 PDQ。
3. 结构性剪枝（GUI 分析侧）：图片长宽比差 >10% 的直接不成对；视频 |时长差| >2s 的直接不成对 → 二阶段任务量再砍一轮。

---

## 5. 查重判定流程（GUI 进程，两阶段）

### 5.1 阶段一：普扫 + 一筛

```
GUI 下发普扫任务 → 各 Agent 全量计算 → 特征上行 PostgreSQL
  │
  ├ 精确去重：按 SHA-512 分组 → 精确重复组（跨机器/跨盘路径列表）
  │
  ├ 图片一筛：PDQ-256 按 64bit 分段 band 倒排出候选对（避免 O(n²)）
  │    剪枝：汉明距离 ≤ T1(默认31)；PDQ Quality 双达标(≥50)；长宽比差 ≤10%
  │
  └ 视频一筛：|时长差| ≤2s 后，缩略图 PDQ-256 汉明距离 ≤ T1
```

**跨里程碑约定（冻结）**：

- **PDQ-256 落库字节序**：32B blob 的字节序 = 官方 hex 字符串顺序（`w[15]→w[0]`，每词 `%04hx`）。M2 落库、M3 一筛解码、M4 复筛全链路统一，集成测试必须覆盖（字节序错位会导致候选爆炸）。
- **band 倒排召回上界**：4×64bit 分段在数学上仅保证汉明 ≤3 的对 100% 召回；差异分散到 4 段的 4~31 对可能漏检。T1=31 只是过滤器。若业务对漏检敏感，M6 可增加第二套错位 band 布局（可选增强，接口不变）。

### 5.2 阶段二：按需补算 + 复筛

```
一筛候选（图片对/视频对，通常 <1%）
  │ GUI 汇总候选涉及的唯一 SHA-512 集合 → 生成二阶段任务
  │ → 按 machine_id 分发（同 SHA 多副本时任选在线机器的副本，离线换副本重发）
  ▼
图片：分区 pHash 逐区比对，通过区数比例 ≥ T2(默认80%)
  → Sobel 结构块直方图直验，相关度 ≥ T3(默认0.85)
  → 相似图片组（并查集合并）
视频：6 帧逐对走图片复筛流程得 6 个相似度
  → 平均值 ≥ T4(默认0.8)（兜底：≥4/6 帧通过；有效帧 <4 判 inconclusive）
  → 相似视频组
```

- 二阶段任务量由候选规模决定，百万级全量只做便宜的一阶段特征——这是本方案的核心成本控制。
- 特征以 SHA-512 为主键：同内容 N 个副本只存一份特征、只解码一次。
- 视频帧的 PDQ-256 入库存档但不参与复筛判定主链路（避免一筛阈值 T1 被隐式二次引入）；如需用帧 PDQ 做轻筛加速，另行决策。

### 5.3 输出与删除

- GUI 展示三类组：精确重复组 / 相似图片组 / 相似视频组（含各级分数明细）。
- 用户勾选 → 二次确认 → GUI 经 TCP 把删除清单发到对应 Agent → Agent 转命名管道给**常驻提权 Helper** → 逐项：查只读 → 置可写 → 删除 → 回执经 Agent 返回 GUI 并写 `delete.log` 审计。可选"移入回收目录"软删模式。
- Helper 不可达时任务整体失败并返回明确错误（提示以管理员权限启动 helper.exe），Agent 不做提权启动。

---

## 6. 数据模型

### 6.1 Agent 本地 SQLite

```sql
files(id, machine_id, disk_no, path, size, mtime, sha512,
      phase1_done,       -- sha/pdq/thumb 是否齐
      phase2_done,       -- phash/sobel/6帧 是否齐
      status,            -- pending/done/partial/failed/crash/deleted
      missing_mask, error, updated_at)
image_features(sha512 PK, width, height,
               pdq256 BLOB(32), pdq_quality,           -- 一阶段
               phash_parts BLOB, sobel_hist BLOB)      -- 二阶段(可空)
video_features(sha512 PK, duration_ms,
               thumb_path, thumb_pdq256 BLOB(32), thumb_quality)  -- 一阶段
video_frames(sha512, frame_idx, pdq256 BLOB,
             phash_parts BLOB, sobel_hist BLOB,        -- 二阶段(可空)
             PK(sha512, frame_idx))
sync_queue(table_name, row_pk, synced)
```

- `status='deleted'` 由删除组件写入；M3/M4 分析侧与 GUI 查询**统一排除**该状态，防止已删文件回流进重复组。

### 6.2 中心 PostgreSQL

同构表 + `machine_id` 维度；结果表：

```sql
dup_groups(id, kind,            -- exact/image/video(确认组, M4)
           representative_file_id, member_count, created_at)
           --  + image_candidate/video_candidate(一筛候选, M3)；kind 不加三值 CHECK
dup_members(group_id, file_id, score_json)   -- 各级分数明细
pair_scores(id, kind, sha_a, sha_b,          -- M4 复筛对级结果（新增表）
            phase2_json, verdict, created_at,
            UNIQUE(kind, sha_a, sha_b))      -- GUI 重启恢复/组全量重建依据；M4 消费候选一律用内容键 (kind,sha_a,sha_b)，禁止持久引用 dup_groups.id
scan_tasks(id, phase, target, status, stats_json, created_at)
```

- M3 每轮"整类 DELETE + 事务重写"候选，`dup_groups.id` 不稳定；跨里程碑只认内容键。

### 6.3 同步策略

- Agent 每 5 分钟或积压 5 万行批量上行；失败留 `sync_queue` 重发；以 `(machine_id, path)` / `sha512` 自然键 `ON CONFLICT UPDATE` 幂等。
- GUI 分析以中心库为准；二阶段结果上行后增量触发复筛。

---

## 7. TCP 通信协议（GUI ↔ Agent 直连）

- 帧格式：`[4B 大端长度][msgpack body]`，帧护栏 16MB；连接建立后双向心跳（默认 15s），断线指数退避重连，任务级 ACK + 断点续传（Agent 任务不随 GUI 断线取消，重连后同 task_id 重发任务触发重绑回传通道）。
- **编码约定（冻结）**：msgpack 必须 map（具名字段）编码（`vmihailenco/msgpack/v5` 默认即 map），协议演进只允许**追加字段**，禁止数组编码/改序/复用字段名。
- 消息类型：

```
GUI → Agent : ScanTask{task_id, roots[], phase, options}      // 下发普扫/二阶段任务
              Phase2Task{task_id, items[]{path, fields_mask}} // 按需补算
              DeleteTask{task_id, entries[]{path}}            // 转常驻提权 Helper
              StatsQuery{}                                    // M6 指标采集（只读）
              Ping / ConfigPush
Agent → GUI : TaskProgress{task_id, done, total, speed}
              FeatureResult{task_id, path, sha512, ...}       // 批量流式回传
              TaskDone{task_id, stats} / Error{path, stage, msg}
              CrashNotice{pid, path, exit_code}               // 同步写入 crash.log
              StatsReport{disks[], cpu, workers, ...}         // M6 指标采集
              Pong / DeleteReport{entries[]{path, ok, err}}
```

- GUI 同时维护到多台 Agent 的连接池，任务按文件所在机器路由（二阶段同 SHA 多副本时任选在线副本，见 §5.2）。
- 备选：如需跨语言扩展或公网穿透，可平替为 gRPC over TCP，消息语义不变。

---

## 8. 日志规范

| 日志（Agent 侧） | 内容 | 格式 |
|---|---|---|
| `agent.log` | 启动、任务收发、进度、同步 | JSON 一行一条 |
| `errors.log` | 每文件每失败字段一行：`{ts, path, stage, err}` | 一行一报错 |
| `crash.log` | worker.exe/ffmpeg 崩溃：`{ts, pid, file, exit_code}` | 一行一次 |
| `delete.log` | 删除动作与回执（审计） | 一行一次 |

坏文件原则：损坏图片/视频、解码失败、超时、Worker 崩溃只影响当前文件当前字段，DB 标失败可下轮补算，整轮扫描不中断。GUI 侧聚合各 Agent 的错误统计展示。

---

## 9. 默认参数表（全部可配）

| 参数 | 默认值 | 参数 | 默认值 |
|---|---|---|---|
| HDD 读块 | 4MB | PDQ 汉明阈值 T1 | 31 |
| HDD 并发流/盘 | 2 | PDQ Quality 下限 | 50（一筛双达标线，防全黑退化簇） |
| SSD 并发流/盘 | 6 | 长宽比宽容度 | 10% |
| Worker 数 | CPU 核数 | pHash 分区 | 3×3，通过比例 T2=80% |
| Worker 重生退避 | 500ms | Sobel 直验阈值 T3 | 0.85 |
| 图片内存驻留阈值 | 256MB | 视频时长差剪枝 | 2s |
| 图片单文件超时 | 30s | 视频缩略图 | 中点帧，灰度，长边 256px |
| 视频单文件超时 | 120s | 视频二阶段抽帧 | 6 帧均分，平均阈值 T4=0.8 |
| ffmpeg 单帧截图超时 | 60s | 视频有效帧下限 | 4（低于则判 inconclusive） |
| IPC 帧护栏 | 16MB | TCP 心跳 | 15s |
| 同步周期 | 5min / 5万行 | 缩略图缓存键 | sha1(path)+mtime |

---

## 10. 里程碑计划

| 阶段 | 交付物 | 验收标准 |
|---|---|---|
| **M1 骨架**（1~2 周） | agent.exe：TCP 服务端、Everything 枚举、盘号映射、SQLite、SHA-512；GUI 独立进程：TCP 连接池、任务下发、精确重复组展示；PostgreSQL 上行 | 双机普扫，精确重复组在 GUI 正确汇总 |
| **M2 一阶段特征**（2 周） | `mediacore.dll`：内存解码 + PDQ-256；worker.exe 双二进制进程池 + 崩溃重生 + 看门狗；视频缩略图管线（ffprobe 时长 + 中点帧 + 缩略图缓存） | 投喂损坏文件主进程存活、crash.log 有记录；同 SHA 只解码一次；缩略图按路径命中缓存 |
| **M3 一筛分析**（1 周） | GUI 侧 band 倒排候选生成、长宽比/质量/时长 ±2s 剪枝 | 百万级特征一筛秒级出候选 |
| **M4 二阶段**（1~2 周） | 分区 pHash + Sobel（复用灰度面）；视频 6 帧按需任务；GUI 复筛与三组展示 | 一筛命中后自动下发补算，相似组分数明细可见 |
| **M5 删除**（1 周） | 常驻提权 Helper（启动即管理员）、只读处理、删除回执与审计 | 勾选删除端到端走通，只读文件可删 |
| **M6 调优与压测**（1~2 周） | HDD/SSD 调度调优、同步压测、百万文件浸泡测试 | HDD 顺序带宽 ≥80%；CPU ≥85%；全量扫描零主进程崩溃 |

各里程碑的详细实施文档见 `docs/details/M*.md`，总清单见 `docs/todolist.md`。

## 11. 风险与缓解

| 风险 | 缓解 |
|---|---|
| Everything 未运行/索引不全 | Agent 启动探测 SDK，失败回退常规遍历并告警 |
| worker.exe 内 cgo 不可恢复崩溃 | worker 是独立进程，最坏=该 worker 死亡重生；agent.exe 不链接 mediacore.dll |
| PDQ C++ 移植质量 | 用官方测试向量回归校验（参考实现字节数组喂入须位精确）后再接入 |
| band 倒排固有漏检（§5.1） | T1=31 为过滤器可接受；召回敏感时 M6 加第二套错位 band 布局 |
| 二阶段图片需二次读盘（普扫没留字节） | 可接受：候选集 <1%；且候选常集中在少数目录，盘级调度按目录序批量取 |
| 大视频哈希一遍、ffmpeg 一遍 | 接受：4MB 顺序读成本低；截图错峰调度 |
| 百万级一筛内存占用 | band 倒排分片；GUI 所在机内存建议 ≥16GB |
| Helper 常驻扩大提权窗口 | 管道 SDDL 仅本机指定用户可连；白名单只读本地配置 + NTFS ACL 仅管理员可写；只执行 GUI 二次确认清单 |
| Session 0 无法弹 UAC（Agent/Helper 服务化时） | 用"以最高权限运行"的计划任务开机预启动 Helper 规避；Helper 启动时一次性 UAC |
| 删除误操作 | Helper 只执行二次确认清单；删除前审计日志；可选软删模式 |

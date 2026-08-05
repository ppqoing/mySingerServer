# 实施 TODO List

> 依据：`docs/architecture-plan.md` v1.2。每个条目的详细实施文档见 `docs/details/` 对应文件。
> 状态标记：`[ ]` 未开始 / `[~]` 进行中 / `[x]` 完成。

| # | 任务 | 目标（完成标志） | 详细文档 | 依赖 | 状态 |
|---|---|---|---|---|---|
| M1 | 骨架：Agent TCP 服务端 + Everything 枚举 + SQLite + SHA-512 + 上行同步 + GUI 独立进程骨架 | 本机双 Agent 验收完成；第二台独立 Windows 验收由项目所有者范围豁免 | [M1-skeleton.md](details/M1-skeleton.md) | 无 | [x] |
| M2 | 一阶段特征：mediacore.dll（内存解码 + PDQ-256）+ Worker 进程池 + 视频缩略图管线 | 坏文件不崩主进程；同 SHA 只解码一次；缩略图命中缓存 | [M2-phase1-features.md](details/M2-phase1-features.md) | M1 | [x] |
| M3 | 一筛分析：band 倒排候选生成 + 长宽比/质量/时长 ±2s 剪枝 | 百万级特征一筛秒级出候选对 | [M3-first-screen.md](details/M3-first-screen.md) | M2 | [x] |
| M4 | 二阶段：分区 pHash + Sobel（复用灰度面）+ 视频 6 帧按需补算 + 复筛成组 | 一筛命中自动下发补算，相似组分数明细可见 | [M4-phase2.md](details/M4-phase2.md) | M3 | [x] |
| M5 | 删除：提权 Helper + 只读处理 + 删除回执审计 | 勾选删除端到端走通，只读文件可删 | [M5-delete.md](details/M5-delete.md) | M1（M4 后体验完整） | [x] |
| M6 | 调优与压测：IO 调度调优 + 同步压测 + 百万文件浸泡测试 | HDD 带宽 ≥80%、CPU ≥85%、全量扫描零主进程崩溃 | [M6-tuning.md](details/M6-tuning.md) | M2~M5 | [x] |

> M6 于 2026-07-30 由项目所有者按实际证据验收关闭，状态为
> `M6_COMPLETE_OWNER_ACCEPTED`。最终长测实际运行 21 小时 38 分 57.880 秒，
> 项目所有者确认时长足够并按完成处理。该关闭决定接受未单独取得
> HDD 利用率与 CPU ≥85% 正式测量证据的风险，不应解读为这些未测指标已
> 技术性 PASS。完整边界见
> [M6 验收记录](acceptance/2026-07-29-m6.md)。

## 里程碑依赖关系

```
M1 ──▶ M2 ──▶ M3 ──▶ M4 ──┐
 └──────────▶ M5 ─────────┴──▶ M6
```

- M5 与 M2~M4 可并行开发（仅依赖 M1 的 TCP 协议）。
- 每个里程碑完成后需通过其详细文档中的全部验收用例，再勾选上表状态。

## 详细文档通用约定

每份 `docs/details/M*.md` 均包含以下章节，与架构计划 v1.2 的选型、参数、协议语义保持一致：

1. 目标与范围（含"不做什么"）
2. 任务分解（可直接勾选的 checklist）
3. 目录与文件结构
4. 关键接口与结构体定义（Go / C++ / SQL / msgpack 消息）
5. 数据模型与配置项
6. 测试与验收用例
7. 风险与注意事项

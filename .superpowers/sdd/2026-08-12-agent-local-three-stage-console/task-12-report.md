# Task 12 实现报告：本机结果和 deleted 状态异步同步 PostgreSQL

## 实现结果

- `central.sql` 新增独立本机 scope 表：analysis run、pair score、group/member、task event、review decision、delete result；主键均包含 machine/run/generation 或稳定事件身份。
- SQLite 新增 `local_outbox` sequence loader、分析/审核/删除快照加载和 generation 绑定 ack。
- 同步器在同一个 PostgreSQL 事务中提交普通 files/features 队列与本机 outbox 快照；远端 commit 成功后才分别 ack 两类本地队列。
- 远端 commit 成功但本地 ack 失败时安全重放；所有远端写入均使用幂等 UPSERT。
- `PublishLocalAnalysis` 在切换 current 的同一 SQLite 事务写 `local.analysis.published` outbox，关闭 publish 后、任务 checkpoint 前崩溃导致中心状态永久 building 的窗口。
- deleted 事件仅来源于 Task 11 明确成功项；远端按 machine/path/SHA 更新 `files.status=deleted`，不清空 SHA，不删除图片/视频特征或本机历史。
- PostgreSQL 配置/连接不可用不影响 Agent 监听和本机业务；既有后台 syncer 健康状态继续报告 degraded 并周期重试。

## RED → GREEN

- Store RED：缺失 outbox sequence loader、snapshot 和 ack API；GREEN 后真实 SQLite 覆盖 run/group/member/review/delete 快照、稳定 sequence、generation 防陈旧 ack 和历史保留。
- Syncer RED：远端 commit 失败后 local outbox 保留，恢复后仍无法提交/ack；GREEN 后和文件队列同事务，恢复提交一次，重复轮询不重发。
- Publish RED：发布 current 后没有原子 published 事件，注入 publish 失败边界无法证明 outbox 回滚；GREEN 后成功恰好一条、失败零条。
- PostgreSQL 合同：新增本机 scope/删除保留集成用例，重放两次只保留一份，files 变 deleted 时 SHA 和 image feature 仍存在。

## 验证结果

- `go test -count=1 ./internal/store ./internal/syncer ./cmd/agent`：PASS。
- `go test -race -count=1 ./internal/store ./internal/syncer ./cmd/agent`：PASS。
- 聚焦 `LocalOutbox|LocalScope|OfflineRecovery|DeletedRemoteRetention|IdempotentReplay|Postgres`：PASS。
- 当前环境未设置 `TEST_POSTGRES_DSN`；真实 PostgreSQL 集成明确 SKIP，而不是伪报 PASS。提供测试库后由 Task 14 执行动态验收。

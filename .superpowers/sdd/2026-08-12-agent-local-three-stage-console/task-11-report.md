# Task 11 实现报告：审核绑定删除与哈希保留

## 范围

- 新增 Agent 本机 `local.delete.prepare`、`local.delete.execute`、`local.delete.status` Socket 业务链。
- 删除请求只携带审核 run/group 或 prepare 返回的 batch/digest/token，不接受客户端路径。
- 复用现有 Helper 删除管道执行物理删除；本机审核删除使用只返回报告、不提前更新 SQLite 的执行入口。
- SQLite 以单事务记录删除批次和逐文件结果；只有 `OK=true && Uncertain=false` 的结果会把文件标记为 `deleted` 并写同步队列和 `local.delete` outbox。

## RED → GREEN 证据

- 协议 RED：缺失删除 DTO 与严格解码，`internal/proto` 编译失败；GREEN 后严格拒绝额外 `path`。
- Store RED：缺失 `LoadCommittedDeletion`、`CommitDeletionResults` 和 `LoadDeletionBatch`；GREEN 后覆盖已提交审核选择、部分成功、uncertain、outbox 注入回滚和数据保留。
- Service RED：缺失两阶段服务、一次性 token、选择摘要与文件身份复核；GREEN 后覆盖 token 重放、过期、重启失效、审核 generation 变化和文件内容变化。
- Helper RED：旧 Forwarder 把 `OK=true,Uncertain=true` 当成功；GREEN 后仅明确成功落状态。
- Socket/组合 RED：缺删除 handler 和生产聚合路由；GREEN 后三个操作接入 Agent，且复用既有 loopback NodeTray 认证边界。
- 一筛 RED：PostgreSQL 文件流和 SHA 集合包含 deleted；GREEN 后默认候选排除 deleted。

## 关键不变量

- Prepare 只读取当前已发布 generation、完整已提交审核、含至少一个 keep、且 exact/duplicate 组中的 active delete 成员。
- 执行前再次核验 machine/run/group/generation、选择摘要、文件 ID/path/SHA-512/size/mtime，并实际重读文件计算 SHA-512。
- token 仅保存在 Agent 进程内，按 batch 一次性消费；重放、过期或 Agent 重启后均失效。
- Helper 断连、超时、缺失报告或 uncertain 形成失败/不确定审计，不修改文件状态。
- 删除事务不对 SHA、图片/视频特征、视频帧、pair score、group/member、review 执行删除或置空。
- current/active 查询排除 deleted；历史查询和 SHA 索引仍可关联 deleted 行。

## 验证

- 聚焦合同：`go test -count=1 ./internal/localdelete ./internal/agent/delete ./internal/agent ./internal/store ./internal/firstscreen ./cmd/agent -run 'DeletePrepare|DeleteExecute|DeletedRetention|Uncertain|PartialDelete|DeletedExcluded|ForwardsDelete|LocalDelete'`
- 完整包：`go test -count=1 ./internal/proto ./internal/localdelete ./internal/agent/delete ./internal/agent ./internal/store ./internal/firstscreen ./cmd/agent`
- Race：`go test -race -count=1 ./internal/localdelete ./internal/agent/delete ./internal/agent ./internal/store ./cmd/agent`
- 静态保留：Task 11 生产路径没有针对特征/分数/成员表的 DELETE 或 `sha512=NULL`。
- PostgreSQL 动态同步属于 Task 12；NodeTray 页面属于 Task 13，本任务不提前实现。

### Task 4：集中缓存完整性、真实批量查询和缺失掩码

**目标：** SQLite 缓存查询只读取当前文件与特征事实；1,000 项查询使用固定数量的批量 `SELECT`，不随项数增长。基础计算和二筛共用一个结构完整性分类器，只计算真正缺失的字段，合法全零特征仍是命中。

**Files:**

- Create: `crates/node-store/src/cache_integrity.rs`
- Modify: `crates/node-store/src/lib.rs`
- Modify: `crates/node-store/src/rows.rs`
- Modify: `crates/node-store/src/content.rs`
- Modify: `crates/node-store/src/features.rs`（只抽取现有固定长度、有限浮点、六槽规则）
- Modify: `crates/node-engine/src/contact_sheet_cache.rs`
- Modify: `crates/node-engine/src/scan/base_compute.rs`
- Modify: `crates/node-engine/src/analysis/phase2.rs`
- Modify: `crates/node-store/tests/content_cache.rs`
- Modify: `crates/node-engine/tests/base_compute_pipeline.rs`
- Modify: `crates/node-engine/tests/local_analysis.rs`
- Modify: `crates/node-engine/tests/worker_pipeline.rs`（仅联系表损坏边界）

**接口：**

```rust
/// SQLite 缓存字段通过结构校验后得到的缺失描述。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CacheCompleteness {
    /// 基础计算仍缺少的协议位。
    pub base_missing_parts: u32,
    /// 图片二筛是否缺少任一必需字段。
    pub image_stage2_missing: bool,
    /// 视频二筛缺失槽位的六位掩码。
    pub video_stage2_missing_slots: u8,
}

/// 依据结构而不是字段内容数值判断缓存是否完整。
pub fn classify_cache_completeness(
    record: &BaseCacheRecord,
    contact_sheet_valid: bool,
) -> CacheCompleteness;
```

**绑定约束：**

- `BaseCacheRecord` 必须携带图片二筛结构状态和视频逐槽位二筛状态；不能以 `Option<CompleteStage2>` 隐藏缺失槽位。
- `lookup_base_cache_by_paths` 与 `lookup_base_cache_by_keys` 是批量原始载荷的唯一入口；保留输入顺序、重复项和文件大小区分。禁止循环调用单项 `load_complete_stage2` 伪装批量查询。
- 1,000 项 SQL trace 的 `SELECT` 数量必须是固定常数，且查询前后没有任务表 `INSERT/UPDATE/DELETE`。本任务不写任务文件、不调整调度器。
- 完整图片一筛要求合法非零尺寸、固定 32 字节 PDQ 和有限 Quality；图片二筛要求 72 字节 pHash 与 128 个有限 Sobel。
- 完整视频要求合法非零尺寸和时长、严格六槽、至少四个成功且完整的一筛槽。二筛掩码只覆盖一筛成功但二筛缺失的槽位。
- `[0; 32]`、`[0; 9]` 和 `[0.0; 128]` 均可为合法计算结果；不得用全零、空字符串默认值或业务数值判断失败占位。
- `base_complete=false`、NULL、空/错误长度 BLOB、非有限浮点、非法尺寸、槽位缺失和明确失败状态才返回缺失；非法单项不得令整批查询失败。
- 本机视频联系表命中必须同时满足：SQLite 相对路径等于该 MD5 派生路径、最终路径位于联系表根内、文件能解码为 JPEG。仅 `is_file()` 不算命中。
- 远端基础缓存导入不要求远端提供本机联系表；保留一个命名明确的薄适配器，在远端完整度比较时只检查可导入字段，不伪称已验证本机 artifact。
- Worker 失败不得写任何图片/视频特征占位；已有有效字段不得被部分结果覆盖。后续任务再次查询时仍得到真实缺失掩码。保留现有 `file_faults` 和运行任务最近失败，不新增故障管理协议。
- 基础缺失位继续使用既有 `BASE_MISSING_PROBE`、`BASE_MISSING_STAGE1`、`BASE_MISSING_CONTACT_SHEET`；不得修改 Protobuf 编号或协议版本。

- [ ] **Step 1：写真实字段矩阵 RED**

  在真实 SQLite 中覆盖完整图片、合法全零图片、NULL、空/错误长度 BLOB、零尺寸、`base_complete=false`、完整视频六槽、缺槽、`decoded=false`、完整/部分图片二筛和视频逐槽二筛。先运行并保存旧实现失败证据。

- [ ] **Step 2：写真实批量 SQL RED**

  同时向 path/key 两个入口传入 1,000 项（含重复路径、相同 MD5 不同大小），用 SQLite trace 断言查询次数不随项数增长、没有任务 DML、输出顺序和重复项与输入一致，并能返回二筛逐槽信息。

- [ ] **Step 3：写联系表和失败边界 RED**

  用损坏 JPEG、错误 MD5 派生相对路径和有效 JPEG 验证本机联系表判定。再验证 Worker 失败后特征仍缺失、已有有效字段未覆盖，新一轮分类仍只请求缺失位。

- [ ] **Step 4：实现最小集中分类器和批量载荷**

  新增 `cache_integrity.rs`，复用 `features.rs` 的固定长度、有限浮点、六槽和最少成功帧规则；批量 SQL 一次取得基础、一筛、图片二筛和视频逐槽二筛原始结构。对非法字段记录结构缺失，不把整批提升为查询错误。

- [ ] **Step 5：删除重复判断并接入真实消费方**

  `BaseComputeDecision::for_cache` 只把 `CacheCompleteness` 转成现有 Worker 掩码；`analysis::phase2` 只消费图片布尔和视频六位缺失掩码。完整缓存不得启动 Worker，部分缓存只派真实缺失部分。

- [ ] **Step 6：运行 GREEN 和回归**

  ```powershell
  cargo test -p dedup-node-store --test content_cache --locked -- --test-threads=1
  cargo test -p dedup-node-store --locked -- --test-threads=1
  cargo test -p dedup-node-engine --features test-hooks --test base_compute_pipeline --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test local_analysis --locked -- --test-threads=1
  cargo test -p dedup-node-engine --test worker_pipeline --locked -- --test-threads=1
  cargo fmt --all -- --check
  git diff --check
  ```

  所有 Cargo 命令使用 `C:\tmp\rust-v2-core-scope-target`，关闭 incremental/debug info并清除继承的 MinGW 编译变量。重型测试前检查 C、D 可用空间；仅当低于 10 GiB 时清理本计划精确可再生的 target。

- [ ] **Step 7：提交和报告**

  提交产品、真实行为测试和中文 Task 4 报告；不得运行真实媒体、打包、部署或触碰 `I:\Tool`。

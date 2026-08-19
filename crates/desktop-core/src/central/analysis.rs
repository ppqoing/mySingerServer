//! 中心分析运行、不可变输入、候选、重复组和复核标记的数据访问。

use dedup_core::{
    AnalysisRunId, ContentKey, LocationKey, MachineId, NormalizedPath, TaskId, Thresholds,
};

use super::{CentralError, CentralStore, pg_i64};

/// 一个节点计算任务在中心分析运行中的固定高水位。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralAnalysisNode {
    /// 手工加入的物理节点。
    pub machine_id: MachineId,
    /// 节点实际创建的计算任务。
    pub task_id: TaskId,
    /// 任务完成事务产生的节点 outbox 高水位。
    pub task_highwater: u64,
    /// 中心已提交的同步高水位。
    pub sync_highwater: u64,
    /// 节点报告的任务状态文本。
    pub task_status: String,
}

/// 从节点任务项分页读取并在中心一次性封存的内容位置。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralAnalysisInput {
    /// MD5 与文件大小构成的跨数据库内容键。
    pub content: ContentKey,
    /// 机器 ID 与规范路径构成的位置键。
    pub location: LocationKey,
}

/// 中心分析运行的固定状态机。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CentralAnalysisStatus {
    /// 等待所有节点一筛任务完成且同步游标追上高水位。
    CollectingStage1,
    /// 一筛数据已经完整同步。
    Stage1Synced,
    /// 正在生成完整一筛候选。
    Screening,
    /// 二筛任务已经批量派发。
    Phase2Dispatched,
    /// 二筛结果已经完整同步。
    Phase2Synced,
    /// 正在使用代表中心规则生成最终组。
    Finalizing,
    /// 所有筛选与分组均成功完成。
    Completed,
    /// 有候选缺失二筛结果，等待用户显式重试。
    Partial,
    /// 用户取消本次运行。
    Cancelled,
}

/// 相似候选的媒体算法类型。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CentralPairKind {
    /// 图片 PDQ 与联合二筛。
    Image,
    /// 六帧视频平均两层筛选。
    Video,
}

/// 一个候选在两层筛选流水线中的持久化结果。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CentralCandidateStatus {
    /// 一筛通过，尚未具备完整二筛数据。
    Stage1Passed,
    /// 联合二筛通过。
    Passed,
    /// 联合二筛明确拒绝。
    Rejected,
    /// 所需特征不完整，不能按零分处理。
    Incomplete,
}

/// 中心保存的一对规范有序候选内容。
#[derive(Clone, Debug, PartialEq)]
pub struct CentralCandidate {
    /// 图片或视频。
    pub kind: CentralPairKind,
    /// 必须严格小于 `right` 的左内容键。
    pub left: ContentKey,
    /// 规范排序后的右内容键。
    pub right: ContentKey,
    /// 一筛直接得分。
    pub stage1_score: f64,
    /// 二筛中通过的九分块数量。
    pub phash_passed_parts: Option<u8>,
    /// pHash 与 Sobel 联合得分。
    pub stage2_score: Option<f64>,
    /// 当前候选状态。
    pub status: CentralCandidateStatus,
}

/// 最终重复组的类别。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CentralGroupKind {
    /// MD5 与文件大小均相同。
    Exact,
    /// 图片两层判定通过。
    Image,
    /// 视频六帧平均两层判定通过。
    Video,
}

/// 原子替换分组结果时写入的一个位置成员。
#[derive(Clone, Debug, PartialEq)]
pub struct CentralGroupMember {
    /// 机器与规范路径。
    pub location: LocationKey,
    /// 成员的外部内容键。
    pub content: ContentKey,
    /// 是否为本组唯一代表位置。
    pub representative: bool,
    /// 与代表文件直接比较的一筛得分。
    pub stage1_score: f64,
    /// 与代表文件直接比较时通过的 pHash 分块数。
    pub phash_passed_parts: Option<u8>,
    /// 与代表文件直接比较的联合二筛得分。
    pub stage2_score: Option<f64>,
    /// 从中心复核表恢复的决定；新分组写入时使用 Undecided。
    pub review: CentralReviewDecision,
    /// 图片或视频宽度；写分组时可为 None。
    pub width: Option<u32>,
    /// 图片或视频高度；写分组时可为 None。
    pub height: Option<u32>,
    /// 图片 PDQ Quality；视频和其他文件为 None。
    pub quality: Option<u8>,
    /// 当前位置仍活动且内容键与分析快照一致。
    pub active: bool,
}

/// 一次中心分析最终写入的一组结果。
#[derive(Clone, Debug, PartialEq)]
pub struct CentralGroupWrite {
    /// UUID v7 字符串组 ID。
    pub group_id: String,
    /// 精确、图片或视频。
    pub kind: CentralGroupKind,
    /// 代表文件内容键。
    pub representative: ContentKey,
    /// 至少两个且只有一个代表的位置。
    pub members: Vec<CentralGroupMember>,
}

/// 分组列表中的稳定摘要。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralGroup {
    /// 组 ID。
    pub group_id: String,
    /// 组类别。
    pub kind: CentralGroupKind,
    /// 当前代表内容。
    pub representative: ContentKey,
    /// 当前活动位置数量。
    pub member_count: u32,
    /// 除代表位置外的可释放字节估算。
    pub reclaimable_bytes: u64,
}

/// 使用不透明游标返回的一页中心重复组。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CentralGroupPage {
    /// 当前页结果。
    pub items: Vec<CentralGroup>,
    /// 还有后续结果时返回的游标。
    pub next_cursor: Option<String>,
}

/// 使用位置游标返回的一页组成员。
#[derive(Clone, Debug, PartialEq)]
pub struct CentralGroupMemberPage {
    /// 当前页冻结成员；每项同时携带当前位置活动状态。
    pub items: Vec<CentralGroupMember>,
    /// 还有后续结果时返回的游标。
    pub next_cursor: Option<String>,
}

/// 管理端对一个组成员的复核决定。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CentralReviewDecision {
    /// 尚未决定。
    Undecided,
    /// 明确保留。
    Keep,
    /// 明确删除。
    Delete,
}

impl CentralStore {
    /// 创建分析运行并把阈值、节点任务和当前高水位保存为稳定快照。
    pub async fn create_analysis_run(
        &mut self,
        thresholds: &Thresholds,
        nodes: &[CentralAnalysisNode],
    ) -> Result<AnalysisRunId, CentralError> {
        thresholds.validate()?;
        let run_id = AnalysisRunId::new();
        let run_text = run_id.as_uuid().to_string();
        let thresholds_toml = toml::to_string(thresholds)?;
        let transaction = self.client.transaction().await?;
        transaction
            .execute(
                "INSERT INTO analysis_runs(analysis_run_id,status,thresholds_toml)
                 VALUES($1,'collecting_stage1',$2)",
                &[&run_text, &thresholds_toml],
            )
            .await?;
        for node in nodes {
            transaction
                .execute(
                    "INSERT INTO nodes(machine_id) VALUES($1)
                     ON CONFLICT(machine_id) DO UPDATE SET last_seen_at=now()",
                    &[&node.machine_id.as_str()],
                )
                .await?;
            transaction
                .execute(
                    "INSERT INTO analysis_run_nodes(
                       analysis_run_id,machine_id,task_id,task_highwater,sync_highwater,task_status)
                     VALUES($1,$2,$3,$4,$5,$6)",
                    &[
                        &run_text,
                        &node.machine_id.as_str(),
                        &node.task_id.as_uuid().to_string(),
                        &pg_i64(node.task_highwater, "任务高水位")?,
                        &pg_i64(node.sync_highwater, "同步高水位")?,
                        &node.task_status,
                    ],
                )
                .await?;
        }
        transaction.commit().await?;
        Ok(run_id)
    }

    /// 把完整输入写入一次事务并封存；封存后的运行不能追加或改写输入。
    pub async fn insert_analysis_inputs(
        &mut self,
        run_id: AnalysisRunId,
        inputs: &[CentralAnalysisInput],
    ) -> Result<(), CentralError> {
        let run_text = run_id.as_uuid().to_string();
        let transaction = self.client.transaction().await?;
        let frozen = transaction
            .query_opt(
                "SELECT inputs_frozen FROM analysis_runs WHERE analysis_run_id=$1 FOR UPDATE",
                &[&run_text],
            )
            .await?
            .ok_or_else(|| CentralError::InvalidState("分析运行不存在".into()))?
            .get::<_, bool>(0);
        if frozen {
            return Err(CentralError::InvalidState("分析输入已经封存".into()));
        }
        for input in inputs {
            transaction
                .execute(
                    "INSERT INTO analysis_run_inputs(
                       analysis_run_id,md5,file_size,machine_id,normalized_path)
                     VALUES($1,$2,$3,$4,$5)",
                    &[
                        &run_text,
                        &input.content.md5().as_slice(),
                        &pg_i64(input.content.file_size(), "文件大小")?,
                        &input.location.machine_id().as_str(),
                        &input.location.normalized_path().as_str(),
                    ],
                )
                .await?;
        }
        transaction
            .execute(
                "UPDATE analysis_runs SET inputs_frozen=TRUE,updated_at=now()
                 WHERE analysis_run_id=$1",
                &[&run_text],
            )
            .await?;
        transaction.commit().await?;
        Ok(())
    }

    /// 原子替换本次运行的完整候选集合；规范排序防止同一对内容重复。
    pub async fn replace_candidates(
        &mut self,
        run_id: AnalysisRunId,
        candidates: &[CentralCandidate],
    ) -> Result<(), CentralError> {
        let run_text = run_id.as_uuid().to_string();
        let transaction = self.client.transaction().await?;
        transaction
            .execute(
                "DELETE FROM candidate_pairs WHERE analysis_run_id=$1",
                &[&run_text],
            )
            .await?;
        for candidate in candidates {
            validate_candidate(candidate)?;
            transaction
                .execute(
                    "INSERT INTO candidate_pairs(
                       analysis_run_id,pair_kind,left_md5,left_size,right_md5,right_size,
                       stage1_score,phash_passed_parts,stage2_score,status)
                     VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)",
                    &[
                        &run_text,
                        &candidate.kind.as_str(),
                        &candidate.left.md5().as_slice(),
                        &pg_i64(candidate.left.file_size(), "左文件大小")?,
                        &candidate.right.md5().as_slice(),
                        &pg_i64(candidate.right.file_size(), "右文件大小")?,
                        &candidate.stage1_score,
                        &candidate.phash_passed_parts.map(i16::from),
                        &candidate.stage2_score,
                        &candidate.status.as_str(),
                    ],
                )
                .await?;
        }
        transaction.commit().await?;
        Ok(())
    }

    /// 原子替换最终组；调用方传入的成员已经由共享代表中心算法生成。
    pub async fn replace_groups(
        &mut self,
        run_id: AnalysisRunId,
        groups: &[CentralGroupWrite],
    ) -> Result<(), CentralError> {
        let run_text = run_id.as_uuid().to_string();
        let transaction = self.client.transaction().await?;
        transaction
            .execute(
                "DELETE FROM duplicate_groups WHERE analysis_run_id=$1",
                &[&run_text],
            )
            .await?;
        for group in groups {
            validate_group(group)?;
            transaction
                .execute(
                    "INSERT INTO duplicate_groups(
                       analysis_run_id,group_id,group_kind,representative_md5,representative_size)
                     VALUES($1,$2,$3,$4,$5)",
                    &[
                        &run_text,
                        &group.group_id,
                        &group.kind.as_str(),
                        &group.representative.md5().as_slice(),
                        &pg_i64(group.representative.file_size(), "代表文件大小")?,
                    ],
                )
                .await?;
            for member in &group.members {
                transaction
                    .execute(
                        "INSERT INTO group_members(
                           analysis_run_id,group_id,machine_id,normalized_path,md5,file_size,
                           representative,stage1_score,phash_passed_parts,stage2_score,active)
                         VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,TRUE)",
                        &[
                            &run_text,
                            &group.group_id,
                            &member.location.machine_id().as_str(),
                            &member.location.normalized_path().as_str(),
                            &member.content.md5().as_slice(),
                            &pg_i64(member.content.file_size(), "成员文件大小")?,
                            &member.representative,
                            &member.stage1_score,
                            &member.phash_passed_parts.map(i16::from),
                            &member.stage2_score,
                        ],
                    )
                    .await?;
            }
        }
        transaction.commit().await?;
        Ok(())
    }

    /// 用 `(kind, representative, group_id)` 固定键分页读取中心组摘要。
    pub async fn page_groups(
        &self,
        run_id: AnalysisRunId,
        cursor: Option<&str>,
        limit: usize,
    ) -> Result<CentralGroupPage, CentralError> {
        if limit == 0 {
            return Err(CentralError::InvalidState("分页大小不能为 0".into()));
        }
        let cursor = cursor.map(decode_group_cursor).transpose()?;
        let (kind, md5, size, group_id) = match cursor {
            Some((kind, md5, size, group_id)) => (
                Some(kind),
                Some(md5.to_vec()),
                Some(pg_i64(size, "游标文件大小")?),
                Some(group_id),
            ),
            None => (None, None, None, None),
        };
        let rows = self
            .client
            .query(
                "WITH current_members AS (
                   SELECT gm.*
                   FROM group_members gm
                   JOIN file_locations f ON f.machine_id=gm.machine_id
                     AND f.normalized_path=gm.normalized_path AND f.active=TRUE
                   JOIN contents c ON c.content_id=f.content_id
                     AND c.md5=gm.md5 AND c.file_size=gm.file_size
                   WHERE gm.active=TRUE
                 ), current_groups AS (
                   SELECT analysis_run_id,group_id,
                          (ARRAY_AGG(md5 ORDER BY representative DESC,machine_id,normalized_path))[1]
                            AS representative_md5,
                          (ARRAY_AGG(file_size ORDER BY representative DESC,machine_id,normalized_path))[1]
                            AS representative_size,
                          COUNT(*) AS member_count,
                          (SUM(file_size) -
                            (ARRAY_AGG(file_size ORDER BY representative DESC,machine_id,normalized_path))[1])::BIGINT
                            AS reclaimable_bytes
                   FROM current_members
                   GROUP BY analysis_run_id,group_id
                   HAVING COUNT(*)>=2
                 )
                 SELECT dg.group_id,dg.group_kind,cg.representative_md5,cg.representative_size,
                        cg.member_count,cg.reclaimable_bytes
                 FROM duplicate_groups dg
                 JOIN current_groups cg ON cg.analysis_run_id=dg.analysis_run_id
                   AND cg.group_id=dg.group_id
                 WHERE dg.analysis_run_id=$1 AND (
                   $2::text IS NULL OR dg.group_kind>$2 OR
                   (dg.group_kind=$2 AND cg.representative_md5>$3) OR
                   (dg.group_kind=$2 AND cg.representative_md5=$3 AND cg.representative_size>$4) OR
                   (dg.group_kind=$2 AND cg.representative_md5=$3
                     AND cg.representative_size=$4 AND dg.group_id>$5))
                 ORDER BY dg.group_kind,cg.representative_md5,cg.representative_size,dg.group_id
                 LIMIT $6",
                &[
                    &run_id.as_uuid().to_string(),
                    &kind,
                    &md5,
                    &size,
                    &group_id,
                    &i64::try_from(limit.saturating_add(1))
                        .map_err(|_| CentralError::InvalidState("分页大小过大".into()))?,
                ],
            )
            .await?;
        let mut items = rows
            .into_iter()
            .map(|row| {
                Ok(CentralGroup {
                    group_id: row.get(0),
                    kind: CentralGroupKind::parse(row.get::<_, &str>(1))?,
                    representative: ContentKey::new(
                        fixed_md5(row.get(2))?,
                        u64::try_from(row.get::<_, i64>(3)).map_err(|_| {
                            CentralError::InvalidState("中心代表文件大小为负数".into())
                        })?,
                    ),
                    member_count: u32::try_from(row.get::<_, i64>(4))
                        .map_err(|_| CentralError::InvalidState("中心组成员数超出范围".into()))?,
                    reclaimable_bytes: u64::try_from(row.get::<_, i64>(5))
                        .map_err(|_| CentralError::InvalidState("中心可释放字节数为负数".into()))?,
                })
            })
            .collect::<Result<Vec<_>, CentralError>>()?;
        let has_more = items.len() > limit;
        items.truncate(limit);
        let next_cursor = has_more
            .then(|| items.last().map(encode_group_cursor))
            .flatten();
        Ok(CentralGroupPage { items, next_cursor })
    }

    /// 用 `(machine_id, normalized_path)` 固定键分页读取冻结成员及当前位置状态。
    pub async fn page_group_members(
        &self,
        run_id: AnalysisRunId,
        group_id: &str,
        cursor: Option<&str>,
        limit: usize,
    ) -> Result<CentralGroupMemberPage, CentralError> {
        if limit == 0 {
            return Err(CentralError::InvalidState("分页大小不能为 0".into()));
        }
        let (machine, path) = cursor
            .map(decode_member_cursor)
            .transpose()?
            .map(|value| (Some(value.0), Some(value.1)))
            .unwrap_or((None, None));
        let rows = self
            .client
            .query(
                "WITH member_state AS (
                   SELECT gm.*,
                          COALESCE(f.active=TRUE AND current.md5=gm.md5
                            AND current.file_size=gm.file_size,FALSE) AS current_active
                   FROM group_members gm
                   LEFT JOIN file_locations f ON f.machine_id=gm.machine_id
                     AND f.normalized_path=gm.normalized_path
                   LEFT JOIN contents current ON current.content_id=f.content_id
                   WHERE gm.analysis_run_id=$1 AND gm.group_id=$2 AND gm.active=TRUE
                 ), ranked_members AS (
                   SELECT member_state.*,
                          ROW_NUMBER() OVER (
                            PARTITION BY analysis_run_id,group_id
                            ORDER BY current_active DESC,representative DESC,
                                     machine_id,normalized_path
                          ) AS current_rank
                   FROM member_state
                 )
                 SELECT gm.machine_id,gm.normalized_path,gm.md5,gm.file_size,
                        (gm.current_active=TRUE AND gm.current_rank=1),
                        gm.stage1_score,gm.phash_passed_parts,gm.stage2_score,
                        COALESCE(rm.decision,'undecided'),COALESCE(i.width,v.width),
                        COALESCE(i.height,v.height),i.quality,gm.current_active
                 FROM ranked_members gm
                 LEFT JOIN contents c ON c.md5=gm.md5 AND c.file_size=gm.file_size
                 LEFT JOIN image_stage1 i ON i.content_id=c.content_id
                 LEFT JOIN video_metadata v ON v.content_id=c.content_id
                 LEFT JOIN review_marks rm ON rm.analysis_run_id=gm.analysis_run_id
                   AND rm.group_id=gm.group_id AND rm.machine_id=gm.machine_id
                   AND rm.normalized_path=gm.normalized_path
                 WHERE (
                   $3::text IS NULL OR gm.machine_id>$3 OR
                   (gm.machine_id=$3 AND gm.normalized_path>$4))
                 ORDER BY gm.machine_id,gm.normalized_path LIMIT $5",
                &[
                    &run_id.as_uuid().to_string(),
                    &group_id,
                    &machine,
                    &path,
                    &i64::try_from(limit.saturating_add(1))
                        .map_err(|_| CentralError::InvalidState("分页大小过大".into()))?,
                ],
            )
            .await?;
        let mut items = rows
            .into_iter()
            .map(|row| {
                let machine: String = row.get(0);
                let path: String = row.get(1);
                Ok(CentralGroupMember {
                    location: LocationKey::new(
                        MachineId::parse(machine.trim_end())?,
                        NormalizedPath::new(path)?,
                    ),
                    content: ContentKey::new(
                        fixed_md5(row.get(2))?,
                        u64::try_from(row.get::<_, i64>(3)).map_err(|_| {
                            CentralError::InvalidState("中心成员文件大小为负数".into())
                        })?,
                    ),
                    representative: row.get(4),
                    stage1_score: row.get(5),
                    phash_passed_parts: row.get::<_, Option<i16>>(6).map(|value| value as u8),
                    stage2_score: row.get(7),
                    review: CentralReviewDecision::parse(row.get(8))?,
                    width: row.get::<_, Option<i32>>(9).map(|value| value as u32),
                    height: row.get::<_, Option<i32>>(10).map(|value| value as u32),
                    quality: row.get::<_, Option<i16>>(11).map(|value| value as u8),
                    active: row.get(12),
                })
            })
            .collect::<Result<Vec<_>, CentralError>>()?;
        let has_more = items.len() > limit;
        items.truncate(limit);
        let next_cursor = has_more
            .then(|| items.last().map(encode_member_cursor))
            .flatten();
        Ok(CentralGroupMemberPage { items, next_cursor })
    }

    /// UPSERT 一个组成员的未决定、保留或删除复核标记。
    pub async fn save_review_mark(
        &self,
        run_id: AnalysisRunId,
        group_id: &str,
        location: &LocationKey,
        decision: CentralReviewDecision,
    ) -> Result<(), CentralError> {
        self.client
            .execute(
                "INSERT INTO review_marks(
                   analysis_run_id,group_id,machine_id,normalized_path,decision)
                 VALUES($1,$2,$3,$4,$5)
                 ON CONFLICT(analysis_run_id,group_id,machine_id,normalized_path)
                 DO UPDATE SET decision=excluded.decision",
                &[
                    &run_id.as_uuid().to_string(),
                    &group_id,
                    &location.machine_id().as_str(),
                    &location.normalized_path().as_str(),
                    &decision.as_str(),
                ],
            )
            .await?;
        Ok(())
    }

    /// 更新分析状态；协调器在完成对应门禁后显式推进。
    pub async fn set_analysis_status(
        &self,
        run_id: AnalysisRunId,
        status: CentralAnalysisStatus,
        error_text: Option<&str>,
    ) -> Result<(), CentralError> {
        let changed = self
            .client
            .execute(
                "UPDATE analysis_runs SET status=$2,error_text=$3,updated_at=now()
                 WHERE analysis_run_id=$1",
                &[&run_id.as_uuid().to_string(), &status.as_str(), &error_text],
            )
            .await?;
        if changed == 0 {
            return Err(CentralError::InvalidState("分析运行不存在".into()));
        }
        Ok(())
    }
}

fn validate_candidate(candidate: &CentralCandidate) -> Result<(), CentralError> {
    if candidate.left >= candidate.right {
        return Err(CentralError::InvalidState("候选内容键必须严格升序".into()));
    }
    if !candidate.stage1_score.is_finite()
        || candidate
            .stage2_score
            .is_some_and(|score| !score.is_finite())
    {
        return Err(CentralError::InvalidState("候选得分必须是有限数值".into()));
    }
    Ok(())
}

fn validate_group(group: &CentralGroupWrite) -> Result<(), CentralError> {
    let representatives = group
        .members
        .iter()
        .filter(|member| member.representative)
        .collect::<Vec<_>>();
    if group.members.len() < 2
        || representatives.len() != 1
        || representatives[0].content != group.representative
    {
        return Err(CentralError::InvalidState(
            "重复组至少两个成员，且必须有唯一匹配代表内容的位置".into(),
        ));
    }
    if group.members.iter().any(|member| {
        !member.stage1_score.is_finite()
            || member.stage2_score.is_some_and(|score| !score.is_finite())
    }) {
        return Err(CentralError::InvalidState(
            "组成员得分必须是有限数值".into(),
        ));
    }
    Ok(())
}

impl CentralAnalysisStatus {
    /// 返回与 PostgreSQL CHECK 约束一致的稳定名称。
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::CollectingStage1 => "collecting_stage1",
            Self::Stage1Synced => "stage1_synced",
            Self::Screening => "screening",
            Self::Phase2Dispatched => "phase2_dispatched",
            Self::Phase2Synced => "phase2_synced",
            Self::Finalizing => "finalizing",
            Self::Completed => "completed",
            Self::Partial => "partial",
            Self::Cancelled => "cancelled",
        }
    }

    pub(crate) fn parse(value: &str) -> Result<Self, CentralError> {
        match value {
            "collecting_stage1" => Ok(Self::CollectingStage1),
            "stage1_synced" => Ok(Self::Stage1Synced),
            "screening" => Ok(Self::Screening),
            "phase2_dispatched" => Ok(Self::Phase2Dispatched),
            "phase2_synced" => Ok(Self::Phase2Synced),
            "finalizing" => Ok(Self::Finalizing),
            "completed" => Ok(Self::Completed),
            "partial" => Ok(Self::Partial),
            "cancelled" => Ok(Self::Cancelled),
            _ => Err(CentralError::InvalidState(format!(
                "未知中心分析状态: {value}"
            ))),
        }
    }
}

impl CentralPairKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Image => "image",
            Self::Video => "video",
        }
    }

    pub(crate) fn parse(value: &str) -> Result<Self, CentralError> {
        match value {
            "image" => Ok(Self::Image),
            "video" => Ok(Self::Video),
            _ => Err(CentralError::InvalidState(format!(
                "未知中心候选类别: {value}"
            ))),
        }
    }
}

impl CentralCandidateStatus {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Stage1Passed => "stage1_passed",
            Self::Passed => "passed",
            Self::Rejected => "rejected",
            Self::Incomplete => "incomplete",
        }
    }

    pub(crate) fn parse(value: &str) -> Result<Self, CentralError> {
        match value {
            "stage1_passed" => Ok(Self::Stage1Passed),
            "passed" => Ok(Self::Passed),
            "rejected" => Ok(Self::Rejected),
            "incomplete" => Ok(Self::Incomplete),
            _ => Err(CentralError::InvalidState(format!(
                "未知中心候选状态: {value}"
            ))),
        }
    }
}

impl CentralGroupKind {
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::Exact => "exact",
            Self::Image => "image",
            Self::Video => "video",
        }
    }

    fn parse(value: &str) -> Result<Self, CentralError> {
        match value {
            "exact" => Ok(Self::Exact),
            "image" => Ok(Self::Image),
            "video" => Ok(Self::Video),
            _ => Err(CentralError::InvalidState(format!(
                "未知中心组类别: {value}"
            ))),
        }
    }
}

impl CentralReviewDecision {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Undecided => "undecided",
            Self::Keep => "keep",
            Self::Delete => "delete",
        }
    }

    fn parse(value: &str) -> Result<Self, CentralError> {
        match value {
            "undecided" => Ok(Self::Undecided),
            "keep" => Ok(Self::Keep),
            "delete" => Ok(Self::Delete),
            _ => Err(CentralError::InvalidState(format!(
                "未知中心复核决定: {value}"
            ))),
        }
    }
}

fn fixed_md5(value: Vec<u8>) -> Result<[u8; 16], CentralError> {
    value
        .try_into()
        .map_err(|_| CentralError::InvalidState("中心 MD5 长度不是 16".into()))
}

fn encode_group_cursor(group: &CentralGroup) -> String {
    let kind = group.kind.as_str().as_bytes();
    let mut bytes = Vec::with_capacity(1 + kind.len() + 16 + 8 + 2 + group.group_id.len());
    bytes.push(kind.len() as u8);
    bytes.extend_from_slice(kind);
    bytes.extend_from_slice(&group.representative.md5());
    bytes.extend_from_slice(&group.representative.file_size().to_be_bytes());
    bytes.extend_from_slice(&(group.group_id.len() as u16).to_be_bytes());
    bytes.extend_from_slice(group.group_id.as_bytes());
    hex_encode(&bytes)
}

fn decode_group_cursor(cursor: &str) -> Result<(String, [u8; 16], u64, String), CentralError> {
    let bytes = hex_decode(cursor)?;
    let mut at = 0;
    let kind_length = take_byte(&bytes, &mut at)? as usize;
    let kind = take_text(&bytes, &mut at, kind_length)?;
    CentralGroupKind::parse(&kind)?;
    let md5 = take_array(&bytes, &mut at)?;
    let size = u64::from_be_bytes(take_array(&bytes, &mut at)?);
    let group_length = u16::from_be_bytes(take_array(&bytes, &mut at)?) as usize;
    let group = take_text(&bytes, &mut at, group_length)?;
    if at != bytes.len() {
        return Err(CentralError::InvalidCursor);
    }
    Ok((kind, md5, size, group))
}

fn encode_member_cursor(member: &CentralGroupMember) -> String {
    let machine = member.location.machine_id().as_str().as_bytes();
    let path = member.location.normalized_path().as_str().as_bytes();
    let mut bytes = Vec::with_capacity(1 + machine.len() + 4 + path.len());
    bytes.push(machine.len() as u8);
    bytes.extend_from_slice(machine);
    bytes.extend_from_slice(&(path.len() as u32).to_be_bytes());
    bytes.extend_from_slice(path);
    hex_encode(&bytes)
}

fn decode_member_cursor(cursor: &str) -> Result<(String, String), CentralError> {
    let bytes = hex_decode(cursor)?;
    let mut at = 0;
    let machine_length = take_byte(&bytes, &mut at)? as usize;
    let machine = take_text(&bytes, &mut at, machine_length)?;
    MachineId::parse(&machine)?;
    let path_length = u32::from_be_bytes(take_array(&bytes, &mut at)?) as usize;
    let path = take_text(&bytes, &mut at, path_length)?;
    NormalizedPath::new(&path)?;
    if at != bytes.len() {
        return Err(CentralError::InvalidCursor);
    }
    Ok((machine, path))
}

fn hex_encode(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut output = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        output.push(HEX[(byte >> 4) as usize] as char);
        output.push(HEX[(byte & 0x0f) as usize] as char);
    }
    output
}

fn hex_decode(value: &str) -> Result<Vec<u8>, CentralError> {
    if !value.len().is_multiple_of(2) {
        return Err(CentralError::InvalidCursor);
    }
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let high = hex_nibble(pair[0])?;
            let low = hex_nibble(pair[1])?;
            Ok((high << 4) | low)
        })
        .collect()
}

fn hex_nibble(value: u8) -> Result<u8, CentralError> {
    match value {
        b'0'..=b'9' => Ok(value - b'0'),
        b'a'..=b'f' => Ok(value - b'a' + 10),
        _ => Err(CentralError::InvalidCursor),
    }
}

fn take_byte(bytes: &[u8], at: &mut usize) -> Result<u8, CentralError> {
    let value = *bytes.get(*at).ok_or(CentralError::InvalidCursor)?;
    *at += 1;
    Ok(value)
}

fn take_array<const N: usize>(bytes: &[u8], at: &mut usize) -> Result<[u8; N], CentralError> {
    let end = at.checked_add(N).ok_or(CentralError::InvalidCursor)?;
    let value = bytes
        .get(*at..end)
        .ok_or(CentralError::InvalidCursor)?
        .try_into()
        .map_err(|_| CentralError::InvalidCursor)?;
    *at = end;
    Ok(value)
}

fn take_text(bytes: &[u8], at: &mut usize, length: usize) -> Result<String, CentralError> {
    let end = at.checked_add(length).ok_or(CentralError::InvalidCursor)?;
    let value = std::str::from_utf8(bytes.get(*at..end).ok_or(CentralError::InvalidCursor)?)
        .map_err(|_| CentralError::InvalidCursor)?
        .to_owned();
    *at = end;
    Ok(value)
}

//! 跨机器协调器所需的运行快照、冻结输入和完整媒体特征读取。

use std::collections::BTreeMap;

use dedup_core::{
    AnalysisRunId, ContentKey, LocationKey, MachineId, MediaKind, NormalizedPath, TaskId,
    Thresholds,
};
use dedup_media::{ImageStage1, ImageStage2, PdqHash};

use crate::analysis::CrossFeatureSet;

use super::{
    CentralAnalysisInput, CentralAnalysisNode, CentralAnalysisStatus, CentralCandidate,
    CentralCandidateStatus, CentralError, CentralPairKind, CentralStore, pg_i64,
};

/// 中心协调器恢复当前运行阶段和不可变阈值所需的最小快照。
#[derive(Clone, Debug, PartialEq)]
pub struct CentralRunSnapshot {
    /// 已提交的分析状态。
    pub status: CentralAnalysisStatus,
    /// 创建运行时保存的九个阈值。
    pub thresholds: Thresholds,
    /// 输入是否已经在一个事务中封存。
    pub inputs_frozen: bool,
}

impl CentralStore {
    /// 读取运行当前状态、阈值快照和输入冻结标记。
    pub async fn analysis_run_snapshot(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<CentralRunSnapshot, CentralError> {
        let row = self
            .client
            .query_opt(
                "SELECT status,thresholds_toml,inputs_frozen FROM analysis_runs
                 WHERE analysis_run_id=$1",
                &[&run_id.as_uuid().to_string()],
            )
            .await?
            .ok_or_else(|| CentralError::InvalidState("分析运行不存在".into()))?;
        let thresholds = toml::from_str::<Thresholds>(row.get(1))
            .map_err(|error| CentralError::InvalidState(format!("中心阈值快照无效: {error}")))?;
        thresholds.validate()?;
        Ok(CentralRunSnapshot {
            status: CentralAnalysisStatus::parse(row.get(0))?,
            thresholds,
            inputs_frozen: row.get(2),
        })
    }

    /// 更新一个已记录节点任务的真实终态、高水位和当前中心游标。
    pub async fn update_analysis_node(
        &self,
        run_id: AnalysisRunId,
        node: &CentralAnalysisNode,
    ) -> Result<(), CentralError> {
        let changed = self
            .client
            .execute(
                "UPDATE analysis_run_nodes SET task_highwater=$4,sync_highwater=$5,task_status=$6
                 WHERE analysis_run_id=$1 AND machine_id=$2 AND task_id=$3",
                &[
                    &run_id.as_uuid().to_string(),
                    &node.machine_id.as_str(),
                    &node.task_id.as_uuid().to_string(),
                    &pg_i64(node.task_highwater, "任务高水位")?,
                    &pg_i64(node.sync_highwater, "同步高水位")?,
                    &node.task_status,
                ],
            )
            .await?;
        if changed == 0 {
            return Err(CentralError::InvalidState("中心分析节点任务不存在".into()));
        }
        Ok(())
    }

    /// 返回一次运行记录的全部 stage1 与 phase2 节点任务，供协调器重建门禁状态。
    pub async fn analysis_node_tasks(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<Vec<CentralAnalysisNode>, CentralError> {
        let rows = self
            .client
            .query(
                "SELECT machine_id,task_id,task_highwater,sync_highwater,task_status
                 FROM analysis_run_nodes WHERE analysis_run_id=$1
                 ORDER BY machine_id,task_id",
                &[&run_id.as_uuid().to_string()],
            )
            .await?;
        rows.into_iter()
            .map(|row| {
                let machine: String = row.get(0);
                let task: String = row.get(1);
                Ok(CentralAnalysisNode {
                    machine_id: MachineId::parse(machine.trim_end())?,
                    task_id: TaskId::from_uuid(
                        uuid::Uuid::parse_str(&task).map_err(|_| {
                            CentralError::InvalidState("中心任务 ID 不是 UUID".into())
                        })?,
                    ),
                    task_highwater: non_negative(row.get(2), "任务高水位")?,
                    sync_highwater: non_negative(row.get(3), "同步高水位")?,
                    task_status: row.get(4),
                })
            })
            .collect()
    }

    /// 为 phase2 追加一个节点任务；同一运行允许同节点在重试时产生多个新任务。
    pub async fn add_analysis_node_task(
        &self,
        run_id: AnalysisRunId,
        node: &CentralAnalysisNode,
    ) -> Result<(), CentralError> {
        self.client
            .execute(
                "INSERT INTO analysis_run_nodes(
                   analysis_run_id,machine_id,task_id,task_highwater,sync_highwater,task_status)
                 VALUES($1,$2,$3,$4,$5,$6)",
                &[
                    &run_id.as_uuid().to_string(),
                    &node.machine_id.as_str(),
                    &node.task_id.as_uuid().to_string(),
                    &pg_i64(node.task_highwater, "任务高水位")?,
                    &pg_i64(node.sync_highwater, "同步高水位")?,
                    &node.task_status,
                ],
            )
            .await?;
        Ok(())
    }

    /// 按内容与位置稳定顺序返回本次运行已经封存的输入。
    pub async fn analysis_inputs(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<Vec<CentralAnalysisInput>, CentralError> {
        let rows = self
            .client
            .query(
                "SELECT md5,file_size,machine_id,normalized_path
                 FROM analysis_run_inputs WHERE analysis_run_id=$1
                 ORDER BY md5,file_size,machine_id,normalized_path",
                &[&run_id.as_uuid().to_string()],
            )
            .await?;
        rows.into_iter()
            .map(|row| {
                let machine: String = row.get(2);
                Ok(CentralAnalysisInput {
                    content: ContentKey::new(
                        fixed_md5(row.get(0))?,
                        non_negative(row.get(1), "输入文件大小")?,
                    ),
                    location: LocationKey::new(
                        MachineId::parse(machine.trim_end())?,
                        NormalizedPath::new(row.get::<_, String>(3))?,
                    ),
                })
            })
            .collect()
    }

    /// 按媒体类型和内容键稳定读取本次运行的完整候选集合。
    pub async fn analysis_candidates(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<Vec<CentralCandidate>, CentralError> {
        let rows = self
            .client
            .query(
                "SELECT pair_kind,left_md5,left_size,right_md5,right_size,stage1_score,
                        phash_passed_parts,stage2_score,status
                 FROM candidate_pairs WHERE analysis_run_id=$1
                 ORDER BY pair_kind,left_md5,left_size,right_md5,right_size",
                &[&run_id.as_uuid().to_string()],
            )
            .await?;
        rows.into_iter()
            .map(|row| {
                Ok(CentralCandidate {
                    kind: CentralPairKind::parse(row.get(0))?,
                    left: ContentKey::new(
                        fixed_md5(row.get(1))?,
                        non_negative(row.get(2), "候选左文件大小")?,
                    ),
                    right: ContentKey::new(
                        fixed_md5(row.get(3))?,
                        non_negative(row.get(4), "候选右文件大小")?,
                    ),
                    stage1_score: row.get(5),
                    phash_passed_parts: row.get::<_, Option<i16>>(6).map(|value| value as u8),
                    stage2_score: row.get(7),
                    status: CentralCandidateStatus::parse(row.get(8))?,
                })
            })
            .collect()
    }

    /// 只装载本次冻结内容关联的媒体类型及完整一筛/二筛特征。
    pub async fn analysis_features(
        &self,
        run_id: AnalysisRunId,
    ) -> Result<CrossFeatureSet, CentralError> {
        let run = run_id.as_uuid().to_string();
        let mut output = CrossFeatureSet::default();
        for row in self
            .client
            .query(
                "SELECT DISTINCT c.md5,c.file_size,c.media_kind
             FROM analysis_run_inputs i JOIN contents c USING(md5,file_size)
             WHERE i.analysis_run_id=$1 ORDER BY c.md5,c.file_size",
                &[&run],
            )
            .await?
        {
            output
                .media_kinds
                .insert(row_content(&row, 0, 1)?, parse_media_kind(row.get(2))?);
        }
        for row in self
            .client
            .query(
                "SELECT DISTINCT c.md5,c.file_size,s.width,s.height,s.pdq,s.quality
             FROM analysis_run_inputs i JOIN contents c USING(md5,file_size)
             JOIN image_stage1 s ON s.content_id=c.content_id
             WHERE i.analysis_run_id=$1 AND s.width IS NOT NULL AND s.height IS NOT NULL
               AND s.pdq IS NOT NULL AND s.quality IS NOT NULL
             ORDER BY c.md5,c.file_size",
                &[&run],
            )
            .await?
        {
            output.image_stage1.insert(
                row_content(&row, 0, 1)?,
                ImageStage1 {
                    width: positive_u32(row.get(2), "图片宽度")?,
                    height: positive_u32(row.get(3), "图片高度")?,
                    pdq: PdqHash::from_bytes(fixed::<32>(row.get(4), "图片 PDQ")?),
                    quality: u8::try_from(row.get::<_, i16>(5))
                        .map_err(|_| CentralError::InvalidState("图片 Quality 越界".into()))?,
                },
            );
        }
        load_video_stage1(&self.client, &run, &mut output).await?;
        for row in self
            .client
            .query(
                "SELECT DISTINCT c.md5,c.file_size,s.phash_parts,s.sobel
             FROM analysis_run_inputs i JOIN contents c USING(md5,file_size)
             JOIN image_stage2 s ON s.content_id=c.content_id
             WHERE i.analysis_run_id=$1 AND s.phash_parts IS NOT NULL AND s.sobel IS NOT NULL
             ORDER BY c.md5,c.file_size",
                &[&run],
            )
            .await?
        {
            output.image_stage2.insert(
                row_content(&row, 0, 1)?,
                decode_stage2(row.get(2), row.get(3))?,
            );
        }
        load_video_stage2(&self.client, &run, &mut output).await?;
        Ok(output)
    }
}

async fn load_video_stage1(
    client: &tokio_postgres::Client,
    run: &str,
    output: &mut CrossFeatureSet,
) -> Result<(), CentralError> {
    let rows = client
        .query(
            "SELECT DISTINCT c.md5,c.file_size,s.slot,s.decoded,s.width,s.height,s.pdq,s.quality
         FROM analysis_run_inputs i JOIN contents c USING(md5,file_size)
         JOIN video_frame_stage1 s ON s.content_id=c.content_id
         WHERE i.analysis_run_id=$1 ORDER BY c.md5,c.file_size,s.slot",
            &[&run],
        )
        .await?;
    let mut grouped = BTreeMap::<ContentKey, Vec<tokio_postgres::Row>>::new();
    for row in rows {
        grouped
            .entry(row_content(&row, 0, 1)?)
            .or_default()
            .push(row);
    }
    for (content, rows) in grouped {
        if rows.len() != 6 {
            continue;
        }
        let mut frames = [None; 6];
        let mut complete = true;
        for (expected, row) in rows.into_iter().enumerate() {
            let slot = usize::try_from(row.get::<_, i16>(2)).unwrap_or(usize::MAX);
            if slot != expected {
                complete = false;
                break;
            }
            if !row.get::<_, bool>(3) {
                continue;
            }
            let fields = (
                row.get::<_, Option<i32>>(4),
                row.get::<_, Option<i32>>(5),
                row.get::<_, Option<Vec<u8>>>(6),
                row.get::<_, Option<i16>>(7),
            );
            let (Some(width), Some(height), Some(pdq), Some(quality)) = fields else {
                complete = false;
                break;
            };
            frames[slot] = Some(ImageStage1 {
                width: positive_u32(width, "视频帧宽度")?,
                height: positive_u32(height, "视频帧高度")?,
                pdq: PdqHash::from_bytes(fixed::<32>(pdq, "视频帧 PDQ")?),
                quality: u8::try_from(quality)
                    .map_err(|_| CentralError::InvalidState("视频帧 Quality 越界".into()))?,
            });
        }
        if complete && frames.iter().flatten().count() >= 4 {
            output.video_stage1.insert(content, Box::new(frames));
        }
    }
    Ok(())
}

async fn load_video_stage2(
    client: &tokio_postgres::Client,
    run: &str,
    output: &mut CrossFeatureSet,
) -> Result<(), CentralError> {
    let rows = client
        .query(
            "SELECT DISTINCT c.md5,c.file_size,s.slot,s.phash_parts,s.sobel
         FROM analysis_run_inputs i JOIN contents c USING(md5,file_size)
         JOIN video_frame_stage2 s ON s.content_id=c.content_id
         WHERE i.analysis_run_id=$1 AND s.phash_parts IS NOT NULL AND s.sobel IS NOT NULL
         ORDER BY c.md5,c.file_size,s.slot",
            &[&run],
        )
        .await?;
    let mut grouped = BTreeMap::<ContentKey, Vec<tokio_postgres::Row>>::new();
    for row in rows {
        grouped
            .entry(row_content(&row, 0, 1)?)
            .or_default()
            .push(row);
    }
    for (content, stage1) in &output.video_stage1 {
        let mut stage2 = [None; 6];
        if let Some(rows) = grouped.remove(content) {
            for row in rows {
                let slot = usize::try_from(row.get::<_, i16>(2)).unwrap_or(usize::MAX);
                if slot < 6 {
                    stage2[slot] = Some(decode_stage2(row.get(3), row.get(4))?);
                }
            }
        }
        if (0..6).all(|slot| stage1[slot].is_none() || stage2[slot].is_some()) {
            output.video_stage2.insert(*content, Box::new(stage2));
        }
    }
    Ok(())
}

fn row_content(
    row: &tokio_postgres::Row,
    md5: usize,
    size: usize,
) -> Result<ContentKey, CentralError> {
    Ok(ContentKey::new(
        fixed_md5(row.get(md5))?,
        non_negative(row.get(size), "内容文件大小")?,
    ))
}

fn parse_media_kind(value: &str) -> Result<MediaKind, CentralError> {
    match value {
        "image" => Ok(MediaKind::Image),
        "video" => Ok(MediaKind::Video),
        "other" => Ok(MediaKind::Other),
        _ => Err(CentralError::InvalidState(format!("未知媒体类型: {value}"))),
    }
}

fn decode_stage2(phash: Vec<u8>, sobel: Vec<u8>) -> Result<ImageStage2, CentralError> {
    let phash = fixed::<72>(phash, "二筛 pHash")?;
    let mut phash_parts = [0_u64; 9];
    for (index, bytes) in phash.chunks_exact(8).enumerate() {
        phash_parts[index] = u64::from_le_bytes(bytes.try_into().expect("固定八字节"));
    }
    let sobel = fixed::<512>(sobel, "二筛 Sobel")?;
    let mut histogram = [0.0_f32; 128];
    for (index, bytes) in sobel.chunks_exact(4).enumerate() {
        histogram[index] = f32::from_le_bytes(bytes.try_into().expect("固定四字节"));
    }
    if histogram.iter().any(|value| !value.is_finite()) {
        return Err(CentralError::InvalidState("中心 Sobel 包含非有限数".into()));
    }
    Ok(ImageStage2 {
        phash_parts,
        sobel: histogram,
    })
}

fn fixed<const N: usize>(value: Vec<u8>, field: &str) -> Result<[u8; N], CentralError> {
    value
        .try_into()
        .map_err(|_| CentralError::InvalidState(format!("{field} 长度不是 {N}")))
}

fn fixed_md5(value: Vec<u8>) -> Result<[u8; 16], CentralError> {
    fixed(value, "中心 MD5")
}

fn non_negative(value: i64, field: &str) -> Result<u64, CentralError> {
    u64::try_from(value).map_err(|_| CentralError::InvalidState(format!("{field} 为负数")))
}

fn positive_u32(value: i32, field: &str) -> Result<u32, CentralError> {
    u32::try_from(value)
        .ok()
        .filter(|value| *value > 0)
        .ok_or_else(|| CentralError::InvalidState(format!("{field} 无效")))
}

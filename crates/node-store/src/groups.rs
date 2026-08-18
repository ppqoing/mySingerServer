//! 重复组的原子替换，以及不受删除影响的稳定游标分页。

use dedup_core::{AnalysisRunId, ContentKey, LocationKey, MachineId, NormalizedPath};
use rusqlite::params;

use crate::{NodeStore, StoreError, open::fixed_bytes, open::sqlite_integer};

/// 重复组的判定种类。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GroupKind {
    /// MD5 与文件大小完全相同。
    Exact,
    /// 图片两层相似判定通过。
    Image,
    /// 视频六帧平均两层判定通过。
    Video,
}

/// 替换分组结果时写入的一个活动成员。
#[derive(Clone, Debug, PartialEq)]
pub struct GroupMemberWrite {
    /// 物理机器和规范路径。
    pub location: LocationKey,
    /// 成员内容键。
    pub content: ContentKey,
    /// 是否是代表文件位置。
    pub representative: bool,
    /// 与代表直接比较的一筛得分。
    pub stage1_score: f64,
    /// 与代表直接比较时通过的 pHash 分块数。
    pub phash_passed_parts: Option<u8>,
    /// 与代表直接比较的联合二筛分数。
    pub stage2_score: Option<f64>,
}

impl GroupMemberWrite {
    /// 创建默认满分的成员；相似分组可再覆盖直接比较分数。
    pub const fn new(location: LocationKey, content: ContentKey, representative: bool) -> Self {
        Self {
            location,
            content,
            representative,
            stage1_score: 1.0,
            phash_passed_parts: None,
            stage2_score: None,
        }
    }
}

/// 一次分析最终写入的一个重复组。
#[derive(Clone, Debug, PartialEq)]
pub struct GroupWrite {
    /// UUID v7 字符串形式的组 ID。
    pub group_id: String,
    /// 精确、图片或视频。
    pub kind: GroupKind,
    /// 代表文件的内容键。
    pub representative: ContentKey,
    /// 至少两个且仅一个标记为代表的位置。
    pub members: Vec<GroupMemberWrite>,
}

/// 分组列表页中的稳定摘要。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct StoredGroup {
    /// 组 ID。
    pub group_id: String,
    /// 组种类。
    pub kind: GroupKind,
    /// 当前代表内容；删除代表后会在原组内更新。
    pub representative: ContentKey,
}

/// 一个重复组分页结果。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GroupPage {
    /// 当前页，按 `(kind,representative,group_id)` 排序。
    pub items: Vec<StoredGroup>,
    /// 还有下一页时返回的不透明十六进制游标。
    pub next_cursor: Option<String>,
}

/// 重复组内的一个活动位置。
#[derive(Clone, Debug, PartialEq)]
pub struct StoredGroupMember {
    /// 机器与规范路径。
    pub location: LocationKey,
    /// 内容键。
    pub content: ContentKey,
    /// 当前是否为代表文件。
    pub representative: bool,
    /// 一筛直接得分。
    pub stage1_score: f64,
    /// 通过 pHash 分块数。
    pub phash_passed_parts: Option<u8>,
    /// 联合二筛直接得分。
    pub stage2_score: Option<f64>,
}

/// 组成员分页结果。
#[derive(Clone, Debug, PartialEq)]
pub struct GroupMemberPage {
    /// 当前页，按 `(machine_id,normalized_path)` 排序。
    pub items: Vec<StoredGroupMember>,
    /// 还有下一页时返回的不透明十六进制游标。
    pub next_cursor: Option<String>,
}

impl NodeStore {
    /// 原子替换一次运行的全部组；不做传递扩组，直接保存算法输出。
    pub fn replace_groups(
        &mut self,
        run_id: AnalysisRunId,
        groups: &[GroupWrite],
    ) -> Result<(), StoreError> {
        let transaction = self.connection.transaction()?;
        transaction.execute(
            "DELETE FROM duplicate_groups WHERE analysis_run_id=?1",
            [run_id.as_uuid().to_string()],
        )?;
        for group in groups {
            validate_group(group)?;
            transaction.execute(
                "INSERT INTO duplicate_groups(
                   analysis_run_id,group_id,group_kind,representative_md5,representative_size)
                 VALUES(?1,?2,?3,?4,?5)",
                params![
                    run_id.as_uuid().to_string(),
                    group.group_id,
                    group.kind.as_str(),
                    group.representative.md5().as_slice(),
                    sqlite_integer(group.representative.file_size())?
                ],
            )?;
            for member in &group.members {
                transaction.execute(
                    "INSERT INTO group_members(
                       analysis_run_id,group_id,machine_id,normalized_path,md5,file_size,
                       representative,stage1_score,phash_passed_parts,stage2_score,active)
                     VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9,?10,1)",
                    params![
                        run_id.as_uuid().to_string(),
                        group.group_id,
                        member.location.machine_id().as_str(),
                        member.location.normalized_path().as_str(),
                        member.content.md5().as_slice(),
                        sqlite_integer(member.content.file_size())?,
                        i64::from(member.representative),
                        member.stage1_score,
                        member.phash_passed_parts,
                        member.stage2_score
                    ],
                )?;
            }
        }
        transaction.commit()?;
        Ok(())
    }

    /// 使用固定三元组游标分页读取组；游标所指组删除后仍可继续。
    pub fn page_groups(
        &self,
        run_id: AnalysisRunId,
        cursor: Option<&str>,
        limit: usize,
    ) -> Result<GroupPage, StoreError> {
        if limit == 0 {
            return Err(StoreError::EmptyPageLimit);
        }
        let cursor = cursor.map(decode_group_cursor).transpose()?;
        let (kind, md5, size, group_id) = cursor
            .map(|value| {
                (
                    Some(value.0),
                    Some(value.1.to_vec()),
                    Some(value.2 as i64),
                    Some(value.3),
                )
            })
            .unwrap_or((None, None, None, None));
        let mut statement = self.connection.prepare_cached(
            "SELECT group_id,group_kind,representative_md5,representative_size
             FROM duplicate_groups
             WHERE analysis_run_id=?1 AND (
               ?2 IS NULL OR group_kind>?2 OR
               (group_kind=?2 AND representative_md5>?3) OR
               (group_kind=?2 AND representative_md5=?3 AND representative_size>?4) OR
               (group_kind=?2 AND representative_md5=?3 AND representative_size=?4 AND group_id>?5)
             )
             ORDER BY group_kind,representative_md5,representative_size,group_id
             LIMIT ?6",
        )?;
        let raw = statement
            .query_map(
                params![
                    run_id.as_uuid().to_string(),
                    kind,
                    md5,
                    size,
                    group_id,
                    i64::try_from(limit + 1).map_err(|_| StoreError::EmptyPageLimit)?
                ],
                |row| {
                    Ok((
                        row.get::<_, String>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, Vec<u8>>(2)?,
                        row.get::<_, i64>(3)?,
                    ))
                },
            )?
            .collect::<Result<Vec<_>, _>>()?;
        let mut items = raw
            .into_iter()
            .map(|(group_id, kind, md5, size)| {
                Ok(StoredGroup {
                    group_id,
                    kind: GroupKind::parse(&kind)?,
                    representative: ContentKey::new(
                        fixed_bytes(md5, "duplicate_groups.representative_md5")?,
                        size as u64,
                    ),
                })
            })
            .collect::<Result<Vec<_>, StoreError>>()?;
        let has_more = items.len() > limit;
        items.truncate(limit);
        let next_cursor = has_more
            .then(|| items.last().map(encode_stored_group_cursor))
            .flatten();
        Ok(GroupPage { items, next_cursor })
    }

    /// 使用位置二元组游标分页读取一个组的当前活动成员。
    pub fn page_group_members(
        &self,
        run_id: AnalysisRunId,
        group_id: &str,
        cursor: Option<&str>,
        limit: usize,
    ) -> Result<GroupMemberPage, StoreError> {
        if limit == 0 {
            return Err(StoreError::EmptyPageLimit);
        }
        let cursor = cursor.map(decode_member_cursor).transpose()?;
        let (machine, path) = cursor
            .map(|value| (Some(value.0), Some(value.1)))
            .unwrap_or((None, None));
        let mut statement = self.connection.prepare_cached(
            "SELECT machine_id,normalized_path,md5,file_size,representative,
                    stage1_score,phash_passed_parts,stage2_score
             FROM group_members
             WHERE analysis_run_id=?1 AND group_id=?2 AND active=1 AND (
               ?3 IS NULL OR machine_id>?3 OR
               (machine_id=?3 AND normalized_path>?4)
             )
             ORDER BY machine_id,normalized_path LIMIT ?5",
        )?;
        let raw = statement
            .query_map(
                params![
                    run_id.as_uuid().to_string(),
                    group_id,
                    machine,
                    path,
                    i64::try_from(limit + 1).map_err(|_| StoreError::EmptyPageLimit)?
                ],
                |row| {
                    Ok((
                        row.get::<_, String>(0)?,
                        row.get::<_, String>(1)?,
                        row.get::<_, Vec<u8>>(2)?,
                        row.get::<_, i64>(3)?,
                        row.get::<_, i64>(4)?,
                        row.get::<_, f64>(5)?,
                        row.get::<_, Option<u8>>(6)?,
                        row.get::<_, Option<f64>>(7)?,
                    ))
                },
            )?
            .collect::<Result<Vec<_>, _>>()?;
        let mut items = raw
            .into_iter()
            .map(|row| {
                Ok(StoredGroupMember {
                    location: LocationKey::new(
                        MachineId::parse(&row.0)?,
                        NormalizedPath::new(row.1)?,
                    ),
                    content: ContentKey::new(
                        fixed_bytes(row.2, "group_members.md5")?,
                        row.3 as u64,
                    ),
                    representative: row.4 != 0,
                    stage1_score: row.5,
                    phash_passed_parts: row.6,
                    stage2_score: row.7,
                })
            })
            .collect::<Result<Vec<_>, StoreError>>()?;
        let has_more = items.len() > limit;
        items.truncate(limit);
        let next_cursor = has_more
            .then(|| items.last().map(encode_stored_member_cursor))
            .flatten();
        Ok(GroupMemberPage { items, next_cursor })
    }
}

fn validate_group(group: &GroupWrite) -> Result<(), StoreError> {
    if group.members.len() < 2 {
        return Err(StoreError::InvalidState("重复组至少需要两个成员".into()));
    }
    let representatives = group
        .members
        .iter()
        .filter(|member| member.representative && member.content == group.representative)
        .count();
    if representatives != 1
        || group
            .members
            .iter()
            .filter(|member| member.representative)
            .count()
            != 1
    {
        return Err(StoreError::InvalidState(
            "重复组必须有且仅有一个匹配代表内容的代表位置".into(),
        ));
    }
    if group.members.iter().any(|member| {
        !member.stage1_score.is_finite()
            || member.stage2_score.is_some_and(|score| !score.is_finite())
    }) {
        return Err(StoreError::NonFiniteScore);
    }
    Ok(())
}

impl GroupKind {
    pub(crate) const fn as_str(self) -> &'static str {
        match self {
            Self::Exact => "exact",
            Self::Image => "image",
            Self::Video => "video",
        }
    }

    fn parse(value: &str) -> Result<Self, StoreError> {
        match value {
            "exact" => Ok(Self::Exact),
            "image" => Ok(Self::Image),
            "video" => Ok(Self::Video),
            _ => Err(StoreError::InvalidState(format!("未知重复组类型: {value}"))),
        }
    }
}

fn encode_stored_group_cursor(group: &StoredGroup) -> String {
    let mut bytes = Vec::new();
    bytes.push(group.kind.as_str().len() as u8);
    bytes.extend_from_slice(group.kind.as_str().as_bytes());
    bytes.extend_from_slice(&group.representative.md5());
    bytes.extend_from_slice(&group.representative.file_size().to_be_bytes());
    bytes.extend_from_slice(&(group.group_id.len() as u16).to_be_bytes());
    bytes.extend_from_slice(group.group_id.as_bytes());
    hex_encode(&bytes)
}

fn decode_group_cursor(cursor: &str) -> Result<(String, [u8; 16], u64, String), StoreError> {
    let bytes = hex_decode(cursor)?;
    let mut at = 0;
    let kind_len = take_u8(&bytes, &mut at)? as usize;
    let kind = take_text(&bytes, &mut at, kind_len)?;
    GroupKind::parse(&kind)?;
    let md5 = take_array::<16>(&bytes, &mut at)?;
    let size = u64::from_be_bytes(take_array::<8>(&bytes, &mut at)?);
    let group_len = u16::from_be_bytes(take_array::<2>(&bytes, &mut at)?) as usize;
    let group = take_text(&bytes, &mut at, group_len)?;
    if at != bytes.len() {
        return Err(StoreError::InvalidCursor);
    }
    Ok((kind, md5, size, group))
}

fn encode_stored_member_cursor(member: &StoredGroupMember) -> String {
    let machine = member.location.machine_id().as_str().as_bytes();
    let path = member.location.normalized_path().as_str().as_bytes();
    let mut bytes = Vec::new();
    bytes.push(machine.len() as u8);
    bytes.extend_from_slice(machine);
    bytes.extend_from_slice(&(path.len() as u32).to_be_bytes());
    bytes.extend_from_slice(path);
    hex_encode(&bytes)
}

fn decode_member_cursor(cursor: &str) -> Result<(String, String), StoreError> {
    let bytes = hex_decode(cursor)?;
    let mut at = 0;
    let machine_len = take_u8(&bytes, &mut at)? as usize;
    let machine = take_text(&bytes, &mut at, machine_len)?;
    MachineId::parse(&machine)?;
    let path_len = u32::from_be_bytes(take_array::<4>(&bytes, &mut at)?) as usize;
    let path = take_text(&bytes, &mut at, path_len)?;
    NormalizedPath::new(&path)?;
    if at != bytes.len() {
        return Err(StoreError::InvalidCursor);
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

fn hex_decode(value: &str) -> Result<Vec<u8>, StoreError> {
    if !value.len().is_multiple_of(2) {
        return Err(StoreError::InvalidCursor);
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

fn hex_nibble(value: u8) -> Result<u8, StoreError> {
    match value {
        b'0'..=b'9' => Ok(value - b'0'),
        b'a'..=b'f' => Ok(value - b'a' + 10),
        _ => Err(StoreError::InvalidCursor),
    }
}

fn take_u8(bytes: &[u8], at: &mut usize) -> Result<u8, StoreError> {
    let value = *bytes.get(*at).ok_or(StoreError::InvalidCursor)?;
    *at += 1;
    Ok(value)
}

fn take_array<const N: usize>(bytes: &[u8], at: &mut usize) -> Result<[u8; N], StoreError> {
    let end = at.checked_add(N).ok_or(StoreError::InvalidCursor)?;
    let value = bytes
        .get(*at..end)
        .ok_or(StoreError::InvalidCursor)?
        .try_into()
        .map_err(|_| StoreError::InvalidCursor)?;
    *at = end;
    Ok(value)
}

fn take_text(bytes: &[u8], at: &mut usize, length: usize) -> Result<String, StoreError> {
    let end = at.checked_add(length).ok_or(StoreError::InvalidCursor)?;
    let text = std::str::from_utf8(bytes.get(*at..end).ok_or(StoreError::InvalidCursor)?)
        .map_err(|_| StoreError::InvalidCursor)?
        .to_owned();
    *at = end;
    Ok(text)
}

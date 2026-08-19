//! 冻结输入上的精确重复分组。

use std::collections::BTreeMap;

use dedup_core::{ContentKey, GroupId, LocationKey};
use dedup_node_store::{AnalysisInput, GroupKind, GroupMemberWrite, GroupWrite};

/// 按 ContentKey 聚合位置；至少两个活动位置才形成精确组。
pub(crate) fn exact_groups(inputs: &[AnalysisInput]) -> Vec<GroupWrite> {
    let mut locations = BTreeMap::<ContentKey, Vec<LocationKey>>::new();
    for input in inputs {
        locations
            .entry(input.content)
            .or_default()
            .push(input.location.clone());
    }
    locations
        .into_iter()
        .filter_map(|(content, mut locations)| {
            locations.sort();
            locations.dedup();
            if locations.len() < 2 {
                return None;
            }
            let members = locations
                .into_iter()
                .enumerate()
                .map(|(index, location)| GroupMemberWrite::new(location, content, index == 0))
                .collect();
            Some(GroupWrite {
                group_id: GroupId::new().as_uuid().to_string(),
                kind: GroupKind::Exact,
                representative: content,
                members,
            })
        })
        .collect()
}

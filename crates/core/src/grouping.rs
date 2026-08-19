//! 本地和中心分析共用的确定性代表直连分组。

use std::collections::{BTreeMap, BTreeSet};

use crate::ContentKey;

/// 一条已经通过最终联合二筛的无向内容边。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct SimilarityEdge {
    /// 规范排序后的较小内容键。
    pub left: ContentKey,
    /// 规范排序后的较大内容键。
    pub right: ContentKey,
    /// 两端一筛得分。
    pub stage1_score: f64,
    /// 图片通过的 pHash 分块数；视频可为空。
    pub phash_passed_parts: Option<u8>,
    /// Sobel 分数或视频六帧联合平均分。
    pub stage2_score: f64,
}

impl SimilarityEdge {
    /// 创建规范排序的无向边。
    pub fn new(
        first: ContentKey,
        second: ContentKey,
        stage1_score: f64,
        phash_passed_parts: Option<u8>,
        stage2_score: f64,
    ) -> Self {
        let (left, right) = if first < second {
            (first, second)
        } else {
            (second, first)
        };
        Self {
            left,
            right,
            stage1_score,
            phash_passed_parts,
            stage2_score,
        }
    }
}

/// 一个内容在代表分组中的位置和相对代表证据。
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct RepresentativeMember {
    /// 成员内容键。
    pub content: ContentKey,
    /// 代表自身为 `None`，其他成员保存与代表直接通过的边。
    pub evidence: Option<SimilarityEdge>,
}

/// 由最小未分组内容作为代表形成的一个直连组。
#[derive(Clone, Debug, PartialEq)]
pub struct RepresentativeGroup {
    /// 组内最小且作为所有成员直接比较中心的内容键。
    pub representative: ContentKey,
    /// 代表在首位，后续成员按内容键升序。
    pub members: Vec<RepresentativeMember>,
}

/// 按稳定键遍历，只加入与当前代表直接有最终通过边的未分组内容。
///
/// 本函数不做连通分量或传递闭包；一旦内容加入较早代表的组，后续代表不能再次使用它。
pub fn group_by_representative(
    contents: &[ContentKey],
    passed_edges: &[SimilarityEdge],
) -> Vec<RepresentativeGroup> {
    let mut remaining = contents.iter().copied().collect::<BTreeSet<_>>();
    let edges = passed_edges
        .iter()
        .copied()
        .map(|edge| ((edge.left, edge.right), edge))
        .collect::<BTreeMap<_, _>>();
    let mut groups = Vec::new();
    while let Some(representative) = remaining.pop_first() {
        let direct = remaining
            .iter()
            .filter_map(|content| {
                let pair = (representative.min(*content), representative.max(*content));
                edges.get(&pair).copied().map(|edge| (*content, edge))
            })
            .collect::<Vec<_>>();
        if direct.is_empty() {
            continue;
        }
        let mut members = vec![RepresentativeMember {
            content: representative,
            evidence: None,
        }];
        for (content, edge) in direct {
            remaining.remove(&content);
            members.push(RepresentativeMember {
                content,
                evidence: Some(edge),
            });
        }
        groups.push(RepresentativeGroup {
            representative,
            members,
        });
    }
    groups
}

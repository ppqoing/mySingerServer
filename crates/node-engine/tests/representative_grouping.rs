use dedup_core::{
    ContentKey, DisplayPath, LocationKey, MachineId, MediaKind, NormalizedPath, SimilarityEdge,
    group_by_representative,
};
use dedup_node_engine::analysis::{
    group_analysis_results,
    model::{AnalysisCandidate, AnalysisCandidateStatus, AnalysisPairKind, ScanAnalysisInput},
};

#[test]
fn grouping_uses_only_direct_edges_from_the_representative() {
    let a = key(1);
    let b = key(2);
    let c = key(3);
    let groups = group_by_representative(
        &[a, b, c],
        &[
            SimilarityEdge::new(a, b, 0.95, Some(9), 0.92),
            SimilarityEdge::new(b, c, 0.94, Some(9), 0.91),
        ],
    );

    assert_eq!(groups.len(), 1);
    assert_eq!(groups[0].representative, a);
    assert_eq!(
        groups[0]
            .members
            .iter()
            .map(|member| member.content)
            .collect::<Vec<_>>(),
        vec![a, b]
    );
}

#[test]
fn each_content_enters_at_most_one_group_and_singletons_are_omitted() {
    let a = key(1);
    let b = key(2);
    let c = key(3);
    let d = key(4);
    let groups = group_by_representative(
        &[d, c, b, a],
        &[
            SimilarityEdge::new(a, c, 0.9, Some(8), 0.9),
            SimilarityEdge::new(a, b, 0.9, Some(8), 0.9),
            SimilarityEdge::new(b, d, 0.9, Some(8), 0.9),
        ],
    );

    assert_eq!(groups.len(), 1);
    assert_eq!(groups[0].representative, a);
    assert_eq!(groups[0].members.len(), 3);
    assert!(groups[0].members.iter().all(|member| member.content != d));
}

#[test]
fn runtime_grouping_preserves_display_paths_and_direct_evidence() {
    let representative = key(1);
    let member = key(2);
    let representative_first = location(r"C:\Root\A.JPG");
    let member_first = location(r"D:\Root\C.JPG");
    let inputs = vec![
        ScanAnalysisInput {
            content: representative,
            location: representative_first.clone(),
            display_path: display_path(r"C:\Root\A.JPG"),
            media_kind: MediaKind::Image,
        },
        ScanAnalysisInput {
            content: member,
            location: member_first.clone(),
            display_path: display_path(r"D:\Root\C.JPG"),
            media_kind: MediaKind::Image,
        },
    ];
    let candidates = vec![AnalysisCandidate {
        kind: AnalysisPairKind::Image,
        left: representative,
        right: member,
        stage1_score: 0.91,
        phash_passed_parts: Some(8),
        stage2_score: Some(0.87),
        status: AnalysisCandidateStatus::Passed,
    }];

    let groups = group_analysis_results(&inputs, &candidates);

    assert_eq!(groups.len(), 1);
    assert_eq!(groups[0].representative, representative);
    assert_eq!(groups[0].members.len(), 2);
    let first = &groups[0].members[0];
    assert!(first.representative);
    assert_eq!(first.location, representative_first);
    assert_eq!(
        first.display_path.as_path(),
        std::path::Path::new(r"C:\Root\A.JPG")
    );
    assert_eq!(first.stage1_score, 1.0);
    assert_eq!(first.phash_passed_parts, None);
    assert_eq!(first.stage2_score, None);
    let second = groups[0]
        .members
        .iter()
        .find(|member| member.location == member_first)
        .expect("第一个成员位置必须保留");
    assert!(!second.representative);
    assert_eq!(
        second.display_path.as_path(),
        std::path::Path::new(r"D:\Root\C.JPG")
    );
    assert_eq!(second.stage1_score, 0.91);
    assert_eq!(second.phash_passed_parts, Some(8));
    assert_eq!(second.stage2_score, Some(0.87));
}

fn key(value: u8) -> ContentKey {
    ContentKey::new([value; 16], 100 + u64::from(value))
}

fn location(path: &str) -> LocationKey {
    LocationKey::new(
        MachineId::parse(&"a".repeat(64)).unwrap(),
        NormalizedPath::new(path).unwrap(),
    )
}

fn display_path(path: &str) -> DisplayPath {
    DisplayPath::new(path).unwrap()
}

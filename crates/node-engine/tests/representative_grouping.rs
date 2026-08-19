use dedup_core::{ContentKey, SimilarityEdge, group_by_representative};

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

fn key(value: u8) -> ContentKey {
    ContentKey::new([value; 16], 100 + u64::from(value))
}

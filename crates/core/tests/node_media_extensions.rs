//! Node 媒体扩展名默认值、规范化和校验行为测试。

use dedup_core::NodeConfig;

#[test]
fn missing_extension_fields_receive_complete_defaults() {
    let loaded = NodeConfig::from_toml("").unwrap();

    assert!(loaded.image_extensions.contains(&"jpg".to_owned()));
    assert!(loaded.image_extensions.contains(&"avif".to_owned()));
    assert!(loaded.image_extensions.contains(&"jxl".to_owned()));
    assert!(loaded.video_extensions.contains(&"mp4".to_owned()));
    assert!(loaded.video_extensions.contains(&"mkv".to_owned()));
    assert!(loaded.video_extensions.contains(&"mxf".to_owned()));
}

#[test]
fn save_normalizes_dots_case_order_and_duplicates() {
    let mut config = NodeConfig::default();
    config.image_extensions = vec![" PNG ".into(), ".JPG".into(), "jpg".into()];
    config.video_extensions = Vec::new();

    let loaded = NodeConfig::from_toml(&config.to_toml().unwrap()).unwrap();

    assert_eq!(
        loaded.image_extensions,
        vec!["jpg".to_owned(), "png".to_owned()],
    );
    assert!(loaded.video_extensions.is_empty());
}

#[test]
fn invalid_extension_token_is_rejected() {
    let mut config = NodeConfig::default();
    config.image_extensions = vec!["bad/path".into()];

    let error = config.to_toml().unwrap_err().to_string();

    assert!(error.contains("image_extensions"));
}

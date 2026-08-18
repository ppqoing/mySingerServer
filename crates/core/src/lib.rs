//! mySingerServer V2 的共享领域内核。
#![warn(missing_docs)]

mod config;
mod error;
mod ids;
mod model;
mod path;
mod thresholds;

pub use config::{DesktopConfig, NodeConfig, NodeEndpoint};
pub use error::CoreError;
pub use ids::{AnalysisRunId, ContentKey, GroupId, LocationKey, MachineId, TaskId};
pub use model::{DeleteMode, EnumeratorKind, MediaKind};
pub use path::{DisplayPath, NormalizedPath};
pub use thresholds::Thresholds;

/// 返回协议、数据库和发布包共同使用的稳定产品代号。
pub const fn product_id() -> &'static str {
    "mysingerserver-rust-v2"
}

#[cfg(test)]
mod tests {
    use super::{
        AnalysisRunId, ContentKey, CoreError, DeleteMode, DesktopConfig, DisplayPath, GroupId,
        LocationKey, MachineId, NodeConfig, NormalizedPath, TaskId, Thresholds,
    };

    /// 防止产品代号在协议、数据库和发布脚本之间发生漂移。
    #[test]
    fn product_id_is_stable() {
        assert_eq!(super::product_id(), "mysingerserver-rust-v2");
    }

    /// 防止内容键排序绕过 MD5 优先级，破坏稳定分组顺序。
    #[test]
    fn content_key_orders_by_md5_then_size() {
        let a = ContentKey::new([1; 16], 20);
        let b = ContentKey::new([2; 16], 10);
        assert!(a < b);
    }

    /// 防止已确认的匹配阈值在配置默认值中静默漂移。
    #[test]
    fn thresholds_match_confirmed_defaults() {
        let thresholds = Thresholds::default();
        assert_eq!(thresholds.pdq_quality_min, 50);
        assert_eq!(thresholds.phash_min_passed_parts, 8);
        assert_eq!(thresholds.video_min_valid_frames, 4);
        assert_eq!(thresholds.video_stage2_min, 0.80);
    }

    /// 防止业务任务 ID 退回随机 v4，失去按创建时间稳定排序的属性。
    #[test]
    fn business_ids_are_uuid_v7() {
        assert_eq!(TaskId::new().as_uuid().get_version_num(), 7);
        assert_eq!(AnalysisRunId::new().as_uuid().get_version_num(), 7);
        assert_eq!(GroupId::new().as_uuid().get_version_num(), 7);
    }

    /// 防止把非规范机器 ID 接受为跨节点身份。
    #[test]
    fn machine_id_accepts_only_lowercase_sha256_hex() {
        let valid = "73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae";
        assert_eq!(MachineId::parse(valid).unwrap().as_str(), valid);
        assert!(MachineId::parse(&valid.to_uppercase()).is_err());
        assert!(MachineId::parse("abc").is_err());
    }

    /// 防止越界阈值进入分析快照后才在算法内部失败。
    #[test]
    fn thresholds_validate_once_at_configuration_boundary() {
        let invalid = Thresholds {
            phash_min_passed_parts: 0,
            ..Thresholds::default()
        };
        assert!(matches!(
            invalid.validate(),
            Err(CoreError::InvalidThreshold {
                field: "phash_min_passed_parts",
                ..
            })
        ));
    }

    /// 防止空桌面配置失去默认本机节点和回收站删除模式。
    #[test]
    fn desktop_toml_uses_confirmed_defaults() {
        let config = DesktopConfig::from_toml("").unwrap();
        assert_eq!(config.nodes[0].to_string(), "127.0.0.1:39091");
        assert_eq!(config.delete_mode, DeleteMode::RecycleBin);
    }

    /// 防止节点接受无法启动 Worker 的零并行度配置。
    #[test]
    fn node_toml_rejects_zero_workers() {
        let error = NodeConfig::from_toml("worker_count = 0").unwrap_err();
        assert!(matches!(
            error,
            CoreError::InvalidConfig {
                field: "worker_count",
                ..
            }
        ));
    }

    /// 防止以字符串前缀判断目录时把相邻目录误认为子目录。
    #[test]
    fn normalized_path_uses_case_insensitive_components() {
        let root = NormalizedPath::new(r"C:\Media\").unwrap();
        let child = NormalizedPath::new(r"c:\media\Album\..\Song.mp3").unwrap();
        let neighbor = NormalizedPath::new(r"C:\Media2\Song.mp3").unwrap();
        assert!(child.is_within(&root));
        assert!(!neighbor.is_within(&root));
        assert_eq!(child.as_str(), r"C:\MEDIA\SONG.MP3");
    }

    /// 防止扩展长度前缀让同一磁盘或 UNC 路径形成两个缓存键。
    #[test]
    fn normalized_path_unifies_verbatim_and_unc_forms() {
        assert_eq!(
            NormalizedPath::new(r"\\?\C:\Media").unwrap(),
            NormalizedPath::new(r"c:\media\").unwrap()
        );
        assert_eq!(
            NormalizedPath::new(r"\\?\UNC\server\share\Media\").unwrap(),
            NormalizedPath::new(r"\\SERVER\SHARE\media").unwrap()
        );
    }

    /// 防止相对路径进入跨机器缓存键并依赖进程当前目录。
    #[test]
    fn normalized_path_rejects_relative_input() {
        assert!(NormalizedPath::new(r"Media\song.mp3").is_err());
    }

    /// 防止位置键混入显示路径大小写或遗漏所属机器。
    #[test]
    fn location_key_uses_machine_and_normalized_path() {
        let machine =
            MachineId::parse("73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae")
                .unwrap();
        let path = NormalizedPath::new(r"c:\media\song.mp3").unwrap();
        let key = LocationKey::new(machine.clone(), path.clone());
        assert_eq!(key.machine_id(), &machine);
        assert_eq!(key.normalized_path(), &path);
    }

    /// 防止界面和实际文件访问丢失用户原始路径大小写。
    #[test]
    fn display_path_preserves_original_spelling() {
        let display = DisplayPath::new(r"C:\Media\My Song.mp3").unwrap();
        assert_eq!(
            display.as_path(),
            std::path::Path::new(r"C:\Media\My Song.mp3")
        );
    }

    /// 防止检查扩展路径前缀时在 UTF-8 多字节文件名中间切片并崩溃。
    #[test]
    fn normalized_path_supports_unicode_windows_names() {
        let path = NormalizedPath::new(r"C:\媒体\歌曲.mp3").unwrap();
        assert_eq!(path.as_str(), r"C:\媒体\歌曲.MP3");
    }
}

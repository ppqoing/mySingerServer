//! 固定图片和 H.264 视频的探测、定位和紧凑 RGB24 输出测试。

#![cfg(windows)]

use std::{env, path::PathBuf};

use dedup_core::MediaKind;
use dedup_media_ffmpeg::{Ffmpeg, required_dlls};

#[test]
fn probes_image_and_video_without_executables() {
    let Some((ffmpeg, _runtime)) = load_fixture() else {
        return;
    };
    let fixtures = fixture_root();
    let image = ffmpeg.probe_media(&fixtures.join("image.jpg")).unwrap();
    assert_eq!(image.media_kind, MediaKind::Image);
    assert_eq!((image.width, image.height), (96, 64));
    assert_eq!(image.duration_ms, None);

    let video = ffmpeg.probe_media(&fixtures.join("video-12s.mp4")).unwrap();
    assert_eq!(video.media_kind, MediaKind::Video);
    assert_eq!((video.width, video.height), (160, 90));
    assert!(
        video
            .duration_ms
            .is_some_and(|value| (2_400..=2_450).contains(&value))
    );
}

#[test]
fn decodes_beginning_and_end_to_tight_rgb24() {
    let Some((ffmpeg, _runtime)) = load_fixture() else {
        return;
    };
    let video = fixture_root().join("video-12s.mp4");
    for position in [1.0 / 12.0, 11.0 / 12.0] {
        let frame = ffmpeg.decode_frame_at(&video, position).unwrap();
        assert_eq!((frame.width, frame.height), (160, 90));
        assert_eq!(frame.rgb24.len(), 160 * 90 * 3);
    }
}

fn fixture_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("..")
        .join("..")
        .join("tests")
        .join("fixtures")
        .join("media")
}

fn load_fixture() -> Option<(Ffmpeg, tempfile::TempDir)> {
    let source = env::var_os("DEDUP_FFMPEG_TEST_SOURCE_DIR").map(PathBuf::from)?;
    let directory = tempfile::tempdir().unwrap();
    let worker = directory.path().join("worker.exe");
    let runtime = directory.path().join("runtime").join("ffmpeg");
    std::fs::create_dir_all(&runtime).unwrap();
    std::fs::write(&worker, []).unwrap();
    for name in required_dlls() {
        std::fs::copy(source.join(name), runtime.join(name)).unwrap();
    }
    let ffmpeg = Ffmpeg::load_from_worker_executable(&worker).unwrap();
    Some((ffmpeg, directory))
}

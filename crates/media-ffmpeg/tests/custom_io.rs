//! FFmpeg 自定义 AVIO 从内存 source 探测和解码的契约。

#![cfg(windows)]

use std::{
    env,
    io::{self, Cursor, Read, Seek, SeekFrom},
    path::PathBuf,
};

use dedup_core::MediaKind;
use dedup_media_ffmpeg::{Ffmpeg, SeekableMediaSource, required_dlls};

struct MemorySource {
    cursor: Cursor<Vec<u8>>,
}

impl MemorySource {
    fn from_file(path: PathBuf) -> Self {
        Self {
            cursor: Cursor::new(std::fs::read(path).unwrap()),
        }
    }
}

impl SeekableMediaSource for MemorySource {
    fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
        self.cursor.read(buffer)
    }

    fn seek(&mut self, position: SeekFrom) -> io::Result<u64> {
        self.cursor.seek(position)
    }

    fn len(&self) -> u64 {
        self.cursor.get_ref().len() as u64
    }
}

#[test]
fn custom_source_probes_image_and_video_without_a_media_path() {
    let Some((ffmpeg, _runtime)) = load_fixture() else {
        return;
    };
    let fixtures = fixture_root();
    let mut image = MemorySource::from_file(fixtures.join("image.jpg"));
    let mut video = MemorySource::from_file(fixtures.join("video-12s.mp4"));

    let image_probe = ffmpeg.probe_source(&mut image).unwrap();
    assert_eq!(image_probe.media_kind, MediaKind::Image);
    assert_eq!((image_probe.width, image_probe.height), (96, 64));

    let video_probe = ffmpeg.probe_source(&mut video).unwrap();
    assert_eq!(video_probe.media_kind, MediaKind::Video);
    video.seek(SeekFrom::Start(0)).unwrap();
    let frame = ffmpeg.decode_frame_from_source(&mut video, 0.5).unwrap();
    assert_eq!((frame.width, frame.height), (160, 90));
    assert_eq!(frame.rgb24.len(), 160 * 90 * 3);
}

#[test]
fn custom_source_decodes_with_explicit_decoder_thread_budget() {
    let Some((ffmpeg, _runtime)) = load_fixture() else {
        return;
    };
    let fixtures = fixture_root();
    let mut video = MemorySource::from_file(fixtures.join("video-12s.mp4"));

    let probe = ffmpeg.probe_source_with_threads(&mut video, 3).unwrap();
    assert_eq!(probe.media_kind, MediaKind::Video);
    video.seek(SeekFrom::Start(0)).unwrap();
    let frame = ffmpeg
        .decode_frame_from_source_with_threads(&mut video, 0.5, 3)
        .unwrap();

    assert_eq!((frame.width, frame.height), (160, 90));
    assert_eq!(frame.rgb24.len(), 160 * 90 * 3);
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
    Some((
        Ffmpeg::load_from_worker_executable(&worker).unwrap(),
        directory,
    ))
}

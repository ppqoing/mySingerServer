use dedup_desktop_ui::{
    MainWindow, UiGroupRow, UiMemberRow, UiNodeRow, UiRuntimeFailureRow, UiRuntimeStageRow,
    UiRuntimeWorkerRow, UiTaskRow,
};
use i_slint_backend_testing::{ElementHandle, TestingBackend, TestingBackendOptions};
use slint::{Color, ComponentHandle, ModelRc, VecModel};
use std::ffi::OsStr;
use std::path::{Path, PathBuf};

const VIEWS: [(&str, i32, i32, i32, i32, i32); 12] = [
    ("01-overview", 0, 0, 0, 0, 0),
    ("02-nodes", 0, 1, 0, 0, 0),
    ("03-scan", 1, 0, 0, 0, 0),
    ("04-tasks", 1, 0, 1, 0, 0),
    ("05-exact", 2, 0, 0, 0, 0),
    ("06-similar-images", 3, 0, 0, 0, 0),
    ("07-similar-videos", 4, 0, 0, 0, 0),
    ("08-cross-machine", 5, 0, 0, 0, 0),
    ("09-review", 6, 0, 0, 0, 0),
    ("10-delete-center", 6, 0, 0, 1, 0),
    ("11-settings", 7, 0, 0, 0, 0),
    ("12-diagnostics", 7, 0, 0, 0, 7),
];

const COMPARISONS: [(&str, &str, &str); 6] = [
    ("01-overview-nodes.png", "01-overview", "02-nodes"),
    ("02-scan-tasks.png", "03-scan", "04-tasks"),
    ("03-exact-cross-machine.png", "05-exact", "08-cross-machine"),
    (
        "04-similar-media.png",
        "06-similar-images",
        "07-similar-videos",
    ),
    ("05-review-delete.png", "09-review", "10-delete-center"),
    (
        "06-settings-diagnostics.png",
        "11-settings",
        "12-diagnostics",
    ),
];

enum PreviewDestination {
    TargetDirectory { root: PathBuf },
    Documentation { root: PathBuf },
}

struct VisualFixture {
    nodes: Vec<UiNodeRow>,
    tasks: Vec<UiTaskRow>,
    groups: Vec<UiGroupRow>,
    members: Vec<UiMemberRow>,
}

impl VisualFixture {
    fn full() -> Self {
        Self {
            nodes: vec![
                UiNodeRow {
                    index: 0,
                    name: "本机节点".into(),
                    address: "127.0.0.1:39091".into(),
                    status: "在线".into(),
                    status_color: Color::from_rgb_u8(22, 163, 74),
                    machine_id: "machine-local".into(),
                    worker_text: "1/2 忙碌".into(),
                    task_text: "1 排队 / 1 运行".into(),
                    sync_text: "120 / 125".into(),
                    error_text: "".into(),
                },
                UiNodeRow {
                    index: 1,
                    name: "影像节点".into(),
                    address: "10.0.0.8:39091".into(),
                    status: "离线".into(),
                    status_color: Color::from_rgb_u8(148, 163, 184),
                    machine_id: "machine-image".into(),
                    worker_text: "0/4 忙碌".into(),
                    task_text: "无任务".into(),
                    sync_text: "98 / 98".into(),
                    error_text: "".into(),
                },
                UiNodeRow {
                    index: 2,
                    name: "视频节点".into(),
                    address: "10.0.0.9:39091".into(),
                    status: "错误".into(),
                    status_color: Color::from_rgb_u8(239, 68, 68),
                    machine_id: "machine-video".into(),
                    worker_text: "0/8 忙碌".into(),
                    task_text: "等待连接".into(),
                    sync_text: "—".into(),
                    error_text: "目标机器拒绝连接".into(),
                },
            ],
            tasks: vec![
                UiTaskRow {
                    id: "task-media-scan".into(),
                    runtime_id: "task-media-scan".into(),
                    owner_kind: "node".into(),
                    node_index: 0,
                    machine_id: "machine-local".into(),
                    title: "媒体扫描".into(),
                    stage: "枚举文件".into(),
                    status: "运行中".into(),
                    status_color: Color::from_rgb_u8(59, 130, 246),
                    progress: 35,
                    counts: "7 / 20 · 失败 0 · 跳过 1".into(),
                    stale: false,
                },
                UiTaskRow {
                    id: "task-image-analysis".into(),
                    runtime_id: "task-image-analysis".into(),
                    owner_kind: "desktop".into(),
                    node_index: 1,
                    machine_id: "machine-image".into(),
                    title: "图片分析".into(),
                    stage: "完成".into(),
                    status: "已完成".into(),
                    status_color: Color::from_rgb_u8(22, 163, 74),
                    progress: 100,
                    counts: "18 / 18 · 失败 0 · 跳过 0".into(),
                    stale: false,
                },
                UiTaskRow {
                    id: "task-video-analysis".into(),
                    runtime_id: "task-video-analysis".into(),
                    owner_kind: "node".into(),
                    node_index: 2,
                    machine_id: "machine-video".into(),
                    title: "视频分析".into(),
                    stage: "提取特征".into(),
                    status: "失败".into(),
                    status_color: Color::from_rgb_u8(239, 68, 68),
                    progress: 60,
                    counts: "6 / 10 · 失败 1 · 跳过 0".into(),
                    stale: false,
                },
            ],
            groups: vec![
                UiGroupRow {
                    id: "exact-001".into(),
                    kind: "精确重复".into(),
                    md5: "11111111111111111111111111111111".into(),
                    size: "2.4 GiB".into(),
                    members: 3,
                    reclaimable: "2.4 GiB".into(),
                },
                UiGroupRow {
                    id: "image-001".into(),
                    kind: "相似图片".into(),
                    md5: "22222222222222222222222222222222".into(),
                    size: "38.5 MiB".into(),
                    members: 2,
                    reclaimable: "38.5 MiB".into(),
                },
                UiGroupRow {
                    id: "video-001".into(),
                    kind: "相似视频".into(),
                    md5: "33333333333333333333333333333333".into(),
                    size: "1.1 GiB".into(),
                    members: 2,
                    reclaimable: "1.1 GiB".into(),
                },
            ],
            members: vec![
                member(
                    "machine-local",
                    "D:\\Media\\exact-a.mp4",
                    true,
                    "未决定",
                    true,
                    true,
                    false,
                ),
                member(
                    "machine-image",
                    "E:\\Archive\\exact-b.mp4",
                    false,
                    "保留",
                    true,
                    false,
                    false,
                ),
                member(
                    "machine-video",
                    "F:\\Video\\exact-c.mp4",
                    false,
                    "删除",
                    false,
                    true,
                    true,
                ),
                member(
                    "machine-local",
                    "D:\\Media\\image-a.jpg",
                    true,
                    "保留",
                    true,
                    true,
                    false,
                ),
                member(
                    "machine-image",
                    "E:\\Archive\\image-b.jpg",
                    false,
                    "删除",
                    false,
                    false,
                    true,
                ),
                member(
                    "machine-video",
                    "F:\\Video\\video-a.mp4",
                    false,
                    "未决定",
                    true,
                    true,
                    false,
                ),
            ],
        }
    }

    fn install(&self, window: &MainWindow) {
        window.set_nodes(ModelRc::new(VecModel::from(self.nodes.clone())));
        window.set_tasks(ModelRc::new(VecModel::from(self.tasks.clone())));
        window.set_runtime_detail_title("媒体扫描".into());
        window.set_runtime_detail_machine_id("machine-local".into());
        window.set_runtime_detail_state("运行中".into());
        window.set_runtime_detail_counts("7 / 20 · 失败 0 · 跳过 1".into());
        window.set_runtime_detail_stale(false);
        window.set_runtime_stages(ModelRc::new(VecModel::from(vec![
            UiRuntimeStageRow {
                stage_id: "enumerate".into(),
                name: "枚举文件".into(),
                state: "已完成".into(),
                state_color: Color::from_rgb_u8(22, 163, 74),
                unit: "文件".into(),
                progress: 100,
                counts: "20 / 20".into(),
                speed: "18.4 文件/秒".into(),
                elapsed: "1.1 秒".into(),
                eta: "—".into(),
                failures: "失败 0 · 跳过 0".into(),
            },
            UiRuntimeStageRow {
                stage_id: "read".into(),
                name: "读取文件".into(),
                state: "运行中".into(),
                state_color: Color::from_rgb_u8(59, 130, 246),
                unit: "字节".into(),
                progress: 35,
                counts: "7 / 20".into(),
                speed: "128.0 MiB/s".into(),
                elapsed: "8.4 秒".into(),
                eta: "15.6 秒".into(),
                failures: "失败 0 · 跳过 1".into(),
            },
            UiRuntimeStageRow {
                stage_id: "probe_stage1".into(),
                name: "媒体探测与一筛".into(),
                state: "运行中".into(),
                state_color: Color::from_rgb_u8(59, 130, 246),
                unit: "文件".into(),
                progress: 25,
                counts: "5 / 20".into(),
                speed: "3.5 文件/秒".into(),
                elapsed: "6.2 秒".into(),
                eta: "21.0 秒".into(),
                failures: "失败 1 · 跳过 0".into(),
            },
        ])));
        window.set_runtime_workers(ModelRc::new(VecModel::from(vec![
            UiRuntimeWorkerRow {
                slot: 0,
                identity: "PID 4812 · 槽位 0".into(),
                stage_id: "probe_stage1".into(),
                step: "生成缩略图".into(),
                cache_detail: "复用本地缩略图".into(),
                path: r"D:\Media\Series\Episode-001.mkv".into(),
                disk: "PhysicalDisk 0".into(),
                completed: "12 个文件".into(),
                speed: "3.5 文件/秒".into(),
                phase: "特征计算".into(),
                cpu_weight: "2".into(),
                decoder_threads: "2".into(),
            },
            UiRuntimeWorkerRow {
                slot: 1,
                identity: "PID 4920 · 槽位 1".into(),
                stage_id: "probe_stage1".into(),
                step: "计算图片一筛特征".into(),
                cache_detail: "缓存未命中".into(),
                path: r"E:\Archive\Movie-002.mp4".into(),
                disk: "PhysicalDisk 1".into(),
                completed: "9 个文件".into(),
                speed: "2.8 文件/秒".into(),
                phase: "解码".into(),
                cpu_weight: "1".into(),
                decoder_threads: "1".into(),
            },
        ])));
        window.set_runtime_failures(ModelRc::new(VecModel::from(vec![UiRuntimeFailureRow {
            stage_id: "probe_stage1".into(),
            path: r"D:\Media\Damaged\broken-clip.mp4".into(),
            message: "Worker 意外退出，已跳过文件".into(),
        }])));
        window.set_groups(ModelRc::new(VecModel::from(self.groups.clone())));
        window.set_members(ModelRc::new(VecModel::from(self.members.clone())));
        window.set_online_count(1);
        window.set_running_count(1);
        window.set_indexed_text("120 / 125".into());
        window.set_sync_text("120 / 125".into());
        window.set_cross_status("partial".into());
        window.set_cross_summary("候选 3 · 未决 2 · 二筛任务 1 · 跳过不完整 0".into());
        window.set_selected_group_id("exact-001".into());
        window.set_delete_file_count(2);
        window.set_delete_node_count(2);
        window.set_delete_reclaimable("1.1 GiB".into());
        window.set_delete_mode("回收站".into());
        window.set_delete_can_execute(false);
        window.set_delete_warning("视频节点离线，当前不能确认执行。".into());
        window.set_postgres_status("中心数据库健康".into());
        window.set_postgres_color(Color::from_rgb_u8(22, 163, 74));
        window.set_last_error("目标机器拒绝连接".into());
        window.set_data_path("D:\\Fixture\\mySingerServer\\data\\desktop".into());
        window.set_config_path("D:\\Fixture\\mySingerServer\\data\\desktop\\config.toml".into());
        window.set_logs_path(
            "D:\\Fixture\\mySingerServer\\data\\desktop\\logs\\desktop-current.log".into(),
        );
        window.set_cache_path(
            "D:\\Fixture\\mySingerServer\\data\\desktop\\cache\\contact-sheets".into(),
        );
        // 扫描预览只填充现有表单字段，右侧摘要由这些真实值直接生成。
        window.set_scan_root("D:\\Media".into());
        window.set_scan_node_index(0);
        window.set_enumerator_index(1);
        window.set_filtering_enabled(true);
        window.set_filtering_reason("全部节点任务已进入终态。".into());
        window.set_analysis_task_ids("task-media-scan".into());
    }
}

fn member(
    machine_id: &str,
    path: &str,
    representative: bool,
    review: &str,
    online: bool,
    preview_enabled: bool,
    delete_enabled: bool,
) -> UiMemberRow {
    let review_color = match review {
        "保留" => Color::from_rgb_u8(22, 163, 74),
        "删除" => Color::from_rgb_u8(239, 68, 68),
        _ => Color::from_rgb_u8(107, 114, 128),
    };
    UiMemberRow {
        machine_id: machine_id.into(),
        path: path.into(),
        md5: "0123456789abcdef0123456789abcdef".into(),
        size: "12.8 MiB".into(),
        representative,
        stage1: "0.98".into(),
        phash: "2".into(),
        stage2: "0.96".into(),
        metadata: "3840×2160".into(),
        review: review.into(),
        review_color,
        online,
        preview_enabled,
        delete_enabled,
    }
}

fn install_testing_backend() {
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        threading: false,
        renderer_name: Some("software".into()),
    })))
    .expect("应能安装软件渲染测试后端");
}

fn render_all_views(fixture: &VisualFixture, destination: PreviewDestination) {
    install_testing_backend();
    let requested_views = std::env::var("RUST_V2_PREVIEW_VIEWS")
        .ok()
        .map(|value| {
            value
                .split(',')
                .map(str::trim)
                .filter(|view| !view.is_empty())
                .map(str::to_owned)
                .collect::<Vec<_>>()
        })
        .unwrap_or_default();
    for (size_name, width, height) in [("1440x900", 1440, 900), ("1080x700", 1080, 700)] {
        for (view, current_page, overview_mode, task_mode, review_tab, settings_section) in VIEWS {
            if !requested_views.is_empty() && !requested_views.iter().any(|item| item == view) {
                continue;
            }
            let window = MainWindow::new().expect("应能构造真实 MainWindow");
            fixture.install(&window);
            window.set_current_page(current_page);
            window.set_overview_mode(overview_mode);
            window.set_task_mode(task_mode);
            window.set_review_tab(review_tab);
            window.set_settings_section(settings_section);
            window.show().expect("应能显示真实 MainWindow");
            window
                .window()
                .set_size(slint::PhysicalSize::new(width, height));
            // 节点预览选择真实错误行，令完整错误块和危险动作一并进入视觉验收。
            if view == "02-nodes" {
                ElementHandle::find_by_accessible_label(
                    &window,
                    "节点项：视频节点；10.0.0.9:39091；错误；Worker 0/8 忙碌；任务 等待连接；同步 —",
                )
                .next()
                .expect("节点预览应包含错误状态夹具")
                .invoke_accessible_default_action();
            }
            let snapshot = window
                .window()
                .take_snapshot()
                .expect("软件渲染后应能取得 RGBA8 快照");
            save_snapshot(
                &snapshot,
                &preview_root(&destination)
                    .join(size_name)
                    .join(format!("{view}.png")),
            );
        }
    }
    if requested_views.is_empty() {
        render_document_comparisons(&destination);
    }
}

fn repository_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(Path::parent)
        .expect("桌面 UI crate 应位于工作区 crates 目录")
        .to_path_buf()
}

fn resolve_preview_destination(
    preview_output: Option<&OsStr>,
    cargo_target: Option<&OsStr>,
) -> Result<PreviewDestination, String> {
    if let Some(requested) = preview_output {
        let document_root = repository_root().join("docs/ui-preview/rust-v2");
        let requested_path = PathBuf::from(requested);
        let requested_resolved = requested_path.canonicalize().map_err(|error| {
            format!(
                "RUST_V2_PREVIEW_OUTPUT_INVALID requested={} expected={} error={error}",
                requested_path.display(),
                document_root.display()
            )
        })?;
        let expected_resolved = document_root.canonicalize().map_err(|error| {
            format!(
                "RUST_V2_PREVIEW_OUTPUT_INVALID requested={} expected={} error={error}",
                requested_path.display(),
                document_root.display()
            )
        })?;
        if requested_resolved != expected_resolved {
            return Err(format!(
                "RUST_V2_PREVIEW_OUTPUT_INVALID requested={} expected={}",
                requested_path.display(),
                document_root.display()
            ));
        }
        return Ok(PreviewDestination::Documentation {
            root: document_root,
        });
    }

    let cargo_target = cargo_target
        .map(PathBuf::from)
        .unwrap_or_else(|| repository_root().join("target"));
    Ok(PreviewDestination::TargetDirectory {
        root: cargo_target.join("visual-preview/current"),
    })
}

fn preview_root(destination: &PreviewDestination) -> PathBuf {
    match destination {
        PreviewDestination::TargetDirectory { root } => root.clone(),
        PreviewDestination::Documentation { root } => root.join("after"),
    }
}

fn comparison_root(destination: &PreviewDestination) -> Option<PathBuf> {
    match destination {
        PreviewDestination::TargetDirectory { .. } => None,
        PreviewDestination::Documentation { root } => Some(root.join("comparison")),
    }
}

fn render_document_comparisons(destination: &PreviewDestination) {
    let PreviewDestination::Documentation { root } = destination else {
        return;
    };
    let after_root = preview_root(destination).join("1440x900");
    let output_root = comparison_root(destination).expect("文档模式必须包含对照板目录");
    std::fs::create_dir_all(&output_root).expect("应能创建对照板目录");

    for (reference_name, upper_view, lower_view) in COMPARISONS {
        let reference_path = root.join(reference_name);
        let upper_path = after_root.join(format!("{upper_view}.png"));
        let lower_path = after_root.join(format!("{lower_view}.png"));
        let reference = image::open(&reference_path)
            .unwrap_or_else(|error| panic!("无法读取参考图 {}: {error}", reference_path.display()))
            .to_rgba8();
        let upper = image::open(&upper_path)
            .unwrap_or_else(|error| panic!("无法读取修复后视图 {}: {error}", upper_path.display()))
            .to_rgba8();
        let lower = image::open(&lower_path)
            .unwrap_or_else(|error| panic!("无法读取修复后视图 {}: {error}", lower_path.display()))
            .to_rgba8();
        assert_eq!(
            upper.dimensions(),
            (1440, 900),
            "对照板上半区必须来自 1440x900 夹具"
        );
        assert_eq!(
            lower.dimensions(),
            (1440, 900),
            "对照板下半区必须来自 1440x900 夹具"
        );

        let canvas = build_comparison_canvas(&reference, &upper, &lower);
        let output_path = output_root.join(reference_name);
        canvas
            .save(&output_path)
            .unwrap_or_else(|error| panic!("无法保存对照板 {}: {error}", output_path.display()));
    }
}

fn build_comparison_canvas(
    reference: &image::RgbaImage,
    upper: &image::RgbaImage,
    lower: &image::RgbaImage,
) -> image::RgbaImage {
    let mut canvas = image::RgbaImage::from_pixel(2320, 900, image::Rgba([255, 255, 255, 255]));
    let reference = resize_to_fit(reference, 1600, 900);
    let reference_x = (1600 - reference.width()) / 2;
    let reference_y = (900 - reference.height()) / 2;
    image::imageops::overlay(
        &mut canvas,
        &reference,
        i64::from(reference_x),
        i64::from(reference_y),
    );

    let upper = image::imageops::resize(upper, 720, 450, image::imageops::FilterType::Lanczos3);
    let lower = image::imageops::resize(lower, 720, 450, image::imageops::FilterType::Lanczos3);
    image::imageops::overlay(&mut canvas, &upper, 1600, 0);
    image::imageops::overlay(&mut canvas, &lower, 1600, 450);
    canvas
}

fn resize_to_fit(
    source: &image::RgbaImage,
    maximum_width: u32,
    maximum_height: u32,
) -> image::RgbaImage {
    assert!(source.width() > 0 && source.height() > 0);
    let source_width = u64::from(source.width());
    let source_height = u64::from(source.height());
    let maximum_width_u64 = u64::from(maximum_width);
    let maximum_height_u64 = u64::from(maximum_height);
    let (width, height) = if source_width * maximum_height_u64 >= source_height * maximum_width_u64
    {
        let height =
            ((source_height * maximum_width_u64 + source_width / 2) / source_width).max(1) as u32;
        (maximum_width, height)
    } else {
        let width =
            ((source_width * maximum_height_u64 + source_height / 2) / source_height).max(1) as u32;
        (width, maximum_height)
    };
    image::imageops::resize(source, width, height, image::imageops::FilterType::Lanczos3)
}

fn save_snapshot(snapshot: &slint::SharedPixelBuffer<slint::Rgba8Pixel>, path: &Path) {
    let bytes = snapshot
        .as_slice()
        .iter()
        .flat_map(|pixel| [pixel.r, pixel.g, pixel.b, pixel.a])
        .collect::<Vec<_>>();
    let image = image::RgbaImage::from_raw(snapshot.width(), snapshot.height(), bytes)
        .expect("快照字节数必须匹配宽高");
    std::fs::create_dir_all(path.parent().expect("预览路径必须有父目录"))
        .expect("应能创建视觉预览目录");
    image.save(path).expect("应能保存视觉预览 PNG");
}

#[test]
fn render_all_views_with_real_row_states() {
    let fixture = VisualFixture::full();
    assert_eq!(fixture.nodes.len(), 3);
    assert_eq!(fixture.tasks.len(), 3);
    assert_eq!(fixture.groups.len(), 3);
    assert_eq!(fixture.members.len(), 6);
    assert!(fixture.nodes.iter().any(|row| row.status == "在线"));
    assert!(fixture.nodes.iter().any(|row| row.status == "离线"));
    assert!(fixture.nodes.iter().any(|row| row.status == "错误"));
    assert!(
        fixture
            .nodes
            .iter()
            .any(|row| row.error_text == "目标机器拒绝连接")
    );
    assert!(fixture.tasks.iter().any(|row| row.status == "运行中"));
    assert!(fixture.tasks.iter().any(|row| row.status == "已完成"));
    assert!(fixture.tasks.iter().any(|row| row.status == "失败"));
    assert!(fixture.members.iter().any(|row| row.review == "未决定"));
    assert!(fixture.members.iter().any(|row| row.review == "保留"));
    assert!(fixture.members.iter().any(|row| row.review == "删除"));
    let preview_output = std::env::var_os("RUST_V2_PREVIEW_OUTPUT");
    let cargo_target = std::env::var_os("CARGO_TARGET_DIR");
    let destination =
        resolve_preview_destination(preview_output.as_deref(), cargo_target.as_deref())
            .unwrap_or_else(|error| panic!("{error}"));
    render_all_views(&fixture, destination);
}

#[test]
fn preview_destination_requires_the_exact_repository_document_root() {
    let document_root = repository_root().join("docs/ui-preview/rust-v2");
    let accepted = resolve_preview_destination(Some(document_root.as_os_str()), None)
        .expect("仓库视觉证据根应允许显式文档输出");
    assert_eq!(
        preview_root(&accepted),
        document_root.join("after"),
        "文档模式必须自动写入 after 子目录"
    );
    assert_eq!(
        comparison_root(&accepted),
        Some(document_root.join("comparison")),
        "文档模式必须同时启用固定对照板目录"
    );

    let external_root = std::env::temp_dir();
    let error = resolve_preview_destination(Some(external_root.as_os_str()), None)
        .err()
        .expect("仓库外路径必须被拒绝");
    assert!(error.contains("RUST_V2_PREVIEW_OUTPUT_INVALID"));
    assert!(error.contains(&document_root.display().to_string()));
}

#[test]
fn ordinary_preview_uses_cargo_target_without_comparison_output() {
    let cargo_target = Path::new(r"C:\tmp\rust-v2-preview-test-target");
    let destination = resolve_preview_destination(None, Some(cargo_target.as_os_str()))
        .expect("普通预览应接受独立 Cargo target");
    assert_eq!(
        preview_root(&destination),
        cargo_target.join("visual-preview/current")
    );
    assert_eq!(comparison_root(&destination), None);
}

#[test]
fn comparison_canvas_fits_reference_and_stacks_two_after_views() {
    let reference = image::RgbaImage::from_pixel(800, 800, image::Rgba([220, 20, 60, 255]));
    let upper = image::RgbaImage::from_pixel(1440, 900, image::Rgba([20, 180, 80, 255]));
    let lower = image::RgbaImage::from_pixel(1440, 900, image::Rgba([30, 90, 220, 255]));

    let canvas = build_comparison_canvas(&reference, &upper, &lower);

    assert_eq!(canvas.dimensions(), (2320, 900));
    assert_eq!(*canvas.get_pixel(0, 0), image::Rgba([255, 255, 255, 255]));
    assert_eq!(
        *canvas.get_pixel(350, 0),
        image::Rgba([220, 20, 60, 255]),
        "正方形参考图应在 1600x900 左画布中等比居中"
    );
    assert_eq!(*canvas.get_pixel(1600, 0), image::Rgba([20, 180, 80, 255]));
    assert_eq!(
        *canvas.get_pixel(1600, 450),
        image::Rgba([30, 90, 220, 255])
    );
}

#[test]
fn comparison_mapping_is_fixed_to_the_six_design_references() {
    assert_eq!(
        COMPARISONS,
        [
            ("01-overview-nodes.png", "01-overview", "02-nodes"),
            ("02-scan-tasks.png", "03-scan", "04-tasks"),
            ("03-exact-cross-machine.png", "05-exact", "08-cross-machine",),
            (
                "04-similar-media.png",
                "06-similar-images",
                "07-similar-videos",
            ),
            ("05-review-delete.png", "09-review", "10-delete-center"),
            (
                "06-settings-diagnostics.png",
                "11-settings",
                "12-diagnostics",
            ),
        ]
    );
}

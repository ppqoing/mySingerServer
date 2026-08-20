use dedup_desktop_ui::{MainWindow, UiGroupRow, UiMemberRow, UiNodeRow, UiTaskRow};
use i_slint_backend_testing::{ElementHandle, TestingBackend, TestingBackendOptions};
use slint::{Color, ComponentHandle, ModelRc, VecModel};
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
    ("12-diagnostics", 7, 0, 0, 0, 6),
];

enum PreviewDestination {
    TargetDirectory,
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
                    node_index: 0,
                    title: "媒体扫描".into(),
                    stage: "枚举文件".into(),
                    status: "运行中".into(),
                    status_color: Color::from_rgb_u8(59, 130, 246),
                    progress: 35,
                    counts: "7 / 20 · 失败 0 · 跳过 1".into(),
                },
                UiTaskRow {
                    id: "task-image-analysis".into(),
                    node_index: 1,
                    title: "图片分析".into(),
                    stage: "完成".into(),
                    status: "已完成".into(),
                    status_color: Color::from_rgb_u8(22, 163, 74),
                    progress: 100,
                    counts: "18 / 18 · 失败 0 · 跳过 0".into(),
                },
                UiTaskRow {
                    id: "task-video-analysis".into(),
                    node_index: 2,
                    title: "视频分析".into(),
                    stage: "提取特征".into(),
                    status: "失败".into(),
                    status_color: Color::from_rgb_u8(239, 68, 68),
                    progress: 60,
                    counts: "6 / 10 · 失败 1 · 跳过 0".into(),
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
    for (size_name, width, height) in [("1440x900", 1440, 900), ("1080x700", 1080, 700)] {
        for (view, current_page, overview_mode, task_mode, review_tab, settings_section) in VIEWS {
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
}

fn preview_root(destination: &PreviewDestination) -> PathBuf {
    match destination {
        PreviewDestination::TargetDirectory => std::env::var_os("RUST_V2_PREVIEW_OUTPUT")
            .map(PathBuf::from)
            .map(|root| root.join("after"))
            .or_else(|| {
                std::env::var_os("CARGO_TARGET_DIR")
                    .map(PathBuf::from)
                    .map(|root| root.join("visual-preview/current"))
            })
            .unwrap_or_else(|| {
                Path::new(env!("CARGO_MANIFEST_DIR"))
                    .parent()
                    .and_then(Path::parent)
                    .expect("桌面 UI crate 应位于工作区 crates 目录")
                    .join("target/visual-preview/current")
            }),
    }
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
fn visual_fixture_covers_every_real_row_state() {
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
    render_all_views(&fixture, PreviewDestination::TargetDirectory);
}

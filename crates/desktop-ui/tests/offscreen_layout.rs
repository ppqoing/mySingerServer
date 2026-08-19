use dedup_desktop_ui::MainWindow;
use i_slint_backend_testing::{ElementHandle, TestingBackend, TestingBackendOptions};
use slint::ComponentHandle;

fn install_testing_backend() {
    // Rust 测试用例各在线程中运行；Slint 平台也是线程局部状态，因此每个用例独立安装。
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        threading: false,
        renderer_name: Some("software".into()),
    })))
    .expect("应能安装软件渲染测试后端");
}

fn assert_light_opaque(pixel: slint::Rgba8Pixel, region: &str) {
    assert_eq!(pixel.a, u8::MAX, "{region} 应完全不透明");
    assert!(
        pixel.r >= 235 && pixel.g >= 235 && pixel.b >= 235,
        "{region} 应符合浅色主题，实际 RGBA=({}, {}, {}, {})",
        pixel.r,
        pixel.g,
        pixel.b,
        pixel.a,
    );
}

#[test]
fn light_shell_renders_at_target_size() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window.show().expect("应能显示真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));
    let snapshot = window
        .window()
        .take_snapshot()
        .expect("软件渲染后应能取得 RGBA8 快照");

    assert_eq!((snapshot.width(), snapshot.height()), (1440, 900));
    assert_eq!(snapshot.as_slice().len(), 1440 * 900);
    let opaque = snapshot
        .as_slice()
        .iter()
        .filter(|pixel| pixel.a == u8::MAX)
        .count();
    assert!(opaque * 100 >= snapshot.as_slice().len() * 99);

    let sidebar = snapshot.as_slice()[400 * 1440 + 20];
    let top_bar = snapshot.as_slice()[5 * 1440 + 600];
    let content = snapshot.as_slice()[400 * 1440 + 160];
    let status_bar = snapshot.as_slice()[884 * 1440 + 800];
    for (pixel, region) in [
        (sidebar, "侧栏"),
        (top_bar, "顶栏"),
        (content, "内容区"),
        (status_bar, "底栏"),
    ] {
        assert_light_opaque(pixel, region);
    }
    assert!(
        sidebar.r > content.r && status_bar.r > content.r,
        "白色侧栏和底栏应围绕稍深的内容区：侧栏={sidebar:?}，内容={content:?}，底栏={status_bar:?}",
    );

    window
        .window()
        .set_size(slint::PhysicalSize::new(1080, 700));
    let compact_snapshot = window
        .window()
        .take_snapshot()
        .expect("最小窗口尺寸仍应完成软件渲染");
    assert_eq!(
        (compact_snapshot.width(), compact_snapshot.height()),
        (1080, 700),
    );

    for label in [
        "总览",
        "节点",
        "扫描",
        "任务",
        "重复文件",
        "审核删除",
        "设置",
        "刷新",
    ] {
        let element = ElementHandle::find_by_accessible_label(&window, label)
            .next()
            .unwrap_or_else(|| panic!("最小窗口应保留可访问动作：{label}"));
        let position = element.absolute_position();
        let size = element.size();
        assert!(
            position.x >= 0.0
                && position.y >= 0.0
                && position.x + size.width <= 1080.0
                && position.y + size.height <= 700.0,
            "{label} 应位于 1080×700 窗口边界内，位置={position:?}，尺寸={size:?}",
        );
    }
}

#[test]
fn duplicate_workspace_columns_stay_ordered_inside_content_area() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window.show().expect("应能显示真实 MainWindow");
    window.invoke_navigate_to(4);
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));

    for (label, page) in [
        ("精确重复", 2),
        ("相似图片", 3),
        ("相似视频", 4),
        ("跨机器", 5),
    ] {
        ElementHandle::find_by_accessible_label(&window, label)
            .next()
            .unwrap_or_else(|| panic!("应能找到重复类型标签：{label}"))
            .invoke_accessible_default_action();
        assert_eq!(window.get_current_page(), page);
        window
            .window()
            .take_snapshot()
            .expect("切页后应完成软件渲染");

        let group = ElementHandle::find_by_accessible_label(&window, "重复组表")
            .next()
            .expect("统一工作区应公开组表区域");
        let member = ElementHandle::find_by_accessible_label(&window, "成员表")
            .next()
            .expect("统一工作区应公开成员表区域");
        let detail = ElementHandle::find_by_accessible_label(&window, "详情面板")
            .next()
            .expect("统一工作区应公开详情区域");
        let (group_position, group_size) = (group.absolute_position(), group.size());
        let (member_position, member_size) = (member.absolute_position(), member.size());
        let (detail_position, detail_size) = (detail.absolute_position(), detail.size());

        assert!(
            group_position.x + group_size.width <= member_position.x
                && member_position.x + member_size.width <= detail_position.x,
            "{label} 的组表、成员表、详情面板应从左到右排列：组={group_position:?}/{group_size:?}，成员={member_position:?}/{member_size:?}，详情={detail_position:?}/{detail_size:?}",
        );
        for (name, position, size) in [
            ("重复组表", group_position, group_size),
            ("成员表", member_position, member_size),
            ("详情面板", detail_position, detail_size),
        ] {
            assert!(
                position.x >= 144.0
                    && position.y >= 58.0
                    && position.x + size.width <= 1440.0
                    && position.y + size.height <= 868.0,
                "{label} 的{name}必须位于内容区内，位置={position:?}，尺寸={size:?}",
            );
        }
    }
}

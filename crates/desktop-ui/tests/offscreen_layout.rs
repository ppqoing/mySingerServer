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

fn assert_inside_window(window: &MainWindow, width: f32, height: f32) {
    for label in [
        "总览",
        "节点",
        "扫描",
        "任务",
        "重复文件",
        "审核删除",
        "设置",
        "刷新",
        "在线节点：0 台",
    ] {
        let element = ElementHandle::find_by_accessible_label(window, label)
            .next()
            .unwrap_or_else(|| panic!("窗口应保留可访问元素：{label}"));
        let position = element.absolute_position();
        let size = element.size();
        assert!(
            position.x >= 0.0
                && position.y >= 0.0
                && position.x + size.width <= width
                && position.y + size.height <= height,
            "{label} 应位于 {width}×{height} 窗口边界内，位置={position:?}，尺寸={size:?}",
        );
    }
}

fn assert_element_inside_window(
    element: &ElementHandle,
    label: &str,
    width: f32,
    height: f32,
) {
    let position = element.absolute_position();
    let size = element.size();
    assert!(
        position.x >= 0.0
            && position.y >= 0.0
            && position.x + size.width <= width
            && position.y + size.height <= height,
        "{label} 应位于 {width}×{height} 窗口边界内，位置={position:?}，尺寸={size:?}",
    );
}

#[test]
fn shell_landmarks_fit_both_window_sizes() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window.set_sync_text("游标 120 / 125".into());
    window.set_postgres_status("已连接".into());
    let full_error = "节点 10.0.0.8 的同步连接在提交后断开，完整诊断必须保留给辅助技术";
    window.set_last_error(full_error.into());
    window.show().expect("应能显示真实 MainWindow");

    for (width, height) in [(1440.0, 900.0), (1080.0, 700.0)] {
        window
            .window()
            .set_size(slint::PhysicalSize::new(width as u32, height as u32));
        window
            .window()
            .take_snapshot()
            .expect("固定应用壳应能完成软件渲染");

        let menu = ElementHandle::find_by_accessible_label(&window, "应用菜单")
            .next()
            .expect("侧栏顶部应公开应用菜单动作");
        let overview = ElementHandle::find_by_accessible_label(&window, "总览")
            .next()
            .expect("侧栏应保留总览动作");
        let search = ElementHandle::find_by_accessible_label(&window, "本地搜索")
            .next()
            .expect("顶栏应公开本地搜索框");
        let refresh = ElementHandle::find_by_accessible_label(&window, "刷新")
            .next()
            .expect("顶栏应公开刷新动作");

        let (menu_position, menu_size) = (menu.absolute_position(), menu.size());
        assert!(
            menu_position.x < 144.0
                && menu_position.y < 58.0
                && menu_position.x + menu_size.width <= 144.0
                && menu_position.y + menu_size.height <= 58.0,
            "应用菜单必须完整位于 144×58 侧栏头部，位置={menu_position:?}，尺寸={menu_size:?}",
        );
        let overview_position = overview.absolute_position();
        assert!(
            overview_position.x < 144.0 && overview_position.y >= 58.0,
            "总览必须位于侧栏头部下方，位置={overview_position:?}",
        );

        let (search_position, search_size) = (search.absolute_position(), search.size());
        let (refresh_position, refresh_size) = (refresh.absolute_position(), refresh.size());
        assert!(
            search_position.x >= 144.0
                && search_position.y < 58.0
                && search_position.y + search_size.height <= 58.0,
            "本地搜索必须位于顶栏，位置={search_position:?}，尺寸={search_size:?}",
        );
        assert!(
            refresh_position.x >= search_position.x + search_size.width
                && refresh_position.y < 58.0
                && refresh_position.y + refresh_size.height <= 58.0,
            "刷新必须位于搜索框右侧且互不覆盖，搜索={search_position:?}/{search_size:?}，刷新={refresh_position:?}/{refresh_size:?}",
        );

        let status_label = format!(
            "状态栏：引擎就绪；同步 游标 120 / 125；PostgreSQL 已连接；最后错误 {full_error}"
        );
        let status = ElementHandle::find_by_accessible_label(&window, &status_label)
            .next()
            .expect("状态栏根可访问名称应包含完整最后错误");
        assert_element_inside_window(&status, "状态栏", width, height);
        for label in [
            "引擎状态：就绪",
            "同步状态：游标 120 / 125",
            "PostgreSQL 状态：已连接",
        ] {
            let segment = ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .unwrap_or_else(|| panic!("状态栏应公开三段只读状态：{label}"));
            let position = segment.absolute_position();
            assert!(
                position.y >= height - 32.0,
                "{label} 必须位于 32px 底栏内，位置={position:?}",
            );
            assert_element_inside_window(&segment, label, width, height);
        }
    }
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
    assert_inside_window(&window, 1440.0, 900.0);

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

    assert_inside_window(&window, 1080.0, 700.0);
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

#[test]
fn settings_workspace_stays_reachable_at_minimum_size() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window.invoke_navigate_to(6);
    window
        .window()
        .set_size(slint::PhysicalSize::new(1080, 700));
    window.show().expect("应能显示真实设置工作区");
    window
        .window()
        .take_snapshot()
        .expect("最小窗口设置工作区应完成软件渲染");

    let mut previous_bottom = 0.0;
    let mut menu_right: f32 = 0.0;
    for label in [
        "常规",
        "相似度算法",
        "存储",
        "节点服务",
        "扫描与性能",
        "外部工具",
        "日志与诊断",
    ] {
        let menu = ElementHandle::find_by_accessible_label(&window, label)
            .next()
            .unwrap_or_else(|| panic!("应能找到设置二级菜单：{label}"));
        let position = menu.absolute_position();
        let size = menu.size();
        assert!(
            position.y >= previous_bottom,
            "设置二级菜单必须从上到下排列：{label} 位置={position:?}，上一项底部={previous_bottom}"
        );
        assert!(
            position.x >= 144.0
                && position.y >= 58.0
                && position.x + size.width <= 1080.0
                && position.y + size.height <= 668.0,
            "{label} 必须位于最小窗口内容区内，位置={position:?}，尺寸={size:?}"
        );
        previous_bottom = position.y + size.height;
        menu_right = menu_right.max(position.x + size.width);
    }

    let content = ElementHandle::find_by_accessible_label(&window, "设置内容卡")
        .next()
        .expect("设置工作区应公开右侧内容卡");
    let content_position = content.absolute_position();
    assert!(
        content_position.x >= menu_right,
        "设置内容卡必须在二级菜单右侧：菜单右边={menu_right}，内容位置={content_position:?}"
    );

    let save = ElementHandle::find_by_accessible_label(&window, "保存设置")
        .next()
        .expect("最小窗口仍应提供保存设置动作");
    let save_position = save.absolute_position();
    let save_size = save.size();
    assert!(
        save_position.x >= 0.0
            && save_position.y >= 0.0
            && save_position.x + save_size.width <= 1080.0
            && save_position.y + save_size.height <= 700.0,
        "保存设置必须位于 1080×700 窗口边界内，位置={save_position:?}，尺寸={save_size:?}"
    );
}

#[test]
fn delete_confirmation_is_a_centered_root_level_light_overlay() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));
    window.set_delete_mode("回收站".into());
    window.set_delete_dialog_open(true);
    window.show().expect("应能显示删除确认覆盖层");
    window
        .window()
        .take_snapshot()
        .expect("删除确认覆盖层应完成软件渲染");

    let overlay = ElementHandle::find_by_accessible_label(&window, "删除确认覆盖层")
        .next()
        .expect("根窗口应公开删除确认覆盖层");
    let card = ElementHandle::find_by_accessible_label(&window, "删除确认：回收站")
        .next()
        .expect("删除确认覆盖层应公开白色确认卡片");
    let (overlay_position, overlay_size) = (overlay.absolute_position(), overlay.size());
    let (card_position, card_size) = (card.absolute_position(), card.size());

    assert_eq!(overlay_position, slint::LogicalPosition::new(0.0, 0.0));
    assert_eq!(overlay_size, slint::LogicalSize::new(1440.0, 900.0));
    assert_eq!(card_size, slint::LogicalSize::new(520.0, 320.0));
    assert!(
        (card_position.x - 460.0).abs() < 0.5 && (card_position.y - 290.0).abs() < 0.5,
        "确认卡片应在根窗口居中，实际位置={card_position:?}",
    );
    assert!(
        overlay_position.x <= 144.0
            && overlay_position.y <= 58.0
            && overlay_position.x + overlay_size.width >= 1440.0
            && overlay_position.y + overlay_size.height >= 868.0,
        "根级覆盖层应遮住 AppShell 内容区，位置={overlay_position:?}，尺寸={overlay_size:?}",
    );
}

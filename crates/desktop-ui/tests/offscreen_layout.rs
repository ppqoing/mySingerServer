use dedup_desktop_ui::{MainWindow, UiGroupRow, UiMemberRow, UiNodeRow, UiTaskRow};
use i_slint_backend_testing::{ElementHandle, TestingBackend, TestingBackendOptions};
use slint::{Color, ComponentHandle, ModelRc, VecModel};

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

fn assert_element_inside_window(element: &ElementHandle, label: &str, width: f32, height: f32) {
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

// Task 5 的相对位置断言必须先证明元素真实参与布局，避免零尺寸元素误过边界比较。
fn assert_element_has_positive_size(element: &ElementHandle, label: &str) {
    let size = element.size();
    assert!(
        size.width > 0.0 && size.height > 0.0,
        "{label} 必须拥有正尺寸，实际={size:?}",
    );
}

fn accessible(window: &MainWindow, label: &str) -> ElementHandle {
    ElementHandle::find_by_accessible_label(window, label)
        .next()
        .unwrap_or_else(|| panic!("应能找到可访问元素：{label}"))
}

// 使用真实只读行模型令组表和成员表创建各自的 ScrollView，不预取任何预览内容。
fn install_duplicate_workspace_fixture(window: &MainWindow) {
    window.set_groups(ModelRc::new(VecModel::from(vec![UiGroupRow {
        id: "group-001".into(),
        kind: "相似图片".into(),
        md5: "0123456789abcdef0123456789abcdef".into(),
        size: "8.0 MiB".into(),
        members: 2,
        reclaimable: "8.0 MiB".into(),
    }])));
    window.set_members(ModelRc::new(VecModel::from(vec![UiMemberRow {
        machine_id: "machine-a".into(),
        path: "D:\\Media\\photo-a.jpg".into(),
        md5: "0123456789abcdef0123456789abcdef".into(),
        size: "8.0 MiB".into(),
        representative: true,
        stage1: "0.99".into(),
        phash: "4".into(),
        stage2: "0.96".into(),
        metadata: "1920×1080 · JPEG".into(),
        review: "未决定".into(),
        review_color: Color::from_rgb_u8(107, 114, 128),
        online: true,
        preview_enabled: true,
        delete_enabled: false,
    }])));
}

// 使用完整字面模型同时撑开总览表格与节点详情，几何预期不从生产布局反推。
fn install_overview_and_nodes_fixture(window: &MainWindow) {
    window.set_nodes(ModelRc::new(VecModel::from(vec![
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
    ])));
    window.set_tasks(ModelRc::new(VecModel::from(vec![
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
    ])));
}

// 使用单个运行中任务验证最小窗口中的取消动作，不依赖总览夹具的任务 ID。
fn install_scan_and_task_fixture(window: &MainWindow) {
    window.set_scan_root("D:\\Media".into());
    window.set_scan_node_index(7);
    window.set_enumerator_index(1);
    window.set_filtering_enabled(true);
    window.set_analysis_task_ids("task-running".into());
    window.set_tasks(ModelRc::new(VecModel::from(vec![UiTaskRow {
        id: "task-running".into(),
        node_index: 7,
        title: "媒体扫描".into(),
        stage: "枚举文件".into(),
        status: "运行中".into(),
        status_color: Color::from_rgb_u8(59, 130, 246),
        progress: 35,
        counts: "7 / 20 · 失败 0 · 跳过 1".into(),
    }])));
}

#[test]
fn scan_and_task_primary_actions_stay_above_the_fold() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_scan_and_task_fixture(&window);
    window.show().expect("应能显示扫描和任务工作区");

    for (width, height) in [(1440.0, 900.0), (1080.0, 700.0)] {
        window
            .window()
            .set_size(slint::PhysicalSize::new(width as u32, height as u32));
        let content_bottom = height - 32.0;

        window.invoke_navigate_to(2);
        window
            .window()
            .take_snapshot()
            .expect("扫描工作区应能完成软件渲染");
        let scan_title = ElementHandle::find_by_accessible_label(&window, "新建扫描标题")
            .next()
            .expect("扫描工作区应公开标题地标");
        let scan_main = ElementHandle::find_by_accessible_label(&window, "扫描主要内容")
            .next()
            .expect("扫描工作区应公开主要内容地标");
        assert_element_has_positive_size(&scan_title, "新建扫描标题");
        assert_element_has_positive_size(&scan_main, "扫描主要内容");
        assert!(
            scan_title.size().height <= 40.0,
            "扫描标题不得吸收首屏富余高度，实际尺寸={:?}",
            scan_title.size(),
        );
        let scan_gap = scan_main.absolute_position().y
            - (scan_title.absolute_position().y + scan_title.size().height);
        assert!(
            (0.0..=32.0).contains(&scan_gap),
            "扫描主要内容必须紧随标题，实际间距={scan_gap}px",
        );
        for label in ["浏览节点路径", "开始扫描"] {
            let action = ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .unwrap_or_else(|| panic!("扫描工作区应公开{label}"));
            let position = action.absolute_position();
            let size = action.size();
            assert!(
                position.x >= 144.0
                    && position.y >= 58.0
                    && position.x + size.width <= width
                    && position.y + size.height <= content_bottom,
                "{label} 必须位于 {width}×{height} 内容区首屏内，位置={position:?}，尺寸={size:?}",
            );
        }

        window.invoke_navigate_to(3);
        window
            .window()
            .take_snapshot()
            .expect("任务工作区应能完成软件渲染");
        let task_title = ElementHandle::find_by_accessible_label(&window, "任务中心标题")
            .next()
            .expect("任务工作区应公开标题地标");
        let tabs = ElementHandle::find_by_accessible_label(&window, "任务标签栏")
            .next()
            .expect("任务工作区应公开标签栏地标");
        let table = ElementHandle::find_by_accessible_label(&window, "任务主表")
            .next()
            .expect("任务工作区应公开主表地标");
        for (element, label) in [
            (&task_title, "任务中心标题"),
            (&tabs, "任务标签栏"),
            (&table, "任务主表"),
        ] {
            assert_element_has_positive_size(element, label);
            let position = element.absolute_position();
            let size = element.size();
            assert!(
                position.x >= 144.0
                    && position.y >= 58.0
                    && position.x + size.width <= width
                    && position.y + size.height <= content_bottom,
                "{label} 必须位于任务内容区内，位置={position:?}，尺寸={size:?}",
            );
        }
        assert!(
            task_title.size().height <= 40.0,
            "任务标题不得吸收主表剩余高度，实际尺寸={:?}",
            task_title.size(),
        );
        let tab_gap = tabs.absolute_position().y
            - (task_title.absolute_position().y + task_title.size().height);
        let table_gap =
            table.absolute_position().y - (tabs.absolute_position().y + tabs.size().height);
        assert!(
            (0.0..=32.0).contains(&tab_gap) && (0.0..=32.0).contains(&table_gap),
            "任务标题、标签栏和主表必须连续置顶，间距分别为 {tab_gap}px / {table_gap}px",
        );

        let task_scroll = table
            .query_descendants()
            .match_inherits("ScrollView")
            .find_first()
            .expect("任务主表必须拥有自己的 ScrollView");
        let scroll_position = task_scroll.absolute_position();
        let scroll_size = task_scroll.size();
        let mut cancel =
            ElementHandle::find_by_accessible_label(&window, "取消任务：task-running").next();
        if cancel.is_none() {
            // 最小窗口允许取消列位于主表自己的横向滚动内容中，但必须能通过真实滚轮到达。
            task_scroll.scroll(-1000.0, 0.0);
            window
                .window()
                .take_snapshot()
                .expect("横向滚动后的任务表应能完成软件渲染");
            cancel =
                ElementHandle::find_by_accessible_label(&window, "取消任务：task-running").next();
        }
        let cancel = cancel.expect("运行中任务取消动作必须直接可见或经任务表横向滚动到达");
        let cancel_position = cancel.absolute_position();
        let cancel_size = cancel.size();
        let cancel_inside_window = cancel_position.x >= 0.0
            && cancel_position.y >= 0.0
            && cancel_position.x + cancel_size.width <= width
            && cancel_position.y + cancel_size.height <= height;
        let cancel_reachable_in_table = scroll_size.width > 0.0
            && scroll_size.height > 0.0
            && cancel_position.y >= scroll_position.y
            && cancel_position.y + cancel_size.height <= scroll_position.y + scroll_size.height;
        assert!(
            cancel_inside_window || cancel_reachable_in_table,
            "取消动作必须直接可见或位于任务表自己的滚动区域，动作={cancel_position:?}/{cancel_size:?}，滚动区={scroll_position:?}/{scroll_size:?}",
        );
    }
}

#[test]
fn overview_and_nodes_start_at_the_top_without_blank_stretch() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_overview_and_nodes_fixture(&window);
    window.show().expect("应能显示总览与节点工作区");

    for (width, height) in [(1440.0, 900.0), (1080.0, 700.0)] {
        window
            .window()
            .set_size(slint::PhysicalSize::new(width as u32, height as u32));
        window.invoke_navigate_to(0);
        window
            .window()
            .take_snapshot()
            .expect("总览应能完成软件渲染");

        let title = ElementHandle::find_by_accessible_label(&window, "总览标题")
            .next()
            .expect("总览应公开标题地标");
        let main = ElementHandle::find_by_accessible_label(&window, "总览主要内容")
            .next()
            .expect("总览应公开第一组主要内容");
        assert_element_has_positive_size(&title, "总览标题");
        assert_element_has_positive_size(&main, "总览主要内容");
        let title_bottom = title.absolute_position().y + title.size().height;
        let gap = main.absolute_position().y - title_bottom;
        assert!(
            (0.0..=32.0).contains(&gap),
            "总览标题到第一组主要内容的间距必须位于 0–32px，实际={gap}px",
        );

        window.invoke_navigate_to(1);
        window
            .window()
            .take_snapshot()
            .expect("节点工作区应能完成软件渲染");

        let table = ElementHandle::find_by_accessible_label(&window, "节点表")
            .next()
            .expect("节点工作区应公开节点表");
        let detail = ElementHandle::find_by_accessible_label(&window, "节点详情")
            .next()
            .expect("节点工作区应公开节点详情");
        let add_bar = ElementHandle::find_by_accessible_label(&window, "添加节点栏")
            .next()
            .expect("节点工作区应公开添加节点栏");
        assert_element_has_positive_size(&table, "节点表");
        assert_element_has_positive_size(&detail, "节点详情");
        assert_element_has_positive_size(&add_bar, "添加节点栏");
        let (table_position, table_size) = (table.absolute_position(), table.size());
        let (detail_position, detail_size) = (detail.absolute_position(), detail.size());
        let (add_position, add_size) = (add_bar.absolute_position(), add_bar.size());

        assert!(
            table_position.x + table_size.width <= detail_position.x
                && add_position.x + add_size.width <= detail_position.x,
            "节点表和添加栏必须位于详情左侧且互不覆盖：表={table_position:?}/{table_size:?}，添加={add_position:?}/{add_size:?}，详情={detail_position:?}/{detail_size:?}",
        );
        assert!(
            table_position.y + table_size.height <= add_position.y,
            "添加节点栏必须位于节点表下方：表={table_position:?}/{table_size:?}，添加={add_position:?}/{add_size:?}",
        );
        assert!(
            (280.0..=320.0).contains(&detail_size.width),
            "节点详情宽度必须保持 280–320px，实际={detail_size:?}",
        );
        for (label, position, size) in [
            ("节点表", table_position, table_size),
            ("节点详情", detail_position, detail_size),
            ("添加节点栏", add_position, add_size),
        ] {
            assert!(
                position.x >= 144.0
                    && position.y >= 58.0
                    && position.x + size.width <= width
                    && position.y + size.height <= height - 32.0,
                "{label} 必须位于内容区内，位置={position:?}，尺寸={size:?}，窗口={width}×{height}",
            );
        }
        for label in [
            "连接全部节点",
            "添加节点",
            "编辑节点",
            "立即同步",
            "移除节点",
        ] {
            let action = ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .unwrap_or_else(|| panic!("{width}×{height} 应公开节点动作：{label}"));
            assert_element_has_positive_size(&action, label);
            assert_element_inside_window(&action, label, width, height);
        }
    }
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
fn duplicate_workspace_regions_keep_their_own_scroll_views_at_minimum_size() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_duplicate_workspace_fixture(&window);
    window.invoke_navigate_to(4);
    window
        .window()
        .set_size(slint::PhysicalSize::new(1080, 700));
    window.show().expect("应能显示真实重复工作区");
    window
        .window()
        .take_snapshot()
        .expect("1080×700 重复工作区应能完成软件渲染");

    for label in ["重复组表", "成员表", "详情面板"] {
        let region = ElementHandle::find_by_accessible_label(&window, label)
            .next()
            .unwrap_or_else(|| panic!("最小窗口应公开{label}"));
        let region_position = region.absolute_position();
        let region_size = region.size();
        assert!(
            region_size.width > 0.0 && region_size.height > 0.0,
            "{label} 在 1080×700 下必须拥有正尺寸，实际={region_size:?}",
        );

        let scroll = region
            .query_descendants()
            .match_inherits("ScrollView")
            .find_first()
            .unwrap_or_else(|| panic!("{label} 必须通过自己的 ScrollView 到达内容"));
        let scroll_position = scroll.absolute_position();
        let scroll_size = scroll.size();
        assert!(
            scroll_size.width > 0.0
                && scroll_size.height > 0.0
                && scroll_position.x >= region_position.x
                && scroll_position.y >= region_position.y
                && scroll_position.x + scroll_size.width <= region_position.x + region_size.width
                && scroll_position.y + scroll_size.height <= region_position.y + region_size.height,
            "{label} 的 ScrollView 必须可在自身区域内到达，区域={region_position:?}/{region_size:?}，滚动区={scroll_position:?}/{scroll_size:?}",
        );
    }
}

#[test]
fn result_review_and_delete_workspaces_keep_named_regions() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_duplicate_workspace_fixture(&window);
    window.set_selected_group_id("group-001".into());
    window.set_delete_file_count(1);
    window.set_delete_node_count(1);
    window.set_delete_reclaimable("8.0 MiB".into());
    window.set_delete_mode("回收站".into());
    window.set_delete_can_execute(false);
    window.set_delete_warning("目标节点离线，当前不能确认执行。".into());
    window.show().expect("应能显示结果、审核与删除工作区");

    for (width, height) in [(1440.0, 900.0), (1080.0, 700.0)] {
        window
            .window()
            .set_size(slint::PhysicalSize::new(width as u32, height as u32));

        for (label, page) in [
            ("精确重复", 2),
            ("相似图片", 3),
            ("相似视频", 4),
            ("跨机器", 5),
        ] {
            window.set_current_page(page);
            window
                .window()
                .take_snapshot()
                .unwrap_or_else(|_| panic!("{label} 在 {width}×{height} 下应能完成软件渲染"));

            let group = ElementHandle::find_by_accessible_label(&window, "重复组表")
                .next()
                .expect("结果工作区应公开重复组表");
            let member = ElementHandle::find_by_accessible_label(&window, "成员表")
                .next()
                .expect("结果工作区应公开成员表");
            let detail = ElementHandle::find_by_accessible_label(&window, "详情面板")
                .next()
                .expect("结果工作区应公开详情面板");
            let (group_position, group_size) = (group.absolute_position(), group.size());
            let (member_position, member_size) = (member.absolute_position(), member.size());
            let (detail_position, detail_size) = (detail.absolute_position(), detail.size());
            let filter = ElementHandle::find_by_accessible_label(&window, "结果过滤栏")
                .next()
                .unwrap_or_else(|| {
                    panic!(
                        "{label} 应公开结果过滤栏；当前三栏几何：组={group_position:?}/{group_size:?}，成员={member_position:?}/{member_size:?}，详情={detail_position:?}/{detail_size:?}"
                    )
                });
            assert_element_has_positive_size(&filter, "结果过滤栏");

            for (name, region) in [
                ("重复组表", &group),
                ("成员表", &member),
                ("详情面板", &detail),
            ] {
                assert_element_has_positive_size(region, name);
            }
            assert!(
                group_position.x + group_size.width <= member_position.x
                    && member_position.x + member_size.width <= detail_position.x,
                "{label} 在 {width}×{height} 下必须保持组、成员、详情从左到右且不覆盖：组={group_position:?}/{group_size:?}，成员={member_position:?}/{member_size:?}，详情={detail_position:?}/{detail_size:?}",
            );
            if width == 1440.0 {
                assert!(
                    (360.0..=380.0).contains(&group_size.width),
                    "{label} 的组表宽度应为 360–380px，实际={group_size:?}",
                );
                assert!(
                    (280.0..=320.0).contains(&detail_size.width),
                    "{label} 的详情宽度应为 280–320px，实际={detail_size:?}",
                );
                assert!(
                    member_size.width > group_size.width,
                    "{label} 的成员表应占中间剩余宽度，组={group_size:?}，成员={member_size:?}",
                );
            } else {
                for (name, region) in [("重复组表", &group), ("成员表", &member)] {
                    assert!(
                        region
                            .query_descendants()
                            .match_inherits("ScrollView")
                            .find_first()
                            .is_some(),
                        "{name} 在 1080×700 下必须通过自己的 ScrollView 横向到达内容",
                    );
                }
            }
        }

        window.set_current_page(6);
        window.set_review_tab(0);
        window
            .window()
            .take_snapshot()
            .expect("审核工作台应能完成软件渲染");
        let review_regions = ["审核过滤栏", "审核组队列", "审核成员列表", "复核详情"].map(|name| {
            ElementHandle::find_by_accessible_label(&window, name)
                .next()
                .unwrap_or_else(|| panic!("审核工作台应公开{name}"))
        });
        for (name, region) in ["审核过滤栏", "审核组队列", "审核成员列表", "复核详情"]
            .into_iter()
            .zip(review_regions.iter())
        {
            assert_element_has_positive_size(region, name);
        }
        for pair in review_regions[1..].windows(2) {
            assert!(
                pair[0].absolute_position().x + pair[0].size().width
                    <= pair[1].absolute_position().x,
                "审核组队列、审核成员列表和复核详情必须从左到右且不覆盖",
            );
        }

        window.set_review_tab(1);
        window.set_delete_filter(0);
        window
            .window()
            .take_snapshot()
            .expect("删除中心应能完成软件渲染");
        let summary = ElementHandle::find_by_accessible_label(&window, "删除批次摘要")
            .next()
            .expect("删除中心应公开删除批次摘要");
        let execution = ElementHandle::find_by_accessible_label(&window, "删除执行详情")
            .next()
            .expect("删除中心应公开删除执行详情");
        assert_element_has_positive_size(&summary, "删除批次摘要");
        assert_element_has_positive_size(&execution, "删除执行详情");
        assert!(
            summary.absolute_position().x + summary.size().width <= execution.absolute_position().x,
            "删除批次摘要与删除执行详情必须左右分栏且不覆盖",
        );

        window.set_delete_filter(2);
        window
            .window()
            .take_snapshot()
            .expect("删除历史空态应能完成软件渲染");
        assert!(
            ElementHandle::find_by_accessible_label(
                &window,
                "删除历史空态：当前版本没有持久删除批次；当前后端没有删除批次历史模型，因此不生成模拟记录。",
            )
            .next()
            .is_some(),
            "没有历史模型时必须同时显示标题和原因，不得制造虚假批次",
        );
    }
}

#[test]
fn settings_workspace_stays_reachable_at_minimum_size() {
    install_testing_backend();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window.invoke_navigate_to(6);
    window.show().expect("应能显示真实设置工作区");

    for (width, height) in [(1440.0, 900.0), (1080.0, 700.0)] {
        window
            .window()
            .set_size(slint::PhysicalSize::new(width as u32, height as u32));
        window
            .window()
            .take_snapshot()
            .expect("设置工作区应完成软件渲染");

        let title = ElementHandle::find_by_accessible_label(&window, "设置标题")
            .next()
            .expect("设置工作区应公开标题地标");
        let main = ElementHandle::find_by_accessible_label(&window, "设置主要内容")
            .next()
            .expect("设置工作区应公开主要内容地标");
        assert_element_has_positive_size(&title, "设置标题");
        assert_element_has_positive_size(&main, "设置主要内容");
        let title_bottom = title.absolute_position().y + title.size().height;
        let main_gap = main.absolute_position().y - title_bottom;
        assert!(
            (0.0..=32.0).contains(&main_gap),
            "设置标题到主要内容的间距必须位于 0–32px，实际={main_gap}px"
        );

        let menu = ElementHandle::find_by_accessible_label(&window, "设置二级菜单")
            .next()
            .expect("设置工作区应公开二级菜单容器");
        assert!(
            (menu.size().width - 190.0).abs() < 0.5,
            "二级菜单宽度必须严格为 190px，实际={:?}",
            menu.size(),
        );
        assert!(
            menu.absolute_position().x + menu.size().width <= main.absolute_position().x,
            "二级菜单必须位于主要内容左侧且不覆盖：菜单={:?}/{:?}，主要内容={:?}/{:?}",
            menu.absolute_position(),
            menu.size(),
            main.absolute_position(),
            main.size(),
        );

        for label in [
            "常规",
            "相似度算法",
            "存储",
            "节点服务",
            "扫描与性能",
            "外部工具",
            "日志与诊断",
        ] {
            let item = ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .unwrap_or_else(|| panic!("应能找到设置二级菜单：{label}"));
            assert!(
                (item.size().height - 40.0).abs() < 0.5,
                "{label} 菜单项高度必须为 40px，实际={:?}",
                item.size(),
            );
        }

        let save = ElementHandle::find_by_accessible_label(&window, "保存设置")
            .next()
            .expect("设置页必须提供保存动作");
        assert_element_inside_window(&save, "保存设置", width, height);
    }

    accessible(&window, "常规").invoke_accessible_default_action();
    window
        .window()
        .set_size(slint::PhysicalSize::new(1080, 700));
    window
        .window()
        .take_snapshot()
        .expect("常规设置应完成软件渲染");
    let grid = ElementHandle::find_by_accessible_label(&window, "常规表单网格")
        .next()
        .expect("常规设置应公开标签、控件与说明三列网格");
    assert_element_has_positive_size(&grid, "常规表单网格");
    let labels = ["PostgreSQL URL 标签", "重连间隔（秒）标签", "删除方式标签"].map(|label| {
        ElementHandle::find_by_accessible_label(&window, label)
            .next()
            .unwrap_or_else(|| panic!("常规表单应公开 {label}"))
    });
    let controls = ["PostgreSQL URL 控件", "重连间隔（秒）控件", "删除方式控件"].map(|label| {
        ElementHandle::find_by_accessible_label(&window, label)
            .next()
            .unwrap_or_else(|| panic!("常规表单应公开 {label}"))
    });
    let label_x = labels[0].absolute_position().x;
    let control_x = controls[0].absolute_position().x;
    for (label, control) in labels.iter().zip(controls.iter()) {
        assert!(
            (label.absolute_position().x - label_x).abs() < 0.5
                && (control.absolute_position().x - control_x).abs() < 0.5
                && (control.size().height - 34.0).abs() < 0.5,
            "常规表单的标签列、控件列和 34px 控件高度必须稳定对齐：标签={:?}/{:?}，控件={:?}/{:?}",
            label.absolute_position(),
            label.size(),
            control.absolute_position(),
            control.size(),
        );
    }

    accessible(&window, "日志与诊断").invoke_accessible_default_action();
    window
        .window()
        .take_snapshot()
        .expect("日志与诊断应完成软件渲染");
    let diagnostics_scroll = ElementHandle::find_by_accessible_label(&window, "诊断内容滚动区")
        .next()
        .expect("日志与诊断必须有自己的内容 ScrollView");
    assert_element_has_positive_size(&diagnostics_scroll, "诊断内容滚动区");
    let status = ElementHandle::find_by_accessible_label(&window, "诊断状态卡")
        .next()
        .expect("日志与诊断必须公开状态卡");
    assert!(
        status.absolute_position().y >= diagnostics_scroll.absolute_position().y
            && status.absolute_position().y + status.size().height
                <= diagnostics_scroll.absolute_position().y + diagnostics_scroll.size().height,
        "诊断状态卡必须在滚动区初始视口内，状态卡={:?}/{:?}，滚动区={:?}/{:?}",
        status.absolute_position(),
        status.size(),
        diagnostics_scroll.absolute_position(),
        diagnostics_scroll.size(),
    );
    diagnostics_scroll.scroll(0.0, -1000.0);
    window
        .window()
        .take_snapshot()
        .expect("滚动后的日志与诊断应完成软件渲染");
    for label in ["诊断路径卡", "诊断动作栏"] {
        let card = ElementHandle::find_by_accessible_label(&window, label)
            .next()
            .unwrap_or_else(|| panic!("日志与诊断必须公开 {label}"));
        assert!(
            card.absolute_position().x >= diagnostics_scroll.absolute_position().x
                && card.absolute_position().x + card.size().width
                    <= diagnostics_scroll.absolute_position().x + diagnostics_scroll.size().width
                && card.absolute_position().y >= diagnostics_scroll.absolute_position().y
                && card.absolute_position().y + card.size().height
                    <= diagnostics_scroll.absolute_position().y + diagnostics_scroll.size().height,
            "{label} 必须经诊断 ScrollView 到达且完整落在视口内：卡片={:?}/{:?}，滚动区={:?}/{:?}",
            card.absolute_position(),
            card.size(),
            diagnostics_scroll.absolute_position(),
            diagnostics_scroll.size(),
        );
    }
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

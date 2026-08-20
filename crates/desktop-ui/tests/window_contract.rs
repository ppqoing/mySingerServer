use dedup_desktop_ui::{MainWindow, UiGroupRow, UiMemberRow, UiNodeRow, UiTaskRow};
use i_slint_backend_testing::ElementHandle;
use slint::{
    Color, ComponentHandle, Model, ModelRc, VecModel,
    platform::{PointerEventButton, WindowEvent},
};
use std::{cell::Cell, cell::RefCell, rc::Rc};

// 使用完整字面行模型驱动真实 MainWindow，避免从生产映射反推预期结果。
fn install_overview_fixture(window: &MainWindow) {
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
            index: 7,
            name: "远程节点".into(),
            address: "10.0.0.8:39092".into(),
            status: "离线".into(),
            status_color: Color::from_rgb_u8(148, 163, 184),
            machine_id: "machine-remote".into(),
            worker_text: "0/4 忙碌".into(),
            task_text: "0 排队 / 0 运行".into(),
            sync_text: "98 / 98".into(),
            error_text: "等待连接".into(),
        },
    ])));
    window.set_tasks(ModelRc::new(VecModel::from(vec![
        UiTaskRow {
            id: "task-queued".into(),
            node_index: 7,
            title: "等待扫描".into(),
            stage: "等待调度".into(),
            status: "排队中".into(),
            status_color: Color::from_rgb_u8(148, 163, 184),
            progress: 0,
            counts: "0 / 24 · 失败 0 · 跳过 0".into(),
        },
        UiTaskRow {
            id: "task-running".into(),
            node_index: 0,
            title: "图片分析".into(),
            stage: "提取特征".into(),
            status: "运行中".into(),
            status_color: Color::from_rgb_u8(59, 130, 246),
            progress: 45,
            counts: "9 / 20 · 失败 0 · 跳过 1".into(),
        },
        UiTaskRow {
            id: "task-completed".into(),
            node_index: 0,
            title: "视频扫描".into(),
            stage: "完成".into(),
            status: "已完成".into(),
            status_color: Color::from_rgb_u8(22, 163, 74),
            progress: 100,
            counts: "12 / 12 · 失败 0 · 跳过 0".into(),
        },
    ])));
    window.set_online_count(1);
    window.set_running_count(1);
    window.set_indexed_text("图片 18 · 视频 12".into());
    window.set_sync_text("游标 120 / 125".into());
}

// 使用三种状态字面量验证任务页签只筛选已加载模型，不模拟后端查询。
fn install_task_center_fixture(window: &MainWindow) {
    window.set_tasks(ModelRc::new(VecModel::from(vec![
        UiTaskRow {
            id: "task-running".into(),
            node_index: 7,
            title: "媒体扫描".into(),
            stage: "枚举文件".into(),
            status: "运行中".into(),
            status_color: Color::from_rgb_u8(59, 130, 246),
            progress: 35,
            counts: "7 / 20 · 失败 0 · 跳过 1".into(),
        },
        UiTaskRow {
            id: "task-completed".into(),
            node_index: 0,
            title: "图片分析".into(),
            stage: "完成".into(),
            status: "已完成".into(),
            status_color: Color::from_rgb_u8(22, 163, 74),
            progress: 100,
            counts: "18 / 18 · 失败 0 · 跳过 0".into(),
        },
        UiTaskRow {
            id: "task-failed".into(),
            node_index: 3,
            title: "视频分析".into(),
            stage: "提取特征".into(),
            status: "失败".into(),
            status_color: Color::from_rgb_u8(239, 68, 68),
            progress: 60,
            counts: "6 / 10 · 失败 1 · 跳过 0".into(),
        },
        UiTaskRow {
            id: "task-queued".into(),
            node_index: 4,
            title: "视频扫描".into(),
            stage: "等待调度".into(),
            status: "排队中".into(),
            status_color: Color::from_rgb_u8(107, 114, 128),
            progress: 0,
            counts: "0 / 8 · 失败 0 · 跳过 0".into(),
        },
    ])));
}

fn accessible(window: &MainWindow, label: &str) -> ElementHandle {
    ElementHandle::find_by_accessible_label(window, label)
        .next()
        .unwrap_or_else(|| panic!("应能找到可访问元素：{label}"))
}

// 从真实元素边界计算中心点，直接向测试窗口发送鼠标移动、按下和释放事件。
fn click_element_center(window: &MainWindow, element: &ElementHandle) {
    click_element_at_fraction(window, element, 0.5, 0.5);
}

// 横向可滚动行使用左侧可见标题区作为选择命中点，避免把内容宽度中心误当视口中心。
fn click_element_at_fraction(
    window: &MainWindow,
    element: &ElementHandle,
    x_fraction: f32,
    y_fraction: f32,
) {
    let position = element.absolute_position();
    let size = element.size();
    let center = slint::LogicalPosition::new(
        position.x + size.width * x_fraction,
        position.y + size.height * y_fraction,
    );
    window
        .window()
        .dispatch_event(WindowEvent::PointerMoved { position: center });
    window.window().dispatch_event(WindowEvent::PointerPressed {
        position: center,
        button: PointerEventButton::Left,
    });
    window
        .window()
        .dispatch_event(WindowEvent::PointerReleased {
            position: center,
            button: PointerEventButton::Left,
        });
}

#[test]
fn main_window_exposes_concept_defaults_and_generated_api() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");

    assert_eq!(window.get_current_page(), 0);
    assert_eq!(window.get_new_node_ip(), "127.0.0.1");
    assert_eq!(window.get_new_node_port(), 39091);
    assert_eq!(window.get_scan_root(), "D:\\Media");
    assert_eq!(window.get_enumerator_index(), 0);
    assert_eq!(window.get_delete_mode(), "回收站");

    window.set_current_page(3);
    window.set_new_node_ip("10.0.0.8".into());
    assert_eq!(window.get_current_page(), 3);
    assert_eq!(window.get_new_node_ip(), "10.0.0.8");
    window.invoke_connect_all();
}

#[test]
fn navigation_actions_preserve_page_mapping() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window.set_scan_root("E:\\媒体库".into());
    window.set_result_run_id("run-keep".into());

    let expected = [
        (0, 0, 0, 0), // 总览
        (0, 1, 1, 0), // 节点
        (1, 2, 1, 0), // 扫描
        (1, 3, 1, 1), // 任务
        (2, 4, 1, 1), // 重复文件
        (6, 5, 1, 1), // 审核删除
        (7, 6, 1, 1), // 设置
    ];
    for (current_page, active_nav, overview_mode, task_mode) in expected {
        window.invoke_navigate_to(active_nav);
        assert_eq!(
            (
                window.get_current_page(),
                window.get_active_nav(),
                window.get_overview_mode(),
                window.get_task_mode(),
            ),
            (current_page, active_nav, overview_mode, task_mode),
            "导航索引 {active_nav} 应使用唯一页面映射",
        );
    }

    for duplicate_page in 3..=5 {
        window.set_current_page(duplicate_page);
        window.invoke_navigate_to(4);
        assert_eq!(
            window.get_current_page(),
            duplicate_page,
            "重复文件导航不应重置已选重复类型",
        );
    }
    assert_eq!(window.get_scan_root(), "E:\\媒体库");
    assert_eq!(window.get_result_run_id(), "run-keep");

    let labels = [
        "总览",
        "节点",
        "扫描",
        "任务",
        "重复文件",
        "审核删除",
        "设置",
    ];
    for label in labels {
        assert!(
            ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .is_some(),
            "侧栏动作应提供稳定的中文标签：{label}",
        );
    }

    let node_action = ElementHandle::find_by_accessible_label(&window, "节点")
        .next()
        .expect("应能找到节点导航动作");
    node_action.invoke_accessible_default_action();
    assert_eq!(
        (
            window.get_current_page(),
            window.get_active_nav(),
            window.get_overview_mode(),
            window.get_task_mode(),
        ),
        (0, 1, 1, 1),
        "侧栏节点动作应复用 navigate-to 映射",
    );

    let refresh_count = Rc::new(Cell::new(0));
    window.on_refresh({
        let refresh_count = refresh_count.clone();
        move || refresh_count.set(refresh_count.get() + 1)
    });
    let refresh_action = ElementHandle::find_by_accessible_label(&window, "刷新")
        .next()
        .expect("应能找到刷新动作");
    refresh_action.invoke_accessible_default_action();
    assert_eq!(refresh_count.get(), 1, "刷新默认动作应只转发现有回调一次");
}

#[test]
fn shell_exposes_menu_search_and_one_refresh_action() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    let menu = accessible(&window, "应用菜单");
    assert_eq!(menu.size(), slint::LogicalSize::new(44.0, 44.0));
    assert!(accessible(&window, "本地搜索").size().width >= 220.0);

    let refresh_count = Rc::new(Cell::new(0));
    window.on_refresh({
        let refresh_count = refresh_count.clone();
        move || refresh_count.set(refresh_count.get() + 1)
    });
    let refresh = accessible(&window, "刷新");
    refresh.invoke_accessible_default_action();
    assert_eq!(refresh_count.get(), 1);
    click_element_center(&window, &refresh);
    assert_eq!(refresh_count.get(), 2, "刷新真实指针单击也必须只转发一次");
}

#[test]
fn settings_sections_preserve_real_values_and_save_once() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));
    window.set_postgres_url("postgresql://fixture@db.internal:5432/dedup".into());
    window.set_reconnect_seconds(17);
    window.set_delete_mode_index(1);
    window.set_pdq_quality("61".into());
    window.set_aspect_tolerance("0.07".into());
    window.set_pdq_hamming("27".into());
    window.set_phash_hamming("8".into());
    window.set_phash_parts("7".into());
    window.set_sobel_min("0.91".into());
    window.set_video_valid("5".into());
    window.set_video_stage1("0.86".into());
    window.set_video_stage2("0.89".into());
    window.set_data_path("D:\\Fixture\\data".into());
    window.set_config_path("D:\\Fixture\\data\\desktop\\config.toml".into());
    window.set_logs_path("D:\\Fixture\\data\\desktop\\logs".into());
    window.set_cache_path("D:\\Fixture\\data\\desktop\\cache".into());
    window.set_postgres_status("中心数据库健康".into());
    window.set_last_error("fixture：上次同步连接中断".into());

    accessible(&window, "设置").invoke_accessible_default_action();
    assert_eq!(window.get_current_page(), 7);

    let expected_values = || {
        assert_eq!(
            window.get_postgres_url(),
            "postgresql://fixture@db.internal:5432/dedup"
        );
        assert_eq!(window.get_reconnect_seconds(), 17);
        assert_eq!(window.get_delete_mode_index(), 1);
        assert_eq!(window.get_pdq_quality(), "61");
        assert_eq!(window.get_aspect_tolerance(), "0.07");
        assert_eq!(window.get_pdq_hamming(), "27");
        assert_eq!(window.get_phash_hamming(), "8");
        assert_eq!(window.get_phash_parts(), "7");
        assert_eq!(window.get_sobel_min(), "0.91");
        assert_eq!(window.get_video_valid(), "5");
        assert_eq!(window.get_video_stage1(), "0.86");
        assert_eq!(window.get_video_stage2(), "0.89");
    };
    for (section, label) in [
        (0, "常规"),
        (1, "相似度算法"),
        (2, "存储"),
        (3, "节点服务"),
        (4, "扫描与性能"),
        (5, "外部工具"),
        (6, "日志与诊断"),
    ] {
        accessible(&window, label).invoke_accessible_default_action();
        assert_eq!(
            window.get_settings_section(),
            section,
            "{label} 必须映射到固定设置分区"
        );
        expected_values();
    }

    accessible(&window, "相似度算法").invoke_accessible_default_action();
    accessible(&window, "关于 Slint").invoke_accessible_default_action();
    assert_eq!(
        window.get_settings_section(),
        1,
        "关于 Slint 只能切换视觉面板，不得改变当前设置分区"
    );
    expected_values();
    accessible(&window, "返回设置").invoke_accessible_default_action();
    assert_eq!(
        window.get_settings_section(),
        1,
        "返回设置也不得重置当前分区"
    );
    expected_values();

    accessible(&window, "存储").invoke_accessible_default_action();
    for label in [
        "数据路径：D:\\Fixture\\data",
        "配置路径：D:\\Fixture\\data\\desktop\\config.toml",
        "日志路径：D:\\Fixture\\data\\desktop\\logs",
        "缓存路径：D:\\Fixture\\data\\desktop\\cache",
    ] {
        assert!(
            ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .is_some(),
            "存储分区必须显示真实只读值：{label}"
        );
    }

    for (section, control) in [
        ("节点服务", "节点服务配置（当前版本未提供）"),
        ("扫描与性能", "扫描性能配置（当前版本未提供）"),
        ("外部工具", "外部工具配置（当前版本未提供）"),
    ] {
        accessible(&window, section).invoke_accessible_default_action();
        assert_eq!(
            accessible(&window, control).accessible_enabled(),
            Some(false),
            "{section} 的概念控件必须明确禁用"
        );
    }

    accessible(&window, "日志与诊断").invoke_accessible_default_action();
    for label in [
        "PostgreSQL 状态：中心数据库健康",
        "最后错误：fixture：上次同步连接中断",
    ] {
        assert!(
            ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .is_some(),
            "诊断分区必须显示真实状态：{label}"
        );
    }
    assert_eq!(
        accessible(&window, "日志筛选（当前版本未提供）").accessible_enabled(),
        Some(false),
        "日志诊断占位能力必须明确禁用"
    );

    let saves = Rc::new(Cell::new(0));
    window.on_save_settings({
        let saves = saves.clone();
        move || saves.set(saves.get() + 1)
    });
    accessible(&window, "保存设置").invoke_accessible_default_action();
    assert_eq!(saves.get(), 1, "保存设置动作必须只转发现有回调一次");
}

#[test]
fn overview_and_nodes_consume_real_models() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));
    install_overview_fixture(&window);
    window.invoke_navigate_to(0);

    for label in [
        "在线节点：1 台",
        "运行任务：1 个",
        "索引摘要：图片 18 · 视频 12",
        "同步摘要：游标 120 / 125",
        "总览节点：本机节点；127.0.0.1:39091；在线；Worker 1/2 忙碌；任务 1 排队 / 1 运行；同步 120 / 125",
        "总览节点：远程节点；10.0.0.8:39092；离线；Worker 0/4 忙碌；任务 0 排队 / 0 运行；同步 98 / 98",
        "最近任务：等待扫描；节点 7；等待调度；0%；排队中",
        "最近任务：图片分析；节点 0；提取特征；45%；运行中",
        "最近任务：视频扫描；节点 0；完成；100%；已完成",
    ] {
        assert!(
            ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .is_some(),
            "总览应消费真实模型并公开可观察内容：{label}",
        );
    }

    window.invoke_navigate_to(1);
    let local = accessible(
        &window,
        "节点项：本机节点；127.0.0.1:39091；在线；Worker 1/2 忙碌；任务 1 排队 / 1 运行；同步 120 / 125",
    );
    let remote = accessible(
        &window,
        "节点项：远程节点；10.0.0.8:39092；离线；Worker 0/4 忙碌；任务 0 排队 / 0 运行；同步 98 / 98",
    );
    assert_eq!(local.accessible_item_selected(), Some(true));
    assert_eq!(remote.accessible_item_selected(), Some(false));

    remote.invoke_accessible_default_action();
    assert_eq!(local.accessible_item_selected(), Some(false));
    assert_eq!(remote.accessible_item_selected(), Some(true));
    assert!(
        ElementHandle::find_by_accessible_label(&window, "节点错误：等待连接")
            .next()
            .is_some(),
        "节点详情必须通过完整可访问文本表达连接错误，不能只使用颜色",
    );
}

#[test]
fn node_add_forwards_entered_ip_and_port() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_overview_fixture(&window);
    window.invoke_navigate_to(1);
    window.set_new_node_ip("192.168.50.18".into());
    window.set_new_node_port(40123);

    let captured = Rc::new(RefCell::new(Vec::new()));
    window.on_add_node({
        let captured = captured.clone();
        move |ip, port| captured.borrow_mut().push((ip.to_string(), port))
    });
    accessible(&window, "添加节点").invoke_accessible_default_action();

    assert_eq!(
        captured.borrow().as_slice(),
        &[(String::from("192.168.50.18"), 40123)],
        "添加动作应只调用一次，并原样转发根表单双向绑定值",
    );
}

#[test]
fn selected_node_actions_forward_existing_callbacks() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));
    install_overview_fixture(&window);
    window.invoke_navigate_to(1);
    window.set_new_node_ip("10.20.30.40".into());
    window.set_new_node_port(41000);
    accessible(
        &window,
        "节点项：远程节点；10.0.0.8:39092；离线；Worker 0/4 忙碌；任务 0 排队 / 0 运行；同步 98 / 98",
    )
    .invoke_accessible_default_action();

    let edited = Rc::new(RefCell::new(Vec::new()));
    window.on_edit_node({
        let edited = edited.clone();
        move |index, ip, port| edited.borrow_mut().push((index, ip.to_string(), port))
    });
    let synced = Rc::new(RefCell::new(Vec::new()));
    window.on_sync_node({
        let synced = synced.clone();
        move |index| synced.borrow_mut().push(index)
    });
    let removed = Rc::new(RefCell::new(Vec::new()));
    window.on_remove_node({
        let removed = removed.clone();
        move |index| removed.borrow_mut().push(index)
    });
    let connected = Rc::new(Cell::new(0));
    window.on_connect_all({
        let connected = connected.clone();
        move || connected.set(connected.get() + 1)
    });

    accessible(&window, "编辑节点").invoke_accessible_default_action();
    accessible(&window, "立即同步").invoke_accessible_default_action();
    accessible(&window, "移除节点").invoke_accessible_default_action();
    accessible(&window, "连接全部节点").invoke_accessible_default_action();

    assert_eq!(
        edited.borrow().as_slice(),
        &[(7, String::from("10.20.30.40"), 41000)],
        "编辑动作应只调用一次，并保持索引、IP 和端口顺序",
    );
    assert_eq!(
        synced.borrow().as_slice(),
        &[7],
        "同步动作应只调用一次并转发选中节点索引",
    );
    assert_eq!(
        removed.borrow().as_slice(),
        &[7],
        "移除动作应只调用一次并转发选中节点索引",
    );
    assert_eq!(connected.get(), 1);
}

#[test]
fn scan_start_forwards_four_arguments_in_order() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1080, 700));
    window.invoke_navigate_to(2);
    window.set_scan_node_index(7);
    window.set_scan_root("D:\\fixture".into());
    window.set_force_recalculate(true);
    window.set_enumerator_index(1);
    window.set_analysis_kind_index(2);

    let captured = Rc::new(RefCell::new(None));
    window.on_start_scan({
        let captured = captured.clone();
        move |node_index, path, force, enumerator| {
            *captured.borrow_mut() = Some((node_index, path.to_string(), force, enumerator));
        }
    });
    accessible(&window, "开始扫描").invoke_accessible_default_action();

    assert_eq!(
        captured.borrow().as_ref(),
        Some(&(7, String::from("D:\\fixture"), true, 1)),
        "扫描动作必须保持 node_index、path、force、enumerator 的原有顺序，且不混入分析类型",
    );

    window.invoke_navigate_to(3);
    assert!(
        ElementHandle::find_by_accessible_label(&window, "开始扫描")
            .next()
            .is_none(),
        "任务工作区不得继续暴露扫描创建动作",
    );
}

#[test]
fn scan_browse_and_local_analysis_forward_only_existing_arguments() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1080, 700));
    window.invoke_navigate_to(2);
    window.set_scan_node_index(7);
    window.set_scan_root("D:\\fixture".into());
    window.set_filtering_enabled(true);
    window.set_analysis_task_ids("scan-a,scan-b".into());
    window.set_analysis_kind_index(2);

    let browsed = Rc::new(RefCell::new(None));
    window.on_browse_paths({
        let browsed = browsed.clone();
        move |node_index, path| {
            *browsed.borrow_mut() = Some((node_index, path.to_string()));
        }
    });
    let analyzed = Rc::new(RefCell::new(None));
    window.on_start_local_analysis({
        let analyzed = analyzed.clone();
        move |node_index, task_ids, kind| {
            *analyzed.borrow_mut() = Some((node_index, task_ids.to_string(), kind));
        }
    });

    accessible(&window, "浏览节点路径").invoke_accessible_default_action();
    accessible(&window, "开始本地分析").invoke_accessible_default_action();

    assert_eq!(
        browsed.borrow().as_ref(),
        Some(&(7, String::from("D:\\fixture"))),
    );
    assert_eq!(
        analyzed.borrow().as_ref(),
        Some(&(7, String::from("scan-a,scan-b"), 2)),
    );
}

#[test]
fn shared_components_keep_task_rows_dense_and_progress_readable() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_task_center_fixture(&window);
    window.invoke_navigate_to(3);

    let row = accessible(
        &window,
        "任务项：媒体扫描；节点 7；枚举文件；35%；7 / 20 · 失败 0 · 跳过 1；运行中",
    );
    assert!(
        (44.0..=64.0).contains(&row.size().height),
        "双行任务行应在 44–64px 内，实际={:?}",
        row.size(),
    );

    let progress = accessible(&window, "任务进度：35%");
    assert!(
        progress.size().width >= 120.0 && progress.size().height >= 8.0,
        "任务进度必须保留可读尺寸，实际={:?}",
        progress.size(),
    );
}

#[test]
fn task_tabs_filter_loaded_models_and_cancel_active_task() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_task_center_fixture(&window);
    window.invoke_navigate_to(3);
    let cancelled = Rc::new(RefCell::new(Vec::new()));
    window.on_cancel_task({
        let cancelled = cancelled.clone();
        move |node_index, id| cancelled.borrow_mut().push((node_index, id.to_string()))
    });

    assert_eq!(window.get_task_tab(), 0);
    assert_eq!(
        accessible(
            &window,
            "任务项：媒体扫描；节点 7；枚举文件；35%；7 / 20 · 失败 0 · 跳过 1；运行中"
        )
        .accessible_item_selected(),
        Some(true)
    );
    assert!(
        ElementHandle::find_by_accessible_label(
            &window,
            "任务项：图片分析；节点 0；完成；100%；18 / 18 · 失败 0 · 跳过 0；已完成"
        )
        .next()
        .is_none()
    );
    assert!(
        ElementHandle::find_by_accessible_label(
            &window,
            "任务项：视频分析；节点 3；提取特征；60%；6 / 10 · 失败 1 · 跳过 0；失败"
        )
        .next()
        .is_none()
    );

    // 先用真实指针选择另一行，再证明运行中行的非按钮中心区域能恢复选中。
    accessible(&window, "失败").invoke_accessible_default_action();
    let failed_row = accessible(
        &window,
        "任务项：视频分析；节点 3；提取特征；60%；6 / 10 · 失败 1 · 跳过 0；失败",
    );
    click_element_at_fraction(&window, &failed_row, 0.2, 0.5);
    assert_eq!(
        failed_row.accessible_item_selected(),
        Some(true),
        "失败行标题区点击应只选择自身，位置={:?}，尺寸={:?}",
        failed_row.absolute_position(),
        failed_row.size(),
    );
    assert!(cancelled.borrow().is_empty(), "任务行选择不得命中取消回调");

    accessible(&window, "运行中").invoke_accessible_default_action();
    let running_row = accessible(
        &window,
        "任务项：媒体扫描；节点 7；枚举文件；35%；7 / 20 · 失败 0 · 跳过 1；运行中",
    );
    assert_eq!(running_row.accessible_item_selected(), Some(false));
    click_element_at_fraction(&window, &running_row, 0.2, 0.5);
    assert_eq!(
        running_row.accessible_item_selected(),
        Some(true),
        "点击运行中任务行的非按钮区域应更新 selected-task-index",
    );
    assert!(
        cancelled.borrow().is_empty(),
        "运行中任务行选择也不得命中取消回调"
    );

    let task_table = accessible(&window, "任务主表");
    let task_scroll = task_table
        .query_descendants()
        .match_inherits("ScrollView")
        .find_first()
        .expect("任务主表应通过自己的 ScrollView 到达取消列");
    task_scroll.scroll(-1000.0, 0.0);
    let cancel_button = accessible(&window, "取消任务：task-running");
    click_element_center(&window, &cancel_button);
    assert_eq!(
        cancelled.borrow().as_slice(),
        &[(7, String::from("task-running"))]
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "取消任务：task-queued")
            .next()
            .is_none(),
        "排队中任务不得生成取消可访问元素",
    );

    accessible(&window, "已完成").invoke_accessible_default_action();
    assert_eq!(window.get_task_tab(), 1);
    assert!(
        ElementHandle::find_by_accessible_label(
            &window,
            "任务项：媒体扫描；节点 7；枚举文件；35%；7 / 20 · 失败 0 · 跳过 1；运行中"
        )
        .next()
        .is_none()
    );
    assert_eq!(
        accessible(
            &window,
            "任务项：图片分析；节点 0；完成；100%；18 / 18 · 失败 0 · 跳过 0；已完成"
        )
        .accessible_item_selected(),
        Some(false)
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "取消任务：task-completed")
            .next()
            .is_none()
    );

    accessible(&window, "失败").invoke_accessible_default_action();
    assert_eq!(window.get_task_tab(), 2);
    assert!(
        ElementHandle::find_by_accessible_label(
            &window,
            "任务项：图片分析；节点 0；完成；100%；18 / 18 · 失败 0 · 跳过 0；已完成"
        )
        .next()
        .is_none()
    );
    assert_eq!(
        accessible(
            &window,
            "任务项：视频分析；节点 3；提取特征；60%；6 / 10 · 失败 1 · 跳过 0；失败"
        )
        .accessible_item_selected(),
        Some(false)
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "取消任务：task-failed")
            .next()
            .is_none()
    );
    assert_eq!(cancelled.borrow().len(), 1, "终态任务不得产生新的取消动作");

    window.set_tasks(ModelRc::new(VecModel::from(vec![UiTaskRow {
        id: "task-running-only".into(),
        node_index: 7,
        title: "仅运行中任务".into(),
        stage: "枚举文件".into(),
        status: "运行中".into(),
        status_color: Color::from_rgb_u8(59, 130, 246),
        progress: 20,
        counts: "2 / 10 · 失败 0 · 跳过 0".into(),
    }])));
    accessible(&window, "已完成").invoke_accessible_default_action();
    accessible(&window, "失败").invoke_accessible_default_action();
    assert!(
        ElementHandle::find_by_accessible_label(&window, "任务表空态：失败")
            .next()
            .is_some(),
        "模型非空但当前页签没有匹配任务时，空态必须留在表体内",
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "任务详情")
            .next()
            .is_some(),
        "无匹配任务时仍应保留固定详情结构",
    );
}

#[test]
fn duplicate_tabs_preserve_loaded_state_and_forward_existing_callbacks() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));
    // 字面模型独立定义期望，防止测试复用生产映射而形成镜像断言。
    window.set_groups(ModelRc::new(VecModel::from(vec![UiGroupRow {
        id: "group-001".into(),
        kind: "相似图片".into(),
        md5: "0123456789abcdef0123456789abcdef".into(),
        size: "8.0 MiB".into(),
        members: 2,
        reclaimable: "8.0 MiB".into(),
    }])));
    window.set_members(ModelRc::new(VecModel::from(vec![
        UiMemberRow {
            machine_id: "machine-a".into(),
            path: "D:\\Media\\photo-a.jpg".into(),
            md5: "0123456789abcdef0123456789abcdef".into(),
            size: "8.0 MiB".into(),
            representative: true,
            stage1: "0.99".into(),
            phash: "2".into(),
            stage2: "0.97".into(),
            metadata: "3840×2160 JPEG".into(),
            review: "未决定".into(),
            review_color: Color::from_rgb_u8(107, 114, 128),
            online: true,
            preview_enabled: true,
            delete_enabled: false,
        },
        UiMemberRow {
            machine_id: "machine-b".into(),
            path: "E:\\Archive\\photo-b.jpg".into(),
            md5: "fedcba9876543210fedcba9876543210".into(),
            size: "8.0 MiB".into(),
            representative: false,
            stage1: "0.98".into(),
            phash: "3".into(),
            stage2: "0.96".into(),
            metadata: "3840×2160 JPEG".into(),
            review: "未决定".into(),
            review_color: Color::from_rgb_u8(107, 114, 128),
            online: true,
            preview_enabled: true,
            delete_enabled: true,
        },
    ])));
    window.set_group_next_cursor("opaque-group-cursor".into());
    window.set_member_next_cursor("opaque-member-cursor".into());
    window.set_result_source_index(1);
    window.set_result_node_index(7);
    window.set_result_run_id("run-state-001".into());
    window.set_selected_group_id("group-001".into());
    window.set_cross_selections("7:scan-a,8:scan-b".into());
    window.set_cross_status("partial".into());
    window.invoke_navigate_to(4);

    let loaded_groups = Rc::new(RefCell::new(Vec::new()));
    window.on_load_groups({
        let loaded_groups = loaded_groups.clone();
        move |central, node, run, kind, cursor| {
            loaded_groups.borrow_mut().push((
                central,
                node,
                run.to_string(),
                kind,
                cursor.to_string(),
            ));
        }
    });
    let loaded_members = Rc::new(RefCell::new(Vec::new()));
    window.on_load_members({
        let loaded_members = loaded_members.clone();
        move |central, node, run, group, kind, cursor| {
            loaded_members.borrow_mut().push((
                central,
                node,
                run.to_string(),
                group.to_string(),
                kind,
                cursor.to_string(),
            ));
        }
    });
    let previews = Rc::new(RefCell::new(Vec::new()));
    window.on_load_preview({
        let previews = previews.clone();
        move |machine, path| {
            previews
                .borrow_mut()
                .push((machine.to_string(), path.to_string()));
        }
    });
    let reviews = Rc::new(RefCell::new(Vec::new()));
    window.on_save_review({
        let reviews = reviews.clone();
        move |machine, path, decision| {
            reviews
                .borrow_mut()
                .push((machine.to_string(), path.to_string(), decision));
        }
    });
    let cross_starts = Rc::new(RefCell::new(Vec::new()));
    window.on_start_cross_analysis({
        let cross_starts = cross_starts.clone();
        move |selections| cross_starts.borrow_mut().push(selections.to_string())
    });
    let cross_polls = Rc::new(Cell::new(0));
    window.on_poll_cross_analysis({
        let cross_polls = cross_polls.clone();
        move || cross_polls.set(cross_polls.get() + 1)
    });
    let cross_retries = Rc::new(Cell::new(0));
    window.on_retry_cross_analysis({
        let cross_retries = cross_retries.clone();
        move || cross_retries.set(cross_retries.get() + 1)
    });

    assert_eq!(previews.borrow().len(), 0, "注入模型不得触发预览加载");
    for (label, page) in [
        ("精确重复", 2),
        ("相似图片", 3),
        ("相似视频", 4),
        ("跨机器", 5),
    ] {
        accessible(&window, label).invoke_accessible_default_action();
        assert_eq!(
            window.get_current_page(),
            page,
            "{label} 必须映射到固定页面"
        );
        assert_eq!(
            window.get_groups().row_count(),
            1,
            "切换 {label} 不得清空组模型"
        );
        assert_eq!(
            window.get_members().row_count(),
            2,
            "切换 {label} 不得清空成员模型"
        );
        assert_eq!(
            window.get_groups().row_data(0).expect("组行应保留").id,
            "group-001"
        );
        assert_eq!(window.get_group_next_cursor(), "opaque-group-cursor");
        assert_eq!(window.get_member_next_cursor(), "opaque-member-cursor");
        assert_eq!(window.get_result_run_id(), "run-state-001");
        assert_eq!(window.get_selected_group_id(), "group-001");
    }
    assert_eq!(previews.borrow().len(), 0, "图片和视频标签切换不得预取预览");

    accessible(&window, "精确重复").invoke_accessible_default_action();
    accessible(&window, "加载结果").invoke_accessible_default_action();
    accessible(&window, "加载下一页重复组").invoke_accessible_default_action();
    click_element_at_fraction(&window, &accessible(&window, "重复组：group-001"), 0.2, 0.5);
    accessible(&window, "加载下一页成员").invoke_accessible_default_action();
    click_element_center(
        &window,
        &accessible(&window, "预览成员：D:\\Media\\photo-a.jpg"),
    );
    assert_eq!(
        previews.borrow().len(),
        1,
        "用户选择成员后才允许加载一次预览"
    );
    click_element_center(
        &window,
        &accessible(&window, "保留成员：D:\\Media\\photo-a.jpg"),
    );

    accessible(&window, "相似图片").invoke_accessible_default_action();
    accessible(&window, "加载结果").invoke_accessible_default_action();
    accessible(&window, "相似视频").invoke_accessible_default_action();
    accessible(&window, "加载结果").invoke_accessible_default_action();
    accessible(&window, "跨机器").invoke_accessible_default_action();
    accessible(&window, "加载结果").invoke_accessible_default_action();
    accessible(&window, "创建中心分析").invoke_accessible_default_action();
    accessible(&window, "推进到下一门禁").invoke_accessible_default_action();
    accessible(&window, "重试未解决二筛").invoke_accessible_default_action();

    assert_eq!(
        loaded_groups.borrow().as_slice(),
        &[
            (true, 7, String::from("run-state-001"), 0, String::new()),
            (
                true,
                7,
                String::from("run-state-001"),
                0,
                String::from("opaque-group-cursor")
            ),
            (true, 7, String::from("run-state-001"), 1, String::new()),
            (true, 7, String::from("run-state-001"), 2, String::new()),
            (true, 7, String::from("run-state-001"), 0, String::new()),
        ],
        "四标签和有限分页必须保持 load-groups 参数顺序与普通类型 0/1/2",
    );
    assert_eq!(
        loaded_members.borrow().as_slice(),
        &[
            (
                true,
                7,
                String::from("run-state-001"),
                String::from("group-001"),
                0,
                String::new(),
            ),
            (
                true,
                7,
                String::from("run-state-001"),
                String::from("group-001"),
                0,
                String::from("opaque-member-cursor"),
            ),
        ],
        "选择组和成员分页必须保持 load-members 六参数顺序",
    );
    assert_eq!(
        previews.borrow().as_slice(),
        &[(
            String::from("machine-a"),
            String::from("D:\\Media\\photo-a.jpg")
        )]
    );
    assert_eq!(
        reviews.borrow().as_slice(),
        &[(
            String::from("machine-a"),
            String::from("D:\\Media\\photo-a.jpg"),
            1,
        )]
    );
    assert_eq!(
        cross_starts.borrow().as_slice(),
        &[String::from("7:scan-a,8:scan-b")]
    );
    assert_eq!(cross_polls.get(), 1);
    assert_eq!(cross_retries.get(), 1);
}

#[test]
fn review_preview_is_single_flight_and_keeps_decision_target_aligned() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));
    window.set_members(ModelRc::new(VecModel::from(vec![
        UiMemberRow {
            machine_id: "machine-a".into(),
            path: "D:\\Media\\review-a.jpg".into(),
            md5: "0000000000000000000000000000000a".into(),
            size: "1.0 MiB".into(),
            representative: false,
            stage1: "0.99".into(),
            phash: "1".into(),
            stage2: "0.98".into(),
            metadata: "1920×1080 JPEG".into(),
            review: "未决定".into(),
            review_color: Color::from_rgb_u8(107, 114, 128),
            online: true,
            preview_enabled: true,
            delete_enabled: true,
        },
        UiMemberRow {
            machine_id: "machine-b".into(),
            path: "D:\\Media\\review-b.jpg".into(),
            md5: "0000000000000000000000000000000b".into(),
            size: "2.0 MiB".into(),
            representative: false,
            stage1: "0.97".into(),
            phash: "2".into(),
            stage2: "0.96".into(),
            metadata: "2560×1440 JPEG".into(),
            review: "未决定".into(),
            review_color: Color::from_rgb_u8(107, 114, 128),
            online: true,
            preview_enabled: true,
            delete_enabled: true,
        },
    ])));
    window.invoke_navigate_to(5);

    let previews = Rc::new(RefCell::new(Vec::new()));
    window.on_load_preview({
        let previews = previews.clone();
        move |machine, path| {
            previews
                .borrow_mut()
                .push((machine.to_string(), path.to_string()));
        }
    });
    let reviews = Rc::new(RefCell::new(Vec::new()));
    window.on_save_review({
        let reviews = reviews.clone();
        move |machine, path, decision| {
            reviews
                .borrow_mut()
                .push((machine.to_string(), path.to_string(), decision));
        }
    });

    let row_a = accessible(&window, "成员：D:\\Media\\review-a.jpg");
    let row_b = accessible(&window, "成员：D:\\Media\\review-b.jpg");
    click_element_at_fraction(&window, &row_a, 0.35, 0.5);
    assert_eq!(row_a.accessible_item_selected(), Some(true));
    assert_eq!(row_b.accessible_item_selected(), Some(false));
    assert!(
        previews.borrow().is_empty(),
        "点击行空白只能选择，不得触发预览"
    );

    click_element_center(
        &window,
        &accessible(&window, "预览成员：D:\\Media\\review-a.jpg"),
    );
    assert_eq!(
        previews.borrow().as_slice(),
        &[(
            String::from("machine-a"),
            String::from("D:\\Media\\review-a.jpg"),
        )],
        "A 行预览按钮只应触发一次精确回调",
    );
    window.set_last_error("无关的节点同步错误".into());
    slint::platform::update_timers_and_animations();
    click_element_center(
        &window,
        &accessible(&window, "预览成员：D:\\Media\\review-b.jpg"),
    );
    assert_eq!(
        previews.borrow().as_slice(),
        &[(
            String::from("machine-a"),
            String::from("D:\\Media\\review-a.jpg"),
        )],
        "A 未返回时 B 行预览必须禁用，不能把第二个请求加入控制器队列",
    );
    assert_eq!(
        accessible(&window, "预览成员：D:\\Media\\review-a.jpg").accessible_enabled(),
        Some(false),
        "A 在途时所有预览动作都应禁用",
    );
    assert_eq!(
        accessible(&window, "预览成员：D:\\Media\\review-b.jpg").accessible_enabled(),
        Some(false),
        "A 在途时 B 预览动作必须显示为禁用",
    );

    click_element_at_fraction(&window, &row_b, 0.35, 0.5);
    assert_eq!(row_a.accessible_item_selected(), Some(false));
    assert_eq!(
        row_b.accessible_item_selected(),
        Some(true),
        "预览在途时仍应允许通过行空白把详情目标切换到 B",
    );

    let image_a = slint::Image::from_rgba8(
        slint::SharedPixelBuffer::<slint::Rgba8Pixel>::clone_from_slice(&[20, 40, 60, 255], 1, 1),
    );
    window.set_preview_image(image_a);
    window.set_preview_info("A 预览".into());
    window.set_preview_result_machine("machine-a".into());
    window.set_preview_result_path("D:\\Media\\review-a.jpg".into());
    window.set_preview_result_succeeded(true);
    window.set_preview_result_sequence(1);
    slint::platform::update_timers_and_animations();
    assert!(
        ElementHandle::find_by_accessible_label(&window, "当前预览：D:\\Media\\review-a.jpg")
            .next()
            .is_none(),
        "A 在选择 B 后返回时不得把 A 图片冒充成 B 的当前预览",
    );
    assert!(
        ElementHandle::find_by_accessible_label(
            &window,
            "预览需要重新加载：D:\\Media\\review-b.jpg",
        )
        .next()
        .is_some(),
        "A 返回后仍应提示用户重新预览当前选择 B",
    );
    assert_eq!(
        accessible(&window, "预览成员：D:\\Media\\review-b.jpg").accessible_enabled(),
        Some(true),
        "唯一在途的 A 返回后应重新启用 B 预览",
    );

    click_element_center(
        &window,
        &accessible(&window, "预览成员：D:\\Media\\review-b.jpg"),
    );
    assert_eq!(
        previews.borrow().as_slice(),
        &[
            (
                String::from("machine-a"),
                String::from("D:\\Media\\review-a.jpg"),
            ),
            (
                String::from("machine-b"),
                String::from("D:\\Media\\review-b.jpg"),
            ),
        ],
        "A 完成后用户重新点击 B 才能发出第二个预览请求",
    );

    let image_b = slint::Image::from_rgba8(
        slint::SharedPixelBuffer::<slint::Rgba8Pixel>::clone_from_slice(&[80, 100, 120, 255], 1, 1),
    );
    window.set_preview_image(image_b);
    window.set_preview_info("B 预览".into());
    window.set_preview_result_machine("machine-b".into());
    window.set_preview_result_path("D:\\Media\\review-b.jpg".into());
    window.set_preview_result_succeeded(true);
    window.set_preview_result_sequence(2);
    slint::platform::update_timers_and_animations();
    assert!(
        ElementHandle::find_by_accessible_label(&window, "当前预览：D:\\Media\\review-b.jpg")
            .next()
            .is_some(),
        "single-flight 的 B 成功响应应归属 B",
    );

    previews.borrow_mut().clear();
    click_element_at_fraction(&window, &row_a, 0.35, 0.5);
    click_element_center(
        &window,
        &accessible(&window, "预览成员：D:\\Media\\review-a.jpg"),
    );
    assert_eq!(
        previews.borrow().as_slice(),
        &[(
            String::from("machine-a"),
            String::from("D:\\Media\\review-a.jpg"),
        )],
        "失败恢复场景应先只发出 A",
    );
    assert_eq!(
        accessible(&window, "预览成员：D:\\Media\\review-b.jpg").accessible_enabled(),
        Some(false),
    );

    click_element_at_fraction(&window, &row_b, 0.35, 0.5);
    assert!(
        ElementHandle::find_by_accessible_label(&window, "当前预览：D:\\Media\\review-b.jpg")
            .next()
            .is_none(),
        "新 A 请求在途时不得继续显示上一次 B 图片",
    );
    window.set_last_error("A 预览失败：测试错误".into());
    window.set_preview_result_machine("machine-a".into());
    window.set_preview_result_path("D:\\Media\\review-a.jpg".into());
    window.set_preview_result_succeeded(false);
    window.set_preview_result_sequence(3);
    slint::platform::update_timers_and_animations();
    assert_eq!(
        accessible(&window, "预览成员：D:\\Media\\review-b.jpg").accessible_enabled(),
        Some(true),
        "关联到 pending A 的失败完成必须解除 single-flight 门禁",
    );
    assert!(
        ElementHandle::find_by_accessible_label(
            &window,
            "预览需要重新加载：D:\\Media\\review-b.jpg",
        )
        .next()
        .is_some(),
        "A 失败后旧图片仍不得冒充当前 B",
    );

    click_element_at_fraction(&window, &row_a, 0.35, 0.5);
    click_element_center(
        &window,
        &accessible(&window, "预览成员：D:\\Media\\review-a.jpg"),
    );
    assert_eq!(
        previews.borrow().as_slice(),
        &[
            (
                String::from("machine-a"),
                String::from("D:\\Media\\review-a.jpg"),
            ),
            (
                String::from("machine-a"),
                String::from("D:\\Media\\review-a.jpg"),
            ),
        ],
        "第一次 A 失败后必须允许精确重试同一 A",
    );
    window.set_last_error("A 预览失败：测试错误".into());
    window.set_preview_result_machine("machine-a".into());
    window.set_preview_result_path("D:\\Media\\review-a.jpg".into());
    window.set_preview_result_succeeded(false);
    window.set_preview_result_sequence(4);
    slint::platform::update_timers_and_animations();
    assert_eq!(
        accessible(&window, "预览成员：D:\\Media\\review-b.jpg").accessible_enabled(),
        Some(true),
        "相同 A 和相同错误文本的第二次失败也必须靠新 sequence 释放门禁",
    );

    click_element_center(
        &window,
        &accessible(&window, "预览成员：D:\\Media\\review-b.jpg"),
    );
    assert_eq!(
        previews.borrow().as_slice(),
        &[
            (
                String::from("machine-a"),
                String::from("D:\\Media\\review-a.jpg"),
            ),
            (
                String::from("machine-a"),
                String::from("D:\\Media\\review-a.jpg"),
            ),
            (
                String::from("machine-b"),
                String::from("D:\\Media\\review-b.jpg"),
            ),
        ],
        "两次 A 失败后重新启用的 B 只能精确入队一次",
    );
    let recovered_b = slint::Image::from_rgba8(
        slint::SharedPixelBuffer::<slint::Rgba8Pixel>::clone_from_slice(
            &[100, 120, 140, 255],
            1,
            1,
        ),
    );
    window.set_preview_image(recovered_b);
    window.set_preview_info("B 恢复预览".into());
    window.set_last_error("".into());
    window.set_preview_result_machine("machine-b".into());
    window.set_preview_result_path("D:\\Media\\review-b.jpg".into());
    window.set_preview_result_succeeded(true);
    window.set_preview_result_sequence(5);
    slint::platform::update_timers_and_animations();
    assert!(
        ElementHandle::find_by_accessible_label(&window, "当前预览：D:\\Media\\review-b.jpg")
            .next()
            .is_some(),
        "失败恢复后的 B 成功响应应重新建立 B owner",
    );

    click_element_center(&window, &accessible(&window, "标记删除"));
    assert_eq!(
        reviews.borrow().as_slice(),
        &[(
            String::from("machine-b"),
            String::from("D:\\Media\\review-b.jpg"),
            2,
        )],
        "恢复后详情复核必须精确保存 B 一次，不能误写 A",
    );
}

#[test]
fn review_filters_loaded_members_and_delete_confirmation_obeys_gate() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    window
        .window()
        .set_size(slint::PhysicalSize::new(1440, 900));
    // 三种字面状态分别守护三个本地筛选分支；期望不复用生产筛选逻辑。
    window.set_members(ModelRc::new(VecModel::from(vec![
        UiMemberRow {
            machine_id: "machine-pending".into(),
            path: "D:\\Media\\pending.jpg".into(),
            md5: "00000000000000000000000000000001".into(),
            size: "1.0 MiB".into(),
            representative: true,
            stage1: "0.99".into(),
            phash: "1".into(),
            stage2: "0.98".into(),
            metadata: "1920×1080 JPEG".into(),
            review: "未决定".into(),
            review_color: Color::from_rgb_u8(107, 114, 128),
            online: true,
            preview_enabled: true,
            delete_enabled: false,
        },
        UiMemberRow {
            machine_id: "machine-kept".into(),
            path: "D:\\Media\\kept.jpg".into(),
            md5: "00000000000000000000000000000002".into(),
            size: "2.0 MiB".into(),
            representative: false,
            stage1: "0.97".into(),
            phash: "2".into(),
            stage2: "0.96".into(),
            metadata: "2560×1440 JPEG".into(),
            review: "保留".into(),
            review_color: Color::from_rgb_u8(22, 163, 74),
            online: true,
            preview_enabled: true,
            delete_enabled: false,
        },
        UiMemberRow {
            machine_id: "machine-delete".into(),
            path: "D:\\Media\\delete.jpg".into(),
            md5: "00000000000000000000000000000003".into(),
            size: "3.0 MiB".into(),
            representative: false,
            stage1: "0.95".into(),
            phash: "3".into(),
            stage2: "0.94".into(),
            metadata: "3840×2160 JPEG".into(),
            review: "删除".into(),
            review_color: Color::from_rgb_u8(239, 68, 68),
            online: true,
            preview_enabled: true,
            delete_enabled: true,
        },
    ])));
    window.set_selected_group_id("group-review-001".into());
    window.invoke_navigate_to(5);

    assert_eq!(window.get_review_tab(), 0);
    assert_eq!(window.get_review_filter(), 0);
    for label in ["未决定", "保留", "删除"] {
        assert!(
            ElementHandle::find_by_accessible_label(&window, label)
                .next()
                .is_some(),
            "审核筛选应使用真实领域语义：{label}",
        );
    }
    for stale_label in ["待审核", "已决定", "已忽略"] {
        assert!(
            ElementHandle::find_by_accessible_label(&window, stale_label)
                .next()
                .is_none(),
            "不得继续显示误导性的审核筛选：{stale_label}",
        );
    }
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\pending.jpg")
            .next()
            .is_some()
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\kept.jpg")
            .next()
            .is_none()
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\delete.jpg")
            .next()
            .is_none()
    );

    accessible(&window, "保留").invoke_accessible_default_action();
    assert_eq!(window.get_review_filter(), 1);
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\pending.jpg")
            .next()
            .is_none()
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\kept.jpg")
            .next()
            .is_some()
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\delete.jpg")
            .next()
            .is_none()
    );

    accessible(&window, "删除").invoke_accessible_default_action();
    assert_eq!(window.get_review_filter(), 2);
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\pending.jpg")
            .next()
            .is_none()
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\kept.jpg")
            .next()
            .is_none()
    );
    assert!(
        ElementHandle::find_by_accessible_label(&window, "成员：D:\\Media\\delete.jpg")
            .next()
            .is_some()
    );

    accessible(&window, "删除中心").invoke_accessible_default_action();
    assert_eq!(window.get_review_tab(), 1);
    assert_eq!(window.get_delete_filter(), 0);

    let prepared = Rc::new(Cell::new(0));
    window.on_prepare_delete({
        let prepared = prepared.clone();
        move || prepared.set(prepared.get() + 1)
    });
    accessible(&window, "准备删除").invoke_accessible_default_action();
    assert_eq!(prepared.get(), 1, "待执行只能转发一次现有准备删除回调");

    accessible(&window, "执行中").invoke_accessible_default_action();
    assert_eq!(window.get_delete_filter(), 1);
    accessible(&window, "历史记录").invoke_accessible_default_action();
    assert_eq!(window.get_delete_filter(), 2);
    accessible(&window, "待执行").invoke_accessible_default_action();
    assert_eq!(window.get_delete_filter(), 0);
    accessible(&window, "审核工作台").invoke_accessible_default_action();
    assert_eq!(window.get_review_tab(), 0);

    let confirmed = Rc::new(Cell::new(0));
    window.on_confirm_delete({
        let confirmed = confirmed.clone();
        move || confirmed.set(confirmed.get() + 1)
    });
    window.set_delete_file_count(3);
    window.set_delete_node_count(2);
    window.set_delete_reclaimable("6.0 MiB".into());
    window.set_delete_mode("回收站".into());
    window.set_delete_can_execute(false);
    window.set_delete_dialog_open(true);

    let recycle_card = accessible(&window, "删除确认：回收站");
    assert!(
        recycle_card
            .accessible_description()
            .expect("回收站确认应提供可访问描述")
            .contains("回收站")
    );
    let disabled_confirm = accessible(&window, "确认执行");
    assert_eq!(disabled_confirm.accessible_enabled(), Some(false));
    disabled_confirm.invoke_accessible_default_action();
    click_element_center(&window, &disabled_confirm);
    assert_eq!(
        confirmed.get(),
        0,
        "禁用门禁必须同时拦截可访问默认动作和真实指针路径",
    );

    window.set_delete_can_execute(true);
    let enabled_confirm = accessible(&window, "确认执行");
    assert_eq!(enabled_confirm.accessible_enabled(), Some(true));
    enabled_confirm.invoke_accessible_default_action();
    assert_eq!(confirmed.get(), 1, "启用后可访问默认动作只确认一次");
    click_element_center(&window, &enabled_confirm);
    assert_eq!(confirmed.get(), 2, "启用后真实指针单击只确认一次");

    window.set_delete_mode("永久删除".into());
    let permanent_card = accessible(&window, "删除确认：永久删除");
    assert!(
        permanent_card
            .accessible_description()
            .expect("永久删除确认应提供可访问描述")
            .contains("不可恢复"),
        "永久删除描述必须明确不可恢复",
    );
}

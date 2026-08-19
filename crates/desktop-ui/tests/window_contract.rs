use dedup_desktop_ui::{MainWindow, UiNodeRow, UiTaskRow};
use i_slint_backend_testing::ElementHandle;
use slint::{Color, ModelRc, VecModel};
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

fn accessible(window: &MainWindow, label: &str) -> ElementHandle {
    ElementHandle::find_by_accessible_label(window, label)
        .next()
        .unwrap_or_else(|| panic!("应能找到可访问元素：{label}"))
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
fn overview_and_nodes_consume_real_models() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
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
}

#[test]
fn node_add_forwards_entered_ip_and_port() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_overview_fixture(&window);
    window.invoke_navigate_to(1);
    window.set_new_node_ip("192.168.50.18".into());
    window.set_new_node_port(40123);

    let captured = Rc::new(RefCell::new(None));
    window.on_add_node({
        let captured = captured.clone();
        move |ip, port| *captured.borrow_mut() = Some((ip.to_string(), port))
    });
    accessible(&window, "添加节点").invoke_accessible_default_action();

    assert_eq!(
        captured.borrow().as_ref(),
        Some(&(String::from("192.168.50.18"), 40123)),
        "添加动作应原样转发根表单双向绑定值",
    );
}

#[test]
fn selected_node_actions_forward_existing_callbacks() {
    i_slint_backend_testing::init_no_event_loop();

    let window = MainWindow::new().expect("应能构造真实 MainWindow");
    install_overview_fixture(&window);
    window.invoke_navigate_to(1);
    window.set_new_node_ip("10.20.30.40".into());
    window.set_new_node_port(41000);
    accessible(
        &window,
        "节点项：远程节点；10.0.0.8:39092；离线；Worker 0/4 忙碌；任务 0 排队 / 0 运行；同步 98 / 98",
    )
    .invoke_accessible_default_action();

    let edited = Rc::new(RefCell::new(None));
    window.on_edit_node({
        let edited = edited.clone();
        move |index, ip, port| *edited.borrow_mut() = Some((index, ip.to_string(), port))
    });
    let synced = Rc::new(RefCell::new(None));
    window.on_sync_node({
        let synced = synced.clone();
        move |index| *synced.borrow_mut() = Some(index)
    });
    let removed = Rc::new(RefCell::new(None));
    window.on_remove_node({
        let removed = removed.clone();
        move |index| *removed.borrow_mut() = Some(index)
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
        edited.borrow().as_ref(),
        Some(&(7, String::from("10.20.30.40"), 41000)),
    );
    assert_eq!(*synced.borrow(), Some(7));
    assert_eq!(*removed.borrow(), Some(7));
    assert_eq!(connected.get(), 1);
}

use dedup_desktop_ui::MainWindow;
use i_slint_backend_testing::ElementHandle;
use std::{cell::Cell, rc::Rc};

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

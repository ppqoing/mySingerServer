use dedup_desktop_ui::MainWindow;

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

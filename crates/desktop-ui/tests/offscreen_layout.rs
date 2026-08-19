use dedup_desktop_ui::MainWindow;
use i_slint_backend_testing::{TestingBackend, TestingBackendOptions};
use slint::ComponentHandle;

#[test]
fn light_shell_renders_at_target_size() {
    slint::platform::set_platform(Box::new(TestingBackend::new(TestingBackendOptions {
        mock_time: true,
        threading: false,
        renderer_name: Some("software".into()),
    })))
    .expect("应能安装软件渲染测试后端");

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

    let left_top = snapshot.as_slice()[20 * 1440 + 20];
    let content = snapshot.as_slice()[700 * 1440 + 1000];
    for pixel in [left_top, content] {
        assert!(
            pixel.r >= 235 && pixel.g >= 235 && pixel.b >= 235,
            "概念 UI 外壳应在采样区域呈现浅色，实际 RGBA=({}, {}, {}, {})",
            pixel.r,
            pixel.g,
            pixel.b,
            pixel.a
        );
    }
}

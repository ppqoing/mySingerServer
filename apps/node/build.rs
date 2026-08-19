//! 编译节点 SystemTrayIcon 的 Slint 声明。

fn main() {
    slint_build::compile("ui/tray.slint").expect("编译 node 托盘 Slint 失败");
}

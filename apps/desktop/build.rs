//! desktop.exe 构建脚本只固定入口重建边界，并在 Windows 嵌入应用图标。

fn main() {
    println!("cargo:rerun-if-changed=src/main.rs");
    println!("cargo:rerun-if-changed=../../crates/desktop-ui/ui/assets/icons/app.ico");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows") {
        winresource::WindowsResource::new()
            .set_icon("../../crates/desktop-ui/ui/assets/icons/app.ico")
            .compile()
            .expect("应能把 Image 2 应用图标嵌入 desktop.exe");
    }
}

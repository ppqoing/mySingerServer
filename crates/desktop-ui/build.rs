//! 编译管理端唯一 Slint 入口，并固定使用概念图定义的浅色 Fluent 控件风格。

fn main() {
    let emit_debug_info = std::env::var("PROFILE").as_deref() != Ok("release");
    // 仅在开发与测试构建中保留元素元数据，供行为测试定位可访问树；Release 不携带该信息。
    let configuration = slint_build::CompilerConfiguration::new()
        .with_style("fluent".into())
        .with_debug_info(emit_debug_info);
    slint_build::compile_with_config("ui/app.slint", configuration)
        .expect("编译 desktop Slint 界面失败");
}

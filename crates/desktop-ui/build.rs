//! 编译管理端唯一 Slint 入口及其页面组件。

fn main() {
    let configuration = slint_build::CompilerConfiguration::new().with_style("fluent-dark".into());
    slint_build::compile_with_config("ui/app.slint", configuration)
        .expect("编译 desktop Slint 界面失败");
}

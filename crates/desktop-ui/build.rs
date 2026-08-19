//! 编译管理端唯一 Slint 入口，并固定使用概念图定义的浅色 Fluent 控件风格。

fn main() {
    let configuration = slint_build::CompilerConfiguration::new().with_style("fluent".into());
    slint_build::compile_with_config("ui/app.slint", configuration)
        .expect("编译 desktop Slint 界面失败");
}

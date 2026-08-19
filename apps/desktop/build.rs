//! desktop.exe 不编译业务资源；构建脚本仅固定组合入口的重建边界。

fn main() {
    println!("cargo:rerun-if-changed=src/main.rs");
}

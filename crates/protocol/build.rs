//! 使用仓库固定的 vendored protoc 生成协议类型和 descriptor set。

use std::{env, path::PathBuf};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let manifest_dir = PathBuf::from(env::var("CARGO_MANIFEST_DIR")?);
    let proto_root = manifest_dir.join("../../proto");
    let proto_file = proto_root.join("node.proto");
    let descriptor = PathBuf::from(env::var("OUT_DIR")?).join("node_descriptor.bin");

    let protoc = protoc_bin_vendored::protoc_bin_path()?;
    // 构建脚本进程只为本次 prost 调用设置固定 protoc，不把路径传到运行时程序。
    unsafe { env::set_var("PROTOC", protoc) };

    let mut config = prost_build::Config::new();
    config.btree_map(["."]);
    config.file_descriptor_set_path(descriptor);
    config.compile_protos(&[proto_file], &[proto_root])?;

    println!("cargo:rerun-if-changed=../../proto/node.proto");
    Ok(())
}

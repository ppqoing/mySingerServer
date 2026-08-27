//! `prost-build` 在 Cargo `OUT_DIR` 生成的协议类型和 descriptor set。

/// 当前发布使用的全部 Protobuf 生成类型。
#[allow(missing_docs)]
pub mod proto {
    include!(concat!(env!("OUT_DIR"), "/mysingerserver.v2.rs"));
}

/// 编译期生成的完整文件描述符，用于协议结构测试和诊断。
pub const FILE_DESCRIPTOR_SET: &[u8] =
    include_bytes!(concat!(env!("OUT_DIR"), "/node_descriptor.bin"));

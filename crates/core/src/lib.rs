//! mySingerServer V2 的共享领域内核。
#![warn(missing_docs)]

/// 返回协议、数据库和发布包共同使用的稳定产品代号。
pub const fn product_id() -> &'static str {
    "mysingerserver-rust-v2"
}

#[cfg(test)]
mod tests {
    /// 防止产品代号在协议、数据库和发布脚本之间发生漂移。
    #[test]
    fn product_id_is_stable() {
        assert_eq!(super::product_id(), "mysingerserver-rust-v2");
    }
}

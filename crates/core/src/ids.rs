//! 跨数据库和协议使用的稳定领域键。

use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::{CoreError, NormalizedPath};

/// 由物理机器信息计算的 64 位小写 SHA-256 十六进制标识。
#[derive(Clone, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(transparent)]
pub struct MachineId(String);

impl MachineId {
    /// 解析数据库、配置或协议边界收到的机器 ID。
    pub fn parse(value: &str) -> Result<Self, CoreError> {
        let is_valid = value.len() == 64
            && value
                .bytes()
                .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte));
        if !is_valid {
            return Err(CoreError::InvalidMachineId);
        }
        Ok(Self(value.to_owned()))
    }

    /// 把已计算的 SHA-256 原始字节编码为规范机器 ID。
    pub fn from_sha256(bytes: [u8; 32]) -> Self {
        const HEX: &[u8; 16] = b"0123456789abcdef";
        let mut value = String::with_capacity(64);
        for byte in bytes {
            value.push(HEX[(byte >> 4) as usize] as char);
            value.push(HEX[(byte & 0x0f) as usize] as char);
        }
        Self(value)
    }

    /// 返回适合协议、SQLite 和 PostgreSQL 保存的规范字符串。
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

/// 以 MD5 与文件大小共同标识一份内容。
///
/// 排序顺序固定为先比较 16 字节 MD5，再比较文件大小，供候选、分页和代表文件选择复用。
#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
pub struct ContentKey {
    md5: [u8; 16],
    file_size: u64,
}

macro_rules! uuid_id {
    ($name:ident, $doc:literal) => {
        #[doc = $doc]
        #[derive(
            Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize,
        )]
        #[serde(transparent)]
        pub struct $name(Uuid);

        impl $name {
            /// 创建按时间可排序的 UUID v7 业务标识。
            pub fn new() -> Self {
                Self(Uuid::now_v7())
            }

            /// 返回底层 UUID，供数据库和协议边界编码。
            pub const fn as_uuid(&self) -> &Uuid {
                &self.0
            }

            /// 从数据库或协议已经验证过的 UUID 恢复业务标识。
            pub const fn from_uuid(value: Uuid) -> Self {
                Self(value)
            }
        }

        impl Default for $name {
            fn default() -> Self {
                Self::new()
            }
        }
    };
}

uuid_id!(TaskId, "一个可恢复节点任务的稳定标识。");
uuid_id!(AnalysisRunId, "一次不可变输入分析运行的稳定标识。");
uuid_id!(GroupId, "一个最终重复组的稳定标识。");

impl ContentKey {
    /// 由原始 MD5 字节和文件大小创建内容键。
    pub const fn new(md5: [u8; 16], file_size: u64) -> Self {
        Self { md5, file_size }
    }

    /// 返回固定 16 字节的 MD5。
    pub const fn md5(self) -> [u8; 16] {
        self.md5
    }

    /// 返回扫描时确认的文件大小。
    pub const fn file_size(self) -> u64 {
        self.file_size
    }
}

/// 一台物理机器上的一个规范文件位置。
#[derive(Clone, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
pub struct LocationKey {
    machine_id: MachineId,
    normalized_path: NormalizedPath,
}

impl LocationKey {
    /// 由物理机器身份和已规范化的绝对路径创建位置键。
    pub const fn new(machine_id: MachineId, normalized_path: NormalizedPath) -> Self {
        Self {
            machine_id,
            normalized_path,
        }
    }

    /// 返回拥有该位置的物理机器身份。
    pub const fn machine_id(&self) -> &MachineId {
        &self.machine_id
    }

    /// 返回用于索引和比较的规范路径。
    pub const fn normalized_path(&self) -> &NormalizedPath {
        &self.normalized_path
    }
}

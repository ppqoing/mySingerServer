//! 由物理机器字段计算稳定且不可配置的节点身份。

use dedup_core::{CoreError, MachineId};
use sha2::{Digest, Sha256};

/// SMBIOS 中用于识别物理机器的三个固定字段。
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct PhysicalMachineFields {
    /// SMBIOS Type 1 System UUID 的规范字符串。
    pub system_uuid: Option<String>,
    /// SMBIOS Type 1 System Serial Number。
    pub system_serial: Option<String>,
    /// SMBIOS Type 2 Baseboard Serial Number。
    pub baseboard_serial: Option<String>,
}

/// 按固定字段顺序、大小写和 NUL 分隔规则计算机器 ID。
pub fn machine_id_from_fields(fields: &PhysicalMachineFields) -> Result<MachineId, CoreError> {
    let values = [
        fields.system_uuid.as_deref(),
        fields.system_serial.as_deref(),
        fields.baseboard_serial.as_deref(),
    ]
    .into_iter()
    .flatten()
    .map(str::trim)
    .filter(|value| !value.is_empty())
    .map(str::to_uppercase)
    .collect::<Vec<_>>();

    if values.is_empty() {
        return Err(CoreError::MissingPhysicalIdentity);
    }

    let mut hasher = Sha256::new();
    hasher.update(b"mysingerserver-v2-machine\0");
    for (index, value) in values.iter().enumerate() {
        if index != 0 {
            hasher.update([0]);
        }
        hasher.update(value.as_bytes());
    }
    Ok(MachineId::from_sha256(hasher.finalize().into()))
}

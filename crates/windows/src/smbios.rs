//! 读取并解析 Windows Raw SMBIOS 固件表中的物理机器字段。

use dedup_core::CoreError;
use uuid::Uuid;
use windows::Win32::{
    Foundation::GetLastError,
    System::SystemInformation::{GetSystemFirmwareTable, RSMB},
};

use crate::PhysicalMachineFields;

const RAW_HEADER_LENGTH: usize = 8;

/// 通过 `GetSystemFirmwareTable('RSMB')` 读取当前物理机器字段。
pub fn read_physical_machine_fields() -> Result<PhysicalMachineFields, CoreError> {
    // 第一次调用只查询所需长度；第二次调用写入同样大小的拥有所有权缓冲区。
    let required = unsafe { GetSystemFirmwareTable(RSMB, 0, None) };
    if required == 0 {
        return Err(last_firmware_error());
    }

    let mut raw = vec![0_u8; required as usize];
    let written = unsafe { GetSystemFirmwareTable(RSMB, 0, Some(raw.as_mut_slice())) };
    if written == 0 {
        return Err(last_firmware_error());
    }
    raw.truncate(written as usize);
    parse_raw_smbios(&raw)
}

pub(crate) fn parse_raw_smbios(raw: &[u8]) -> Result<PhysicalMachineFields, CoreError> {
    if raw.len() < RAW_HEADER_LENGTH {
        return Err(CoreError::InvalidSmbios("缺少 RawSMBIOSData 头"));
    }
    let major = raw[1];
    let minor = raw[2];
    let table_length = u32::from_le_bytes(raw[4..8].try_into().unwrap()) as usize;
    let table_end = RAW_HEADER_LENGTH
        .checked_add(table_length)
        .filter(|end| *end <= raw.len())
        .ok_or(CoreError::InvalidSmbios("表长度超出缓冲区"))?;

    let mut fields = PhysicalMachineFields::default();
    let mut offset = RAW_HEADER_LENGTH;
    while offset + 4 <= table_end {
        let structure_type = raw[offset];
        let formatted_length = raw[offset + 1] as usize;
        if formatted_length < 4 || offset + formatted_length > table_end {
            return Err(CoreError::InvalidSmbios("结构长度无效"));
        }
        let formatted = &raw[offset..offset + formatted_length];
        let strings_start = offset + formatted_length;
        let strings_end = find_double_nul(raw, strings_start, table_end)?;
        let strings = parse_strings(&raw[strings_start..strings_end]);

        match structure_type {
            1 if formatted_length >= 24 => {
                fields.system_serial = indexed_string(&strings, formatted[7]);
                fields.system_uuid = format_system_uuid(&formatted[8..24], major, minor);
            }
            2 if formatted_length >= 8 => {
                fields.baseboard_serial = indexed_string(&strings, formatted[7]);
            }
            127 => break,
            _ => {}
        }
        offset = strings_end + 2;
    }
    Ok(fields)
}

fn find_double_nul(raw: &[u8], start: usize, end: usize) -> Result<usize, CoreError> {
    if start + 1 > end {
        return Err(CoreError::InvalidSmbios("缺少字符串区"));
    }
    (start..end.saturating_sub(1))
        .find(|index| raw[*index] == 0 && raw[*index + 1] == 0)
        .ok_or(CoreError::InvalidSmbios("字符串区缺少双 NUL 结尾"))
}

fn parse_strings(raw: &[u8]) -> Vec<String> {
    raw.split(|byte| *byte == 0)
        .map(|bytes| String::from_utf8_lossy(bytes).trim().to_owned())
        .collect()
}

fn indexed_string(strings: &[String], index: u8) -> Option<String> {
    if index == 0 {
        return None;
    }
    strings
        .get(index as usize - 1)
        .filter(|value| !value.is_empty())
        .cloned()
}

fn format_system_uuid(bytes: &[u8], major: u8, minor: u8) -> Option<String> {
    let raw: [u8; 16] = bytes.try_into().ok()?;
    if raw.iter().all(|byte| *byte == 0) || raw.iter().all(|byte| *byte == 0xff) {
        return None;
    }
    let uuid = if (major, minor) >= (2, 6) {
        Uuid::from_bytes_le(raw)
    } else {
        Uuid::from_bytes(raw)
    };
    Some(uuid.to_string())
}

fn last_firmware_error() -> CoreError {
    let code = unsafe { GetLastError() }.0;
    CoreError::FirmwareApi { code }
}

//! Windows x64 应用目录、机器身份、文件枚举、进程和 Shell 边界。
#![warn(missing_docs)]

mod app_layout;
mod job;
mod machine_id;
mod shell;
mod smbios;
mod walker;

pub use app_layout::AppLayout;
pub use job::{CREATE_WORKER_FLAGS, WorkerJob};
pub use machine_id::{PhysicalMachineFields, machine_id_from_fields};
pub use shell::{move_to_recycle_bin, open_folder};
pub use smbios::read_physical_machine_fields;
pub use walker::{WalkedFile, WindowsWalker};

#[cfg(test)]
mod tests {
    use std::path::Path;

    use dedup_core::CoreError;

    use super::{
        AppLayout, PhysicalMachineFields, machine_id_from_fields, read_physical_machine_fields,
    };

    /// 防止运行时目录意外依赖进程当前工作目录。
    #[test]
    fn layout_is_based_on_executable_not_current_directory() {
        let layout = AppLayout::from_executable(Path::new(r"C:\Portable\worker.exe")).unwrap();
        assert_eq!(layout.data_root(), Path::new(r"C:\Portable\data"));
        assert_eq!(
            layout.ffmpeg_root(),
            Path::new(r"C:\Portable\runtime\ffmpeg")
        );
    }

    /// 防止字段顺序、大小写或 NUL 分隔变化导致同一物理机身份漂移。
    #[test]
    fn physical_fields_make_stable_machine_id() {
        let fields = PhysicalMachineFields {
            system_uuid: Some(" 00112233-4455-6677-8899-aabbccddeeff ".into()),
            system_serial: Some("sys-42".into()),
            baseboard_serial: Some("board-9".into()),
        };
        assert_eq!(
            machine_id_from_fields(&fields).unwrap().as_str(),
            "73bdb7a3377f81376a84f316b3ee1555e345afbfa87aa99c77b1bfcc364c4cae"
        );
    }

    /// 防止没有物理字段时生成所有机器共享的空输入身份。
    #[test]
    fn empty_physical_fields_are_rejected() {
        let error = machine_id_from_fields(&PhysicalMachineFields::default()).unwrap_err();
        assert!(matches!(error, CoreError::MissingPhysicalIdentity));
    }

    /// 防止 SMBIOS Type 1/2 的字符串索引和 UUID 字节序被解析错位。
    #[test]
    fn raw_smbios_extracts_identity_fields() {
        let raw = smbios_fixture();
        let fields = super::smbios::parse_raw_smbios(&raw).unwrap();
        assert_eq!(
            fields.system_uuid.as_deref(),
            Some("00112233-4455-6677-8899-aabbccddeeff")
        );
        assert_eq!(fields.system_serial.as_deref(), Some("SYS-42"));
        assert_eq!(fields.baseboard_serial.as_deref(), Some("BOARD-9"));
    }

    /// 验证当前 Windows x64 主机实际允许读取 SMBIOS 并形成非空机器 ID。
    #[test]
    fn current_machine_smbios_produces_machine_id() {
        let fields = read_physical_machine_fields().unwrap();
        let machine_id = machine_id_from_fields(&fields).unwrap();
        assert_eq!(machine_id.as_str().len(), 64);
    }

    fn smbios_fixture() -> Vec<u8> {
        let mut table = vec![1, 27, 0, 1, 1, 2, 3, 4];
        table.extend_from_slice(&[
            0x33, 0x22, 0x11, 0x00, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd,
            0xee, 0xff,
        ]);
        table.extend_from_slice(&[0, 0, 0]);
        table.extend_from_slice(b"Maker\0Product\0V1\0SYS-42\0\0");
        table.extend_from_slice(&[2, 8, 0, 2, 1, 2, 3, 4]);
        table.extend_from_slice(b"Maker\0Board\0V1\0BOARD-9\0\0");
        table.extend_from_slice(&[127, 4, 0xff, 0xff, 0, 0]);

        let mut raw = vec![0, 3, 5, 0];
        raw.extend_from_slice(&(table.len() as u32).to_le_bytes());
        raw.extend_from_slice(&table);
        raw
    }
}

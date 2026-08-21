//! 物理磁盘身份和介质类型解析的可控 Windows 查询行为测试。

use std::{
    collections::{BTreeMap, BTreeSet, HashSet},
    io,
    path::{Path, PathBuf},
};

#[allow(dead_code)]
#[path = "../src/storage_device.rs"]
mod storage_device;

use storage_device::{LocalDiskKind, StorageDeviceQuery, resolve_storage_location_with};

#[derive(Clone, Debug)]
struct FakeVolume {
    drive_type: u32,
    extents: Vec<u32>,
}

#[derive(Default)]
struct FakeQueries {
    volumes: BTreeMap<String, FakeVolume>,
    seek_penalty: BTreeMap<u32, Option<bool>>,
}

impl FakeQueries {
    fn volume(mut self, drive: &str, drive_type: u32, extents: &[u32]) -> Self {
        self.volumes.insert(
            drive.to_ascii_uppercase(),
            FakeVolume {
                drive_type,
                extents: extents.to_vec(),
            },
        );
        self
    }

    fn media(mut self, disk: u32, incurs_seek_penalty: Option<bool>) -> Self {
        self.seek_penalty.insert(disk, incurs_seek_penalty);
        self
    }

    fn drive(path: &Path) -> io::Result<String> {
        let text = path.to_string_lossy();
        text.get(..2)
            .filter(|prefix| prefix.as_bytes().get(1) == Some(&b':'))
            .map(str::to_ascii_uppercase)
            .ok_or_else(|| io::Error::new(io::ErrorKind::Unsupported, "不是盘符路径"))
    }
}

impl StorageDeviceQuery for FakeQueries {
    fn volume_root(&self, path: &Path) -> io::Result<PathBuf> {
        Ok(PathBuf::from(format!(r"{}\", Self::drive(path)?)))
    }

    fn drive_type(&self, volume_root: &Path) -> u32 {
        self.volumes
            .get(&Self::drive(volume_root).unwrap())
            .map_or(0, |volume| volume.drive_type)
    }

    fn disk_extents(&self, volume_root: &Path) -> io::Result<Vec<u32>> {
        self.volumes
            .get(&Self::drive(volume_root)?)
            .map(|volume| volume.extents.clone())
            .ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "fixture volume missing"))
    }

    fn incurs_seek_penalty(&self, disk_number: u32) -> Option<bool> {
        self.seek_penalty.get(&disk_number).copied().flatten()
    }
}

#[test]
fn single_extent_volumes_share_physical_ids_and_map_known_or_unknown_media() {
    let queries = FakeQueries::default()
        .volume("C:", 3, &[7])
        .volume("D:", 3, &[7])
        .volume("E:", 3, &[8])
        .volume("F:", 3, &[9])
        .media(7, Some(true))
        .media(8, Some(false))
        .media(9, None);

    let c = resolve_storage_location_with(Path::new(r"C:\Media\a.bin"), &queries).unwrap();
    let d = resolve_storage_location_with(Path::new(r"D:\Other\b.bin"), &queries).unwrap();
    let e = resolve_storage_location_with(Path::new(r"E:\Media\c.bin"), &queries).unwrap();
    let f = resolve_storage_location_with(Path::new(r"F:\Media\d.bin"), &queries).unwrap();

    assert_eq!(c.physical_disk_id(), d.physical_disk_id());
    assert_ne!(c.physical_disk_id(), e.physical_disk_id());
    assert_eq!(c.physical_disk_id().disk_numbers(), &[7]);
    assert_eq!(c.disk_kind(), LocalDiskKind::Hdd);
    assert_eq!(d.disk_kind(), LocalDiskKind::Hdd);
    assert_eq!(e.disk_kind(), LocalDiskKind::Ssd);
    assert_eq!(f.disk_kind(), LocalDiskKind::Unknown);
}

#[test]
fn multiple_extents_form_a_stable_composite_unknown_location() {
    let queries = FakeQueries::default()
        .volume("G:", 3, &[12, 5, 12])
        .media(5, Some(false))
        .media(12, Some(false));

    let location =
        resolve_storage_location_with(Path::new(r"G:\Striped\movie.mkv"), &queries).unwrap();

    assert_eq!(location.physical_disk_id().disk_numbers(), &[5, 12]);
    assert_eq!(location.disk_kind(), LocalDiskKind::Unknown);
}

#[test]
fn physical_disk_identity_is_independent_from_extent_layout() {
    let queries = FakeQueries::default()
        .volume("H:", 3, &[7])
        .volume("I:", 3, &[7, 7])
        .volume("J:", 3, &[7, 8])
        .media(7, Some(false))
        .media(8, Some(false));

    let single = resolve_storage_location_with(Path::new(r"H:\one.bin"), &queries).unwrap();
    let repeated = resolve_storage_location_with(Path::new(r"I:\two.bin"), &queries).unwrap();
    let mixed = resolve_storage_location_with(Path::new(r"J:\three.bin"), &queries).unwrap();

    assert_eq!(single.physical_disk_id(), repeated.physical_disk_id());
    assert_ne!(single.physical_disk_id(), mixed.physical_disk_id());
    assert_eq!(repeated.disk_kind(), LocalDiskKind::Unknown);
    assert_eq!(mixed.disk_kind(), LocalDiskKind::Unknown);

    let mut hash_ids = HashSet::new();
    hash_ids.insert(single.physical_disk_id().clone());
    hash_ids.insert(repeated.physical_disk_id().clone());
    assert_eq!(hash_ids.len(), 1);

    let mut ordered_ids = BTreeSet::new();
    ordered_ids.insert(single.physical_disk_id().clone());
    ordered_ids.insert(repeated.physical_disk_id().clone());
    assert_eq!(ordered_ids.len(), 1);
}

#[test]
fn unc_remote_relative_and_extentless_paths_are_rejected_without_fallback() {
    let queries = FakeQueries::default()
        .volume("R:", 4, &[20])
        .volume("N:", 3, &[]);

    for path in [
        Path::new(r"\\server\share\movie.mkv"),
        Path::new(r"R:\mapped\movie.mkv"),
        Path::new(r"N:\missing-extents\movie.mkv"),
        Path::new(r"relative\movie.mkv"),
    ] {
        let error = resolve_storage_location_with(path, &queries).unwrap_err();
        assert_eq!(error.kind(), io::ErrorKind::Unsupported, "path={path:?}");
    }
}

#[test]
fn public_api_exposes_only_the_real_system_resolver() {
    let _resolver: fn(&Path) -> io::Result<dedup_windows::StorageLocation> =
        dedup_windows::resolve_storage_location;
}

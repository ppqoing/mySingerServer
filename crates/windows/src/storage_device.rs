//! 本机绝对路径到物理磁盘身份和旋转介质类型的 Windows 查询边界。

use std::{
    ffi::{OsStr, OsString, c_void},
    io,
    mem::{offset_of, size_of},
    os::windows::ffi::{OsStrExt, OsStringExt},
    path::{Path, PathBuf},
    ptr,
};

use windows::{
    Win32::{
        Foundation::{CloseHandle, ERROR_MORE_DATA, HANDLE},
        Storage::FileSystem::{
            CreateFileW, FILE_FLAGS_AND_ATTRIBUTES, FILE_SHARE_DELETE, FILE_SHARE_READ,
            FILE_SHARE_WRITE, GetDriveTypeW, GetVolumeNameForVolumeMountPointW, GetVolumePathNameW,
            IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS, OPEN_EXISTING,
        },
        System::{
            IO::DeviceIoControl,
            Ioctl::{
                DEVICE_SEEK_PENALTY_DESCRIPTOR, DISK_EXTENT, IOCTL_STORAGE_QUERY_PROPERTY,
                PropertyStandardQuery, STORAGE_PROPERTY_QUERY, StorageDeviceSeekPenaltyProperty,
            },
        },
    },
    core::{HRESULT, PCWSTR},
};

const DRIVE_REMOTE: u32 = 4;
const INITIAL_EXTENT_CAPACITY: usize = 8;
const MAX_EXTENT_CAPACITY: usize = 4096;

/// 一个本机物理磁盘或跨多个 extent 的保守复合身份。
#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct PhysicalDiskId {
    disk_numbers: Vec<u32>,
}

impl PhysicalDiskId {
    fn new(mut disk_numbers: Vec<u32>) -> Self {
        disk_numbers.sort_unstable();
        disk_numbers.dedup();
        Self { disk_numbers }
    }

    /// 返回按编号排序去重的底层物理磁盘集合。
    pub fn disk_numbers(&self) -> &[u32] {
        &self.disk_numbers
    }
}

/// Windows 能可靠确认的本机磁盘介质类型。
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum LocalDiskKind {
    /// 设备明确报告有寻道惩罚。
    Hdd,
    /// 设备明确报告没有寻道惩罚。
    Ssd,
    /// 多 extent 或设备属性查询不能可靠判定。
    Unknown,
}

/// 一个解析完成的本机物理存储位置。
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct StorageLocation {
    physical_disk_id: PhysicalDiskId,
    disk_kind: LocalDiskKind,
}

impl StorageLocation {
    /// 返回调度器使用的稳定物理磁盘身份。
    pub const fn physical_disk_id(&self) -> &PhysicalDiskId {
        &self.physical_disk_id
    }

    /// 返回可靠判定的 HDD、SSD 或保守 Unknown。
    pub const fn disk_kind(&self) -> LocalDiskKind {
        self.disk_kind
    }
}

/// 把已解析的本机绝对路径映射到物理磁盘；相对或网络路径直接拒绝。
pub fn resolve_storage_location(path: &Path) -> io::Result<StorageLocation> {
    resolve_storage_location_with(path, &SystemStorageDeviceQuery)
}

pub(super) trait StorageDeviceQuery {
    fn volume_root(&self, path: &Path) -> io::Result<PathBuf>;
    fn drive_type(&self, volume_root: &Path) -> u32;
    fn disk_extents(&self, volume_root: &Path) -> io::Result<Vec<u32>>;
    fn incurs_seek_penalty(&self, disk_number: u32) -> Option<bool>;
}

pub(super) fn resolve_storage_location_with<Q>(
    path: &Path,
    query: &Q,
) -> io::Result<StorageLocation>
where
    Q: StorageDeviceQuery + ?Sized,
{
    if !path.is_absolute() || is_unc_path(path) {
        return Err(unsupported("只支持本机绝对盘符路径"));
    }
    let volume_root = query.volume_root(path)?;
    if query.drive_type(&volume_root) == DRIVE_REMOTE {
        return Err(unsupported("不支持映射网络盘"));
    }
    let extents = query.disk_extents(&volume_root)?;
    if extents.is_empty() {
        return Err(unsupported("路径没有可用的物理磁盘 extent"));
    }
    let composite = extents.len() != 1;
    let physical_disk_id = PhysicalDiskId::new(extents);
    let disk_kind = if composite {
        LocalDiskKind::Unknown
    } else {
        match query.incurs_seek_penalty(physical_disk_id.disk_numbers[0]) {
            Some(true) => LocalDiskKind::Hdd,
            Some(false) => LocalDiskKind::Ssd,
            None => LocalDiskKind::Unknown,
        }
    };
    Ok(StorageLocation {
        physical_disk_id,
        disk_kind,
    })
}

struct SystemStorageDeviceQuery;

impl StorageDeviceQuery for SystemStorageDeviceQuery {
    fn volume_root(&self, path: &Path) -> io::Result<PathBuf> {
        let path = wide(path.as_os_str());
        let mut buffer = vec![0u16; 1024];
        // SAFETY: 输入和输出均为 NUL 结尾或有明确长度的 UTF-16 缓冲区。
        unsafe { GetVolumePathNameW(PCWSTR(path.as_ptr()), &mut buffer) }.map_err(io_error)?;
        Ok(PathBuf::from(from_wide(&buffer)?))
    }

    fn drive_type(&self, volume_root: &Path) -> u32 {
        let root = wide(volume_root.as_os_str());
        // SAFETY: 指针来自本调用期间有效的 NUL 结尾 UTF-16 根路径。
        unsafe { GetDriveTypeW(PCWSTR(root.as_ptr())) }
    }

    fn disk_extents(&self, volume_root: &Path) -> io::Result<Vec<u32>> {
        let handle = open_volume(volume_root)?;
        query_disk_extents(handle.0)
    }

    fn incurs_seek_penalty(&self, disk_number: u32) -> Option<bool> {
        query_seek_penalty(disk_number).ok().flatten()
    }
}

struct OwnedHandle(HANDLE);

impl Drop for OwnedHandle {
    fn drop(&mut self) {
        // SAFETY: OwnedHandle is created only from a successful CreateFileW and drops once.
        let _ = unsafe { CloseHandle(self.0) };
    }
}

fn open_volume(volume_root: &Path) -> io::Result<OwnedHandle> {
    let root = wide(volume_root.as_os_str());
    let mut volume_name = vec![0u16; 128];
    // SAFETY: 根路径和输出缓冲区在调用期间有效。
    unsafe { GetVolumeNameForVolumeMountPointW(PCWSTR(root.as_ptr()), &mut volume_name) }
        .map_err(io_error)?;
    let end = volume_name
        .iter()
        .position(|unit| *unit == 0)
        .ok_or_else(|| invalid_data("卷 GUID 响应没有 NUL 结尾"))?;
    let end = end.saturating_sub(usize::from(end > 0 && volume_name[end - 1] == b'\\' as u16));
    open_device(&OsString::from_wide(&volume_name[..end]))
}

fn open_device(path: &OsStr) -> io::Result<OwnedHandle> {
    let path = wide(path);
    // SAFETY: path 是 NUL 结尾 UTF-16；返回句柄由 OwnedHandle 唯一关闭。
    let handle = unsafe {
        CreateFileW(
            PCWSTR(path.as_ptr()),
            0,
            FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
            None,
            OPEN_EXISTING,
            FILE_FLAGS_AND_ATTRIBUTES(0),
            None,
        )
    }
    .map_err(io_error)?;
    Ok(OwnedHandle(handle))
}

fn query_disk_extents(handle: HANDLE) -> io::Result<Vec<u32>> {
    let header_size = offset_of!(windows::Win32::System::Ioctl::VOLUME_DISK_EXTENTS, Extents);
    let extent_size = size_of::<DISK_EXTENT>();
    let mut capacity = INITIAL_EXTENT_CAPACITY;
    loop {
        let bytes = header_size + capacity * extent_size;
        let mut buffer = vec![0u64; bytes.div_ceil(size_of::<u64>())];
        let mut returned = 0u32;
        // SAFETY: 输出缓冲区按 u64 对齐且大小明确；同步调用不使用 OVERLAPPED。
        let result = unsafe {
            DeviceIoControl(
                handle,
                IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS,
                None,
                0,
                Some(buffer.as_mut_ptr().cast::<c_void>()),
                (buffer.len() * size_of::<u64>()) as u32,
                Some(&mut returned),
                None,
            )
        };
        match result {
            Ok(()) => return parse_disk_extents(&buffer, returned as usize),
            Err(error)
                if error.code() == HRESULT::from_win32(ERROR_MORE_DATA.0)
                    && capacity < MAX_EXTENT_CAPACITY =>
            {
                capacity *= 2;
            }
            Err(error) => return Err(io_error(error)),
        }
    }
}

fn parse_disk_extents(buffer: &[u64], returned: usize) -> io::Result<Vec<u32>> {
    let bytes = buffer.as_ptr().cast::<u8>();
    let header_size = offset_of!(windows::Win32::System::Ioctl::VOLUME_DISK_EXTENTS, Extents);
    if returned < header_size {
        return Err(invalid_data("物理磁盘 extent 响应缺少头部"));
    }
    // SAFETY: 已确认至少包含 u32 计数字段，使用 unaligned 读取避免布局假设。
    let count = unsafe { ptr::read_unaligned(bytes.cast::<u32>()) } as usize;
    let extent_size = size_of::<DISK_EXTENT>();
    let required = header_size
        .checked_add(
            count
                .checked_mul(extent_size)
                .ok_or_else(|| invalid_data("extent 数量溢出"))?,
        )
        .ok_or_else(|| invalid_data("extent 响应大小溢出"))?;
    if required > returned || required > buffer.len() * size_of::<u64>() {
        return Err(invalid_data("物理磁盘 extent 响应被截断"));
    }
    let mut disks = Vec::with_capacity(count);
    for index in 0..count {
        // SAFETY: required 已验证整个 extent 数组位于返回和分配缓冲区内。
        let extent = unsafe {
            ptr::read_unaligned(
                bytes
                    .add(header_size + index * extent_size)
                    .cast::<DISK_EXTENT>(),
            )
        };
        disks.push(extent.DiskNumber);
    }
    Ok(disks)
}

fn query_seek_penalty(disk_number: u32) -> io::Result<Option<bool>> {
    let path = format!(r"\\.\PhysicalDrive{disk_number}");
    let handle = open_device(OsStr::new(&path))?;
    let query = STORAGE_PROPERTY_QUERY {
        PropertyId: StorageDeviceSeekPenaltyProperty,
        QueryType: PropertyStandardQuery,
        AdditionalParameters: [0],
    };
    let mut descriptor = DEVICE_SEEK_PENALTY_DESCRIPTOR::default();
    let mut returned = 0u32;
    // SAFETY: 输入输出均是对应 Win32 结构，长度准确且同步调用不使用 OVERLAPPED。
    unsafe {
        DeviceIoControl(
            handle.0,
            IOCTL_STORAGE_QUERY_PROPERTY,
            Some(ptr::from_ref(&query).cast::<c_void>()),
            size_of::<STORAGE_PROPERTY_QUERY>() as u32,
            Some(ptr::from_mut(&mut descriptor).cast::<c_void>()),
            size_of::<DEVICE_SEEK_PENALTY_DESCRIPTOR>() as u32,
            Some(&mut returned),
            None,
        )
    }
    .map_err(io_error)?;
    let expected = size_of::<DEVICE_SEEK_PENALTY_DESCRIPTOR>() as u32;
    if returned < expected || descriptor.Size < expected || descriptor.Version < expected {
        return Ok(None);
    }
    Ok(Some(descriptor.IncursSeekPenalty))
}

fn wide(value: &OsStr) -> Vec<u16> {
    value.encode_wide().chain([0]).collect()
}

fn from_wide(buffer: &[u16]) -> io::Result<OsString> {
    let end = buffer
        .iter()
        .position(|unit| *unit == 0)
        .ok_or_else(|| invalid_data("Windows 路径响应没有 NUL 结尾"))?;
    Ok(OsString::from_wide(&buffer[..end]))
}

fn is_unc_path(path: &Path) -> bool {
    let text = path.as_os_str().to_string_lossy();
    let uppercase = text.to_ascii_uppercase();
    (text.starts_with(r"\\") && !text.starts_with(r"\\?\")) || uppercase.starts_with(r"\\?\UNC\")
}

fn unsupported(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::Unsupported, message)
}

fn invalid_data(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}

fn io_error(error: windows::core::Error) -> io::Error {
    io::Error::other(error.to_string())
}

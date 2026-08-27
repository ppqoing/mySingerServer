//! 进程内显式登记可再生产物，并用租约保护正在使用的文件。

use std::{
    collections::BTreeMap,
    fs, io,
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};

/// 清理器唯一允许处理的可再生产物类型。
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ArtifactKind {
    /// 视频 MD5 联系表。
    ContactSheet,
    /// 可重新获取或生成的预览缓存。
    Preview,
    /// 专用缓存目录中的未完成临时文件。
    OrphanTemporary,
    /// 其他经调用方明确确认可再生的派生产物。
    RegisteredDerivation,
}

#[derive(Clone, Debug)]
struct ArtifactState {
    kind: ArtifactKind,
    contact_sheet_reference: Option<String>,
    active_leases: usize,
    cleaning: bool,
}

#[derive(Debug)]
struct RegistryInner {
    install_root: PathBuf,
    cache_root: PathBuf,
    entries: Mutex<BTreeMap<PathBuf, ArtifactState>>,
}

/// 只保存当前进程显式登记、且位于安装根内的可再生产物集合。
#[derive(Clone, Debug)]
pub struct RegenerableArtifactRegistry {
    inner: Arc<RegistryInner>,
}

impl RegenerableArtifactRegistry {
    /// 从已经存在的绝对安装根和其中唯一允许清理的 cache 根创建空 registry。
    pub fn new(install_root: &Path, cache_root: &Path) -> io::Result<Self> {
        if !install_root.is_absolute() || !cache_root.is_absolute() {
            return Err(invalid_input("安装根和 cache 根必须是绝对路径"));
        }
        let install_root = fs::canonicalize(install_root)?;
        let cache_root = fs::canonicalize(cache_root)?;
        if cache_root == install_root || !cache_root.starts_with(&install_root) {
            return Err(invalid_input("cache 根必须位于安装根内部且不能等于安装根"));
        }
        Ok(Self {
            inner: Arc::new(RegistryInner {
                install_root,
                cache_root,
                entries: Mutex::new(BTreeMap::new()),
            }),
        })
    }

    /// 显式登记一个文件或父目录已存在的计划文件；不会遍历安装根或推断其他文件。
    pub fn register(&self, path: &Path, kind: ArtifactKind) -> io::Result<()> {
        let path = self.normalize_path_even_if_missing(path)?;
        if path.exists() && !path.is_file() {
            return Err(invalid_input("registry 只接受文件"));
        }
        validate_kind_path(&self.inner.cache_root, &path, kind)?;
        let reference = match kind {
            ArtifactKind::ContactSheet => Some(contact_sheet_reference(&path)?),
            _ => None,
        };
        let mut entries = self.entries()?;
        match entries.get(&path) {
            Some(existing) if existing.kind != kind => {
                return Err(invalid_input("同一路径不能登记为不同产物类型"));
            }
            Some(_) => return Ok(()),
            None => {}
        }
        entries.insert(
            path,
            ArtifactState {
                kind,
                contact_sheet_reference: reference,
                active_leases: 0,
                cleaning: false,
            },
        );
        Ok(())
    }

    /// 原子登记一个计划文件并在首次 write 前取得活动租约。
    pub fn lease_planned(&self, path: &Path, kind: ArtifactKind) -> io::Result<ArtifactLease> {
        let path = self.normalize_path_even_if_missing(path)?;
        if path.exists() && !path.is_file() {
            return Err(invalid_input("registry 只接受文件"));
        }
        validate_kind_path(&self.inner.cache_root, &path, kind)?;
        let reference = match kind {
            ArtifactKind::ContactSheet => Some(contact_sheet_reference(&path)?),
            _ => None,
        };
        let mut entries = self.entries()?;
        let state = match entries.entry(path.clone()) {
            std::collections::btree_map::Entry::Vacant(entry) => entry.insert(ArtifactState {
                kind,
                contact_sheet_reference: reference,
                active_leases: 0,
                cleaning: false,
            }),
            std::collections::btree_map::Entry::Occupied(entry) => {
                if entry.get().kind != kind {
                    return Err(invalid_input("同一路径不能登记为不同产物类型"));
                }
                entry.into_mut()
            }
        };
        if state.cleaning {
            return Err(io::Error::new(
                io::ErrorKind::WouldBlock,
                "产物已冻结等待清理",
            ));
        }
        state.active_leases = state
            .active_leases
            .checked_add(1)
            .ok_or_else(|| io::Error::other("产物租约计数溢出"))?;
        Ok(ArtifactLease {
            inner: Arc::clone(&self.inner),
            path: Some(path),
        })
    }

    /// 为已经登记且尚未冻结清理的产物取得活动租约。
    pub fn lease(&self, path: &Path) -> io::Result<ArtifactLease> {
        let path = self.normalize_registered_path(path)?;
        let mut entries = self.entries()?;
        let state = entries
            .get_mut(&path)
            .ok_or_else(|| invalid_input("产物尚未登记"))?;
        if state.cleaning {
            return Err(io::Error::new(
                io::ErrorKind::WouldBlock,
                "产物已冻结等待清理",
            ));
        }
        state.active_leases = state
            .active_leases
            .checked_add(1)
            .ok_or_else(|| io::Error::other("产物租约计数溢出"))?;
        Ok(ArtifactLease {
            inner: Arc::clone(&self.inner),
            path: Some(path),
        })
    }

    /// 移除已经不再存在且没有活动租约的登记。
    pub fn unregister(&self, path: &Path) -> io::Result<()> {
        let path = self.normalize_path_even_if_missing(path)?;
        let mut entries = self.entries()?;
        if let Some(state) = entries.get(&path)
            && (state.active_leases != 0 || state.cleaning)
        {
            return Err(io::Error::new(
                io::ErrorKind::WouldBlock,
                "活动或清理中的产物不能取消登记",
            ));
        }
        entries.remove(&path);
        Ok(())
    }

    pub(crate) fn freeze_inactive(&self) -> io::Result<FrozenArtifacts> {
        let mut entries = self.entries()?;
        let mut claims = Vec::new();
        let mut skipped_active = 0;
        for (path, state) in entries.iter_mut() {
            if state.active_leases != 0 || state.cleaning {
                skipped_active += usize::from(state.active_leases != 0);
                continue;
            }
            state.cleaning = true;
            claims.push(ArtifactCleanupClaim {
                inner: Arc::clone(&self.inner),
                path: path.clone(),
                kind: state.kind,
                contact_sheet_reference: state.contact_sheet_reference.clone(),
                deleted: false,
            });
        }
        Ok(FrozenArtifacts {
            claims,
            skipped_active,
        })
    }

    fn normalize_registered_path(&self, path: &Path) -> io::Result<PathBuf> {
        if !path.is_absolute() {
            return Err(invalid_input("产物路径必须是绝对路径"));
        }
        let path = fs::canonicalize(path)?;
        if !path.starts_with(&self.inner.install_root) || path == self.inner.install_root {
            return Err(invalid_input("产物路径必须位于安装根内"));
        }
        if !path.is_file() {
            return Err(invalid_input("registry 只接受文件"));
        }
        Ok(path)
    }

    fn normalize_path_even_if_missing(&self, path: &Path) -> io::Result<PathBuf> {
        if path.exists() {
            return self.normalize_registered_path(path);
        }
        if !path.is_absolute() {
            return Err(invalid_input("产物路径必须是绝对路径"));
        }
        let parent = path
            .parent()
            .ok_or_else(|| invalid_input("产物路径缺少父目录"))?;
        let parent = fs::canonicalize(parent)?;
        let file_name = path
            .file_name()
            .ok_or_else(|| invalid_input("产物路径缺少文件名"))?;
        let normalized = parent.join(file_name);
        if !normalized.starts_with(&self.inner.install_root) {
            return Err(invalid_input("产物路径必须位于安装根内"));
        }
        Ok(normalized)
    }

    fn entries(&self) -> io::Result<std::sync::MutexGuard<'_, BTreeMap<PathBuf, ArtifactState>>> {
        self.inner
            .entries
            .lock()
            .map_err(|_| io::Error::other("产物 registry 锁已损坏"))
    }
}

/// 活动期间阻止磁盘满清理删除对应产物。
#[derive(Debug)]
pub struct ArtifactLease {
    inner: Arc<RegistryInner>,
    path: Option<PathBuf>,
}

impl Drop for ArtifactLease {
    fn drop(&mut self) {
        let Some(path) = self.path.take() else {
            return;
        };
        if let Ok(mut entries) = self.inner.entries.lock()
            && let Some(state) = entries.get_mut(&path)
        {
            state.active_leases = state.active_leases.saturating_sub(1);
        }
    }
}

pub(crate) struct FrozenArtifacts {
    pub(crate) claims: Vec<ArtifactCleanupClaim>,
    pub(crate) skipped_active: usize,
}

pub(crate) struct ArtifactCleanupClaim {
    inner: Arc<RegistryInner>,
    path: PathBuf,
    kind: ArtifactKind,
    contact_sheet_reference: Option<String>,
    deleted: bool,
}

impl ArtifactCleanupClaim {
    pub(crate) fn path(&self) -> &Path {
        &self.path
    }

    pub(crate) const fn kind(&self) -> ArtifactKind {
        self.kind
    }

    pub(crate) fn contact_sheet_reference(&self) -> Option<&str> {
        self.contact_sheet_reference.as_deref()
    }

    pub(crate) fn mark_deleted(mut self) {
        if let Ok(mut entries) = self.inner.entries.lock() {
            entries.remove(&self.path);
        }
        self.deleted = true;
    }
}

impl Drop for ArtifactCleanupClaim {
    fn drop(&mut self) {
        if self.deleted {
            return;
        }
        if let Ok(mut entries) = self.inner.entries.lock()
            && let Some(state) = entries.get_mut(&self.path)
        {
            state.cleaning = false;
        }
    }
}

fn contact_sheet_reference(path: &Path) -> io::Result<String> {
    let components = path.components().collect::<Vec<_>>();
    let start = components
        .iter()
        .position(|component| {
            component
                .as_os_str()
                .to_string_lossy()
                .eq_ignore_ascii_case("contact-sheets")
        })
        .ok_or_else(|| invalid_input("联系表必须位于 contact-sheets 目录"))?;
    Ok(components[start..]
        .iter()
        .map(|component| component.as_os_str().to_string_lossy())
        .collect::<Vec<_>>()
        .join("/"))
}

fn validate_kind_path(cache_root: &Path, path: &Path, kind: ArtifactKind) -> io::Result<()> {
    let relative = path
        .strip_prefix(cache_root)
        .map_err(|_| invalid_input("产物路径必须位于配置的 exact cache 根内"))?;
    let components = relative
        .components()
        .map(|component| component.as_os_str().to_string_lossy().to_ascii_lowercase())
        .collect::<Vec<_>>();
    if components.iter().any(|component| {
        matches!(
            component.as_str(),
            "src" | "target" | "dist" | "media" | "logs" | ".git"
        )
    }) {
        return Err(invalid_input("源码、构建、日志和扫描媒体目录禁止登记"));
    }
    let file_name = path
        .file_name()
        .and_then(|name| name.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase();
    let extension = path
        .extension()
        .and_then(|extension| extension.to_str())
        .unwrap_or_default()
        .to_ascii_lowercase();
    let database_sidecar = file_name.contains(".db-")
        || file_name.contains(".sqlite-")
        || file_name.contains(".sqlite3-");
    if database_sidecar
        || matches!(
            extension.as_str(),
            "db" | "sqlite" | "sqlite3" | "toml" | "log" | "exe" | "zip"
        )
    {
        return Err(invalid_input("数据库、配置、日志、程序和压缩包禁止登记"));
    }
    let accepted = match kind {
        ArtifactKind::ContactSheet => components
            .first()
            .is_some_and(|value| value == "contact-sheets"),
        ArtifactKind::Preview => components.first().is_some_and(|value| value == "previews"),
        ArtifactKind::OrphanTemporary => extension == "partial",
        ArtifactKind::RegisteredDerivation => {
            components.first().is_some_and(|value| value == "derived")
        }
    };
    if !accepted {
        return Err(invalid_input("产物类型与固定缓存目录不匹配"));
    }
    Ok(())
}

fn invalid_input(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message)
}

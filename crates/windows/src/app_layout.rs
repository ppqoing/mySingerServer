//! 从可执行文件绝对路径推导全部可写数据与运行库目录。

use std::path::{Path, PathBuf};

use dedup_core::CoreError;

/// 一个便携部署中与当前工作目录无关的固定目录布局。
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AppLayout {
    executable_dir: PathBuf,
    data_root: PathBuf,
    desktop_root: PathBuf,
    node_root: PathBuf,
    ffmpeg_root: PathBuf,
}

impl AppLayout {
    /// 从进程可执行文件的绝对路径创建布局。
    ///
    /// 相对路径和没有父目录的路径会被拒绝，避免数据静默写入当前工作目录。
    pub fn from_executable(executable: &Path) -> Result<Self, CoreError> {
        if !executable.is_absolute() || executable.file_name().is_none() {
            return Err(CoreError::InvalidExecutablePath(
                executable.display().to_string(),
            ));
        }
        let executable_dir = executable
            .parent()
            .ok_or_else(|| CoreError::InvalidExecutablePath(executable.display().to_string()))?;
        let data_root = executable_dir.join("data");
        Ok(Self {
            executable_dir: executable_dir.to_path_buf(),
            desktop_root: data_root.join("desktop"),
            node_root: data_root.join("node"),
            ffmpeg_root: executable_dir.join("runtime").join("ffmpeg"),
            data_root,
        })
    }

    /// 返回三个程序及运行库所在的部署根目录。
    pub fn executable_dir(&self) -> &Path {
        &self.executable_dir
    }

    /// 返回便携部署唯一的数据根目录。
    pub fn data_root(&self) -> &Path {
        &self.data_root
    }

    /// 返回管理工具配置、缓存和日志的根目录。
    pub fn desktop_root(&self) -> &Path {
        &self.desktop_root
    }

    /// 返回节点 SQLite、配置、缓存和日志的根目录。
    pub fn node_root(&self) -> &Path {
        &self.node_root
    }

    /// 返回 Worker 唯一允许搜索的 FFmpeg DLL 目录。
    pub fn ffmpeg_root(&self) -> &Path {
        &self.ffmpeg_root
    }

    /// 返回桌面端 TOML 配置文件路径。
    pub fn desktop_config(&self) -> PathBuf {
        self.desktop_root.join("config.toml")
    }

    /// 返回节点 TOML 配置文件路径。
    pub fn node_config(&self) -> PathBuf {
        self.node_root.join("config.toml")
    }

    /// 返回固定放在 `node.exe` 目录中的 Node 配置引导文件路径。
    pub fn node_bootstrap(&self) -> PathBuf {
        self.executable_dir.join("bootstrap.toml")
    }

    /// 返回节点当前 V2 SQLite 文件路径。
    pub fn node_database(&self) -> PathBuf {
        self.node_root.join("node.db")
    }

    /// 返回桌面端滚动日志目录。
    pub fn desktop_logs(&self) -> PathBuf {
        self.desktop_root.join("logs")
    }

    /// 返回节点和 Worker 滚动日志目录。
    pub fn node_logs(&self) -> PathBuf {
        self.node_root.join("logs")
    }

    /// 返回桌面端缓存目录。
    pub fn desktop_cache(&self) -> PathBuf {
        self.desktop_root.join("cache")
    }

    /// 返回节点视频联系表等缓存目录。
    pub fn node_cache(&self) -> PathBuf {
        self.node_root.join("cache")
    }
}

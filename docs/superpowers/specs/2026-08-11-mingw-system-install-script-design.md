# MinGW 系统级安装脚本设计

## 目标

提供一个由用户以管理员身份执行的 PowerShell 脚本，在 Windows x64 系统中安装项目构建所需的 WinLibs MinGW-w64，并配置系统级环境变量。脚本只负责构建工具链，不修改应用程序发布包，也不制作安装包。

## 安装来源与目录

- 发行包：WinLibs x86_64 POSIX/UCRT，GCC 16.1.0、MinGW-w64 14.0.0，release 4。
- 下载地址：`https://github.com/brechtsanders/winlibs_mingw/releases/download/16.1.0posix-14.0.0-ucrt-r4/winlibs-x86_64-posix-seh-gcc-16.1.0-mingw-w64ucrt-14.0.0-r4.zip`。
- 预期 SHA-256：`c406a22f8cac82559a3a1d96b62ff603f666499fb5ff4784e87b4eb6fa37dede`。
- 固定安装目录：`C:\Tools\WinLibs\mingw64`。

选择 C 盘是因为先前 WinGet 已成功下载、校验并解压发行包，但复制到 D 盘时空间不足。无空格的固定工具目录也便于 Go CGO 和现有 PowerShell 构建脚本调用。

## 脚本流程

1. 检查操作系统架构为 x64，并检查当前 PowerShell 具有管理员权限。
2. 检查 C 盘安装和临时文件所需的可用空间；不足时在下载前停止。
3. 如果目标目录已经包含完整的 `gcc.exe`、`g++.exe`、`windres.exe` 和 `dlltool.exe`，复用现有安装并跳过下载。
4. 如果目标目录存在但工具不完整，停止并报告冲突，不自动覆盖或删除已有文件。
5. 下载固定版本 ZIP 到临时目录，计算 SHA-256；不匹配时停止，不安装文件。
6. 解压到临时目录，验证所需工具，然后移动到最终安装目录。
7. 配置系统级环境变量，并同步当前 PowerShell 进程环境。
8. 执行版本检查和最小 C 程序编译、运行验证。
9. 无论成功或失败，清理脚本拥有的临时文件。

## 环境变量

脚本配置以下 Machine 范围变量：

- `Path`：追加 `C:\Tools\WinLibs\mingw64\bin`，已存在时不重复添加。
- `CC`：`C:\Tools\WinLibs\mingw64\bin\gcc.exe`。
- `CXX`：`C:\Tools\WinLibs\mingw64\bin\g++.exe`。
- `M5_CC`：`C:\Tools\WinLibs\mingw64\bin\gcc.exe`。
- `M5_WINDRES`：`C:\Tools\WinLibs\mingw64\bin\windres.exe`。
- `M5_DLLTOOL`：`C:\Tools\WinLibs\mingw64\bin\dlltool.exe`。

脚本仅追加自身的 `bin` 目录，不删除或重排已有系统 `Path` 项。环境变量写入完成后通过 `WM_SETTINGCHANGE` 广播环境变化，使之后启动的应用程序获得新变量。

## 错误处理与安全边界

- 非管理员、非 x64、磁盘空间不足、下载失败、哈希不符、解压失败或工具验证失败均返回非零退出码。
- 不关闭正在运行的程序，不修改用户级环境变量，不删除非脚本创建的目录。
- 现有目标目录不完整时要求用户手工处理，避免覆盖未知文件。
- 安装完成前不写入环境变量，防止环境变量指向半成品目录。

## 验证标准

脚本成功必须同时满足：

- 六个系统级环境变量值正确，且系统 `Path` 中安装目录只出现一次。
- `gcc.exe`、`g++.exe`、`windres.exe` 和 `dlltool.exe` 均存在。
- 三个项目必需工具的版本命令返回成功。
- 使用 `gcc.exe` 编译的最小 C 程序能够运行并返回退出码 0。


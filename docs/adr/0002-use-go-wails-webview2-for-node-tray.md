# 节点托盘程序采用 Go + Wails/WebView2

Status: accepted

节点托盘程序采用 Go 承载配置校验、进程监督和 Windows 集成，使用 Wails/WebView2 承载已确认的页签式轻量界面。该选择保留现有 Go + C++ 工程边界，并比纯 Win32 控件更容易长期维持统一交互样式；部署包必须检测 WebView2 Runtime，缺失时只能使用随包提供的微软官方引导程序处理。纯 Win32 控件因复杂表单维护成本较高而未采用，WPF/WinForms 因会重新引入工程已排除的 .NET 技术栈而未采用。

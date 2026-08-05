# WebView2 Bootstrapper 来源与许可指针

本文件只是来源、完整性与官方分发文档的指针，**不是 Microsoft 许可证全文，也不替代适用条款**。

- 官方 Evergreen Bootstrapper 地址：<https://go.microsoft.com/fwlink/p/?LinkId=2124703>
- 官方分发说明：<https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/distribution>
- 官方下载页：<https://developer.microsoft.com/en-us/microsoft-edge/webview2/>
- 当前缓存的实际来源：固定依赖 `github.com/wailsapp/wails/v2@v2.12.0` 中的嵌入资源 `internal/webview2runtime/MicrosoftEdgeWebview2Setup.exe`。
- Wails module sum：`h1:BHO/kLNWFHYjCzucxbzAYZWUjub1Tvb4cSguQozHn5c=`。

发布脚本只接受 `manifest.json` 中固定的文件大小、SHA-256 和有效 Microsoft Authenticode 签名。更新 Bootstrapper 时，必须从 Microsoft 官方来源或另一个可复验的固定上游依赖重新取得文件，同时更新实际来源、获取时间、哈希、大小和签名信息。不得把本 NOTICE 冒充许可证，也不得从非官方镜像替换缓存。

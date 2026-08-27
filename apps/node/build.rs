//! 编译节点 SystemTrayIcon，并在 Windows 嵌入管理员启动清单。

const NODE_MANIFEST: &str = r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="requireAdministrator" uiAccess="false" />
      </requestedPrivileges>
    </security>
  </trustInfo>
</assembly>"#;

fn main() {
    slint_build::compile("ui/tray.slint").expect("编译 node 托盘 Slint 失败");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows")
        && std::env::var("PROFILE").as_deref() == Ok("release")
    {
        winresource::WindowsResource::new()
            .set_manifest(NODE_MANIFEST)
            .compile()
            .expect("应能把 requireAdministrator 清单嵌入 node.exe");
    }
}

# Rust V2 媒体扩展名过滤设计

## 1. 目标

扫描只收录配置中声明的图片和视频扩展名，避免非媒体文件进入任务项、SQLite/PostgreSQL 缓存查询和 Worker 计算。实现保持简单：Everything 在查询表达式中筛选，Windows Walker 在遍历文件时筛选，不增加内容预探测、数据库结构或新的任务状态。

## 2. 范围

本次只实现以下内容：

- Node 配置增加图片扩展名和视频扩展名；
- 提供本解决方案当前声明支持的完整图片、视频扩展名默认值；
- Desktop 的远程 Node 配置表单可以编辑并恢复默认值；
- Everything 查询增加扩展名条件；
- Windows Walker 跳过扩展名不匹配的文件；
- 增加必要的配置、协议、枚举和 UI 行为测试。

明确不实现：

- 不动态读取 FFmpeg demuxer 或 decoder 列表；
- 不读取文件头，不在枚举阶段调用 FFmpeg；
- 不按扩展名改变现有 Worker 媒体探测和解码流程；
- 不修改 SQLite/PostgreSQL schema；
- 不增加任务状态、扫描模式、排除目录或其他扫描设置；
- 不重构设置页，不实现标签编辑器或复杂格式管理界面；
- 不做旧 Go/C++ 配置兼容。

## 3. 默认扩展名

扩展名在配置中统一使用不带前导点的小写 ASCII 字符串。

默认图片扩展名：

```text
apng, avif, bmp, cur, dds, dib, dpx, exr, fits, gif, hdr, heic, heif,
ico, j2c, j2k, jfif, jls, jp2, jpc, jpe, jpeg, jpg, jxl, pam, pbm,
pcd, pcx, pfm, pgm, pgx, png, pnm, ppm, psd, qoi, ras, sgi, svg,
tga, tif, tiff, webp, xbm, xpm, xwd
```

默认视频扩展名：

```text
3g2, 3gp, 264, 265, 266, amv, apv, asf, av1, avc, avi, bik, bink,
cdxl, dav, dif, divx, dv, evo, evc, f4v, flm, flv, gxf, h261, h263,
h264, h265, h266, hevc, ifv, ismv, ivf, kux, lvf, m1v, m2t, m2ts,
m2v, m4v, mj2, mjpeg, mjpg, mk3d, mkv, moflex, mov, mp4, mpe, mpeg,
mpg, mts, mxf, nsv, nut, nuv, obu, ogm, ogv, pdv, qt, r3d, rm, rmvb,
roq, rpl, ser, smjpeg, smk, str, swf, ts, ty, usm, vc1, viv, vivo,
vob, vvc, webm, wmv, wtv, xmv, y4m, yop
```

这两个列表是产品的扩展名匹配边界，不是运行时 FFmpeg 能力发现结果。以后升级固定 FFmpeg 依赖时，可在同一常量和测试中调整。

## 4. Node 配置与协议

`NodeConfig` 直接增加两个字段，避免引入额外配置层级：

```toml
image_extensions = ["jpg", "jpeg", "png"]
video_extensions = ["mp4", "mkv", "webm"]
```

行为固定为：

- 旧的 Rust V2 TOML 缺少字段时，通过 `serde(default)` 使用完整默认列表；
- 配置加载和 Desktop 保存边界去除前导点、转小写、排序并去重；
- 单个扩展名只能包含 ASCII 字母、数字、`_`、`+`、`-`；
- 任一列表允许为空，空列表表示禁用该类别；
- 两组出现相同扩展名时由最终匹配集合自然合并，不增加额外冲突规则。

`NodeConfigValue` 追加两个 `repeated string` 字段，现有加载、保存并重启、同机器重连验证流程不变。协议版本不因两个追加字段单独升级。

## 5. 扩展名匹配

Node 启动时从两个配置数组建立一个大小写无关的扩展名集合。匹配只读取 `Path::extension()`：

- 无扩展名直接不匹配；
- 文件名大小写不影响结果；
- 图片和视频列表取并集；
- 不读取文件内容；
- 扩展名匹配只决定文件是否进入扫描，不替代后续 Worker 解码。

## 6. Everything 枚举

每个扫描根的 Everything 查询在现有 `file:` 和 `path:` 条件后追加一个 `ext:` 条件：

```text
file: path:"D:\Media" ext:jpg;jpeg;png;mp4;mkv
```

扩展名来自已规范化、稳定排序的图片和视频并集。两组均为空时直接返回空结果，不向 Everything 发起无扩展名限制的查询。Everything 查询失败后的整次 Windows Walker 回退继续使用同一扩展名集合。

## 7. Windows Walker 枚举

Windows Walker 的 Node 枚举适配层在收到文件项时先检查扩展名。不匹配时立即返回并继续遍历，不创建 `NormalizedPath`、`DisplayPath` 或 `ScannedPath`，也不把该项加入排序和去重清单。

Everything 和 Windows Walker 最终只返回匹配项，因此任务总文件数、总字节数、SQLite 任务项、缓存查询和 Worker 调度自然只包含匹配媒体。

## 8. Desktop 配置页面

在现有“设置 → 节点服务”远程 Node 配置表单中增加一个“扫描文件类型”小节：

- “图片扩展名”单行输入框；
- “视频扩展名”单行输入框；
- 输入使用英文逗号分隔；
- 一个“恢复默认格式”按钮同时恢复两组默认值；
- 清空某个输入框表示禁用该类别；
- 编辑后复用现有 dirty 状态和“保存并重启”按钮；
- 加载配置、保存失败、重启和重连验证继续复用现有流程。

不增加弹窗、标签控件、格式搜索或独立保存动作。

## 9. 错误与日志

- 配置扩展名非法时，在 Desktop 本地校验或 Node 保存校验处返回具体字段错误；
- Everything 查询失败仍使用现有告警日志并回退 Windows Walker；
- 扩展名匹配但文件内容损坏或格式不符时，继续使用现有 Worker 文件失败和异常日志；
- 被扩展名正常排除的文件不是错误，不逐文件写日志。

## 10. 验证

只增加与本需求直接相关的行为测试：

1. `NodeConfig` 默认值、TOML 缺省、规范化、空列表和非法扩展名；
2. Protobuf 两个数组字段完整往返；
3. Everything 查询字符串包含稳定的 `ext:` 条件，空集合不查询；
4. Windows Walker 在混合目录中只返回匹配项，覆盖大小写和无扩展名文件；
5. Everything 失败回退 Walker 时仍使用同一过滤集合；
6. Desktop 加载、编辑、恢复默认并保存两个字段；
7. 扫描集成测试确认不匹配文件不会形成任务项或缓存查询。

本次不新增性能框架或额外端到端环境。

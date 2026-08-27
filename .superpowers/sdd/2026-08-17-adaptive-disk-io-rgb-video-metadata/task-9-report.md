# Task 9 实施报告：RGB 视频联系表

## 实施结果

- 保持现有 `FrameToGray` 调用顺序及灰度特征链不变；PDQ、pHash、Sobel 仍只读取 `feature_canvas`。
- 同一解码 `AVFrame` 直接缩放为受 `contact_sheet_tile_max_side` 限制的 RGB24 tile，不保留全尺寸 RGB 帧；六帧 RGB tile 纳入 256 MiB 工作集硬预算。
- 颜色元数据按 frame 优先、stream 回退解析 range、colorspace、transfer 与 primaries；保留旋转及 SAR 显示尺寸语义，并在转换前后检查取消和 deadline。
- 联系表使用双画布：`feature_canvas` 仅供灰度特征计算，`rgb_canvas` 仅供用户 JPEG 输出；缺帧使用中性深色占位。
- TurboJPEG 固定使用 `TJPF_RGB`、`TJSAMP_420`、quality 90、`TJFLAG_NOREALLOC`，并使用预分配编码缓冲。

## TDD 证据

1. 新增 RGB 图像类型测试后，首次编译以缺少 `native_algorithms/rgb_image.h` 的 C1083 失败，构成有效 RED。
2. 新增真实 AVFrame RGB tile 测试后，首次链接以缺少 `VideoAnalysisTestFrameToRgbTile` 的 LNK2019 失败，构成有效 RED。
3. 最小实现后，4K/8K 有界 tile、六帧内存预算、frame/stream 颜色元数据优先级、旋转/SAR、转换前后取消/deadline 均通过。
4. H264 与 HEVC 实际视频生成的 JPEG 均通过 RGB420、SOF 三分量和非 `R=G=B` 真实色差检查；红、绿、蓝及肤色内存顺序测试通过。

## 验证结果

- focused：`videocore_video_analysis`、`videocore_contact_sheet`、`videocore_video_legacy_golden`，3/3 通过。
- 灰度联系表 SHA-256 保持 `58ed90699d51e6213fd40dad6610d0df387af242fc8bb7c378c8c54120ca0742`，未更新旧 golden。
- 标准门禁：`scripts/build.ps1 -VideoCoreOnly`，20/20 测试通过；14/14 精确导出通过；递归原生 DLL 闭包通过。
- 构建缓存：`C:\tmp\mysingerserver-task9-standard-build`。
- 新鲜 stage：`C:\tmp\mysingerserver-task9-stage`。
- 标准构建产生的非白名单 `internal/wproc/videocore/libvideocore.a` 已恢复到 BASE；工作树内原 `videocore/build` 已恢复。

## 修复轮次 1：256 MiB 峰值预算

- 复审确认原预算遗漏六张保留灰度 tile，以及旋转转换期间同时存在的 `pre_rotated` 与 `rotated` RGB tile。
- 新增手算确定性边界测试：方形 tile 最大边 1472 时峰值低于 256 MiB 并接受，1473 时超过 256 MiB 并拒绝；旧实现对 1473 错误接受，取得有效 RED。
- `SafeCanvasSize` 改用溢出安全乘法，统一计入六张灰度 tile、六张 RGB tile、单帧双 RGB 转换峰值、灰度/RGB 双 canvas、PDQ 临时区、JPEG 预分配缓冲及安全余量。
- GREEN 后 focused 三项 3/3 通过，灰度联系表 SHA-256 与 legacy golden 保持不变。
- 新鲜标准门禁 20/20 通过，14/14 精确导出及递归原生 DLL 闭包通过；缓存位于 `C:\tmp\mysingerserver-task9-repair1-build`，stage 位于 `C:\tmp\mysingerserver-task9-repair1-stage`。

# Meta PDQ 上游固定来源

Rust V2 的 PDQ 算法按以下唯一上游快照做独立等价移植：

- 仓库：`https://github.com/facebook/ThreatExchange`
- Commit：`baefb4ed67b6cdc1d4c82dbaef858d50866ac424`
- 下载归档 SHA-256：`7EB03276C7DEF45D4E4DD0FBCEE233FAC2F85D24F269E73F1E3FBDCDBEE509C6`
- 许可证：BSD 3-Clause；发布包必须包含对应许可证正文。

## 参考实现范围

生产实现只复现下列上游阶段，不编译、链接或发布上游 C++：

- `pdq/cpp/common/pdqhashtypes.cpp`：256 位 hash 的 `u16` 字序和文本表示。
- `pdq/cpp/downscaling/downscaling.cpp`：两轮 Jarosz XY box filter 与 64×64 中心抽样。
- `pdq/cpp/hashing/pdqhashing.cpp`：Quality、16×64 DCT、阈值和 bit 设置顺序。
- `pdq/cpp/hashing/torben.cpp`：Torben 中位数。

Rust 边界先把 RGB24 用项目固定公式 `(77R + 150G + 29B + 128) >> 8` 转成灰度；
PDQ 内部阶段再按上游 `f32` 运算顺序处理。上游的 16 个 `u16` 结果只在
`PdqHash::from_upstream_words` 转换一次：逆序遍历 word，并逐个写成大端字节。

## 固定测试图片

三张图片均复制自上述 commit，测试通过 `image` 解码为 RGB24 后进入同一生产像素管线：

| 本地文件 | 上游路径 | SHA-256 |
|---|---|---|
| `bridge-original.jpg` | `pdq/data/reg-test-input/dih/bridge-1-original.jpg` | `B5B0799616DF52D475A3968DC7E54F1D0724C912244FFA6175BC786375DD7298` |
| `blur-a-little.jpg` | `pdq/data/bridge-mods/blur-a-little.jpg` | `25094AFA08E258B3135208B54B0ED28A50AD5386A4E4B66501E7AD886864FD54` |
| `small.jpg` | `pdq/data/misc-images/small.jpg` | `329479790610CD0A2668EEA439374CF279EF58D7330400DBB4E3569429EF5E3D` |

测试期望直接取自同一上游快照；若结果变化，应先定位像素解码、浮点运算顺序或位序差异，
不能通过更新 golden 掩盖实现漂移。

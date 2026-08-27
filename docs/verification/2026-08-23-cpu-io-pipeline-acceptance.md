# Rust V2 Node 真实媒体半小时运行验收

结论：PASS

## 自动化门禁

- 实际计算窗口：1800 秒
- 运行样本：894 条；系统样本：658 条
- 最大采样间隔：3 秒
- 机器 ID：cd0ea5ed8a50982a0116d0a169c9974e96fa3d9bb2487bce37e18633866acf4f
- 运行任务 ID：2f990bb8-18d8-4d83-aaac-de1bb0d7e210、d414a8c7-603f-40a1-b90f-b2743fd31eb6、f12c0b42-a957-4712-862c-63fc4aead3fe
- 有效 Worker 配置：12；非空闲峰值：12；非空闲平均：7.99

- 无自动化失败条件。

## Node 配置摘要

- 枚举器：Everything（不可用时由 Node 回退 Windows Walker）
- 单块读取超时：3 秒；重试：2 次；块大小：4 MiB
- 请求配置：Worker 12；HDD 1/盘、SSD 16/盘、未知盘 1/盘、总读取 16
- 配置与运行数据位于本次隔离目录，重启后运行详情不持久化。

## 实际执行配置

- Worker 槽：12；CPU 权重预算：23；Hash 并发：16
- 全局磁盘许可：16；HDD/盘：1；SSD/盘：16；未知盘/盘：1
- 以上值直接来自 Node 运行详情；缺失字段显示 —，不从进程数或默认配置估算。

## 总文件与字节

- 源媒体文件：14786
- 源媒体字节：108.80 GiB
- 运行任务最终计数：0 / 14786

## 各阶段耗时与吞吐

| 阶段 | 完成/总计 | 已运行毫秒 | 峰值速度/秒 | 失败 |
| --- | ---: | ---: | ---: | ---: |
| 计算基础特征 (compute_base_features) | 14757 / 14786 | 760333 | 389.78 | 8 |
| 枚举文件 (enumerate_files) | 14786 / 14786 | 15545 | 281,436.53 | 0 |
| 查询基础缓存 (lookup_base_cache) | 14786 / 14786 | 56607 | 826.76 | 0 |

## Worker 并行

- 峰值非空闲 Worker：12
- 平均非空闲 Worker：7.99
- 观察到的物理盘：PhysicalDisk2
- 同时工作的物理盘峰值：1

## Worker 子阶段

| 显式阶段 | 采样行数 | 最大 CPU 权重 | 最大解码线程 |
| --- | ---: | ---: | ---: |
| decode | 4783 | 2 | 2 |
| feature | 2332 | 1 | 1 |
| idle | 3048 | — | — |
| result_wait | 18 | 1 | 1 |

## 流水线运行指标

| 项目 | 类型 | 当前峰值 | 历史峰值 | 硬容量 | 等待 P95 | 服务/持有 P95 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| Hash队列 | 队列 | 16 | 16 | 16 | — | 30000 ms |
| 路径缓存队列 | 队列 | 0 | 0 | 2 | — | — |
| 内容缓存队列 | 队列 | 0 | 0 | 64 | — | — |
| 待解码队列 | 队列 | 68 | 71 | 76 | 1 ms | 10000 ms |
| 持久化队列 | 队列 | 17 | 35 | 1012 | 64 ms | 16 ms |
| Hash磁盘许可 | 资源 | 16 | 16 | 16 | 5000 ms | 30000 ms |
| 媒体磁盘许可 | 资源 | 12 | 12 | 16 | 1000 ms | 52547 ms |
| CPU权重 | 资源 | 13 | 14 | 23 | — | — |
| Worker槽 | 资源 | 12 | 12 | 12 | — | — |

- Hash累计读取：108.80 GiB
- 队列容量门禁：PASS

## Node/Worker CPU 与内存

| 进程 | 平均每 tick CPU 毫秒 | Working Set 峰值 | Private 峰值 |
| --- | ---: | ---: | ---: |
| Everything | 34.56 | 645.81 MiB | 640.46 MiB |
| node | 1,582.68 | 108.30 MiB | 97.55 MiB |
| runtime_acceptance | 0.43 | 6.34 MiB | 2.81 MiB |
| worker | 1,652.23 | 891.57 MiB | 907.00 MiB |

## 物理磁盘读取

| 物理盘实例 | 平均读吞吐 | 峰值读吞吐 | 队列峰值 |
| --- | ---: | ---: | ---: |
| 0 C: D: | 1.27 MiB/s | 591.10 MiB/s | 1.00 |
| 1 G: H: | 1.53 MiB/s | 1,006.59 MiB/s | 0.00 |
| 2 I: | 160.07 MiB/s | 386.29 MiB/s | 30.00 |

## 最近失败

- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [2号酱@Rouer22]【注销】\[Twitter][萝莉] [2号酱@Rouer22]【注销】\P-2号酱@Rouer22 (17).jpg, Worker 管道分帧失败: 协议帧被截断（399 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Fariskitten猫型人偶]【注销】\[Twitter][萝莉] [Fariskitten猫型人偶]【注销】\V-Fariskitten猫型人偶 (27).mp4, Worker 管道分帧失败: 协议帧被截断（390 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [i.k@Criskissly]【注销】\[Twitter][萝莉] [i.k@Criskissly]【注销】\V-i.k@Criskissly (5).mp4, Worker 管道分帧失败: 协议帧被截断（388 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [i.k@Criskissly]【注销】\[Twitter][萝莉] [i.k@Criskissly]【注销】\V-i.k@Criskissly (6).mp4, Worker 管道分帧失败: 协议帧被截断（387 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Kit@kittyxkum]\[Twitter][萝莉] [Kit@kittyxkum]\V-Kit@kittyxkum (316).mp4, Worker 管道分帧失败: 协议帧被截断（363 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Kit@kittyxkum]\[Twitter][萝莉] [Kit@kittyxkum]\V-Kit@kittyxkum (311).mp4, Worker 管道分帧失败: 协议帧被截断（363 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Kit@kittyxkum]\[Twitter][萝莉] [Kit@kittyxkum]\V-Kit@kittyxkum (305).mp4, Worker 管道分帧失败: 协议帧被截断（363 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [Loli Gumy@LoliGumy]\[Twitter][萝莉] [Loli Gumy@LoliGumy]\V-Loli Gumy@LoliGumy (3).mp4, Worker 管道分帧失败: 协议帧被截断（330 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [不许凶然然@zkr03zkr]【注销】\[Twitter][萝莉] [不许凶然然@zkr03zkr]【注销】\V-不许凶然然@zkr03zkr (3).mp4, Worker 管道分帧失败: 协议帧被截断（316 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [姜兔兔@nainai010821]\[Twitter][萝莉] [姜兔兔@nainai010821]\V-姜兔兔@nainai010821 (52).mp4, Worker 管道分帧失败: 协议帧被截断（291 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [小瑶萝莉酱@kissyaoyao]\[Twitter][萝莉] [小瑶萝莉酱@kissyaoyao]\P-小瑶萝莉酱@kissyaoyao (180).jpg, Worker 管道分帧失败: 协议帧被截断（283 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\61F058AE0ABDF3748619DF441F3B7672CDB4CD1A.torrent, 媒体解码失败: FFmpeg operation avformat_open_input(custom_io) failed with code -1094995529（270 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [崽崽baby@Yourzz0299]【冻结】\[Twitter][萝莉] [崽崽baby@Yourzz0299]【冻结】\V-崽崽baby@Yourzz0299 (2).mp4, Worker 管道分帧失败: 协议帧被截断（269 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [崽崽baby@Yourzz0299]【冻结】\[Twitter][萝莉] [崽崽baby@Yourzz0299]【冻结】\V-崽崽baby@Yourzz0299 (3).mp4, Worker 管道分帧失败: 协议帧被截断（267 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [晨喵酱@_chenmiaojiang]【停更】\[Twitter][萝莉] [晨喵酱@_chenmiaojiang]【停更】\P-晨喵酱@_chenmiaojiang (3).jpg, Worker 管道分帧失败: 协议帧被截断（261 次）
- compute_base_features, I:\tmp\bt\399\6EC480DC646BB5F60F06542E7A83B1605F7D8213.torrent, 媒体解码失败: FFmpeg operation avformat_open_input(custom_io) failed with code -1094995529（260 次）
- compute_base_features, I:\tmp\bt\OnlyFans.2024.KittyKum.Anal.Masturbation.XXX.1080p.MP4-P2P[XC]\34D850C9CB79430D525567C96BDA89914AFB2C5F.torrent, 媒体解码失败: FFmpeg operation avformat_open_input(custom_io) failed with code -1094995529（260 次）
- compute_base_features, I:\tmp\bt\Npxvip 我可以成為你的私人女僕把你吸干凈嗎\1A8F600F2AE9D8E0E6A3A4E0D09F9121A5A688DC.torrent, 媒体解码失败: FFmpeg operation avformat_open_input(custom_io) failed with code -1094995529（255 次）
- compute_base_features, I:\tmp\bt\福利姬KittyxKum  精油透明阳具双洞开发流白浆 想在这个万圣节成为我的特别款待\E767C537796D895AC9893BEA70AE4AEF87AD9DD0.torrent, 媒体解码失败: FFmpeg operation avformat_open_input(custom_io) failed with code -1094995529（229 次）
- compute_base_features, I:\tmp\Twitter推特高质量福利姬270套36000张图片视频合集-萝莉篇56G\[Twitter][萝莉] [茶小狸@NaiziQv]\[Twitter][萝莉] [茶小狸@NaiziQv]\V-茶小狸@NaiziQv (4).mp4, Worker 管道分帧失败: 协议帧被截断（221 次）

## 文件故障分类

- 疑似物理读取故障样本：0
- Worker崩溃观察样本：6091（本轮仅记录，不单独作为 CPU/I/O 架构 FAIL 条件）

## 联系表复用

- 本次记录的 MD5 联系表复用数：0

## 磁盘满清理

- 触发次数：0
- 本次未触发，不能从本次实测证明清理路径。

## 真实媒体未修改证明

- 验收前：14786 个文件，108.80 GiB
- 验收后：14786 个文件，108.80 GiB
- 路径、长度、LastWriteTimeUtc 逐项一致：True

## 实测与未触发边界

- 自动化门禁来自 runtime/system NDJSON 与前后媒体清单。
- OS 采样只说明本轮实际观察到的 CPU、内存和物理盘吞吐。
- SSD/HDD 识别结果仅作观察，不属于本轮 CPU/I/O 架构验收门禁。
- 没有发生的故障、磁盘满或崩溃路径不会被写成“已通过实测”。
- 原始证据目录：C:\tmp\rust-v2-runtime-acceptance\192adf49629a422581046444b605e88f\evidence
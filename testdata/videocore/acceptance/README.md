# VideoCore 动态验收输入

本目录仅保存验收清单，不提交、不生成也不复制真实媒体语料。动态验收 runner 必须引用
`../compat/manifest.json` 中已提交的合成 fixture，并把全部运行产物写入显式指定的临时证据目录。

真实验收只可通过 `windows && videocoreacceptance` 构建标签显式启动，并要求绝对路径的
stage、corpus、runner 与 evidence 参数。PostgreSQL DSN 只能通过 `FS_PG_DSN` 环境变量传递，
不得写入命令行、日志或证据。

当前仓库未附带真实 FFmpeg 再分发 stage，因此本目录本身不代表动态验收已经完成。

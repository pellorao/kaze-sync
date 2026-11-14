# 更新日志

[English](CHANGELOG.md) | [简体中文](CHANGELOG.zh-CN.md)

kaze-sync 的重要变更按发布版本分组列于下方, 版本号遵循 `vMAJOR.MINOR.PATCH` 格式.

## v0.1.0 - 2026-11-14

第一个公开版本, 涵盖 2026-11-02 至 2026-11-14 的开发. 完整说明见 [v0.1.0 发布说明](RELEASE-NOTES.zh-CN.md).

### 新增

- **同步与存储**: 支持每台设备任意多个文件夹的双向同步, 采用 256 KiB 去重数据块, 成批块下载, 并行计算哈希以及支持 15 GiB 以上直至 128 GiB 单文件的分阶段块列表; 服务端每个文件夹使用独立的 SQLite 数据库; 支持符号链接同步 (协议版本 5); 针对 Windows 占用文件 (`in_use`) 和活动 SQLite 数据库提供慢速自动重试; 提供全局与文件夹两层 `.gitignore` 语法排除规则, 支持保存前预览命中的服务端文件, 出厂默认规则包含 Nextcloud 系统文件.
- **写入控制 (协议版本 6)**: 提供目录与文件的三种写入模式: **不限** (`free`), **单端** (`single`) 和**独占** (`exclusive`), 配备可配置的单端写入时间窗 (默认 60 秒) 与新文件夹提醒; 同时写入或基于过时内容的写入会在后到的设备上冻结对应范围, 并将改动留作待处理项, 由用户在 Web UI 中逐条或按组决定上传或丢弃 (丢弃时默认将本机副本保存到 `.syncsaved/`). `free` 目录下的文件自动继承 `single`, 且服务端拒绝将单个文件设为 `free`.
- **历史版本, 恢复与导出**: 服务端默认保留被覆盖或删除的文件版本 30 天, 并由固定内存上限的每日垃圾回收清理; 提供带日期轴和时刻轴的单文件, 已删除文件及按时刻目录恢复对话框; 提供 `kaze-syncd export` 命令以将当前或指定时刻的文件导出为普通文件.
- **Web UI 与通知**: 提供支持简体中文, 繁体中文, 英文, 日文和韩文五种语言的深色主题 Web UI, 具备实时状态更新, 写入控制图标与提示框, 始终保持在视口内的菜单和提示框, 独立滚动的表格容器以及统计当前目录的文件夹页标题栏; 服务端和客户端支持通过 `ntfy.json` 发送带速率限制的 [ntfy](https://ntfy.sh) 消息推送.
- **平台, 安装与安全**: 服务端 `kaze-syncd` (x86-64) 以 root 安装并以非特权用户 `kaze-sync` 运行在 OpenWrt (`procd`) 及普通 Linux (带单元安全加固的 `systemd` 或 `OpenRC`) 上; 提供 Linux 与 Windows 客户端 `kaze-syncc` (`kaze-syncc.exe` 与无窗口的 `kaze-syncc-agent.exe`); 支持自签名证书, 自定义证书 (`--cert`/`--key`) 和本地 CA (`--ca` / `kaze-syncd ca`) 三种 TLS 证书模式; 支持服务端数据库自动迁移并在迁移前备份到 `<data>/var/backups/`, 以及客户端本地状态库结构版本 3 (拒绝更高版本的数据库).
- **文档与测试**: 提供中英文双语 [README](README.zh-CN.md) 与[用户手册](docs/manuals/user-manual.zh-CN.md) (已对照代码逐项核对, 含 [2.6 节端口与防火墙规则](docs/manuals/user-manual.zh-CN.md#26-端口与防火墙规则)及 [2.4 节](docs/manuals/user-manual.zh-CN.md#24-端口-证书与防火墙)在 Windows, macOS, Linux, Android, iPhone 和 iPad 上导入本地 CA 的步骤); 包含单元测试, 模拟测试及端到端测试 (含同时检查推送消息延迟的 8 场景写入控制驱动).

### 已知限制

- `install` 不会检查服务用户 `kaze-sync` 能否读取自定义的 `--cert` 和 `--key` 文件; 启动服务前需赋予 `kaze-sync` 对这两个文件及其上层目录的读取权限 ([手册 2.4 节](docs/manuals/user-manual.zh-CN.md#24-端口-证书与防火墙)).
- 服务端安装与 CA 续期需要 root; 仅提供源码构建 (无预编译二进制, 无版本号输出, 无自动更新); 仅支持 x86-64 服务端与 Linux/Windows 客户端; 未开启开发人员模式的 Windows 不创建同步的符号链接并折叠显示一行提醒; 服务端数据不是备份且采用块存储 (需用 `kaze-syncd export` 导出普通文件). 完整列表见 [v0.1.0 发布说明](RELEASE-NOTES.zh-CN.md#已知限制)和[手册第 18 节](docs/manuals/user-manual.zh-CN.md#18-kaze-sync-不做什么).

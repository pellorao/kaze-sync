# kaze-sync v0.2.0 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.2.0` (发布于 2026-11-14) 是 kaze-sync 继 `v0.1.0` 之后的一次小型更新. kaze-sync 是面向 Linux 和 Windows 电脑的自托管文件同步服务, 自带历史版本保留 (`kaze-syncd` 运行在 OpenWrt 或使用 systemd, OpenRC 的 x86-64 Linux 上, `kaze-syncc` 客户端运行在 Linux 和 Windows 上并集中在 Web UI 中配置). 本版本为两个二进制文件和 Web UI 引入语义化版本号, 在 `install` 时检查自定义 TLS 证书的读取权限, 并改进 Web UI 的提示框, 时间列与键盘焦点细节. 关于同步, 写入控制与历史恢复等完整功能介绍, 请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md)或保存在 Git 标签 `v0.1.0` 中的 `v0.1.0` 发布说明.

与 `v0.1.0` 一样, 本次发布仅提供源码, 没有预编译二进制包下载: 请按[用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-构建二进制)的步骤, 在任意装有 Go 的机器上编译静态链接的二进制文件. 可通过 `kaze-syncd --version` 和 `kaze-syncc --version` (或登录 Web UI 后查看左上角) 以及 Git 标签 `v0.2.0` 标识构建版本.

## 主要更新

- **二进制文件与 Web UI 引入语义化版本号.** kaze-sync 现在使用统一的语义化版本号 (`0.2.0`), 在构建时嵌入两个二进制文件; 执行 `kaze-syncd --version` 和 `kaze-syncc --version` 分别输出 `kaze-syncd 0.2.0` 和 `kaze-syncc 0.2.0`. 登录后的 Web UI 左上角显示 `Kaze Sync` 及浅灰色版本号 (与顶栏的对比度为 5.3:1), 浏览器标签页标题显示为 `... - Kaze Sync`; 登录页显示 `Kaze Sync` 但有意不显示版本号, 使未登录的访问者无法获知服务端版本. 此后每次发布都会提高版本号 (修复升 `PATCH`, 用户可见的新功能以及 `1.0.0` 之前不兼容的协议或数据变更升 `MINOR`).
- **`install` 检查自定义 TLS 证书读取权限.** 使用自定义 `--cert` 和 `--key` 文件安装服务端时 (含重复运行 `install` 检查已保存路径时), `install` 现在会按权限位 (含 `kaze-sync` 用户的附加组; 不检查 ACL) 核对非特权服务用户 `kaze-sync` 能否读取这两个文件以及能否进入每一级上层目录. 如果无权访问或文件不存在, `install` 会输出一行以 `warning:` 开头的警告, 写明对应的文件或目录并给出[手册 2.4 节](docs/manuals/user-manual.zh-CN.md#24-端口-证书与防火墙)中的 `chgrp`/`chmod` 修复命令, 同时照常完成安装. 这消除了 `v0.1.0` 的第一条已知限制.
- **页面自带提示框, 时间列表头时区与焦点保持.** Web UI 中的多处提示 (时间戳的时区, 排除规则行的报错, 时区示例, 以及没有剩余文件夹可映射时禁用的 **添加映射** 按钮说明) 现在改用页面自带的提示框代替浏览器原生提示框: 悬停, 聚焦或点击时打开, 按 `Esc` 关闭, 鼠标指针可移入提示框内, 且在对话框和通知浮窗内均可正常使用. 表格中每个时间列的表头统一写一次时区 (长时区名在表头自动换行) 而不再逐个时间戳标注, 时间戳不再作为键盘 `Tab` 停靠点; 页面实时刷新时保持键盘焦点.
- **Web UI 窄屏布局与焦点修复.** 窄屏下总览页中的文件夹名称保持约八个字符的最小宽度, 不再缩成每行一个字; 打开写入控制对话框时键盘焦点停在当前已选中的选项上; 置灰的 **添加映射** 按钮保留清晰的焦点环.

## 升级

自 `v0.1.0` 以来, 同步协议仍为版本 6, 服务端数据库与客户端本地状态库的表结构均未变化. 本次升级不涉及数据迁移, 服务端与客户端可以按任意顺序升级 (完整升级步骤见[用户手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)):

- **服务端 (`kaze-syncd`)**: 编译 `v0.2.0` 二进制文件 ([手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-构建二进制)), 复制到服务器上, 以 root 执行 `./kaze-syncd install --data <同一目录>`. 原有的监听端口, TLS 证书和管理员密码均保持不变, `install` 会把新二进制复制进 `<data>/` 并重启服务.
- **客户端 (`kaze-syncc`)**: 在每台 Linux 或 Windows 电脑上停止正在运行的 agent, 替换二进制文件 (Linux 为 `kaze-syncc`, Windows 为 `kaze-syncc.exe` 及带 `-H=windowsgui` 编译的 `kaze-syncc-agent.exe`), 再重新启动 agent.

## 已知限制

- **服务端安装与证书续期需要 root.** 安装服务端和续期本地 CA 证书必须以 root 执行, 不支持非 root 部署服务端.
- **无预编译二进制, 无自动更新.** 不提供预编译二进制文件下载, 也没有自动更新机制.
- **功能边界 ([手册第 18 节](docs/manuals/user-manual.zh-CN.md#18-kaze-sync-不做什么)).** 不做文件分享, 多用户账号或多人协作; Web UI 不提供文件预览或文件下载; 不同步空目录, Windows 联接点, 除可执行位以外的文件权限, 文件所有者和扩展属性; 不做端到端加密, 传输压缩或带宽限速; 没有按设备设置的排除规则; Web UI 没有双因素认证; 客户端没有桌面托盘图标或弹窗; 没有 macOS 或移动端客户端; 服务端仅支持 x86-64 架构; 单个文件超过 128 GiB 时服务端拒收.
- **不是备份.** 历史版本与服务端数据存放在同一块磁盘上, 请定期将服务端数据目录备份到另一块磁盘或另一台机器 ([手册第 14 节](docs/manuals/user-manual.zh-CN.md#14-服务端的备份与还原)).
- **未开启开发人员模式的 Windows 无法创建符号链接.** 未开启开发人员模式的 Windows 客户端不会在本地创建同步过来的符号链接; 这些链接保留在服务端, 并在 Web UI 中按设备折叠显示一行提醒.
- **高频改写的文件.** 每隔几秒就改写一次的文件 (例如状态文件) 每次写入都会产生旧版本, 直到超出保留期被清理; 若不需要保留这些版本, 请用排除规则忽略对应目录 ([手册第 7 节](docs/manuals/user-manual.zh-CN.md#7-排除规则)及[第 13 节](docs/manuals/user-manual.zh-CN.md#13-保留期与垃圾回收)).
- **服务端存储格式.** 服务端磁盘上保存的是去重数据块和 SQLite 数据库而非普通文件; 需要普通文件时请以 root 执行 `kaze-syncd export` ([手册第 12 节](docs/manuals/user-manual.zh-CN.md#12-导出为普通文件)).
- **数据库迁移后的服务端回滚.** 服务端完成数据库结构迁移后如需回退到旧版本二进制, 必须使用升级前在 `<data>/var/backups/` 下自动生成的数据库备份并加 `--new-epoch` 还原 ([手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)).

## 环境要求与兼容性

| 组件 | 要求 |
|---|---|
| 构建工具链 | Go `1.27.3` (见 `go.mod`), 使用 `CGO_ENABLED=0` 编译静态二进制文件 ([手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-构建二进制)) |
| 服务端 (`kaze-syncd`) | x86-64 Linux, 使用 OpenWrt (`procd`, 需挂载独立数据盘), systemd 或 OpenRC; 安装与维护需要 root 权限; 目标机器内存 512 MB |
| Linux 客户端 (`kaze-syncc`) | x86-64 Linux, 提供 systemd 用户会话 |
| Windows 客户端 (`kaze-syncc.exe`, `kaze-syncc-agent.exe`) | x86-64 Windows, 两个 `.exe` 文件放在同一个固定目录下 |
| Web UI 浏览器 | Chrome 或 Edge 114 以上, Firefox 125 以上, Safari 17 以上, 且开启 JavaScript |
| 协议与数据库版本 | 同步协议版本 6 (服务端要求客户端协议版本为 6); 客户端本地状态库结构版本 3 |

## 更多文档

- [README](README.zh-CN.md): 了解 kaze-sync 的设计取舍与五步快速开始.
- [更新日志](CHANGELOG.zh-CN.md): 各发布版本的变更摘要.
- [用户手册](docs/manuals/user-manual.zh-CN.md): 日常操作的完整参考, 涵盖安装, Web UI 各页面, 文件夹与写入控制, 排除规则, 处理待处理改动, 通知与推送, 恢复历史版本, 导出普通文件, 备份与还原, 升级, 卸载及问题排查.
- [`docs/operations.md`](docs/operations.md): 系统级部署细节, OpenWrt 单盘划分数据分区, systemd 加固说明以及备份还原命令.

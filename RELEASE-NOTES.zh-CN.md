# kaze-sync v0.3.3 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.3.3` (发布于 2026-11-15) 是 kaze-sync 的一次 Web UI 补丁版本. kaze-sync 是面向 Linux 和 Windows 电脑的自托管文件同步服务, 自带历史版本保留. 本次更新修复日文, 韩文和中文界面在数量为 1 时出现英文句子的问题, 以及文件夹名含字面占位符时被改写的问题. 关于同步, 写入控制与历史恢复等完整功能介绍, 请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md)或保存在 Git 标签 `v0.1.0` 中的 `v0.1.0` 发布说明.

与此前版本一样, 本次发布仅提供源码, 没有预编译二进制包下载: 请按[用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-构建二进制)的步骤, 在任意装有 Go 的机器上编译静态链接的二进制文件. 可通过 `kaze-syncd --version` 和 `kaze-syncc --version` (或登录 Web UI 后查看左上角) 以及 Git 标签 `v0.3.3` 标识构建版本.

## 主要更新

- **非英文界面在数量为 1 时不再出现英文句子.** 数量为 1 时另有写法的文本 (例如映射行恰有 1 个独占锁时删除映射确认框的末句) 在日文, 韩文和中文界面中曾显示为英文, 现在显示界面语言.
- **占位符一次性填入.** 含字面 `{path}` 的文件夹名在对话框中按原样显示.
- 测试: `internal/server/i18n1_test.go`, `internal/server/jstest/rt.test.js`; 已在日文, 韩文和简体中文下核对截图.

## 升级

同步协议仍为版本 6, 服务端数据库与客户端本地状态库的表结构及客户端行为均未变化. 本次升级不涉及数据迁移; 由于客户端没有 Web UI, 只有升级服务端二进制才会看到本次变化, 客户端无需升级 (如仍需升级, 服务端与客户端可按任意顺序升级; 完整升级步骤见[用户手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)):

- **服务端 (`kaze-syncd`)**: 编译 `v0.3.3` 二进制文件 ([手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-构建二进制)), 复制到服务器上, 以 root 执行 `./kaze-syncd install --data <同一目录>`. 原有的监听端口, TLS 证书和管理员密码均保持不变, `install` 会把新二进制复制进 `<data>/` 并重启服务.
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

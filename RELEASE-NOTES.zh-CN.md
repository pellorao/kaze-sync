# kaze-sync v0.3.5 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.3.5` (发布于 2026-11-18) 是 kaze-sync 的一次功能与维护版本. kaze-sync 是面向 Linux 和 Windows 电脑的自托管文件同步服务, 自带历史版本保留. 本次更新让设备页显示各客户端的发布版本, 让服务端的垃圾回收 (GC) 内存按机器内存计算而不再固定为 64 MB, 让两个程序在内存中保留自身内存与 CPU 用量的每日摘要, 并且首次随版本提供预编译包. 关于同步, 写入控制与历史恢复等完整功能介绍, 请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md)或保存在 Git 标签 `v0.1.0` 中的 `v0.1.0` 发布说明.

**预编译包.** 此前的版本只提供源码. 从本版本起, [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) 页面提供 (仅限 x86-64 即 `amd64`) `kaze-sync-0.3.5-linux-amd64.tar.gz` (服务端 `kaze-syncd` 和 Linux 客户端 `kaze-syncc`), `kaze-sync-0.3.5-windows-amd64.zip` (`kaze-syncc.exe` 和无窗口的 `kaze-syncc-agent.exe`) 以及 `SHA256SUMS`. 请按[用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-下载并校验服务端发布包)的说明用 `sha256sum -c SHA256SUMS` (Windows 上用 `Get-FileHash`) 校验下载. 包由 `go run ./tools/release` 可复现地构建. 其他 CPU 架构 (例如大多数 OpenWrt 路由器) 仍需从源码编译 ([用户手册附录 A](docs/manuals/user-manual.zh-CN.md#附录-a-从源码编译)). 可通过 `kaze-syncd --version` 和 `kaze-syncc --version` (或登录 Web UI 后查看左上角) 以及 Git 标签 `v0.3.5` 标识构建版本.

## 主要更新

- **设备页显示客户端版本.** 设备列表和设备页显示客户端随每次长轮询上报的发布版本 (`x.y.z`), 替代原来的整数协议版本. 比本版本更旧的客户端不上报发布版本, 升级之前显示 `-`. 协议版本仍为 6.
- **GC 内存随机器而定.** 服务端启动时一次性算出 GC 引用数组的内存预算: 取物理内存与容器或服务内存限额中较小者的 3/8, 下限 64 MB (读不到物理内存时也用 64 MB). 512 MB 的机器得到 192 MB, 一趟可处理约 600 万个块引用, 而原来固定的 64 MB 上限需要多趟. 小数据量仍只分配实际需要的数组, 不增加设置项, `msg=gc` 日志行新增以字节为单位的 `budget` 字段. 用户手册新增的 "内存估算" 一节 (第 13 节) 说明多少文件需要多少内存, 以及 `keep_days` 如何影响它.
- **每日资源摘要.** 两个程序每分钟采样自身的内存和 CPU, 只在内存中保留今天和昨天的摘要. 客户端随长轮询请求上报, 已登录的管理员可以通过 `GET /resources` 以 JSON 读取服务端和各设备的数据: 常驻内存中位数, CPU 时间, GC 耗时与 CPU, 引用数, 内存预算和观察到的内存峰值, 并附带标记说明当天覆盖了多少, 峰值是否准确. 不写任何文件或数据库, 不增加依赖或 Web UI 控件. 进程重启后摘要随之丢失.
- **文档.** 用户手册默认从预编译包安装, 从源码编译改为附录; README 快速开始改为下载预编译包; 运维说明改写为新的内存模型.
- **发布工具.** `go run ./tools/release` 构建发布包, `go run ./tools/pubcheck` 在发布前检查源码树中的个人注记和本机路径.

## 升级

同步协议仍为版本 6, 最低客户端版本不变, 数据库表结构也未变化, 所以不会进行数据迁移. 长轮询请求里新增的字段都是可选的: 旧客户端可以继续与新服务端同步, 新客户端也可以与旧服务端配合. 仍建议按用户手册的做法先升级服务端, 再升级客户端 (完整步骤见[用户手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)):

- **服务端 (`kaze-syncd`)**: 下载并校验 Linux 包 ([手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-下载并校验服务端发布包)), 把 `kaze-syncd` 复制到服务器上, 以 root 执行 `./kaze-syncd install --data <同一目录>`. 原有的监听端口, TLS 证书和管理员密码均保持不变, `install` 会把新二进制复制进 `<data>/` 并重启服务.
- **客户端 (`kaze-syncc`)**: 在每台 Linux 或 Windows 电脑上停止正在运行的 agent, 替换二进制文件 (Linux 为 `kaze-syncc`, Windows 为 `kaze-syncc.exe` 和 `kaze-syncc-agent.exe`), 再重新启动 agent. 客户端重启并完成一次长轮询后, 设备页显示 `0.3.5`.

## 已知限制

- **服务端安装与证书续期需要 root.** 安装服务端和续期本地 CA 证书必须以 root 执行, 不支持非 root 部署服务端.
- **预编译包仅限 x86-64, 无自动更新.** 其他架构需从源码编译, 也没有自动更新机制.
- **功能边界 ([手册第 18 节](docs/manuals/user-manual.zh-CN.md#18-kaze-sync-不做什么)).** 不做文件分享, 多用户账号或多人协作; Web UI 不提供文件预览或文件下载; 不同步空目录, Windows 联接点, 除可执行位以外的文件权限, 文件所有者和扩展属性; 不做端到端加密, 传输压缩或带宽限速; 没有按设备设置的排除规则; Web UI 没有双因素认证; 客户端没有桌面托盘图标或弹窗; 没有 macOS 或移动端客户端; 服务端仅支持 x86-64 架构; 单个文件超过 128 GiB 时服务端拒收.
- **不是备份.** 历史版本与服务端数据存放在同一块磁盘上, 请定期将服务端数据目录备份到另一块磁盘或另一台机器 ([手册第 14 节](docs/manuals/user-manual.zh-CN.md#14-服务端的备份与还原)).
- **未开启开发人员模式的 Windows 无法创建符号链接.** 未开启开发人员模式的 Windows 客户端不会在本地创建同步过来的符号链接; 这些链接保留在服务端, 并在 Web UI 中按设备折叠显示一行提醒.
- **高频改写的文件.** 每隔几秒就改写一次的文件 (例如状态文件) 每次写入都会产生旧版本, 直到超出保留期被清理; 若不需要保留这些版本, 请用排除规则忽略对应目录 ([手册第 7 节](docs/manuals/user-manual.zh-CN.md#7-排除规则)及[第 13 节](docs/manuals/user-manual.zh-CN.md#13-保留期与垃圾回收)).
- **服务端存储格式.** 服务端磁盘上保存的是去重数据块和 SQLite 数据库而非普通文件; 需要普通文件时请以 root 执行 `kaze-syncd export` ([手册第 12 节](docs/manuals/user-manual.zh-CN.md#12-导出为普通文件)).
- **数据库迁移后的服务端回滚.** 服务端完成数据库结构迁移后如需回退到旧版本二进制, 必须使用升级前在 `<data>/var/backups/` 下自动生成的数据库备份并加 `--new-epoch` 还原 ([手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)).

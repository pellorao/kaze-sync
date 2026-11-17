# kaze-sync v0.4.0 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.4.0` (发布于 2026-11-18) 把 ntfy 出错提醒换成了 Web Push. 完整功能介绍请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md), 或 Git tag `v0.1.0` 里的 `v0.1.0` 发布说明. 预编译包 (`kaze-sync-0.4.0-linux-amd64.tar.gz`, `kaze-sync-0.4.0-windows-amd64.zip`, `SHA256SUMS`, 仅限 x86-64) 在 [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) 页面, 用 `sha256sum -c SHA256SUMS` 校验 ([用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-下载并校验服务端发布包)). 用 `kaze-syncd --version` 和 `kaze-syncc --version` 以及 Git tag `v0.4.0` 识别构建.

## 主要更新

- **只由服务端发出的 Web Push.** 服务端保存 VAPID 密钥和各浏览器的订阅并自己发出每条提醒, 客户端不再推送任何东西. 每条推送都转发通知铃铛里的一条 (待处理改动满 2 分钟, 设备错误满 5 分钟, 客户端过旧, 设备离线, 数据盘空间不足, 垃圾回收失败, 证书无法续期). 每个原因对每个浏览器只推送一次, 重复出现要等满 1 小时, 合计每小时最多 20 条. 设置页新增 **推送通知** 卡片: 在浏览器里开启或关闭推送, 查看订阅, 发送测试推送. 推送需要受信任的证书 (本地 CA 或你自己的证书), 不支持 iPhone 和 iPad (用户手册 9.1 节).
- **移除 ntfy**, 服务端和客户端都不再有; 留下的 `ntfy.json` 不再被读取.
- **通知铃铛.** 新增四类问题 (客户端过旧, 数据盘空间不足, 垃圾回收失败, 证书无法续期); 设备 3 天没有连接 (原为 7 天) 即出现离线提醒.
- **排除规则预览.** 在过滤框里快速连续输入不再丢字.
- **客户端修复.** 某文件夹的同步周期连续失败 3 次时向服务端上报; Windows 客户端加单实例锁, 停止 agent 时容忍空的或读不出的 pid 文件.

## 升级

同步协议仍为版本 6, 最低客户端版本不变, 数据库表结构也未变化, 所以不会进行数据迁移. 先升级服务端, 再升级客户端 ([用户手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)): 在服务端以 root 执行 `./kaze-syncd install --data <同一目录>`; 在每台客户端替换二进制文件 (Linux 为 `kaze-syncc`, Windows 为 `kaze-syncc.exe` 和 `kaze-syncc-agent.exe`) 并重启 agent. 本次升级不改变服务端证书; 要在手机上用 Web Push, 先切换到本地 CA (用户手册 2.4 节), 这会更换服务端证书和客户端固定的指纹.

## 已知限制

- **服务端安装与证书续期需要 root.** 安装服务端和续期本地 CA 证书必须以 root 执行, 不支持非 root 部署服务端.
- **预编译包仅限 x86-64, 无自动更新.** 其他架构需从源码编译, 也没有自动更新机制.
- **功能边界 ([手册第 18 节](docs/manuals/user-manual.zh-CN.md#18-kaze-sync-不做什么)).** 不做文件分享, 多用户账号或多人协作; Web UI 不提供文件预览或文件下载; 不同步空目录, Windows 联接点, 除可执行位以外的文件权限, 文件所有者和扩展属性; 不做端到端加密, 传输压缩或带宽限速; 没有按设备设置的排除规则; Web UI 没有双因素认证; 客户端没有桌面托盘图标或弹窗; 没有 macOS 或移动端客户端; 服务端仅支持 x86-64 架构; 单个文件超过 128 GiB 时服务端拒收.
- **不是备份.** 历史版本与服务端数据存放在同一块磁盘上, 请定期将服务端数据目录备份到另一块磁盘或另一台机器 ([手册第 14 节](docs/manuals/user-manual.zh-CN.md#14-服务端的备份与还原)).
- **未开启开发人员模式的 Windows 无法创建符号链接.** 未开启开发人员模式的 Windows 客户端不会在本地创建同步过来的符号链接; 这些链接保留在服务端, 并在 Web UI 中按设备折叠显示一行提醒.
- **高频改写的文件.** 每隔几秒就改写一次的文件 (例如状态文件) 每次写入都会产生旧版本, 直到超出保留期被清理; 若不需要保留这些版本, 请用排除规则忽略对应目录 ([手册第 7 节](docs/manuals/user-manual.zh-CN.md#7-排除规则)及[第 13 节](docs/manuals/user-manual.zh-CN.md#13-保留期与垃圾回收)).
- **服务端存储格式.** 服务端磁盘上保存的是去重数据块和 SQLite 数据库而非普通文件; 需要普通文件时请以 root 执行 `kaze-syncd export` ([手册第 12 节](docs/manuals/user-manual.zh-CN.md#12-导出为普通文件)).
- **数据库迁移后的服务端回滚.** 服务端完成数据库结构迁移后如需回退到旧版本二进制, 必须使用升级前在 `<data>/var/backups/` 下自动生成的数据库备份并加 `--new-epoch` 还原 ([手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)).

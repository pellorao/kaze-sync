# kaze-sync v0.5.0 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.5.0` (发布于 2026-11-18) 新增设备改名, 以及待处理改动框中的批量勾选. 完整功能介绍请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md), 或 Git tag `v0.1.0` 里的 `v0.1.0` 发布说明. 预编译包 (`kaze-sync-0.5.0-linux-amd64.tar.gz`, `kaze-sync-0.5.0-windows-amd64.zip`, `SHA256SUMS`, 仅限 x86-64) 在 [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) 页面, 用 `sha256sum -c SHA256SUMS` 校验 ([用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-下载并校验服务端发布包)). 用 `kaze-syncd --version` 和 `kaze-syncc --version` 以及 Git tag `v0.5.0` 识别构建.

## 主要更新

- **设备改名.** 设备页的 **⋯** 菜单新增 **设备改名…**. 设备保留原有令牌和映射, 不需要重新注册; 当前文件, 待处理改动和通知显示新名称, 旧版本保留写入时的名称. 客户端从配置中取得新名称并记入 `device.json`.
- **待处理改动的批量操作.** 待处理改动框的每一行都有复选框 (表头的复选框全选), **把勾选的行设为** 对勾选的行统一设定动作. 一组超过 2000 个改动时按目录折叠.
- **可读性.** 灰色小字的对比度达到 4.5:1, 表头带有 `scope`.

## 升级

同步协议仍为版本 6 (配置中新增可选的设备名, 旧客户端会忽略它), 最低客户端版本不变, 数据库表结构也未变化, 所以不会进行数据迁移. 先升级服务端, 再升级客户端 ([用户手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)): 在服务端以 root 执行 `./kaze-syncd install --data <同一目录>`; 在每台客户端替换二进制文件并重启 agent. 比本版本旧的客户端在设备改名后, 升级前仍使用注册时的名称.

## 已知限制

已知限制与 [v0.4.0](CHANGELOG.zh-CN.md) 相同: 服务端安装需要 root, 预编译包仅限 x86-64 且无自动更新, 功能边界见[手册第 18 节](docs/manuals/user-manual.zh-CN.md#18-kaze-sync-不做什么), 不是备份, Windows 符号链接需要开发人员模式, 高频改写的文件会产生大量旧版本, 数据库迁移后的服务端回滚需要升级前的备份.

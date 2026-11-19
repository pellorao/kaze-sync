# kaze-sync v0.5.3 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.5.3` (发布于 2026-11-19) 让待处理的删除在服务端也删了该路径后自动清除 (包括服务端在这项核对出现之前做的删除), 并修复服务端还原后等待处理的范围的历史读取. 完整功能介绍请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md), 或 Git tag `v0.1.0` 里的 `v0.1.0` 发布说明. 预编译包 (`kaze-sync-0.5.3-linux-amd64.tar.gz`, `kaze-sync-0.5.3-windows-amd64.zip`, `SHA256SUMS`, 仅限 x86-64) 在 [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) 页面, 用 `sha256sum -c SHA256SUMS` 校验 ([用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-下载并校验服务端发布包)). 用 `kaze-syncd --version` 和 `kaze-syncc --version` 以及 Git tag `v0.5.3` 识别构建.

## 主要更新

- **待处理的删除自动了结.** 客户端向服务端查询待处理的删除; 服务端也删了该路径后只清除记录, 期间放回的文件留在原处. 详见[更新日志](CHANGELOG.zh-CN.md).

## 升级

协议 (在当前版本内追加了一个可选字段, 旧服务端和旧客户端照样可用), 最低客户端版本和数据库结构都不变. 先升级服务端 (在服务端用 root 运行 `./kaze-syncd install --data <同一目录>`, [用户手册 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)), 再升级客户端.

## 已知限制

已知限制与 [v0.4.0](CHANGELOG.zh-CN.md) 相同: 服务端安装需要 root, 预编译包仅限 x86-64 且无自动更新, 功能边界见[手册第 18 节](docs/manuals/user-manual.zh-CN.md#18-kaze-sync-不做什么), 不是备份, Windows 符号链接需要开发人员模式, 高频改写的文件会产生大量旧版本, 数据库迁移后的服务端回滚需要升级前的备份.

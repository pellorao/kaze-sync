# kaze-sync v0.7.3 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.7.3` (发布于 2026-11-25) 修复三个管理页问题: 规则编辑器的 "预览" 按钮丢失键盘焦点, 经设置页链接离开时未保存的规则修改不经询问就被丢弃, 版本历史表格在窄屏上需要横向滚动. 完整功能介绍请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md), 或 Git tag `v0.1.0` 里的 `v0.1.0` 发布说明. 预编译包与安装包 (`kaze-sync-0.7.3-linux-amd64.tar.gz`, `kaze-sync-0.7.3-windows-amd64.zip`, `kaze-sync-server-0.7.3-linux-amd64.sh`, `kaze-sync-client-0.7.3-linux-amd64.sh`, `kaze-sync-client-0.7.3-windows-amd64-setup.exe`, `SHA256SUMS`, 仅限 x86-64) 在 [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) 页面, 用 `sha256sum -c SHA256SUMS` 校验 ([用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-下载并校验服务端发布包)). 用 `kaze-syncd --version` 和 `kaze-syncc --version` 以及 Git tag `v0.7.3` 识别构建. Windows 安装程序没有代码签名, SmartScreen 会显示警告, 见用户手册.

## 主要更新

- 修复: 规则编辑器的 "预览" 按钮保持键盘焦点; 带着未保存的规则修改离开设置页或规则编辑器时先询问; 版本历史适配窄屏. 只改服务端 Web UI, 详见[更新日志](CHANGELOG.zh-CN.md).

## 升级

协议 (只新增可选的头, 旧服务端和旧客户端照样可用), 最低客户端版本和数据库结构都不变. 先升级服务端 (用 root 运行新的服务端安装包, 或用新二进制运行 `./kaze-syncd install`, [用户手册 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)), 再升级客户端.

## 已知限制

已知限制与 [v0.4.0](CHANGELOG.zh-CN.md) 相同: 服务端安装需要 root, 预编译包仅限 x86-64 且无自动更新 (升级只在你启动时进行), 功能边界见[手册第 18 节](docs/manuals/user-manual.zh-CN.md#18-kaze-sync-不做什么), 不是备份, Windows 符号链接需要开发人员模式, 高频改写的文件会产生大量旧版本, 数据库迁移后的服务端回滚需要升级前的备份.

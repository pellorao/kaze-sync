# kaze-sync v0.4.1 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.4.1` (发布于 2026-11-18) 是 `v0.4.0` Web Push 的补丁版本. 完整功能介绍请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md), 或 Git tag `v0.1.0` 里的 `v0.1.0` 发布说明. 预编译包 (`kaze-sync-0.4.1-linux-amd64.tar.gz`, `kaze-sync-0.4.1-windows-amd64.zip`, `SHA256SUMS`, 仅限 x86-64) 在 [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) 页面, 用 `sha256sum -c SHA256SUMS` 校验 ([用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-下载并校验服务端发布包)). 用 `kaze-syncd --version` 和 `kaze-syncc --version` 以及 Git tag `v0.4.1` 识别构建.

## 主要更新

- **每次推送都提醒.** 与仍在显示的通知同 tag 的新通知过去会静默替换旧的, 桌面浏览器只有第一次测试推送会提醒. 带 tag 的通知现在设置 `renotify`, 每次测试推送使用不同的 tag.
- **订阅失败说得更清楚.** 开启推送时浏览器连不上推送服务 (`AbortError`, 例如没有 Google Play 服务的 Android Chrome), 推送通知卡片会明确说明.

## 升级

同步协议仍为版本 6, 最低客户端版本不变, 数据库表结构也未变化, 所以不会进行数据迁移. 改动只在服务端的网页资源里, 客户端无需为此升级, 但为保持版本一致仍建议一并升级. 先升级服务端, 再升级客户端 ([用户手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)): 在服务端以 root 执行 `./kaze-syncd install --data <同一目录>`; 在每台客户端替换二进制文件并重启 agent. 服务端升级后, 浏览器在下一次更新检查时取得新的 service worker.

## 已知限制

已知限制与 [v0.4.0](CHANGELOG.zh-CN.md) 相同: 服务端安装需要 root, 预编译包仅限 x86-64 且无自动更新, 功能边界见[手册第 18 节](docs/manuals/user-manual.zh-CN.md#18-kaze-sync-不做什么), 不是备份, Windows 符号链接需要开发人员模式, 高频改写的文件会产生大量旧版本, 数据库迁移后的服务端回滚需要升级前的备份.

# kaze-sync v0.3.0 发布说明

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## 这是什么版本

`v0.3.0` (发布于 2026-11-15) 是 kaze-sync 的一次 Web UI 更新. kaze-sync 是面向 Linux 和 Windows 电脑的自托管文件同步服务, 自带历史版本保留 (`kaze-syncd` 运行在 OpenWrt 或使用 systemd, OpenRC 的 x86-64 Linux 上, `kaze-syncc` 客户端运行在 Linux 和 Windows 上并集中在 Web UI 中配置). 由于未安装合适中日韩系统字体的机器上 Web UI 文字显示不佳, 部分机器甚至完全没有韩文字体, `kaze-syncd` 现在自带自托管字体及按语言划分的中日韩字体子集, 使用极小的字体文件显示设置页语言列表中的各语言名称, 并修复窄屏下设置页时区列表溢出的问题. 关于同步, 写入控制与历史恢复等完整功能介绍, 请参阅[用户手册](docs/manuals/user-manual.zh-CN.md), [更新日志](CHANGELOG.zh-CN.md)或保存在 Git 标签 `v0.1.0` 中的 `v0.1.0` 发布说明.

与此前版本一样, 本次发布仅提供源码, 没有预编译二进制包下载: 请按[用户手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-构建二进制)的步骤, 在任意装有 Go 的机器上编译静态链接的二进制文件. 可通过 `kaze-syncd --version` 和 `kaze-syncc --version` (或登录 Web UI 后查看左上角) 以及 Git 标签 `v0.3.0` 标识构建版本.

## 主要更新

- **内置自托管 Web UI 字体.** 此前 Web UI 使用系统字体栈, 在未安装合适中日韩字体的机器上文字显示不佳, 部分机器上韩文甚至完全没有字体. `kaze-syncd` 现在内置 `Adobe Source` 家族字体与 `JetBrains Mono`: 拉丁, 希腊和西里尔字母使用 `Source Sans 3`, 汉字, 假名和谚文按页面语言选用对应地区版本的 `Source Han Sans` (简体中文, 繁体中文, 日文, 韩文), 路径, 规则, 命令和指纹使用 `JetBrains Mono`. 全部字体均采用 `SIL Open Font License 1.1` 许可证, 许可证与来源位于 `internal/server/web/fonts/` (`SOURCE.md`), 页面不向其他站点发起任何请求.
- **按语言划分子集以保持低流量.** 拉丁字体完整内嵌 (`Source Sans 3` 约 94 KB, `JetBrains Mono` 两个字重共约 77 KB), 而汉字, 假名与谚文字体按语言制作子集, 涵盖各地区的常用字符 (`GB 2312` 一级字, `Big5` 常用字, `JIS X 0208` 一级汉字加全部假名, `KS X 1001` 的 2350 个常用谚文音节) 以及界面文案用到的全部字符; 常规与粗体单文件大小分别为简体中文约 630 KB, 繁体中文约 955 KB, 日文约 513 KB, 韩文约 195 KB (粗体略大). 浏览器仅下载当前页面语言的文件, 且仅在页面含有该范围的字符时才下载 (英文页面不内嵌汉字字体), 因此首次打开页面的总开销约为英文 94 KB, 韩文 0.5 MB, 日文 1.1 MB, 简体中文 1.4 MB, 繁体中文 2.0 MB, 字体文件缓存一周. 子集之外的字符 (如文件名中的生僻汉字) 先回退到本机安装的 `Source Han Sans` 或 `Noto Sans CJK`, 再回退到系统字体.
- **设置页语言列表使用极小字体文件.** 设置页语言列表中的各语言名称分别使用四个极小的字体文件 (每个约 2 KB) 以各自语言的字体显示, 因此打开设置页不会下载体积较大的地区字体子集.
- **服务端二进制体积与子集构建脚本.** 内嵌字体使服务端二进制文件增加约 4.8 MB (x86-64 `kaze-syncd` 从 22.8 MB 增至 27.7 MB), 客户端二进制的行为不变. 仓库提供重建子集的脚本 `tools/fontsubset/subset.py` (依赖 `fonttools` 与 `brotli`), 各子集包含的字符列于 `tools/fontsubset/coverage/`, 新增界面文案若使用了子集之外的字符会使测试失败.
- **窄屏下设置页布局修复.** 修复在 400 px 宽度下设置页的时区列表超出所在卡片约 19 px 的问题; 现在列表会随卡片收缩.

## 升级

同步协议仍为版本 6, 服务端数据库与客户端本地状态库的表结构及客户端行为均未变化. 本次升级不涉及数据迁移, 服务端与客户端可以按任意顺序升级; 由于客户端没有 Web UI, 只有升级服务端二进制才会看到本次界面变化 (完整升级步骤见[用户手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级)):

- **服务端 (`kaze-syncd`)**: 编译 `v0.3.0` 二进制文件 ([手册 2.1 节](docs/manuals/user-manual.zh-CN.md#21-构建二进制)), 复制到服务器上, 以 root 执行 `./kaze-syncd install --data <同一目录>`. 原有的监听端口, TLS 证书和管理员密码均保持不变, `install` 会把新二进制复制进 `<data>/` 并重启服务.
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

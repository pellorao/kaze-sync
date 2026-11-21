# kaze-sync

[English](README.md) | [简体中文](README.zh-CN.md)

kaze-sync 是自托管且带版本历史的文件同步服务, 让你在多台 Linux 和 Windows 电脑之间同步任意多个本地文件夹, 同时在自己的硬件上保留历史版本. 一个小型服务端 (`kaze-syncd`) 运行在 OpenWrt 路由器上, 或使用 systemd 或 OpenRC 的普通 x86-64 Linux 机器上; Linux 和 Windows 上的轻量客户端 (`kaze-syncc`) 通过它保持各文件夹同步. 你不需要在每台电脑上分别改配置文件, 所有配置都在服务端的 Web UI 中集中完成.

这份 README 帮你判断 kaze-sync 是否适合你的需求, 并带你把第一套部署跑起来. 跑通之后, 各项日常操作都可以在[用户手册](docs/manuals/user-manual.zh-CN.md)中查到详细说明.

## kaze-sync 是什么, 不是什么

在动手构建和安装之前, 先看它的设计目标是否契合你的场景:

- **轻量.** 首要设计目标是低 CPU, 低内存, 低流量. 服务端在小数据量下约占 24 MB 内存, 在维护者自己约 37000 个文件的部署上约占 35 到 65 MB, 按 512 MB 内存的机器设计; 客户端与服务端空闲时几乎不占 CPU; 文件变化时只传输变化的 256 KiB 块.
- **每台设备多个文件夹的双向同步.** 每台电脑都可以同步任意多个目录, 例如 Windows 上的 `D:\Docs` 和 `E:\Photos`, Linux 上的 `~/Docs`. 因为按块增量传输, 约 1 GB 的 SQLite 数据库这类大文件也能高效同步.
- **不怕误操作.** 每次覆盖或删除都会在服务端保留旧内容 (默认 30 天), 因此存错或删错都能挽回: 你可以恢复某个文件的历史版本, 已删除的文件, 或把整个目录恢复到某一时刻. 两台电脑同时修改也不会丢数据: 后到达服务端的那一台会停止同步这部分内容, 把改动暂存起来, 直到你在 Web UI 中选择上传还是丢弃.
- **集中配置.** 文件夹, 各设备上的本地路径, 排除规则 (`.gitignore` 语法, 分全局和文件夹两层), 保留期, 通知和恢复都在同一个 Web UI 里完成. 客户端没有任何需要编辑的设置.
- **可从公网访问.** 不必额外部署 VPN 也能安全暴露到公网: 服务端默认使用随机高端口, 按指纹固定的自签名证书 (也可以换用你自己的证书, 或浏览器可信任的本地 CA), 随机生成的管理员密码以及失败锁定. 客户端只主动向外连接, 因此在 NAT 之后无需端口映射就能工作.

同样重要的是它不做什么: 它不是文件分享, 多用户或协作服务; 不是备份 (历史版本与数据在同一块盘上, 不过每个客户端都保有一份普通文件副本); 不提供浏览器端的文件预览或下载; 也不支持 macOS, 移动端或非 x86-64 架构的服务端. 完整列表见手册第 18 节.

Web UI 提供简体中文, 繁体中文, 英文, 日文和韩文五种界面语言, 默认跟随浏览器设置. 手册中的按钮和提示均与简体中文界面上的文字一致.

## 快速开始

如果以上特性符合你的需要, 只需五步就能装好服务端, 并让第一台电脑开始同步. 你需要一台能以 root 登录的服务器 (预编译包适用于 x86-64 Linux; 其他 CPU 请用 Go 从源码编译, 见[手册附录 A](docs/manuals/user-manual.zh-CN.md#附录-a-从源码编译)).

1. **下载并校验** (在服务器上): 从 [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) 页面下载 `kaze-sync-server-<version>-linux-amd64.sh` (服务端安装包, 约 36 MB, 内含两个客户端安装包) 和 `SHA256SUMS` 到同一个目录, 然后核对校验和:

   ```bash
   grep ' kaze-sync-server-<version>-linux-amd64.sh$' SHA256SUMS | sha256sum -c -
   ```

2. **安装服务端.** 以 root 运行安装包:

   ```bash
   sudo sh kaze-sync-server-<version>-linux-amd64.sh --data /var/lib/kaze-sync       # OpenWrt: 没有 sudo, 使用已挂载的数据盘, 例如 /mnt/data/kaze-sync
   ```

   它会安装并启动服务, 然后打印监听端口, 证书指纹, 以及只显示一次的管理员密码. 请在防火墙或云厂商安全组中放行该 TCP 端口, 使客户端和你的浏览器能够连接 (在 OpenWrt 上加 `--wan`, 安装包会替你完成; 见[手册 2.6 节](docs/manuals/user-manual.zh-CN.md#26-端口与防火墙规则)).

   以后升级时, 以 root 不带选项运行新的安装包即可. 它会沿用数据目录, 并保留现有的 OpenWrt WAN 规则. 见[手册第 15 节](docs/manuals/user-manual.zh-CN.md#15-升级). 如果你更愿意解压归档并自己运行 `kaze-syncd install`, 手册也有说明 ([2.1 节](docs/manuals/user-manual.zh-CN.md#21-用安装包安装服务端)).

3. **登录.** 在浏览器中打开 `https://<服务器>:<端口>/`, 用安装包打印的管理员密码登录 (忘记了可用 `/var/lib/kaze-sync/kaze-syncd admin-password` 生成新的, 数据目录按你的实际路径). 因为默认使用自签名证书, 浏览器会弹出证书警告; 核对指纹与安装时打印的一致后接受即可.
4. **注册客户端.** 登录 Web UI 后, 把第一台电脑接入服务端. 在总览页按 **添加设备** (`添加设备`), 输入设备名称, 再按 **生成注册码** (`生成注册码`). 对话框提供 Windows 的下载链接 (下载后双击运行; 如果 SmartScreen 提示发布者未知, 请点 **更多信息**, 再点 **仍要运行**); 对 Linux, 则提供一行命令, 可以粘贴到终端或 SSH 会话中, 在没有图形界面的机器上也能用:

   ```bash
   curl -fsS 'https://<服务器>:<端口>/dl/<CODE>/linux' | sh
   ```

   (服务端使用自签名证书时, 对话框中的命令会更长, 并固定服务端的公钥.) 安装程序会放好客户端, 注册设备, 并在每次登录时自动启动后台 agent; 你不需要再输入其他内容. 以后可以用 `kaze-syncc upgrade` 从你的服务端升级客户端.
5. **映射文件夹.** 设备注册后, 还需要把服务端文件夹与一个本机路径关联起来, 才会开始同步. 在总览页按 **新建文件夹** (`新建文件夹`). 然后打开该设备的页面, 按 **添加映射** (`添加映射`), 选择文件夹, 并输入这台电脑上的本机路径. 如果该目录里已有文件, 勾选 **允许接管非空目录** (`允许接管非空目录`). 保存后同步立即开始. 要接入更多电脑, 对每台电脑重复第 4 步和第 5 步即可.


## 接下来看什么

第一个文件夹开始同步后, 可以按需查阅以下文档:

- [发布说明](RELEASE-NOTES.zh-CN.md)与[变更记录](CHANGELOG.zh-CN.md): 当前版本包含什么, 如何测试, 有哪些已知限制.
- [用户手册](docs/manuals/user-manual.zh-CN.md): 日常操作的完整参考, 涵盖各平台的安装与升级, Web UI 每个页面的用法, 设备, 文件夹与映射管理, 排除规则, 写入控制与待处理改动, 通知与推送, Windows 上的占用文件, 恢复历史版本与目录, 导出普通文件, 保留期与清理, 服务端备份, 卸载以及常见问题排查.
- [`docs/operations.md`](docs/operations.md): 部署细节, 例如给只有一块盘的 OpenWrt 机器新建数据分区, systemd 加固和备份步骤.
- [`docs/design.md`](docs/design.md): 各部分为什么这样工作.

## 许可证

kaze-sync 以 [MIT 许可证](LICENSE) 发布. [`internal/server/web/fonts/`](internal/server/web/fonts/) 里随附的 Web UI 字体 (Adobe Source Sans 3, Source Serif 4, Source Han Sans, Source Han Serif 和 JetBrains Mono 的子集) 另按 SIL Open Font License 1.1 授权, 许可证文件和来源说明与字体放在一起 (见 [`SOURCE.md`](internal/server/web/fonts/SOURCE.md)).

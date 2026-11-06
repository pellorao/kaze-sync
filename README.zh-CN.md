# kaze-sync

[English](README.md) | [简体中文](README.zh-CN.md)

kaze-sync 是自托管且带版本历史的文件同步服务, 让你在多台 Linux 和 Windows 电脑之间同步任意多个本地文件夹, 同时在自己的硬件上保留历史版本. 一个小型服务端 (`kaze-syncd`) 运行在 OpenWrt 路由器上, 或使用 systemd 或 OpenRC 的普通 x86-64 Linux 机器上; Linux 和 Windows 上的轻量客户端 (`kaze-syncc`) 通过它保持各文件夹同步. 你不需要在每台电脑上分别改配置文件, 所有配置都在服务端的 Web UI 中集中完成.

这份 README 帮你判断 kaze-sync 是否适合你的需求, 并带你把第一套部署跑起来. 跑通之后, 各项日常操作都可以在[用户手册](docs/manuals/user-manual.zh-CN.md)中查到详细说明.

## kaze-sync 是什么, 不是什么

在动手构建和安装之前, 先看它的设计目标是否契合你的场景:

- **轻量.** 首要设计目标是低 CPU, 低内存, 低流量. 服务端正常运行时约占 24 MB 内存, 按 512 MB 内存的机器设计; 客户端与服务端空闲时几乎不占 CPU; 文件变化时只传输变化的 256 KiB 块.
- **每台设备多个文件夹的双向同步.** 每台电脑都可以同步任意多个目录, 例如 Windows 上的 `D:\Docs` 和 `E:\Photos`, Linux 上的 `~/Docs`. 因为按块增量传输, 约 1 GB 的 SQLite 数据库这类大文件也能高效同步.
- **不怕误操作.** 每次覆盖或删除都会在服务端保留旧内容 (默认 30 天), 所以存错或删错都能挽回: 你可以恢复某个文件的历史版本, 已删除的文件, 或把整个目录恢复到某一时刻. 两台电脑同时修改也不会丢数据. 后到服务端的那一台会停止同步这部分内容, 它的改动会一直等着, 直到你在 Web UI 中选择上传还是丢弃.
- **集中配置.** 文件夹, 各设备上的本地路径, 排除规则 (`.gitignore` 语法, 分全局和文件夹两层), 保留期, 通知和恢复都在同一个 Web UI 里完成. 客户端没有任何需要编辑的设置.
- **可从公网访问.** 不必额外部署 VPN 也能安全暴露到公网: 服务端默认使用随机高端口, 按指纹固定的自签名证书 (也可以换用你自己的证书, 或浏览器可信任的本地 CA), 随机生成的管理员密码以及失败锁定. 客户端只主动向外连接, 因此在 NAT 之后无需端口映射就能工作.

同样重要的是它不做什么: 它不是文件分享, 多用户或协作服务; 不是备份 (历史版本与数据在同一块盘上, 不过每个客户端都保有一份普通文件副本); 不提供浏览器端的文件预览或下载; 也不支持 macOS, 移动端或非 x86-64 架构的服务端. 完整列表见手册第 18 节.

Web UI 提供简体中文, 繁体中文, 英文, 日文和韩文五种界面语言, 默认跟随浏览器设置. 手册中的按钮和提示均与简体中文界面上的文字一致.

## 快速开始

如果以上特性符合你的需要, 只需五步就能装好服务端并让第一台电脑开始同步. 目前还没有预编译的发布包下载, 因此你需要一台装有 Go (版本见 `go.mod`) 的机器来编译二进制文件, 以及一台能以 root 登录的服务器.

1. **构建** (在任意机器上执行):

   ```bash
   CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncd ./cmd/kaze-syncd
   CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncc ./cmd/kaze-syncc
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kaze-syncc.exe ./cmd/kaze-syncc
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o kaze-syncc-agent.exe ./cmd/kaze-syncc
   ```

2. **安装服务端.** 把编译好的 `kaze-syncd` 复制到服务器, 以 root 执行以下两条命令:

   ```bash
   ./kaze-syncd install --data /var/lib/kaze-sync       # OpenWrt: 已挂载的数据盘, 如 /mnt/data/kaze-sync
   /var/lib/kaze-sync/kaze-syncd admin-password         # 只显示一次管理员密码
   ```

   `install` 会启动服务并打印监听端口和证书指纹. 请在防火墙或云厂商安全组中放行该 TCP 端口, 以便浏览器和客户端连接 (在 OpenWrt 上加 `install --wan` 可以自动放行).

3. **登录.** 在浏览器中打开 `https://<服务器>:<端口>/`, 用 `admin-password` 打印出的密码登录. 因为默认使用自签名证书, 浏览器会弹出证书警告; 核对指纹与 `install` 打印的一致后接受即可.
4. **注册客户端.** 登录 Web UI 后, 把第一台电脑接入服务端. 在总览页点 **添加设备**, 输入设备名称, 复制对话框里生成的命令. 把 `kaze-syncc` 或 `kaze-syncc.exe` 放到这台电脑的固定目录后执行该命令:

   ```bash
   kaze-syncc enroll https://<服务器>:<端口> <CODE> --fingerprint sha256:<HEX>
   ```

   这会向服务端注册该设备, 并将后台 agent 设为每次登录自动启动.
5. **映射文件夹.** 设备注册后还要把服务端文件夹与本机目录关联起来才会开始同步. 在总览页点 **新建文件夹**. 随后打开该设备的页面, 点 **添加映射**, 选择刚才建好的文件夹并输入本机路径. 如果该目录里已有文件, 勾选 **允许接管非空目录**. 保存后同步立即开始. 要接入更多电脑, 对每台电脑重复第 4 步和第 5 步即可.

## 接下来看什么

第一个文件夹开始同步后, 可以按需查阅以下文档:

- [用户手册](docs/manuals/user-manual.zh-CN.md): 日常操作的完整参考, 需要查某项操作怎么做时随时翻阅. 内容包括各平台的安装与升级, Web UI 每个页面的用法, 设备, 文件夹与映射管理, 排除规则, 写入控制与待处理改动的处理, 通知与推送, Windows 上的占用文件, 恢复历史版本与目录, 导出普通文件, 保留期与清理, 服务端备份, 卸载以及常见问题排查.
- [`docs/operations.md`](docs/operations.md): 部署细节, 例如给只有一块盘的 OpenWrt 机器新建数据分区, systemd 加固和备份步骤.
- [`docs/design.md`](docs/design.md): 各部分为什么这样工作.

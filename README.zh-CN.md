# kaze-sync

[English](README.md) | [简体中文](README.zh-CN.md)

kaze-sync 是自托管的、带版本历史的文件同步服务. 一个小型服务端 (`kaze-syncd`) 运行在 OpenWrt 上, 或使用 systemd 或 OpenRC 的普通 x86-64 Linux 上, Linux 和 Windows 上的客户端 (`kaze-syncc`) 通过它同步任意多个本地文件夹. 所有配置都在服务端的 Web UI 中完成.

这份 README 面向想知道 kaze-sync 是否适合自己, 并想先把第一套部署跑起来的读者. [用户手册](docs/manuals/user-manual.zh-CN.md)详细说明每一项操作.

## kaze-sync 是什么, 不是什么

它是:

- **轻量.** 首要设计目标是低 CPU, 低内存, 低流量. 服务端正常运行时约占 24 MB 内存, 按 512 MB 内存的机器设计; 两端空闲时几乎不占 CPU; 只传输变化的 256 KiB 块.
- **每台设备多个文件夹的双向同步**, 例如 Windows 上的 `D:\Docs` 和 `E:\Photos`, Linux 上的 `~/Docs`. 约 1 GB 的 SQLite 数据库这类大文件也能高效同步.
- **不怕误操作.** 每次覆盖或删除都会在服务端保留旧内容 (默认 30 天). 可以恢复某个文件的某个版本, 已删除的文件, 或把整个目录恢复到某一时刻. 并发修改不会丢数据: 后到的修改成为冲突副本.
- **集中配置.** 文件夹, 各设备上的路径, 排除规则 (`.gitignore` 语法, 全局和按文件夹两层), 保留期, 通知和恢复都在同一个 Web UI 里. 客户端没有需要编辑的设置.
- **可从公网访问.** 服务端使用随机高端口, 按指纹固定的自签名证书 (或你自己的证书), 随机的管理员密码和失败锁定. 客户端只主动向外连接, 在 NAT 之后也能工作.

它不是: 文件分享, 多用户或协作服务; 备份 (版本与数据在同一块盘上, 不过每个客户端都有普通文件副本); 带浏览器文件查看或下载的服务; macOS, 移动端或非 x86-64 服务端的方案. 完整列表见手册第 18 节.

## 快速开始

构建需要 Go (版本见 `go.mod`), 以及一台能以 root 登录的服务器. 目前没有发布下载.

1. **构建** (在任意机器上):

   ```bash
   CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncd ./cmd/kaze-syncd
   CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncc ./cmd/kaze-syncc
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kaze-syncc.exe ./cmd/kaze-syncc
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o kaze-syncc-agent.exe ./cmd/kaze-syncc
   ```

2. **安装服务端.** 把 `kaze-syncd` 复制到服务器, 以 root 执行:

   ```bash
   ./kaze-syncd install --data /var/lib/kaze-sync       # OpenWrt: 已挂载的数据盘, 如 /mnt/data/kaze-sync
   /var/lib/kaze-sync/kaze-syncd admin-password         # 只显示一次管理员密码
   ```

   `install` 会打印端口和证书指纹. 请在防火墙或云厂商安全组中放行该 TCP 端口 (OpenWrt 上 `install --wan` 可以代劳).

3. **登录.** 打开 `https://<服务器>:<端口>/`, 用管理员密码登录. 浏览器会警告自签名证书, 核对指纹后接受.
4. **注册客户端.** 在总览页点 **添加设备**, 输入名称, 复制对话框里的命令. 把 `kaze-syncc` 或 `kaze-syncc.exe` 放在固定目录后, 在这台电脑上执行:

   ```bash
   kaze-syncc enroll https://<服务器>:<端口> <CODE> --fingerprint sha256:<HEX>
   ```

   这会注册设备, 并设为每次登录自动启动 agent.
5. **映射文件夹.** 在总览页点 **新建文件夹**. 打开设备页, 点 **添加映射**, 选择文件夹并输入本机路径. 目录里已有文件时, 勾选 **允许接管非空目录**. 同步立即开始. 其他电脑重复第 4 步和第 5 步.

## 接下来看什么

- [用户手册](docs/manuals/user-manual.zh-CN.md): 各平台的安装与升级, Web UI 每个页面, 设备, 文件夹与映射, 排除规则, 冲突, 通知, Windows 上的占用文件, 恢复版本与目录, 导出, 保留期, 备份, 卸载与排查问题.
- [`docs/operations.md`](docs/operations.md): 部署细节, 例如给只有一块盘的 OpenWrt 机器新建数据分区, systemd 加固和备份步骤.
- [`docs/design.md`](docs/design.md): 各部分为什么这样工作.

# 部署, 备份与升级

依据 `docs/design.md` 第 7 节与第 9 节; 这里只写操作步骤. 服务端支持 OpenWrt (数据放在单独挂载的数据盘上) 和使用 systemd 或 OpenRC 的普通 x86-64 Linux (如 VPS, 数据可以放在根文件系统上); `install` 自动识别服务管理器, 没有选择参数.

## 部署

在开发机上构建 (静态链接, 无依赖):

```
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncd ./cmd/kaze-syncd
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncc ./cmd/kaze-syncc
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kaze-syncc.exe ./cmd/kaze-syncc
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o kaze-syncc-agent.exe ./cmd/kaze-syncc
```

`kaze-syncc-agent.exe` 必须与 `kaze-syncc.exe` 放在同一目录, 自启动才无窗口. 复制到客户端前检查它的 PE 子系统, 输出 `2` (图形界面) 才可部署, `3` (控制台) 会在每次登录时弹出可见窗口:

```
python3 -c "import struct,sys; d=open(sys.argv[1],'rb').read(4096); print(struct.unpack_from('<H', d, struct.unpack_from('<I', d, 60)[0]+92)[0])" kaze-syncc-agent.exe
```

### OpenWrt

在路由器上以 root 执行 (数据盘须已挂载, 数据目录不能在 overlay 上; 只有一块盘时见下一节):

```
./kaze-syncd install --data /mnt/data/kaze-sync        # 可选 --listen :PORT, --cert F --key F, --wan
/mnt/data/kaze-sync/kaze-syncd admin-password          # 显示一次随机密码
```

### 普通 Linux (systemd 或 OpenRC)

以 root 执行, 数据目录自选, 可以在根文件系统上:

```
./kaze-syncd install --data /var/lib/kaze-sync        # 可选 --listen :PORT, --cert F --key F; 不支持 --wan
/var/lib/kaze-sync/kaze-syncd admin-password
```

- `install` 创建系统用户 `kaze-sync` (`useradd`, Alpine 用 busybox `adduser`), 写入并启用服务 (systemd: `/etc/systemd/system/kaze-sync.service`; OpenRC: `/etc/init.d/kaze-sync`, runlevel `default`) 并启动. 不配置防火墙, 也没有 `--wan`: 在云厂商的安全组或本机防火墙 (如 `ufw allow <端口>/tcp`, `firewall-cmd --add-port=<端口>/tcp --permanent`) 中放行 `install` 打印的 TCP 端口.
- 启停与状态: systemd 用 `systemctl start|stop|restart|status kaze-sync`, OpenRC 用 `rc-service kaze-sync start|stop|restart|status`. 进程退出后 5 秒自动重启 (OpenRC 的 `supervise-daemon` 在 30 分钟内连续重启 5 次后放弃, systemd 不限次数); 开机自动启动.
- 日志: systemd 用 `journalctl -u kaze-sync`, OpenRC 经 `logger` 进 syslog (Alpine 在 `/var/log/messages`, 标签 `kaze-sync`). 下文 `logread` 的位置都按此替换.
- 数据目录所在的盘 (若是单独的挂载点) 未挂载时服务启动失败而不是新建空库, 与 OpenWrt 相同.
- systemd 单元带加固选项 (2026-11-04 在 Ubuntu 24.04 上实测, `systemd-analyze security kaze-sync` 的暴露分从 9.2 降到 1.3): 服务只能写 `<数据目录>/var` (`ProtectSystem=strict` 加 `ReadWritePaths=<数据目录>/var`), 看不到 `/home`, `/root`, `/run/user` (`ProtectHome=yes`), 独占 `/tmp` 和 `/dev`, 不能提权, 不能改内核参数, 时钟和主机名, 只能建 TCP/IP 与本地套接字, 没有任何 capability, 系统调用限于 `@system-service` 并去掉 `@privileged`, `@resources`. 服务本来就只写 `var/`, 也不发起对外连接, 所以正常使用不受影响. 数据目录, `--cert` 或 `--key` 在 `/home`, `/root` 或 `/run/user` 之下时, `install` 自动改用 `ProtectHome=read-only` (仍只有 `var/` 可写); 其余选项照常. 再次执行 `install` 会重写这个单元文件, 自己的改动不要直接改它, 用 drop-in: `systemctl edit kaze-sync`, 在 `[Service]` 下写覆盖项 (例如让服务多写一个目录: `ReadWritePaths=/mnt/extra`; 排查问题时临时放开: `ProtectSystem=` 置空, 或 `SystemCallFilter=` 置空以清除过滤器), 保存后 `systemctl restart kaze-sync`. 主要选项 (`ProtectSystem=strict` 等) 需要 systemd 231 以上, 较新的选项 (如 `ProtectClock` 245, `ProtectProc` 247) 在老版本被忽略并在日志里留下警告, 加固程度相应降低. OpenRC 和 OpenWrt 的 procd 没有这类选项, 只靠专用用户和 `var/` 的属主隔离.

### 首次登录与资源

`install` 打印端口和证书指纹. 用浏览器打开 `https://<服务器>:<端口>/`, 用密码登录; 在总览页 "添加设备" 得到注册命令, 在客户端上执行. 之后的配置都在 Web UI 中进行.

服务端的资源: 常驻内存小数据量下约 24 MB (M3 实测), 维护者自己约 37000 个文件的部署上 35 到 65 MB (2026-11 观察期). GC 时另占内存, 与常驻内存分开算: 它按块引用数分配, 每个引用 32 字节 (排序数组), 上限是内存预算 `GCMemoryBudget` = max(64 MB, min(MemTotal, cgroup 限额) x 3/8), 进程启动时算一次 (设计 7 节). 引用数 = 所有当前文件与保留期内历史版本的块数之和, 约为 文件数 x (1 + v) x 平均块数, v 为保留期内每个文件的平均历史版本数, 随 `keep_days` 与文件改动频率增大 (默认 30 天, 每周改一次的文件 v 约 4, 每天改的约 30). 按 v = 2 (假设, 未实测), 每个文件 1 块估算: 1 万文件约 1 MB, 10 万约 10 MB, 100 万约 96 MB (512 MB 机器预算 192 MB, 均单趟), 500 万约需 480 MB, 受预算限制分 3 趟. 超过预算只是 GC 多扫几趟 (最多 256 趟), 不会失败, 内存不变. 大文件每 256 KiB 一块 (1 GB 文件每个版本 4096 块). GC 日志 (`msg=gc`) 带 `block_refs`, 用它除以文件数可得实际的 (1 + v) x 块数. 完整说明与表见用户手册 13 节.

## OpenWrt 只有一块盘: 新建数据分区

`install` 拒绝根文件系统 (overlay) 上的数据目录. 只有一块盘时, 在同一块盘上新建一个分区作数据盘 (以 x86 镜像, 盘 `/dev/sda` 为例; 先备份配置, 分区操作写错会毁掉系统):

1. 安装工具: `opkg update && opkg install block-mount e2fsprogs fdisk` (OpenWrt 25.12 及以后用 `apk add`).
2. 在盘上腾出空闲空间: 虚拟机在宿主上扩大虚拟磁盘; 物理机把镜像写入时就选更大的盘, 或把镜像写到较大的盘上. 空闲空间位于已有分区之后.
3. `fdisk /dev/sda`: 输入 `n` 新建分区 (分区号取默认的 3, 起止取默认, 用满空闲空间), 再输入 `w` 写入 (盘变大后 GPT 备份表的位置提示会在写入时一并修正). 然后 `reboot` 让内核重读分区表.
4. 格式化并登记挂载:

```
mkfs.ext4 -L kaze-data /dev/sda3
block detect | uci import fstab
uci set fstab.@mount[-1].target=/mnt/data
uci set fstab.@mount[-1].enabled=1
uci commit fstab
/etc/init.d/fstab enable
mkdir -p /mnt/data && block mount
mount | grep /mnt/data                 # 确认已挂载
```

之后按上文 "OpenWrt" 一节用 `--data /mnt/data/kaze-sync` 安装. sysupgrade 不会改动这个分区 (`fstab` 是 conffile, 升级后保留); 但升级用的镜像写入的是系统分区, 不要用整盘镜像 (如 `dd` 整盘) 的方式升级, 否则数据分区会被覆盖.

## 版本保留, GC 与删除

- 每次覆盖或删除都留一个旧版本, 保留 `keep_days` 天 (总览页修改, 默认 30). 保存后立即触发一次 GC, 不需要重启.
- GC 每天一次 (服务启动一分钟后检查, 距上次不足一天则顺延), 先删除超出保留期的版本, 再删除无人引用且超过 1 小时的块, 并清理 `var/tmp/` 中超过 1 小时的残留. 刚上传尚未提交的块受 1 小时宽限保护. GC 在后台运行, 不影响同步.
- GC 结果记在日志里 (OpenWrt: `logread`, 其他系统见上文 "日志"): `logread | grep 'msg=gc '` 显示删除的版本数, 块数, 趟数和耗时.
- 文件夹页的 "恢复目录到某一时刻": 填写 UTC 时间, 当前目录下在该时刻存在而现在不同或已删除的文件都恢复成当时的内容; 之后新建的文件不动, 不删除任何文件. 只有仍在 `keep_days` 内的版本才能恢复.
- 文件夹页的 "删除文件夹" 只在没有设备映射它时可用 (先在各设备页删除映射); 它的库 `var/folders/<id>.db` 随之删除, 块在下次 GC 回收.

## 备份与还原

必须备份 `var/meta.db`, `var/folders/` 和 `var/blobs/`; 另外建议备份 `var/tls/` (自签名证书和私钥, 很小) 以及本地 CA 模式下的 `<data>/ca/` (CA 私钥): 没有它们, 还原后会生成新证书或新 CA, 指纹变化, 全部客户端需要重新注册. 备份放在另一块盘或另一台机器上.

备份 (以 `/mnt/data/kaze-sync` 与目标目录 `/backup` 为例), 避开 GC 运行的时刻; 下面写 OpenWrt 的启停命令, systemd 换成 `systemctl stop|start kaze-sync`, OpenRC 换成 `rc-service kaze-sync stop|start`:

```
/etc/init.d/kaze-sync stop
cd /mnt/data/kaze-sync/var && tar cf /backup/meta.tar meta.db folders tls     # 停服务后 WAL 已合并
/etc/init.d/kaze-sync start                                                   # 停机只有几秒钟
tar cf /backup/blobs.tar blobs                                                # 块写入后不再修改, 可增量复制
tar cf /backup/ca.tar -C /mnt/data/kaze-sync ca                              # 仅本地 CA 模式
```

还原到新机器 (新数据盘已挂载, 已有同一版本或更新版本的 `kaze-syncd`):

```
mkdir -p /mnt/data/kaze-sync/var
tar xf /backup/meta.tar -C /mnt/data/kaze-sync/var
tar xf /backup/blobs.tar -C /mnt/data/kaze-sync/var
tar xf /backup/ca.tar    -C /mnt/data/kaze-sync                              # 仅本地 CA 模式
./kaze-syncd export /tmp/check --data /mnt/data/kaze-sync       # 可选: 先确认数据可读, 见下
./kaze-syncd install --data /mnt/data/kaze-sync --new-epoch      # 修正属主, 更换历史标识, 启动服务
```

`--new-epoch` 必须在还原备份时使用 (原地回滚到旧备份同样): 还原使服务端的版本号回到过去, 它让各客户端放弃已知的服务端状态并从头读取一次 (设计 5.5). 客户端不需要任何操作; 备份之后在客户端上新增的文件会重新上传; 同一路径上内容与备份不同的文件, 该文件成为待处理改动 (原因 `reset`, 不会丢失), 在 Web UI 的处理框中选上传, 上传并覆盖或丢弃 (设计 2.8.9); 备份之后在客户端上删除的文件会被取回; 内容一致的无变化. 不带该选项时客户端只能发现版本号回退了的文件夹, 其余可能静默不一致. 平时升级执行 `install` 不要带它.

`install` 把 `var/` 下的目录, 数据库和证书交给服务用户 (块文件保持 root 所有, 只读即可); 管理员密码, 设备和映射都在 `meta.db` 里, 客户端不需要重新注册, 也不必重启 agent. 因为 `meta.db` 与各文件夹库必须取自同一时刻, 备份时不要只还原其中一部分.

## 导出为普通文件

服务端磁盘上没有原始目录树; 灾难恢复和迁出用 `export`, 它直接读数据库和块, 服务停着也能用:

```
kaze-syncd export DIR [--data DIR] [--folder NAME|ID] [--at "2026-11-02 12:00[:00]"]
```

不带 `--folder` 时每个文件夹导出到 `DIR/<名称>`; 带 `--folder` 时直接导出到 `DIR`. 不带 `--at` 导出当前状态, 带 `--at` 导出 UTC 该时刻的状态 (与目录恢复同一套查询). 每个文件写完后校验哈希; 缺块或损坏的文件报告并跳过, 其余照常导出, 命令以非零状态退出. 文件内容, 可执行位和修改时间与客户端上的一致.

## 发布与版本号

版本号遵循 Semantic Versioning (`MAJOR.MINOR.PATCH`), 只有一个来源: `internal/version/VERSION` (构建时嵌入, 服务端和客户端共用). 它显示在管理页左上角 `Kaze Sync <版本号>` (登录页不显示, 未登录的人不必知道版本), 也可用 `kaze-syncd --version` 和 `kaze-syncc --version` 查看. 在本工程里 "发布" 就是部署到正式服务端的那次构建, 每次发布变更版本号:

- 升哪一位: 只含修复升 PATCH; 有用户可见的新功能升 MINOR; 1.0.0 之前, 协议或数据不兼容的变化也升 MINOR; 1.0.0 之后不兼容的变化升 MAJOR.
- 流程: 改 `VERSION` -> 把 `CHANGELOG.md` / `CHANGELOG.zh-CN.md` 的 "Unreleased" / "未发布" 一节改为 `## vX.Y.Z - YYYY-MM-DD` 并补全, 重写 `RELEASE-NOTES.md` / `RELEASE-NOTES.zh-CN.md` (标题 `# kaze-sync vX.Y.Z release notes` / `# kaze-sync vX.Y.Z 发布说明`; 旧版本的发布说明留在它的 tag 里) -> `go test ./internal/version` (核对 `VERSION` 与四份文档最新一条一致) 和相应的全部测试 -> 提交 -> 打 tag `vX.Y.Z` -> 构建并部署 (见 "部署" 和 "升级") -> 部署后核对 (服务状态, 日志, 抽查一次恢复, `.kaze-test` 双向同步; 登录后的页面左上角显示新版本号, 登录页不显示 (静态资源不含版本号), 不用登录就能核对的是服务端二进制的 sha256 和 `--version` 输出).
- 两次发布之间的提交不改 `VERSION`; 它们的用户可见改动先记在 changelog 的 "Unreleased" 一节.

每次发布在 tag 对应的源码根目录运行 `go run ./tools/release`, 按 `internal/version/VERSION` 生成 `dist/vX.Y.Z/` (Git 忽略): `kaze-sync-X.Y.Z-linux-amd64.tar.gz` 含服务端与客户端, `kaze-sync-X.Y.Z-windows-amd64.zip` 含客户端及无窗口 agent, 另附 `SHA256SUMS`. 使用本机 Go 工具链与已缓存模块, 缺依赖即停止, 不联网下载; 首次运行 `go run` 本身也可设置 `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off`.
构建固定 `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false` 和链接参数, 归档按文件名排序并固定时间戳及权限; 同一源码与同一 Go 工具链可重编后比对 `SHA256SUMS` (校验用 `sha256sum -c SHA256SUMS`). 旧 tag 可用 `go run ./tools/release -source <源码目录> -out <产物目录>` 补包.
命令最后仅打印带 `--verify-tag` 的 `gh release create` 上传命令, 不执行; 维护者核对后将包与校验和上传到公开 GitHub 仓库的 Releases, 本地 `dist/` 不提交. 部署使用这次打包的二进制.

## 升级

服务端: 用新二进制再执行一次 `kaze-syncd install --data <同一目录>` (可重复执行, 端口, 证书和密码不变); 它复制新二进制并重启服务. 库表迁移 (按 `schema_version` 逐库, 先全局库, 再每个文件夹库) 在服务启动时自动进行, `install` 也会在重启前执行同一迁移; 版本低于当前的库先被复制到 `var/backups/` (`<库>-v<旧版本>-<时间>.db`, 每个库留最近 3 份), 迁移失败时服务不启动, 日志和 `install` 输出指明原因, 库不被改动. 回滚见设计 9.3 "库表迁移"; 回滚客户端二进制前先备份该客户端的各 `folder-*.db` (在客户端的状态目录: Linux 是 `~/.local/state/kaze-sync`, Windows 是 `%LOCALAPPDATA%\kaze-sync`; 先停 agent 再复制), 回滚时和二进制一起还原. 先升级服务端, 再逐台升级客户端: 替换客户端二进制 (Windows 是两个 exe), 重启 agent (Linux: `systemctl --user restart kaze-sync`; Windows: 注销再登录, 或结束 `kaze-syncc-agent.exe` 后重新运行). 服务端升级后, 版本过旧 (低于服务端的 `MinAgentVersion`, 设计 9.3) 的客户端不再同步: 它的日志 (Linux 在 `~/.local/state/kaze-sync/kaze-syncc.log`) 里每分钟一条 `client too old ... upgrade kaze-syncc`, 总览页该设备的版本列显示 "发布版本 (旧客户端不上报时为 `-`) 加红色 "过旧""; 按上面的步骤替换这台客户端的二进制并重启 agent 即可, 数据不受影响, 升级后自动继续同步. 只有协议变得不兼容的改动才会提高 `MinAgentVersion`, 其余升级不要求同步升级客户端. OpenWrt 自身的 `sysupgrade` 不影响数据和服务 (设计 9.1). 普通 Linux 上执行 `install` 同样会复制新二进制, 迁移数据库 (同上, 先备份) 并重启服务 (systemd 先 `daemon-reload`).

### Web Push 的运维

- 服务端向浏览器厂商的推送服务发出站 HTTPS (443, 需要 DNS); 出站被限制的路由器要放行. 只有订阅了的浏览器才会产生这类连接.
- VAPID 私钥和订阅存在全局库 `meta` (`vapid_key`, `push_subs`), 随库一起备份与还原; 还原到不同的 VAPID 密钥后, 已有订阅会被推送服务拒绝 (401/403), 要在各浏览器重新开启推送.
- 升级到带 Web Push 的版本后, 数据目录里留下的 `ntfy.json` 不再被读取 (启动日志一行 info), 可删.
- 切换到本地 CA 或证书文件模式后, 浏览器重新信任证书才能注册 Service Worker (用户手册 2.4); 无需改 VAPID.
- `-wc-local` 的演练用本机假推送服务收推送 (`METRIC` 行里的 "web push"); 对远端服务端不设假推送服务, 延迟用设置页的测试推送测.

## 用脚本检查服务器状态

`go run ./tools/adminstatus` 以管理员身份登录 Web UI (`POST /login`, 与浏览器相同), 取总览页和 `/push/subscriptions`, 打印服务端版本, 各设备的在线状态, 最近连接, 客户端版本, "版本过旧" 标记, 以及通知 (Web Push) 的证书模式和各订阅的状态; 给自动化 (定期巡检) 用, 不必开浏览器.

```
go run ./tools/adminstatus -setup                      # 一次: 从标准输入读密码, 存入 ~/.config/kaze-sync/admin-password (0600)
go run ./tools/adminstatus -url https://HOST:PORT [-ca CA.pem | -fp sha256:HEX] [-expect 0.4.0] [-json] [-rename OLD=NEW]
```

- 密码只从密码文件读 (`-pwfile` 改路径; 文件必须是普通文件且权限为 0600, 否则拒绝), 不进命令行, 环境变量, 输出或日志. `-setup` 在终端上关闭回显.
- 证书: 本地 CA 模式用 `-ca` 给出 `kaze-syncd ca --export -` 导出的 CA 证书; 自签名用 `-fp` 钉住证书指纹; 都不给则按系统信任库. `-url`, `-fp`, `-ca` 也可由 `KAZE_STATUS_URL`, `KAZE_STATUS_FP`, `KAZE_STATUS_CA` 提供.
- 检查项 (任何一项不过则退出码 1 并在结果里列出): 设备离线, 设备被标 "版本过旧", `-expect` 给出时服务端或任一设备版本不符, 通知接口读不出, 订阅失效 (`gone`) 或最近一次推送状态不是 2xx. 无法登录或读取为退出码 2 (密码错误会计入登录失败锁定, 脚本不重试).
- `-rename OLD=NEW`: 登录后先把总览上名为 `OLD` 的设备改名为 `NEW` (与设备页 "设备改名…" 同一个表单, design 2.5), 再按上面检查并打印; 名字不合规或重名时退出码 2, 并给出页面上的错误. 给不开浏览器又不能把密码交给别人的场合用 (例如由自动化代管理员改名).
- 自签名证书模式下 Web Push 本来就不可用, 只显示 `available=false`, 不算问题. `-setup` 的关回显只在有 `stty` 的系统上生效. 页面改版会使解析失败: 找不到服务端版本或设备时报告为问题, 不静默通过.

## 客户端调试日志

默认日志只记启动, 警告和错误. 诊断同步延迟时, 在客户端状态目录 (Linux `~/.local/state/kaze-sync`, Windows `%LOCALAPPDATA%\kaze-sync`) 里创建一个名为 `debug` 的空文件并重启 agent, 日志 (`kaze-syncc.log`) 就多出 `level=DEBUG` 的时间线: 文件事件 (`watcher event`), 去抖到点后的任务 (`task planned`), 扫描 (`scan`) 与拉取 (`pull`) 的耗时和结果, 到服务端的请求 (`poll`, `server commit`, `upload blocks`, `changes`, `download blocks`, 带耗时 `ms`) 和轮询带来的通知 (`poll news`). 用完删掉该文件并重启 agent; 日志仍受 10 MB 轮转限制.

## 客户端卸载 (手动)

- Linux: `systemctl --user disable --now kaze-sync`, 删除 `~/.config/systemd/user/kaze-sync.service`, 删除状态目录 `~/.local/state/kaze-sync` (含日志); 同步目录里的文件保留.
- Windows: 删除注册表项 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 中的 `kaze-sync`, 结束 `kaze-syncc-agent.exe`, 删除 `%LOCALAPPDATA%\kaze-sync`; 同步目录里的文件保留.
- 然后在 Web UI 的设备页吊销该设备.

## 写入控制

依据 `docs/design.md` 2.8; 用户怎么用见用户手册 5.1 节. 这里写运维相关的几点.

### 待处理清单在服务端内存里

设备上有哪些改动待处理 (冻结的范围), 以及 Web UI 提交但设备还没执行的决定, 都只存在服务端进程的内存里, 不写数据库. 服务端重启 (升级, 崩溃, 还原备份) 后:

- 每个设备的下一次长轮询会收到新的 `boot` 标识, 发现服务端换了一次运行, 就把各文件夹的待处理清单重新发给服务端. 真实的状态在设备的本地库 (`held` 表) 里, 所以不会丢. 重发之前 (通常几秒), Web UI 的通知栏和处理框是空的.
- 还没被设备取走或执行的决定丢失. 待处理改动仍在设备上, Web UI 再次显示为未处理, 重新点一次即可. 用 `-wc` 演练时不要在中途重启服务端.
- 每个文件夹每台设备最多保存 20000 条 (`maxHeldItems`), 超出部分不保存, 但设备那一侧不丢; 处理框每个范围最多画 2000 行, 其余用整范围的按钮处理.
- 只有设备才知道清单是否清空: 设备执行完决定后重新上报, 服务端据此清除对应的决定. 设备离线时决定一直等着, 超过 10 分钟后该行在 Web UI 上重新显示为可选.

### 单端写入时间窗 N

N 存在全局库 `meta` 的 `write_window` (秒), 默认 60, 即 1 分钟; Web UI 设置页的 "单端写入时间窗" 改它 (0 到 60 分钟, 0 表示不用时间窗). 它只影响服务端, 不改变 `config_version`, 设备不必重新扫描. 日常使用不需要改它. 演练驱动需要 N 很小才能在几分钟内走完, 所以它在开始时把 N 设为 1 分钟 (默认值本来就是 1 分钟, 若你改过则会被临时改掉), 结束时还原为开始前读到的值. 驱动被 `kill -9` 或断电打断时不会还原: 在设置页自己改回去.

### 演练驱动 `-wc`

`e2e/wc.go` 是写入控制的真机场景组 (设计 2.8.12): 用真实的 `kaze-syncc` 进程和一个运行中的服务端走一遍 "新文件夹提示, 交接, 同时写入, 离线编辑, 恢复后写入, 独占误写, git" 等场景, 并检查两端与服务端的哈希. 它不碰真实数据: 自己建设备 `wct-a`, `wct-b` 和文件夹 `wct-<场景>`, 结束时删掉.

```
KAZE_E2E_PASSWORD='<管理员密码>' go run ./e2e -wc https://<服务器>:<端口> -wc-fp sha256:<指纹>
go run ./e2e -wc-local                       # 本机一次性服务端加两个 Linux 客户端, 试驱动本身
```

- `-wc URL`: 对运行中的服务端演练. 管理员密码只从环境变量 `KAZE_E2E_PASSWORD` 读 (不要写进命令行历史); 不带 `-wc-fp` 时要求服务端证书被系统信任. 不需要 `e2e.json`, 也不起 VM.
- `-wc-win-dir 'C:\x\wct'`: 把 B 端换成 Windows 客户端, 目录是 Windows 盘上的路径 (Windows 写法), 驱动把 `wct.exe` 和 `wct-agent.exe` 复制进去. 不带时 A, B 都是本机的 Linux 客户端.
- `-wc-only 文本`: 只跑名称含该文本的场景, 例如 `-wc-only hand-over`.
- `-wc-local`: 用进程内的一次性服务端 (本机 127.0.0.1) 自测, 约 8.5 分钟, 全程不碰任何真实服务端.
- 全部通过时输出 `ALL PASSED`, 否则退出码 1, 并打印失败的场景; 失败时在 `/tmp/wc-run-*` 留下两端日志和处理框的页面快照 (`held-*.html`), 通过时这个目录被删除. 输出里的 `METRIC` 行是从写入到处理框出现的延迟和处理所需的点击次数.
- 驱动没有信号处理: 用 Ctrl-C 或 `kill` 中断后它不会清理, 见下面的清理.

驱动启动的 agent 进程在正常结束时由它停掉. 它们的状态目录在上面的 `/tmp/wc-run-*` 里.

### `wct-*` 的清理

正常结束时驱动自己做: 把时间窗还原, 停两个 agent, 删除两端对每个 `wct-*` 文件夹的映射, 删除这些文件夹, 吊销 `wct-a` 和 `wct-b`. 被打断或崩溃后服务端上会留下 `wct-a`, `wct-b` 和若干 `wct-*` 文件夹, 手工清理:

1. 确认没有驱动还在跑 (`pgrep -af 'e2e.*-wc'`, 以及留下的 `kaze-syncc` agent 进程, 命令行里带 `wct` 的状态目录), 把它们结束掉.
2. Web UI 设置页把 "单端写入时间窗" 改回你要的值.
3. 设备页: 删除 `wct-a`, `wct-b` 对各 `wct-*` 文件夹的映射, 再吊销这两台设备 (吊销会提示受影响的独占, 这里的独占都是演练建的).
4. 总览页: 逐个删除名为 `wct-*` 的文件夹 (没有映射时才能删).
5. 删除 `/tmp/wc-run-*` 和 Windows 端的 `-wc-win-dir` 目录.

演练中被丢弃的文件在各端同步根的 `.syncsaved/` 里, 目录随 `/tmp/wc-run-*` 一起删除; 真实同步目录里的 `.syncsaved` 由扫描清除 30 天前的时间目录, 不需要手工清理.

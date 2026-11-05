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

`kaze-syncc-agent.exe` 必须与 `kaze-syncc.exe` 放在同一目录, 自启动才无窗口.

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

服务端的资源: 常驻内存约 24 MB; GC 时额外占用约每百万个块引用 32 MB 除以趟数 (设计 7 节的表). 512 MB 内存的机器处理 512 GB 以内的数据单趟完成, 更大的数据只是 GC 多扫几遍, 内存不变.

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

`--new-epoch` 必须在还原备份时使用 (原地回滚到旧备份同样): 还原使服务端的版本号回到过去, 它让各客户端放弃已知的服务端状态并从头读取一次 (设计 5.5). 客户端不需要任何操作; 备份之后在客户端上新增的文件会重新上传; 同一路径上内容与备份不同的文件, 客户端版本变成冲突副本 (不会丢失), 取备份中的版本; 备份之后在客户端上删除的文件会被取回; 内容一致的无变化. 不带该选项时客户端只能发现版本号回退了的文件夹, 其余可能静默不一致. 平时升级执行 `install` 不要带它.

`install` 把 `var/` 下的目录, 数据库和证书交给服务用户 (块文件保持 root 所有, 只读即可); 管理员密码, 设备和映射都在 `meta.db` 里, 客户端不需要重新注册, 也不必重启 agent. 因为 `meta.db` 与各文件夹库必须取自同一时刻, 备份时不要只还原其中一部分.

## 导出为普通文件

服务端磁盘上没有原始目录树; 灾难恢复和迁出用 `export`, 它直接读数据库和块, 服务停着也能用:

```
kaze-syncd export DIR [--data DIR] [--folder NAME|ID] [--at "2026-11-02 12:00[:00]"]
```

不带 `--folder` 时每个文件夹导出到 `DIR/<名称>`; 带 `--folder` 时直接导出到 `DIR`. 不带 `--at` 导出当前状态, 带 `--at` 导出 UTC 该时刻的状态 (与目录恢复同一套查询). 每个文件写完后校验哈希; 缺块或损坏的文件报告并跳过, 其余照常导出, 命令以非零状态退出. 文件内容, 可执行位和修改时间与客户端上的一致.

## 升级

服务端: 用新二进制再执行一次 `kaze-syncd install --data <同一目录>` (可重复执行, 端口, 证书和密码不变); 它复制新二进制并重启服务. 库表迁移 (按 `schema_version` 逐库, 先全局库, 再每个文件夹库) 在服务启动时自动进行, `install` 也会在重启前执行同一迁移; 版本低于当前的库先被复制到 `var/backups/` (`<库>-v<旧版本>-<时间>.db`, 每个库留最近 3 份), 迁移失败时服务不启动, 日志和 `install` 输出指明原因, 库不被改动. 不带 root 的部署 (用户级服务) 换二进制并重启即可, 不需要手工改库. 回滚见设计 9.3 "库表迁移"; 回滚客户端二进制前先备份该客户端的各 `folder-*.db` (在客户端的状态目录: Linux 是 `~/.local/state/kaze-sync`, Windows 是 `%LOCALAPPDATA%\kaze-sync`; 先停 agent 再复制), 回滚时和二进制一起还原. 先升级服务端, 再逐台升级客户端: 替换客户端二进制 (Windows 是两个 exe), 重启 agent (Linux: `systemctl --user restart kaze-sync`; Windows: 注销再登录, 或结束 `kaze-syncc-agent.exe` 后重新运行). 服务端升级后, 版本过旧 (低于服务端的 `MinAgentVersion`, 设计 9.3) 的客户端不再同步: 它的日志 (Linux 在 `~/.local/state/kaze-sync/kaze-syncc.log`) 里每分钟一条 `client too old ... upgrade kaze-syncc`, 总览页该设备的版本列显示 "N (过旧, 请升级客户端)"; 按上面的步骤替换这台客户端的二进制并重启 agent 即可, 数据不受影响, 升级后自动继续同步. 只有协议变得不兼容的改动才会提高 `MinAgentVersion`, 其余升级不要求同步升级客户端. OpenWrt 自身的 `sysupgrade` 不影响数据和服务 (设计 9.1). 普通 Linux 上执行 `install` 同样会复制新二进制, 迁移数据库 (同上, 先备份) 并重启服务 (systemd 先 `daemon-reload`).

## 客户端调试日志

默认日志只记启动, 警告和错误. 诊断同步延迟时, 在客户端状态目录 (Linux `~/.local/state/kaze-sync`, Windows `%LOCALAPPDATA%\kaze-sync`) 里创建一个名为 `debug` 的空文件并重启 agent, 日志 (`kaze-syncc.log`) 就多出 `level=DEBUG` 的时间线: 文件事件 (`watcher event`), 去抖到点后的任务 (`task planned`), 扫描 (`scan`) 与拉取 (`pull`) 的耗时和结果, 到服务端的请求 (`poll`, `server commit`, `upload blocks`, `changes`, `download blocks`, 带耗时 `ms`) 和轮询带来的通知 (`poll news`). 用完删掉该文件并重启 agent; 日志仍受 10 MB 轮转限制.

## 客户端卸载 (手动)

- Linux: `systemctl --user disable --now kaze-sync`, 删除 `~/.config/systemd/user/kaze-sync.service`, 删除状态目录 `~/.local/state/kaze-sync` (含日志); 同步目录里的文件保留.
- Windows: 删除注册表项 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 中的 `kaze-sync`, 结束 `kaze-syncc-agent.exe`, 删除 `%LOCALAPPDATA%\kaze-sync`; 同步目录里的文件保留.
- 然后在 Web UI 的设备页吊销该设备.

## 独占目录: 手动解除只读

子文件夹被另一台设备独占时 (设计 2.8), 本机该目录下的文件设为只读 (Linux 去掉文件和目录的写权限, Windows 设文件的只读属性). 在 Web UI 解除独占后, 客户端在下一次全量扫描 (最迟 1 小时) 自动恢复可写. 客户端被卸载, 或来不及收到解除时, 手动恢复:

```
chmod -R u+w <目录>                      # Linux
attrib -r <目录>\* /s /d                 # Windows
```

被拒绝的本地改动保存在同步根下的 `.syncsaved/<时间>/<原路径>`, 不再需要时直接删除 (Web UI 上对应的错误通知随之消失); `.syncsaved` 不同步, 也不会自动清理.

# 部署, 备份与升级

依据 `docs/design.md` 第 7 节与第 9 节; 这里只写操作步骤. 服务端只支持 OpenWrt, 数据放在单独挂载的数据盘上.

## 部署

在开发机上构建 (静态链接, 无依赖):

```
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncd ./cmd/kaze-syncd
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncc ./cmd/kaze-syncc
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kaze-syncc.exe ./cmd/kaze-syncc
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o kaze-syncc-agent.exe ./cmd/kaze-syncc
```

`kaze-syncc-agent.exe` 必须与 `kaze-syncc.exe` 放在同一目录, 自启动才无窗口.

在路由器上以 root 执行 (数据盘须已挂载, 数据目录不能在 overlay 上):

```
./kaze-syncd install --data /mnt/data/kaze-sync        # 可选 --listen :PORT, --cert F --key F, --wan
/mnt/data/kaze-sync/kaze-syncd admin-password          # 显示一次随机密码
```

`install` 打印端口和证书指纹. 用浏览器打开 `https://<路由器>:<端口>/`, 用密码登录; 在总览页 "添加设备" 得到注册命令, 在客户端上执行. 之后的配置都在 Web UI 中进行.

服务端的资源: 常驻内存约 24 MB; GC 时额外占用约每百万个块引用 32 MB 除以趟数 (设计 7 节的表). 512 MB 内存的机器处理 512 GB 以内的数据单趟完成, 更大的数据只是 GC 多扫几遍, 内存不变.

## 版本保留, GC 与删除

- 每次覆盖或删除都留一个旧版本, 保留 `keep_days` 天 (总览页修改, 默认 30). 保存后立即触发一次 GC, 不需要重启.
- GC 每天一次 (服务启动一分钟后检查, 距上次不足一天则顺延), 先删除超出保留期的版本, 再删除无人引用且超过 1 小时的块, 并清理 `var/tmp/` 中超过 1 小时的残留. 刚上传尚未提交的块受 1 小时宽限保护. GC 在后台运行, 不影响同步.
- GC 结果记在日志里: `logread | grep 'msg=gc '` 显示删除的版本数, 块数, 趟数和耗时.
- 文件夹页的 "恢复目录到某一时刻": 填写 UTC 时间, 当前目录下在该时刻存在而现在不同或已删除的文件都恢复成当时的内容; 之后新建的文件不动, 不删除任何文件. 只有仍在 `keep_days` 内的版本才能恢复.
- 文件夹页的 "删除文件夹" 只在没有设备映射它时可用 (先在各设备页删除映射); 它的库 `var/folders/<id>.db` 随之删除, 块在下次 GC 回收.

## 备份与还原

必须备份 `var/meta.db`, `var/folders/` 和 `var/blobs/`; 另外建议备份 `var/tls/` (自签名证书和私钥, 很小): 没有它, 还原后会生成新证书, 指纹变化, 全部客户端需要重新注册. 备份放在另一块盘或另一台机器上.

备份 (以 `/mnt/data/kaze-sync` 与目标目录 `/backup` 为例), 避开 GC 运行的时刻:

```
/etc/init.d/kaze-sync stop
cd /mnt/data/kaze-sync/var && tar cf /backup/meta.tar meta.db folders tls     # 停服务后 WAL 已合并
/etc/init.d/kaze-sync start                                                   # 停机只有几秒钟
tar cf /backup/blobs.tar blobs                                                # 块写入后不再修改, 可增量复制
```

还原到新机器 (新数据盘已挂载, 已有同一版本或更新版本的 `kaze-syncd`):

```
mkdir -p /mnt/data/kaze-sync/var
tar xf /backup/meta.tar -C /mnt/data/kaze-sync/var
tar xf /backup/blobs.tar -C /mnt/data/kaze-sync/var
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

服务端: 用新二进制再执行一次 `kaze-syncd install --data <同一目录>` (可重复执行, 端口, 证书和密码不变); 它复制新二进制, 按 `schema_version` 逐库迁移 (先全局库, 再每个文件夹库), 然后重启服务. 先升级服务端, 再逐台升级客户端: 替换客户端二进制 (Windows 是两个 exe), 重启 agent (Linux: `systemctl --user restart kaze-sync`; Windows: 注销再登录, 或结束 `kaze-syncc-agent.exe` 后重新运行). 服务端升级后, 版本过旧 (低于服务端的 `MinAgentVersion`, 设计 9.3) 的客户端不再同步: 它的日志 (Linux 在 `~/.local/state/kaze-sync/kaze-syncc.log`) 里每分钟一条 `client too old ... upgrade kaze-syncc`, 总览页该设备的版本列显示 "N (过旧, 请升级客户端)"; 按上面的步骤替换这台客户端的二进制并重启 agent 即可, 数据不受影响, 升级后自动继续同步. 只有协议变得不兼容的改动才会提高 `MinAgentVersion`, 其余升级不要求同步升级客户端. OpenWrt 自身的 `sysupgrade` 不影响数据和服务 (设计 9.1).

## 客户端卸载 (手动)

- Linux: `systemctl --user disable --now kaze-sync`, 删除 `~/.config/systemd/user/kaze-sync.service`, 删除状态目录 `~/.local/state/kaze-sync` (含日志); 同步目录里的文件保留.
- Windows: 删除注册表项 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 中的 `kaze-sync`, 结束 `kaze-syncc-agent.exe`, 删除 `%LOCALAPPDATA%\kaze-sync`; 同步目录里的文件保留.
- 然后在 Web UI 的设备页吊销该设备.

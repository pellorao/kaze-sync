# 自托管文件同步服务: 实施计划

依据 `docs/design.md`. 原则: 轻量优先, 最小可用版本先行; 每个里程碑结束时可实际使用或验证, 有明确验收标准, 并提交一次. 规模为 Go 产品代码的非测试行数估计 (HTML 模板与样式表计入); 测试工具单独列出, 不计入.

## 里程碑总览

| 里程碑 | 内容 | 产品代码 (行) | 结束后可用于 |
|---|---|---|---|
| M1 | 可先行模块 (设计 12 节), 实现中, 12.2 已完成 | 约 700 | 后续里程碑的基础 |
| M2 | 服务端核心与 OpenWrt 安装, 注册, Web UI 骨架, 分块单向上传客户端 | 约 2160 | Linux 上的带版本备份, 可在 Web UI 中浏览和恢复 |
| M3 | OpenWrt 测试虚拟机与端到端驱动 | 约 30 (驱动约 600) | 在真实部署上自动验证 |
| M4 | 多客户端双向同步, 冲突, 状态与通知 | 约 1090 | 在 Linux 之间替代 Nextcloud |
| M5 | 两层排除规则的集中配置与客户端应用 | 约 150 | 按需排除 |
| M6 | Windows 客户端 | 约 110 | Windows 与 Linux 混合使用 |
| M7 | 保留与 GC, 目录恢复到某一时刻, 导出 | 约 220 | 长期运行 |
| M8 | 普通 Linux 服务端安装 (systemd, OpenRC) | 约 80 | 服务端部署到 VPS |
| 合计 | | 约 4540 | |

依赖关系: M1 已开始, 接口不变. M8 只依赖 M7 (2026-11-04 用户增加). M2 起需要设计定稿. M3 的虚拟机由另一项工作构建 (结果另行报告), 端到端场景须在 M4 前就绪. M5 与 M6 互不依赖, 可对调.

## M1: 可先行模块 (约 700 行)

内容: 排除匹配器 (约 250), 块存储与哈希 (约 120, 已完成), 原子落地 (约 100), `FS` 接口与真实实现 (约 80), 目录监视 (约 170), 路径规则 (约 80).

验收标准: 设计 12.1 到 12.6 各节的测试全部通过; NTFS 相关测试按下文 "Windows 单元测试" 的方式运行.

## M2: 服务端核心与分块单向上传 (约 2160 行)

内容:

- 服务端 (约 1400): `run`, `install` (OpenWrt: 数据目录检查, 复制二进制, 用户创建, procd 脚本, `sysupgrade.conf`, `--wan` 防火墙规则, 数据库初始化与迁移, 随机端口, 自签名证书与指纹, `--cert`/`--key`), `admin-password` (生成, 替换, 重启服务); 表结构与提交事务 (base 检查, 旧内容移入 `versions`, 缺块返回 `need`, 冲突副本标记); 设备 API 的 `enroll`, `poll` (本阶段只下发配置), `commit`, `blocks`; 映射的保存时校验; 会话, CSRF, 安全响应头, 按 IP 锁定; Web UI 的登录, 总览 (设备表, 文件夹表, 添加设备, 新建文件夹, 版本保留天数), 设备页 (映射增删改), 文件夹页 (只读浏览, 版本列表与单版本恢复).
- 客户端 (约 760): `enroll` (Linux 用户 unit 与 Windows Run 键), `device.json` 与配置缓存, 多个映射的处理 (标记文件, 非空目录接管), 本地状态库, 全量扫描 (稳定期, SQLite 规则, 内置排除), 分块哈希, 缺块上传与提交.

验收标准:

- 在 Web UI 中添加设备, 在 Linux 上执行页面给出的命令完成注册; 为该设备映射 3 个文件夹到 3 个本地目录后, 三者在一个扫描周期内各自开始上传, 互不影响.
- 新增, 修改, 删除文件后, 服务端当前状态与各本地目录一致; 每次覆盖和删除都在文件夹页的版本列表中可见, 恢复后内容出现在服务端当前状态中.
- 分块: 1 GB 文件首次上传后, 修改其中 1 MiB, 第二次只上传 1 个块; 用一份真实的约 1 GB SQLite 数据库执行一次典型操作, 记录上传字节数, 据此确认块大小 1 MiB (或改为 256 KiB).
- 上传 1 GB 文件时杀掉客户端, 重启后只上传尚未到达的块.
- SQLite: 带非空 `-wal` 的数据库不上传并上报 `in_use`; 旁文件从不上传.
- 映射到非空且未勾选接管的目录时不上传任何内容; 移走同步根时没有任何删除提交.
- 安全: 带外部 `Origin` 的 POST 被拒绝; 同一 IP 第 11 次认证失败返回 429; `admin-password` 后旧会话失效且新密码可登录.
- 证书文件模式: 用测试 CA 签发的证书安装后, 客户端不带指纹注册成功.
- 本地 CA 模式 (2026-11-05, 单元测试): `install --ca` 生成 CA 与服务端证书 (SAN, 链, 权限, CA 私钥只属 root 且不在 `var/` 下), 续期条件 (临近到期, SAN 变化, CA 更换), 服务读不到 CA 私钥时只警告, `server.pem` 被替换后一分钟内重新载入, 模式之间切换, `ca --export` 只含证书; 客户端按 CA 指纹校验: 接受 CA 下的叶证书和续期后的证书, 拒绝其他 CA, 过期叶证书, 非 CA 的固定证书冒充 CA.

## M3: 测试环境与端到端驱动 (约 30 行产品代码, 驱动约 600 行)

### 环境构成

| 角色 | 位置 | 说明 |
|---|---|---|
| 服务端 | OpenWrt x86-64 虚拟机 (VMware Workstation) | 与目标路由器相同的系统, 用真实的 `kaze-syncd install` 和 procd 部署 |
| Linux 客户端 | 现有 WSL Ubuntu | 驱动程序也在这里运行 |
| Windows 客户端 | Windows 宿主本机 | 由驱动经 WSL 互操作启动 `kaze-syncc.exe`, 同步根在 NTFS 上 |

不克隆 ARW 的发布虚拟机, 两者互不影响. 已确认 WSL2 (NAT 模式) 能访问 VMware `vmnet8` 子网.

### OpenWrt 测试虚拟机 (概要)

虚拟机由另一项工作构建, 结果另行报告; 本计划只规定它需要具备的性质, 确切步骤放在新项目的 `e2e/vm/` 程序中, 可重复执行以重建基线:

- 用 OpenWrt Image Builder 生成定制的 x86-64 镜像 (或修改官方 combined-efi 镜像), 包含数据盘所需的软件包 (例如 block-mount, ext4 支持).
- `uci-defaults` 脚本在首次启动时完成设置: 写入本项目专用的 SSH 公钥, 预置 dropbear 主机密钥 (首次启动前已知, 可直接固定), 网络改为在 `vmnet8` 中可达 (关闭 OpenWrt 自带的 DHCP 服务, 避免与 VMware 的冲突), 时区 UTC.
- 第二块虚拟磁盘作为数据盘, 格式化为 ext4 并按 fstab 挂载, 模拟目标路由器的 1 TB 盘.
- 转换为 VMDK, 手写 `.vmx` (NAT, `tools.syncTime = "FALSE"`), 拍下干净快照 `kaze-sync-clean`.
- 驱动取得虚拟机 IP 的方式随构建结果而定 (open-vm-tools 可用时用 `vmrun getGuestIPAddress`, 否则使用 `uci-defaults` 设置的固定地址).

### 端到端驱动

位于新项目的 `e2e/` (Go, `go run ./e2e -config <path>`), 借鉴 ARW `scripts/release-vm.mjs` 的做法 (还原快照, 启动, 固定主机密钥的 SSH, 结束时硬关机并还原), 在新项目中独立实现, 不共享代码. 配置是仓库外的 JSON:

```json
{
  "vmrun": "/mnt/c/Program Files (x86)/VMware/VMware Workstation/vmrun.exe",
  "vmx": "D:\\VMs\\kaze-sync-openwrt\\kaze-sync-openwrt.vmx",
  "snapshot": "kaze-sync-clean",
  "ssh": {"user": "root", "port": 22, "identityFile": "/home/<me>/.ssh/kaze-sync-e2e", "hostKey": "ssh-ed25519 AAAA..."},
  "windowsWorkDir": "C:\\Users\\<me>\\AppData\\Local\\Temp\\kaze-sync-e2e"
}
```

驱动在 WSL 中运行: 经互操作调用 `vmrun.exe`; 用 WSL 的 `ssh` (临时 known-hosts 文件, `StrictHostKeyChecking=yes`, 固定 `HostKeyAlias`); 向虚拟机传文件用 `ssh ... 'cat > <path>'`, 不依赖 dropbear 上的 scp 或 sftp; 本地启动 Linux 版 `kaze-syncc`, 经互操作启动 Windows 版 `kaze-syncc.exe`, 两者都带 `--no-autostart`; 通过 HTTP 客户端加 cookie jar 提交 Web UI 表单配置服务端 (请求带服务端自己的 `Origin`, 顺带测试 Web UI); 对 Windows 同步根的文件操作经 `/mnt/c` 进行, 由 Windows 文件系统真实执行并产生变更通知.

运行流程: 还原快照并启动, 等待 SSH, 上传 `kaze-syncd`, `kaze-syncd install --data <数据盘>`, `admin-password`, 登录 Web UI, 添加两台设备并各映射两个文件夹, 两个客户端注册 (指纹经 SSH 读取), 依次执行场景, 收集两端日志 (服务端用 `logread`), 硬关机并还原快照. 目标总耗时 10 分钟以内.

M3 内的产品代码 (约 30): 健康检查端点 `/healthz`, `--no-autostart`.

### M3 验收标准

- 驱动从还原快照到硬关机全流程自动完成, WSL 与宿主都能访问 `https://<ip>:<port>/healthz`.
- `kaze-syncd` 由 procd 以 `kaze-syncd` 用户运行; 杀掉进程后自动重启; 重启虚拟机后服务自动恢复.
- `install` 把 `--data` 指向 overlay 上的目录时拒绝; 卸载数据盘后 `kaze-syncd run` 报错退出, 不创建新库.
- sysupgrade: 在虚拟机中用同一镜像执行一次 `sysupgrade` (保留配置), 重启后 `kaze-syncd` 用户, 初始化脚本, 自启动链接, 防火墙规则都在, 服务自动运行, 数据完整.
- M2 的单向上传场景在环境中自动通过.

## M4: 多客户端双向同步与通知 (约 1090 行)

内容:

- 服务端 (约 390): 完整的长轮询 (变更通知, 状态上报, 在线判断, `last_seen`), `changes`, `blocks/get`, `case_collision` 与 `type_conflict` 检查; 总览页的设备状态; 通知汇总与文件夹页的通知明细 (设计 4.3).
- 客户端 (约 700): `Pull` 三遍应用, 按块从本地复用加成批下载, 删除与空父目录清理, 冲突处理表与冲突副本, 文件监视接入与去抖, agent 事件循环, 状态上报, 配置推送的应用.

验收标准:

- 确定性多客户端模拟 (3 个客户端, 各订阅多个文件夹, 至少 1000 个随机种子, 每个数百步操作) 满足收敛与不丢数据; 每个 FS 和 HTTP 调用处注入崩溃的测试同样满足.
- 通知: 设计 4.3 表中每种来源出现后, 在总览汇总 (按文件夹的错误数与提醒数) 和文件夹页明细中可见, 链接指向正确的浏览位置或设备页; 原因消除后自动消失; 服务端重启后冲突副本与长期离线通知仍在.
- 端到端场景 (OpenWrt 服务端, WSL Linux 客户端, 宿主 Windows 客户端, 各两个文件夹):
  1. 一端新建文件, 另一端 10 秒内出现, 且落在对应的本地目录.
  2. 两端同时修改同一文件, 两端出现同一个冲突副本, 总览页的通知汇总中该文件夹错误数为 1; 在一端删除冲突副本后通知消失.
  3. 停止一端的 agent 后两端各自修改, 恢复后按冲突表收敛.
  4. 上传大文件时在服务端执行 `/etc/init.d/kaze-sync restart`, 客户端只补传剩余的块.
  5. 1 GB 文件在一端局部改写 1 MiB, 另一端只下载变化的块 (1 MiB 跨 4 个 256 KiB 块, 共 4 个块, 不是 1 个).
  6. 移动含 1000 个文件的目录, 没有任何块上传或下载.
  7. 在 Web UI 中给一端新增第三个映射, 该端在一次长轮询内开始同步, 总览页显示配置已最新.
  8. 在服务端虚拟机上用防火墙丢弃该端口的入站包 1 分钟后恢复, 客户端自动重连并收敛 (只有客户端发起连接).
- 从 Nextcloud 迁移演练: 两台已有相同文件的客户端先后接入 (勾选接管), 第二台不下载任何块.

自此可以在 Linux 之间用数据副本试运行 1 到 2 周, 期间保持 Nextcloud 不动.

## M5: 两层排除规则 (约 150 行)

内容: 总览页的全局规则与文件夹页的文件夹规则的编辑和行号报错, 全局规则初始化为默认规则; 服务端拼接两层并在任一层修改时广播; 客户端按设备系统选择大小写模式, 扫描剪枝, 拉取过滤, 规则变化时停止跟踪新排除的路径并以 `cursor=0` 全量对账.

验收标准:

- 修改全局规则后, 所有设备上的所有文件夹在一次长轮询内按新规则生效.
- 全局规则有 `build/`, 某文件夹规则为 `!/build/`, `/build/*`, `!/build/keep.txt` 时, 该文件夹只同步 `build` 下的 `keep.txt`, 其他文件夹的 `build/` 仍被排除.
- 新增排除后, 各设备的本地文件保留, 服务端上的文件不受影响; 取消排除后两边按冲突表收敛.
- 内置排除不能被 `!` 覆盖; 同一规则在 Windows 上不区分大小写, 在 Linux 上区分.

## M6: Windows 客户端 (约 110 行)

内容: 锁定文件的退避重试与上报, 被打开文件上 rename 和删除的重试, 仅大小写改名的上传与应用, `kaze-syncc-agent.exe` 的 GUI 子系统构建.

验收标准:

- Windows 单元测试在 NTFS 上通过 (锁定文件, 被打开文件上的覆盖与删除, 仅大小写改名, 长路径, 监视溢出).
- 端到端: Linux 端创建的 `a:b.txt` 被拒绝并作为错误通知出现在该文件夹的明细中, 在 Linux 端改名后通知消失; Windows 端 `a.txt` 改名为 `A.txt` 后 Linux 端只有一个 `A.txt`; Windows 上两个不同盘符的映射同时同步.
- 手动验证一次: Word 打开文档期间远端更新会等待, 关闭后完成; 注销再登录后 agent 无窗口自动运行.

自此可以替换 Nextcloud: 在路由器上安装服务端 (若采用 Q15 的建议, 加 `--wan`); 停用 Nextcloud 客户端; 在 Web UI 中为每台设备添加全部映射并勾选接管; 第一台上传完成后, 其余设备依次接入, 按哈希采纳, 不重新下载.

## M7: 长期运行 (约 220 行)

内容: 按 `keep_days` 删除旧版本, 块 GC (收集与按预算分趟的核心已先行实现, 见设计 7 节; M7 补定时与触发, 1 小时宽限与 `tmp/` 清理, 单实例运行), 修改 `keep_days` 后立即触发 GC, 文件夹页的 "目录恢复到某一时刻", `kaze-syncd export`, 删除文件夹 (只在没有映射时, 同时删除它的库 `folders/<id>.db`), 部署, 备份与升级说明.

验收标准:

- 构造带时间的历史后, GC 只删除超出保留期的版本和无引用的块, 刚上传尚未提交的块不受影响; 多个版本共享的块在任一版本仍被引用时保留.
- 在 Web UI 中把 `keep_days` 从 30 改为 1 后, 不重启服务, 超过 1 天的版本在保存后立即被清除, 其独占的块随后被回收.
- 端到端: 一端误删含 1000 个文件的目录, 在 Web UI 中把该目录恢复到删除前, 两端都恢复全部文件, 之后新建的文件不受影响.
- 端到端升级: 用旧版本部署并同步后, 用新二进制再执行 `kaze-syncd install`, 迁移自动完成, 同步不受影响.
- `kaze-syncd export --at` 导出的树与当时客户端的树逐字节一致; 按设计 9.4 的步骤备份 `meta.db`, `folders/` 与 `blobs/` 后, 在干净的虚拟机上还原并 `export`, 结果一致.

## M8: 普通 Linux 服务端安装 (约 80 行)

内容 (设计 9.1 "systemd 与 OpenRC"): `install` 按 `/etc/openwrt_release`, `/run/systemd/system`, `/sbin/openrc-run` 识别服务管理器, 都不是时拒绝; systemd unit 与 OpenRC 脚本的写入, 启用与重启; `useradd` 与 busybox `adduser` 两种系统用户创建; 普通 Linux 上不做数据目录的设备号检查, 带 `--wan` 时报错; `admin-password` 和 `install` 末尾按服务管理器重启服务; `docs/operations.md` 补普通 Linux 的安装, 启停, 日志, 放行端口和备份步骤, 以及 OpenWrt 只有一块盘时在同一块盘上新建分区并挂载的步骤.

验收标准:

- 单元测试: 三种服务管理器的识别与拒绝, 各自写出的服务定义与执行的命令 (用替身记录, 不改真实系统), 普通 Linux 上带 `--wan` 报错, OpenWrt 上仍拒绝根文件系统上的数据目录.
- 虚拟机 (共用目录中的 `ubuntu` 与 `alpine`, 从各自登记的基线开始, 结束后还原; 用前加锁): 在根文件系统上的数据目录执行 `install` 与 `admin-password`, 服务以 `kaze-sync` 用户运行; 重启虚拟机后服务自动启动; 杀掉服务进程后被重新拉起; 用新二进制再执行 `install` 完成升级且端口, 证书, 密码不变; WSL 中的 Linux 客户端注册到该服务端并同步一个文件夹 (双向各一个文件).
- 在两台虚拟机上记录服务端空闲时的常驻内存与系统总内存占用 (设计 "首要目标").
- 已有端到端场景 (OpenWrt) 全部通过.

## 开发与验证方式

- 仓库布局: `cmd/kaze-syncd`, `cmd/kaze-syncc`, `internal/server` (含 `web/` 模板与静态文件), `internal/client` (引擎内核, 扫描, 拉取, agent 循环, `fs`, `place`, `watch`), `internal/ignore`, `internal/blobstore`, `internal/proto` (协议类型, 块大小, 路径规则), `e2e/` (含 `e2e/vm/`). 不预设其他层次.
- Linux 单元测试: WSL 中 `go test ./...` (差分测试需要 `git`).
- Windows 单元测试: 在 WSL 中 `GOOS=windows go test -c -o <Windows 磁盘上的目录>/x.test.exe ./internal/...`, 再经互操作运行, 使其临时目录位于 NTFS. 可执行文件不受 PowerShell 执行策略限制.
- 服务端二进制: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/kaze-syncd`.
- 端到端: `go run ./e2e -config <path>`, 每个里程碑结束时运行全部已有场景.
- 每个里程碑结束时: 单元与模拟测试全绿, 端到端场景通过, 提交一次.

计划层面没有待决问题 (原 E1 随虚拟机改由另一项工作构建而取消, E2 原为 systemd 相关问题, 已由 M8 的设计取代).

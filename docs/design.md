# 自托管文件同步服务: 设计

状态: 定稿 (用户 2026-11-02 复核通过). 工程名 `kaze-sync` (用户 2026-11-02, Q16), 服务端二进制 `kaze-syncd`, 客户端 `kaze-syncc`; 服务端的系统用户, procd 脚本和防火墙规则都叫 `kaze-sync`.

## 首要目标: 低 CPU, 低内存, 低流量

本项目最大的特色是资源占用低 (用户 2026-11-02), 它是开发的首要目标, 每项设计与实现都先按它取舍:

- **低内存**: 服务端要能在 512 MB 内存的 VPS 或路由器上运行; 内存随数据量增长的部分 (如 GC) 有明确上限, 超出预算时用时间换内存, 不靠 swap. 数据量与所需服务端内存的对应关系在文档中写明, 由用户选择机器.
- **低 CPU**: 两端空闲时接近零占用, 不做无变化时的轮询计算; 扫描先比较大小与修改时间, 只对变化的文件计算哈希.
- **低流量**: 只传变化的块, 已有的块不重传; 空闲连接的保活开销尽量小.
- 每个里程碑实测两端的常驻内存, 空闲 CPU 和同步流量, 结果记入验证报告; 新功能若明显增加其中任一项, 先与用户讨论.

## 轻量原则

除资源占用外, 本项目的另一特征是功能尽可能少. 每个功能, 页面, 命令, 配置项和表都必须由以下某项用户需求直接要求, 否则不做:

1. Windows 与 Linux 客户端上多个本地目录的双向同步.
2. glob 排除规则.
3. 最小限度的版本管理, 防止误删和误覆盖.
4. 在服务端 Web UI 中集中配置和查看.
5. 服务端可以从公网访问.
6. 大文件 (约 1 GB 的 SQLite 数据库) 的高效同步.

能用一种机制解决的不用两种. 删掉的内容和节省的行数见第 13 节.

## 0. 为什么自研

现有方案都至少缺一项硬需求: Syncthing (中心节点拓扑) 的路径, 本机覆盖规则和客户端错误分散在每台设备的本地 GUI 中, 不能集中配置和查看; Seafile 和 ownCloud Infinite Scale / OpenCloud 的排除规则只有简单通配符且在客户端本地配置; rclone bisync 不能持续监视, 多台机器并发运行没有协调; Unison 需要每台机器一份 profile, 冲突默认交互处理. 用户已决定跳过 Syncthing 试用, 直接自研.

本设计借鉴了它们的做法: 冲突时 "先到服务端者胜, 后到者成为冲突副本" 与 Seafile 和 Nextcloud 相同; 固定大小块, 按块哈希复用本地数据, 以及临时文件加 rename 的落地方式与 Syncthing 相同; 大小写冲突与 Syncthing 一样只报告, 不自动改名.

来源: [Syncthing syncing](https://docs.syncthing.net/users/syncing.html), [Syncthing ignoring](https://docs.syncthing.net/users/ignoring.html), [Syncthing caseSensitiveFS](https://docs.syncthing.net/advanced/folder-caseSensitiveFS.html), [Seafile excluding files](https://help.seafile.com/syncing_client/excluding_files/), [Seafile conflicts](https://robotics.shanghaitech.edu.cn/seafile/help/conflicts/), [rclone bisync](https://rclone.org/bisync/).

## 1. 范围与非目标

目标:

- 服务端只运行在 OpenWrt 上: 局域网中的 x86-64 路由器 (Intel i5, 8 GB 内存, 1 TB 数据盘). 若干 Windows 和 Linux 客户端, 单用户.
- 服务端有多个同步文件夹. 每台客户端可以同步多个文件夹, 每个文件夹映射到本机的一个本地目录 (与 Nextcloud 一样, 例如 Windows 上的 `D:\Docs` 和 `E:\Photos`, Linux 上的 `~/Docs`), 各文件夹互相独立.
- 客户端监视本地变更并上传, 拉取其他客户端的变更; 并发修改时不丢数据.
- 大文件按固定大小的块传输和存储, 只传变化的块, 各版本共享未变化的块.
- 服务端保留被覆盖和被删除的旧内容, 可在 Web UI 中按文件恢复, 或把一个目录恢复到某一时刻.
- 服务端 Web UI 是唯一的图形界面和唯一的通知渠道: 设备注册, 文件夹与映射, 排除规则, 版本保留期, 状态, 通知, 只读的文件浏览, 版本恢复.
- 服务端可从公网访问: 使用随机高端口, 默认自签名证书加指纹固定, 可选使用证书文件; 随机生成的管理员密码; 按 IP 的失败锁定.
- 所有连接都由客户端发起, 客户端不需要任何入站端口 (2.2).
- 两端各一个静态二进制, 无外部依赖.

非目标 (每一项都意味着不写对应代码):

- 分享, 多用户, 协作编辑; Web UI 中打开, 预览或下载文件 (Web UI 只浏览名称和元数据).
- OpenWrt 以外的服务端安装 (systemd, OpenRC); 列入下面的未来支持计划, 现在不实现.
- ACME / Let's Encrypt: 公网端口不是 443 或 80, HTTP 与 TLS-ALPN 验证都无法进行; 需要正式证书时用证书文件模式 (例如 acme.sh 的 DNS 验证).
- 客户端上的任何通知 (桌面通知, shell 提示, 推送服务); 客户端本地 GUI, 托盘, 客户端本地命令 (除注册外).
- 按设备的排除规则, 也就是按设备只同步文件夹的一部分: 需要时拆成单独的文件夹 (3.2).
- 端到端加密, 内容定义分块 (5.3), 传输压缩, 带宽限速.
- 空目录同步, 符号链接, ACL, 扩展属性, 所有者.
- 双因素认证 (4.3), 配置修改历史, 自动更新, macOS, 移动端.
- 服务端不是备份: 版本与当前数据在同一块盘上; 但客户端本身就持有各文件夹的普通文件副本.

未来支持计划 (现在不实现, 不为它预留代码; 实施前先与用户确认设计):

- 服务端安装到普通 Linux VPS (systemd), 目标机器内存 512 MB 到 1 GB (用户 2026-11-02). 当前实现保持对它友好: OpenWrt 专有部分 (procd, uci, `sysupgrade.conf`) 只放在 `install` 中, 其余服务端代码不依赖它们.

## 2. 架构

### 2.1 组件

- `kaze-syncd`: 一个 HTTPS 端口上同时提供设备 API (`/api/v1/`, 设备令牌认证) 和 Web UI (其余路径, 管理员会话认证). 内含 SQLite 元数据库, 内容寻址的块存储, 版本保留与 GC. 子命令: `run` (服务进程), `install` (安装或升级, 含首次初始化), `admin-password` (生成新管理员密码), `export` (灾难恢复导出).
- `kaze-syncc`: 子命令只有 `enroll` (注册本机并配置自启动) 和 `run` (后台 agent). Windows 额外用 `-ldflags -H=windowsgui` 构建 `kaze-syncc-agent.exe` 供自启动使用, 源码相同, 没有额外代码.
- 客户端引擎内核对每个文件夹只有两个同步步骤: `Scan` (本地变更 -> 提交到服务端) 和 `Pull` (服务端变更 -> 本地). agent 事件循环在文件事件, 长轮询返回和定时器到来时调用它们. 内核不含计时器和 goroutine, 测试可以逐步驱动.

### 2.2 连接方向: 只由客户端发起

- 客户端的全部通信都是它主动发起的 HTTPS 请求: 长轮询, 拉取变更, 提交, 块上传和下载. 服务端到客户端的信号 (有新变更, 配置已修改) 只通过客户端挂起的长轮询返回. 服务端从不连接客户端.
- 因此客户端在 NAT, 运营商级 NAT 或防火墙后都能工作, 不需要端口转发或 NAT 穿透; 只有服务端需要一个可达的端口 (随机高端口, 9.1).
- 服务端就是路由器本身, 所以局域网内的客户端访问路由器的公网地址 (或解析到它的域名) 时直接到达路由器, 不需要 NAT 回流. 每台客户端只需一个服务端地址, 在家和在外都能用.
- 长轮询每次最多挂起 55 秒. Go 默认的 TCP keepalive (15 秒) 让 NAT 映射在等待期间保持活跃. 若中间设备更早切断空闲连接, 长轮询以错误结束, 客户端按 1 秒到 60 秒的退避重试; 正确性不受影响, 只是通知变慢.
- 客户端遵守 `HTTPS_PROXY` 环境变量 (Go `http.ProxyFromEnvironment`), 经 HTTP 代理的 CONNECT 隧道工作; 指纹固定在隧道内照样有效.

### 2.3 服务端存储

```
<data>/                   安装时用 --data 指定, 位于大容量数据盘; root 所有, 0755
  kaze-syncd              服务端二进制; root 所有, 0755 (服务用户不能改写)
  var/                    服务进程写入的一切; kaze-sync 用户所有, 0700
    meta.db               全局库: devices, mappings, folders, meta; SQLite, WAL, synchronous=FULL
    folders/<id>.db       每个同步文件夹一个库: files, versions, meta; 同样的 SQLite 设置
    blobs/ab/cd/<sha256>  块内容 (至多 256 KiB), 写入后只读
    tmp/                  写入中的块
    tls/                  自签名证书与私钥
```

`<data>` 和二进制属 root, 是为了让被攻破的服务进程不能替换自己的二进制 (否则下一次 `install` 或重启会以 root 或持续地执行被改过的代码). 服务用户只能写 `var/`. `kaze-syncd run --data <data>` 在内部定位 `var/`.

元数据分在两类库里 (用户 2026-11-02): 全局库 `meta.db` 放设备, 映射, 文件夹和全局设置; 每个同步文件夹有自己的库 `folders/<id>.db` (`<id>` 为 `folders.id`), 放该文件夹的 `files` 和 `versions`. 好处是一个文件夹的库损坏或锁争用只波及该文件夹, 各文件夹的提交可并行; 块存储和磁盘层面的故障仍是共用的. 新建文件夹时在同一操作中创建并初始化它的库 (失败则不创建文件夹); 删除文件夹时删库 (M7). 服务进程按需打开文件夹库并保持打开 (只打开有请求访问过的文件夹).

表 (全局库 4 张: `folders`, `devices`, `mappings`, `meta`; 每个文件夹库 3 张: `files`, `versions`, `meta`):

- `files` (在文件夹库中): 该文件夹每个路径的当前状态, 含删除后的墓碑. 列: `path` (原样 UTF-8, `/` 分隔), `key` (NFC 加 Unicode 大小写折叠), `seq` (文件夹内唯一且单调递增), `hash` (整个文件的 SHA-256, 删除时为 NULL), `blocks` (按顺序拼接的块哈希, 每块 32 字节), `size`, `mtime_ns`, `exec`, `device`, `created_at`, `conflict` (路径是否为冲突副本名, 提交时计算). 主键 `path`; 部分唯一索引 `UNIQUE(key) WHERE hash IS NOT NULL`; 部分索引 `(path) WHERE conflict AND hash IS NOT NULL` 供通知查询; 索引 `(seq)` 供 `GET /changes` 按游标读取 (文件夹库架构版本 2 加入).
- `versions` (在文件夹库中): 被替换或删除的旧内容, 列与 `files` 相同, 另加 `id` 和 `replaced_at`; 索引 `(path)`.
- `meta(key, value)` (在文件夹库中): `schema_version`, 该文件夹的下一个 `seq`.
- `folders(id, name, rules)`: `rules` 为该文件夹的排除规则, 接在全局规则之后.
- `devices(id, name, os, token_hash, created_at, last_seen)`: `name` 限 `[A-Za-z0-9_-]{1,32}` 且唯一 (要进入冲突副本文件名); 设备行在注册时创建 (注册时才知道 `os`). `files.device` 存设备名文本 (跨库, 所以不用外键), 吊销或改名后来源仍可读.
- `mappings(device_id, folder_id, path, adopt)`: 本机路径, 是否允许接管已有的非空目录.
- `meta(key, value)` (在全局库中): `schema_version`, `config_version`, `admin_password_hash`, `global_rules`, `keep_days`, `listen`, `cert`, `key`, `last_gc` (上次请求 GC 的时间, 供每天一次的计时在重启后继续).

会话, 注册码, 设备最近上报的状态, 失败计数都只放在内存中: 服务端重启后管理员需重新登录, 未使用的注册码失效, 客户端在重连后的第一次长轮询中重新上报状态. `devices.last_seen` 持久化 (每次长轮询结束时更新, 至多每分钟写一次), 供 "设备长期离线" 通知在重启后仍然成立.

要点:

- 变更日志就是按 `seq` 排序的 `files`, `seq` 在每个文件夹内单调递增, 客户端为每个订阅的文件夹各保存一个游标; 新映射用 `cursor=0` 拿到完整当前状态. 墓碑永久保留, 不存在 "游标过旧" 的情况.
- 每次提交在该文件夹库的一个 SQLite 事务中完成: 更新 `files`, 把旧内容移入 `versions`, 分配 `seq`. Web UI 中的恢复也是服务端内部提交, `device` 记为 `web`.
- 块以 SHA-256 命名, 相同内容只存一份: 改名, 复制, 各版本中未变化的块, 多台客户端已有的相同文件都不重复存储或传输.
- 服务端磁盘上看不到原始目录树 (用户已接受): `kaze-syncd export` 把任一时刻的树导出为普通文件; `var/meta.db` 与 `var/folders/` 必须纳入备份 (9.4).

### 2.4 设备 API

HTTP/1.1 + JSON, 全部位于 `/api/v1/`, 除 `enroll` 外每个请求带 `Authorization: Bearer <token>`.

| 方法与路径 | 作用 |
|---|---|
| `POST /enroll` | `{code, os, hostname}` 换取设备令牌和设备名 (`name`, 冲突副本命名用), 只此一次 |
| `POST /poll` | 请求体带每个订阅文件夹已知的游标 (`cursors`, 文件夹 id 到 `seq`), `config_version` 和当前状态 (3.6). `cursors` 是客户端已读到的各文件夹最新 `seq` (不是已应用的游标, 见 5.5), 缺少某个文件夹的键视为有新内容. 本设备已订阅文件夹的最新 `seq` 与 `cursors` 不同, 或配置版本不同时立即返回, 否则最多挂起 55 秒; 任何一次成功提交都会唤醒挂起的长轮询. 返回各订阅文件夹的最新 `seq` (`cursors`), 配置有变化时附带完整的新配置 |
| `GET /changes?folder=F&since=N` | 该文件夹 `seq > N` 的条目, 每页至多 5000 条, 块哈希合计超过 2 万个时提前结束该页, 带 `cursor` 和 `more` |
| `POST /commit` | `{folder, ops}`, 至多 1000 个操作, 每个独立返回 `ok` (新 `seq`), `need` (缺少的块哈希), `conflict` (附服务端当前状态) 或 `rejected` (附原因) |
| `POST /blocks` | 上传块包: 请求体为若干 `[32 字节哈希][4 字节长度][数据]`, 服务端逐块校验并保存, 中途断开时已完整收到的块保留 |
| `POST /blocks/get` | `{hashes}`, 以同样格式返回块包 (每次至多 64 MiB, 即 256 个块); 请求中任一块不存在时整个请求失败, 不发送部分内容 |

长轮询同时承担心跳, 状态上报和配置下发, 因此没有单独的配置和状态端点. "在线" 即该设备当前有挂起的长轮询, 或 90 秒内结束过一次.

提交操作: `{path, base_seq, hash, blocks, size, mtime_ns, exec}`, `hash` 为 null 表示删除. 服务端检查顺序: 先检查 base (`base_seq` 等于当前 `seq`; 或者路径不存在或是墓碑且操作为新增; 删除墓碑视为成功的空操作), 不符返回 `conflict`; 再检查名称 (`rejected`); 最后检查块是否都已存在, 缺块时返回 `need`, 不改变任何状态, 客户端上传后重新提交. `rejected` 的原因: `case_collision`, `invalid_name`, `type_conflict` (第 6 节).

请求体大小在入口限制 (JSON 至多 4 MiB, 块包至多 64 MiB, 单块至多 256 KiB), 之后内部不再重复校验.

### 2.5 设备认证, 注册与 TLS

- 设备令牌: 32 字节随机数, 数据库只存 SHA-256. 在 Web UI 中吊销设备即删除令牌.
- 注册: 管理员在总览页 "添加设备" (输入名称), 服务端生成一次性注册码 (12 个 base32 字符, 约 60 bit, 15 分钟有效, 只在内存中存哈希), 页面显示可直接复制的 Linux 和 Windows 命令, 例如 `kaze-syncc enroll https://sync.example.org:41873 K7QF-M2XD-9PLA --fingerprint sha256:...`.
- TLS 两种模式, 由 `kaze-syncd install` 的参数决定:
  - 自签名 (默认, 公网与局域网通用): 首次安装生成 ECDSA P-256 自签名证书, 有效期 100 年, 不需要续期. `install` 打印其 SHA-256 指纹, 注册命令附带 `--fingerprint`, 客户端只接受该证书, 与地址和主机名无关. 浏览器第一次访问时手动接受一次例外 (可对照打印的指纹).
  - 证书文件 (`--cert <file> --key <file>`): 例如用 acme.sh 的 DNS 验证取得的正式证书. 浏览器和客户端都按系统根证书校验, 注册命令不带指纹. 证书续期后由续期工具执行 `/etc/init.d/kaze-sync restart` (acme.sh 的 `--reloadcmd`), 服务端不监视证书文件.
- 两种模式之间切换需要各客户端重新注册一次, 文档写明, 不做兼容逻辑.

## 3. 集中配置

### 3.1 客户端本地只有引导信息和缓存

客户端状态目录 (Linux `~/.local/state/kaze-sync/`, Windows `%LOCALAPPDATA%\kaze-sync\`) 中只有:

- `device.json`: `server`, `fingerprint` (可空), `token`, `device_id`, `name` (设备名), 由 `kaze-syncc enroll` 写入 (Linux 下权限 0600).
- `config.json`: 最近一次从服务端取得的有效配置, 原子写入.
- `folder-<id>.db`: 每个已订阅文件夹的本地同步状态 (5.1).
- 日志文件.

没有需要用户编辑的东西. 重新注册只需再执行一次 `kaze-syncc enroll`.

### 3.2 配置模型

| 项 | 内容 | 在 Web UI 的位置 |
|---|---|---|
| 全局 | 全局排除规则, 版本保留天数 `keep_days` | 总览页 |
| 文件夹 | 名称, 该文件夹的排除规则 | 文件夹页 |
| 映射 (设备 x 文件夹) | 本机路径, 允许接管非空目录 | 设备页 |

排除规则只有两层, 都不属于某台设备:

1. 全局规则: 所有文件夹共用, 修改后对所有客户端上的所有文件夹生效. 初始内容为 8.2 的默认规则.
2. 文件夹规则: 接在全局规则之后. 它不能删除全局规则中的行, 但因为最后匹配的规则生效, 它可以用 `!` 覆盖任何全局规则, 所以表达能力不受限制: 一个文件夹甚至可以先写 `!*` 取消全局规则的全部效果, 再写自己的完整规则. 唯一需要留意的是 gitignore "父目录被排除后其下内容不能重新包含" 的限制, 此时先重新包含目录, 再排除其内容, 最后重新包含所需文件. 例如全局规则有 `build/`, 某文件夹想保留 `/build/keep.txt`:

```
!/build/
/build/*
!/build/keep.txt
```

没有按设备的规则层: 同一个文件夹在所有设备上同步相同的内容. 只想在某台设备上同步一部分时, 把那部分拆成单独的文件夹, 只映射到需要它的设备. 匹配是否区分大小写仍由设备的系统决定 (Windows 不区分, Linux 区分); 内置排除 (8.1) 始终生效. 扫描间隔不是配置项, 固定为 1 小时 (文件监视负责及时性, 全量扫描只是兜底).

服务端在保存时校验 (配置进入系统的唯一信任边界): 规则逐行编译, 错误时指出行号; 路径必须是该设备系统下的绝对路径 (Windows 为盘符路径, Linux 以 `/` 开头); 同一设备的两个映射路径不得互相包含, Windows 设备按不区分大小写比较; `keep_days` 为 1 到 3650 的整数.

### 3.3 下发与版本

- 任何影响客户端的修改 (全局规则, 文件夹规则, 映射) 都在同一个事务中把 `config_version` 加 1 并触发长轮询广播, 在线的客户端在 1 秒内收到新配置. 修改 `keep_days` 只影响服务端, 不改变 `config_version` (第 7 节).
- 有效配置由服务端拼接规则后下发, 客户端不区分两层:

```json
{"version": 42,
 "folders": [{"id": 3, "name": "Docs", "path": "D:\\Docs", "adopt": false, "rules": ["~$*", "*.tmp", "/Build/"]},
             {"id": 5, "name": "Photos", "path": "E:\\Photos", "adopt": true, "rules": ["~$*", "*.tmp", "Thumbs.db"]}]}
```

- 版本号只用于判断是否需要更新, 不保存配置修改历史: 排除规则写错不会删除任何文件 (8.3), 路径写错只会在新路径上开始同步, 旧路径的文件原样保留.
- 客户端只在两个同步步骤之间应用新配置.

### 3.4 离线工作

agent 启动时先读 `config.json`, 立即按缓存的配置扫描和排队上传; 服务端不可达时继续监视和排队, 按退避重连. 从未取得过配置时只等待.

### 3.5 映射变化在客户端上的处理

- 新增映射: 本机路径不存在时创建目录并写入标记文件 `.syncroot`; 目录为空时写入标记; 目录非空且没有标记时, 只有勾选了 "允许接管" 才写入标记并开始全量对账 (从 Nextcloud 迁移的情况), 否则上报 `folder` 错误并等待. 这防止把路径误填为 `C:\Users\pello` 后把整个用户目录上传进去.
- 标记文件在之后消失 (磁盘未挂载, 目录被移走): 该文件夹停止同步并上报 `folder` 错误, 绝不把整个目录当作 "全部被删除".
- 路径修改等同于删除旧映射加新增映射; 删除映射时停止同步, 删除本地状态库和标记文件, 本地文件原样保留.
- 规则变化: 见 8.3.

### 3.6 客户端上报的状态

随每次长轮询上报, 状态变化时 (至多每 10 秒一次) 客户端取消挂起的长轮询并带新状态重新发起:

- 全局: `agent_version`, `os`, `hostname`, 已应用的 `config_version`.
- 每个文件夹: `state` (idle, syncing, error), `last_synced_at` (最近一次本地与服务端确认一致的时刻), `pending_up`, `pending_down`, 至多 100 条当前错误 `{path, kind, message, since}`.

错误种类只有 5 种, 细节放在 `message` 中:

| `kind` | 含义 |
|---|---|
| `in_use` | 文件被锁定, 正在写入, 或 SQLite 数据库正在使用 (5.6), 稍后自动重试 |
| `rejected` | 服务端拒绝 (大小写冲突, Windows 非法名称, 文件与目录同名) |
| `skipped` | 不同步的条目 (符号链接, 非 UTF-8 文件名) |
| `io` | 其他读写错误 |
| `folder` | 同步根缺失或非空目录未获准接管 |

## 4. Web UI

### 4.1 技术选择

服务端渲染 HTML: Go `html/template` 加 `embed`, 一个手写样式表, 一个很小的原生 JS 文件 (只用于恢复和删除前的确认, 不用内联事件, 便于设置严格 CSP). 表单提交后重定向 (PRG). 总览页用 `<meta http-equiv="refresh" content="15">` 自动刷新. 不用 SPA 框架和 htmx: 页面只有表格和表单, 服务端渲染没有前端构建步骤和第二套依赖, 也不需要单独的 JSON API.

### 4.2 页面 (共 4 个)

| 页面 | 内容 |
|---|---|
| 登录 | 管理员密码 |
| 总览 | 通知汇总 (4.3); 设备表 (名称, 系统, 在线, 各文件夹的最近同步时间与待处理数, 客户端版本, 配置是否最新); 文件夹表 (文件数, 总大小); "添加设备" 表单 (提交后显示注册码和命令); "新建文件夹" 表单; 全局排除规则; 版本保留天数 |
| 设备 | 该设备的全部映射, 可增删改 (文件夹, 本机路径, 允许接管); 吊销 |
| 文件夹 | 该文件夹的通知明细 (4.3); 文件夹规则; 只读的文件浏览 (目录, 名称, 大小, mtime, 来源设备), 可切换显示已删除的文件; 文件的版本列表 (时间, 大小, 来源设备) 与恢复; 目录的 "恢复到某一时刻" 表单; 删除文件夹 (只在没有映射时可用) |

Web UI 不提供文件下载, 预览或打开 (用户要求). 冲突副本和旧版本都通过 "恢复" 落到客户端后, 在客户端上查看. 回收站不是单独页面: 已删除文件在文件夹浏览中显示并可恢复.

### 4.3 通知

Web UI 是唯一的通知渠道, 客户端不发出任何通知. 通知不是单独存储的事件, 而是由当前状态即时算出: 每条通知都对应一个仍然存在的原因, 原因消除后通知自动消失. 因此不需要 "已读" 或 "忽略" 状态, 也不需要任何表; 被忽略却未解决的问题也不会被隐藏.

来源, 级别与处理:

| 来源 | 级别 | 处理 | 何时消失 |
|---|---|---|---|
| 客户端 `rejected` | 错误 | 在该客户端上改名 (大小写冲突时改成不冲突的名称, 非法名称时去掉非法字符), 或用规则排除 | 客户端不再上报 |
| 客户端 `io` | 错误 | 按消息检查该客户端的磁盘或权限 | 客户端不再上报 |
| 客户端 `folder` | 错误 | 挂载磁盘, 或在设备页修改路径或勾选接管 (链接到设备页) | 客户端不再上报 |
| 冲突副本 (服务端当前状态中名称为冲突副本的文件) | 错误 | 在任一客户端上比较两份, 删除冲突副本, 或改名以同时保留 (链接到浏览中的位置, 可查看两者的版本列表) | 冲突副本被删除或改名 |
| 客户端 `in_use` | 提醒 | 关闭占用该文件的程序; SQLite 数据库见 5.6 | 客户端不再上报 |
| 客户端 `skipped` | 提醒 | 不需要时用排除规则排除 | 客户端不再上报 |
| 设备 7 天未连接 | 提醒 | 让设备上线, 或吊销 | 设备再次连接 |

- 级别只有两种: 错误 (需要动手才会消失) 和提醒 (通常会自行消失, 或只需知晓).
- 总览页的通知汇总: 每个文件夹一行, 显示错误数与提醒数, 点击进入该文件夹页的通知明细; 另有一行 "设备" 汇总长期离线的设备 (这类通知不属于某个文件夹), 链接到对应设备页. 没有通知的文件夹不显示.
- 文件夹页的通知明细逐条列出: 级别, 来源设备, 路径 (链接到浏览中所在目录), 消息, 起始时间, 处理说明. 用户逐条按说明处理.
- 客户端上报的错误保存在内存中; 设备离线时保留其最后一次上报的内容并标出 "最后上报于", 服务端重启后在设备重连前不显示. 冲突副本由 `files.conflict` 的部分索引查询, 长期离线由 `devices.last_seen` 计算, 两者都不受重启影响.
- 通知汇总取代了上一版总览页的冲突列表和设备页的错误列表.

### 4.4 认证与公网安全

- 单个管理员. `kaze-syncd admin-password` 在服务端执行: 生成新的随机密码 (20 个 base32 字符, 100 bit), 替换旧密码 (只存 SHA-256), 显示一次, 然后重启服务, 由此清除所有会话和锁定 (两者都在内存中). 忘记密码时就用它重置. 不允许用户自选密码: 随机密码的强度使在线猜测在任何速率下都不可行, 也排除了密码复用; 因此慢哈希 (bcrypt) 不必要.
- 失败锁定: 登录, 注册码, 设备令牌的每次认证失败都按来源 IP (IPv6 按 /64) 计数, 1 小时内 10 次失败后该 IP 的所有认证请求被拒绝 1 小时 (429), 成功登录清零, 失败均记日志. 不做全局锁定: 100 bit 的密码和 60 bit, 15 分钟有效, 一次性的注册码即使面对大量 IP 也无法穷举, 全局锁定只会给攻击者一个让管理员无法登录的杠杆.
- 会话: 32 字节随机令牌, Cookie `__Host-kaze-sync`, `Secure; HttpOnly; SameSite=Strict; Path=/`; 绝对有效期 7 天, 不滑动续期; 登出即失效.
- CSRF: Go 1.25 标准库的 `http.CrossOriginProtection` (检查 `Sec-Fetch-Site` 与 `Origin`) 加 `SameSite=Strict`, 不需要表单令牌.
- 响应头: `Content-Security-Policy: default-src 'self'`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`. 不发送 HSTS (自签名模式下它会让浏览器拒绝例外). 服务器设置 `ReadHeaderTimeout` 等超时, 防止慢速连接耗尽资源.
- 不做双因素认证 (TOTP): 它防的是 "密码被猜中或复用", 随机生成的密码已消除这两种情况; 剩下的 "管理员电脑被入侵" 通常同时暴露会话和 TOTP 密钥. 代价约 100 行加二维码和恢复流程, 不值得.

## 5. 同步算法

### 5.1 客户端本地状态

每个已订阅文件夹一个 SQLite:

- `local(path, key, seq, hash, blocks, size, mtime_ns, exec)`: 每个已同步路径 "上次与服务端一致时" 的状态, 即提交时的 base; `blocks` 用于下载时复用本地已有的块; 索引 `(hash)` 用于按整文件哈希找到可复用的本地文件.
- `meta`: `cursor`, `rules_hash`.

### 5.2 变更检测

- 文件事件只是提示: 递归监视同步根, 收集变化的目录, 去抖 2 秒后只扫描这些目录. 监视器报告溢出或出错时退回全量扫描 (`ReadDirectoryChangesW` 溢出时返回 0 字节或 `ERROR_NOTIFY_ENUM_DIR`, 微软文档要求重新枚举; Linux 对应 `IN_Q_OVERFLOW`).
- 启动时, 配置变化时, 以及每小时做一次全量扫描, 兜住漏掉的事件 (与 Syncthing 默认一致).
- 扫描逐目录 `ReadDir` (拿到精确的文件名大小写), 应用排除规则并剪枝被排除的目录, 与 `local` 比较: `size` 与 `mtime_ns` 都没变视为未改, 不读内容; 有变化则计算哈希, 与 `local` 相同只更新 stat 字段, 否则为修改; `local` 中有而目录中没有为删除; 反之为新增 (`base_seq=0`).
- 稳定性: 只提交 mtime 已超过 5 秒的文件, 并且计算哈希前后各 stat 一次, 不一致则本轮跳过 (文件正在写入).

### 5.3 分块: 固定 256 KiB

- 文件按 256 KiB 切块, 每块 SHA-256, 文件 = 有序的块哈希列表; 另有整个文件的 SHA-256 作为文件标识 (冲突判断, 改名复用, 落地校验都用它). 两种哈希在同一次读取中算出. 不超过 256 KiB 的文件只有一块, 块哈希即文件哈希.
- 上传只传服务端缺少的块, 下载只取本地没有的块; 服务端各版本共享未变化的块, 所以保留许多个版本的 1 GB 数据库, 代价只是各版本之间变化的块.
- 选择固定大小而不是内容定义分块 (CDC): SQLite 按固定大小的页原地改写, 虚拟机磁盘镜像按扇区原地改写, 偏移都不移动, 固定块正好对齐这种修改; 在文件末尾追加也只影响最后的块. CDC 只在 "大文件中间插入或删除数据" 时更好, 而用户的大文件不是这种类型 (docx 等压缩格式每次保存整体重写, 两种方式都无能为力). 固定块不需要分块库, 块列表不需要存每块长度.
- 块大小取 256 KiB (`proto.BlockSize`, 已实现, 用户 2026-11-02 由 1 MiB 改为 256 KiB, 以降低小写入的流量放大): 1 GB 文件约 4096 块, 1 TB 数据约 400 万个块文件. GC 需要在内存中比对这些块的哈希, 它是服务端内存随数据量增长的唯一部分, 由预算和分趟限制 (第 7 节). 代价是分散的小写入仍会放大: 一次检查点改动 k 个分散的 4 KiB 页, 最多上传 k × 256 KiB. 一旦投入使用不再更改.

### 5.4 上传

扫描得到修改或新增的文件后, 带块列表 `POST /commit`. 结果为 `need` 时, 按需要的块从文件中读出 (读取时校验块哈希, 不符说明文件刚被改动, 放弃本轮), 打包 `POST /blocks`, 然后重新提交. `ok` 时用返回的 `seq` 和当前 stat 更新 `local`; `conflict` 按第 6 节处理; `rejected` 记为错误, 等文件再次变化时重试. 同一批中删除排在新增之前 (6.4). 断点续传以块为单位自然实现: 已到达服务端的块不会再被要求.

### 5.5 下载与应用

`Pull` 读取该文件夹 `cursor` 之后的变更, 每页分三遍处理: 第 1 遍把 key 相同的删除与新增 (仅大小写改名) 合并为一次 rename (必须最先做, 否则在不区分大小写的文件系统上写入 `A.txt` 会覆盖 `a.txt`, 随后删除 `a.txt` 又把它删掉); 第 2 遍新增与修改; 第 3 遍删除. 每页处理完持久化 `cursor`, 但 `cursor` 停在第一条未能应用的条目之前 (推迟, 被占用, 出错), 下次 `Pull` 从那里重读; 已应用的条目靠 "`seq` 等于 `local` 中的 `seq`" 和磁盘状态检查而无副作用地重复跳过. 客户端另外记住已读到的最新 `seq` (`seen`, 只在内存中), 长轮询的 `cursors` 报的是它, 所以暂时应用不了的条目不会让长轮询反复立即返回, 由重试定时器再次 `Pull`.

单条处理规则:

- 被有效排除规则排除: 跳过. `seq` 等于 `local` 中的 `seq`: 自己提交的回声, 跳过.
- 本地磁盘状态与 `local` 不一致 (有未提交的本地修改): 跳过, 由下次 `Scan` 经冲突流程解决. 下载永远不覆盖未同步的本地内容.
- 目标是正在使用的 SQLite 数据库 (5.6): 推迟, 上报 `in_use`.
- 新内容: 用 12.3 节的原子落地写入同目录的临时文件. 内容按块依次来自: 本地有相同整文件哈希的未改动文件 (改名或复制, 完全不走网络); 同一路径旧版本中哈希相同的块 (由 `local.blocks` 定位, 局部修改的大文件只下载变化的块); 其余用 `POST /blocks/get` 成批下载 (每批 64 个块位置, 批内重复的块只请求一次). 冲突副本改名后的文件也是块来源 (服务端版本与冲突副本常共享大部分块). 整文件哈希不符 (本地来源恰好被改动) 时放弃本地复用重新下载. 设置 mtime 与可执行位, 复查目标后 rename, 更新 `local`.
- 删除: 确认磁盘与 `local` 一致后删除文件, 自下而上删除因此变空的父目录 (到根为止).

残余风险: 最后一次 stat 与 rename 之间的极短窗口内若有程序写入目标, 这次写入会被覆盖. Syncthing 和 Nextcloud 客户端都有同样的窗口, 再缩小需要系统特定的锁, 不值得.

### 5.6 正在使用的文件与 SQLite 数据库

风险: 应用写入时复制数据库会得到撕裂或不一致的文件; SQLite 的 `-wal` 和 `-journal` 旁文件中还有尚未写回主文件的事务或回滚所需的原始页; 应用可能连续数小时打开数据库, 5 秒稳定期并不能保证一致.

客户端只做三件事, 都是内置行为, 不需要配置:

1. 识别 SQLite 数据库: 文件头 16 字节为 `SQLite format 3\0` (哈希时本来就要读).
2. 若同目录存在非空的 `<name>-wal` 或 `<name>-journal`, 该数据库视为正在使用: 不上传, 也不在下载时覆盖它, 上报 `in_use`, 每次扫描重试. 在 WAL 模式下这意味着 "应用关闭 (最后一个连接检查点并删除 `-wal`) 后才同步"; 在回滚日志模式下, 没有进行中的事务时主文件本身就是一致的, 进行中的事务会改变 mtime, 被 5.2 的前后 stat 比较捕获.
3. `<name>-wal`, `<name>-shm`, `<name>-journal` 在 `<name>` 存在时内置排除. 旁文件永远不需要同步: 只有不存在非空旁文件时才同步主文件, 此时主文件已包含全部数据.

文档写明的限制与建议: WAL 模式且长期打开的数据库只在应用关闭后同步; 需要更早同步时, 让应用执行 `PRAGMA wal_checkpoint(TRUNCATE)`. 同一个数据库不要在两台机器上同时使用, 否则产生冲突副本. 虚拟机镜像在虚拟机关机后才能可靠同步; 其他独占打开文件的程序 (如 Outlook PST) 在 Windows 上读取会失败, 按 `in_use` 重试. Linux 上无法知道其他进程是否以写方式打开了普通文件, 只依赖稳定期和前后 stat 比较.

不在 Windows 上用 "拒绝写共享" 方式打开文件来探测写者: 我们读取期间其他程序将无法打开该文件写入, 会让正常保存失败, 而 SQLite 的情况已由第 2 条覆盖.

### 5.7 Windows 文件锁与路径

- 读取遇到 `ERROR_SHARING_VIOLATION` 或 `ERROR_LOCK_VIOLATION`, rename 或删除一个被其他程序打开的文件失败: 都按 30 秒, 2 分钟, 10 分钟退避重试, 之后每次扫描重试, 上报 `in_use`. 下载时保留临时文件.
- NTFS 目录项中的大小和时间对仍打开着的文件可能延迟更新, 扫描因此可能在应用关闭文件后才发现修改. 这与 "不同步正在写入的文件" 的目标一致, 不做额外处理.
- 长路径: Go 的 `os` 包自动为长绝对路径加 `\\?\` 前缀.

### 5.8 崩溃安全

原则: 只在一步成功后才更新本地状态; "整文件哈希相同即采纳" 让每一步都可以安全重做.

| 崩溃时机 | 重启后的结果 |
|---|---|
| 上传块包中途 | 已完整到达的块保留, 重新提交时只被要求剩余的块 |
| 提交成功, 未更新 `local` | 再次提交旧 base 得到 `conflict`, 哈希相同, 采纳 |
| 下载到临时文件中途 | 原子落地从临时文件已有长度处续写 |
| rename 完成, 未更新 `local` | 扫描发现变化, 提交得到 `conflict`, 哈希相同, 采纳 |
| 处理完一页变更, 未保存 `cursor` | 重新拉取该页, 每条都因 `seq` 或哈希相同而跳过 |

服务端块先写入 `tmp/`, 校验并 fsync 后 rename 进 `blobs/`; 元数据提交使用 `synchronous=FULL`.

## 6. 冲突与跨平台

### 6.1 策略: 基于 base 的乐观并发加冲突副本

每个提交携带 base (上次同步时的 `seq`), 服务端只接受基于当前状态的修改, 即先到服务端者胜. 被拒后客户端按下表处理:

| 本地 | 服务端当前 | 处理 |
|---|---|---|
| 修改 | 修改, 内容相同 | 采纳, 只更新本地 base |
| 修改 | 修改, 内容不同 | 本地文件改名为冲突副本并作为新文件上传; 原路径下载服务端版本 |
| 修改 | 已删除 | 修改优先: 以服务端当前 `seq` 为 base 重新提交 |
| 删除 | 修改 | 删除让步: 下载服务端版本 |
| 删除 | 已删除 | 采纳 |
| 新建 | 已存在, 内容相同 | 采纳 (从 Nextcloud 迁移时多台客户端已有相同文件) |
| 新建 | 已存在, 内容不同 | 同 "修改 vs 修改" |

冲突副本命名: `report.conflict-laptop-20261101-153012.docx` (扩展名前插入设备名和本地时间). 冲突副本作为普通文件同步到所有客户端, 并作为通知列在 Web UI (4.3); 用户在客户端上比较后删除多余的一份. 不依赖各机器的时钟决定胜负 (Syncthing 按 mtime 决定). 被覆盖的内容无论如何都在 `versions` 中.

### 6.2 离线编辑

与在线相同, 只是提交推迟. 重新上线时先 `Scan` 后 `Pull`.

### 6.3 改名与移动

协议中没有改名操作: 改名是删除加新增, 内容相同所以服务端不需要任何块上传, 其他客户端按整文件哈希在本地复制 (5.5). 若 A 移动目录的同时 B 修改了其中某个文件, A 对旧路径的删除被拒绝 (删除让步), 该文件以修改后的内容留在旧位置. 不丢数据.

### 6.4 大小写

服务端每个文件夹的命名空间不区分大小写 (存活路径的 `key` 唯一). Linux 客户端同时有 `A.txt` 和 `a.txt` 时, 后提交的被拒绝 (`case_collision`), 显示为通知. Windows 上仅大小写改名产生同 key 的删除加新增, 同批中删除在前, 服务端顺序处理; 其他 Windows 客户端合并为一次 rename. 一般情况下客户端先提交不依赖任何删除的新增, 再提交删除, 最后提交依赖删除的新增 (同 key, 或文件与目录同名): 移动一个大目录时其他客户端先看到新增, 此时旧文件还在本地, 可按整文件哈希本地复制, 即使两批超过单次提交的 1000 个操作也不下载任何块.

### 6.5 Windows 非法名称

服务端拒绝 (`invalid_name`): 含 `<>:"\|?*` 或 0x00 到 0x1F; 任一路径段以 `.` 或空格结尾; 路径段 (去掉扩展名后, 不区分大小写) 为 `CON`, `PRN`, `AUX`, `NUL`, `COM1` 到 `COM9`, `LPT1` 到 `LPT9`. 在服务端统一检查 (用户决定), 命名空间在两种系统上都能落地. Linux 上的非 UTF-8 文件名由客户端跳过.

### 6.6 文件与目录同名

提交 `x` 时存在存活的 `x/...`, 或提交 `x/y` 时 `x` 是存活文件, 返回 `type_conflict`. 被拒的一方把本地的文件或整个目录改名为冲突副本名, 然后照常同步. 服务端因此拒绝时客户端把 `cursor` 退回 0 做一次完整对账 (已一致的条目都被跳过, 只多读一遍变更列表). 下载 `x` 而本地 `x` 是目录时: 目录内全是已同步且未改动的文件则等待服务端的删除条目先到, 否则把目录改名为冲突副本名; 下载 `x/y` 而本地 `x` 是文件时同理.

### 6.7 符号链接, 权限, mtime, 行尾

- 符号链接和 Windows 联接点不跟随, 不同步, 上报 `skipped`.
- 只同步可执行位 (用户决定): 原子落地通过临时文件替换目标, 无论如何都要为新文件决定 mode; 同步一个布尔值与 "保留本地原 mode" 代价相同, 而行为更好. Windows 客户端不读写它, 提交时沿用 base 中的值.
- mtime 原样同步; 内容按字节原样传输, 不转换行尾和编码; 路径按原样保存, 只在计算 `key` 时做 NFC.

## 7. 版本管理

- 任何覆盖或删除都把旧内容移入 `versions`. 保留规则只有一条: `keep_days` (默认 30 天, 按 `replaced_at` 计算). 它在总览页随时可改, 存于 `meta`, 立即生效, 不需要重启; 保存后立即触发一次 GC, 清除按新值已过期的版本. 分块使大文件的多个版本只占变化部分的空间, 因此不需要再限制每个路径的版本数.
- 恢复 (Web UI 文件夹页):
  - 单个版本 (含已删除文件的最后内容): 作为新的当前状态提交, 当前内容 (若有) 进入 `versions`, 随后同步到所有客户端. 恢复从不改写或删除历史.
  - 目录恢复到某一时刻: 对目录下每个路径取该时刻的状态 (`created_at <= T < replaced_at` 的版本, 或 `created_at <= T` 的当前状态), 与当前不同且有内容的就恢复. 只恢复内容, 从不删除该时刻之后新建的文件. 它同时覆盖 "误删整个目录" 和 "一批文件被错误覆盖" 两种情况.
- GC: 每天一次, 以及 `keep_days` 保存后立即一次; 同一时刻只运行一个 GC (已有 GC 运行时, 新的触发等它结束后再运行一次). 删除超出 `keep_days` 的 `versions` 行, 收集所有文件夹库中 `files` 与 `versions` 引用的块哈希 (内存做法见下), 删除未被引用且修改时间超过 1 小时的块 (宽限期保护已上传但尚未提交的块), 清理 `tmp/`.
- GC 的内存预算与分趟 (用户 2026-11-02, 替代原先 "哈希集合约 400 MB" 的做法): 引用的块哈希收集为排序的 32 字节数组 (`[][32]byte`, 不用 map; 每个引用 32 字节, 无额外开销), 排序后去重, 用二分查找判断某个块是否被引用. 内存预算是常量 `GCMemoryBudget` (`internal/server/gc.go`, 默认 64 MB, 不做成配置项).
  - 先读各文件夹库的块总数 (引用数的上界, 各版本重复引用也计入), 所需内存 = 引用数 x 32 字节. 不超过预算时只有 1 趟, 就是纯内存方案.
  - 超过预算时分 k 趟, k = 所需内存 / 预算 向上取整 (至多 256). 第 p 趟 (p = 0 到 k-1) 处理哈希首字节在 [p x 256 / k, (p+1) x 256 / k) 的块: 扫一遍所有文件夹库的块列表, 只把首字节在该范围内的哈希收入数组, 排序去重, 再与 `blobs/` 下对应首字节的前缀目录比对, 删除未被引用且超过宽限期的块. 各趟复用同一个数组, 所以峰值内存约为预算 (加首字节分布不均的约 1/64 余量), 与数据量无关; 代价是块列表被扫描 k 遍.
  - 宽限期, `tmp/` 清理, 单实例运行, `keep_days` 触发等语义不变. `tmp/` 清理与分趟无关, 只做一次.
  - 实现状态 (M7 完成): 收集与分趟的核心 (`sweepPasses`, `collectRefs`, `folderRefs`) 与 `blobstore.SweepBlobs` / `SweepTmp` 由 `gc()` 调用; `requestGC` 保证同一时刻只运行一个 GC (运行中的新请求让它结束后再运行一次); `gcLoop` 每天请求一次, 上次请求时间存在全局 `meta` 的 `last_gc` 中 (重启后继续计时, 服务启动一分钟内不运行); 总览页保存 `keep_days` 时立即请求一次. 版本删除先于块收集, 日志 `msg=gc` 一行记录删除的版本, 块, `tmp/` 文件数, 趟数与耗时.
  - 宽限期对 "上传后尚未提交" 的块成立; 对 "早已无引用的旧块被新提交重新引用" 的情形, 由 `blobstore.Use` 保证: 提交检查块是否存在时, 对超过 10 分钟的旧块刷新修改时间 (小于 1 小时宽限), Put 与 Use 持共享锁, 清扫在删除每个块之前持独占锁并重新读取修改时间, 所以收集引用与删除之间的新提交不会丢块.
  - 实测 (基准 `BenchmarkGCCollect`, 内存中的合成数据, i9-13900): 收集 100 万个引用约 0.15 s, 数组约 31 MB; 400 万个约 0.65 s, 数组约 122 MB; k = 2 与 k = 4 的峰值分别降到约 1/2 与 1/4 (400 万: 63 MB 与 33 MB), 耗时只增加 3% 到 15% (基准中的 "扫描" 是内存生成, 真实的 SQLite 扫描每趟更慢, 见下表的说明).

数据量与服务端内存 (块 256 KiB; 引用数 = `files` 与 `versions` 中的块引用总数, 同一块被多个版本引用时重复计入; "对应数据量" 按每个引用 256 KiB 估算, 去重后实际更小):

| 引用数 | 对应数据量 | 引用数组 | 预算 64 MB 的趟数 k | GC 时服务端内存峰值 |
|---|---|---|---|---|
| 100 万 | 约 256 GB | 约 32 MB | 1 | 约 24 + 32 = 56 MB |
| 200 万 | 约 512 GB | 约 64 MB | 1 | 约 24 + 64 = 88 MB |
| 400 万 | 约 1 TB | 约 128 MB | 2 | 约 24 + 64 = 88 MB |
| 800 万 | 约 2 TB | 约 256 MB | 4 | 约 24 + 64 = 88 MB |
| 1600 万 | 约 4 TB | 约 512 MB | 8 | 约 24 + 64 = 88 MB |

常驻内存约 24 MB (M3 实测, 与数据量基本无关; 文件夹库按需打开, 空闲连接各占少量缓存). GC 额外占用约每百万个引用 32 MB, 乘以 1/k. 各机器适合的数据量 (系统与页缓存至少留一半内存, 所以预算取内存的 1/8; 预算改常量后重新编译即可):

| 服务端内存 | 建议预算 | 单趟 (k = 1) 可处理 | 默认或建议预算下 4 TB 的趟数 | 说明 |
|---|---|---|---|---|
| 512 MB | 64 MB (默认) | 200 万引用, 约 512 GB | 8 | 目标配置; 超过 512 GB 时 GC 多扫几遍, 内存不变 |
| 1 GB | 128 MB | 400 万引用, 约 1 TB | 4 | 1 TB 数据单趟完成 |
| 2 GB | 256 MB | 800 万引用, 约 2 TB | 2 | 2 TB 数据单趟完成 |

趟数多只让 GC 变慢 (每天一次, 在后台), 不影响同步. 每趟要扫描全部文件夹库的块列表 (`files` 与 `versions` 的 `blocks` 列, 1 TB 约 130 MB 数据), 磁盘读取量 = k x 该大小; 每趟耗时取决于盘速, 须在目标路由器上实测并记入验证报告 (尚未测量).

- `kaze-syncd export <dir> [--folder F] [--at <time>]`: 在服务端本机把某时刻的树导出为普通文件, 用于灾难恢复和迁出. 与目录恢复共用 "某时刻状态" 的查询.

## 8. 排除系统

### 8.1 语法: gitignore 语义

规则为 UTF-8 文本, 每行一条, 匹配对象是相对文件夹根的路径 (分隔符一律 `/`). 以下语义逐条取自 [gitignore 文档](https://git-scm.com/docs/gitignore):

- 空行忽略; `#` 开头为注释; 行尾未转义的空格去除; `\` 转义任意字符; 以 `\` 结尾的模式无效 (保存时报错).
- `!` 前缀取反. 结尾 `/` 只匹配目录; 目录被排除即其整个子树被排除.
- 开头或中间含 `/` 的模式相对文件夹根; 否则匹配任意层级.
- `*` 匹配 `/` 以外的任意字符串, `?` 匹配 `/` 以外的单个字符, `[a-z]`, `[!a-z]`, `[[:alpha:]]` 等为字符类 (fnmatch FNM_PATHNAME 语义).
- `**/x` 任意层级; `x/**` 其内全部; `a/**/b` 中间零或多层; 其他连续星号视为 `*`.
- 最后一条匹配的规则决定结果.
- 父目录被排除时, 其下内容不能被 `!` 重新包含, 扫描因此可以剪枝. 只保留某个子目录时逐级展开, 例如 `/*` 后接 `!/Docs/` (另见 3.2 的例子).
- 大小写: Windows 设备不区分, Linux 设备区分 (对应 Git wildmatch 的 `WM_CASEFOLD`).
- 与 Git 的差异: 没有逐目录 `.gitignore`, 规则来自 3.2 的两层拼接, 都锚定在文件夹根; 不支持 `{a,b}` (Git 也不支持).

示例:

| 规则 | 含义 |
|---|---|
| `node_modules/` | 任意层级名为 `node_modules` 的目录 |
| `/Videos/` | 只排除根下的 `Videos` 目录 |
| `*.iso` | 任意层级的 `.iso` 文件 |
| `Projects/**/bin/` | `Projects` 下任意深度的 `bin` 目录 |
| `/*` 后接 `!/Docs/` | 只同步根下的 `Docs` 目录 |

内置排除 (不可覆盖): `.syncroot`, `.syncpart-*`, 以及 5.6 中的 SQLite 旁文件.

### 8.2 默认规则

全局规则的初始内容 (出厂默认, `internal/server/db.go` 的 `defaultRules`, 约 35 条), 参考 Nextcloud 客户端自带的 `sync-exclude.lst` (去掉其 `]` 前缀后即为 gitignore 语法):

- Office 与 Windows: `~$*`, `*.tmp`, `Thumbs.db`, `desktop.ini`, `photothumb.db`, `System Volume Information/`, `$RECYCLE.BIN/`.
- Nextcloud 客户端遗留: `.sync_*.db*`, `.owncloudsync.log`, `.nextcloudsync.log`.
- 编辑器与锁文件: `*~`, `.~lock.*`, `.*.sw?`, `*.kate-swp`.
- 下载中的临时文件: `*.part`, `*.filepart`, `*.crdownload`.
- macOS: `.DS_Store`, `.Trashes/`, `.Spotlight-V100/`, `.TemporaryItems/`, `.DocumentRevisions-V100/`, `.fseventd/`, `.apdisk`, `*.textClipping`, `._*`.
- Linux: `.Trash-*/`, `.directory`, `.fuse_hidden*`, `.nfs*`.
- 其他同步工具的元数据: `.stfolder/`, `.stversions/`, `.stignore`, `*.unison`, `.sync.ffs_db`.

不收 Nextcloud 列表中的 `My Saved Places.`, `.synkron.*`, `.symform*`, `*.gnucash.tmp-*` (过于小众). 用户自己的规则 (如 `.venv/`, `node_modules/`) 不进出厂默认, 部署后在 Web UI 中按全局或按文件夹添加. 出厂默认只在初始化时写入 `meta`, 之后以 Web UI 中的内容为准.

### 8.3 规则变化时的行为

- 新排除了已同步的文件: 从 `local` 删除这些行, 不向服务端提交删除. 本地文件保留, 服务端和其他设备上的文件不受影响.
- 取消排除: 用 `cursor=0` 全量对账, 两边差异按 6.1 收敛, 不需要专门的代码路径.

由于规则不按设备区分, 同一规则在所有设备上排除相同的路径 (只有大小写敏感性随系统不同).

### 8.4 实现

Git `wildmatch.c` 的 Go 移植, 服务端 (保存时校验) 与客户端共用 (12.1). 性能目标: 10 万路径 x 100 条规则在 1 秒内; 正确性用 `git check-ignore --no-index` 差分测试.

## 9. 部署

### 9.1 服务端: 只支持 OpenWrt

前提: 数据盘已在 OpenWrt 中挂载 (block-mount 在启动早期按 `/etc/config/fstab` 挂载). 在路由器上以 root 执行:

```
./kaze-syncd install --data /mnt/data/kaze-sync             # 可选 --listen :41873, --cert <f> --key <f>, --wan
/mnt/data/kaze-sync/kaze-syncd admin-password               # 显示一次随机密码
```

`kaze-syncd install` 是安装和升级的唯一入口, 可以重复执行:

- 校验 `--data` 不在根文件系统 (overlay) 上: 与 `/` 的设备号相同时拒绝安装, 防止数据盘未挂载时把数据写进很小的 overlay.
- 复制自身到 `<data>/kaze-syncd`. 二进制放在数据盘上, 不占 overlay, 也不受 sysupgrade 影响.
- 创建系统用户 `kaze-sync`: OpenWrt 没有 `useradd`, 用 OpenWrt 自带的 `/lib/functions.sh` 中的 `user_exists`, `user_add`, `group_add` (它们追加 `/etc/passwd`, `/etc/shadow`, `/etc/group`, `user_add` 未给 UID 时从 32768 起选第一个空闲 UID).
- 创建数据目录, 初始化或迁移数据库, 首次安装生成自签名证书并打印指纹; 端口在首次安装时随机选取 (20000 到 59999 之间), 存入 `meta` 并打印, 之后保持不变, `--listen` 可改; `--cert`/`--key` 存入 `meta`; 之后不带 `--cert` 执行时沿用已有的证书模式 (暂不提供切回自签名). 高端口不需要任何特权或能力, 也避开路由器上 LuCI 占用的 80 和 443.
- 写入 procd 初始化脚本 `/etc/init.d/kaze-sync` 并执行 `/etc/init.d/kaze-sync enable` 和 `restart`:

```sh
#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1
start_service() {
	procd_open_instance
	procd_set_param command /mnt/data/kaze-sync/kaze-syncd run --data /mnt/data/kaze-sync
	procd_set_param user kaze-sync
	procd_set_param respawn
	procd_set_param stderr 1
	procd_close_instance
}
```

  `respawn` 不带参数时使用 procd 的默认值 (3600 秒内失败 5 次后停止重启, 每次间隔 5 秒). `stderr 1` 把日志送入 syslog (`logread`).
- 保证 sysupgrade 后仍然可用: sysupgrade 只保留 `/etc/sysupgrade.conf`, `/lib/upgrade/keep.d/*` 中列出的路径 (用 `find` 递归, 保留普通文件和符号链接) 以及已修改的软件包 conffiles. `/etc/passwd`, `/etc/group`, `/etc/shadow`, `/etc/sysupgrade.conf` 都是 base-files 的 conffiles, 修改后自动保留. `install` 在 `/etc/sysupgrade.conf` 中追加 (已有则不重复) `/etc/init.d/kaze-sync` 和 `/etc/rc.d/S99kaze-sync`. 不使用 `/lib/upgrade/keep.d/`: 那个目录本身不被保留, 写在那里的列表会在第一次升级后消失. 二进制, 数据库和证书都在数据盘上.
- `--wan` (Q15, 用户选 B): 在 `/etc/config/firewall` 中创建或更新名为 `kaze-sync` 的规则 (与工程同名, Q16) (src `wan`, proto `tcp`, 本机端口, target `ACCEPT`) 并重新加载防火墙; 不带 `--wan` 时删除该规则. 防火墙配置是 conffile, sysupgrade 后保留.

`kaze-syncd run` 在 `<data>/var/meta.db` 不存在时直接报错退出, 不初始化新库 (数据盘未挂载时不会在空目录上启动). `install`, `admin-password`, `export` 以 root 运行, 它们创建的数据库旁文件在退出前改为属于 `kaze-sync` 用户.

静态链接 (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64`) 的 Go 二进制在 OpenWrt 的 musl 系统上直接运行.

来源: OpenWrt 源码 [sysupgrade](https://github.com/openwrt/openwrt/blob/main/package/base-files/files/sbin/sysupgrade), [base-files conffiles](https://github.com/openwrt/openwrt/blob/main/package/base-files/Makefile), [functions.sh](https://github.com/openwrt/openwrt/blob/main/package/base-files/files/lib/functions.sh), [procd.sh](https://github.com/openwrt/openwrt/blob/main/package/system/procd/files/procd.sh).

### 9.2 客户端

```
kaze-syncc enroll https://sync.example.org:41873 K7QF-M2XD-9PLA --fingerprint sha256:...   # 证书文件模式不带 --fingerprint
```

一条命令完成全部本地设置: 换取令牌, 写 `device.json`, 配置自启动并立即启动 agent. 之后的一切在 Web UI 中进行.

- Linux: 写入 systemd 用户 unit 并 `systemctl --user enable --now`.
- Windows: 在 `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` 写入 `kaze-syncc-agent.exe run`, 登录后无窗口运行, 不需要管理员权限.
- `--no-autostart` 跳过自启动, 供测试使用. 卸载在文档中给出手动步骤 (删除 Run 键或用户 unit 与状态目录), 不提供命令.

### 9.3 升级

服务端: 用新二进制再执行一次 `kaze-syncd install --data <同一目录>`. OpenWrt 本身的 sysupgrade 见 9.1. 客户端: 替换二进制后重启 agent. 两端的 SQLite 按 `schema_version` 顺序迁移 (服务端在 `install` 中逐库执行: 先全局库, 再每个文件夹库, 各库有自己的版本号). 协议不兼容时使用新的 `/api/vN/` 前缀; 约定先升级服务端再升级客户端, 不维护兼容矩阵; 版本过旧的客户端收到明确错误, 版本号显示在总览页.

### 9.4 备份

`var/meta.db` 和 `var/folders/` 下的全部库必须备份 (没有它们, 块无法还原成文件, 且各库须取自同一时刻); 建议同时备份 `var/tls/` (很小), 否则还原后证书重新生成, 指纹变化, 全部客户端需重新注册; 还原后执行 `kaze-syncd install` 修正属主, 完整步骤见 `docs/operations.md`. 备份步骤: `/etc/init.d/kaze-sync stop`, 复制 `var/meta.db` 与 `var/folders/` (含各库的 `-wal` 文件, 停服务后 WAL 已合并), `/etc/init.d/kaze-sync start` (几秒钟), 再复制 `var/blobs/` (块写入后不再修改, 可增量复制). 避开 GC 运行时刻. 另外可定期 `kaze-syncd export` 得到普通文件形式的快照. 备份应放在另一块盘或另一台机器上.

## 10. 语言与依赖

Go 1.25 或更高 (需要 `http.CrossOriginProtection`). 理由: 在 WSL 中交叉编译即得到 Windows 和 OpenWrt 可用的静态二进制; 标准库覆盖 HTTPS, TLS, 证书生成, SHA-256, `html/template`, `embed`, `log/slog`; Syncthing 在同一领域用 Go 证明了可行; 编译快. Rust 的优势在这里换不来可感知的收益 (瓶颈是磁盘和网络).

依赖 (只列非标准库):

| 库 | 用途 | 说明 |
|---|---|---|
| `modernc.org/sqlite` | 两端元数据库 | 纯 Go, 无需 cgo |
| `github.com/fsnotify/fsnotify` | Linux 文件监视 | 不支持递归, 扫描时为每个目录添加监视; 溢出返回 `ErrEventOverflow`; 受 `fs.inotify.max_user_watches` 限制 |
| `golang.org/x/sys/windows` | Windows 递归监视, Run 键, 错误码 | 对同步根调用一次 `ReadDirectoryChanges` 且 `bWatchSubtree=TRUE` |
| `golang.org/x/text` | NFC, 大小写折叠 | 计算 `key` |

来源: [fsnotify](https://pkg.go.dev/github.com/fsnotify/fsnotify), [ReadDirectoryChangesW](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-readdirectorychangesw), [CrossOriginProtection](https://www.alexedwards.net/blog/preventing-csrf-in-go).

## 11. 测试策略

- 文件系统: 引擎通过 12.4 的 `FS` 接口访问文件系统; 同一套一致性测试同时跑在真实实现与内存替身上.
- 服务端不做假实现: 测试在进程内用 `httptest` 运行真实服务端, 通过包装 `http.RoundTripper` 注入网络错误和断连.
- 确定性多客户端模拟: N 个客户端各自一个内存文件系统, 每个客户端订阅多个文件夹, 共用一个服务端; 按随机种子生成操作序列 (编辑, 局部改写大文件, 删除, 改名, 移动目录, 离线, 修改全局与文件夹规则, 修改映射), 逐步调用 `Scan` 和 `Pull`, 静止后断言: (1) 收敛: 各客户端 (除去排除的部分) 与服务端当前状态一致; (2) 不丢数据: 任何曾在任一客户端上存在过的内容, 都能在某个客户端, 服务端当前状态或 `versions` 中找到. 失败时打印种子可重放.
- 崩溃测试: 在每个 FS 调用和 HTTP 调用处注入一次 "进程被杀", 重建引擎后继续, 断言同样的不变式.
- 分块: 局部改写大文件的 1 个块, 上传和下载都只传 1 个块; 改名大文件不传任何块.
- SQLite: 构造带非空 `-wal` 的数据库, 断言不上传, 不覆盖, 旁文件不同步; 删除 `-wal` 后正常同步.
- 通知: 每种来源出现后在汇总与明细中可见, 原因消除后消失; 服务端重启后冲突副本和长期离线通知仍在.
- Web UI 与安全: `httptest` 驱动表单流程; 带外部 `Origin` 的 POST 被拒绝; 同一 IP 第 11 次失败返回 429; 会话 7 天后失效.
- 排除系统: Git wildmatch 测试向量加 `git check-ignore --no-index` 差分测试.
- Windows: 在真实 NTFS 上运行锁定文件, 被打开文件上的 rename 与删除, 仅大小写改名, 长路径, 监视溢出.
- OpenWrt 安装: 只在 OpenWrt 测试虚拟机中真实运行 (procd, 用户创建, 防火墙规则, sysupgrade 后保留).
- 测试虚拟机与正式环境的差别: 虚拟机为 512 MB 内存, 不用交换空间 (与 512 MB VPS 同档, 用于测内存), 由官方 25.12.6 x86-64 ext4 镜像在宿主上用 `debugfs` 写入首次启动配置; 因 OpenWrt 的 open-vm-tools 不支持 VIX 的 `getGuestIPAddress`, 使用 e2e 配置中的固定 IP (`ip`) 而不是向 VMware 询问地址; 数据盘 (`/dev/sdb`) 由 `/etc/rc.local` 挂载, 没有安装 block-mount.
- 端到端: 专用测试环境 (OpenWrt 服务端虚拟机, WSL 中的 Linux 客户端, 宿主上的 Windows 客户端) 中由自动驱动程序运行, 见 `docs/plan.md`.

## 12. 可先行实现的模块

这些模块已由另一个 Agent 实现中, 接口不变.

### 12.1 排除规则匹配器 `internal/ignore`

```go
type LineError struct{ Line int; Msg string }
func Compile(lines []string, caseFold bool) (*Matcher, []LineError)
func (m *Matcher) Excluded(path string, isDir bool) bool      // 假定祖先目录未被排除
func (m *Matcher) ExcludedPath(path string, isDir bool) bool  // 检查每一级祖先
```

两层规则由调用方拼接后传给 `Compile`, 接口不受 Q5 影响. 验收测试: Git `t3070-wildmatch` 中适用于 `WM_PATHNAME` 的向量; 至少 2000 条随机规则与路径对比 `git check-ignore --no-index` (`caseFold=true` 时对比 `git -c core.ignoreCase=true`); 父目录被排除后 `!` 无效; 无效行返回带行号的 `LineError`; 10 万路径 x 100 条规则 1 秒内.

### 12.2 哈希与内容寻址块存储 `internal/blobstore` (已完成)

已按上一版改为整块写入: `Put(h Hash, r io.Reader) error`, `Has(h Hash) (bool, error)`, 删除 `ErrOffsetMismatch` 与部分续写; `Hash`, `ParseHash`, `String`, `HashReader`, `Open`, `Sweep` 不变; 块大小常量 `proto.BlockSize = 1 << 20`.

### 12.3 原子落地 `internal/client/place`

```go
type Expect struct{ Exists bool; Size, MtimeNs int64 }
type Meta struct{ MtimeNs int64; Exec bool }
func Place(fsys FS, dst string, h Hash, open func(offset int64) (io.ReadCloser, error), m Meta, expect Expect) error
var ErrTargetChanged, ErrHashMismatch, ErrLocked error
```

`h` 是整个文件的 SHA-256; 分块只改变调用方传入的 `open`, 它从 `offset` 所在的块开始, 依次从本地块来源或成批下载的块中提供内容.

### 12.4 文件系统接口与测试替身 `internal/client/fs`

接口与验收测试不变 (`ReadDir`, `Stat`, `Open`, `OpenAppend`, `Sync`, `Rename`, `Remove`, `MkdirAll`, `SetMeta`; 内存替身的 `CaseInsensitive`, `Lock`, `FailAfter`; `RoundTripper` 故障注入). 下载复用本地块需要在任意偏移读取, 由 `Open` 返回的读取器加 `io.Seeker` 断言或 `Read` 跳过实现; 若实现时发现必须改为 `io.ReadSeekCloser`, 这是唯一可能的调整.

### 12.5 目录变化监视 `internal/client/watch`

```go
func Start(root string, out chan<- string) (*Watcher, error) // "" 表示可能丢失事件
func (w *Watcher) Add(dir string) error
func (w *Watcher) Close() error
```

### 12.6 路径规则 `internal/proto/path`

```go
func Key(p string) string
func CheckPortable(p string) error
```

## 13. 删减与新增

按轻量原则, 相对第 2 版设计 (5.8k 行) 删除或合并的内容 (行数为非测试代码估计):

| 删减或合并 | 节省 (行) |
|---|---|
| Web UI 从 9 页减为 4 页: 设置并入总览, 添加设备并入总览, 回收站并入文件夹浏览, 冲突页与设备错误列表由通知取代且去掉 "删除副本 / 替换原文件" 操作 | 约 490 |
| 去掉所有下载: 旧版本下载, 冲突副本下载, 下载端点 | 约 50 |
| 按设备 (映射) 的规则层删除; 扫描间隔改为常量; 去掉暂停开关和 `max_versions` | 约 125 |
| 状态与配置端点并入长轮询; HEAD/PUT 偏移续传由块包取代 | 约 100 |
| 会话, 注册码, 设备状态三张表改为内存, `settings` 并入 `meta` | 约 60 |
| 去掉服务端配置文件与 TOML 依赖, 参数由 `install` 存入 `meta` 或在 Web UI 中修改 | 约 55 |
| 去掉 ACME 与 HSTS 逻辑及 `autocert` 依赖; 随机密码用 SHA-256, 去掉 bcrypt 依赖 | 约 50 |
| 服务端安装只支持 OpenWrt (procd), 去掉 systemd 与 OpenRC 两套服务定义和探测 | 约 30 |
| 去掉 `kaze-syncd init`, `kaze-syncd uninstall`, `kaze-syncc autostart` 子命令 | 约 60 |
| 错误种类从 7 种合并为 5 种 | 约 10 |
| 合计 | 约 1030 |

新增 (因需求而加):

| 新增 | 增加 (行) |
|---|---|
| 固定 256 KiB 块传输 (客户端分块, 缺块循环, 本地块拼装; 服务端块包, 缺块检查, GC 读块列表) | 约 300 |
| 通知: 汇总, 明细, 冲突副本索引, 长期离线判断 (无存储状态) | 约 130 |
| OpenWrt 安装: procd 脚本, 用户创建, `sysupgrade.conf`, 数据目录与 `meta.db` 检查, `--wan` 防火墙规则 | 约 70 |
| SQLite 三条规则 | 约 40 |
| 按 IP 锁定 | 约 40 |
| 全局规则层; `keep_days` 在 Web UI 修改并立即触发 GC | 约 35 |
| 证书文件模式 | 约 10 |
| 合计 | 约 625 |

考虑过但不加入: 通知的 "已读 / 忽略" 状态 (约 40 行加一张表, 会隐藏未解决的问题), 客户端或推送通知, 证书文件变化时自动重新加载 (约 10, 用续期工具重启代替), 两种 TLS 模式之间免重新注册的切换, 双因素认证 (约 100), 全局登录锁定 (约 20), 大批量删除保护 (约 60), 空目录同步 (约 150), 内容定义分块, Windows 拒绝写共享探测 (约 30), 配置修改历史, 客户端状态命令 (约 40).

按模块自下而上重新估算, 总规模约 4.46k 行 (上一版 4.34k): 本轮增加通知 (约 +130), 全局规则与 `keep_days` (约 +35), OpenWrt 安装与防火墙 (约 +40 净), 证书文件 (约 +10); 减少 ACME (约 -30), 映射规则 (约 -15), 冲突列表与设备错误列表 (约 -40), systemd 与 OpenRC (约 -30).

## 14. 风险与待决问题

主要风险:

- 同步引擎的 bug 可能在客户端之间传播错误的删除或覆盖. 对策: 第 11 节的不变式与崩溃测试; 服务端版本作为最后防线; 先在数据副本上试运行.
- 服务端是路由器: 公网访问需要公网 IPv4 或 IPv6 (运营商级 NAT 后无法从外部访问); 服务暴露在公网上, 依赖随机密码, 按 IP 锁定和以 `kaze-sync` 用户运行来限制风险.
- sysupgrade: 已定数据放在单独挂载的数据盘上 (用户 2026-11-02), 升级镜像只改写系统盘, 不影响数据. 测试虚拟机中仍用单独的数据盘验证 sysupgrade 后服务恢复.
- 256 KiB 块对分散的小写入仍有放大, 块文件数多 (1 TB 约 400 万个), GC 需按预算分多趟扫描块列表 (第 7 节), 数据量大时 GC 变慢但内存不变; 用真实数据库和真实数据量测量.
- WAL 模式且长期打开的 SQLite 数据库只在应用关闭后同步, 这是有意的限制 (5.6).
- Windows 文件锁和第三方程序 (杀毒软件, 索引服务) 的行为只能在真机上暴露.
- Linux inotify 监视数量上限: 超限时退化为每小时全量扫描, 不影响正确性.

待决问题: 无 (已答复的 Q1 到 Q16 已移除).

# kaze-sync User Manual

[English](user-manual.md) | [简体中文](user-manual.zh-CN.md)

This manual describes every operation a kaze-sync user performs: installing the server and the clients, enrolling devices, mapping folders, writing exclusion rules, dealing with conflicts, restoring old versions, backing up, upgrading, uninstalling and fixing problems. It assumes you can use a terminal, `ssh` and `scp`. For a first working setup in a few commands, start with the [README](../../README.md); this manual is for looking things up afterwards.

Two conventions:

- The Web UI is in Chinese only. Wherever this manual names a button, menu item or message, it gives the English meaning and, in code font, the exact text on screen, for example **Add device** (`添加设备`).
- Commands run on the server are marked `# server`, commands run on a client are marked `# client`. Replace `<...>` with your own value.

Deployment details that are the same for everyone (partitioning the only disk of an OpenWrt box, systemd hardening, the exact memory budget) live in [`docs/operations.md`](../operations.md), which is written in Chinese. This manual points to its sections instead of copying them.

## 1. How the pieces fit

- **Server** (`kaze-syncd`): one process that keeps all files and their old versions, and serves the Web UI. It runs on OpenWrt, or on x86-64 Linux with systemd or OpenRC (a VPS, a home server).
- **Client** (`kaze-syncc`): a background agent on each Linux or Windows computer. It watches local folders, uploads changes and downloads other devices' changes. Clients only ever connect to the server; they need no open port, so they work behind NAT.
- **Device**: a computer you enrolled. Each device has a name.
- **Folder**: a named set of files on the server. A folder is the same on every device that uses it.
- **Mapping**: the link between a folder and a local directory on one device, for example folder `Docs` is `D:\Docs` on the Windows laptop and `/home/me/Docs` on the Linux desktop. All configuration (folders, mappings, rules, retention) is done in the Web UI of the server and pushed to the clients, usually within a second.
- **Versions**: every overwrite or delete keeps the old content on the server for a number of days (30 by default), so you can restore it.

All Web UI actions are done by a single administrator with one random password.

## 2. Installing the server

### 2.1 Build the binaries

There are no release downloads; you build from source on any machine that has Go (the version named in `go.mod`, currently 1.27.3). The results are static and have no dependencies.

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncd ./cmd/kaze-syncd
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncc ./cmd/kaze-syncc
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kaze-syncc.exe ./cmd/kaze-syncc
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o kaze-syncc-agent.exe ./cmd/kaze-syncc
```

The last line builds the same program without a console window; Windows autostart uses it. Keep `kaze-syncc-agent.exe` in the same directory as `kaze-syncc.exe`. The server is supported only on x86-64.

### 2.2 OpenWrt

Prerequisite: a data disk that is mounted and is not the root file system (overlay). `install` refuses a data directory on the root file system, because the router's small flash would fill up and a system upgrade would erase it. If the router has only one disk, first create a data partition on it: see the section "OpenWrt 只有一块盘: 新建数据分区" in [`docs/operations.md`](../operations.md).

Copy `kaze-syncd` to the router, then as root:

```bash
./kaze-syncd install --data /mnt/data/kaze-sync        # server
/mnt/data/kaze-sync/kaze-syncd admin-password          # server: shows the admin password once
```

What `install` does: copies itself to `<data>/kaze-syncd`; creates the system user `kaze-sync`; creates and migrates the databases; generates a self-signed certificate; picks a random TCP port between 20000 and 59999 (kept on later runs); writes and enables the `procd` service `/etc/init.d/kaze-sync` and starts it; adds the service files to `/etc/sysupgrade.conf` so that `sysupgrade` keeps them. It prints the port and the certificate fingerprint (`sha256:...`). Write both down; you need them to enroll clients.

Options:

| Option | Meaning |
|---|---|
| `--data DIR` | Required. Absolute path made of letters, digits and `. _ - /` |
| `--listen :PORT` | Listen address instead of the random port; remembered for later runs |
| `--cert FILE --key FILE` | Use your own certificate instead of the self-signed one (section 2.4) |
| `--wan` | OpenWrt only: add a firewall rule named `kaze-sync` that accepts the port from the WAN zone. Without `--wan`, a rule left over from an earlier run is removed |
| `--new-epoch` | Only after restoring a backup (section 14) |

### 2.3 Plain Linux (systemd or OpenRC)

As root, with any data directory you like (it may be on the root file system):

```bash
./kaze-syncd install --data /var/lib/kaze-sync         # server
/var/lib/kaze-sync/kaze-syncd admin-password           # server
```

`install` detects the service manager itself (OpenWrt, then systemd, then OpenRC; anything else is refused). It creates the user `kaze-sync` (with `useradd`, or busybox `adduser` on Alpine), writes the service (systemd: `/etc/systemd/system/kaze-sync.service`; OpenRC: `/etc/init.d/kaze-sync`), enables it at boot and starts it. It does **not** touch any firewall and does not accept `--wan`: open the printed TCP port in your cloud provider's security group or in the local firewall (for example `ufw allow <port>/tcp` or `firewall-cmd --add-port=<port>/tcp --permanent`).

Day-to-day control:

| | systemd | OpenRC | OpenWrt |
|---|---|---|---|
| start, stop, restart, status | `systemctl start\|stop\|restart\|status kaze-sync` | `rc-service kaze-sync start\|stop\|restart\|status` | `/etc/init.d/kaze-sync start\|stop\|restart` |
| log | `journalctl -u kaze-sync` | syslog, tag `kaze-sync` (Alpine: `/var/log/messages`) | `logread` |

A crashed server restarts after 5 seconds. If the data disk is not mounted at boot, the service fails to start instead of creating an empty database.

The systemd unit is hardened (the service may only write below `<data>/var`). Add your own changes with a drop-in (`systemctl edit kaze-sync`), not by editing the unit, because the next `install` rewrites the unit. Details and the systemd version requirements are in [`docs/operations.md`](../operations.md), section "普通 Linux (systemd 或 OpenRC)".

`install` needs root and there is no supported way to run the server without it.

### 2.4 Port, certificate and firewall

- **Self-signed certificate (default).** Works for the internet and for a LAN alike. Valid for 100 years. Clients pin the certificate by its fingerprint, so the address and host name do not matter. Your browser will warn about the certificate once; compare the fingerprint it shows with the one `install` printed, then accept the exception.
- **Your own certificate.** `--cert /path/fullchain.pem --key /path/key.pem` (absolute paths, checked at install time). Browsers and clients then validate it against the system roots and clients enroll without `--fingerprint`. The service runs as user `kaze-sync` and reads these files itself at start, so that user must be able to read both. The server does not watch the files: after each renewal restart the service (for acme.sh, put the restart command in `--reloadcmd`). Later `install` runs keep certificate mode; there is no switch back to self-signed. Switching between the two modes needs every client to be enrolled again.
- **Port.** Random by default so that it is not found by scanning; `--listen :PORT` sets it. It is always HTTPS, and the Web UI and the client protocol share it.
- If you forget the port or the fingerprint, run `install` again with the same `--data` (it is safe to repeat; it prints both), or look for the line `listening` in the server log.

### 2.5 First login and the admin password

Open `https://<server>:<port>/` in a browser (Chrome or Edge 114+, Firefox 125+, Safari 17+) and sign in with the password that `admin-password` printed. The sign-in lasts 7 days.

`admin-password` can be run at any time. It sets a new random password, shows it once, and restarts the service, which signs out every session and clears every lockout. You cannot choose the password yourself. Run it as `<data>/kaze-syncd admin-password`, or with `--data DIR` from another copy of the binary.

Resources: the server uses about 24 MB of memory in normal operation; garbage collection (section 13) adds a bounded amount. A machine with 512 MB of memory handles data up to about 512 GB with one collection pass; larger data only makes the collection scan more often, never use more memory.

## 3. Installing and enrolling clients

A client needs the enrollment command from the Web UI. In the overview page, press **Add device** (`添加设备`), type a name (letters, digits, `-` and `_`, at most 32 characters, unique; it appears in conflict copy file names), and press **Generate enrollment code** (`生成注册码`). A dialog shows a one-time code that is valid for 15 minutes, with ready-to-copy commands for Linux and Windows, including the fingerprint when the server uses a self-signed certificate:

```text
kaze-syncc enroll https://sync.example.org:41873 K7QF-M2XD-9PLA --fingerprint sha256:...
```

The server address is whatever name or IP your clients can reach, with the port; it must be `https://`.

### 3.1 Linux client

Copy `kaze-syncc` to a **permanent** location, for example `~/.local/bin/`, because the autostart entry records the path it was run from. Then run the command from the dialog:

```bash
kaze-syncc enroll https://sync.example.org:41873 <CODE> --fingerprint sha256:<HEX>    # client
```

It saves the device token in `~/.local/state/kaze-sync/`, writes the systemd user unit `~/.config/systemd/user/kaze-sync.service`, enables it and starts the agent. Check with `systemctl --user status kaze-sync`. Linux clients need a systemd user session; other init systems are not supported. For the agent to run while you are not logged in, enable lingering with `loginctl enable-linger`. `--no-autostart` only enrolls and does not set up or start the service; then `kaze-syncc run` runs the agent in the foreground.

### 3.2 Windows client

Put `kaze-syncc.exe` and `kaze-syncc-agent.exe` together in a permanent directory, open a terminal there and run the command from the dialog (`kaze-syncc.exe enroll ...`). It saves the token in `%LOCALAPPDATA%\kaze-sync\`, registers the windowless agent in `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` under the name `kaze-sync` (no administrator rights needed) and starts it. The agent then starts at every sign-in.

### 3.3 After enrolling

The device appears in the device table of the overview page, with a green dot when it is online. It does not sync anything until you map a folder to it (section 5). To enroll a computer again (for example after reinstalling it), revoke the old device first or add a new device under another name, because device names are unique; then run the new command.

## 4. The Web UI

Every page has a top bar: the **kaze-sync** logo (back to the overview), the notification bell (section 9), the gear (settings) and sign-out. Page titles show where you are as a path (`Overview / Docs / projects`).

| Page | Address | What it is for |
|---|---|---|
| Sign-in | `/login` | Admin password |
| Overview (`总览`) | `/` | Folder table and device table. Buttons: **New folder** (`新建文件夹`), **Add device** (`添加设备`) |
| Folder | `/folders/<id>` | Browse files, restore, exclusion rules, delete the folder |
| Device | `/devices/<id>` | The device's mappings, revoke the device |
| Settings (`设置`) | `/settings` | Time zone, version retention, global exclusion rules |

The overview folder table shows for each folder its state (Synced `已同步`, Syncing `同步中` with the number of pending uploads, Errors `有错误`, Unmapped `未映射`, Device offline `设备离线`), the devices that map it, the number of files and the total size, and the number of errors and warnings. The device table shows the system, the last connection time, each folder's state on that device, the client protocol version (shown in red as "N, `过旧`" when it is too old, section 15) and whether the device has the latest configuration.

Pages update by themselves: the notification bell and the overview tables refresh within a few seconds of a change (at most once every 5 seconds). The folder's file list does not refresh itself, so that a big sync does not keep re-reading it; it shows **Content changed, refresh** (`内容有更新, 刷新`) instead.

Times are shown as `2026-11-03 22:48:27` in the browser's time zone by default; hover for the zone name. The pages need JavaScript for the menus, dialogs and live updates, and they are usable at phone width.

**Settings.** **Time zone** (`时区`): follow the browser, or fix an IANA zone for every browser; it only affects what the Web UI shows and what you type into it. **Version retention** (`版本保留`): days to keep old versions, 1 to 3650, default 30. **Global exclusion rules** (`全局排除规则`): section 7.

## 5. Folders and mappings

**Create a folder.** Overview, **New folder** (`新建文件夹`), a name of at most 64 bytes (names are unique). The folder starts empty and unmapped.

**Map it to a device.** Device page, **Add mapping** (`添加映射`): choose the folder (only folders not yet mapped on this device are listed), type the local path, and decide whether to allow adopting a non-empty directory:

- The path must be absolute on that device: `D:\Docs` on Windows, `/home/me/Docs` on Linux. Two mappings of one device must not contain each other.
- If the directory does not exist the client creates it. If it exists and is empty, the client starts syncing it.
- If it exists, is not empty and was never synced, the client refuses (a `folder` error, section 9) unless **Allow adopting a non-empty directory** (`允许接管非空目录`) is ticked. This protects you from typing `C:\Users\me` by mistake and uploading everything. When adoption is allowed, the client compares its files with the server: identical files are accepted without transfer, which is how you move over from another sync tool or add a second computer that already has the files. Differing files become conflict copies (section 8).
- The client puts a marker file `.syncroot` into the directory. If the marker later disappears (an unmounted disk, a moved directory), the client stops syncing that folder and reports an error. It never treats a missing directory as "everything deleted". Do not delete `.syncroot`.

**Change a mapping.** On the device page, the **⋯** menu at the end of the row has **Edit mapping…** (`编辑映射…`) and **Delete mapping…** (`删除映射…`). Changing the path counts as deleting the old mapping and adding a new one: the files at the old path stay where they are, and the new path is compared with the server from scratch.

**Delete a mapping.** The client stops syncing that folder, deletes its local state database and the marker file. Your local files stay untouched, and the folder and its history stay on the server.

**Delete a folder.** Folder page, the **⋯** menu in the title row, **Delete folder…** (`删除文件夹…`). It is available only when no device maps the folder (delete the mappings first); the menu item says how many devices still do. It permanently removes the folder and all its history; its blocks are reclaimed in the next garbage collection (section 13). Files on the clients stay.

Several folders per device work independently of each other: a long upload in one does not delay another.

## 6. Devices

**Add**: section 3. **Online** means the device has an open long-poll request, or had one end in the last 90 seconds. A device that has not connected for 7 days raises a warning (section 9).

**Revoke.** Device page, the **⋯** menu in the title row, **Revoke device…** (`吊销设备…`). The device's token stops working at once and all its mappings are deleted. Its local files stay. The revoked client logs `sync is stopped, the server does not accept this device` and tries again only every ten minutes or so. To clean the computer, follow section 16.1.

## 7. Exclusion rules

Rules decide which paths are not synchronized. There are two levels, neither belongs to a device:

1. **Global rules**: Settings page, **Global exclusion rules**. Apply to every folder on every device. They start with a default list (below).
2. **Folder rules**: folder page, top level only, the **Exclusion rules** (`排除规则`) button (it shows the rule count). They are appended after the global rules.

The syntax is that of `.gitignore`, one rule per line, matched against the path relative to the folder root with `/` as separator:

- Blank lines are ignored. `#` starts a comment. `!` negates. A trailing `/` matches directories only. A pattern with `/` at the beginning or in the middle is anchored at the folder root; otherwise it matches at any depth.
- `*` and `?` do not match `/`; `[a-z]`, `[!a-z]` and `[[:alpha:]]` are character classes; `**/x`, `x/**` and `a/**/b` match across directories.
- The last matching rule wins. If a directory is excluded, nothing below it can be re-included; to keep one file below an excluded directory, re-include the directory first, exclude its content, then re-include the file.
- Matching is case-insensitive on Windows devices and case-sensitive on Linux devices.
- No `{a,b}`, and no per-directory `.gitignore` files.

Examples:

| Rule | Meaning |
|---|---|
| `node_modules/` | Any directory called `node_modules`, at any depth |
| `/Videos/` | Only the `Videos` directory at the folder root |
| `*.iso` | Every `.iso` file |
| `Projects/**/bin/` | Any `bin` directory below `Projects` |
| `/*` then `!/Docs/` | Only the `Docs` directory at the root |

A folder rule cannot delete a global rule, but because the last match wins it can undo one with `!`. For example, with the global rule `build/`, this keeps only `build/keep.txt` in one folder:

```text
!/build/
/build/*
!/build/keep.txt
```

The rule boxes colour comments grey and invalid lines red; hover over a red line to see the message, and the errors are listed under the box. Saving is refused while a line is invalid, and nothing is changed.

**Built-in exclusions** always apply and cannot be overridden: `.syncroot`, temporary download parts `.syncpart-*`, and the side files `-wal`, `-shm`, `-journal` of SQLite databases (section 10).

**Default global rules** cover: Office and Windows leftovers (`~$*`, `*.tmp`, `Thumbs.db`, `desktop.ini`, `$RECYCLE.BIN/`, `System Volume Information/` and a few more), editor and lock files (`*~`, `.~lock.*`, `.*.sw?`, `*.kate-swp`), partial downloads (`*.part`, `*.filepart`, `*.crdownload`), macOS metadata (`.DS_Store`, `._*`, `.Trashes/`, and others), Linux leftovers (`.Trash-*/`, `.directory`, `.fuse_hidden*`, `.nfs*`), and metadata of other sync tools (`.stfolder/`, `.stversions/`, `.stignore`, ...). Open the settings page for the full list. The defaults are written once at installation; afterwards the text in the settings page is the truth.

Your own rules (`.venv/`, `node_modules/`, caches, files that change every few seconds and would fill the server with versions) are yours to add. The defaults deliberately do not include them.

**What happens when rules change.** The new rules reach online clients within a second. A path that becomes excluded is simply no longer tracked: the local file stays, and the copy on the server and on other devices stays too (nothing is deleted anywhere). When you remove an exclusion, the client compares both sides again and differences are settled like any other conflict (section 8).

## 8. Conflicts and conflict copies

The first device to reach the server wins. When two devices change the same file while out of contact, the second device to sync does this:

| This device | Server now | Result |
|---|---|---|
| Changed | Changed to identical content | Accepted, nothing transferred |
| Changed | Changed differently | The local file is renamed to a **conflict copy** and uploaded as a new file; the server's version is downloaded to the original path |
| Changed | Deleted | The change wins and is uploaded again |
| Deleted | Changed | The deletion gives way and the server's file is downloaded |
| New | Exists with different content | Same as "changed differently" |

Nothing is lost: the overwritten content also stays in the version history. The decision never depends on clocks.

A conflict copy is named by inserting the device name and the local time before the extension: `report.conflict-laptop-20261101-153012.docx`. It is an ordinary file and syncs to every device. In the Web UI the copy has a red **Conflict copy** (`冲突副本`) label and the original has **Has conflict** (`有冲突`); the bell lists the conflict as an error. Compare the two files on a client, then delete the copy (or rename it to keep both); the notification disappears by itself.

Other cases:

- **Rename or move** is a delete plus a new file with the same content; other devices copy locally instead of downloading. If one device moves a directory while another edits a file inside it, the edited file stays at the old location.
- **Case.** The server treats names case-insensitively inside a folder. If a Linux client has both `A.txt` and `a.txt`, the one that arrives second is rejected (`被拒绝`), shown as an error; rename one of them. Renaming only the case on Windows works.
- **Windows-illegal names** are rejected by the server on every device: characters `<>:"\|?*` or control characters, names ending in a dot or space, and the reserved names `CON`, `PRN`, `AUX`, `NUL`, `COM1`-`COM9`, `LPT1`-`LPT9`. Rename the file on the device that created it, or exclude it.
- **A file and a directory with the same name** are a conflict: the side that arrives second renames its file or directory to a conflict copy name.
- **Symbolic links and Windows junctions** are not followed and not synced (reported as skipped, `已跳过`). Empty directories, permissions other than the executable bit, owners and extended attributes are not synced. File contents are copied byte for byte, with no line-ending conversion.

## 9. Notifications

The Web UI is the only place where kaze-sync notifies you; clients show nothing. Click the bell. It shows a count (red when there are errors, yellow when only warnings, `99+` above 99). The panel groups entries by folder; devices that have been offline for long are grouped under **Devices** (`设备`). Each entry has the message, the path (a link to the file in the folder page, highlighted for two seconds), the device, the start time and a hint. More than 5 entries of the same kind from one device collapse into one line that expands on click.

Notifications are computed from the current state: there is nothing to dismiss, and each one disappears when its cause is gone. Entries reported by clients are kept in server memory, so after a server restart they reappear when the client reports again; conflict copies and long-offline devices do not depend on that.

| Source | Level | What to do |
|---|---|---|
| Conflict copy exists | Error | Compare on a client, delete the copy or rename it |
| `rejected` (`被拒绝`): case collision, illegal name, file and directory with one name | Error | Rename on that client, or exclude the path |
| `io` (`读写错误`): other read or write error | Error | Check the disk and permissions on that client, using the message |
| `folder` (`文件夹错误`): sync root missing, or non-empty directory not allowed to be adopted | Error | Mount the disk, or fix the path or tick adoption on the device page |
| `in_use` (`占用中`): file locked, being written, or an open SQLite database | Warning | Close the program using it (section 10) |
| `skipped` (`已跳过`): symbolic link, file name that is not UTF-8 | Warning | Exclude it if you do not need the message |
| Device not connected for 7 days | Warning | Bring the device online, or revoke it |

Items a client reports as skipped or rejected are not on the server, so the folder page lists them as grey **Not synced** rows (`未同步: ...`) inside their directory; **Details** (`详情`) in the row menu shows the message, device and start time. Folder and device pages also show a one-line summary under the title ("1 个错误, 2 个提醒") that opens the panel.

## 10. Files in use and SQLite databases

- **Windows.** Programs such as Word or Outlook lock files. When reading, renaming or deleting a file fails because another program has it open, the client retries after 30 seconds, 2 minutes and 10 minutes, and after that at every scan, and reports the file as `in_use`. A download keeps its temporary file meanwhile. Once the program closes the file the sync completes by itself.
- **Fresh files.** A file is uploaded only after its modification time is at least 5 seconds old and unchanged while being read, so half-written files are not uploaded.
- **SQLite databases.** A database whose side file `<name>-wal` or `<name>-journal` exists and is not empty is treated as in use: it is neither uploaded nor overwritten by a download, and is reported as `in_use`. In WAL mode that means the database syncs only after the application closes it (or after you run `PRAGMA wal_checkpoint(TRUNCATE)`). The side files themselves are never synced. Do not use one database on two machines at once; you would get conflict copies.
- Virtual machine disk images sync reliably only while the machine is shut down. On Linux there is no way to know whether another program has a normal file open for writing, so only the 5-second rule above protects you.

## 11. Restoring old content

Everything that is overwritten or deleted is kept for the retention period (section 13). The Web UI only shows names and metadata and never offers a download; a restore always writes the content back as the current version, and the clients then receive it like any other change. A restore never alters or deletes history.

**One file, one version.** In the folder page, open the row menu (right-click on the row, the **⋯** button at the end, a long press on a touch screen, or `Shift+F10` with the keyboard) and choose **Version history…** (`版本历史…`). The table lists time, size and the device that made each version; the first row is the current state. Press **Restore** (`恢复`) on the version you want and confirm in the row. The current content moves into the history, so the restore can itself be undone.

**A deleted file.** Turn on **Show deleted** (`显示已删除`) at the top of the folder page. Deleted files appear in grey with a **Deleted** (`已删除`) label. The menu has **Restore last content…** (`恢复最后的内容…`), or open **Version history…** to pick an earlier version.

**A file, a directory or a whole folder as of a point in time.** Choose **Restore to a point in time…** (`恢复到某一时刻…`) in the row menu of a file or directory, or, for the current directory, **Restore this directory to a point in time…** (`恢复此目录到某一时刻…`) in the menu of the title row or after right-clicking empty space (at the top level of the folder the text is "Restore whole folder...", `恢复整个文件夹到某一时刻…`). In the dialog:

1. Type the time as `2026-11-03 22:48:27` (seconds are optional), or press one of the shortcuts: 1 hour ago (`1 小时前`), today 00:00 (`今天 0 点`), this time yesterday (`昨天此时`), 7 days ago (`7 天前`). The time zone in use is written beside the box; change it on the settings page. A time older than the retention period warns that the content may already be purged.
2. The dialog previews what will be restored: the number of files and the first 20 paths, each marked "now deleted" (`现在已删除`) or "content differs now" (`现在内容不同`). If nothing differs, the button stays disabled.
3. Press **Restore** (`恢复`). Afterwards a message says how many files were restored and how many failed (a name conflict, or the file was changed at the same moment).

For a path, the version that was current at that time is restored. Only content is restored: files created after that time are never deleted, and the current contents are kept in the history. This covers both "I deleted a directory" and "a batch of files was overwritten by mistake".

## 12. Export to plain files

The server stores blocks and databases, not a normal directory tree. To get ordinary files out (disaster recovery, moving away), run on the server as root (the data below `var/` belongs to the service user):

```bash
kaze-syncd export DIR [--data DIR] [--folder NAME|ID] [--at "2026-11-02 12:00[:00]"]     # server
```

Without `--folder`, each folder is exported to `DIR/<folder name>`; with `--folder`, directly into `DIR`. Without `--at` the current state is written; with it, the state at that time, which is **UTC**. `--data` defaults to the directory that holds the binary. The command reads the databases and blocks directly, so it works while the service is stopped, and on a restored backup. Every file is verified by its hash after writing; a file with a missing or damaged block is reported and skipped, the others are still exported, and the command exits with a non-zero status. Contents, executable bit and modification time are restored. Plan for enough free disk space: exporting a folder writes all of it.

## 13. Retention and garbage collection

- Every overwrite or delete keeps the old version for `keep_days` days (**Version retention** in settings; default 30; 1 to 3650). Saving a new value starts a cleanup immediately, with no restart.
- Garbage collection runs once a day (one minute after the server starts, then once per day) and also right after you save the retention. It deletes versions older than the retention, then blocks that nothing references and that are more than an hour old, and leftovers in `var/tmp/`. It runs in the background and does not interrupt syncing. Blocks uploaded but not yet committed are protected by the one-hour grace period.
- Disk space therefore shrinks after the next collection, not at the moment of a delete. The result is in the log, one line per run (`msg=gc`) with the number of versions and blocks removed, passes and duration; on OpenWrt, `logread | grep 'msg=gc '`.
- The memory used by a collection is capped at 64 MB; very large data only needs more passes. The table of data size against memory is in the design document (`docs/design.md`, section 7).
- Files that change every few seconds produce one version per change. If they are not worth keeping, exclude them (section 7).

## 14. Backup and restore of the server

The server is not a backup: versions live on the same disk as the data (the clients, however, hold ordinary copies of every folder). To protect against disk loss, back up `<data>/var/meta.db`, `<data>/var/folders/` and `<data>/var/blobs/`, plus the small `<data>/var/tls/` (without it the restored server creates a new certificate, its fingerprint changes and every client must enroll again). `meta.db` and the folder databases must come from the same moment, so stop the service while copying them, and avoid the time garbage collection runs. The blocks never change after being written, so they can be copied while the service runs and incrementally. Keep the backup on another disk or machine. The commands (with systemd shown; use your service manager's stop and start):

```bash
systemctl stop kaze-sync                                                          # server
cd /var/lib/kaze-sync/var && tar cf /backup/meta.tar meta.db folders tls
systemctl start kaze-sync                                                         # downtime: seconds
tar cf /backup/blobs.tar blobs
```

**Restore** (on a new machine, or in place to roll back), with a `kaze-syncd` of the same or a newer version:

```bash
mkdir -p /var/lib/kaze-sync/var                                                    # server
tar xf /backup/meta.tar  -C /var/lib/kaze-sync/var
tar xf /backup/blobs.tar -C /var/lib/kaze-sync/var
./kaze-syncd export /tmp/check --data /var/lib/kaze-sync                           # optional: check the data reads
./kaze-syncd install --data /var/lib/kaze-sync --new-epoch                         # fixes owners, new history id, starts the service
```

`--new-epoch` is **required** after any restore. A restore sends the server's history back in time; the option makes every client forget what it knew of the server and read it again once. Clients need no action. Files that differ from the backup become conflict copies on the clients (nothing is lost), files created on a client after the backup are uploaded again, and files deleted on a client after the backup come back. Without the option clients can notice only some of the rollbacks, and may silently disagree with the server. Do not use `--new-epoch` for ordinary upgrades.

The admin password, devices and mappings are inside `meta.db`: clients do not need to enroll again, as long as the certificate came back too. Never restore only some of the files.

The step-by-step version with OpenWrt commands is in [`docs/operations.md`](../operations.md), section "备份与还原".

## 15. Upgrading

Always upgrade the **server first**, then the clients.

1. Build the new binaries (section 2.1).
2. **Server**: run `./kaze-syncd install --data <same directory>` with the new binary (on OpenWrt or Linux, as root). Port, certificate and password stay. It copies the new binary, migrates the databases (global first, then each folder) and restarts the service; with systemd it also reloads the unit definition, and it rewrites the unit file, so keep your changes in a drop-in. OpenWrt's own `sysupgrade` does not touch the data or the service.
3. **Linux client**: stop the agent, replace the binary at the same path, start it again:

```bash
systemctl --user stop kaze-sync                    # client
cp kaze-syncc ~/.local/bin/kaze-syncc
systemctl --user start kaze-sync
```

4. **Windows client**: the running agent locks its exe, so stop it first (PowerShell), copy both exe files, start the agent:

```powershell
Stop-Process -Name kaze-syncc-agent
Copy-Item kaze-syncc.exe, kaze-syncc-agent.exe <install directory>
Start-Process "<install directory>\kaze-syncc-agent.exe" -ArgumentList run
```

Why the order matters: some releases change the protocol (the current protocol version is 4). A new client talking to an old server fails on batched downloads. A client older than the server's minimum version is refused with HTTP 426; its log then contains `client too old, sync is stopped until kaze-syncc is upgraded` about once a minute and the device table shows "N, `过旧`" in red. Upgrade that client as above; its data is not harmed and it resumes by itself. Only changes that make old clients unusable raise the minimum, so most upgrades do not force a client upgrade.

## 16. Uninstalling

### 16.1 Client

Do the first step in the Web UI so that the server forgets the device, then clean the computer. Your synced files are kept in all cases.

1. Web UI: device page, **Revoke device…** (or just delete the mappings if you keep the device).
2. Linux: `systemctl --user disable --now kaze-sync`, delete `~/.config/systemd/user/kaze-sync.service` (then `systemctl --user daemon-reload`), delete the state directory `~/.local/state/kaze-sync` (it contains the token, the log and the sync state), and delete the binary.
3. Windows: end `kaze-syncc-agent.exe` (`Stop-Process -Name kaze-syncc-agent`), delete the value `kaze-sync` under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, delete `%LOCALAPPDATA%\kaze-sync`, and delete the exe files.
4. Optional: the `.syncroot` marker files in the synced directories can be deleted afterwards.

### 16.2 Server

There is no uninstall command. This deletes the server and **all synced history**; take a backup first if you are unsure. First revoke or remove the clients (section 16.1), or they will keep retrying.

systemd:

```bash
systemctl disable --now kaze-sync                  # server
rm /etc/systemd/system/kaze-sync.service
systemctl daemon-reload
userdel kaze-sync
rm -r /var/lib/kaze-sync                           # the data directory you gave to --data; contains everything
```

OpenRC: `rc-service kaze-sync stop`, `rc-update del kaze-sync default`, delete `/etc/init.d/kaze-sync`, `deluser kaze-sync` (Alpine), then delete the data directory. OpenWrt: `/etc/init.d/kaze-sync stop`, `/etc/init.d/kaze-sync disable`, delete `/etc/init.d/kaze-sync`, remove its two lines (`/etc/init.d/kaze-sync`, `/etc/rc.d/S99kaze-sync`) from `/etc/sysupgrade.conf`, remove the firewall rule with `uci -q delete firewall.kaze_sync; uci commit firewall; /etc/init.d/firewall reload`, then delete the data directory. The user `kaze-sync` has no login shell and may stay on OpenWrt.

Close the port in your firewall or security group if you opened it by hand.

## 17. Troubleshooting

**Where the logs are.**

| Component | Where |
|---|---|
| Server, systemd | `journalctl -u kaze-sync` |
| Server, OpenRC | syslog, tag `kaze-sync` (Alpine: `/var/log/messages`) |
| Server, OpenWrt | `logread` |
| Linux client | `~/.local/state/kaze-sync/kaze-syncc.log` (`$XDG_STATE_HOME/kaze-sync/` if set) |
| Windows client | `%LOCALAPPDATA%\kaze-sync\kaze-syncc.log` |

The client log records startup, warnings and errors, and is moved to `kaze-syncc.log.old` at the next agent start once it is over 10 MB.

**Debug timeline of the client.** To find out why a change is slow, create an empty file named `debug` in the client's state directory (the directory above) and restart the agent. The log then also has `level=DEBUG` lines for file events, scans, pulls and every request to the server with its duration. Delete the file and restart the agent when done.

**Common messages and problems.**

| Symptom | Cause and fix |
|---|---|
| `enroll` says `invalid or expired code` | The code is single use and lasts 15 minutes. Create a new one; check that you copied it fully |
| `enroll` says `too many failures` (HTTP 429), or the sign-in page answers 429 | 10 failed attempts within an hour lock that IP for one hour. Wait, or run `admin-password` on the server, which restarts the service and clears all lockouts |
| `enroll` says `cannot register device` | A device with that name already exists. Revoke it, or add the device under another name |
| `enroll` fails with a certificate error | The fingerprint does not match: copy the command again from the dialog (`install` prints it too, and so does the `listening` log line), or the server uses your own certificate and `--fingerprint` must be left out |
| Browser warns about the certificate | Expected for the self-signed certificate; compare the fingerprint, accept once |
| Forgot the admin password, port or fingerprint | Section 2.5 and section 2.4 |
| Server does not start after a reboot | The data disk was not mounted. The server then exits instead of creating an empty database; mount the disk and start the service |
| `sync is stopped, the server does not accept this device` | The device was revoked. Enroll again as a new device (section 3.3) |
| `client too old` | Section 15 |
| A folder shows an error and the bell lists a `folder` entry | Section 5: the local directory is missing, the disk is not mounted, or adopting a non-empty directory is not ticked |
| Changes appear after a few seconds, not instantly | Normal: file events are collected for 2 seconds (at most 10 seconds when files keep changing), a file must be 5 seconds old, then the other side learns it through the long poll. The usual delay is 5 to 10 seconds |
| The first sync takes long | Every file is read and hashed once. One test with 19,000 files and 1.6 GiB took about 4.5 minutes |
| A file does not arrive | Check the folder page for the grey **Not synced** row and the bell: symbolic link, illegal name, case collision, a file in use, or an exclusion rule |
| The server data grows quickly | Files that change often keep one version per change. Exclude them (section 7) or shorten the retention |

## 18. What kaze-sync does not do

No sharing, multiple users or collaborative editing; no file preview or download in the Web UI; no sync of empty directories, symbolic links, permissions or owners; no end-to-end encryption, compression or bandwidth limit; no per-device rules (use a separate folder for a part you want on one device only); no two-factor sign-in; no notifications on the clients; no macOS or mobile clients; no automatic updates; no server architectures other than x86-64; a single file larger than about 15 GiB cannot be synced. The reasons are in `docs/design.md`.

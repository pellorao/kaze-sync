# kaze-sync User Manual

[English](user-manual.md) | [简体中文](user-manual.zh-CN.md)

This manual describes every operation a kaze-sync user performs: installing the server and the clients, enrolling devices, mapping folders, writing exclusion rules, dealing with conflicts, restoring old versions, backing up, upgrading, uninstalling and fixing problems. It assumes you can use a terminal, `ssh` and `scp`. For a first working setup in a few commands, start with the [README](../../README.md); this manual is for looking things up afterwards.

Two conventions:

- The Web UI comes in Simplified Chinese, Traditional Chinese, English, Japanese and Korean (see Settings, section 4). Wherever this manual names a button, menu item or message, it gives the English text and, in code font, the exact text of the Simplified Chinese interface, for example **Add device** (`添加设备`).
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

What `install` does: copies itself to `<data>/kaze-syncd`; creates the system user `kaze-sync`; creates and migrates the databases; generates a self-signed certificate (or, with `--ca`, a local CA and a certificate it signs); picks a random TCP port between 20000 and 59999 (kept on later runs); writes and enables the `procd` service `/etc/init.d/kaze-sync` and starts it; adds the service files to `/etc/sysupgrade.conf` so that `sysupgrade` keeps them. It prints the port and the certificate fingerprint (`sha256:...`). Write both down; you need them to enroll clients.

Options:

| Option | Meaning |
|---|---|
| `--data DIR` | Required. Absolute path made of letters, digits and `. _ - /` |
| `--listen :PORT` | Listen address instead of the random port; remembered for later runs |
| `--cert FILE --key FILE` | Use your own certificate instead of the self-signed one (section 2.4) |
| `--ca [--san NAMES]` | Use a local CA and a certificate signed by it, which browsers can trust (section 2.4) |
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
- **Your own certificate.** `--cert /path/fullchain.pem --key /path/key.pem` (absolute paths, checked at install time). Browsers and clients then validate it against the system roots and clients enroll without `--fingerprint`. The service runs as user `kaze-sync` and reads these files itself at start, so that user must be able to read both files and to enter every directory above them. `install` itself runs as root and does not check this: if the service user cannot read them, `install` succeeds, but the service exits at every start with `permission denied` in its log (the service manager may still report it as started for a moment). Fix it with, for example, `chgrp kaze-sync key.pem && chmod 640 key.pem` (and `chgrp kaze-sync <dir> && chmod 750 <dir>` for a directory that is closed to others), then restart the service. The server does not watch the files: after each renewal restart the service (for acme.sh, put the restart command in `--reloadcmd`). Later `install` runs keep certificate mode; there is no switch back to self-signed. Switching between the modes needs every client to be enrolled again.
- **Local CA (browsers trust it, with saved passwords).** `--ca` makes the server create its own certificate authority (CA) and a server certificate signed by it. Browsers show a warning for a self-signed certificate and, for that reason, do not offer to fill in saved passwords; once the CA is trusted on your computer, the page counts as secure and Chrome, Edge and Firefox fill in the password again. The certificate names are `localhost`, `127.0.0.1`, `::1`, the host name of the server and the host of `--listen`; add the address or name you type in the browser with `--san` (comma separated, for example `--san 192.168.1.1,nas.example.org`; repeat `install` to change the list). Open the Web UI only by one of these names, otherwise the browser warns again. The CA lives in `<data>/ca/`, readable by root only; the service cannot read the CA key. Clients pin the CA fingerprint (the one `install` prints; the enrollment dialog shows it as well), so the client machines do not need to trust anything. To trust the CA in your browser, export its certificate (it contains no secret) and import it on each computer where you use the Web UI:

  ```bash
  /mnt/data/kaze-sync/kaze-syncd ca --data /mnt/data/kaze-sync --export ca.pem     # on the server
  ```

  - **Windows (Chrome and Edge)**: `certutil -user -addstore Root ca.pem`. Windows asks once to confirm the installation of the root certificate; answer **Yes**. It goes into the current user's store, no administrator is needed. Restart the browser.
  - **Linux, system store**: `sudo cp ca.pem /usr/local/share/ca-certificates/kaze-sync-ca.crt && sudo update-ca-certificates`. Chrome and Chromium use their own store: `certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n kaze-sync -i ca.pem` (package `libnss3-tools`).
  - **Firefox**: Settings, Privacy & Security, Certificates, View Certificates, Authorities, Import, and tick "Trust this CA to identify websites". (Firefox on Windows can also read the Windows store: set `security.enterprise_roots.enabled` to true in `about:config`.)

  Whoever has `ca-key.pem` can make certificates your computers trust, so keep `<data>/ca/` private and in the backup (section 14). The server certificate is valid for two years and is renewed when fewer than 90 days remain: by `install`, by `kaze-syncd ca --data <data>` (as root), or by the service itself when it runs as the owner of the CA key. The running service picks up a renewed certificate within a minute, without a restart. With the default root-owned CA, run `kaze-syncd ca --data <data>` from a monthly cron job (`0 4 1 * * /mnt/data/kaze-sync/kaze-syncd ca --data /mnt/data/kaze-sync`); if you forget, the service logs a warning during the last 30 days. Later `install` runs keep CA mode. From self-signed or certificate-file mode, switch with `install --ca`; as with the other switches, every client must enroll again (or, for a client you can edit, put the CA fingerprint into the `fingerprint` field of its `device.json` and restart it).
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
| Settings (`设置`) | `/settings` | Time zone, language, version retention, global exclusion rules |

The overview folder table shows for each folder its state (Synced `已同步`, Syncing `同步中` with the number of pending uploads, Errors `有错误`, Unmapped `未映射`, Device offline `设备离线`), the devices that map it, the number of files and the total size, and the number of errors and warnings. The device table shows the system, the last connection time, each folder's state on that device, the client protocol version (shown in red as "N, `过旧`" when it is too old, section 15) and whether the device has the latest configuration.

Pages update by themselves: the notification bell and the overview tables refresh within a few seconds of a change (at most once every 5 seconds). The folder's file list does not refresh itself, so that a big sync does not keep re-reading it; it shows **Content changed, refresh** (`内容有更新, 刷新`) instead, and only when something changed directly in the directory you are looking at (a file there, or a subdirectory that appeared or disappeared), not for changes deep in a subdirectory. The state **Syncing** appears only for work that takes more than about 3 seconds; a quick rescan after a file was rewritten does not flip the folder to Syncing.

In the file list a directory row shows the total size of all files below it and the time of the most recent modification among them (deleted files are not counted; a directory with nothing but deleted files shows neither). The numbers are added up while the list is read, so they cost nothing extra.

Times are shown as `2026-11-03 22:48:27` in the browser's time zone by default; hover for the zone name. The pages need JavaScript for the menus, dialogs and live updates, and they are usable at phone width.

**Settings.** **Time zone** (`时区`): follow the browser, or fix an IANA zone for every browser (the list shows each zone with its current UTC offset, such as `+09:00`, summer time included, sorted by that offset from `-12:00` up to `+14:00` and by name within the same offset); it only affects what the Web UI shows and what you type into it. **Language** (`语言`): follow the browser's language list (the default; English when none of its languages is available) or fix one of the five languages; the choice is kept in this browser only, so a phone and a computer can differ, and times keep the `2026-11-03 22:48:27` format in every language. **Version retention** (`版本保留`): days to keep old versions, 1 to 3650, default 30. **Global exclusion rules** (`全局排除规则`): section 7.

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

**Built-in exclusions** always apply and cannot be overridden: `.syncroot`, temporary download parts `.syncpart-*` (a part untouched for more than a day is deleted by the next scan), and the side files `-wal`, `-shm`, `-journal` of SQLite databases (section 10).

**Default global rules** cover: Office and Windows leftovers (`~$*`, `*.tmp`, `Thumbs.db`, `desktop.ini`, `$RECYCLE.BIN/`, `System Volume Information/` and a few more), editor and lock files (`*~`, `.~lock.*`, `.*.sw?`, `*.kate-swp`), partial downloads (`*.part`, `*.filepart`, `*.crdownload`), macOS metadata (`.DS_Store`, `._*`, `.Trashes/`, and others), Linux leftovers (`.Trash-*/`, `.directory`, `.fuse_hidden*`, `.nfs*`), and metadata of other sync tools (`.stfolder/`, `.stversions/`, `.stignore`, ...). Open the settings page for the full list. The defaults are written once at installation; afterwards the text in the settings page is the truth.

Your own rules (`.venv/`, `node_modules/`, caches, files that change every few seconds and would fill the server with versions) are yours to add. The defaults deliberately do not include them.

**What happens when rules change.** The new rules reach online clients within a second. A path that becomes excluded is simply no longer tracked, and the local file stays; the client itself never deletes anything because of a rule (the server copy is dealt with when you save, below). When you remove an exclusion, the client compares both sides again and differences are settled like any other conflict (section 8).

**Saving rules in the Web UI deletes the covered files on the server, after a preview.** The rules dialog (and the global rules card on the settings page) has one main button. While the text differs from the saved rules and has not been previewed, the button reads **Preview** (`预览`). Pressing it lists, marked "not in effect yet" (`尚未生效`), the files that the server already holds and that the rules in the box would match (their real number and the first 50 paths; for the global rules, with the folder names; the preview has a one-line glob filter, see below), or says that no server file matches (`没有命中服务端文件`); nothing is saved. When many files match, narrow the list with the filter box: a pattern without `/` matches the file name, one with `/` the whole path, `*` stays inside a name, `**` crosses `/`, `?` is one character, and case is ignored; at most 50 filtered paths are listed, and the line under the list reads "Files on the server that match: N, M after the filter, showing only K" (`命中的服务端文件: N 个, 过滤后 M 个, 目前仅显示 K 个`). The filter only changes what is shown: saving still deletes every matching file. The button then becomes **Save** (`保存`), or the red **Save and delete N files** (`保存并删除 N 个文件`) when files match. If you edit the rules after previewing, the preview disappears and the button returns to **Preview**. Pressing **Save** saves at once, without another question. If files matching the rules came in or went away since the preview, nothing is saved: the refreshed preview is shown with "the matching files changed, please check again" (`命中文件有变化, 请再核对`) and you press the button again. Matching files are deleted on the server like any other deletion, so they enter the version history and can be restored within the retention period. Local copies on every device are kept and are no longer synced, even on a device that is offline or has not applied the rules yet. Files that start matching later are simply not synced; nothing else is deleted. Without JavaScript the save itself returns the preview first (when files match), and the red button saves. **Cancel** throws away unsaved edits and closes the dialog without asking; closing it any other way (Esc) with unsaved edits asks whether to discard them.

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
- **Symbolic links** are synced as links, never followed: the target is stored as text, exactly as written (an absolute path, a relative path, a dangling link or one pointing outside the folder all sync; whether the target exists on another device is up to you). The target's content is not synced through the link. A target longer than 4096 bytes is skipped (reported as skipped, `已跳过`). Nothing is ever read or written through a link, so a link to a directory outside the folder cannot make a download write outside it. On a device where a link cannot be created, such as Windows without Developer Mode, the link stays on the server, nothing is created there, and the bell shows one folded line per device ("win: 3 个符号链接未建立 (Windows 需开启开发人员模式)") and the folder page lists the links as grey **Not synced** rows. Turn on Developer Mode (Settings, System, For developers) and the links appear by themselves; nothing needs restarting. The line changes only when the set of affected links changes. Windows junctions are not followed and not synced (reported as skipped). Empty directories, permissions other than the executable bit, owners and extended attributes are not synced. File contents are copied byte for byte, with no line-ending conversion.

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
| `skipped` (`已跳过`): a symbolic link this device cannot create (see the Symbolic links paragraph in section 8), a link target longer than 4096 bytes, a Windows junction, file name that is not UTF-8 | Warning | Exclude it if you do not need the message |
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

1. Pick the time on the two axes, or type it as `2026-11-03 22:48:27` (seconds are optional). The dialog opens at "now" (`现在`) of today. The upper axis is the day axis: it runs from the oldest state a restore can reach on the left (the start of the retention period, or the end of the initial import described below if that is later) to "now", written outside the axis on the right, and its blue line stops only at midnight (00:00) of each day (the earliest day stops at the earliest point, the latest at today's 00:00); drag it, or click anywhere on the axis, and it jumps to the nearest of those points. The lower axis is the time-of-day axis and shows 24 hours: the day you stopped on, always from 00:00 to 24:00 (today too); the part of today after "now" is hatched and cannot be chosen. To choose the time within that day, drag its blue line or click on it (down to the minute; type the time for seconds); the two axes never move each other: dragging the lower axis does not move the line on the upper axis, and dragging the upper axis only moves by whole days. The upper axis writes only the day of the month (30, 1, 2); when the retention period is longer than 30 days it writes `MM-DD` (12-01) instead, and only as many dates as fit the width. On the lower axis midnight is a tall tick labelled 00:00, 24:00 is written at the right end, and the other ticks are hours (the wider the axis, the finer the ticks; on a narrow screen there are fewer of them). The yellow marks under the baselines are the days (upper axis) or the minutes (lower axis) in which something changed in the file or directory you chose. The first day of the range is usually not a whole day: the part before the earliest point is hatched too and the line cannot go there. The time box follows the lines, and the text above it says how long ago that is. The pale blue area left of a line is the data that stays after the restore; what is right of it will be rolled back. The line does not jump to the marks, but the preview is for the state at that time, and the state only changes at a yellow mark; the line on or right of a mark includes that change. The `‹` and `›` buttons (which may cross days), or the arrow keys on the lower axis (which stay within the day), move one change at a time (`PageUp`/`PageDown` one hour, `Home` and `End` the ends of the day); on the day axis the arrow keys move one day (`PageUp`/`PageDown` seven days, `Home` to the oldest, `End` to now). Typing a time moves both lines there. While you drag, only the lines and the text move; the preview is refreshed when you let go (after a short pause for keys and buttons) and only if the state changed. When the line is at "now", or after the last change, there is nothing to compare and the preview asks you to choose the day first and then the time of day. The block below the preview names the time zone in use (change it on the settings page), the retention period and the earliest time you can restore to. A time older than the retention period warns that the content may already be purged. A time earlier than the first record of the folder is computed against the state at the end of the folder's initial import, the first moment the folder was complete (the first full upload of the first device; the server infers it as the first quiet time of 2 minutes after the first files arrived). The dialog says "The chosen time is earlier than the earliest record, computed against the state when the initial import finished: initial import finished at <time>" (`所选时间早于最早记录, 按首次导入完成时的状态计算: 首次导入完成于 <时间>`). Files created after the import did not exist then and are left alone (a restore never deletes). For a directory or file created after the import, the first record of that directory or file is used instead (`所选时间早于最早记录, 按最早记录计算: 最早记录为 <时间>`). The preview and the restore agree. If there is no history at all it says so (`这里没有任何历史记录`).
2. The dialog previews what will be restored: a one-line glob filter (written as above; it only changes what is shown, the restore still covers everything) and at most 50 paths, each marked "deleted" (`已删除`) or "changed" (`已改变`) as of now (the two in different pale colours); the line under the list reads "Files that existed then and differ now: N, M after the filter, showing only K" (N is the real total; the other two parts appear only when a filter is set or the list is cut). The preview area keeps the same height, so the dialog does not jump while you move the line. If nothing differs, the button stays disabled.
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

The server is not a backup: versions live on the same disk as the data (the clients, however, hold ordinary copies of every folder). To protect against disk loss, back up `<data>/var/meta.db`, `<data>/var/folders/` and `<data>/var/blobs/`, plus the small `<data>/var/tls/` and, in local CA mode, `<data>/ca/` (without them the restored server creates a new certificate or a new CA, its fingerprint changes and every client must enroll again, and the browsers must trust the new CA). `meta.db` and the folder databases must come from the same moment, so stop the service while copying them, and avoid the time garbage collection runs. The blocks never change after being written, so they can be copied while the service runs and incrementally. Keep the backup on another disk or machine. The commands (with systemd shown; use your service manager's stop and start):

```bash
systemctl stop kaze-sync                                                          # server
cd /var/lib/kaze-sync/var && tar cf /backup/meta.tar meta.db folders tls
systemctl start kaze-sync                                                         # downtime: seconds
tar cf /backup/blobs.tar blobs
tar cf /backup/ca.tar -C /var/lib/kaze-sync ca                                     # only in local CA mode
```

**Restore** (on a new machine, or in place to roll back), with a `kaze-syncd` of the same or a newer version:

```bash
mkdir -p /var/lib/kaze-sync/var                                                    # server
tar xf /backup/meta.tar  -C /var/lib/kaze-sync/var
tar xf /backup/blobs.tar -C /var/lib/kaze-sync/var
tar xf /backup/ca.tar    -C /var/lib/kaze-sync                                     # only in local CA mode
./kaze-syncd export /tmp/check --data /var/lib/kaze-sync                           # optional: check the data reads
./kaze-syncd install --data /var/lib/kaze-sync --new-epoch                         # fixes owners, new history id, starts the service
```

`--new-epoch` is **required** after any restore. A restore sends the server's history back in time; the option makes every client forget what it knew of the server and read it again once. Clients need no action. Files that differ from the backup become conflict copies on the clients (nothing is lost), files created on a client after the backup are uploaded again, and files deleted on a client after the backup come back. Without the option clients can notice only some of the rollbacks, and may silently disagree with the server. Do not use `--new-epoch` for ordinary upgrades.

The admin password, devices and mappings are inside `meta.db`: clients do not need to enroll again, as long as the certificate came back too. Never restore only some of the files.

The step-by-step version with OpenWrt commands is in [`docs/operations.md`](../operations.md), section "备份与还原".

## 15. Upgrading

Always upgrade the **server first**, then the clients.

1. Build the new binaries (section 2.1).
2. **Server**: run `./kaze-syncd install --data <same directory>` with the new binary (on OpenWrt or Linux, as root). Port, certificate and password stay. It copies the new binary and restarts the service (the databases are migrated automatically, see below); with systemd it also reloads the unit definition, and it rewrites the unit file, so keep your changes in a drop-in. OpenWrt's own `sysupgrade` does not touch the data or the service.
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

**Database migration and rollback.** When the server starts (and when `install` runs) it brings its databases to the schema of the new version by itself, global database first, then each folder. If there is nothing to migrate, nothing happens. Before a database is changed, it is copied to `<data>/var/backups/` (`meta-v<old version>-<UTC time>.db`, or `folder-<id>-v<old version>-<UTC time>.db`; the newest 3 copies of each database are kept, and deleting a folder deletes its copies). The copy is as large as the database, so the disk needs that much free space during the upgrade. If a backup or migration step fails, the server does not start and the error says which database and why; that database is unchanged, so fix the cause (for example free disk space) and start again. A server set up without root (a user-level service) needs no `install`: replace the binary and restart; no manual database edit is needed. **Rolling a client back.** A client's folder state databases (`folder-*.db` in its state directory: `~/.local/state/kaze-sync` on Linux, `%LOCALAPPDATA%\kaze-sync` on Windows) change structure with some upgrades. Before replacing a client binary with an older one, stop the agent and copy these files; restore them together with the binary. A client finding a database made by a newer version stops syncing that folder and shows the reason as the folder's error instead of failing on every scan.

To go back to the old version after a migration: stop the server, put the old binary back, copy the backup of every migrated database over `var/meta.db` and `var/folders/<id>.db` (newest copy from the upgrade; delete the `-wal` and `-shm` files next to each), then follow the restore steps in section 14, including `--new-epoch`. An old binary refuses to start on a database that a newer version has migrated, and says so, rather than damaging it.

Why the order matters: some releases change the protocol (the current protocol version is 5; version 5 added symbolic links, and its minimum is 5, so every client must be upgraded after the server). A new client talking to an old server fails on batched downloads. A client older than the server's minimum version is refused with HTTP 426; its log then contains `client too old, sync is stopped until kaze-syncc is upgraded` about once a minute and the device table shows "N, `过旧`" in red. Upgrade that client as above; its data is not harmed and it resumes by itself. Only changes that make old clients unusable raise the minimum, so most upgrades do not force a client upgrade.

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

OpenRC (Alpine):

```bash
rc-service kaze-sync stop                          # server
rc-update del kaze-sync default
rm /etc/init.d/kaze-sync
deluser kaze-sync                                  # also removes the group
rm -r /var/lib/kaze-sync                           # the data directory you gave to --data
```

OpenWrt:

```bash
/etc/init.d/kaze-sync stop                         # server
/etc/init.d/kaze-sync disable                      # removes /etc/rc.d/S99kaze-sync
rm /etc/init.d/kaze-sync
sed -i '/^\/etc\/init.d\/kaze-sync$/d;/^\/etc\/rc.d\/S99kaze-sync$/d' /etc/sysupgrade.conf
uci -q delete firewall.kaze_sync; uci commit firewall; /etc/init.d/firewall reload   # rule added by --wan; harmless if absent
rm -r /mnt/data/kaze-sync                          # the data directory you gave to --data; contains everything
```

The data directory is a folder on your data disk. Deleting it frees the space; the disk, its partition and its mount entry are yours and stay (to reuse the disk for something else, unmount and repartition it with your usual tools). OpenWrt has no `deluser`: the user `kaze-sync` has no login shell and may stay; to remove it anyway, run `sed -i '/^kaze-sync:/d' /etc/passwd /etc/group /etc/shadow`.

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

The client log records startup, warnings and errors, and is moved to `kaze-syncc.log.old` as soon as it grows past 10 MB (also while the agent runs; one old file is kept).

**Debug timeline of the client.** To find out why a change is slow, create an empty file named `debug` in the client's state directory (the directory above) and restart the agent. The log then also has `level=DEBUG` lines for file events, scans, pulls and every request to the server with its duration. Delete the file and restart the agent when done.

**Common messages and problems.**

| Symptom | Cause and fix |
|---|---|
| `enroll` says `invalid or expired code` | The code is single use and lasts 15 minutes. Create a new one; check that you copied it fully |
| `enroll` says `too many failures` (HTTP 429), or the sign-in page answers 429 | 10 failed attempts within an hour lock that IP for one hour. Wait, or run `admin-password` on the server, which restarts the service and clears all lockouts |
| `enroll` says `cannot register device` | A device with that name already exists. Revoke it, or add the device under another name |
| `enroll` fails with a certificate error | The fingerprint does not match: copy the command again from the dialog (`install` prints it too, and so does the `listening` log line), or the server uses your own certificate and `--fingerprint` must be left out |
| Browser warns about the certificate, or does not fill in saved passwords | Expected for the self-signed certificate; compare the fingerprint, accept once. To remove the warning and get password filling, use a local CA (section 2.4); with it, the warning means the CA is not trusted on this computer yet or the address in the browser is not among the certificate names (`--san`) |
| Forgot the admin password, port or fingerprint | Section 2.5 and section 2.4 |
| The service exits right after starting and the log says `permission denied` on the certificate or key | The service user `kaze-sync` cannot read your own certificate files or enter their directory (section 2.4) |
| Server does not start after a reboot | The data disk was not mounted. The server then exits instead of creating an empty database; mount the disk and start the service |
| `sync is stopped, the server does not accept this device` | The device was revoked. Enroll again as a new device (section 3.3) |
| `client too old` | Section 15 |
| A folder shows an error and the bell lists a `folder` entry | Section 5: the local directory is missing, the disk is not mounted, or adopting a non-empty directory is not ticked |
| Changes appear after a few seconds, not instantly | Normal: file events are collected for 2 seconds (at most 10 seconds when files keep changing), a file must be 5 seconds old, then the other side learns it through the long poll. The usual delay is 5 to 10 seconds |
| The first sync takes long | Every file is read and hashed once. One test with 19,000 files and 1.6 GiB took about 4.5 minutes |
| A file does not arrive | Check the folder page for the grey **Not synced** row and the bell: symbolic link that cannot be created on that device, illegal name, case collision, a file in use, or an exclusion rule |
| The server data grows quickly | Files that change often keep one version per change. Exclude them (section 7) or shorten the retention |

## 18. What kaze-sync does not do

No sharing, multiple users or collaborative editing; no file preview or download in the Web UI; no sync of empty directories, Windows junctions, permissions or owners; no end-to-end encryption, compression or bandwidth limit; no per-device rules (use a separate folder for a part you want on one device only); no two-factor sign-in; no notifications on the clients; no macOS or mobile clients; no automatic updates; no server architectures other than x86-64; a single file larger than 128 GiB is refused by the server (the block list of one file is held in the server's memory). The reasons are in `docs/design.md`.

# kaze-sync

[English](README.md) | [简体中文](README.zh-CN.md)

kaze-sync is a self-hosted file sync service with version history. It lets you keep any number of local folders in sync across your Linux and Windows computers while keeping old versions on your own hardware. A small server (`kaze-syncd`) runs on OpenWrt or on a plain x86-64 Linux machine with systemd or OpenRC, and lightweight clients (`kaze-syncc`) on Linux and Windows sync your folders through it. Instead of editing configuration files on every computer, you configure everything in the server's Web UI.

This README helps you decide whether kaze-sync fits your needs and gets your first setup running. Once it works, the [user manual](docs/manuals/user-manual.md) describes every operation in detail.

## What kaze-sync is, and is not

To see whether kaze-sync fits your setup, start with what it is built for:

- **Light.** The first design goal is low CPU, low memory, and low traffic. The server uses about 24 MB of memory on a small data set and 35 to 65 MB on the maintainer's own deployment of about 37,000 files, and is designed for a machine with 512 MB; when idle, both clients and server use almost no CPU; when files change, only the changed 256 KiB blocks are transferred.
- **Two-way sync of several folders per device.** You can sync any number of folders on each machine—for example `D:\Docs` and `E:\Photos` on Windows, and `~/Docs` on Linux. Because transfers work in blocks, large files such as a 1 GB SQLite database sync efficiently.
- **Safe against mistakes.** Every overwrite or delete keeps the old content on the server (30 days by default), so you can undo a wrong save or delete by restoring a single file version, a deleted file, or a whole directory as of a point in time. Simultaneous edits on two computers never lose data either: the device that reaches the server second stops syncing that part of the folder and holds its changes until you choose in the Web UI whether to upload or discard them.
- **Centrally configured.** You manage folders, per-device paths, exclusion rules (`.gitignore` syntax, global and per folder), retention, notifications, and restores all in one Web UI. Clients have no settings to edit.
- **Reachable from the internet.** You can expose the server safely without a VPN: it uses a random high port, a self-signed certificate pinned by fingerprint (or your own certificate, or a local CA that your browser can trust), a random admin password, and lockout after failed attempts. Clients only connect out, so they work behind NAT without port forwarding.

Equally important is what kaze-sync is not: it is not a file-sharing, multi-user, or collaboration service; it is not a backup (versions live on the same disk as the data, although every client holds ordinary copies); it does not offer a file viewer or file download in the browser; and it is not a macOS, mobile, or non-x86-64 server solution. See section 18 of the manual for the full list.

The Web UI speaks Simplified Chinese, Traditional Chinese, English, Japanese, and Korean, following your browser language by default. The manual names every button in English and gives the Simplified Chinese on-screen text next to it.

## Quick start

If kaze-sync fits what you need, you can get a server and your first client syncing in five steps. Because no release downloads exist yet, you need Go (the version in `go.mod`) on any machine to build the binaries, and a server you can log in to as root.

1. **Build** (on any machine):

   ```bash
   CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncd ./cmd/kaze-syncd
   CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncc ./cmd/kaze-syncc
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kaze-syncc.exe ./cmd/kaze-syncc
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o kaze-syncc-agent.exe ./cmd/kaze-syncc
   ```

2. **Install the server.** Copy `kaze-syncd` to your server and run these two commands as root:

   ```bash
   ./kaze-syncd install --data /var/lib/kaze-sync       # OpenWrt: a mounted data disk, e.g. /mnt/data/kaze-sync
   /var/lib/kaze-sync/kaze-syncd admin-password         # shows the admin password once
   ```

   `install` starts the service and prints the listening port and the certificate fingerprint. Open that TCP port in your firewall or cloud security group so clients and your browser can reach it (on OpenWrt, `install --wan` does this for you; see [section 2.6 of the manual](docs/manuals/user-manual.md#26-ports-and-firewall-rules)).

3. **Sign in.** Open `https://<server>:<port>/` in your browser and log in with the password printed by `admin-password`. Because the certificate is self-signed by default, the browser will show a warning; compare the fingerprint with the one `install` printed, then accept it.
4. **Enroll a client.** Now connect your first computer to the server. In the Web UI overview, press **Add device** (`添加设备`), enter a name, and copy the enrollment command from the dialog. Put `kaze-syncc` or `kaze-syncc.exe` in a permanent directory on that computer and run the command:

   ```bash
   kaze-syncc enroll https://<server>:<port> <CODE> --fingerprint sha256:<HEX>
   ```

   This registers the device with the server and configures the background agent to start automatically at every login.
5. **Map a folder.** An enrolled device does not sync anything until you link a server folder to a local path. In the overview, press **New folder** (`新建文件夹`). Then open the device page, press **Add mapping** (`添加映射`), choose the folder, and type the local path on that computer. Tick **Allow adopting a non-empty directory** (`允许接管非空目录`) if the directory already holds files. Syncing starts at once. Repeat steps 4 and 5 for each additional computer.

## Where to go next

Once your first folder is syncing, consult these documents when you need more detail:

- [User manual](docs/manuals/user-manual.md): the full reference for everyday operation, covering how to install and upgrade on each platform, use every Web UI page, manage devices, folders, and mappings, write exclusion rules, control who may write where and resolve held changes, set up notifications, handle files in use on Windows, restore versions and directories, export plain files, tune retention, back up the server, uninstall, and troubleshoot problems.
- [`docs/operations.md`](docs/operations.md) (Chinese): deployment details such as creating a data partition on a one-disk OpenWrt machine, systemd hardening, and backup steps.
- [`docs/design.md`](docs/design.md) (Chinese): why things work the way they do.

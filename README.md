# kaze-sync

[English](README.md) | [简体中文](README.zh-CN.md)

kaze-sync is a self-hosted file sync service with version history. A small server (`kaze-syncd`) runs on OpenWrt or on a plain x86-64 Linux machine with systemd or OpenRC, and clients (`kaze-syncc`) on Linux and Windows keep any number of local folders in sync through it. You configure everything in the server's Web UI.

This README is for someone who wants to know whether kaze-sync fits and to get a first setup running. The [user manual](docs/manuals/user-manual.md) describes every operation in detail.

## What kaze-sync is, and is not

It is:

- **Light.** The first design goal is low CPU, low memory and low traffic. The server uses about 24 MB of memory in normal operation and is designed for a machine with 512 MB; idle clients and server use almost no CPU; only changed 256 KiB blocks are transferred.
- **Two-way sync of several folders per device**, for example `D:\Docs` and `E:\Photos` on Windows, `~/Docs` on Linux. Large files such as a 1 GB SQLite database sync efficiently.
- **Safe against mistakes.** Every overwrite or delete keeps the old content on the server (30 days by default). You can restore a file version, a deleted file, or a whole directory as of a point in time. Concurrent edits never lose data: the later change becomes a conflict copy.
- **Centrally configured.** Folders, per-device paths, exclusion rules (`.gitignore` syntax, global and per folder), retention, notifications and restores are all in one Web UI. Clients have no settings to edit.
- **Reachable from the internet.** The server uses a random high port, a self-signed certificate pinned by fingerprint (or your own certificate, or a local CA that your browser can trust), a random admin password and lockout after failed attempts. Clients only connect out, so they work behind NAT.

It is not: a file-sharing, multi-user or collaboration service; a backup (versions live on the same disk as the data, although every client holds ordinary copies); a service with a file viewer or download in the browser; a macOS, mobile or non-x86-64 server solution. The full list is in the manual (section 18).

The Web UI speaks Simplified Chinese, Traditional Chinese, English, Japanese and Korean, following the browser by default. The manual names every button in English and gives the Simplified Chinese on-screen text next to it.

## Quick start

You need Go (the version in `go.mod`) to build, and a server you can log in to as root. No release downloads exist yet.

1. **Build** (on any machine):

   ```bash
   CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncd ./cmd/kaze-syncd
   CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncc ./cmd/kaze-syncc
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kaze-syncc.exe ./cmd/kaze-syncc
   CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o kaze-syncc-agent.exe ./cmd/kaze-syncc
   ```

2. **Install the server.** Copy `kaze-syncd` to the server and, as root:

   ```bash
   ./kaze-syncd install --data /var/lib/kaze-sync       # OpenWrt: a mounted data disk, e.g. /mnt/data/kaze-sync
   /var/lib/kaze-sync/kaze-syncd admin-password         # shows the admin password once
   ```

   `install` prints the port and the certificate fingerprint. Open that TCP port in your firewall or cloud security group (on OpenWrt, `install --wan` does it).

3. **Sign in.** Open `https://<server>:<port>/` and log in with the admin password. The browser warns about the self-signed certificate; compare the fingerprint, then accept.
4. **Enroll a client.** In the overview, press **Add device** (`添加设备`), enter a name, and copy the command from the dialog. Run it on the computer, with `kaze-syncc` or `kaze-syncc.exe` in a permanent directory:

   ```bash
   kaze-syncc enroll https://<server>:<port> <CODE> --fingerprint sha256:<HEX>
   ```

   This registers the device and starts the agent at every login.
5. **Map a folder.** In the overview, press **New folder** (`新建文件夹`). Open the device page, press **Add mapping** (`添加映射`), choose the folder and type the local path. Tick **Allow adopting a non-empty directory** (`允许接管非空目录`) if the directory already holds files. Syncing starts at once. Repeat step 4 and 5 for each further computer.

## Where to go next

- [User manual](docs/manuals/user-manual.md): install and upgrade on each platform, every Web UI page, devices, folders and mappings, exclusion rules, conflicts, notifications, files in use on Windows, restoring versions and directories, export, retention, backup, uninstall and troubleshooting.
- [`docs/operations.md`](docs/operations.md) (Chinese): deployment details such as creating a data partition on a one-disk OpenWrt machine, systemd hardening and backup steps.
- [`docs/design.md`](docs/design.md) (Chinese): why things work the way they do.

# kaze-sync User Manual

[English](user-manual.md) | [简体中文](user-manual.zh-CN.md)

With kaze-sync, you can keep folders in sync across your Linux and Windows computers through your own server, and recover old or deleted files whenever you make a mistake. This manual is your complete reference for running kaze-sync: installing the server and clients, enrolling devices, mapping folders, writing exclusion rules, controlling who may write where and deciding what happens on simultaneous writes, restoring old versions, backing up, upgrading, uninstalling, and troubleshooting.

It assumes you are comfortable using a terminal, `ssh`, and `scp`. If you just want to get your first setup running in a few commands, start with the [README](../../README.md) and come back here when you need to look up a specific operation.

Throughout this manual, we follow two conventions:

- The Web UI comes in Simplified Chinese, Traditional Chinese, English, Japanese, and Korean (see Settings, section 4). Wherever this manual names a button, menu item, or message, it gives the English text followed by the exact Simplified Chinese interface text in code font—for example, **Add device** (`添加设备`).
- Commands that you run on the server are marked `# server`, and commands that you run on a client computer are marked `# client`. Replace placeholders like `<...>` with your own values.

System-level deployment details that are the same for every installation—such as partitioning the single disk of an OpenWrt router, systemd hardening, and the exact memory budget—live in [`docs/operations.md`](../operations.md) (written in Chinese). This manual points to the relevant sections there instead of repeating them.

## Table of contents

- [1. How the pieces fit](#1-how-the-pieces-fit)
- [2. Installing the server](#2-installing-the-server)
  - [2.1 Download and verify the server package](#21-download-and-verify-the-server-package)
  - [2.2 OpenWrt](#22-openwrt)
  - [2.3 Plain Linux (systemd or OpenRC)](#23-plain-linux-systemd-or-openrc)
  - [2.4 Port, certificate and firewall](#24-port-certificate-and-firewall)
  - [2.5 First login and the admin password](#25-first-login-and-the-admin-password)
  - [2.6 Ports and firewall rules](#26-ports-and-firewall-rules)
- [3. Installing and enrolling clients](#3-installing-and-enrolling-clients)
  - [3.1 Linux client](#31-linux-client)
  - [3.2 Windows client](#32-windows-client)
  - [3.3 After enrolling](#33-after-enrolling)
- [4. The Web UI](#4-the-web-ui)
- [5. Folders and mappings](#5-folders-and-mappings)
  - [5.1 Write control: who may write where](#51-write-control-who-may-write-where)
- [6. Devices](#6-devices)
- [7. Exclusion rules](#7-exclusion-rules)
- [8. Simultaneous writes and the decision dialog](#8-simultaneous-writes-and-the-decision-dialog)
- [9. Notifications](#9-notifications)
  - [9.1 Push messages with ntfy](#91-push-messages-with-ntfy)
- [10. Files in use and SQLite databases](#10-files-in-use-and-sqlite-databases)
- [11. Restoring old content](#11-restoring-old-content)
- [12. Export to plain files](#12-export-to-plain-files)
- [13. Retention and garbage collection](#13-retention-and-garbage-collection)
- [14. Backup and restore of the server](#14-backup-and-restore-of-the-server)
- [15. Upgrading](#15-upgrading)
- [16. Uninstalling](#16-uninstalling)
  - [16.1 Client](#161-client)
  - [16.2 Server](#162-server)
- [17. Troubleshooting](#17-troubleshooting)
- [18. What kaze-sync does not do](#18-what-kaze-sync-does-not-do)
- [Appendix A: Building from source](#appendix-a-building-from-source)

## 1. How the pieces fit

Before installing anything, it helps to see how the parts of kaze-sync fit together so that every command and screen in the rest of this manual makes sense:

- **Server** (`kaze-syncd`): a single process that stores all synced files and their historical versions, and serves the Web UI. It runs on OpenWrt, or on x86-64 Linux with systemd or OpenRC (such as a VPS or a home server).
- **Client** (`kaze-syncc`): a background agent that runs on each of your Linux or Windows computers. It watches your local folders, uploads local changes, and downloads changes from your other devices. Because clients only make outbound connections to the server and need no open incoming ports, they work behind NAT without any router setup.
- **Device**: a computer that you have enrolled with the server. Each device has a unique name.
- **Folder**: a named set of files stored on the server. A folder holds the same content across every device that syncs it.
- **Mapping**: the link between a server folder and a local directory path on one specific device—for example, the folder `Docs` might map to `D:\Docs` on your Windows laptop and `/home/me/Docs` on your Linux desktop. You configure all folders, mappings, exclusion rules, and retention settings in the server's Web UI, and the server pushes changes to online clients, usually within a second.
- **Versions**: whenever a file is overwritten or deleted, the server keeps the old content for a configurable number of days (30 by default) so you can restore it later.

A single administrator manages the entire Web UI using one randomly generated password.

**Where files live.** Knowing where kaze-sync puts its files on disk makes upgrades, backups, and troubleshooting straightforward. Wherever the operating system or platform already defines a standard location, kaze-sync uses it as is:
- Linux client state: `~/.local/state/kaze-sync/` (XDG)
- Linux systemd user unit: `~/.config/systemd/user/`
- Linux user program directory: `~/.local/bin/`
- Windows client state and autostart: `%LOCALAPPDATA%\kaze-sync\` and the `Run` registry key
- Linux root server installation: `/var/lib/kaze-sync`
- OpenWrt server installation: your mounted data disk

For anything else where you choose the path yourself—such as the `--data` directory of a test server you start by hand, or private files like a saved admin password—keep everything together under a single `~/.kaze-sync/` directory (for example, `--data ~/.kaze-sync/server` and `~/.kaze-sync/config/`) so files are never scattered across your home directory.

## 2. Installing the server

Because clients connect to the server to enroll and receive their configuration, you always start by getting the server binary and setting up the server.

### 2.1 Download and verify the server package

Prebuilt release packages are published on the project's [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page for x86-64 (`amd64`) Linux and Windows only. For the server, download `kaze-sync-0.3.5-linux-amd64.tar.gz` (replacing `0.3.5` with the release version you want) together with `SHA256SUMS` into the same directory—either directly on the server, or on your workstation if you will copy the binary over afterward. That Linux archive holds both statically linked, dependency-free binaries: `kaze-syncd` (the server) and `kaze-syncc` (the Linux client). If your machine has no prebuilt package—for example, an OpenWrt router whose CPU is not x86-64—or if you prefer to compile the binaries yourself, see [Appendix A: Building from source](#appendix-a-building-from-source).

Before unpacking the archive, verify its SHA-256 checksum against `SHA256SUMS` so you know the download was neither corrupted nor altered:

```bash
sha256sum -c SHA256SUMS
```

If you downloaded only `kaze-sync-0.3.5-linux-amd64.tar.gz` and not `kaze-sync-0.3.5-windows-amd64.zip`, `sha256sum` will report that the Windows `.zip` file is missing; confirm that the line for `kaze-sync-0.3.5-linux-amd64.tar.gz` prints `OK` before continuing.

Because the archive stores `kaze-syncd` and `kaze-syncc` at its root without a wrapper directory, create a dedicated directory and unpack into it:

```bash
mkdir -p kaze-sync-0.3.5
tar -xzf kaze-sync-0.3.5-linux-amd64.tar.gz -C kaze-sync-0.3.5
```

Both binaries embed the release version: `kaze-syncd --version` and `kaze-syncc --version` print `kaze-syncd 0.3.5` and `kaze-syncc 0.3.5` (for the `0.3.5` release). In the Web UI, the server version appears next to `Kaze Sync` at the top-left after you sign in (the sign-in page omits it).

### 2.2 OpenWrt

If your server is an OpenWrt router, you must store data on a mounted data disk rather than on the root file system (overlay). `install` refuses a data directory on the root file system because synced files would quickly fill the router's small flash storage and a system upgrade would erase them. If your router has only a single disk, create a dedicated data partition on it first; see "OpenWrt 只有一块盘: 新建数据分区" in [`docs/operations.md`](../operations.md).

The prebuilt `kaze-sync-0.3.5-linux-amd64.tar.gz` archive works only on x86-64 OpenWrt routers; there is no prebuilt package for any other OpenWrt CPU architecture (see [Appendix A](#appendix-a-building-from-source), and note in [section 18](#18-what-kaze-sync-does-not-do) that non-x86-64 servers are unsupported). Once your data disk is mounted, copy the unpacked `kaze-syncd` binary from [section 2.1](#21-download-and-verify-the-server-package) to the router (or unpack the verified archive directly on the router) and run these commands as root (`install` copies `kaze-syncd` into `/mnt/data/kaze-sync/kaze-syncd`):

```bash
./kaze-syncd install --data /mnt/data/kaze-sync        # server
/mnt/data/kaze-sync/kaze-syncd admin-password          # server: shows the admin password once
```

Running `install` performs the entire server setup in one step:
- It copies the binary to `<data>/kaze-syncd` and creates the system user `kaze-sync`.
- It creates and migrates the databases.
- It generates a self-signed TLS certificate (or, if you pass `--ca`, a local CA and a server certificate signed by it).
- It picks a random TCP port between 20000 and 59999 (and remembers that port on later runs).
- It writes, enables, and starts the `procd` service `/etc/init.d/kaze-sync`, and adds the service files to `/etc/sysupgrade.conf` so that OpenWrt's `sysupgrade` preserves them across firmware upgrades.

At the end, `install` prints the listening port and the certificate fingerprint (`sha256:...`). Write both down—you will need them to sign in and to enroll clients.

You can customize the installation with these options:

| Option | Meaning |
|---|---|
| `--data DIR` | Required. Absolute path made of letters, digits, and `. _ - /` |
| `--listen :PORT` | Listen address to use instead of the random port; remembered for later runs |
| `--cert FILE --key FILE` | Use your own certificate instead of the self-signed one (section 2.4) |
| `--ca [--san NAMES]` | Use a local CA and a certificate signed by it, which browsers can trust (section 2.4) |
| `--wan` | OpenWrt only: add a firewall rule named `kaze-sync` that accepts the port from the WAN zone. Without `--wan`, any rule left over from an earlier run is removed |
| `--new-epoch` | Used only after restoring a server backup (section 14) |

### 2.3 Plain Linux (systemd or OpenRC)

On a standard x86-64 Linux server, the root file system has plenty of space and persists across upgrades, so you can place the data directory anywhere you like (including on the root file system). From the directory where you unpacked `kaze-syncd` ([section 2.1](#21-download-and-verify-the-server-package)), run as root (`install` copies `kaze-syncd` into `/var/lib/kaze-sync/kaze-syncd`):

```bash
./kaze-syncd install --data /var/lib/kaze-sync         # server
/var/lib/kaze-sync/kaze-syncd admin-password           # server
```

`install` automatically detects your service manager (checking for OpenWrt first, then systemd, then OpenRC, and refusing anything else). It creates the system user `kaze-sync` (using `useradd`, or busybox `adduser` on Alpine), writes the service file (`/etc/systemd/system/kaze-sync.service` on systemd, `/etc/init.d/kaze-sync` on OpenRC), enables it at boot, and starts it.

Unlike on OpenWrt, `install` on plain Linux does **not** touch your firewall and does not accept `--wan`. Open the printed TCP port yourself in your cloud provider's security group or local firewall (for example, `ufw allow <port>/tcp` or `firewall-cmd --add-port=<port>/tcp --permanent`; see section 2.6).

Use your platform's service manager for day-to-day control and logs:

| | systemd | OpenRC | OpenWrt |
|---|---|---|---|
| start, stop, restart, status | `systemctl start\|stop\|restart\|status kaze-sync` | `rc-service kaze-sync start\|stop\|restart\|status` | `/etc/init.d/kaze-sync start\|stop\|restart` |
| log | `journalctl -u kaze-sync` | syslog, tag `kaze-sync` (Alpine: `/var/log/messages`) | `logread` |

If the server process crashes, the service manager restarts it after 5 seconds. If your data disk is not mounted when the machine boots, the service fails to start rather than silently creating an empty database on the underlying mount point.

On systemd, the generated unit is hardened so the service may only write below `<data>/var`. If you need to customize the unit, use a drop-in (`systemctl edit kaze-sync`) rather than editing the unit file directly, because the next `install` run will overwrite the unit file. See "普通 Linux (systemd 或 OpenRC)" in [`docs/operations.md`](../operations.md) for hardening details and systemd version requirements.

Note that `install` requires root privileges, and there is no supported way to run the server without root. Separating installation from execution is deliberate: `install` creates the unprivileged `kaze-sync` user, writes the service file, keeps the binary and data directory owned by root so a compromised server process cannot replace its own code, and restricts the CA private key to root only. The server process itself then runs as the unprivileged `kaze-sync` user and can write only below `<data>/var`.

### 2.4 Port, certificate and firewall

Because the Web UI and the client protocol share a single HTTPS port, every connection to the server is encrypted. By default, `install` picks a random port so port scanners do not easily find it, or you can fix a port with `--listen :PORT` (see section 2.6).

For TLS certificates, you can choose among three modes depending on how you use the Web UI and whether you already own a domain certificate:

- **Self-signed certificate (default).** Works out of the box on both the internet and a LAN, and is valid for 100 years. Clients pin the certificate by its SHA-256 fingerprint, so the IP address or hostname you connect to does not matter. When you open the Web UI for the first time, your browser will warn about the untrusted certificate; compare the fingerprint shown by the browser with the one `install` printed, then accept the exception once.
- **Your own certificate.** If you have a certificate from a public CA (such as Let's Encrypt), pass `--cert /path/fullchain.pem --key /path/key.pem` (both must be absolute paths, which `install` checks at install time). Browsers and clients then validate the server against system root certificates, and clients enroll without `--fingerprint`.
  Because the service runs as the unprivileged user `kaze-sync` and reads these files itself at startup, `kaze-sync` must have permission to read both files and traverse every parent directory above them. `install` runs as root, so it can always read the files itself; instead it checks the permission bits of both files and of every parent directory against the user `kaze-sync` (its primary and supplementary groups; ACLs are not examined). If `kaze-sync` cannot read the certificate or key, or cannot enter a parent directory, `install` prints a `warning:` line that names the file or directory and the fix, and carries on with the installation. Do not ignore the warning: the service would then exit immediately on every start with `permission denied` in the log (even if the service manager briefly reports it as started). Grant access with `chgrp kaze-sync key.pem && chmod 640 key.pem` (and `chgrp kaze-sync <dir> && chmod 750 <dir>` on any restricted parent directory), then restart the service.
  The server does not watch certificate files for changes, so restart the service after each renewal (for `acme.sh`, put the restart command in `--reloadcmd`). Later `install` runs preserve certificate mode, and there is no flag to switch back to self-signed.
- **Local CA (browsers trust it, with saved passwords).** Because browsers treat self-signed certificates as insecure, they refuse to autofill saved passwords on a self-signed Web UI. Passing `--ca` solves this without needing a public domain: the server generates its own private Certificate Authority (CA) and a server certificate signed by that CA. Once you import the CA certificate on your computer, Chrome, Edge, and Firefox treat the Web UI as secure and autofill your saved admin password.
  The signed server certificate covers `localhost`, `127.0.0.1`, `::1`, the server's hostname, and any host given in `--listen`. To include the IP address or domain name you actually type into your browser, pass `--san` (comma-separated, for example `--san 192.168.1.1,nas.example.org`; re-run `install` anytime you want to change the list). Always open the Web UI using one of those covered names, or the browser will warn again.
  The CA files live in `<data>/ca/` and are readable only by root, so the running service cannot read the CA private key. Clients simply pin the CA fingerprint (printed by `install` and included in the Web UI's enrollment command), so client machines do not need the CA installed in their system trust store.
  To make your browser trust the CA, import the public CA certificate once on each computer or phone where you open the Web UI (devices that only run the client do not need it). First, export the public CA certificate on the server:

  ```bash
  /mnt/data/kaze-sync/kaze-syncd ca --data /mnt/data/kaze-sync --export ca.pem     # on the server
  ```

  Because `ca.pem` contains only the public certificate and no secret key, you can copy it to your other devices by USB stick, cloud drive, or any similar channel. Before importing it on a device, verify that its SHA-256 fingerprint matches the CA fingerprint printed by `install`: on Linux and macOS, run `openssl x509 -in ca.pem -noout -fingerprint -sha256`; on Windows, run `certutil -dump ca.pem` and check "Cert Hash(sha256)" (shown as "证书哈希(sha256)" on Chinese Windows). Then import it into your browser or OS:

  - **Windows (Chrome and Edge)**: Run `certutil -user -addstore Root ca.pem`. Windows asks once to confirm installing the root certificate; click **Yes**. Because it goes into the current user's certificate store, no administrator privileges are needed. Restart the browser afterward.
  - **Linux, system store**: Run `sudo cp ca.pem /usr/local/share/ca-certificates/kaze-sync-ca.crt && sudo update-ca-certificates`. Chrome and Chromium on Linux use their own NSS store instead: run `certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n kaze-sync -i ca.pem` (from package `libnss3-tools`).
  - **Firefox**: Open Settings -> Privacy & Security -> Certificates -> View Certificates -> Authorities -> Import, select `ca.pem`, and tick "Trust this CA to identify websites". (On Windows, Firefox can also read the Windows trust store if you set `security.enterprise_roots.enabled` to `true` in `about:config`.)
  - **macOS**: Double-click `ca.pem` (or import it into the login keychain in Keychain Access), open the certificate, expand the Trust section, and choose "Always Trust" from the pop-up menu. Alternatively, run `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ca.pem` on the command line.
  - **Android**: Open Settings -> Security (or Security & privacy -> More security settings, depending on the manufacturer) -> Encryption & credentials -> Install a certificate -> CA certificate, choose `ca.pem`, and confirm the warning. Chrome on Android uses this user-installed store; some apps ignore user-installed CAs, which does not affect using the Web UI in Chrome.
  - **iPhone and iPad**: Copy `ca.pem` onto the device (via AirDrop, Files, or a download link) and open it. After iOS reports that a profile was downloaded, open Settings and follow the prompts to install that profile (also listed under Settings -> General; the exact wording differs by iOS version). Finally, open Settings -> General -> About -> Certificate Trust Settings and turn on full trust for the certificate.

  Anyone who obtains `ca-key.pem` can issue certificates that your computers will trust, so keep `<data>/ca/` private and include it in your server backups (section 14). The server certificate signed by the local CA is valid for two years and is renewed automatically when fewer than 90 days remain—either when you run `install`, or when you run `kaze-syncd ca --data <data>` as root; the service itself never renews it, because it cannot read the CA key. The running server detects and loads a renewed certificate within a minute without a restart. With the default root-owned CA, add a monthly cron job to handle renewal (`0 4 1 * * /mnt/data/kaze-sync/kaze-syncd ca --data /mnt/data/kaze-sync`); if you forget, the server logs a warning during the final 30 days before expiration. Later `install` runs preserve CA mode. You can switch from self-signed or certificate-file mode to CA mode by running `install --ca`.

Whenever you switch between certificate modes, every client must enroll again (or, if you can edit a client's files directly when switching to `--ca`, you can put the CA fingerprint into the `fingerprint` field of its `device.json` and restart the client).

If you ever forget the server's listening port or certificate fingerprint, either run `install` again with the same `--data` directory (which is safe to repeat and prints both values) or check the `listening` line in the server log.

### 2.5 First login and the admin password

With the server running and the port open, open `https://<server>:<port>/` in a supported browser (Chrome or Edge 114+, Firefox 125+, Safari 17+) and sign in with the password that `admin-password` printed. Your sign-in session lasts 7 days.

You can run `admin-password` again at any time—for example, if you forget the password or get locked out. Each run generates a new random password (you cannot choose a custom password), prints it once, and restarts the service, which immediately signs out all active browser sessions and clears all failed-login lockouts. Run it as `<data>/kaze-syncd admin-password`, or run another copy of the binary with `--data DIR`.

**Resource usage.** The server's resident memory is about 24 MB on a small data set and 35 to 65 MB on the maintainer's own deployment of about 37,000 files. Daily garbage collection (section 13) needs extra memory on top, in proportion to the number of files and to how many versions you retain; section 13 ("Memory sizing") has a table. In short, up to about a million files with the default 30-day retention need roughly 100 MB of extra memory during garbage collection, and a machine with 512 MB of RAM can run that in a single pass. If the data is larger than the memory budget allows, garbage collection scans in several passes: it takes longer but never fails and never uses more memory.

### 2.6 Ports and firewall rules

Because the server (`kaze-syncd`) serves both the Web UI and the client sync protocol over a single HTTPS listening socket and opens no plain-HTTP port, local admin port, or UDP socket, your firewall setup comes down to a single rule: **open one inbound TCP port on the server** so your browsers and clients can reach `https://<server>:<port>/`.

Clients (`kaze-syncc` on Linux and Windows) do not open any listening socket at all. Because every connection is initiated outbound from the client to the server, client computers need no inbound firewall rules and work behind NAT without port forwarding. If you also filter outbound traffic, clients only need outbound TCP to the server's port, and both the server and clients need outbound HTTP or HTTPS (plus DNS if the URL uses a hostname) only when you enable optional ntfy push alerts in `ntfy.json` (section 9.1); neither program makes any other outbound connections.

**Which port to open and how to find it.** On the first run, `install` picks a random TCP port between 20000 and 59999, saves it in the server database, and keeps that same port on later `install` runs. To choose the port yourself, pass `--listen :<port>` (or a specific address and port such as `--listen 127.0.0.1:<port>`). With the default `:<port>` form, the server binds both IPv4 and IPv6 on all interfaces, so if your clients or browsers connect over IPv6, open the port in both your IPv4 and IPv6 firewalls. Because `kaze-syncd` has no `status` subcommand, you can check the port later by re-running `install` with the same `--data` directory (which is safe to repeat and prints `installed in <data>, listening on :<port>` along with the fingerprint), by reading the `listening addr=... fingerprint=...` line in the server log (`journalctl -u kaze-sync` on systemd, syslog tag `kaze-sync` in `/var/log/messages` on OpenRC/Alpine, or `logread` on OpenWrt), by running `ss -ltnp` on the server, or by checking the `https://<server>:<port>` URL on an enrolled client.

**Opening the port.** If your server runs in the cloud, add an inbound rule for TCP `<port>` in your provider's security group from the addresses your clients and browsers use (you can restrict the source to known addresses if they are fixed, though the port is already protected by TLS certificate pinning, device tokens, a random admin password, and IP lockout after failed attempts). For a firewall on the server itself, replace `<port>` with your listening port in the commands for your platform:

On **Ubuntu / Debian (`ufw`)** or **RHEL / Rocky / Fedora (`firewalld`)**:

```bash
ufw allow <port>/tcp                               # server (ufw)

firewall-cmd --add-port=<port>/tcp --permanent     # server (firewalld)
firewall-cmd --reload
```

On **Alpine Linux (OpenRC with `iptables` / `ip6tables`)**, install `iptables` and `iptables-openrc` first if they are not yet installed (`iptables-openrc` provides the `iptables` and `ip6tables` init scripts), then add the rule, save the ruleset, and enable the firewall service at boot (include the `ip6tables` lines if you use IPv6):

```bash
apk add iptables iptables-openrc                   # server: if not yet installed
iptables -A INPUT -p tcp --dport <port> -j ACCEPT  # server (IPv4)
rc-service iptables save
rc-update add iptables
ip6tables -A INPUT -p tcp --dport <port> -j ACCEPT # server (IPv6)
rc-service ip6tables save
rc-update add ip6tables
```

If the `INPUT` chain already contains a `REJECT` or `DROP` rule, `-A` appends the new rule after it so it never matches; use `iptables -I INPUT -p tcp --dport <port> -j ACCEPT` (and likewise `ip6tables -I INPUT -p tcp --dport <port> -j ACCEPT`) instead, which inserts the rule at the top of the chain. `rc-service iptables save` (and `rc-service ip6tables save`) can be run while the service is not started: the rules you added with `iptables` are already active, and the saved ruleset is restored at boot by the service enabled with `rc-update add`.

On **OpenWrt**, passing `--wan` to `install` creates the WAN firewall rule automatically, or you can create the same `kaze-sync` rule manually with `uci` (note that running `install` later without `--wan` removes `firewall.kaze_sync`):

```bash
./kaze-syncd install --data /mnt/data/kaze-sync --wan    # server: automatic

uci set firewall.kaze_sync=rule                          # server: manual equivalent
uci set firewall.kaze_sync.name=kaze-sync
uci set firewall.kaze_sync.src=wan
uci set firewall.kaze_sync.proto=tcp
uci set firewall.kaze_sync.dest_port=<port>
uci set firewall.kaze_sync.target=ACCEPT
uci commit firewall
/etc/init.d/firewall reload
```

## 3. Installing and enrolling clients

Once you can sign in to the Web UI, the next step is to connect your computers to the server.

To enroll a computer, first generate an enrollment command in the Web UI. On the overview page, press **Add device** (`添加设备`), enter a device name (up to 32 characters using letters, digits, `-`, and `_`; the name must be unique and is shown in notifications that name a device), and press **Generate enrollment code** (`生成注册码`). The dialog displays a single-use code valid for 15 minutes along with ready-to-copy enrollment commands for Linux and Windows (including `--fingerprint` when the server uses a self-signed certificate or local CA):

```text
kaze-syncc enroll https://sync.example.org:41873 K7QF-M2XD-9PLA --fingerprint sha256:...
```

Make sure the server URL in the command uses whatever hostname or IP address your client computer can actually reach, including the port, and starts with `https://`.

### 3.1 Linux client

On an x86-64 (`amd64`) Linux computer, download `kaze-sync-0.3.5-linux-amd64.tar.gz` (using a release version compatible with your server) and `SHA256SUMS` from the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page into the same directory. That archive holds both `kaze-syncc` and `kaze-syncd`. (If your Linux computer uses another CPU architecture, there is no prebuilt release package for it; see [Appendix A: Building from source](#appendix-a-building-from-source).)

First verify the checksum before unpacking:

```bash
sha256sum -c SHA256SUMS                                # client
```

(If you did not also download the Windows `.zip`, `sha256sum` notes that it is missing; confirm that `kaze-sync-0.3.5-linux-amd64.tar.gz` prints `OK`.)

Next, unpack `kaze-syncc` into a **permanent** directory—such as `~/.local/bin/`—because the autostart service records the exact path from which you run `enroll` (so moving the binary later would break autostart):

```bash
mkdir -p ~/.local/bin                                  # client
tar -xzf kaze-sync-0.3.5-linux-amd64.tar.gz -C ~/.local/bin kaze-syncc
```

Then run the enrollment command from the Web UI dialog (run `~/.local/bin/kaze-syncc` if `~/.local/bin` is not in your `PATH`):

```bash
kaze-syncc enroll https://sync.example.org:41873 <CODE> --fingerprint sha256:<HEX>    # client
```

`enroll` saves the device token in `~/.local/state/kaze-sync/`, writes the systemd user unit `~/.config/systemd/user/kaze-sync.service`, enables it, and starts the background agent. You can verify that it is running with `systemctl --user status kaze-sync`.

Linux clients require a systemd user session; other init systems are not supported on the client. By default, a systemd user session runs only while you are logged in; if you want the agent to keep syncing when you are logged out, enable lingering with `loginctl enable-linger`. If you pass `--no-autostart`, `enroll` only registers the device without creating or starting the systemd service, and you can run the agent in the foreground whenever you want with `kaze-syncc run`.

### 3.2 Windows client

On an x86-64 (`amd64`) Windows computer, download `kaze-sync-0.3.5-windows-amd64.zip` (using a release version compatible with your server) and `SHA256SUMS` from the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page. The `.zip` archive contains `kaze-syncc.exe` and the windowless `kaze-syncc-agent.exe`. (If your Windows computer uses another CPU architecture, there is no prebuilt release package for it; see [Appendix A: Building from source](#appendix-a-building-from-source).)

Verify the archive in PowerShell before unpacking, and compare the printed hash against the `kaze-sync-0.3.5-windows-amd64.zip` line in `SHA256SUMS` (PowerShell outputs uppercase hex while `SHA256SUMS` uses lowercase; case does not matter):

```powershell
Get-FileHash .\kaze-sync-0.3.5-windows-amd64.zip -Algorithm SHA256
```

Unpack the archive into a **permanent** directory (`<install directory>`) so both `kaze-syncc.exe` and `kaze-syncc-agent.exe` stay together in the folder that autostart will record:

```powershell
Expand-Archive .\kaze-sync-0.3.5-windows-amd64.zip -DestinationPath <install directory>
```

Open a terminal in `<install directory>` and run the enrollment command from the dialog (`kaze-syncc.exe enroll ...`).

`enroll` saves the device token in `%LOCALAPPDATA%\kaze-sync\`, registers the windowless `kaze-syncc-agent.exe` under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` with the value name `kaze-sync` (which requires no administrator privileges), and starts the agent immediately. From then on, the agent starts automatically whenever you sign in to Windows.

### 3.3 After enrolling

As soon as enrollment finishes, your computer appears in the device table on the Web UI overview page with a green dot showing it is online. A newly enrolled device does not sync any files yet—you will link folders to it in section 5.

If you ever need to enroll the same computer again (for instance, after reinstalling its operating system), either revoke the old device entry first or enroll under a new device name, because device names must be unique; then generate and run a fresh enrollment command.

## 4. The Web UI

Now that your server and first client are connected, you will do all configuration and monitoring in the Web UI.

Every page shares the same top navigation bar: the **kaze-sync** logo (which takes you back to the overview), the notification bell (section 9), the gear icon (Settings), and the sign-out button. Below the bar, the page title shows your current location as a breadcrumb path (such as `Overview / Docs / projects`).

| Page | Address | What it is for |
|---|---|---|
| Sign-in | `/login` | Enter the admin password |
| Overview (`总览`) | `/` | View the folder table and device table; **New folder** (`新建文件夹`) and **Add device** (`添加设备`) buttons |
| Folder | `/folders/<id>` | Browse files, restore old versions, set write control (section 5.1), decide held changes (section 8), edit folder exclusion rules, or delete the folder |
| Device | `/devices/<id>` | Add, edit, or delete the device's folder mappings, or revoke the device |
| Settings (`设置`) | `/settings` | Configure time zone, language, version retention, the single-writer time window, and global exclusion rules |

On the **Overview** page, the two tables give you the status of your entire setup at a glance:
- The **folder table** shows each folder's current state (Synced `已同步`, Syncing `同步中` along with the number of pending uploads, Errors `有错误`, Unmapped `未映射`, or Device offline `设备离线`), which devices map it, its total file count and size, and its count of errors and warnings.
- The **device table** shows each device's operating system, last connection time, the state of each folder mapped on that device, the client release version (`x.y.z`, or `-` for a client that does not report one; highlighted in red with "`过旧`" next to it when a client is too old; see section 15), and whether the device has received the latest configuration.

**How pages update.** Status indicators update automatically: within a few seconds of any change (at most once every 5 seconds), the notification bell and the overview tables refresh themselves. The file list on a folder page, however, does not auto-refresh, so a large ongoing sync will not constantly reload the table while you are browsing it. Instead, when something changes directly inside the directory you are viewing (a file in that directory, or a subdirectory appearing or disappearing—not changes deep inside a subdirectory), a prompt appears saying **Content changed, refresh** (`内容有更新, 刷新`). Similarly, to avoid visual flicker during brief rescans after a file rewrite, a folder flips to **Syncing** only when sync work takes longer than about 3 seconds.

When you browse a folder's file list, each directory row displays the combined size of all files underneath it and the most recent modification time among them (deleted files are excluded; a directory containing only deleted files shows neither). The server sums these values while reading the list, so displaying them adds no extra overhead.

All timestamps use the format `2026-11-03 22:48:27` in your browser's local time zone by default; hover over any timestamp to see the zone name. The Web UI requires JavaScript for menus, dialogs, and live updates, and its layout works on phone-width screens as well as desktops.

**Settings.** Click the gear icon in the top bar to adjust global preferences:
- **Time zone** (`时区`): follow each browser's local time zone, or lock a specific IANA time zone across all browsers. The dropdown lists every zone with its current UTC offset (such as `+09:00`, including daylight saving time), sorted from `-12:00` to `+14:00` and alphabetically by name within the same offset. This setting only affects how times are displayed and parsed in the Web UI.
- **Language** (`语言`): follow the browser's preferred language list (the default, falling back to English if none of the five supported languages match) or lock one of the five languages. Because language preference is stored in the current browser only, your phone and desktop can use different languages. Timestamps keep the `2026-11-03 22:48:27` format regardless of language.
- **Version retention** (`版本保留`): how many days to keep overwritten or deleted versions, from 1 to 3650 (default 30).
- **Single-writer time window** (`单端写入时间窗`): how many minutes a write window lasts in single-writer and any-writer modes, from 0 to 60 (default 1; see section 5.1). Saving a new value takes effect on the server immediately without restarting any client.
- **Global exclusion rules** (`全局排除规则`): exclusion patterns applied to every folder on every device (section 7).

## 5. Folders and mappings

To start syncing files between computers, you create a folder on the server and map it to a local directory path on each device that should participate.

**Create a folder.** On the overview page, press **New folder** (`新建文件夹`) and enter a unique name of at most 64 bytes. A new folder starts empty and unmapped, and adds a reminder in the notification bell asking you to choose its write control mode (section 5.1); the reminder stays until you choose.

**Map it to a device.** Open the device page for a computer and press **Add mapping** (`添加映射`). Choose the folder (the dropdown lists only folders not yet mapped on this device), type the local directory path, and choose whether to allow adopting a non-empty directory:

- The local path must be an absolute path on that device—for example, `D:\Docs` on Windows or `/home/me/Docs` on Linux. On the same device, two mappings cannot be nested inside each other.
- If the local directory does not exist yet, the client creates it; if it exists and is empty, the client starts syncing immediately.
- If the directory already exists, is not empty, and has never been synced before, the client refuses to touch it and reports a `folder` error (section 9) unless you tick **Allow adopting a non-empty directory** (`允许接管非空目录`). This safeguard prevents you from accidentally typing `C:\Users\me` and uploading your entire home directory. When you do tick adoption, the client compares its existing local files against the server: identical files are adopted without transferring their data over the network—which is how you migrate from another sync tool or attach a second computer that already has a copy of the files—while files whose contents differ are put on hold for your decision (section 8).
- Once syncing begins, the client places an empty marker file named `.syncroot` at the root of the local directory. If `.syncroot` ever disappears later (because an external drive was unmounted or the directory was moved), the client immediately stops syncing that folder and reports an error rather than mistaking the missing directory for "every file was deleted". Do not delete `.syncroot`.

**Change a mapping.** On the device page, open the **⋯** menu at the end of a mapping's row to choose **Edit mapping…** (`编辑映射…`) or **Delete mapping…** (`删除映射…`). Changing a mapping's local path is treated as deleting the old mapping and creating a new one: the files at the old path remain untouched where they are, and the client compares the new path against the server from scratch.

**Delete a mapping.** Choosing **Delete mapping…** (`删除映射…`) tells the client to stop syncing that folder and remove its local state database and `.syncroot` marker file. Your local files stay untouched on the computer, and the folder and all its history remain on the server.

**Delete a folder.** To remove a folder from the server entirely, open the folder page, click the **⋯** menu in the title row, and choose **Delete folder…** (`删除文件夹…`). To prevent accidental data loss, this action is enabled only when no device maps the folder (the menu item shows how many devices still map it, so you can delete those mappings first). Deleting a folder permanently removes the folder and all of its version history from the server, and its data blocks are reclaimed during the next garbage collection run (section 13). Local files on your clients are not deleted.

If you map several folders on the same device, they sync independently of one another, so a large upload in one folder never blocks changes in another.

### 5.1 Write control: who may write where

Most of the time your computers edit different files, and sync simply works. Trouble starts only when two computers write to the same place at about the same time. For a single document, that is an ordinary collision; for a Git repository, it is worse—if two computers both run `git` inside the same working tree, each one rewrites `.git/index` and other internal files, and mixing the two produces a broken repository. kaze-sync neither tries to merge such writes nor creates a second copy of the file. Instead, it follows one rule: **the first computer to reach the server wins, and the computer that comes second stops syncing that part of the folder until you decide what to do with its changes.** Nothing is lost while it waits: its edits stay on its own disk as held changes, and you choose what happens to them in a dialog (section 8).

**What "frozen" means.** A device that has stopped in this way is called frozen. It pauses sync for the affected directory in both directions—neither uploading its local changes there nor writing the server's changes there to disk—while every other device and directory carries on normally. Because the freeze persists across agent restarts, restarting the client does not clear it. Once you decide every held change in that directory, the freeze ends, and the device re-reads the directory from the server and resumes syncing.

**The three write modes.** "That part of the folder" is what write control lets you define. Every folder, directory, and file has one of three write modes, which determines who may write and how much freezes when two devices collide:

| Mode | What is allowed | When two devices write at the same time |
|---|---|---|
| Any writer (`不限`) | Every device may write. Each file is judged on its own. | Only the file that both changed is frozen on the device that came second; its other files keep syncing. |
| Single writer (`单端`) | One device at a time writes to the whole directory. The device whose write reached the server last is the current writer. | The device that came second is frozen for the whole directory, not only for the file. |
| Exclusive (`独占`) | Only the device you name may write. | Any change made on another device freezes that device's sync of the directory. |

Under single writer, whether a write freezes a device depends on timing and on how current the device's content is, so it helps to know what the server treats as simultaneous.

**What counts as "at the same time" in single-writer mode.** The server judges when changes arrive using its own clock and version numbers, so the clocks on your computers do not matter. A write by another device counts as simultaneous if either condition holds:

- It arrives within the **single-writer time window** after the current writer's last write (1 minute by default; section 4, Settings). This catches cases such as a `git commit` reaching the server in several batches while another device is already writing.
- It was made on out-of-date content—that is, another device has written to the directory since this device last synced it. This catches a laptop that edited a repository while offline and reconnects after the desktop has already updated it.

Setting the time window to 0 turns off the first test and keeps only the second. Any-writer mode uses the same time window, but applies it per file: if another device wrote the same file less than one time window ago, the second write counts as simultaneous.

**Where the settings apply.** A new folder's root defaults to **single writer** until you change it, and you do not have to use a single mode for the entire folder. Any setting you place on a directory covers that directory and everything below it until a deeper directory defines its own setting; directories without their own setting inherit from above. A deeper setting can either loosen or tighten the parent mode—for example, the folder root can be any writer, `scratch/` single writer, and `scratch/repo/` exclusive. You can also mark a single file as exclusive, though not as any writer. If you later revoke the device that owns an exclusive directory or delete its mapping for the folder, its exclusive directories fall back to single writer.

**Which mode to choose.** Pick the mode for each directory based on whether its files must change together or can change independently:

- **A Git repository, or any directory whose files must stay consistent as a whole.** Use single writer on the repository directory, or exclusive if you always work on it from one specific computer. With exclusive, run `git` only on the owner device: even `git status` rewrites `.git/index`, which counts as a write on any other device and freezes the repository there. With single writer, running `git` on a second device freezes the repository there if another device wrote within the last time window or if the second device is not yet fully synced; otherwise the write is accepted and makes the second device the current writer for one time window, during which running `git` on the first device will freeze it there.
- **Your root folder.** Single writer at the root is the safe default, but it carries a trade-off: if you edit a file on your laptop while offline and your desktop changes any other file in the same folder in the meantime, the laptop's entire folder freezes when it reconnects, even if the files are completely unrelated. **If the files in a folder are independent of one another (such as documents, photos, or notes), set the root to any writer and set only repositories and similar subdirectories to single writer or exclusive.** Two devices will then collide only when they edit the exact same file.

**Setting write control.** Once you know which mode a directory needs, configure it on the folder page:

1. Open the folder in the Web UI.
2. Open the **⋯** menu on a directory or file row and choose **Write control…** (`写入控制…`). You can also use the **⋯** menu in the title row, which acts on the directory you are currently viewing (the folder root when at the top level).
3. Pick **Inherit** (`继承`), **Any writer** (`不限`), **Single writer** (`单端`), or **Exclusive** (`独占`). For exclusive, also select the owner from the list of devices that map this folder (the list indicates which devices are currently online).
4. Press **Save** (`保存`).

If any device currently has held changes (section 8) inside this directory—or in a parent or child directory affected by the change—pressing **Save** first opens a confirmation dialog. It shows how many held changes exist and on which devices, and notes that saving will resubmit them under the new rules (for example, as ordinary changes if the directory becomes any writer), where they may be held again. If you cancel, the setting remains unchanged.

In the Web UI, write control is shown with icons rather than text labels. Every directory and file row carries an icon before its name whose shape and colour indicate its effective mode—a plain grey icon for any writer, a blue `1` for single writer, and a purple lock for exclusive—drawn solid when configured directly on that path and outlined when inherited; a red triangle on the icon marks held changes inside it. The same icon appears before each folder on the overview page (showing the root's mode at a glance) and before the current directory in the folder title row. Hover over the icon, tap it, or focus the item's name with `Tab` to open a tooltip showing the mode in words (**Any writer**, **Single writer**, or **Exclusive · device**) and what it means (a file inside an any-writer directory keeps the grey icon, and the tooltip notes that the file forms its own single-writer scope so concurrent edits freeze only that file; whether the setting is direct or inherited is indicated by the solid or outlined icon, and the write-control dialog names the source path); press `Escape` to close the tooltip. Beneath the folder title, a summary line also states the effective mode of the directory you are viewing, where it is inherited from, and—for single writer—which device is the current writer. You can open the write-control dialog for any path directly at `/folders/<id>?wc=<path>`, which is the link used by the new-folder reminder below.

**The reminder for a new folder.** Because the root's write mode determines how easily a folder freezes, the Web UI prompts you once for each new folder (as well as once for existing folders when you upgrade to this version). The notification bell shows a yellow warning for the folder—"No write rules are set yet; the root is single writer by default"—explaining the three modes and offering two links: **Set write rules…** (`设置写入规则…`), which opens the root's write-control dialog, and **Keep the default** (`保持默认`), which confirms single writer for the root. Saving any write setting in the folder also clears the reminder; once cleared, it never returns even if you later remove all write settings, and it is never sent via push notifications.

**Restores and older clients.** Two other operations interact with write control:

- **Restoring from the Web UI** (section 11) is always accepted and synced to every device. Because a restore counts as a write, edits made in a single-writer directory within one time window afterward—or on devices that have not yet received the restored files—are held. If the restored range includes an exclusive directory, or a single-writer directory whose current writer wrote less than one time window ago, the restore dialog notes that the restore will still proceed and sync to all devices, and that any other device modifying that range afterward (within the time window for single writer, or any non-owner device for exclusive) will be frozen.
- **Older clients.** A client whose protocol version is too old for the server stops syncing until you upgrade it (section 15), preventing it from bypassing write control.

## 6. Devices

Once your devices are enrolled (section 3) and syncing folders, you can monitor their connection status or retire them from the Web UI.

In the device table, a device is shown as **Online** whenever it has an active long-poll request open to the server, or had one end within the last 90 seconds. If an enrolled device does not connect for 7 days, the server raises a warning notification (section 9) so you notice if a background agent has stopped running.

**Revoke a device.** If a computer is lost, retired, or being reinstalled, open its device page, click the **⋯** menu in the title row, and choose **Revoke device…** (`吊销设备…`). The device's authentication token stops working immediately and all of its folder mappings are deleted on the server, while its local files stay untouched on disk. If the revoked client agent is still running, it logs `sync is stopped, the server does not accept this device` and slows its retry attempts to about once every ten minutes. To remove the client software from the computer afterward, follow section 16.1.

## 7. Exclusion rules

Not every file in a synced folder belongs on the server—temporary editor files, build outputs, and local caches waste bandwidth and version history. Exclusion rules let you tell kaze-sync which paths to ignore.

Rules live on the server in two layers (there are no per-device rules):

1. **Global rules**: configured on the Settings page under **Global exclusion rules** (`全局排除规则`). These apply to every folder on every device and start with a sensible default list (described below).
2. **Folder rules**: configured on each folder's top-level page via the **Exclusion rules** (`排除规则`) button (which shows the active rule count). Folder rules are appended after the global rules.

**Rule syntax.** Rules use `.gitignore` syntax, one pattern per line, matched against paths relative to the folder root using `/` as the directory separator:

- Blank lines are ignored, `#` starts a comment, and `!` negates an earlier exclusion. A trailing `/` matches directories only. A pattern containing `/` at the beginning or in the middle is anchored to the folder root; a pattern without `/` (other than a trailing `/`) matches at any depth.
- `*` and `?` match any characters except `/`; `[a-z]`, `[!a-z]`, and `[[:alpha:]]` match character classes; `**/x`, `x/**`, and `a/**/b` match across directory levels.
- The last matching rule wins. However, if a rule excludes a directory, clients do not descend into it, so no rule can re-include a file inside an excluded directory directly. To keep a single file inside an excluded directory, first re-include the directory itself, then exclude everything inside it, and finally re-include the specific file.
- Pattern matching is case-insensitive on Windows devices and case-sensitive on Linux devices.
- Brace expansion (`{a,b}`) and per-directory `.gitignore` files inside synced folders are not supported.

Examples:

| Rule | Meaning |
|---|---|
| `node_modules/` | Any directory named `node_modules`, at any depth |
| `/Videos/` | Only the `Videos` directory at the folder root |
| `*.iso` | Every `.iso` file at any depth |
| `Projects/**/bin/` | Any `bin` directory underneath `Projects` |
| `/*` then `!/Docs/` | Exclude everything at the root except the `Docs` directory |

Because folder rules are evaluated after global rules and the last match wins, a folder rule can override a global exclusion using `!`. For example, if the global rules exclude `build/`, adding these three lines to a folder's rules syncs only `build/keep.txt` in that folder:

```text
!/build/
/build/*
!/build/keep.txt
```

As you type in the rule editor, comments are coloured grey and invalid lines are highlighted in red. Hover over a red line to see what is wrong, or check the error list below the text box. To prevent broken rules from taking effect, the Web UI refuses to save while any line is invalid.

**Built-in and default exclusions.** Three kinds of internal files are always excluded automatically and cannot be overridden by your rules: the `.syncroot` marker file, temporary download files named `.syncpart-*` (any `.syncpart-*` left untouched for more than a day is cleaned up on the next scan), and SQLite database side files ending in `-wal`, `-shm`, or `-journal` (section 10).

In addition, `install` populates the **Global exclusion rules** once with defaults covering common OS and application clutter:
- Office and Windows leftovers (`~$*`, `*.tmp`, `Thumbs.db`, `desktop.ini`, `$RECYCLE.BIN/`, `System Volume Information/`, and a few more)
- Editor backup and lock files (`*~`, `.~lock.*`, `.*.sw?`, `*.kate-swp`)
- Partial browser downloads (`*.part`, `*.filepart`, `*.crdownload`)
- macOS metadata (`.DS_Store`, `._*`, `.Trashes/`, and others)
- Linux desktop and filesystem leftovers (`.Trash-*/`, `.directory`, `.fuse_hidden*`, `.nfs*`)
- Metadata from other sync tools (`.stfolder/`, `.stversions/`, `.stignore`, ...)

Open the Settings page to view or edit the full list; after installation, whatever text is saved on the Settings page is the single source of truth. Project-specific directories such as `.venv/`, `node_modules/`, caches, or log/telemetry files that change every few seconds (which would otherwise flood the server with versions) are deliberately left out of the defaults so you can add them as needed.

**What happens when rules change.** When you save updated rules, online clients receive them within a second:
- When a path **becomes excluded**, clients simply stop tracking it and leave the local copy on every computer untouched—a client never deletes a local file because of an exclusion rule. Instead, the server deletes its own copy of any newly excluded files when you save the rules (moving them into version history, as explained below).
- When you **remove an exclusion**, clients scan the previously ignored path, compare local files against the server, and put any differences on hold for your decision (section 8).

**Previewing and saving rules in the Web UI.** Because saving new exclusion rules deletes any already-synced server files that match those rules, the Web UI always makes you preview the impact before saving:
1. **Preview.** Both the folder rules dialog and the global rules card on the Settings page have a single primary action button. Whenever you edit the rules text and have not yet previewed your changes, that button reads **Preview** (`预览`). Clicking **Preview** does not save anything; instead, it checks which existing files on the server match your edited rules and displays them under the label "not in effect yet" (`尚未生效`), showing the total match count and up to the first 50 paths (including the folder name when previewing global rules), or stating that no server file matches (`没有命中服务端文件`).
2. **Filter the preview (optional).** If many server files match, you can inspect them using the one-line glob filter box in the preview: a pattern without `/` matches the filename only, a pattern with `/` matches the whole path, `*` stays within one path segment, `**` crosses `/`, `?` matches a single character, and case is ignored. The preview shows at most 50 matching paths and summarizes the counts underneath as "Files on the server that match: N, M after the filter, showing only K" (`命中的服务端文件: N 个, 过滤后 M 个, 目前仅显示 K 个`). Filtering only narrows what you see on screen—saving still deletes all `N` matching files on the server.
3. **Save.** Once you have previewed, the action button changes to **Save** (`保存`) if no server files match, or to a red **Save and delete N files** (`保存并删除 N 个文件`) button if `N` files match. (If you edit the rules text again after previewing, the preview closes and the button reverts to **Preview**.) Clicking **Save** applies the rules immediately without another confirmation prompt. If any matching files were uploaded or deleted on the server while you were looking at the preview, the save stops, refreshes the preview with the message "the matching files changed, please check again" (`命中文件有变化, 请再核对`), and lets you click save again once you have checked.

When matching server files are deleted by saving rules, they are recorded as ordinary deletions on the server: their contents remain in version history and can be restored during the retention period, while local copies on all devices (including devices that are currently offline or have not yet pulled the new rules) stay on disk and simply stop syncing. Any local files created later that match the rules are ignored from the start.

If you use the Web UI with JavaScript disabled, submitting the save form returns the preview page first whenever server files match, and clicking the red button on that page completes the save. Clicking **Cancel** discards unsaved edits and closes the dialog immediately; closing the dialog in any other way (such as pressing `Esc`) while you have unsaved edits asks whether to discard them.

## 8. Simultaneous writes and the decision dialog

In everyday use, changes flow from one device to the server and out to your other devices within seconds, so you rarely see the dialogs described here. They appear mainly when two devices modify the same place while out of contact with each other, or when a directory's write mode (section 5.1) refuses a write. Even then, kaze-sync never loses data, never relies on system clocks, and never creates duplicate conflict files: the first device to reach the server wins, and what the second device tried to write is **held** locally until you decide what to do with it.

**What gets held.** What counts as a collision depends on the write mode of the affected path, but the file-level cases below are always held on the second device (except the first row, where both devices made the exact same change):

| This device | Server now | Result |
|---|---|---|
| Changed | Changed to identical content | Accepted with no data transfer, nothing is held |
| Changed | Changed differently | Held |
| Changed | Deleted | Held |
| Deleted | Changed | Held |
| New | Exists with different content | Held |
| File | Directory with the same name (or the reverse) | Held |

A write mode can also hold changes even when no individual file collides—namely, when a second device writes to a single-writer directory or a non-owner device writes to an exclusive directory. Existing local files whose contents differ from the server when you adopt a non-empty directory (section 5) or after you restore a server backup (section 14) are held in the same way. Even when you choose to overwrite a server file, its previous content is still kept in version history.

**Seeing what is held.** Whenever a device has held changes, the notification bell lists an error entry for each affected device and directory, such as `"laptop has 3 changes waiting for your decision: /repo (written at the same time as desktop)"`. The text in parentheses gives the reason—either `"written at the same time as <device>"`, `"exclusive to <device>"`, `"differs from the server's version"`, `"a file and a directory share a name"`, or `"the server's history was reset"`. (Server push alerts in section 9.1 report only the folder name and held count.) You also see a red **On hold** tag per device on affected rows in the folder file list, and `"N on hold"` on the device page. Click the notification link or the **On hold** tag (or open `/folders/<id>?held=<device id>` directly) to open the decision dialog. If the device is currently offline, the dialog notes this and queues your decision until the device reconnects.

**The decision dialog.** The dialog lists all held changes for one device, grouped by the frozen directory. Because a single operation such as a `git` command can touch hundreds of related files at once, you will usually resolve an entire group together using the two buttons above the group:

- **Discard all** (`全部丢弃`): discards all of the device's held changes in this group and replaces those local paths with the server's current versions.
- **Upload all and overwrite** (`全部上传并覆盖`): uploads all of the device's held changes in this group (after asking for confirmation) to replace the server's versions, moving the replaced server content into version history. Only the files that this device actually changed are overwritten; files it did not touch remain as the first device left them.

When a group needs file-by-file attention, press **Choose one by one** (`逐个选择`) below the group buttons. This expands a table with one row per path showing the local change (modified, added, or deleted, with size and timestamp), the base version it started from, the current server version (and which device wrote it), and an action menu. Pick an action for the rows you want to resolve (leaving any others undecided) and press **Apply the choices** (`执行所选`):

| Action | Available when | What happens |
|---|---|---|
| Upload (`上传`) | The server has not changed this path since the device's version | The device's change goes to the server and then to the other devices |
| Upload and overwrite (`上传并覆盖`) | Always | The same, replacing whatever the server has now; the old server content is kept in version history |
| Discard (`丢弃`) | Always | The device's version is dropped and the server's current version is downloaded |

When a file and a directory share the same name, only **Upload and overwrite** and **Discard** are available. Be careful about mixing different actions within a repository or other tightly coupled directory, as doing so can leave the server with a mix of both devices' versions (using a single group button avoids this). As soon as you select two different actions in the same group, a warning appears above the table: `"Different actions in one scope can leave the server with a mix of both devices' versions. Prefer one action for the whole scope."` This is only a warning and does not block applying your choices. Each group table displays at most 2000 rows and notes how many additional items remain (use the group buttons for the rest); the dialog does not auto-refresh while open, so reopen it to check progress.

**What happens to the discarded file: `.syncsaved`.** Discarding a held change does not have to delete your local copy. By default, **When discarding, keep the local copy in .syncsaved** (`丢弃时把本机那一份保存到 .syncsaved`) is ticked: before downloading the server's version, the client moves its local file to `.syncsaved/<YYYYMMDD-HHMMSS>/<original path>` in the sync root. Every file discarded in a single decision is grouped under the same timestamped directory so you can copy anything back by hand. Untick the checkbox if you want the client to delete the discarded local file immediately (though a local directory that collides with a same-named server file is always moved to `.syncsaved` and never deleted outright). `.syncsaved` is never synced, and the client automatically removes timestamped directories older than 30 days during folder scans. If you want the local version to become the synced version on the server, choose **Upload and overwrite** instead.

**Safeguards.** Because either the local file or the server version might change while you are looking at the dialog, the client verifies that neither side has changed before carrying out your decision. If anything changed in the meantime, the action for that path is aborted, the item remains held, and the row explains why:

- `"Changed again on the device: choose again"` (`本机又改过, 请重新选择`): the local file's size or modification time no longer matches what was shown in the dialog, ensuring the client never uploads or discards unseen edits.
- `"The server changed: choose again"` (`服务端又变了, 请重新选择`): another write updated the path on the server after the dialog opened; the row updates to show the new server state.
- `"It failed: try again"` (`执行失败, 请重试`).

The group-level buttons enforce the same check: they apply only to paths that have not changed on the server since you opened the dialog, leaving any newly changed paths held so you can inspect them individually.

**After you decide.** Clicking a button queues your decision in the server's memory for the device to carry out on its next contact with the server (usually within seconds). While pending, affected rows display `"Waiting for laptop"` (using the device's name) for up to 10 minutes and disappear as soon as the device completes the action. If the device has not reported back after 10 minutes, the label disappears and the rows show the action menu again as if undecided, but the decision remains queued on the server and will still run when the device next connects (choosing a new action replaces the queued one). Because pending decisions live only in server memory while held changes are stored on the client device, if the server restarts before the device carries out a decision, the device re-reports its held changes and you simply submit the decision again.

Once every held change in a directory has been resolved, the device re-reads that directory from the server and the freeze ends. If you change the directory's write control mode while changes are still held (for example, switching it to any writer), the device resubmits its held changes under the new mode.

**Other filesystem cases and edge cases.** Not every surprise comes from two devices writing at once. The following cases come from renames, from names and links that Windows and Linux treat differently, and from things a folder does not sync:

- **Rename or move.** Renaming or moving a file is synced as a deletion at the old path plus a new file with the same content hash at the new path. Because other devices already have those data blocks locally, they copy the content on disk instead of downloading it again. If one device moves a directory while another device edits a file inside it, the edited file is kept at its old path so the edit is not lost.
- **Filename case.** Because Windows and Linux share folders, the server treats filenames case-insensitively within a folder. If a Linux client creates both `A.txt` and `a.txt` in the same directory, the first one uploaded succeeds and the second is rejected (`被拒绝`) and reported as an error; rename one of the files to clear it. On Windows, renaming a file solely to change its case (`a.txt` to `A.txt`) works as expected.
- **Windows-illegal names.** So that every synced folder can be mapped on Windows, the server rejects filenames that Windows forbids regardless of which OS created them: names containing `<>:"\|?*` or control characters, names ending with a dot or a space, and the reserved device names `CON`, `PRN`, `AUX`, `NUL`, `COM1`-`COM9`, and `LPT1`-`LPT9`. Rename the file on the device where it was created, or exclude it with a rule.
- **File and directory with the same name.** If one device creates a file and another creates a directory at the exact same path, the second device to reach the server holds its change; the dialog offers only overwrite and discard.
- **Symbolic links.** Symbolic links are synced as links and are never followed: the server stores the link target as literal text (whether it is a relative path, an absolute path, a dangling link, or a path pointing outside the folder; making sure the target makes sense on other devices is up to you). The target's content is never synced through the link, and because kaze-sync never reads or writes through a symbolic link, a link pointing outside the folder can never cause downloads to write outside the sync root. A symbolic link whose target path is longer than 4096 bytes is skipped (reported as `skipped`, `已跳过`).
  If a receiving device cannot create symbolic links—most commonly Windows when Developer Mode is turned off—the link remains safely on the server while nothing is created on that Windows machine. To avoid flooding you with alerts, the notification bell shows a single folded warning line per device (`win: 3 个符号链接未建立 (Windows 需开启开发人员模式)`), and the folder page shows those links as grey **Not synced** rows. If you turn on Windows Developer Mode (Settings -> System -> For developers), the client creates the missing links automatically without needing a restart. That notification line updates only when the set of affected links changes.
- **What is not synced inside a folder.** Windows directory junctions are neither followed nor synced (they are reported as `skipped`). Empty directories, file permissions other than the executable bit, file ownership, and extended attributes are not synced. File contents are always copied byte for byte without line-ending conversion.

## 9. Notifications

Because client agents run silently in the background with no tray icon or pop-ups on your computers, the Web UI notification bell is your central place to see anything that needs attention. (If you want alerts pushed to your phone or desktop without opening the Web UI, see section 9.1.)

Click the bell icon in the top bar to open the notification panel. The badge on the bell shows the total number of active issues—coloured red when any error is present, yellow when there are only warnings, and capped at `99+` above 99. Inside the panel, entries are grouped by folder, while devices that have been offline for a long time are grouped under **Devices** (`设备`). Each entry shows what went wrong, the file path (which you can click to jump straight to that file in the folder page, highlighted for two seconds), the affected device, when the issue started, and a short hint on how to fix it. If a single device reports more than 5 entries of the same kind in one folder, they collapse into a single summary line that you can click to expand.

Notifications are never dismissed manually; instead, they reflect the live state of your folders and devices, and each notification disappears on its own as soon as you fix the underlying cause. Client-reported issues are held in server memory, so after a server restart they reappear as soon as the client reports them again (long-offline devices are derived from the database and do not wait for a client report; held changes are re-sent by each device within moments, because they are stored on the device).

| Source | Level | What to do |
|---|---|---|
| Changes on hold ("N changes waiting for your decision") | Error | Open the decision dialog from the entry's link and decide what to keep (section 8) |
| No write rules set for a new folder | Warning | Choose who may write with **Set write rules…** (`设置写入规则…`), or accept single writer for the root with **Keep the default** (`保持默认`) (section 5.1) |
| `rejected` (`被拒绝`): case collision or illegal name | Error | Rename the item on that client, or exclude its path |
| `io` (`读写错误`): other local read or write error | Error | Check the disk and file permissions on that client using the error message |
| `folder` (`文件夹错误`): sync root missing, or non-empty directory not allowed to be adopted | Error | Mount the disk, or fix the path / tick adoption on the device page |
| `in_use` (`占用中`): file locked, being written, or an open SQLite database | Warning | Close the program using the file (section 10) |
| `skipped` (`已跳过`): symbolic link this device cannot create (section 8), link target longer than 4096 bytes, Windows junction, or non-UTF-8 filename | Warning | Add an exclusion rule if you want to silence the warning |
| `report` (`待处理改动未上报`): this client could not tell the server about its held changes | Warning | Retried automatically; check that the client can reach the server |
| Device not connected for 7 days | Warning | Bring the device back online, or revoke it |

When a client skips or gets a file rejected, that file does not exist on the server. So you can still see it while browsing the Web UI, the folder page lists skipped and rejected items in place inside their directory as grey **Not synced** rows (`未同步: ...`); open the row menu and click **Details** (`详情`) to see the error message, device, and start time. Both folder pages and device pages also show a status line right under the page title: clicking its issue summary (such as `1 个错误, 2 个提醒`) opens the notification panel, and hovering over the device count (`2 台设备`) shows the device names in a tooltip. On a folder page, the file count, total size, error count, and reminder count in that line cover the directory you are viewing and everything below it, whereas the sync status label (syncing, error, device offline, and so on) describes the folder as a whole and appears only at the folder's top level, not inside subdirectories.

### 9.1 Push messages with ntfy

So you do not have to open the Web UI to check whether everything is healthy, both the server and the clients can push short alert messages to an [ntfy](https://ntfy.sh) server (either your own self-hosted instance or `ntfy.sh`).

Push notifications are off by default. To enable them on the server or on a client, create a JSON file named `ntfy.json` with two keys and restart that program:

```json
{"url": "https://ntfy.example.org", "topic": "choose-a-long-random-name"}
```

- **Where to put `ntfy.json`**: on the server, place it in the `--data` directory (next to `var/`); on a client, place it in the state directory (`~/.local/state/kaze-sync` on Linux, `%LOCALAPPDATA%\kaze-sync` on Windows). Because each program reads its own `ntfy.json`, a client can post to a different topic than the server. Removing the file or leaving `url` or `topic` empty turns push notifications off. Because anyone who knows your topic name on a public ntfy server can read your alerts, pick a long random topic name, keep `ntfy.json` private, and never commit it to a repository.
- **Privacy**: each push message is a single short line naming the folder or device and the kind of problem. It never includes filenames, file contents, passwords, or the admin password.
- **What a client pushes**: when it cannot reach the server for about five minutes; when the server rejects the device (sync stopped); when the client protocol version is too old for the server (sync stopped); when a folder fails to scan or pull three cycles in a row; when a folder continues to report `io`, `folder`, or `rejected` errors; or when a folder's local state database cannot be opened. Warnings (`in_use` and `skipped` files, and `report`) are shown only in the Web UI and are never pushed.
- **What the server pushes** (checked every five minutes): when a folder has changes on hold (one message per folder, reporting only the count); when device-reported errors persist; when a device has not connected for 7 days; when free space on the data disk drops below 256 MB; when the daily garbage collection cleanup fails; or when the server certificate cannot be renewed or is about to expire.
- **Rate limiting and recovery**: to avoid spamming you, each distinct problem is pushed once and not repeated until it clears; even if the same problem flaps on and off, it is pushed at most once per hour, and total pushes are capped at 20 messages per hour. When a previously pushed problem clears, kaze-sync sends a single `Recovered` message. If the ntfy server itself is unreachable, kaze-sync logs a single line and continues syncing normally.

## 10. Files in use and SQLite databases

When you work on files inside a synced folder, programs often hold files open or write to them over time. kaze-sync uses several safeguards so it does not upload half-written files or clash with open applications:

- **Locked files on Windows.** Applications such as Word and Outlook lock open files against reading, renaming, or deletion. Whenever an operation on Windows fails because another program holds the file open, the client marks the file as `in_use` (a warning in the notification bell) and retries automatically after 30 seconds, 2 minutes, and 10 minutes, and then on every subsequent scan. If a download is waiting to overwrite a locked file, the client keeps the downloaded temporary file on disk so it does not have to download it again. As soon as you close the program, the sync finishes on its own.
- **Freshly modified files.** Before uploading any modified file, the client checks that its modification time is at least 5 seconds old and does not change while the file is being read. This prevents half-written files from being uploaded while an application is still saving them.
- **SQLite databases.** Syncing a live SQLite database while transactions are in flight would corrupt it if the main database file and its write-ahead log or rollback journal were synced separately. Therefore, the side files `<name>-wal`, `<name>-shm`, and `<name>-journal` are always excluded from sync, and whenever `<name>-wal` or `<name>-journal` exists and is non-empty, the client treats the main database file `<name>` as `in_use`: it neither uploads the database nor overwrites it with a download. In SQLite WAL mode, this means the database syncs as soon as the application closes it cleanly (or after you run `PRAGMA wal_checkpoint(TRUNCATE)`). Still, never open and write to the same SQLite database on two machines at the same time, or both sides will modify it independently and the clash is held for your decision (section 8), where you keep one of the two versions.
- **Virtual machine images and Linux files.** Virtual machine disk images sync reliably only while the virtual machine is shut down. Note that on Linux, the operating system provides no general mandatory lock check to tell whether another process has an ordinary file open for writing, so on Linux only the 5-second quiet rule above protects active files.

## 11. Restoring old content

Whenever a file is overwritten or deleted on any device, the server keeps the previous version for the configured retention period (section 13). For simplicity and security, the Web UI only displays filenames and metadata and never offers direct browser downloads; instead, restoring always writes the chosen historical content back to the server as the new current version, which connected clients then download just like any other change. Because a restore is recorded as a new version rather than rewriting history, every restore can itself be undone.

Depending on what went wrong, you can restore a single file version, a deleted file, or an entire directory tree as of a point in time:

**Restore one version of a single file.** On the folder page, open the file's row menu (by right-clicking the row, clicking the **⋯** button at the end of the row, long-pressing on a touch screen, or pressing `Shift+F10` with the keyboard) and choose **Version history…** (`版本历史…`). The dialog lists every version with its timestamp, size, and the device that created it, with the current state in the first row. Click **Restore** (`恢复`) on the version you want: the row is highlighted and a confirmation panel appears below the table; click **Restore** (`恢复`) there to confirm, or **Cancel** (`取消`). The file's previous current content moves into version history, and the restored version becomes current.

**Restore a deleted file.** Turn on **Show deleted** (`显示已删除`) at the top of the folder page. Deleted files appear in the list as grey rows badged with **Deleted** (`已删除`). Open the row menu on a deleted file and choose **Restore last content…** (`恢复最后的内容…`) to bring back its final version immediately, or open **Version history…** (`版本历史…`) to pick an earlier version.

**Restore a file, a directory, or a whole folder to a point in time.** If a batch of files was overwritten by mistake or a whole directory was deleted, open the row menu of the affected file or directory and choose **Restore to a point in time…** (`恢复到某一时刻…`). To restore the directory you are currently viewing, open the title row menu (or right-click empty space in the file list) and choose **Restore this directory to a point in time…** (`恢复此目录到某一时刻…`)—when you are at the top level of a folder, this item reads "Restore whole folder..." (`恢复整个文件夹到某一时刻…`). Then follow the three steps in the dialog:

1. **Choose the target time.** You can either pick the time visually on the two timeline axes or type it directly into the time box as `2026-11-03 22:48:27` (seconds are optional). When the dialog opens, it starts at "now" (`现在`) of today:
   - **How the two axes work.** The upper axis is the **day axis**, running from the earliest restorable point on the left (either the start of the retention period or the end of the initial import described below, whichever is later) to "now" (`现在`, labelled outside the right end of the axis). Its blue vertical line snaps only to midnight (`00:00`) of each day (except on the earliest day, where it stops at the earliest restorable timestamp, and on the latest day, where it stops at today's `00:00`); drag the line or click anywhere on the upper axis to jump to the nearest day boundary. The upper axis labels days by day-of-month (`30`, `1`, `2`), or as `MM-DD` (`12-01`) when the retention period is longer than 30 days, showing as many labels as fit the screen width.
     The lower axis is the **time-of-day axis**, which always spans the full 24 hours (`00:00` to `24:00`) of whichever day is selected on the upper axis, with a tall tick at `00:00`, `24:00` at the right end, and hour ticks in between (finer on wide screens, sparser on narrow screens). Drag its blue line or click on the lower axis to choose the exact minute within that day (to specify seconds, type them in the time box). Unreachable parts of a day—the hours after "now" on today, or the hours before the earliest restorable point on the first day of the range—are drawn hatched and the line cannot enter them. The two axes never move each other when dragged: dragging the lower axis stays within the chosen day without moving the upper line, and dragging the upper axis moves only in whole days.
   - **Reading the yellow change marks and blue shading.** Beneath both axes, yellow tick marks show when changes actually occurred in the file or directory you selected: on the upper axis, one mark per change at its timestamp (multiple changes on the same day appear as multiple marks, and changes falling on the same pixel combine into a taller mark); on the lower axis, one mark for each minute that had a change. The pale blue region to the left of the blue line represents the history that stays after the restore, while everything to the right of the line will be rolled back. Although dragging the blue line does not snap to yellow marks, the state of your files only changes at those yellow marks: placing the line on or to the right of a yellow mark includes that change.
   - **Stepping with buttons or keyboard.** To jump from one actual change to the next, click the `‹` and `›` buttons (which cross day boundaries automatically) or focus the lower axis and press the Left/Right arrow keys (which stay within that day; `PageUp`/`PageDown` move by one hour, `Home` and `End` jump to the ends of the day). When the upper day axis is focused, the Left/Right arrow keys move by one day (`PageUp`/`PageDown` move by seven days, `Home` jumps to the oldest point, `End` jumps to now). Typing a timestamp into the box moves both blue lines to that moment. While you drag a line, only the lines and the timestamp text update (including the note above the box showing how long ago that time is); the file preview refreshes once you release the mouse button (or after a brief pause when using the keyboard or `‹`/`›` buttons), and only if the effective state actually changed. When the line sits at "now" or after the most recent change, nothing differs from the current state, so the preview prompts you to choose the day first and then the time of day.
   - **Time zone, retention, and earliest-record rules.** Below the preview, a summary block shows the active time zone (which you can change on the Settings page), the retention period, and the earliest restorable timestamp. If you type a time older than the retention period, the dialog warns that old versions from that time may already have been purged. If you pick a time earlier than the folder's very first recorded change, the server compares against the state at the end of the folder's **initial import**—the first moment the first device finished its initial full upload (which the server detects as the first 2-minute quiet period after the first files arrived)—and displays: "The chosen time is earlier than the earliest record, computed against the state when the initial import finished: initial import finished at <time>" (`所选时间早于最早记录, 按首次导入完成时的状态计算: 首次导入完成于 <时间>`). Files created after that initial import did not exist back then and are left untouched (because a point-in-time restore never deletes files). Similarly, if you are restoring a specific directory or file that was created after the initial import and pick a time before its creation, the server uses that directory or file's own earliest record instead (`所选时间早于最早记录, 按最早记录计算: 最早记录为 <时间>`). In every case, what the preview shows is exactly what the restore will do. If the selected target has no history at all, the dialog states `这里没有任何历史记录`.
2. **Review the preview.** The preview lists up to 50 files whose state at the chosen time differs from now, marking each row as either "deleted" (`已删除`) or "changed" (`已改变`) as of today (highlighted in two distinct pale colours). If the list is long, use the one-line glob filter in the preview header (same syntax as in section 7; filtering only changes what is displayed, while the restore still applies to all affected files). Beneath the list, a summary line reads "Files that existed then and differ now: N, M after the filter, showing only K" (where `N` is the true total, and the filtered/truncated counts appear only when applicable). The preview box maintains a fixed height so the dialog does not jump while you scrub the timeline, and if no files differ at the chosen time, the restore button stays disabled.
3. **Press Restore (`恢复`).** Once the restore completes, a status banner reports how many files were restored and how many failed (for example, due to a name conflict or a concurrent edit at that exact moment).

For every affected path, a point-in-time restore writes back the version that was current at the chosen timestamp. Importantly, it restores **content only**: any new files created after the chosen timestamp are never deleted, and the overwritten current versions are saved into history. This design safely handles both "I accidentally deleted a directory" and "a batch of existing files was overwritten with bad data".

**Restore and write control.** A restore is always accepted regardless of write control (section 5.1). If the restored range contains an exclusive directory, or a single-writer directory whose current writer wrote less than one time window ago, the dialog shows a note: the restore goes ahead and syncs to every device, and any other device that modifies the range afterward (within the time window for single writer, or any device other than the owner for exclusive) is frozen.

## 12. Export to plain files

While each client computer holds an ordinary directory tree of the folders it maps, the server itself stores content as deduplicated blocks and SQLite databases rather than plain files. If you ever need to extract ordinary files directly from the server—for disaster recovery or to migrate away from kaze-sync—run `kaze-syncd export` on the server as root (since the files under `var/` belong to the `kaze-sync` service user):

```bash
kaze-syncd export DIR [--data DIR] [--folder NAME|ID] [--at "2026-11-02 12:00[:00]"]     # server
```

- **Target directory (`DIR`) and `--folder`**: without `--folder`, the command exports every folder into its own subdirectory `DIR/<folder name>`; with `--folder NAME|ID`, it writes that single folder's files directly into `DIR`.
- **Point-in-time export (`--at`)**: without `--at`, the current state is exported; with `--at`, it exports the state as of that timestamp, interpreted in **UTC**.
- **`--data`**: defaults to the directory containing the `kaze-syncd` binary you invoke.

Because `export` reads the databases and block files directly from disk, it works whether the server service is running or stopped, and you can run it directly against an unpacked backup directory. As it writes each file, `export` restores the file content, executable bit, and modification time, and verifies the written file against its cryptographic hash. If any file has a missing or damaged block, `export` reports that file, skips it, continues exporting all remaining files, and exits with a non-zero status code at the end. Make sure the destination disk has enough free space to hold the full uncompressed size of the folders you are exporting.

## 13. Retention and garbage collection

Keeping every overwritten or deleted file forever would eventually fill your server's disk, so kaze-sync automatically expires old history and reclaims disk space in the background:

- **Retention period.** Every overwrite or delete keeps the superseded version for `keep_days` days, which you configure under **Version retention** (`版本保留`) on the Settings page (1 to 3650 days, default 30). If you lower this number to free up space, saving the setting triggers a garbage collection pass immediately without needing a service restart.
- **When garbage collection runs.** The server runs garbage collection once a day (starting one minute after the server process boots, and every 24 hours thereafter), plus immediately whenever you save a new retention setting. A collection run first deletes version records older than `keep_days`, then deletes any data blocks that are no longer referenced by any current file or retained version and are more than one hour old, and finally cleans up stale temporary files in `var/tmp/`. The one-hour grace period protects blocks that a client has just uploaded as part of an in-progress sync but has not yet committed. Garbage collection runs in the background and does not interrupt syncing.
- **When disk space actually shrinks.** Because unreferenced blocks are removed by garbage collection rather than at the moment you delete a file, disk usage drops only after the next collection pass finishes. Each run logs a single summary line with `msg=gc` showing how many versions and blocks were removed, how many scan passes it took, and the total duration (on OpenWrt, view it with `logread | grep 'msg=gc '`).
- **Memory cap.** Garbage collection never uses more than a fixed memory budget (3/8 of the machine's RAM, at least 64 MB); with a very large data set it performs more scan passes instead. How much memory a given number of files needs is in "Memory sizing" below.
- **Frequently changing files.** Every time a file changes, the previous content becomes a retained version. If an application rewrites a cache or status file every few seconds, it will generate thousands of versions; add an exclusion rule for such files (section 7).

### Memory sizing

Garbage collection keeps one 32-byte hash per block reference in memory. The array is sized from the actual number of references, so a small data set uses very little, and it never grows past a budget of 3/8 of the machine's RAM (or of the memory limit of its container or service, if lower), with a minimum of 64 MB. A 512 MB machine therefore has a budget of 192 MB, which is about 6 million references in one pass. If there are more references than that, garbage collection splits the work into as many passes as needed (up to 256): it only takes longer, it does not fail. This is extra memory used while garbage collection runs; the server's resident memory (section 2.5, "Resource usage") is separate.

The number of references is the sum, over every current file and every retained historical version, of its number of blocks (blocks are 256 KiB; a file smaller than that is one block; a block shared by several files or versions is counted each time). So:

```text
references  ~  files x (1 + v) x average blocks per file
GC memory   =  min(references x 32 bytes, budget)
```

Here `v` is the average number of historical versions each file has inside the retention period. Every overwrite or delete of a file adds one version, and a version is dropped once it is older than `keep_days`, so `v` grows with both the retention period and how often your files change: with the default 30 days, a file edited once a week has about 4 versions, one that never changes has none, and one rewritten every day has about 30. Doubling `keep_days` roughly doubles `v` for files that change regularly (files that never change are unaffected). The table assumes `v = 2` and one block per file, which is an assumption for a typical document and source-code collection, not a measurement; to use your own value, multiply by `(1 + v) / 3` and by the average blocks per file. Each run of garbage collection logs `block_refs=<n>` on its `msg=gc` line, so after a run you can divide it by your file count to get the real `(1 + v) x blocks` for your data.

| Files (v = 2, 1 block each) | References | GC extra memory | On a 512 MB machine (budget 192 MB) |
|---|---|---|---|
| 10,000 | 30,000 | about 1 MB | one pass |
| 100,000 | 300,000 | about 10 MB | one pass |
| 1,000,000 | 3,000,000 | about 96 MB | one pass |
| 5,000,000 | 15,000,000 | 480 MB wanted, capped at the budget | 3 passes, only more scan time |

Large files count per block: a 1 GB file has 4,096 blocks per version, so if it changes every day for 30 days it adds about 127,000 references (about 4 MB). Keeping the resident memory and the garbage-collection memory both in mind, a 512 MB machine is comfortable for a million files at the default retention; the number to watch when you raise `keep_days` or sync many large, frequently changing files is how many passes the `msg=gc` line reports.

## 14. Backup and restore of the server

Although every client computer holds ordinary copies of the folders it syncs, the server itself is not a backup: your current metadata and all historical versions live on the same server disk. To protect your version history, device registrations, and folder setup against disk failure, back up the server's data directory to another disk or another machine.

**What to back up.** A complete server backup consists of:
- `<data>/var/meta.db` and `<data>/var/folders/` (the global database and per-folder databases)
- `<data>/var/blobs/` (the content blocks)
- `<data>/var/tls/` and, if you use local CA mode, `<data>/ca/` (the server certificate and CA keys; if you lose these, a restored server will generate a new certificate or CA with a new fingerprint, forcing you to re-enroll every client and re-trust the CA in your browsers)

Because `meta.db` and the databases under `folders/` must come from the exact same instant, **stop the service for a few seconds** while copying them (and avoid running the backup while daily garbage collection is deleting blocks). Content blocks under `blobs/`, on the other hand, are immutable once written, so you can copy them while the service is running—including incrementally with `rsync`.

Here is the complete backup procedure (shown with `systemd`; substitute your service manager's stop and start commands on OpenWrt or OpenRC):

```bash
systemctl stop kaze-sync                                                          # server
cd /var/lib/kaze-sync/var && tar cf /backup/meta.tar meta.db folders tls
systemctl start kaze-sync                                                         # downtime: seconds
tar cf /backup/blobs.tar blobs
tar cf /backup/ca.tar -C /var/lib/kaze-sync ca                                     # only in local CA mode
```

**Restoring a backup.** To restore onto a new server or roll back in place, use a `kaze-syncd` binary of the same or a newer version and unpack all archives together (never restore only a subset of the files):

```bash
mkdir -p /var/lib/kaze-sync/var                                                    # server
tar xf /backup/meta.tar  -C /var/lib/kaze-sync/var
tar xf /backup/blobs.tar -C /var/lib/kaze-sync/var
tar xf /backup/ca.tar    -C /var/lib/kaze-sync                                     # only in local CA mode
./kaze-syncd export /tmp/check --data /var/lib/kaze-sync                           # optional: check the data reads
./kaze-syncd install --data /var/lib/kaze-sync --new-epoch                         # fixes owners, new history id, starts the service
```

Passing `--new-epoch` to `install` is **required** after every restore (and must never be used for ordinary upgrades). Because restoring a backup moves the server's databases back in time relative to what your clients have already synced, `--new-epoch` assigns a new history identifier that tells every connecting client to discard its cached knowledge of the server and reconcile from scratch once:
- You do not need to touch the clients.
- As long as `<data>/var/tls/` (and `<data>/ca/`) were restored along with `meta.db`, the admin password, enrolled devices, and folder mappings are all preserved and clients reconnect automatically.
- Any file on a client whose content differs from the restored backup is put on hold for your decision (section 8; nothing is lost); any file created on a client after the backup was taken is uploaded again; and any file deleted on a client after the backup was taken is downloaded again from the backup. Without `--new-epoch`, clients would notice only some of the rolled-back changes and could silently remain out of sync with the server.

For a step-by-step restore walkthrough using OpenWrt commands, see "备份与还原" in [`docs/operations.md`](../operations.md).

## 15. Upgrading

When upgrading to a newer version of kaze-sync, always upgrade the **server first**, and then upgrade your clients.

**Why the order matters.** Some releases update the sync protocol (the current protocol version is 6, which added write control and raised the server's minimum required client protocol version to 6, meaning every client must be upgraded after upgrading the server; version 5 had added symbolic links). If you upgrade a client before the server, the new client will fail on batched downloads against the older server. Conversely, when you upgrade the server first, any client older than the server's minimum protocol version is cleanly paused with HTTP 426: the client logs `client too old, sync is stopped until kaze-syncc is upgraded` about once a minute, and the Web UI device table marks the device in red as "`过旧`" next to its version (`-` when the client is too old to report a release version). Its local data is unharmed, and as soon as you upgrade that client binary, syncing resumes automatically. (Only protocol changes that make old clients incompatible raise the minimum version, so most server upgrades do not force an immediate client upgrade.)

Follow these four steps to upgrade:

1. **Download and verify the new release packages** (`kaze-sync-<version>-linux-amd64.tar.gz`, `kaze-sync-<version>-windows-amd64.zip`, and `SHA256SUMS` from [GitHub Releases](https://github.com/pellorao/kaze-sync/releases), following [section 2.1](#21-download-and-verify-the-server-package) and [section 3](#3-installing-and-enrolling-clients); if you build from source instead, see [Appendix A](#appendix-a-building-from-source)).
2. **Upgrade the server.** Copy the new `kaze-syncd` binary to the server and run `./kaze-syncd install --data <same directory>` as root (on both OpenWrt and plain Linux). Your listening port, certificate, and admin password remain unchanged. `install` copies the new binary into `<data>/`, restarts the service, and automatically migrates the databases (see below). On systemd, `install` also reloads the daemon and rewrites `/etc/systemd/system/kaze-sync.service`, so keep any custom unit settings in a systemd drop-in. (On OpenWrt, running a system firmware upgrade via `sysupgrade` leaves both the data disk and the kaze-sync service intact.)
3. **Upgrade each Linux client.** Stop the user agent, overwrite the binary at its existing path, and start the agent again:

```bash
systemctl --user stop kaze-sync                    # client
cp kaze-syncc ~/.local/bin/kaze-syncc
systemctl --user start kaze-sync
```

4. **Upgrade each Windows client.** If you built the Windows binaries from source (or want to double-check a binary before copying it), verify that `kaze-syncc-agent.exe` was built with `-ldflags -H=windowsgui`. Run this check—it must print `2` (GUI subsystem) and never `3` (console subsystem, which would pop up a visible terminal window at every Windows sign-in):

```
python3 -c "import struct,sys; d=open(sys.argv[1],'rb').read(4096); print(struct.unpack_from('<H', d, struct.unpack_from('<I', d, 60)[0]+92)[0])" kaze-syncc-agent.exe
```

   Because Windows locks a running `.exe` file, stop the background agent in PowerShell, copy both `.exe` files over the existing ones, and start the agent again:

```powershell
Stop-Process -Name kaze-syncc-agent
Copy-Item kaze-syncc.exe, kaze-syncc-agent.exe <install directory>
Start-Process "<install directory>\kaze-syncc-agent.exe" -ArgumentList run
```

**Automatic database migration and how to roll back.**
- **Server databases.** Whenever the server starts (including during `install`), it automatically migrates its databases to the new schema—first the global `meta.db`, then each folder database. If the schema has not changed, startup proceeds immediately. Before modifying any database file, the server copies the untouched file to `<data>/var/backups/` as `meta-v<old version>-<UTC time>.db` or `folder-<id>-v<old version>-<UTC time>.db` (keeping the newest 3 backups per database, and removing a folder's backups if that folder is deleted). Because each backup is a full copy of the database, make sure your disk has at least that much free space before upgrading. If backing up or migrating a database fails, the server refuses to start, prints which database failed and why, and leaves that database unchanged—fix the underlying issue (such as freeing disk space) and start the service again.
  If you ever need to **roll the server back** to an older version after a migration has run, an older `kaze-syncd` binary will refuse to start against a newer database schema and tell you why rather than corrupting it. To roll back safely: stop the server, put the old `kaze-syncd` binary back, copy the newest pre-upgrade backup of each migrated database from `var/backups/` back over `var/meta.db` and `var/folders/<id>.db` (and delete any `-wal` and `-shm` files next to them), and then run the restore command from section 14 including `--new-epoch`.
- **Client databases.** A client's per-folder state databases (`folder-*.db` inside `~/.local/state/kaze-sync` on Linux or `%LOCALAPPDATA%\kaze-sync` on Windows) also change schema in some releases. If you might want to roll a client binary back to an older version, stop the agent and back up its `folder-*.db` files before replacing the binary, and restore them alongside the old binary if you roll back. If an older client binary encounters a `folder-*.db` created by a newer client, it stops syncing that folder and reports the version mismatch as the folder's error rather than failing on every scan.

## 16. Uninstalling

When you retire a computer or decommission your server, follow these steps to remove the background services cleanly.

### 16.1 Client

Always start in the Web UI so the server stops expecting the device, and then remove the client from the computer. Your local synced files are left untouched in all cases.

1. **In the Web UI**: open the device page and click **Revoke device…** (`吊销设备…`) (or simply delete its folder mappings if you want to keep the device entry).
2. **On Linux**: run `systemctl --user disable --now kaze-sync`, delete `~/.config/systemd/user/kaze-sync.service` (then run `systemctl --user daemon-reload`), delete the state directory `~/.local/state/kaze-sync` (which holds the device token, log, and local sync state), and delete the `~/.local/bin/kaze-syncc` binary.
3. **On Windows**: stop `kaze-syncc-agent.exe` (`Stop-Process -Name kaze-syncc-agent`), delete the `kaze-sync` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, delete `%LOCALAPPDATA%\kaze-sync`, and delete `kaze-syncc.exe` and `kaze-syncc-agent.exe`.
4. **Optional cleanup**: delete the hidden `.syncroot` marker file at the root of each formerly synced local directory.

### 16.2 Server

There is no automated `uninstall` command for the server. The commands below permanently delete the server and **all synced files and version history stored on it**—take a backup or run `export` first if you might need anything. Before removing the server, revoke and uninstall all clients (section 16.1) so they do not keep retrying connections in the background.

On **systemd**:

```bash
systemctl disable --now kaze-sync                  # server
rm /etc/systemd/system/kaze-sync.service
systemctl daemon-reload
userdel kaze-sync
rm -r /var/lib/kaze-sync                           # the data directory you gave to --data; contains everything
```

On **OpenRC (Alpine)**:

```bash
rc-service kaze-sync stop                          # server
rc-update del kaze-sync default
rm /etc/init.d/kaze-sync
deluser kaze-sync                                  # also removes the group
rm -r /var/lib/kaze-sync                           # the data directory you gave to --data
```

On **OpenWrt**:

```bash
/etc/init.d/kaze-sync stop                         # server
/etc/init.d/kaze-sync disable                      # removes /etc/rc.d/S99kaze-sync
rm /etc/init.d/kaze-sync
sed -i '/^\/etc\/init.d\/kaze-sync$/d;/^\/etc\/rc.d\/S99kaze-sync$/d' /etc/sysupgrade.conf
uci -q delete firewall.kaze_sync; uci commit firewall; /etc/init.d/firewall reload   # rule added by --wan; harmless if absent
rm -r /mnt/data/kaze-sync                          # the data directory you gave to --data; contains everything
```

On OpenWrt, deleting `/mnt/data/kaze-sync` simply removes that folder and frees space on your data disk; your disk, partition, and mount configuration remain untouched (if you want to repurpose the disk entirely, unmount and repartition it with your usual tools). Because OpenWrt has no `deluser` utility, the `kaze-sync` user (which has no login shell) can safely stay, or you can remove it manually with `sed -i '/^kaze-sync:/d' /etc/passwd /etc/group /etc/shadow`.

Finally, if you manually opened the server's TCP port in a firewall or cloud security group, close that rule.

## 17. Troubleshooting

When something does not work as expected, check the notification bell in the Web UI first (section 9), and then inspect the server or client logs.

**Where the logs are.**

| Component | Where |
|---|---|
| Server, systemd | `journalctl -u kaze-sync` |
| Server, OpenRC | syslog, tag `kaze-sync` (Alpine: `/var/log/messages`) |
| Server, OpenWrt | `logread` |
| Linux client | `~/.local/state/kaze-sync/kaze-syncc.log` (`$XDG_STATE_HOME/kaze-sync/` if set) |
| Windows client | `%LOCALAPPDATA%\kaze-sync\kaze-syncc.log` |

By default, the client log records only startup events, warnings, and errors. Whenever `kaze-syncc.log` grows past 10 MB (even while the agent is running), the client rotates it to `kaze-syncc.log.old`, keeping one previous log file.

**Debug timeline of the client.** If you need to investigate why a change is taking longer than expected to sync, create an empty file named `debug` inside the client's state directory (shown in the table above) and restart the agent. While `debug` exists, the client also writes `level=DEBUG` lines for local file events, folder scans, pulls, and every HTTP request to the server along with its duration. Delete the `debug` file and restart the agent once you finish troubleshooting.

**Common messages and problems.**

| Symptom | Cause and fix |
|---|---|
| `enroll` says `invalid or expired code` | Enrollment codes are single-use and expire after 15 minutes. Generate a new code in the Web UI and make sure you copy the entire string |
| `enroll` says `too many failures` (HTTP 429), or the sign-in page answers 429 | 10 failed attempts within an hour lock that client IP address for one hour. Either wait an hour, or run `admin-password` on the server, which restarts the service and clears all lockouts immediately |
| `enroll` says `cannot register device` | A device with that name already exists on the server. Revoke the old device first, or add the device under a different name |
| `enroll` fails with a certificate error | Either the `--fingerprint` does not match the server's certificate (copy the command fresh from the Web UI dialog, or check the fingerprint printed by `install` or the `listening` log line), or the server is using your own public CA certificate and `--fingerprint` must be omitted |
| Browser warns about the certificate, or does not fill in saved passwords | Expected when using the default self-signed certificate: compare the fingerprint and accept the warning once. If you want no browser warning and working password autofill, switch to a local CA (section 2.4). If you already use `--ca` and still see a warning, either the CA certificate is not yet trusted on this computer or the address in the browser's URL bar is not listed in `--san` |
| Forgot the admin password, port or fingerprint | Run `admin-password` for a new password (section 2.5), or re-run `install` / check the server log for the port and fingerprint (section 2.4) |
| The service exits right after starting and the log says `permission denied` on the certificate or key | The service user `kaze-sync` cannot read your custom `--cert`/`--key` files or traverse their parent directories (`install` warns about this when it runs); fix the file and directory permissions (section 2.4) |
| Server does not start after a reboot | The external data disk was not mounted at boot. The server refuses to start rather than creating an empty database; mount the data disk and start the service |
| `sync is stopped, the server does not accept this device` | The device was revoked in the Web UI. Enroll it again as a new device (section 3.3) |
| `client too old` | The server was upgraded to a protocol version that requires a newer client; upgrade the client binary (section 15) |
| A folder shows an error and the bell lists a `folder` entry | Either the local directory or `.syncroot` is missing (for example, an unmounted drive), or the directory is non-empty and you have not ticked **Allow adopting a non-empty directory** on the device page (section 5) |
| Changes appear after a few seconds, not instantly | This is normal: the client batches local file events for 2 seconds (up to 10 seconds when files keep changing continuously), waits until a modified file is at least 5 seconds old, uploads it, and notifies other devices via long polling. Total end-to-end delay is normally 5 to 10 seconds |
| The first sync takes long | On the initial scan, the client must read and hash every file once. As a reference, a test folder with 19,000 files (1.6 GiB) took about 4.5 minutes |
| A folder or directory stopped syncing on one device, and the bell shows "changes waiting for your decision" | The device wrote at the same time as another device, wrote on out-of-date content, or wrote to an exclusive directory it does not own, so it is frozen there until you decide. Open the decision dialog (section 8). If this keeps happening for unrelated files, set the folder's root to any writer (section 5.1) |
| The bell shows a held change, but I did not edit anything | A program on that device rewrote a file, which counts as a write. In an exclusive directory, even running `git status` on a non-owner device triggers a freeze because it rewrites `.git/index`. In a single-writer directory, it freezes the directory only if that device is out of date or another device wrote within the last time window; otherwise the write is accepted and makes that device the current writer for one time window. Run `git` only on the writer device, or switch the mode to any writer |
| A decision is stuck on "Waiting for ..." | The device carries out your decision on its next contact with the server, and is currently offline or has its agent stopped. After 10 minutes the label disappears and the rows look undecided again, but the decision stays queued and runs as soon as the device reconnects (pressing the button again simply replaces the queued decision). Only if the server restarts before the device reconnects is the queued decision lost, in which case you need to press the button again |
| A file does not arrive | Check the folder page in the Web UI for a grey **Not synced** row and check the notification bell: common causes are a symbolic link that cannot be created on that device (Windows without Developer Mode), a Windows-illegal filename, a case collision, a locked file (`in_use`), or an exclusion rule |
| The server data grows quickly | Files that change every few seconds create a new version on every write. Add an exclusion rule for them (section 7) or shorten the version retention period |

## 18. What kaze-sync does not do

To stay light enough for a 512 MB router while remaining simple to operate, kaze-sync deliberately leaves out features that are not needed for personal multi-device folder sync:
- No file sharing, multiple user accounts, or collaborative editing
- No file preview or file download in the Web UI
- No syncing of empty directories, Windows directory junctions, file permissions (other than the executable bit), or file owners and extended attributes
- No end-to-end encryption, transfer compression, or bandwidth rate limiting
- No per-device exclusion rules (if you want a subset of files only on one device, put them in a separate folder)
- No two-factor authentication for the Web UI
- No desktop tray icons or pop-up notifications on clients
- No macOS or mobile clients, and no server architectures other than x86-64
- No automatic self-updates
- Single files larger than 128 GiB are refused by the server (because the server holds the block list of a single file in memory)

For the engineering reasons behind each of these choices, see [`docs/design.md`](../design.md).

## Appendix A: Building from source

If your client runs on a CPU architecture that has no prebuilt package on [GitHub Releases](https://github.com/pellorao/kaze-sync/releases), or if you prefer to compile the binaries yourself, you can build them from source on any machine with Go installed (use the version specified in `go.mod`, currently 1.27.3). The resulting binaries are statically linked and have no external dependencies, so you can build them on your workstation and copy them to your server and clients:

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncd ./cmd/kaze-syncd
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o kaze-syncc ./cmd/kaze-syncc
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o kaze-syncc.exe ./cmd/kaze-syncc
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o kaze-syncc-agent.exe ./cmd/kaze-syncc
```

The fourth command compiles the exact same Windows client without a console window so that Windows can run it silently at sign-in. Always keep `kaze-syncc-agent.exe` in the same directory as `kaze-syncc.exe`. Adjust `GOARCH` when cross-compiling `kaze-syncc` for a non-x86-64 client; note that the server (`kaze-syncd`) is supported only on x86-64 (so non-x86-64 OpenWrt routers and other non-x86-64 server hosts are unsupported).

Both binaries embed the release version at build time: `kaze-syncd --version` and `kaze-syncc --version` print `kaze-syncd 0.3.5` and `kaze-syncc 0.3.5` (for the `0.3.5` release). In the Web UI, the server version appears next to `Kaze Sync` at the top-left after you sign in (the sign-in page omits it).

Once built, copy `kaze-syncd` to your server and follow [section 2.2](#22-openwrt) (OpenWrt) or [section 2.3](#23-plain-linux-systemd-or-openrc) (plain Linux) to install it, and copy the client binaries to a permanent directory on each computer to enroll them as described in [section 3](#3-installing-and-enrolling-clients).

# kaze-sync v0.2.0 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.2.0` (released on 2026-11-14) is a small follow-up release to `v0.1.0` of kaze-sync, a self-hosted file sync service with version history for Linux and Windows computers (`kaze-syncd` on OpenWrt or x86-64 Linux with systemd or OpenRC, and `kaze-syncc` clients on Linux and Windows configured centrally in the Web UI). It introduces Semantic Versioning across both binaries and the Web UI, checks custom TLS certificate permissions during `install`, and refines Web UI tips, time columns, and keyboard focus. For the full overview of kaze-sync's sync, write-control, and restore features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`.

Like `v0.1.0`, this is a source-only release with no prebuilt binary downloads: build the static binaries from source with Go on any machine as described in [section 2.1 of the user manual](docs/manuals/user-manual.md#21-build-the-binaries). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` (or in the top-left of the Web UI after signing in) and by the Git tag `v0.2.0`.

## Highlights

- **Semantic Versioning across binaries and Web UI.** kaze-sync now uses a single Semantic Versioning number (`0.2.0`), embedded at build time into both binaries. Running `kaze-syncd --version` and `kaze-syncc --version` prints `kaze-syncd 0.2.0` and `kaze-syncc 0.2.0`. After you sign in, the top-left of the Web UI reads `Kaze Sync` followed by the version in muted grey (5.3:1 contrast on the top bar), and the browser tab title reads `... - Kaze Sync`; the login page reads `Kaze Sync` but deliberately does not show the version so that someone who is not signed in learns nothing about the server version. Every release from now on raises this number (`PATCH` for fixes, `MINOR` for user-visible features and, before `1.0.0`, incompatible protocol or data changes).
- **Custom TLS certificate permission check during `install`.** When you configure custom `--cert` and `--key` files (including on repeated `install` runs that reuse stored paths), `install` now checks permission bits—including `kaze-sync`'s supplementary groups, without examining ACLs—to verify that the unprivileged service user `kaze-sync` can read both files and enter every parent directory above them. If `kaze-sync` cannot access a file or directory, or if a file is missing, `install` prints a `warning:` line naming the file or directory and the `chgrp`/`chmod` fix from [manual section 2.4](docs/manuals/user-manual.md#24-port-certificate-and-firewall) while still completing the installation. This resolves the first known limitation of `v0.1.0`.
- **Custom Web UI tips, cleaner time columns, and preserved focus.** Web UI hints—the time zone of a timestamp, the error of an exclusion-rule line, the time zone sample, and the explanation of the disabled **Add mapping** (`添加映射`) button when no folder is left to map—now open in the page's own tip rather than the browser's native tooltip. The tip opens on hover, focus, or click, closes on `Esc`, lets the pointer move onto it, and works inside dialogs and the notification popover. In tables, the time zone is now written once in the header of each time column (with long zone names wrapping in the header) instead of on every timestamp, timestamps are no longer keyboard `Tab` stops, and live page updates preserve keyboard focus.
- **Web UI layout and focus fixes.** On narrow screens, folder names in the overview keep a minimum width of about eight characters instead of shrinking to one letter per line; opening the write-control dialog places keyboard focus on the currently chosen option; and the greyed **Add mapping** (`添加映射`) button retains a visible focus ring.

## Upgrading

The sync protocol remains at version 6 and neither the server database schema nor the client state database schema has changed since `v0.1.0`. No data migration runs during this upgrade, and you can upgrade the server and clients in any order (for the full upgrade procedure, see [section 15 of the user manual](docs/manuals/user-manual.md#15-upgrading)):

- **Server (`kaze-syncd`)**: build the `v0.2.0` binary ([manual section 2.1](docs/manuals/user-manual.md#21-build-the-binaries)), copy it to the server, and run `./kaze-syncd install --data <same dir>` as root. Your listening port, TLS certificate, and admin password remain unchanged, and `install` copies the new binary into `<data>/` and restarts the service.
- **Clients (`kaze-syncc`)**: on each Linux or Windows computer, stop the running agent, replace the binary (`kaze-syncc` on Linux; both `kaze-syncc.exe` and the `-H=windowsgui` `kaze-syncc-agent.exe` on Windows), and start the agent again.

## Known limitations

- **Root-only server installation and CA renewal.** Server installation and local CA certificate renewal require root; non-root server deployment is not supported.
- **Source-only, no auto-update.** There are no prebuilt binary downloads and no automatic self-updates.
- **Scope boundaries ([manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do)).** There is no file sharing, multi-user account system, or collaborative editing; no file preview or file download in the Web UI; no syncing of empty directories, Windows directory junctions, file permissions other than the executable bit, file owners, or extended attributes; no end-to-end encryption, transfer compression, or bandwidth rate limiting; no per-device exclusion rules; no two-factor authentication for the Web UI; no desktop tray icons or pop-up notifications on clients; no macOS or mobile clients; no server architectures other than x86-64; and single files larger than 128 GiB are refused by the server.
- **Not a backup.** Historical versions live on the same disk as the server's data; back up the server data directory to another disk or machine ([manual section 14](docs/manuals/user-manual.md#14-backup-and-restore-of-the-server)).
- **Symbolic links on Windows without Developer Mode.** A Windows client without Developer Mode enabled cannot create synced symbolic links locally; the links stay on the server and appear as a single folded warning line per device in the Web UI.
- **High-frequency files.** Files rewritten every few seconds (such as live status or telemetry files) create a new historical version on every write until retention removes them; exclude such paths if that matters ([manual section 7](docs/manuals/user-manual.md#7-exclusion-rules) and [section 13](docs/manuals/user-manual.md#13-retention-and-garbage-collection)).
- **Server storage format.** Server data is stored as deduplicated blocks and SQLite databases rather than plain files; run `kaze-syncd export` as root when you need plain files directly from the server ([manual section 12](docs/manuals/user-manual.md#12-export-to-plain-files)).
- **Server rollback after database migration.** Rolling the server binary back to an older version after a database schema migration requires restoring the pre-upgrade database backup from `<data>/var/backups/` and running `install --new-epoch` ([manual section 15](docs/manuals/user-manual.md#15-upgrading)).

## Requirements and compatibility

| Component | Requirement |
|---|---|
| Build toolchain | Go `1.27.3` (as in `go.mod`), building static binaries with `CGO_ENABLED=0` ([manual section 2.1](docs/manuals/user-manual.md#21-build-the-binaries)) |
| Server (`kaze-syncd`) | x86-64 Linux with OpenWrt (`procd`, requiring a mounted data disk), systemd, or OpenRC; root access required for installation and maintenance; targets machines with 512 MB RAM |
| Linux client (`kaze-syncc`) | x86-64 Linux with a systemd user session |
| Windows client (`kaze-syncc.exe`, `kaze-syncc-agent.exe`) | x86-64 Windows; both `.exe` files placed in the same permanent directory |
| Web UI browsers | Chrome or Edge 114+, Firefox 125+, or Safari 17+ with JavaScript enabled |
| Protocol and database versions | Sync protocol version 6 (server requires client protocol 6); client state database schema version 3 |

## Where to read more

- [README](README.md): overview of what kaze-sync is and a five-step quick start.
- [Changelog](CHANGELOG.md): summary of changes in each release.
- [User manual](docs/manuals/user-manual.md): complete reference for installation, Web UI pages, folders and write control, exclusion rules, resolving held changes, notifications, restoring old versions, exporting plain files, backups, upgrades, uninstallation, and troubleshooting.
- [`docs/operations.md`](docs/operations.md) (Chinese): system-level deployment details, OpenWrt disk partitioning, systemd hardening, and backup and restore commands.

# kaze-sync v0.4.0 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.4.0` (released on 2026-11-18) replaces the ntfy error alerts with Web Push. For the full overview of kaze-sync's features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`. The prebuilt packages (`kaze-sync-0.4.0-linux-amd64.tar.gz`, `kaze-sync-0.4.0-windows-amd64.zip`, `SHA256SUMS`, x86-64 only) are on the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page; check them with `sha256sum -c SHA256SUMS` ([user manual, section 2.1](docs/manuals/user-manual.md#21-download-and-verify-the-server-package)). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` and by the Git tag `v0.4.0`.

## Highlights

- **Web Push, sent only by the server.** The server keeps a VAPID key and the browsers' subscriptions and sends each alert itself; clients no longer push anything. Every push repeats one entry of the notification bell (held changes after 2 minutes, device errors after 5 minutes, a too-old client, an offline device, low disk space, a failed garbage collection, a certificate that cannot be renewed). Each cause is pushed once per browser, repeats wait an hour, and pushes are capped at 20 per hour. The settings page has a **Push notifications** card to turn push on or off in a browser, list subscriptions, and send a test push. Push needs a trusted certificate (the local CA or your own certificate) and is not available on iPhone and iPad ([user manual, section 9.1](docs/manuals/user-manual.md)).
- **ntfy is removed**, on the server and on the clients; a leftover `ntfy.json` is no longer read.
- **Notification bell.** Four more problems are listed (a too-old client, low data-disk space, a failed garbage collection, a certificate that cannot be renewed), and a device that has not connected for 3 days (was 7) raises the offline warning.
- **Exclusion-rules preview.** Typing quickly in the filter box no longer drops characters.
- **Client fixes.** A folder whose sync cycles fail 3 times in a row is now reported to the server; the Windows client holds a single-instance lock, and stopping the agent tolerates an empty or unreadable pid file.

## Upgrading

The sync protocol remains at version 6, the minimum client version is unchanged, and the database schemas are unchanged, so no data migration runs. Upgrade the server first and then the clients ([user manual, section 15](docs/manuals/user-manual.md#15-upgrading)): run `./kaze-syncd install --data <same dir>` as root on the server; on each client replace the binaries (`kaze-syncc` on Linux; `kaze-syncc.exe` and `kaze-syncc-agent.exe` on Windows) and restart the agent. The server certificate does not change with this upgrade; to use Web Push on a phone, switch to the local CA first ([user manual, section 2.4](docs/manuals/user-manual.md)), which changes the server certificate and the fingerprint the clients pin.

## Known limitations

- **Root-only server installation and CA renewal.** Server installation and local CA certificate renewal require root; non-root server deployment is not supported.
- **Prebuilt packages for x86-64 only, no auto-update.** Other architectures build from source, and there are no automatic self-updates.
- **Scope boundaries ([manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do)).** There is no file sharing, multi-user account system, or collaborative editing; no file preview or file download in the Web UI; no syncing of empty directories, Windows directory junctions, file permissions other than the executable bit, file owners, or extended attributes; no end-to-end encryption, transfer compression, or bandwidth rate limiting; no per-device exclusion rules; no two-factor authentication for the Web UI; no desktop tray icons or pop-up notifications on clients; no macOS or mobile clients; no server architectures other than x86-64; and single files larger than 128 GiB are refused by the server.
- **Not a backup.** Historical versions live on the same disk as the server's data; back up the server data directory to another disk or machine ([manual section 14](docs/manuals/user-manual.md#14-backup-and-restore-of-the-server)).
- **Symbolic links on Windows without Developer Mode.** A Windows client without Developer Mode enabled cannot create synced symbolic links locally; the links stay on the server and appear as a single folded warning line per device in the Web UI.
- **High-frequency files.** Files rewritten every few seconds (such as live status or telemetry files) create a new historical version on every write until retention removes them; exclude such paths if that matters ([manual section 7](docs/manuals/user-manual.md#7-exclusion-rules) and [section 13](docs/manuals/user-manual.md#13-retention-and-garbage-collection)).
- **Server storage format.** Server data is stored as deduplicated blocks and SQLite databases rather than plain files; run `kaze-syncd export` as root when you need plain files directly from the server ([manual section 12](docs/manuals/user-manual.md#12-export-to-plain-files)).
- **Server rollback after database migration.** Rolling the server binary back to an older version after a database schema migration requires restoring the pre-upgrade database backup from `<data>/var/backups/` and running `install --new-epoch` ([manual section 15](docs/manuals/user-manual.md#15-upgrading)).

# kaze-sync v0.4.1 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.4.1` (released on 2026-11-18) is a patch release for the Web Push of `v0.4.0`. For the full overview of kaze-sync's features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`. The prebuilt packages (`kaze-sync-0.4.1-linux-amd64.tar.gz`, `kaze-sync-0.4.1-windows-amd64.zip`, `SHA256SUMS`, x86-64 only) are on the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page; check them with `sha256sum -c SHA256SUMS` ([user manual, section 2.1](docs/manuals/user-manual.md#21-download-and-verify-the-server-package)). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` and by the Git tag `v0.4.1`.

## Highlights

- **Every push alerts.** A notification with the same tag as one still shown used to replace it silently, so on desktop browsers only the first test push alerted. Tagged notifications now set `renotify`, and each test push uses a unique tag.
- **Clearer subscribe failure.** When the browser cannot reach its push service while turning push on (`AbortError`, for example Android Chrome without Google Play services), the Push notifications card says so.

## Upgrading

The sync protocol remains at version 6, the minimum client version is unchanged, and the database schemas are unchanged, so no data migration runs. The changes are in the server's web assets only; clients need no upgrade for them, but upgrade them anyway to keep versions aligned. Upgrade the server first and then the clients ([user manual, section 15](docs/manuals/user-manual.md#15-upgrading)): run `./kaze-syncd install --data <same dir>` as root on the server; on each client replace the binaries and restart the agent. After the server upgrade, a browser picks up the new service worker on its next update check.

## Known limitations

The known limitations are unchanged from [v0.4.0](CHANGELOG.md): root-only server installation, x86-64 prebuilt packages only, no auto-update, the scope boundaries in [manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do), not a backup, Windows symbolic links need Developer Mode, high-frequency files create many versions, and a server rollback after a database migration needs the pre-upgrade backup.

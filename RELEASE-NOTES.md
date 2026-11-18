# kaze-sync v0.5.0 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.5.0` (released on 2026-11-18) adds device renaming and bulk selection in the held-changes dialog. For the full overview of kaze-sync's features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`. The prebuilt packages (`kaze-sync-0.5.0-linux-amd64.tar.gz`, `kaze-sync-0.5.0-windows-amd64.zip`, `SHA256SUMS`, x86-64 only) are on the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page; check them with `sha256sum -c SHA256SUMS` ([user manual, section 2.1](docs/manuals/user-manual.md#21-download-and-verify-the-server-package)). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` and by the Git tag `v0.5.0`.

## Highlights

- **Rename a device.** The device page's **⋯** menu has **Rename device…**. The device keeps its token and mappings and is not enrolled again; current files, held changes and notifications show the new name, while older versions keep the name they were written under. Clients take the new name from their configuration and record it in `device.json`.
- **Bulk actions for held changes.** Each row of the held-changes dialog has a checkbox (the header box selects all), and **Set the checked rows to** applies one action to them. A group with more than 2000 changes is folded by directory.
- **Readability.** Small muted text now reaches 4.5:1 contrast, and table headers carry `scope`.

## Upgrading

The sync protocol remains at version 6 (the configuration gains an optional device name that older clients ignore), the minimum client version is unchanged, and the database schemas are unchanged, so no data migration runs. Upgrade the server first and then the clients ([user manual, section 15](docs/manuals/user-manual.md#15-upgrading)): run `./kaze-syncd install --data <same dir>` as root on the server; on each client replace the binaries and restart the agent. A client older than this release keeps using its enrolled name after a rename until it is upgraded.

## Known limitations

The known limitations are unchanged from [v0.4.0](CHANGELOG.md): root-only server installation, x86-64 prebuilt packages only, no auto-update, the scope boundaries in [manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do), not a backup, Windows symbolic links need Developer Mode, high-frequency files create many versions, and a server rollback after a database migration needs the pre-upgrade backup.

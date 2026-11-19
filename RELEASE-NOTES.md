# kaze-sync v0.5.2 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.5.2` (released on 2026-11-19) fixes four defects found in an architecture review: Web Push counted some sends twice against the hourly quota, the held-changes page could hang on 2001 changes of one path, quick successive device renames could leave an intermediate name, and Web Push still sent after a mapping was removed. For the full overview of kaze-sync's features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`. The prebuilt packages (`kaze-sync-0.5.2-linux-amd64.tar.gz`, `kaze-sync-0.5.2-windows-amd64.zip`, `SHA256SUMS`, x86-64 only) are on the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page; check them with `sha256sum -c SHA256SUMS` ([user manual, section 2.1](docs/manuals/user-manual.md#21-download-and-verify-the-server-package)). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` and by the Git tag `v0.5.2`.

## Highlights

- **Four fixes.** Push quota is reserved once per send; large same-path held groups render; renames are serialized; unmapped folders no longer push. Details in the [changelog](CHANGELOG.md).

## Upgrading

The protocol, the minimum client version and the database schemas are unchanged, and only the server changes: run `./kaze-syncd install --data <same dir>` as root on the server ([user manual, section 15](docs/manuals/user-manual.md#15-upgrading)); clients need no upgrade.

## Known limitations

The known limitations are unchanged from [v0.4.0](CHANGELOG.md): root-only server installation, x86-64 prebuilt packages only, no auto-update, the scope boundaries in [manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do), not a backup, Windows symbolic links need Developer Mode, high-frequency files create many versions, and a server rollback after a database migration needs the pre-upgrade backup.

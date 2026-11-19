# kaze-sync v0.5.3 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.5.3` (released on 2026-11-19) makes a held deletion clear by itself once the server has deleted the path too, including deletions the server made before this check existed, and fixes the history of a scope that was waiting for a decision when the server was restored. For the full overview of kaze-sync's features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`. The prebuilt packages (`kaze-sync-0.5.3-linux-amd64.tar.gz`, `kaze-sync-0.5.3-windows-amd64.zip`, `SHA256SUMS`, x86-64 only) are on the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page; check them with `sha256sum -c SHA256SUMS` ([user manual, section 2.1](docs/manuals/user-manual.md#21-download-and-verify-the-server-package)). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` and by the Git tag `v0.5.3`.

## Highlights

- **Held deletions settle themselves.** The client asks the server about its held deletions; once the server has deleted the path too, only the record goes and a file put back meanwhile stays. Details in the [changelog](CHANGELOG.md).

## Upgrading

The protocol (an optional field was added within the current version, so older servers and clients still work), the minimum client version and the database schemas are unchanged. Upgrade the server first (`./kaze-syncd install --data <same dir>` as root, [user manual, section 15](docs/manuals/user-manual.md#15-upgrading)), then the clients.

## Known limitations

The known limitations are unchanged from [v0.4.0](CHANGELOG.md): root-only server installation, x86-64 prebuilt packages only, no auto-update, the scope boundaries in [manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do), not a backup, Windows symbolic links need Developer Mode, high-frequency files create many versions, and a server rollback after a database migration needs the pre-upgrade backup.

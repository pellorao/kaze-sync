# kaze-sync v0.7.1 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.7.1` (released on 2026-11-23) fixes the header bar of the Web UI, which showed a gap on its right when the page had no scrollbar. For the full overview of kaze-sync's features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`. The prebuilt packages and installers (`kaze-sync-0.7.1-linux-amd64.tar.gz`, `kaze-sync-0.7.1-windows-amd64.zip`, `kaze-sync-server-0.7.1-linux-amd64.sh`, `kaze-sync-client-0.7.1-linux-amd64.sh`, `kaze-sync-client-0.7.1-windows-amd64-setup.exe`, `SHA256SUMS`, x86-64 only) are on the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page; check them with `sha256sum -c SHA256SUMS` ([user manual, section 2.1](docs/manuals/user-manual.md#21-download-and-verify-the-server-package)). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` and by the Git tag `v0.7.1`. The Windows installer is not code-signed: SmartScreen shows a warning, see the user manual.

## Highlights

- Fix: the header bar spans the full page width when the page does not scroll. Server-side Web UI change only; details in the [changelog](CHANGELOG.md).

## Upgrading

The protocol (new optional headers only, so older servers and clients still work), the minimum client version and the database schemas are unchanged. Upgrade the server first (run the new server installer as root, or `./kaze-syncd install` with the new binary, [user manual, section 15](docs/manuals/user-manual.md#15-upgrading)), then the clients.

## Known limitations

The known limitations are unchanged from [v0.4.0](CHANGELOG.md): root-only server installation, x86-64 prebuilt packages only, no auto-update (an upgrade runs only when you start it), the scope boundaries in [manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do), not a backup, Windows symbolic links need Developer Mode, high-frequency files create many versions, and a server rollback after a database migration needs the pre-upgrade backup.

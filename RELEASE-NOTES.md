# kaze-sync v0.6.0 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.6.0` (released on 2026-11-22) adds installers: one file to download and run installs or upgrades the server, and the same for the Linux and Windows clients, keeping ports, certificates, passwords, device registrations and mappings. For the full overview of kaze-sync's features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`. The prebuilt packages and installers (`kaze-sync-0.6.0-linux-amd64.tar.gz`, `kaze-sync-0.6.0-windows-amd64.zip`, `kaze-sync-server-0.6.0-linux-amd64.sh`, `kaze-sync-client-0.6.0-linux-amd64.sh`, `kaze-sync-client-0.6.0-windows-amd64-setup.exe`, `SHA256SUMS`, x86-64 only) are on the [GitHub Releases](https://github.com/pellorao/kaze-sync/releases) page; check them with `sha256sum -c SHA256SUMS` ([user manual, section 2.1](docs/manuals/user-manual.md#21-download-and-verify-the-server-package)). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` and by the Git tag `v0.6.0`. The Windows installer is not code-signed: SmartScreen shows a warning, see the user manual.

## Highlights

- **Installers.** A self-extracting server installer (it also carries the client installers), a Linux client installer and a Windows `setup.exe`; the Add device dialog offers download links and a one-line `curl` command for a Linux device without a desktop. Details in the [changelog](CHANGELOG.md).
- **`kaze-syncc upgrade`.** A client fetches the installer from its own server and upgrades itself.
- Fix: a frozen scope no longer retries "link not created" rows.

## Upgrading

The protocol (new optional headers only, so older servers and clients still work), the minimum client version and the database schemas are unchanged. Upgrade the server first (run the new server installer as root, or `./kaze-syncd install` with the new binary, [user manual, section 15](docs/manuals/user-manual.md#15-upgrading)), then the clients.

## Known limitations

The known limitations are unchanged from [v0.4.0](CHANGELOG.md): root-only server installation, x86-64 prebuilt packages only, no auto-update (an upgrade runs only when you start it), the scope boundaries in [manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do), not a backup, Windows symbolic links need Developer Mode, high-frequency files create many versions, and a server rollback after a database migration needs the pre-upgrade backup.

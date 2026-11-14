# Changelog

[English](CHANGELOG.md) | [简体中文](CHANGELOG.zh-CN.md)

Notable changes to kaze-sync are grouped by release below; versions follow `vMAJOR.MINOR.PATCH`.

## v0.1.0 - 2026-11-14

First public release, covering the development from 2026-11-02 to 2026-11-14. For full details, see the [v0.1.0 release notes](RELEASE-NOTES.md).

### Added

- **Sync and storage**: Two-way sync of any number of folders per device using 256 KiB deduplicated blocks, batched block downloads, parallel hashing, and staged block lists supporting single files over 15 GiB up to 128 GiB; one SQLite database per folder on the server; symbolic link sync; slow-retry handling for locked files on Windows (`in_use`) and live SQLite databases; global and per-folder `.gitignore` exclusion rules with a preview of matching server files before saving and factory exclusions including Nextcloud system files.
- **Write control (protocol 6)**: Per-directory and per-file write modes—Any writer (`free`), Single writer (`single`), and Exclusive (`exclusive`)—with a configurable single-writer time window (60 seconds by default) and a new-folder prompt; colliding or out-of-date writes freeze the affected scope on the later device and hold changes as pending items for per-item or per-group decision in the Web UI (with discarded local copies saved under `.syncsaved/`). A file inside a `free` directory inherits `single`, and the server refuses `free` on a single file.
- **Version history, restore, and export**: Server-side retention of overwritten and deleted files (30 days by default) with bounded-memory daily garbage collection; single-file, deleted-file, and point-in-time directory restore dialog with a day axis and a time-of-day axis; `kaze-syncd export` for writing out current or point-in-time files as ordinary files.
- **Web UI and notifications**: Dark-theme Web UI in five languages (Simplified Chinese, Traditional Chinese, English, Japanese, and Korean) with live updates, write-control icons and tooltips, menus and tooltips that stay within the viewport, tables that scroll inside their own box, and folder page headers that count the current directory; optional rate-limited [ntfy](https://ntfy.sh) push messages (`ntfy.json`) on server and clients.
- **Platforms, installation, and security**: Root-installed `kaze-syncd` (x86-64) running as unprivileged user `kaze-sync` on OpenWrt (`procd`) and plain Linux (`systemd` with unit hardening, or `OpenRC`); Linux and Windows `kaze-syncc` clients (`kaze-syncc.exe` and windowless `kaze-syncc-agent.exe`); self-signed, custom (`--cert`/`--key`), and local CA (`--ca` / `kaze-syncd ca`) TLS certificate modes; automatic server database migration with pre-upgrade backups in `<data>/var/backups/`, and client state database schema version 3 (refusing newer schemas).
- **Documentation and testing**: English and Simplified Chinese [README](README.md) and [user manual](docs/manuals/user-manual.md) (reviewed against the code, including [section 2.6](docs/manuals/user-manual.md#26-ports-and-firewall-rules) on ports and firewall rules and [section 2.4](docs/manuals/user-manual.md#24-port-certificate-and-firewall) on importing the local CA on Windows, macOS, Linux, Android, iPhone, and iPad); unit, simulation, and end-to-end tests including an 8-scenario write-control driver that also checks push-message latency.

### Known limitations

- `install` does not check that the service user `kaze-sync` can read custom `--cert` and `--key` files; grant `kaze-sync` read access to both files and their parent directories before starting the service ([manual section 2.4](docs/manuals/user-manual.md#24-port-certificate-and-firewall)).
- Server installation and CA renewal require root; source-only build (no prebuilt binaries, version string, or auto-update); x86-64 server and Linux/Windows clients only; Windows without Developer Mode skips creating synced symbolic links with one folded warning; server data is not a backup and uses block storage (`kaze-syncd export` writes plain files). See the [v0.1.0 release notes](RELEASE-NOTES.md#known-limitations) and [manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do) for the full list.

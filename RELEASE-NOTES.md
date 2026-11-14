# kaze-sync v0.3.4 release notes

[English](RELEASE-NOTES.md) | [简体中文](RELEASE-NOTES.zh-CN.md)

## What this release is

`v0.3.4` (released on 2026-11-15) is a Web UI patch release of kaze-sync, a self-hosted file sync service with version history for Linux and Windows computers. `kaze-syncd` now sets page, card and dialog titles in a serif face, as the status page does, and the notification count badge no longer overlaps the settings button next to it. For the full overview of kaze-sync's sync, write-control, and restore features, see the [user manual](docs/manuals/user-manual.md), the [changelog](CHANGELOG.md), or the `v0.1.0` release notes in the Git tag `v0.1.0`.

Like earlier releases, this is a source-only release with no prebuilt binary downloads: build the static binaries from source with Go on any machine as described in [section 2.1 of the user manual](docs/manuals/user-manual.md#21-build-the-binaries). Identify a build with `kaze-syncd --version` and `kaze-syncc --version` (or in the top-left of the Web UI after signing in) and by the Git tag `v0.3.4`.

## Highlights

- **Serif titles.** Page titles, card titles, dialog titles and the title line of the folder, device and settings pages are set in Source Serif 4 (Latin) and Source Han Serif (Han characters, kana, hangul; the regional version of the interface language), in one weight. Body text stays sans-serif and paths, rules and codes stay monospaced. The serif faces are renamed `Kaze Serif` and `Kaze Han Serif` in the font files (the SIL Open Font License reserves the name `Source`); the same renaming now also applies to the Source Han Sans faces, which carry the same reserved name.
- **Cost.** The server binary grows by about 3.1 MB. The serif files add 0.05 MB to the first page in English and 0.4 to 1.3 MB in Korean, Japanese, Simplified and Traditional Chinese (cached for a week afterwards); the bold sans Han file, which every title used to pull in, is now downloaded only when other bold text is on the page.
- **Count badge.** The count on the bell in the top bar (1 digit, 2 digits, `99+`; also on a narrow screen) keeps clear of the settings button; before, the ring of the badge reached 4 px into it.
- Tests: `internal/server/fonts_test.go` (serif files, per-language stacks, coverage of every interface character, renamed faces).

## Upgrading

The sync protocol remains at version 6, and database schemas and client behavior are unchanged. No data migration runs during this upgrade; because clients have no Web UI, only the server binary shows the change and clients need no upgrade (though you can still upgrade them in any order; for the full upgrade procedure, see [section 15 of the user manual](docs/manuals/user-manual.md#15-upgrading)):

- **Server (`kaze-syncd`)**: build the `v0.3.4` binary ([manual section 2.1](docs/manuals/user-manual.md#21-build-the-binaries)), copy it to the server, and run `./kaze-syncd install --data <same dir>` as root. Your listening port, TLS certificate, and admin password remain unchanged, and `install` copies the new binary into `<data>/` and restarts the service.
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

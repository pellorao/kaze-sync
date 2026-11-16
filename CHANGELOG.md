# Changelog

[English](CHANGELOG.md) | [简体中文](CHANGELOG.zh-CN.md)

Notable changes to kaze-sync are grouped by release below; versions follow `vMAJOR.MINOR.PATCH`.

## Unreleased

### Fixed

- The device list and device details show the client release version (`x.y.z`) reported by poll requests instead of the integer protocol version. Older clients that do not report a release version show `-`. The protocol version remains 6.

## v0.3.4 - 2026-11-15

Sets titles in a serif face like the status page and keeps the notification count badge off the settings button. For full details, see the [v0.3.4 release notes](RELEASE-NOTES.md).

### Changed

- Page titles, card titles, dialog titles and the title line of the folder, device and settings pages in the Web UI are set in a serif face (Source Serif 4 for Latin text, Source Han Serif of the interface language's region for Han characters, kana and hangul; one weight, 600), as on the status page; body text stays sans-serif. The server binary grows by about 3.1 MB and the serif files add 0.05 to 1.3 MB to the first page of an interface (see `docs/design.md` 4.7).
- The serif faces are shipped as `Kaze Serif` and `Kaze Han Serif` (SIL Open Font License, reserved font name `Source`). Source Han Sans carries the same reserved name, so its faces are now renamed `Kaze Han Sans` in the font files too. `tools/fontsubset/subset.py` does it (`rename_family`, `rename_han`).

### Fixed

- The count badge on the bell in the top bar reached 4 px into the settings button next to it (the badge sticks out 12 px past the button with its ring, the gap is 8 px). The bell now keeps 8 px more room, so the badge stays 4 px clear with 1 digit, 2 digits and `99+`, at any width.
- Tests: `internal/server/fonts_test.go`, `internal/server/badge_test.go`.

## v0.3.3 - 2026-11-15

Fixes the Web UI showing English sentences in the Japanese, Korean and Chinese interfaces when a count is 1, and a folder name that contains a literal placeholder being rewritten in dialogs. For full details, see the [v0.3.3 release notes](RELEASE-NOTES.md).

### Fixed

- In the Japanese, Korean and Chinese interfaces, texts that have a separate wording for the count 1 (for example the ending of the delete-mapping confirmation when the row holds exactly 1 exclusive lock) were shown in English, because the server merged the English-only singular variants into every language's dictionary. A singular variant is now served only from the language that wrote the text.
- A dialog text is now filled in one pass, so a folder name that contains a literal `{path}` (or another placeholder name) is shown as typed instead of being replaced.
- Tests: `internal/server/i18n1_test.go`, `internal/server/jstest/rt.test.js`.

## v0.3.2 - 2026-11-15

Names the folder and local path when confirming mapping deletion, renames the embedded Latin font subset to `Kaze Sans` to comply with the SIL Open Font License, fixes device-page mapping table wrapping, undersized click targets, and narrow-screen summary-line wrapping in the Web UI, and adds corresponding tests. For full details, see the [v0.3.2 release notes](RELEASE-NOTES.md).

### Changed

- Choosing `"Delete mapping..."` from a row menu on the device page previously asked for confirmation without stating which mapping would be deleted; it now asks `"Delete the mapping of folder <name> on <path>? ..."`, naming the folder and the local path just as the revoke-device confirmation names the device.
- Because the embedded Latin text face is a subset made by us—making it a modified version—and clause 3 of the SIL Open Font License forbids a modified version from using the reserved font name `"Source"`, the face is renamed from `Source Sans 3` to `Kaze Sans` in both the font file and the CSS. `tools/fontsubset/subset.py` performs the renaming (`rename_family`) while keeping the copyright and license records in the font file, and the look and size (about 93 KB) do not change. Sync protocol, database schemas, and `kaze-syncc` client behavior are unchanged; only the server binary shows these changes.

### Fixed

- On a narrow screen (640 px and narrower), the mapping table on the device page now puts the local path on a second line under the folder name instead of in a column of its own, so the row's `"..."` menu button stays on screen and the table no longer scrolls sideways to hide it. On wider screens, a long local path now wraps inside its column, so the table does not scroll sideways at 650 to 800 px either.
- Several Web UI controls were 18 to 22 px high or under 24 px wide—the device name links in the overview's device table, the links in the breadcrumb, short folder or file names in lists (such as `.git`), the links and group heads in the notifications list, and the `"N devices"` hint button on the folder page—and now have click targets of at least 24 px (WCAG 2.2 target size).
- When the summary line under a page title (for example `"linux · Client version 0.3 · Configuration ..."` on the device page) wraps on a narrow screen, it no longer shows a lone `"·"` at the start of a line. Tests were added for all of the above (`internal/server/ui1114p1_test.go`, `jstest/delmap.test.js`), and screenshots of the pages and dialogs were checked in five languages (English, Simplified Chinese, Traditional Chinese, Japanese, and Korean) at 400, 800, and 1280 px width, showing no overflow, cut text, or sideways page scroll.

## v0.3.1 - 2026-11-15

Raises the Web UI type scale and matching control, row, and dialog sizes so pages read comfortably without browser zoom, fixes narrow-screen wrapping and table scrolling, and adds a stylesheet size test. For full details, see the [v0.3.1 release notes](RELEASE-NOTES.md).

### Changed

- Because Web UI text was small, `style.css` now follows the Primer type scale with four variables (`--fs-xs` 12 px, `--fs-sm` 14 px, `--fs-md` 16 px, `--fs-lg` 20 px). Body text moves from 14 to 16 px with line height 1.6 (was 1.5), text fields move from 14 to 16 px, table text is 16 px (with times and sizes at 14 px), card and dialog titles as well as the choices and `Device` label in the write-control dialog are 16 px, and the page title stays 20 px. Table headers, tags, counters, help lines, notes, tips, and notification details move from 12 to 14 px, buttons move from 13 to 14 px, and monospace text for paths, rules, and fingerprints is 14 px to match the optical size of 16 px `Source Sans 3`; 12 px is kept only for the version number beside the name, the notification badge, menu group captions, and the end labels of the restore time axis. The fonts themselves did not change, server binary size is unchanged in practice, and `kaze-syncc` is unchanged in behavior.
- Surrounding elements grew on the 2 px spacing grid to match: table rows are 44 px (file list rows 40 px), buttons 32 px high (small 28 px), text fields 40 px, menu items 36 px, tags 24 px, the notification badge 20 px, switches grew to match, two-line status cells on the device page got 4 px vertical padding, table cell padding drops from 12 to 8 px on screens up to 640 px, and stand-alone controls (counters, the show-deleted switch, notification links, expanders, and checkboxes) keep at least a 24 px target. Dialogs are 640 px wide (wide 860 px, restore 1000 px; were 560, 760, and 900 px), the notification list 480 px (was 420 px), the login box 400 px (was 360 px), tips up to 360 px (was 320 px), and the context menu 224 px (was 200 px); the SVG restore time axes and the restore preview height keep their size. `internal/server/fontsize_test.go` fails if a font size in `style.css` is not one of the scale variables or if row, control, and dialog sizes fall below the new minimums.

### Fixed

- Where a plain enlargement looked wrong or clipped on narrow screens, layouts were adjusted: the version history dialog keeps each time on one line and scrolls its table sideways inside the dialog on a narrow screen (previously the `Restore` button was cut off and the dialog scrolled as a whole), and the held-changes detail table also scrolls in its own box; card headers wrap so a long note drops under the title on a narrow screen instead of squeezing Settings page headers into two lines; the time zone name in the file list's `Modified` column header no longer breaks in the middle of a word; and size cells (`"16 B"`) do not wrap. Checked with headless Chrome in English, Simplified Chinese, Traditional Chinese, Japanese, and Korean at 400, 800, and 1280 px wide on the overview, folder, device, and settings pages, the dialogs (new folder, add device, confirm, restore, write control, exclusion rules, version history, held changes, and add and edit mapping), the notification list, and the context menu, pages show no horizontal page scroll, overflow, or truncation (one minor pre-existing quirk remains on narrow screens: when the summary line under a page title wraps, a lone `"·"` separator can stand at the start of a line).

## v0.3.0 - 2026-11-15

Ships self-hosted Web UI fonts (Source Sans 3, per-language Source Han Sans subsets, and JetBrains Mono) so text renders consistently without missing CJK fonts or external requests, renders each option in the Settings language list with a tiny per-language font file, and fixes a narrow-screen overflow on the Settings page. For full details, see the [v0.3.0 release notes](RELEASE-NOTES.md).

### Changed

- The Web UI previously used the system font stack, so text looked poor on machines without suitable CJK fonts and Korean had no font at all on some machines. `kaze-syncd` now ships its own fonts from the Adobe Source family alongside `JetBrains Mono`: `Source Sans 3` for Latin, Greek, and Cyrillic text, `Source Han Sans` in the regional version matching the page language (Simplified Chinese, Traditional Chinese, Japanese, and Korean) for Han, kana, and hangul, and `JetBrains Mono` for paths, rules, commands, and fingerprints. All fonts are under the SIL Open Font License 1.1 (licenses and sources in `internal/server/web/fonts/`, `SOURCE.md`), and the page requests nothing from other sites. Embedding the fonts adds about 4.8 MB to the server binary (from 22.8 MB to 27.7 MB for x86-64 `kaze-syncd`); the client binary has no Web UI and does not change in behavior.
- To keep traffic low, the Latin fonts are embedded in full (`Source Sans 3` about 94 KB; `JetBrains Mono` about 77 KB for two weights), while the Han, kana, and hangul fonts are subsetted per language to the common characters of each region (GB 2312 level 1, Big5 common, JIS X 0208 level 1 kanji plus all kana, and the 2350 common KS X 1001 hangul syllables) plus every character of the interface texts; each regular and bold file is about 630 KB (Simplified Chinese), 955 KB (Traditional Chinese), 513 KB (Japanese), and 195 KB (Korean), with bold a little larger. A browser downloads only the files of the page language and only when the page has characters in that range (English pages embed no Han font), so first opening a page costs about 94 KB in English, 0.5 MB in Korean, 1.1 MB in Japanese, 1.4 MB in Simplified Chinese, or 2.0 MB in Traditional Chinese in total, and caches font files for one week; characters outside the subset (such as a rare Han character in a file name) fall back to a `Source Han Sans` or `Noto Sans CJK` installed on the machine and then to a system font. `tools/fontsubset/subset.py` (requiring `fonttools` and `brotli`) rebuilds the subsets, `tools/fontsubset/coverage/` lists the characters each subset holds, and a test fails if new interface text uses a character outside the subsets.
- Language names in the Settings language list are each shown in their own language's font from four tiny font files (about 2 KB each), so opening Settings does not download the large regional subsets.

### Fixed

- At 400 px width, the time zone list on the Settings page overflowed its card by about 19 px; it now shrinks to the card.

## v0.2.0 - 2026-11-14

Adds Semantic Versioning across both binaries and the Web UI, checks custom TLS certificate permissions during `install`, and refines Web UI tips, time columns, and keyboard focus. For full details, see the [v0.2.0 release notes](RELEASE-NOTES.md).

### Added

- kaze-sync now has a single Semantic Versioning number (`0.2.0`), embedded at build time into both binaries. `kaze-syncd --version` and `kaze-syncc --version` print `kaze-syncd 0.2.0` and `kaze-syncc 0.2.0`. After sign-in, the top-left of the Web UI reads `Kaze Sync` followed by the version in muted grey (5.3:1 contrast on the top bar), and the browser tab title reads `... - Kaze Sync`; the login page reads `Kaze Sync` and deliberately omits the version so that someone who is not signed in learns nothing about the server version. Every release from now on raises this number (`PATCH` for fixes, `MINOR` for user-visible features and, before `1.0.0`, incompatible protocol or data changes).

### Changed

- `install` now checks that the service user `kaze-sync` can read the custom `--cert` and `--key` files and enter every directory above them (permission bits, including the user's supplementary groups; ACLs are not examined). If not, or if a file is missing, it prints a `warning:` line that names the file or directory and the `chgrp`/`chmod` fix from [manual section 2.4](docs/manuals/user-manual.md#24-port-certificate-and-firewall), and still completes the installation. A repeated `install` checks the stored files too. This removes the first known limitation of v0.1.0 below.
- Web UI hints—the time zone of a timestamp, the error of an exclusion-rule line, the time zone sample, and the explanation of the disabled **Add mapping** (`添加映射`) button (shown when no folder is left to map)—now appear in the page's own tip (opens on hover, focus, or click; `Esc` closes it; the pointer can move onto it; works inside dialogs and the notification popover) instead of the browser's native tooltip. The time zone is written once in the header of each time column (with long zone names wrapping in the header) instead of per timestamp, timestamps are not keyboard `Tab` stops, and live updates keep keyboard focus.
- On narrow screens, folder names in the Web UI overview keep a minimum width of about eight characters instead of shrinking to one letter per line; opening the write-control dialog puts keyboard focus on the currently chosen option; and the greyed **Add mapping** (`添加映射`) button keeps a visible focus ring.

## v0.1.0 - 2026-11-14

First public release, covering the development from 2026-11-02 to 2026-11-14. For full details, see the v0.1.0 release notes (kept in the Git tag `v0.1.0`).

### Added

- **Sync and storage**: Two-way sync of any number of folders per device using 256 KiB deduplicated blocks, batched block downloads, parallel hashing, and staged block lists supporting single files over 15 GiB up to 128 GiB; one SQLite database per folder on the server; symbolic link sync; slow-retry handling for locked files on Windows (`in_use`) and live SQLite databases; global and per-folder `.gitignore` exclusion rules with a preview of matching server files before saving and factory exclusions including Nextcloud system files.
- **Write control (protocol 6)**: Per-directory and per-file write modes—Any writer (`free`), Single writer (`single`), and Exclusive (`exclusive`)—with a configurable single-writer time window (60 seconds by default) and a new-folder prompt; colliding or out-of-date writes freeze the affected scope on the later device and hold changes as pending items for per-item or per-group decision in the Web UI (with discarded local copies saved under `.syncsaved/`). A file inside a `free` directory inherits `single`, and the server refuses `free` on a single file.
- **Version history, restore, and export**: Server-side retention of overwritten and deleted files (30 days by default) with bounded-memory daily garbage collection; single-file, deleted-file, and point-in-time directory restore dialog with a day axis and a time-of-day axis; `kaze-syncd export` for writing out current or point-in-time files as ordinary files.
- **Web UI and notifications**: Dark-theme Web UI in five languages (Simplified Chinese, Traditional Chinese, English, Japanese, and Korean) with live updates, write-control icons and tooltips, menus and tooltips that stay within the viewport, tables that scroll inside their own box, and folder page headers that count the current directory; optional rate-limited [ntfy](https://ntfy.sh) push messages (`ntfy.json`) on server and clients.
- **Platforms, installation, and security**: Root-installed `kaze-syncd` (x86-64) running as unprivileged user `kaze-sync` on OpenWrt (`procd`) and plain Linux (`systemd` with unit hardening, or `OpenRC`); Linux and Windows `kaze-syncc` clients (`kaze-syncc.exe` and windowless `kaze-syncc-agent.exe`); self-signed, custom (`--cert`/`--key`), and local CA (`--ca` / `kaze-syncd ca`) TLS certificate modes; automatic server database migration with pre-upgrade backups in `<data>/var/backups/`, and client state database schema version 3 (refusing newer schemas).
- **Documentation and testing**: English and Simplified Chinese [README](README.md) and [user manual](docs/manuals/user-manual.md) (reviewed against the code, including [section 2.6](docs/manuals/user-manual.md#26-ports-and-firewall-rules) on ports and firewall rules and [section 2.4](docs/manuals/user-manual.md#24-port-certificate-and-firewall) on importing the local CA on Windows, macOS, Linux, Android, iPhone, and iPad); unit, simulation, and end-to-end tests including an 8-scenario write-control driver that also checks push-message latency.

### Known limitations

- `install` does not check that the service user `kaze-sync` can read custom `--cert` and `--key` files; grant `kaze-sync` read access to both files and their parent directories before starting the service (fixed in v0.2.0) ([manual section 2.4](docs/manuals/user-manual.md#24-port-certificate-and-firewall)).
- Server installation and CA renewal require root; source-only build (no prebuilt binaries, version string (fixed in v0.2.0), or auto-update); x86-64 server and Linux/Windows clients only; Windows without Developer Mode skips creating synced symbolic links with one folded warning; server data is not a backup and uses block storage (`kaze-syncd export` writes plain files). See the v0.1.0 release notes (kept in the Git tag `v0.1.0`) and [manual section 18](docs/manuals/user-manual.md#18-what-kaze-sync-does-not-do) for the full list.

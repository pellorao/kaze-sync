# Changelog

[English](CHANGELOG.md) | [简体中文](CHANGELOG.zh-CN.md)

Notable changes to kaze-sync are grouped by release below; versions follow `vMAJOR.MINOR.PATCH`.

## v0.7.3 - 2026-11-25

Patch release: Web UI fixes.

### Fixed

- The rule editor keeps keyboard focus on its Preview button while a preview is loading, instead of dropping it.
- Leaving with unsaved edits asks first: the Settings link inside the rule editor, and the header and breadcrumb links on the Settings page (global rules), ask before discarding the edits. The question for these links now reads "Discard them and leave?".
- Version history on a narrow screen shows the source device under the time, so the table no longer needs a sideways scroll.

## v0.7.2 - 2026-11-23

Patch release: Web UI header bar and refresh flash fix.

### Fixed

- The header bar spans the full page width again when the page has no scrollbar. The 0.7.1 fix did not work in Chrome (a reserved scrollbar gutter is painted with the root background colour only), so it is removed; when the page scrolls, the scrollbar covers the right end of the bar as on any web page.
- Refreshing a page no longer starts with a white frame: the page declares a dark colour scheme, so the first frame is dark.

## v0.7.1 - 2026-11-23

Patch release: Web UI header bar fix.

### Fixed

- The header bar now spans the full page width when the page has no scrollbar; the reserved scrollbar gutter beside it used to show the page background as a gap.

## v0.7.0 - 2026-11-23

Minor release: download the client installers from the Web UI.

### Added

- The overview's device table has a **Download client** button: a signed-in admin downloads the Windows and Linux installers that the server holds, under their Releases page names and without an enrollment code, and runs one on an enrolled device to upgrade it. A device whose reported release is older than the server's installer for its system shows an **Upgrade available** button that opens the same dialog. Requests to `/dl/windows` or `/dl/linux` without a session count towards the IP lockout.

## v0.6.0 - 2026-11-22

Minor release: installers for the server and the clients, and `kaze-syncc upgrade`.

### Added

- Run `kaze-syncd install` to upgrade an installed server using its existing data directory, TLS settings and OpenWrt WAN rule. The installer refuses older server binaries before replacing the installed copy.
- Publish Linux and Windows client installers with `kaze-syncd client-pkg` without restarting the server. The server can personalize downloads for a one-time enrollment code and serve the original packages to enrolled devices over the authenticated API.
- The enrollment dialog in the Web UI offers the Windows and Linux installer download links with copy buttons, a one-line `curl ... | sh` command for a Linux device without a desktop (with `--pinnedpubkey` for a self-signed or local-CA certificate) and a `wget` alternative that checks the file's SHA-256, shows the version of each client package on the server, warns when it differs from the server version, and says so when the server has no client package.
- `kaze-syncc install --from FILE` installs or upgrades the client from an installer file: it stops the agent, replaces the program files through temporary names (restoring the old ones if a step fails), registers the device from the registration block of a downloaded installer, restarts the agent and keeps `device.json` and the folder databases. It takes the directory of the existing autostart record, refuses a package whose protocol is newer than the server's unless `--force` is given, and the new `kaze-sync-setup` program (the Windows `setup.exe`) does the same from its own file and asks for a pasted registration command when it has none.
- `kaze-syncc upgrade` fetches the client installer from the server the device is registered with (over its pinned connection, with the device token), verifies it and installs it through the same path as `install`: nothing happens when the server offers no newer version, and `--force` installs anyway. On Windows it hands the downloaded setup program over to a new console window and exits, because a running program cannot replace itself.
- `go run ./tools/release` also builds three installers and lists them in `SHA256SUMS`: `kaze-sync-server-X.Y.Z-linux-amd64.sh` (a self-extracting sh that checks its own SHA-256, carries both client installers and publishes them with `kaze-syncd client-pkg`), `kaze-sync-client-X.Y.Z-linux-amd64.sh` and `kaze-sync-client-X.Y.Z-windows-amd64-setup.exe`, which are also the files to hand to `kaze-syncd client-pkg` when only the client is released. The build fails unless the Windows agent has the GUI subsystem, and the installers are reproducible.
- The README quick start and the user manual (installing the server and clients, upgrading) now use the installers: the server installer, the download links and `curl` line of the Add device dialog, `kaze-syncc upgrade` and `kaze-syncd client-pkg`; the manual steps stay as manual installation. New manual section 3.3 covers a Linux client without a graphical interface, and the SmartScreen steps for the unsigned Windows installer are written down. Section 15 now says to pass `--wan` on OpenWrt whenever `--data` is passed.

### Fixed

- A frozen scope no longer retries "link not created" rows, so a restored file is not replaced by a stale link.
- Installers: unique staging names for client binaries and a short retry of the install lock; a 404 from the package probe is treated as an older server; a Linux client install refuses a machine without systemd before changing anything.

## v0.5.3 - 2026-11-19

Patch release: held deletions clear by themselves once the server has deleted the path too.

### Fixed

- Held changes: a held deletion now clears by itself once the server has deleted the path too, also when the server's deletion came before this check existed (the client asks the server instead of waiting for the deletion to come again). Only the record goes; a file put back in the meantime stays and syncs as a new file.
- After a server restore (new epoch), a scope that was waiting for a decision now reads the server's new history from the start once it is unfrozen; before, the files skipped while it was frozen kept no server version.
- Server: a client checking its held deletions gets the current entries without their block lists, so the check stays small whatever the size of the files. Upgrade the server first to benefit; older servers still work. The check never disturbs the client's own pending uploads on the server, and it asks one path at a time until the server is known to support it.
- Held changes: clearing a held deletion also drops a stale "link not created" record for that path.

## v0.5.2 - 2026-11-19

Patch release: four fixes from an architecture review.

### Fixed

- Web Push: a notification queued behind a slow push service no longer takes the hourly quota twice; one reservation covers it from queueing to sending.
- Held changes: a group of 2001 or more changes on the same path no longer hangs the page; the fold count now matches what is rendered.
- Device rename: two renames in quick succession (`A` to `B`, then `B` to `C`) can no longer leave the middle name on the folder's records.
- Notifications: after a mapping is removed, Web Push no longer sends for it, matching the bell.

## v0.5.1 - 2026-11-18

Patch release: the held-changes bulk buttons no longer do nothing silently.

### Fixed

- Held-changes dialog: **Upload** under **Set the checked rows to** is disabled, with the reason written beside the buttons, when no row it would apply to can take it (a group that folds into directory rows only offers Overwrite and Discard). When some checked rows cannot take the chosen action, the dialog says "N rows keep their action".

## v0.5.0 - 2026-11-18

Devices can be renamed, and held changes can be handled in bulk.

### Added

- Device rename: the device page's title-row **⋯** menu has **Rename device…**. The new name follows the enrollment rules and must not clash with another device. The device keeps its token and mappings and is not enrolled again; current files, pending changes and notifications show the new name, while older versions keep the name they were written under. Clients take the new name from their configuration and write it into `device.json` (older clients keep using the enrolled name until upgraded). `tools/adminstatus -rename OLD=NEW` submits the same form from a script.
- Held-changes dialog: a checkbox on each row (the header box selects all) and **Set the checked rows to** upload, upload and overwrite, or discard. A group with more than 2000 changes is folded by directory; a directory row's action applies to every held change under it.

### Changed

- Small muted text reaches 4.5:1 contrast on every surface, and every table header carries `scope="col"`.

## v0.4.1 - 2026-11-18

Patch release: repeated push notifications now alert every time.

### Fixed

- Web Push: a notification with the same tag as one still shown replaced it silently, so only the first test push (or the first push of a repeated cause) alerted on desktop browsers. Tagged notifications now set `renotify`, and each test push uses a unique tag.
- Settings page: when the browser cannot reach its push service while subscribing (`AbortError`, for example Android Chrome without Google Play services or without a route to the push service), the card now says so instead of a generic failure.

## v0.4.0 - 2026-11-18

Error alerts now go out as Web Push sent only by the server, replacing ntfy.

### Added

- Web Push (design 4.3): the server keeps a VAPID key and the browsers' subscriptions, encrypts each message per RFC 8291 and sends it with the standard library only. Every push repeats one entry of the notification bell: held changes after 2 minutes, device errors after 5 minutes, a too-old client, an offline device, low disk space, a failed garbage collection, and a certificate that cannot be renewed. Each cause is pushed once per browser, repeats wait an hour, and all pushes are capped at 20 per hour. New admin endpoints `GET /push/subscriptions`, `POST /push/subscribe`, `/push/unsubscribe` and `/push/test`, and `/sw.js`. Push needs a trusted certificate (local CA or your own certificate), and iPhone and iPad are not supported. See the user manual, section 9.1.
- The settings page has a **Push notifications** card: turn push on or off in this browser, see every subscription with its last push, and send a test push to or delete any of them. When push cannot be turned on in a browser (self-signed certificate, untrusted certificate, unsupported browser, notifications blocked), the card says why and what to do next.
- The notification bell lists four more problems: a too-old device client, low data-disk space, a failed garbage collection and a certificate that cannot be renewed, plus a warning for an invalid push subscription.
- A client reports a `folder` error to the server when the sync cycles of a folder fail 3 times in a row.

### Fixed

- Exclusion-rules preview: typing quickly in the filter box no longer drops characters (the box is no longer rebuilt when results arrive; stale results are discarded).

### Changed

- A device that has not connected for 3 days (was 7) raises the offline warning.
- The user manual (sections 2.4, 9 and 9.1) describes Web Push and importing the CA on a phone.

### Removed

- ntfy push messages and `ntfy.json`, on the server and on the clients (the `internal/ntfy` package). A leftover `ntfy.json` is no longer read; each program logs one info line about it. Turn on Web Push instead (user manual 9.1); with the default self-signed certificate, switch to the local CA first (section 2.4).

## v0.3.5 - 2026-11-18

Shows each client's release version on the device pages, sizes the server's garbage-collection memory from the machine's memory, adds daily resource summaries, and publishes prebuilt packages. For full details, see the [v0.3.5 release notes](RELEASE-NOTES.md).

### Added

- Prebuilt release packages: `kaze-sync-X.Y.Z-linux-amd64.tar.gz` (server and Linux client), `kaze-sync-X.Y.Z-windows-amd64.zip` (Windows client and windowless agent) and `SHA256SUMS`, built reproducibly by `go run ./tools/release` and published on GitHub Releases. `go run ./tools/pubcheck` checks the tree before publishing.

### Changed

- The user manual installs from the prebuilt packages by default and moves building from source to Appendix A; section 13 gains a "Memory sizing" table that relates garbage-collection memory to file count and `keep_days`, and the README quick start downloads the packages.

- Both processes now sample RSS and CPU once per minute and keep today/yesterday summaries in memory. Clients attach summaries to poll requests (protocol 6); the admin-session-protected `GET /resources` returns server and device statistics. Server GC time, CPU, reference counts, budget and observed RSS peaks are reported separately, with explicit coverage and peak-certainty flags. No metrics files, database writes, new dependencies or UI controls are added.

- The server now calculates its GC reference-array budget once at startup as 3/8 of the smaller of physical memory and the cgroup memory limit, with a 64 MB minimum and a 64 MB fallback when physical memory cannot be read. A 512 MB machine gets a 192 MB budget for about 6 million references in one pass. Small data sets still allocate only the array they need; no configuration option is added. GC logs include `budget` in bytes.

### Fixed

- The device list and device details show the client release version (`x.y.z`) reported by poll requests instead of the integer protocol version. Older clients that do not report a release version show `-`. The protocol version remains 6.
- The daily resource summaries now notice a wall clock that is set back: the time comparison no longer uses Go's monotonic reading, which had hidden the step, so a partial day is correctly marked as not eligible for export. The garbage-collection memory-limit lookup walks cgroup paths with POSIX path rules, so it also terminates on Windows (where the server is not supported and only the tests run).

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

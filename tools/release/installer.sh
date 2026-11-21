#!/bin/sh
# kaze-sync @KIND@ installer @VERSION@ (docs/design.md 9.5): this shell header, then a gzip tar payload, then a
# manifest. The header only checks and unpacks the payload; the installation itself is done by the program inside.
KS_KIND=@KIND@
KS_VERSION=@VERSION@
KS_PAYLOAD_SIZE=@PAYLOAD_SIZE@
KS_PAYLOAD_SHA256=@PAYLOAD_SHA256@
KS_HEADER_SIZE=@HEADER_SIZE@

fail() {
	echo "kaze-sync installer: $*" >&2
	exit 1
}

# server: sudo sh FILE [kaze-syncd install options]
server() {
	[ "$(id -u)" = 0 ] || fail "run it as root: sudo sh $self"
	root=${KAZE_SYNC_ROOT:-} # only the tests set it
	data=
	want=
	for a in "$@"; do
		if [ -n "$want" ]; then
			data=$a
			want=
			continue
		fi
		case $a in
		--data | -data) want=1 ;;
		--data=* | -data=*) data=${a#*=} ;;
		esac
	done
	installed=
	if [ -e "$root/etc/init.d/kaze-sync" ] || [ -e "$root/etc/systemd/system/kaze-sync.service" ]; then
		installed=1
	fi
	if [ -z "$installed" ] && [ -z "$data" ]; then
		if [ -e "$root/etc/openwrt_release" ]; then
			fail "first installation on OpenWrt needs the data directory on a disk with room for it: sudo sh $self --data DIR"
		fi
		if [ -t 0 ]; then
			printf 'Data directory [/var/lib/kaze-sync]: '
			read -r data
			[ -n "$data" ] || data=/var/lib/kaze-sync
		else
			fail "first installation needs a data directory: sudo sh $self --data /var/lib/kaze-sync"
		fi
		set -- --data "$data" "$@"
	fi
	"$unpacked/kaze-syncd" install "$@" || fail "kaze-syncd install failed"
	if [ -n "$data" ]; then
		set -- --data "$data"
	else
		set --
	fi
	"$unpacked/kaze-syncd" client-pkg "$@" "$unpacked/client-linux-amd64.sh" "$unpacked/client-windows-amd64-setup.exe" ||
		fail "the server is installed but the client installers were not published; run kaze-syncd client-pkg with the two client installer files of this release"
	if [ -z "$installed" ]; then
		"$unpacked/kaze-syncd" admin-password "$@" || fail "kaze-syncd admin-password failed"
	fi
	echo "kaze-sync server $KS_VERSION is installed. Upgrade the clients one by one with: kaze-syncc upgrade"
}

# client: sh FILE [kaze-syncc install options]
client() {
	"$unpacked/kaze-syncc" install --from "$self" "$@"
}

# payload writes the compressed payload to the standard output.
payload() {
	tail -c +$((KS_HEADER_SIZE + 1)) "$self" | head -c "$KS_PAYLOAD_SIZE"
}

main() {
	self=$0
	[ -f "$self" ] || fail "cannot read $self"
	for tool in tail head gunzip tar sha256sum mktemp; do
		command -v "$tool" >/dev/null 2>&1 || fail "$tool is needed and was not found"
	done
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/kaze-sync-install.XXXXXX") || fail "cannot create a temporary directory"
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM
	# Two passes over the file (check, then unpack) keep a second copy of the payload out of /tmp, which is
	# memory on OpenWrt.
	sum=$(payload | sha256sum)
	[ "${sum%% *}" = "$KS_PAYLOAD_SHA256" ] || fail "the payload SHA-256 does not match: the file is damaged or incomplete, download it again"
	unpacked=$tmp/x
	mkdir "$unpacked" || fail "cannot create $unpacked"
	payload | gunzip -c | tar -xf - -C "$unpacked" || fail "cannot unpack the payload"
	"$KS_KIND" "$@"
	rc=$?
	exit "$rc"
}

main "$@"
exit $?

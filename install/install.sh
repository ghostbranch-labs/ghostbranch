#!/bin/sh
# Install Ghostbranch on macOS or Linux:
#
#   curl -fsSL https://ghostbranch.site/install.sh | sh
#
# It downloads the release archive for this machine and the release's
# checksums.txt from GitHub over HTTPS, refuses the archive unless its
# SHA-256 matches, and puts the ghostbranch binary in ~/.local/bin. It never
# uses sudo. Source: https://github.com/ghostbranch-labs/ghostbranch/blob/main/install/install.sh
# A copy installed elsewhere is upgraded by running this again with GHOSTBRANCH_INSTALL_DIR set to its folder.
#
# Settings (environment variables):
#   GHOSTBRANCH_VERSION      a release tag to install, such as v0.1.0 (default: the latest release)
#   GHOSTBRANCH_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
# For tests only:
#   GHOSTBRANCH_DOWNLOAD_BASE  replaces https://github.com/ghostbranch-labs/ghostbranch/releases;
#                              must be https://, or http://127.0.0.1 or http://localhost
#                              (for a local base, redirects may use plain http too: tests only)
#   GHOSTBRANCH_TEST_ARCH      pretends uname -m printed this
#
# Everything runs from main, called on the last line, so a download cut
# short runs nothing.

set -eu

say() { printf '%s\n' "$*"; }
fail() {
	printf 'ghostbranch install: %s\n' "$*" >&2
	exit 1
}

supported="darwin_amd64, darwin_arm64, linux_amd64, linux_arm64"

detect_target() {
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "this script installs on macOS and Linux; for Windows use install.ps1. Supported: $supported" ;;
	esac
	machine=${GHOSTBRANCH_TEST_ARCH:-$(uname -m)}
	case "$machine" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "no build for $os $machine. Supported: $supported" ;;
	esac
}

check_base() {
	case "$base" in
	*@*) fail "GHOSTBRANCH_DOWNLOAD_BASE must not hold a user name: $base" ;;
	https://*) proto='=https' ;;
	http://127.0.0.1 | http://127.0.0.1:* | http://127.0.0.1/* | http://localhost | http://localhost:* | http://localhost/*) proto='=http' ;;
	*) fail "GHOSTBRANCH_DOWNLOAD_BASE must be https://, or http://127.0.0.1 or http://localhost: $base" ;;
	esac
}

check_version() { # version what
	case "$1" in
	*[!0-9A-Za-z.-]* | "") fail "$2 isn't a version (such as 0.1.0 or v0.1.0): $1" ;;
	[0-9]*.[0-9]*.[0-9]*) ;;
	*) fail "$2 isn't a version (such as 0.1.0 or v0.1.0): $1" ;;
	esac
}

fetch() { # url file
	curl -q --fail --silent --show-error --location --proto "$proto" --proto-redir "$proto" --output "$2" "$1" ||
		fail "couldn't download $1"
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		fail "neither sha256sum nor shasum is installed, so the download can't be checked"
	fi
}

main() {
	command -v curl >/dev/null 2>&1 || fail "curl is needed"
	command -v tar >/dev/null 2>&1 || fail "tar is needed"
	detect_target
	base=${GHOSTBRANCH_DOWNLOAD_BASE:-https://github.com/ghostbranch-labs/ghostbranch/releases}
	base=${base%/}
	check_base
	dir=${GHOSTBRANCH_INSTALL_DIR:-$HOME/.local/bin}
	while [ "${#dir}" -gt 1 ] && [ "${dir%/}" != "$dir" ]; do dir=${dir%/}; done
	case "$dir" in *"'"*) fail "the install folder can't hold a single quote: $dir" ;; esac

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/ghostbranch-install.XXXXXX")
	trap 'rm -rf "$tmp"; rm -f "$dir/.ghostbranch.new"' EXIT
	trap 'exit 130' INT TERM
	trap 'exit 129' HUP

	if [ -n "${GHOSTBRANCH_VERSION:-}" ]; then
		check_version "${GHOSTBRANCH_VERSION#v}" "GHOSTBRANCH_VERSION"
		tag=v${GHOSTBRANCH_VERSION#v}
		from="$base/download/$tag"
	else
		from="$base/latest/download"
	fi
	fetch "$from/checksums.txt" "$tmp/checksums.txt"

	# The archive's name holds the version, so it comes from checksums.txt.
	archive=$(awk -v suffix="_${os}_${arch}.tar.gz" '
		length($2) > length(suffix) && substr($2, length($2) - length(suffix) + 1) == suffix && $2 ~ /^ghostbranch_/ { print $2 }
	' "$tmp/checksums.txt")
	[ -n "$archive" ] || fail "the release has no archive for ${os}_${arch}"
	case "$archive" in *"
"*) fail "the release lists more than one archive for ${os}_${arch}" ;; esac
	version=${archive#ghostbranch_}
	version=${version%"_${os}_${arch}.tar.gz"}
	check_version "$version" "the version in $archive"
	want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")

	fetch "$base/download/v$version/$archive" "$tmp/$archive"
	got=$(sha256_of "$tmp/$archive")
	if [ "$got" != "$want" ]; then
		rm -f "$tmp/$archive"
		fail "checksum mismatch for $archive (expected $want, got $got); nothing was installed"
	fi
	say "Verified $archive against checksums.txt."

	mkdir -p "$tmp/x"
	tar -xzf "$tmp/$archive" -C "$tmp/x" ghostbranch || fail "couldn't extract ghostbranch from $archive"
	mkdir -p "$dir"
	# Copy next to the target, then rename over it: an earlier copy is
	# replaced in one step, even while it is running.
	rm -f "$dir/.ghostbranch.new"
	cp "$tmp/x/ghostbranch" "$dir/.ghostbranch.new"
	chmod 0755 "$dir/.ghostbranch.new"
	mv -f "$dir/.ghostbranch.new" "$dir/ghostbranch"

	ran=1
	out=$(GHOSTBRANCH_NO_UPDATE_CHECK=1 "$dir/ghostbranch" version 2>&1) || ran=0
	installed=$(printf '%s\n' "$out" | head -n 1)
	if [ "$ran" = 1 ]; then
		say "Installed $installed"
	else
		say "Warning: $dir/ghostbranch was installed but doesn't run on this machine ($installed)."
	fi
	say "  at $dir/ghostbranch"
	case ":${PATH}:" in
	*":$dir:"*) ;;
	*)
		say ""
		say "$dir isn't on your PATH. Add it with:"
		case "${SHELL:-}" in
		*/zsh) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc" ;;
		*/bash) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.bashrc" ;;
		*/fish) say "  fish_add_path '$dir'" ;;
		*) say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile" ;;
		esac
		say "then open a new terminal."
		;;
	esac
	say ""
	say "Next: ghostbranch login, then ghostbranch init in your repo."
}

main "$@"

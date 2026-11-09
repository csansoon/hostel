#!/bin/sh
# Installs Hostel from a GitHub release of csansoon/hostel.
#
#   curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/csansoon/hostel/main/install.sh | sh -s -- --server /opt/hostel
#
#   sh install.sh [--version vX.Y.Z] [--dir DIR] [--server DIR]
#
# Without --server, it installs the hostel binary for this machine into DIR
# (default ~/.local/bin): for guests and for "hostel machine connect". With
# --server, it also puts the binaries for all machines into DIR/helpers
# (helper_dir of server.toml). The server uses the tool definitions in its
# binary. It verifies each file against SHA256SUMS of the release.
#
# It downloads with curl (5 minutes for each file, 2 retries).
# HOSTEL_INSTALL_URL replaces the download URL of the release (a mirror).
set -eu

repo=csansoon/hostel
version=latest
dir="$HOME/.local/bin"
server=""
while [ $# -gt 0 ]; do
	case "$1" in
	--version) version="$2"; shift 2 ;;
	--dir) dir="$2"; shift 2 ;;
	--server) server="$2"; shift 2 ;;
	*) echo "install.sh: unknown argument $1" >&2; exit 2 ;;
	esac
done

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) echo "install.sh: unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
# A shell under Rosetta reports x86_64 on an Apple silicon Mac.
if [ "$(uname -s)" = Darwin ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
	arch=arm64
fi
case "$os" in
darwin | linux) ;;
*) echo "install.sh: unsupported OS $os; on Windows, download hostel-windows-$arch.exe from the release" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch() {
	if [ -n "${HOSTEL_INSTALL_URL:-}" ]; then
		curl -fsSL --max-time 300 -o "$tmp/$1" "$HOSTEL_INSTALL_URL/$1"
	elif [ "$version" = latest ]; then
		curl -fsSL --max-time 300 --retry 2 -o "$tmp/$1" "https://github.com/$repo/releases/latest/download/$1"
	else
		curl -fsSL --max-time 300 --retry 2 -o "$tmp/$1" "https://github.com/$repo/releases/download/$version/$1"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

fetch SHA256SUMS
get() {
	fetch "$1"
	want=$(awk -v f="$1" '$2 == f {print $1}' "$tmp/SHA256SUMS")
	if [ -z "$want" ] || [ "$(sha256 "$tmp/$1")" != "$want" ]; then
		echo "install.sh: checksum mismatch for $1" >&2
		exit 1
	fi
}

get "hostel-$os-$arch"
mkdir -p "$dir"
install -m 755 "$tmp/hostel-$os-$arch" "$dir/hostel"
echo "installed $dir/hostel ($("$dir/hostel" --version))"
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "note: $dir is not on your PATH; add it, for example: export PATH=\"$dir:\$PATH\"" ;;
esac

if [ -n "$server" ]; then
	mkdir -p "$server/helpers"
	awk '$2 ~ /^(hostel|mise)-/ && $2 !~ /\.tar\.gz$/ {print $2}' "$tmp/SHA256SUMS" > "$tmp/helpers"
	while read -r f; do
		get "$f"
		install -m 755 "$tmp/$f" "$server/helpers/$f"
	done < "$tmp/helpers"
	echo "installed $server/helpers"
fi

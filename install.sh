#!/bin/sh
# Installs Hostel from a GitHub release of csansoon/hostel.
#
#   sh install.sh [--version vX.Y.Z] [--dir DIR] [--server DIR]
#
# Without --server, it installs the hostel binary for this machine into DIR
# (default ~/.local/bin): for guests and for "hostel room connect". With
# --server, it also puts the binaries for all rooms into DIR/helpers and the
# platform scripts into DIR/platforms (helper_dir and platforms_dir of
# server.toml). It verifies each file against SHA256SUMS of the release.
#
# A private repository needs the GitHub CLI, logged in ("gh auth login");
# without gh, it downloads with curl (public repository only).
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
case "$os" in
darwin | linux) ;;
*) echo "install.sh: unsupported OS $os; on Windows, download hostel-windows-$arch.exe from the release" >&2; exit 1 ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch() {
	if [ -n "${HOSTEL_INSTALL_URL:-}" ]; then
		curl -fsSL --max-time 300 -o "$tmp/$1" "$HOSTEL_INSTALL_URL/$1"
	elif command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
		if [ "$version" = latest ]; then
			gh release download -R "$repo" -p "$1" -D "$tmp" --clobber
		else
			gh release download "$version" -R "$repo" -p "$1" -D "$tmp" --clobber
		fi
	elif [ "$version" = latest ]; then
		curl -fsSL --max-time 300 -o "$tmp/$1" "https://github.com/$repo/releases/latest/download/$1"
	else
		curl -fsSL --max-time 300 -o "$tmp/$1" "https://github.com/$repo/releases/download/$version/$1"
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

if [ -n "$server" ]; then
	mkdir -p "$server/helpers"
	awk '$2 ~ /^(hostel|mise)-/ && $2 != "hostel-platforms.tar.gz" {print $2}' "$tmp/SHA256SUMS" > "$tmp/helpers"
	while read -r f; do
		get "$f"
		install -m 755 "$tmp/$f" "$server/helpers/$f"
	done < "$tmp/helpers"
	get hostel-platforms.tar.gz
	rm -rf "$server/platforms"
	tar -xzf "$tmp/hostel-platforms.tar.gz" -C "$server"
	echo "installed $server/helpers and $server/platforms"
fi

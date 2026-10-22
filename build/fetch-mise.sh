#!/bin/sh
# Downloads the pinned mise binaries into a directory and verifies each one
# against the committed checksum file.
# Usage: fetch-mise.sh <version> <checksums> <outdir> [<goos>/<goarch>...]
# Without targets, it fetches all six.
set -eu

version=$1
checksums=$2
outdir=$3
shift 3
targets=${*:-darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

mkdir -p "$outdir"
for target in $targets; do
	goos=${target%/*}
	goarch=${target#*/}
	case $goos in
	darwin) os=macos suffix= ext= ;;
	linux) os=linux suffix=-musl ext= ;;
	windows) os=windows suffix= ext=.exe ;;
	esac
	case $goarch in
	amd64) arch=x64 ;;
	arm64) arch=arm64 ;;
	esac
	asset="mise-v$version-$os-$arch$suffix$ext"
	out="$outdir/mise-$goos-$goarch$ext"
	want=$(awk -v a="$asset" '$2 == a { print $1 }' "$checksums")
	if [ -z "$want" ]; then
		echo "fetch-mise: no checksum for $asset in $checksums; update the file for mise $version" >&2
		exit 1
	fi
	if [ -f "$out" ] && [ "$(sha256 "$out")" = "$want" ]; then
		continue
	fi
	tmp="$out.download"
	curl -fsSL --retry 3 --max-time 300 -o "$tmp" "https://github.com/jdx/mise/releases/download/v$version/$asset"
	got=$(sha256 "$tmp")
	if [ "$got" != "$want" ]; then
		rm -f "$tmp"
		echo "fetch-mise: checksum mismatch for $asset: got $got, want $want" >&2
		exit 1
	fi
	chmod 0755 "$tmp"
	mv "$tmp" "$out"
	echo "fetch-mise: $out"
done

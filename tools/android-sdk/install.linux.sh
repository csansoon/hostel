#!/bin/sh
# Installs the Android SDK in $HOSTEL_CACHE_DIR/android-sdk (command-line tools,
# platform tools, the emulator binaries, and the SDK platform) and the Ubuntu
# packages that the emulator needs. Java comes from the jdk tool. Machine env:
# HOSTEL_ANDROID_API (default 36) selects the SDK platform.
set -eu

api="${HOSTEL_ANDROID_API:-36}"
export ANDROID_HOME="$HOSTEL_CACHE_DIR/android-sdk"

# Packages that the emulator loads and that minimal Ubuntu images do not have.
missing=""
for p in curl unzip libpulse0 libnss3 libxcomposite1 libxcursor1 libxdamage1 libxi6 libxtst6 libgl1 libxkbfile1 libxkbcommon0 libxkbcommon-x11-0 libxrandr2 libdbus-1-3 libbsd0; do
	dpkg -s "$p" >/dev/null 2>&1 || missing="$missing $p"
done
# Ubuntu 24.04 and Debian 13 renamed these packages ("t64").
dpkg -s libasound2t64 >/dev/null 2>&1 || dpkg -s libasound2 >/dev/null 2>&1 || missing="$missing libasound2t64"
dpkg -s libpng16-16t64 >/dev/null 2>&1 || dpkg -s libpng16-16 >/dev/null 2>&1 || missing="$missing libpng16-16t64"
if [ -n "$missing" ]; then
	if ! sudo -n true 2>/dev/null; then
		echo "missing packages:$missing; install them, or give the machine user sudo without a password" >&2
		exit 1
	fi
	echo "install:$missing"
	sudo -n DEBIAN_FRONTEND=noninteractive apt-get update -qq
	# shellcheck disable=SC2086
	sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $missing >/dev/null
fi

if ! command -v java >/dev/null; then
	echo "Java is missing; the SDK manager needs Java 17 or later: add jdk to the machine's tools, for example \"jdk:21\"" >&2
	exit 1
fi

sdkmanager="$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager"
if ! [ -x "$sdkmanager" ]; then
	zip=$(curl -fsS https://dl.google.com/android/repository/repository2-3.xml |
		grep -o 'commandlinetools-linux-[0-9]*_latest.zip' | sort -t- -k3 -n -u | tail -n 1)
	echo "download $zip"
	tmp="$HOSTEL_CACHE_DIR/android-cmdline-tools.tmp"
	rm -rf "$tmp"
	mkdir -p "$tmp" "$ANDROID_HOME/cmdline-tools"
	curl -fsSLo "$tmp/tools.zip" "https://dl.google.com/android/repository/$zip"
	unzip -q "$tmp/tools.zip" -d "$tmp"
	rm -rf "$ANDROID_HOME/cmdline-tools/latest"
	mv "$tmp/cmdline-tools" "$ANDROID_HOME/cmdline-tools/latest"
	rm -rf "$tmp"
fi
yes | "$sdkmanager" --licenses >/dev/null 2>&1 || true
"$sdkmanager" platform-tools emulator "platforms;android-$api" >/dev/null
# -version starts QEMU, so it fails when a library is missing.
"$ANDROID_HOME/emulator/emulator" -version | head -n 1
echo "SDK platform android-$api"

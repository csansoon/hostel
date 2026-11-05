#!/bin/sh
# Installs the Android SDK in $HOSTEL_CACHE_DIR/android-sdk (command-line tools,
# platform tools, the emulator binaries, and the SDK platform). Java comes from
# the jdk tool. Machine env: HOSTEL_ANDROID_API (default 36) selects the SDK
# platform.
set -eu

api="${HOSTEL_ANDROID_API:-36}"
export ANDROID_HOME="$HOSTEL_CACHE_DIR/android-sdk"

if ! command -v java >/dev/null; then
	echo "Java is missing; the SDK manager needs Java 17 or later: add jdk to the machine's tools, for example \"jdk:21\"" >&2
	exit 1
fi

sdkmanager="$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager"
if ! [ -x "$sdkmanager" ]; then
	zip=$(curl -fsS https://dl.google.com/android/repository/repository2-3.xml |
		grep -o 'commandlinetools-mac-[0-9]*_latest.zip' | sort -t- -k3 -n -u | tail -n 1)
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
"$ANDROID_HOME/emulator/emulator" -version | head -n 1
echo "SDK platform android-$api"

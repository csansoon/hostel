#!/bin/sh
# KVM works, the emulator starts, and a golden AVD exists for each option. The
# last line is the options, comma-separated.
set -eu
api="${HOSTEL_ANDROID_API:-36}"
tag="${HOSTEL_ANDROID_TAG:-google_apis}"
avd_home="${ANDROID_AVD_HOME:-$HOME/.android/avd}"
if ! [ -r /dev/kvm ] || ! [ -w /dev/kvm ]; then
	echo "the room user cannot use /dev/kvm" >&2
	exit 1
fi
"$HOSTEL_CACHE_DIR/android-sdk/emulator/emulator" -version >/dev/null
for d in $(echo "$HOSTEL_TOOL_ALL_OPTIONS" | tr ',' ' '); do
	if ! [ -d "$avd_home/hostel-golden-$d-$api-$tag.avd/snapshots/clean" ]; then
		echo "no golden AVD for $d; repair the room to run install again" >&2
		exit 1
	fi
done
echo "$HOSTEL_TOOL_ALL_OPTIONS"

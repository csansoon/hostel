#!/bin/sh
# The SDK and a golden AVD for each device exist, and the disk has space for
# builds.
set -eu
api="${HOSTEL_ANDROID_API:-36}"
tag="${HOSTEL_ANDROID_TAG:-google_apis}"
avd_home="${ANDROID_AVD_HOME:-$HOME/.android/avd}"
"$HOSTEL_CACHE_DIR/android-sdk/emulator/emulator" -version >/dev/null
for d in $(echo "$HOSTEL_PLATFORM_DEVICES" | tr ',' ' '); do
	if ! [ -d "$avd_home/hostel-golden-$d-$api-$tag.avd/snapshots/clean" ]; then
		echo "no golden AVD for $d; repair the room to run provision again" >&2
		exit 1
	fi
done
free_gb=$(df -Pk "$HOME" | awk 'NR == 2 { print int($4 / 1048576) }')
if [ "$free_gb" -lt 10 ]; then
	echo "only $free_gb GB of free disk; at least 10 GB are necessary" >&2
	exit 1
fi

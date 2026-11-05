#!/bin/sh
# The SDK and its platform exist, and the disk has space for builds. The last
# line is the version: the API level of the SDK platform.
set -eu
api="${HOSTEL_ANDROID_API:-36}"
sdk="$HOSTEL_CACHE_DIR/android-sdk"
"$sdk/platform-tools/adb" version >/dev/null
if ! [ -d "$sdk/platforms/android-$api" ]; then
	echo "the SDK has no platform android-$api; repair the room to run install again" >&2
	exit 1
fi
free_gb=$(df -Pk "$HOME" | awk 'NR == 2 { print int($4 / 1048576) }')
if [ "$free_gb" -lt 10 ]; then
	echo "only $free_gb GB of free disk; at least 10 GB are necessary" >&2
	exit 1
fi
echo "$api"

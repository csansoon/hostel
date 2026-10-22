#!/bin/sh
# Xcode and the simulator service work, each device has a golden simulator,
# and the disk has space for builds.
set -eu
xcodebuild -version >/dev/null
devices=$(xcrun simctl list devices)
for d in $(echo "$HOSTEL_PLATFORM_DEVICES" | tr ',' ' '); do
	if ! echo "$devices" | grep -q "^ *hostel-golden-$d-[0-9.]* ("; then
		echo "no golden simulator for $d; repair the room to run provision again" >&2
		exit 1
	fi
done
free_gb=$(df -g "$HOME" | awk 'NR == 2 { print $4 }')
if [ "$free_gb" -lt 20 ]; then
	echo "only $free_gb GB of free disk; at least 20 GB are necessary" >&2
	exit 1
fi

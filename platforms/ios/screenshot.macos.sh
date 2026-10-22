#!/bin/sh
# $1 is the output path, $2 the device (default: the first lease device).
set -eu
device="${2:-$(echo "$HOSTEL_DEVICES" | cut -d, -f1)}"
i=1
for d in $(echo "$HOSTEL_DEVICES" | tr ',' ' '); do
	if [ "$d" = "$device" ]; then
		udid=$(echo "$SIMULATOR_UDIDS" | cut -d, -f"$i")
		xcrun simctl io "$udid" screenshot "$1"
		exit 0
	fi
	i=$((i + 1))
done
echo "device $device is not in this lease (devices: $HOSTEL_DEVICES)" >&2
exit 1

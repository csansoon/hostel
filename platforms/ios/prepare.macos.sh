#!/bin/sh
# Clones the golden simulator of each lease device, boots the clones, and
# writes SIMULATOR_UDID (the first device) and SIMULATOR_UDIDS (all, in the
# order of HOSTEL_DEVICES) to device.env. The clones have the name
# hostel-<slot>-<device>, so reset finds them without device.env.
set -eu

udids() {
	xcrun simctl list devices | sed -n "s/^ *\($1\) (\([0-9A-F-]*\)) .*/\2/p"
}

all=""
for d in $(echo "$HOSTEL_DEVICES" | tr ',' ' '); do
	golden=$(udids "hostel-golden-$d-[0-9.]*" | head -n 1)
	if [ -z "$golden" ]; then
		echo "no golden simulator for $d; repair the room (hostel admin room repair) to run provision again" >&2
		exit 1
	fi
	name="hostel-$HOSTEL_SLOT-$d"
	for old in $(udids "$name"); do
		xcrun simctl shutdown "$old" 2>/dev/null || true
		xcrun simctl delete "$old"
	done
	udid=$(xcrun simctl clone "$golden" "$name")
	xcrun simctl bootstatus "$udid" -b >/dev/null
	echo "$d: $name ($udid)"
	all="$all${all:+,}$udid"
done
printf 'SIMULATOR_UDID=%s\nSIMULATOR_UDIDS=%s\n' "${all%%,*}" "$all" > device.env

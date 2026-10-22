#!/bin/sh
# Shuts down and deletes the simulators of this slot. It finds them by name, so
# a repeated reset and a reset after a failed prepare work too.
set -eu
for udid in $(xcrun simctl list devices | sed -n "s/^ *hostel-$HOSTEL_SLOT-[a-z0-9-]* (\([0-9A-F-]*\)) .*/\1/p"); do
	xcrun simctl shutdown "$udid" 2>/dev/null || true
	xcrun simctl delete "$udid"
done

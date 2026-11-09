#!/bin/sh
# The simulator service works, and each option has a golden simulator. The last
# line is the options, comma-separated.
set -eu
devices=$(xcrun simctl list devices)
for d in $(echo "$HOSTEL_TOOL_ALL_OPTIONS" | tr ',' ' '); do
	if ! echo "$devices" | grep -q "^ *hostel-golden-$d-[0-9.]* ("; then
		echo "no golden simulator for $d; repair the machine to run install again" >&2
		exit 1
	fi
done
echo "$HOSTEL_TOOL_ALL_OPTIONS"

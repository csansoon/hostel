#!/bin/sh
# Shuts down and deletes the simulators of this slot. It finds them by their
# exact names (hostel-<slot>-<option>), so a repeated stop and a stop after a
# failed start work too, and the simulators of other slots stay.
set -eu
for d in $(echo "$HOSTEL_TOOL_ALL_OPTIONS" | tr ',' ' '); do
	for udid in $(xcrun simctl list devices | sed -n "s/^ *hostel-$HOSTEL_SLOT-$d (\([0-9A-F-]*\)) .*/\1/p"); do
		# Shut down first: the simulator's launchd_sim also has the UDID in its
		# command line, and killing it leaves all its processes behind.
		xcrun simctl shutdown "$udid" 2>/dev/null || true
		# Processes that use the simulator and that cleanup did not find (for
		# example the agent-device iOS runner after its daemon ended) have its
		# UDID in their command line.
		pkill -KILL -U "$(id -u)" -f "$udid" || true
		xcrun simctl shutdown "$udid" 2>/dev/null || true
		xcrun simctl delete "$udid"
	done
done

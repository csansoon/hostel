#!/bin/sh
# Records the apps that run, the jobs of the user's launchd domain, and the
# entries of ~/Applications and ~/Library/LaunchAgents, so stop can remove what
# the lease added. Only a lease that asked for macos-desktop runs it, and that
# lease holds the whole machine (whole_machine).
set -eu
# "<pid> <start time> <type> <bundle id> <executable path>" of each app.
lsappinfo list | awk '
	/^ *[0-9]+\) / { b = ""; x = "" }
	/bundleID=/ { b = $0; sub(/.*bundleID=/, "", b); gsub(/"/, "", b); gsub(/ /, "", b) }
	/executable path=/ { x = $0; sub(/.*executable path="/, "", x); sub(/"$/, "", x) }
	/ pid = [0-9]+ .*type="/ {
		t = $0; sub(/.*type="/, "", t); sub(/".*/, "", t)
		for (i = 1; i <= NF; i++) if ($i == "pid") print $(i+2), t, b, x
	}' |
	while read -r pid type bundle exe; do
		echo "$pid $(ps -o lstart= -p "$pid" | tr -s ' ' '_') $type $bundle $exe"
	done > "$HOSTEL_ROOM_DIR/desktop-apps"
launchctl print "gui/$(id -u)" | awk '/^\tservices = \{/ { s = 1; next } s && /^\t\}/ { exit } s { print $NF }' \
	> "$HOSTEL_ROOM_DIR/desktop-jobs"
if [ -d "$HOME/Applications" ]; then
	find "$HOME/Applications" -mindepth 1 -maxdepth 1 -exec basename {} \;
fi > "$HOSTEL_ROOM_DIR/desktop-applications"
if [ -d "$HOME/Library/LaunchAgents" ]; then
	find "$HOME/Library/LaunchAgents" -mindepth 1 -maxdepth 1 -exec basename {} \;
fi > "$HOSTEL_ROOM_DIR/desktop-launchagents"

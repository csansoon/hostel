#!/bin/sh
# Records the GUI apps that run and the entries of ~/Applications, so stop can
# remove what the lease added. Only a lease that asked for macos-desktop runs
# it, and that lease holds the whole machine (whole_machine).
set -eu
# "<pid> <start time> <bundle id>" of each foreground app.
lsappinfo list | awk '
	/bundleID=/ { b = $0; sub(/.*bundleID=/, "", b); gsub(/"/, "", b) }
	/ pid = [0-9]+ .*type="Foreground"/ { for (i = 1; i <= NF; i++) if ($i == "pid") print $(i+2), b }' |
	while read -r pid bundle; do
		echo "$pid $(ps -o lstart= -p "$pid" | tr -s ' ' '_') $bundle"
	done > "$HOSTEL_SLOT_DIR/desktop-apps"
if [ -d "$HOME/Applications" ]; then
	find "$HOME/Applications" -mindepth 1 -maxdepth 1 -exec basename {} \;
fi > "$HOSTEL_SLOT_DIR/desktop-applications"

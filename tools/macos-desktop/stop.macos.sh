#!/bin/sh
# Ends the apps and launchd jobs that the lease started, deletes their saved
# windows, the agent-device runner, and the apps and launch agents that the
# lease added to ~/Applications and ~/Library/LaunchAgents. Apps that a guest
# starts with "open", and agents that an app registers, are children of launchd,
# so the process cleanup does not find them. The lease holds the whole machine,
# so all of them belong to it. A repeated stop (no record of start) does nothing.
set -eu
[ -f "$HOSTEL_ROOM_DIR/desktop-apps" ] || exit 0
uid=$(id -u)
# macOS starts its own per-user agents at any time; they stay.
system_path() {
	case "$1" in /System/* | /usr/* | /Library/Apple/* | /bin/* | /sbin/*) return 0 ;; esac
	return 1
}

# Jobs first, so that launchd does not start an ended agent again.
launchctl print "gui/$uid" | awk '/^\tservices = \{/ { s = 1; next } s && /^\t\}/ { exit } s { print $NF }' |
	while read -r label; do
		grep -qxF "$label" "$HOSTEL_ROOM_DIR/desktop-jobs" && continue
		case "$label" in com.apple.*) continue ;; esac
		launchctl bootout "gui/$uid/$label" 2>/dev/null || true
	done
if [ -d "$HOME/Library/LaunchAgents" ] && [ -f "$HOSTEL_ROOM_DIR/desktop-launchagents" ]; then
	find "$HOME/Library/LaunchAgents" -mindepth 1 -maxdepth 1 -exec basename {} \; | while read -r plist; do
		grep -qxF "$plist" "$HOSTEL_ROOM_DIR/desktop-launchagents" || rm -rf "${HOME:?}/Library/LaunchAgents/$plist"
	done
fi

lsappinfo list | awk '
	/^ *[0-9]+\) / { b = ""; x = "" }
	/bundleID=/ { b = $0; sub(/.*bundleID=/, "", b); gsub(/"/, "", b); gsub(/ /, "", b) }
	/executable path=/ { x = $0; sub(/.*executable path="/, "", x); sub(/"$/, "", x) }
	/ pid = [0-9]+ .*type="/ {
		t = $0; sub(/.*type="/, "", t); sub(/".*/, "", t)
		for (i = 1; i <= NF; i++) if ($i == "pid") print $(i+2), t, b, x
	}' > "$HOSTEL_ROOM_DIR/desktop-apps.now"
while read -r pid type bundle exe; do
	# An app that ended since the list is not killed (its PID can be reused).
	start=$(ps -o lstart= -p "$pid") || continue
	start=$(echo "$start" | tr -s ' ' '_')
	grep -q "^$pid $start " "$HOSTEL_ROOM_DIR/desktop-apps" && continue
	case "$bundle" in com.apple.finder | com.apple.dock) continue ;; esac
	if [ "$type" != Foreground ]; then
		case "$bundle" in com.apple.*) continue ;; esac
		system_path "$exe" && continue
	fi
	# SIGKILL: a quit can wait at a "save changes" dialog.
	kill -KILL "$pid" 2>/dev/null || true
	case "$bundle" in
	*/* | "" | "[NULL]") ;;
	*)
		rm -rf "$HOME/Library/Saved Application State/$bundle.savedState" \
			"$HOME/Library/Containers/$bundle/Data/Library/Saved Application State/$bundle.savedState"
		;;
	esac
done < "$HOSTEL_ROOM_DIR/desktop-apps.now"
pkill -KILL -f AgentDeviceRunner 2>/dev/null || true
# Without the record of start, ~/Applications stays as it is.
if [ -d "$HOME/Applications" ] && [ -f "$HOSTEL_ROOM_DIR/desktop-applications" ]; then
	find "$HOME/Applications" -mindepth 1 -maxdepth 1 -exec basename {} \; | while read -r app; do
		grep -qxF "$app" "$HOSTEL_ROOM_DIR/desktop-applications" || rm -rf "${HOME:?}/Applications/$app"
	done
fi

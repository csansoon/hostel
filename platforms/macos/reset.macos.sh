#!/bin/sh
# Desktop leases only (HOSTEL_EXCLUSIVE=1): end the GUI apps that the lease
# started, delete their saved windows, the agent-device runner, and the apps
# that the lease added to ~/Applications. Apps that a guest starts with "open"
# are children of launchd, so the process cleanup does not find them. A repeated
# cleanup (no record of prepare) does nothing.
set -eu
[ "${HOSTEL_EXCLUSIVE:-}" = 1 ] || exit 0
[ -f "$HOSTEL_SLOT_DIR/desktop-apps" ] || exit 0
lsappinfo list | awk '
	/bundleID=/ { b = $0; sub(/.*bundleID=/, "", b); gsub(/"/, "", b) }
	/ pid = [0-9]+ .*type="Foreground"/ { for (i = 1; i <= NF; i++) if ($i == "pid") print $(i+2), b }' > "$HOSTEL_SLOT_DIR/desktop-apps.now"
while read -r pid bundle; do
	start=$(ps -o lstart= -p "$pid" | tr -s ' ' '_') || continue
	grep -qxF "$pid $start $bundle" "$HOSTEL_SLOT_DIR/desktop-apps" && continue
	case "$bundle" in com.apple.finder | com.apple.dock) continue ;; esac
	# SIGKILL: a quit can wait at a "save changes" dialog.
	kill -KILL "$pid" 2>/dev/null || true
	case "$bundle" in
	*/* | "" | "[ NULL ]") ;;
	*)
		rm -rf "$HOME/Library/Saved Application State/$bundle.savedState" \
			"$HOME/Library/Containers/$bundle/Data/Library/Saved Application State/$bundle.savedState"
		;;
	esac
done < "$HOSTEL_SLOT_DIR/desktop-apps.now"
pkill -KILL -f AgentDeviceRunner 2>/dev/null || true
# Without the record of prepare, ~/Applications stays as it is.
if [ -d "$HOME/Applications" ] && [ -f "$HOSTEL_SLOT_DIR/desktop-applications" ]; then
	find "$HOME/Applications" -mindepth 1 -maxdepth 1 -exec basename {} \; | while read -r app; do
		grep -qxF "$app" "$HOSTEL_SLOT_DIR/desktop-applications" || rm -rf "${HOME:?}/Applications/$app"
	done
fi

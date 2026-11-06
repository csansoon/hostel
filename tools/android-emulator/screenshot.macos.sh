#!/bin/sh
# $1 is the output path, $2 the option (default: the first option of the lease).
set -eu
option="${2:-$(echo "$HOSTEL_TOOL_OPTIONS" | cut -d, -f1)}"
i=1
for d in $(echo "$HOSTEL_TOOL_OPTIONS" | tr ',' ' '); do
	if [ "$d" = "$option" ]; then
		serial=$(echo "$ANDROID_SERIALS" | cut -d, -f"$i")
		"$HOSTEL_CACHE_DIR/android-sdk/platform-tools/adb" -s "$serial" exec-out screencap -p > "$1"
		exit 0
	fi
	i=$((i + 1))
done
echo "option $option is not in this lease (options: $HOSTEL_TOOL_OPTIONS)" >&2
exit 1

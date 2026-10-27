#!/bin/sh
# Stops the emulators of this slot. It finds them by the slot's console ports
# (HOSTEL_ANDROID_PORT + 2 * device index, as in prepare), so a repeated reset
# and a reset after a failed prepare work too, and the emulators of other slots
# stay.
set -eu

adb="$HOSTEL_CACHE_DIR/android-sdk/platform-tools/adb"
base="${HOSTEL_ANDROID_PORT:-5554}"
i=0
for _ in $(echo "$HOSTEL_PLATFORM_DEVICES" | tr ',' ' '); do
	port=$((base + 2 * i))
	i=$((i + 1))
	pattern="qemu-system-.* -port $port "
	pgrep -u "$(id -u)" -f "$pattern" >/dev/null || continue
	"$adb" -s "emulator-$port" emu kill >/dev/null 2>&1 || true
	n=0
	while pgrep -u "$(id -u)" -f "$pattern" >/dev/null; do
		n=$((n + 1))
		if [ "$n" -gt 20 ]; then
			pkill -KILL -u "$(id -u)" -f "$pattern" || true
			break
		fi
		sleep 1
	done
	echo "stopped emulator-$port"
done

#!/bin/sh
# Starts one emulator for each lease device from the golden AVD's "clean"
# snapshot. -read-only keeps the AVD unchanged, so each lease gets a clean
# device and several slots can use the same golden AVD. Each slot and device has
# its own console port: HOSTEL_ANDROID_PORT (slot or machine env, default 5554)
# + 2 * device index; a machine with several slots sets it for each slot. Writes
# ANDROID_SERIAL (the first device), ANDROID_SERIALS (all, in the order of
# HOSTEL_DEVICES), and ANDROID_HOME to device.env.
set -eu

api="${HOSTEL_ANDROID_API:-36}"
tag="${HOSTEL_ANDROID_TAG:-google_apis}"
export ANDROID_HOME="$HOSTEL_CACHE_DIR/android-sdk"
adb="$ANDROID_HOME/platform-tools/adb"
avd_home="${ANDROID_AVD_HOME:-$HOME/.android/avd}"
base="${HOSTEL_ANDROID_PORT:-5554}"

# A repeated prepare starts again from a clean device.
sh "$(dirname "$0")/reset.linux.sh"

serials=""
i=0
for d in $(echo "$HOSTEL_DEVICES" | tr ',' ' '); do
	name="hostel-golden-$d-$api-$tag"
	if ! [ -d "$avd_home/$name.avd/snapshots/clean" ]; then
		echo "no golden AVD $name; repair the room (hostel admin room repair) to run provision again" >&2
		exit 1
	fi
	port=$((base + 2 * i))
	"$ANDROID_HOME/emulator/emulator" -avd "$name" -port "$port" -read-only -snapshot clean -no-snapshot-save \
		-no-window -no-audio -no-boot-anim -gpu swiftshader_indirect >"emulator-$d.log" 2>&1 &
	serials="$serials${serials:+,}emulator-$port"
	i=$((i + 1))
done

for serial in $(echo "$serials" | tr ',' ' '); do
	n=0
	until [ "$("$adb" -s "$serial" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = 1 ]; do
		n=$((n + 1))
		if [ "$n" -gt 240 ]; then
			echo "$serial did not boot in 240 s" >&2
			exit 1
		fi
		sleep 1
	done
	echo "$serial booted"
done
printf 'ANDROID_SERIAL=%s\nANDROID_SERIALS=%s\nANDROID_HOME=%s\n' "${serials%%,*}" "$serials" "$ANDROID_HOME" > device.env

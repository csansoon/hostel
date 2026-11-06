#!/bin/sh
# Installs the system image and one golden AVD for each option of the tool with
# a "clean" snapshot. start starts read-only emulators from that snapshot. A
# golden AVD has the API level and the image tag in its name, so a new image
# gets new golden AVDs; old ones are deleted. The SDK comes from the
# android-sdk tool. Machine env: HOSTEL_ANDROID_API (default 36),
# HOSTEL_ANDROID_TAG (default google_apis).
set -eu

api="${HOSTEL_ANDROID_API:-36}"
tag="${HOSTEL_ANDROID_TAG:-google_apis}"
abi=x86_64
[ "$(uname -m)" = arm64 ] && abi=arm64-v8a
image="system-images;android-$api;$tag;$abi"
export ANDROID_HOME="$HOSTEL_CACHE_DIR/android-sdk"

"$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager" "$image" >/dev/null
echo "$image"

avdmanager="$ANDROID_HOME/cmdline-tools/latest/bin/avdmanager"
adb="$ANDROID_HOME/platform-tools/adb"
avd_home="${ANDROID_AVD_HOME:-$HOME/.android/avd}"

# device_type prints the first AVD device definition that the SDK has for a
# Hostel option.
device_type() {
	case "$1" in
	pixel-9) candidates="pixel_9 pixel_8 pixel_7" ;;
	pixel-tablet) candidates="pixel_tablet" ;;
	*) echo "unknown option $1" >&2; return 1 ;;
	esac
	types=$("$avdmanager" list device -c)
	for c in $candidates; do
		if echo "$types" | grep -qx "$c"; then
			echo "$c"
			return 0
		fi
	done
	echo "the SDK has no device definition for $1 (tried: $candidates)" >&2
	return 1
}

# The console port of the golden AVD's one boot, the last port of the emulator
# range; slots use HOSTEL_ANDROID_PORT (default 5554).
port=5682
pattern="qemu-system-.* -port $port "
# An install that was stopped can leave its emulator running.
pkill -KILL -u "$(id -u)" -f "$pattern" || true
for d in $(echo "$HOSTEL_TOOL_ALL_OPTIONS" | tr ',' ' '); do
	name="hostel-golden-$d-$api-$tag"
	for old in "$avd_home"/hostel-golden-"$d"-*.avd; do
		[ -e "$old" ] || continue
		old=$(basename "$old" .avd)
		if [ "$old" != "$name" ]; then
			"$avdmanager" delete avd -n "$old" >/dev/null
			echo "deleted $old"
		fi
	done
	if [ -d "$avd_home/$name.avd/snapshots/clean" ]; then
		echo "$name exists"
		continue
	fi
	type=$(device_type "$d")
	echo no | "$avdmanager" create avd --force -n "$name" -k "$image" -d "$type" >/dev/null
	"$ANDROID_HOME/emulator/emulator" -avd "$name" -port "$port" -no-snapshot-load -no-snapshot-save -no-window -no-audio \
		-no-boot-anim -gpu swiftshader_indirect >"$HOSTEL_CACHE_DIR/android-emulator-install.log" 2>&1 &
	pid=$!
	trap 'kill "$pid" 2>/dev/null || true' EXIT
	serial="emulator-$port"
	i=0
	until [ "$("$adb" -s "$serial" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = 1 ]; do
		i=$((i + 1))
		if [ "$i" -gt 300 ] || ! kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
			tail -n 20 "$HOSTEL_CACHE_DIR/android-emulator-install.log" >&2
			echo "the first boot of $name did not end" >&2
			exit 1
		fi
		sleep 1
	done
	"$adb" -s "$serial" emu avd snapshot save clean >/dev/null
	"$adb" -s "$serial" emu kill >/dev/null
	wait "$pid" || true
	trap - EXIT
	echo "created $name ($type)"
done

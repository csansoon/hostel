#!/bin/sh
# Installs the Android SDK in $HOSTEL_CACHE_DIR/android-sdk, the Ubuntu packages
# that the emulator needs, and one golden AVD for each device of the platform
# with a "clean" snapshot. prepare starts read-only emulators from that
# snapshot. A golden AVD has the API level and the image tag in its name, so a
# new image gets new golden AVDs; old ones are deleted. Machine env:
# HOSTEL_ANDROID_API (default 36), HOSTEL_ANDROID_TAG (default google_apis).
set -eu

api="${HOSTEL_ANDROID_API:-36}"
tag="${HOSTEL_ANDROID_TAG:-google_apis}"
image="system-images;android-$api;$tag;x86_64"
export ANDROID_HOME="$HOSTEL_CACHE_DIR/android-sdk"

if ! [ -r /dev/kvm ] || ! [ -w /dev/kvm ]; then
	echo "the room user cannot use /dev/kvm; the emulator needs KVM (a machine with nested virtualization)" >&2
	exit 1
fi

# Packages that the emulator loads and that minimal Ubuntu images do not have.
missing=""
for p in curl unzip libpulse0 libnss3 libxcomposite1 libxcursor1 libxdamage1 libxi6 libxtst6 libgl1 libxkbfile1 libxkbcommon0 libxkbcommon-x11-0 libxrandr2 libdbus-1-3 libbsd0; do
	dpkg -s "$p" >/dev/null 2>&1 || missing="$missing $p"
done
# Ubuntu 24.04 and Debian 13 renamed these packages ("t64").
dpkg -s libasound2t64 >/dev/null 2>&1 || dpkg -s libasound2 >/dev/null 2>&1 || missing="$missing libasound2t64"
dpkg -s libpng16-16t64 >/dev/null 2>&1 || dpkg -s libpng16-16 >/dev/null 2>&1 || missing="$missing libpng16-16t64"
if [ -n "$missing" ]; then
	if ! sudo -n true 2>/dev/null; then
		echo "missing packages:$missing; install them, or give the room user sudo without a password" >&2
		exit 1
	fi
	echo "install:$missing"
	sudo -n DEBIAN_FRONTEND=noninteractive apt-get update -qq
	# shellcheck disable=SC2086
	sudo -n DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $missing >/dev/null
fi

if ! command -v java >/dev/null; then
	echo "Java is missing; the SDK manager needs Java 17 or later: add it to the machine tools, for example tools = { java = \"21\" }" >&2
	exit 1
fi

sdkmanager="$ANDROID_HOME/cmdline-tools/latest/bin/sdkmanager"
if ! [ -x "$sdkmanager" ]; then
	zip=$(curl -fsS https://dl.google.com/android/repository/repository2-3.xml |
		grep -o 'commandlinetools-linux-[0-9]*_latest.zip' | sort -t- -k3 -n -u | tail -n 1)
	echo "download $zip"
	tmp="$HOSTEL_CACHE_DIR/android-cmdline-tools.tmp"
	rm -rf "$tmp"
	mkdir -p "$tmp" "$ANDROID_HOME/cmdline-tools"
	curl -fsSLo "$tmp/tools.zip" "https://dl.google.com/android/repository/$zip"
	unzip -q "$tmp/tools.zip" -d "$tmp"
	rm -rf "$ANDROID_HOME/cmdline-tools/latest"
	mv "$tmp/cmdline-tools" "$ANDROID_HOME/cmdline-tools/latest"
	rm -rf "$tmp"
fi
yes | "$sdkmanager" --licenses >/dev/null 2>&1 || true
"$sdkmanager" platform-tools emulator "$image" >/dev/null
# -version starts QEMU, so it fails when a library is missing.
version=$("$ANDROID_HOME/emulator/emulator" -version)
echo "$(echo "$version" | head -n 1); $image"

avdmanager="$ANDROID_HOME/cmdline-tools/latest/bin/avdmanager"
adb="$ANDROID_HOME/platform-tools/adb"
avd_home="${ANDROID_AVD_HOME:-$HOME/.android/avd}"

# device_type prints the first AVD device definition that the SDK has for a
# Hostel device name.
device_type() {
	case "$1" in
	pixel-9) candidates="pixel_9 pixel_8 pixel_7" ;;
	pixel-tablet) candidates="pixel_tablet" ;;
	*) echo "unknown device $1" >&2; return 1 ;;
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

# The console port of the golden AVD's one boot; prepare uses 5554 to 5616.
port=5680
for d in $(echo "$HOSTEL_PLATFORM_DEVICES" | tr ',' ' '); do
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
		-no-boot-anim -gpu swiftshader_indirect >"$HOSTEL_CACHE_DIR/android-provision.log" 2>&1 &
	pid=$!
	serial="emulator-$port"
	i=0
	until [ "$("$adb" -s "$serial" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = 1 ]; do
		i=$((i + 1))
		if [ "$i" -gt 300 ] || ! kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
			tail -n 20 "$HOSTEL_CACHE_DIR/android-provision.log" >&2
			echo "the first boot of $name did not end" >&2
			exit 1
		fi
		sleep 1
	done
	"$adb" -s "$serial" emu avd snapshot save clean >/dev/null
	"$adb" -s "$serial" emu kill >/dev/null
	wait "$pid" || true
	echo "created $name ($type)"
done

#!/bin/sh
# Checks Xcode and the iOS runtime, and creates one golden simulator for each
# device of the platform. prepare clones them. A golden simulator has the
# runtime version in its name, so a new runtime gets new golden simulators; old
# ones are deleted. HOSTEL_IOS_RUNTIME (machine env) selects a runtime version
# such as "26.5"; the default is the newest iOS runtime.
set -eu

xcodebuild -version

runtime_line() {
	xcrun simctl list runtimes | grep '^iOS ' | grep -v unavailable |
		if [ -n "${HOSTEL_IOS_RUNTIME:-}" ]; then grep "^iOS $HOSTEL_IOS_RUNTIME "; else tail -n 1; fi
}

# device_type prints the first simulator device type that Xcode has for a
# Hostel device name.
device_type() {
	case "$1" in
	iphone-17-pro) candidates="iPhone-17-Pro" ;;
	ipad-pro-13) candidates="iPad-Pro-13-inch-M5-12GB iPad-Pro-13-inch-M4-8GB" ;;
	*) echo "unknown device $1" >&2; return 1 ;;
	esac
	types=$(xcrun simctl list devicetypes)
	for c in $candidates; do
		if echo "$types" | grep -q "(com.apple.CoreSimulator.SimDeviceType.$c)"; then
			echo "com.apple.CoreSimulator.SimDeviceType.$c"
			return 0
		fi
	done
	echo "Xcode has no device type for $1 (tried: $candidates)" >&2
	return 1
}

# simulators prints "<name> <udid>" for each simulator whose name matches the
# pattern.
simulators() {
	xcrun simctl list devices | sed -n "s/^ *\($1\) (\([0-9A-F-]*\)) .*/\1 \2/p"
}

line=$(runtime_line)
if [ -z "$line" ]; then
	echo "no iOS simulator runtime${HOSTEL_IOS_RUNTIME:+ $HOSTEL_IOS_RUNTIME}; install it with: xcodebuild -downloadPlatform iOS" >&2
	exit 1
fi
version=$(echo "$line" | cut -d' ' -f2)
runtime=$(echo "$line" | sed 's/.* - //')
echo "runtime: iOS $version ($runtime)"

for d in $(echo "$HOSTEL_PLATFORM_DEVICES" | tr ',' ' '); do
	name="hostel-golden-$d-$version"
	found=""
	for sim in $(simulators "hostel-golden-$d-[0-9.]*" | tr ' ' '='); do
		if [ "${sim%=*}" = "$name" ] && [ -z "$found" ]; then
			found=1
			# A provision that was stopped can leave it booted; clone needs it shut down.
			xcrun simctl shutdown "${sim#*=}" 2>/dev/null || true
		else
			xcrun simctl delete "${sim#*=}"
		fi
	done
	if [ -n "$found" ]; then
		echo "$name exists"
		continue
	fi
	type=$(device_type "$d")
	udid=$(xcrun simctl create "$name" "$type" "$runtime")
	# The first boot does the one-time setup work, so clones boot faster.
	xcrun simctl bootstatus "$udid" -b >/dev/null
	xcrun simctl shutdown "$udid"
	echo "created $name ($udid)"
done

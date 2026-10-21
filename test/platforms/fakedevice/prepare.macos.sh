#!/bin/sh
# Writes the device facts of the lease's devices (the first device first).
set -e
echo "prepare macos $HOSTEL_LEASE_ID $HOSTEL_DEVICES" >> "$HOSTEL_CACHE_DIR/fakedevice.log"
ids=""
for d in $(echo "$HOSTEL_DEVICES" | tr ',' ' '); do
	ids="$ids${ids:+,}$d-$HOSTEL_LEASE_ID"
done
printf 'FAKE_DEVICE_ID=%s\nFAKE_DEVICE_IDS=%s\nFAKE_DEVICE_OS=%s\n' "${ids%%,*}" "$ids" "macos" > device.env

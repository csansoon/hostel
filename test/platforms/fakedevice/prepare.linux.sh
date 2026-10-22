#!/bin/sh
# Writes the device facts of the lease's devices (the first device first).
set -e
# Test markers in the room cache: a slow or a failing prepare.
if test -e "$HOSTEL_CACHE_DIR/slow-prepare"; then sleep 20; fi
if test -e "$HOSTEL_CACHE_DIR/fail-prepare"; then echo "prepare failed on purpose" >&2; exit 1; fi
echo "prepare linux $HOSTEL_LEASE_ID $HOSTEL_DEVICES" >> "$HOSTEL_CACHE_DIR/fakedevice.log"
ids=""
for d in $(echo "$HOSTEL_DEVICES" | tr ',' ' '); do
	ids="$ids${ids:+,}$d-$HOSTEL_LEASE_ID"
done
printf 'FAKE_DEVICE_ID=%s\nFAKE_DEVICE_IDS=%s\nFAKE_DEVICE_OS=%s\n' "${ids%%,*}" "$ids" "linux" > device.env

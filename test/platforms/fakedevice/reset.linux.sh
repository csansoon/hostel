#!/bin/sh
# Test markers in the room cache: a slow or a failing reset.
if test -e "$HOSTEL_CACHE_DIR/slow-reset"; then sleep 20; fi
if test -e "$HOSTEL_CACHE_DIR/fail-reset"; then echo "reset failed on purpose" >&2; exit 1; fi
echo "reset linux $HOSTEL_LEASE_ID $HOSTEL_DEVICES" >> "$HOSTEL_CACHE_DIR/fakedevice.log"

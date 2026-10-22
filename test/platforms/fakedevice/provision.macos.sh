#!/bin/sh
# Records each call, so tests can check which OS variant ran and when.
echo "provision macos $HOSTEL_PLATFORM_DEVICES" >> "$HOSTEL_CACHE_DIR/fakedevice.log"

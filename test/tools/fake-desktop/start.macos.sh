#!/bin/sh
# Records each call, so tests can check the order and HOSTEL_EXCLUSIVE.
echo "start macos $HOSTEL_LEASE_ID exclusive=$HOSTEL_EXCLUSIVE" >> "$HOSTEL_CACHE_DIR/fake-desktop.log"

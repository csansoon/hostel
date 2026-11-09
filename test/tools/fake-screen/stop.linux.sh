#!/bin/sh
# Test markers in the machine cache: a slow or a failing stop.
if test -e "$HOSTEL_CACHE_DIR/slow-stop"; then sleep 20; fi
if test -e "$HOSTEL_CACHE_DIR/fail-stop"; then echo "stop failed on purpose" >&2; exit 1; fi
echo "stop linux $HOSTEL_LEASE_ID $HOSTEL_TOOL_OPTIONS exclusive=$HOSTEL_EXCLUSIVE" >> "$HOSTEL_CACHE_DIR/fake-screen.log"

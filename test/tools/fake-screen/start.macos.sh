#!/bin/sh
# Writes the facts of the lease's options (the first option first).
set -e
# Test markers in the room cache: a slow or a failing start.
if test -e "$HOSTEL_CACHE_DIR/slow-start"; then sleep 20; fi
if test -e "$HOSTEL_CACHE_DIR/fail-start"; then echo "start failed on purpose" >&2; exit 1; fi
echo "start macos $HOSTEL_LEASE_ID $HOSTEL_TOOL_OPTIONS exclusive=$HOSTEL_EXCLUSIVE" >> "$HOSTEL_CACHE_DIR/fake-screen.log"
ids=""
for o in $(echo "$HOSTEL_TOOL_OPTIONS" | tr ',' ' '); do
	ids="$ids${ids:+,}$o-$HOSTEL_LEASE_ID"
done
printf 'FAKE_SCREEN_ID=%s\nFAKE_SCREEN_IDS=%s\nFAKE_SCREEN_OS=%s\n' "${ids%%,*}" "$ids" "macos" > tool.env

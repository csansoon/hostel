#!/bin/sh
# Like the ios health script, it needs the result of provision: health runs only
# for platforms that the room is ready for.
if ! grep -q "^provision" "$HOSTEL_CACHE_DIR/fakedevice.log" 2>/dev/null; then
	echo "fakedevice health: provision has not run" >&2
	exit 1
fi

#!/bin/sh
# Like the ios-simulator check, it needs the result of install; the last line
# is the options.
if ! grep -q "^install" "$HOSTEL_CACHE_DIR/fake-screen.log" 2>/dev/null; then
	echo "fake-screen check: install has not run" >&2
	exit 1
fi
echo "$HOSTEL_TOOL_ALL_OPTIONS"

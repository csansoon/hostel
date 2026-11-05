#!/bin/sh
# Needs the result of install; the last line is the version.
set -e
if ! test -f "$HOSTEL_CACHE_DIR/fake-sdk/version"; then
	echo "fake-sdk check: install has not run" >&2
	exit 1
fi
cat "$HOSTEL_CACHE_DIR/fake-sdk/version"

#!/bin/sh
# Records each call, so tests can check which OS variant ran and when.
set -e
mkdir -p "$HOSTEL_CACHE_DIR/fake-sdk/bin"
echo "1.4.2" > "$HOSTEL_CACHE_DIR/fake-sdk/version"
echo "install macos" >> "$HOSTEL_CACHE_DIR/fake-sdk.log"

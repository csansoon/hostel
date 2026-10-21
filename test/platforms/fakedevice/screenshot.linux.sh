#!/bin/sh
# $1 is the output path, $2 the device (default: the first device).
set -e
device="${2:-$(echo "$HOSTEL_DEVICES" | cut -d, -f1)}"
printf '\211PNG\r\n\032\nfake screenshot of %s on linux\n' "$device" > "$1"

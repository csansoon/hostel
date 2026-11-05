#!/bin/sh
# $1 is the output path, $2 the option (default: the first option of the lease).
set -e
option="${2:-$(echo "$HOSTEL_TOOL_OPTIONS" | cut -d, -f1)}"
printf '\211PNG\r\n\032\nfake screenshot of %s on linux\n' "$option" > "$1"

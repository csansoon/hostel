#!/bin/sh
# $1 is the output path. A desktop screenshot needs a logged-in desktop session
# (machine label "desktop") and the Screen Recording permission for SSH
# sessions (docs/static-room-setup.md, section 6).
set -eu
if ! screencapture -x "$1" || [ ! -s "$1" ]; then
	echo "screencapture failed: the machine needs a logged-in desktop session and the Screen Recording permission for sshd-keygen-wrapper" >&2
	exit 1
fi

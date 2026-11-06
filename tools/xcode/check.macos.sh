#!/bin/sh
# Xcode works, and the disk has space for builds. The last line is the version
# of Xcode. Xcode comes with the machine image (DEVELOPER_DIR in the machine
# env selects one of several).
set -eu
version=$(xcodebuild -version | sed -n 's/^Xcode //p')
if [ -z "$version" ]; then
	echo "xcodebuild -version printed no Xcode version" >&2
	exit 1
fi
free_gb=$(df -g "$HOME" | awk 'NR == 2 { print $4 }')
if [ "$free_gb" -lt 20 ]; then
	echo "only $free_gb GB of free disk; at least 20 GB are necessary" >&2
	exit 1
fi
echo "$version"

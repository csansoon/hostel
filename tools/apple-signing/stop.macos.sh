#!/bin/sh
# Removes the lease's signing keychain from the search list and deletes it.
# A repeated stop (no keychain) does nothing. Cleanup then deletes the
# secrets with the room.
set -eu
kc="$HOSTEL_ROOM_DIR/signing.keychain-db"
[ -e "$kc" ] || exit 0
if [ -z "${HOSTEL_SIGNING_LOCKED:-}" ]; then
	HOSTEL_SIGNING_LOCKED=1 exec lockf -k -t 120 "$HOSTEL_CACHE_DIR/apple-signing.lock" /bin/sh "$0" "$@"
fi
list=$(security list-keychains -d user | sed -e 's/^[[:space:]]*"//' -e 's/"[[:space:]]*$//')
set --
while IFS= read -r k; do
	[ -n "$k" ] && [ "$k" != "$kc" ] && set -- "$@" "$k"
done <<LIST
$list
LIST
security list-keychains -d user -s "$@"
security delete-keychain "$kc" 2>/dev/null || rm -f "$kc"

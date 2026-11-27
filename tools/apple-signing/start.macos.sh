#!/bin/sh
# Makes a keychain for the lease with the team's signing identity, from the
# server's secrets (docs/m14-implementation.md), and gives builds the App Store
# Connect API key. Each room of a Mac changes the same keychain search list, so
# the keychain steps hold a lock. A repeated start makes the keychain again.
set -eu
if [ -z "${HOSTEL_SIGNING_LOCKED:-}" ]; then
	HOSTEL_SIGNING_LOCKED=1 exec lockf -k -t 120 "$HOSTEL_CACHE_DIR/apple-signing.lock" /bin/sh "$0" "$@"
fi
s="$HOSTEL_SECRETS_DIR"
kc="$HOSTEL_ROOM_DIR/signing.keychain-db"
for f in apple-signing-p12 apple-signing-p12-password asc-key asc-key-id asc-issuer-id; do
	[ -s "$s/$f" ] || { echo "the secret $f is missing or empty; an admin sets it with \"hostel config secrets set $f <file>\"" >&2; exit 1; }
done

# Apple's intermediate certificate (WWDR G3, public, from
# apple.com/certificateauthority); without it the identity is not valid.
wwdr='-----BEGIN CERTIFICATE-----
MIIEUTCCAzmgAwIBAgIQfK9pCiW3Of57m0R6wXjF7jANBgkqhkiG9w0BAQsFADBi
MQswCQYDVQQGEwJVUzETMBEGA1UEChMKQXBwbGUgSW5jLjEmMCQGA1UECxMdQXBw
bGUgQ2VydGlmaWNhdGlvbiBBdXRob3JpdHkxFjAUBgNVBAMTDUFwcGxlIFJvb3Qg
Q0EwHhcNMjAwMjE5MTgxMzQ3WhcNMzAwMjIwMDAwMDAwWjB1MUQwQgYDVQQDDDtB
cHBsZSBXb3JsZHdpZGUgRGV2ZWxvcGVyIFJlbGF0aW9ucyBDZXJ0aWZpY2F0aW9u
IEF1dGhvcml0eTELMAkGA1UECwwCRzMxEzARBgNVBAoMCkFwcGxlIEluYy4xCzAJ
BgNVBAYTAlVTMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA2PWJ/KhZ
C4fHTJEuLVaQ03gdpDDppUjvC0O/LYT7JF1FG+XrWTYSXFRknmxiLbTGl8rMPPbW
BpH85QKmHGq0edVny6zpPwcR4YS8Rx1mjjmi6LRJ7TrS4RBgeo6TjMrA2gzAg9Dj
+ZHWp4zIwXPirkbRYp2SqJBgN31ols2N4Pyb+ni743uvLRfdW/6AWSN1F7gSwe0b
5TTO/iK1nkmw5VW/j4SiPKi6xYaVFuQAyZ8D0MyzOhZ71gVcnetHrg21LYwOaU1A
0EtMOwSejSGxrC5DVDDOwYqGlJhL32oNP/77HK6XF8J4CjDgXx9UO0m3JQAaN4LS
VpelUkl8YDib7wIDAQABo4HvMIHsMBIGA1UdEwEB/wQIMAYBAf8CAQAwHwYDVR0j
BBgwFoAUK9BpR5R2Cf70a40uQKb3R01/CF4wRAYIKwYBBQUHAQEEODA2MDQGCCsG
AQUFBzABhihodHRwOi8vb2NzcC5hcHBsZS5jb20vb2NzcDAzLWFwcGxlcm9vdGNh
MC4GA1UdHwQnMCUwI6AhoB+GHWh0dHA6Ly9jcmwuYXBwbGUuY29tL3Jvb3QuY3Js
MB0GA1UdDgQWBBQJ/sAVkPmvZAqSErkmKGMMl+ynsjAOBgNVHQ8BAf8EBAMCAQYw
EAYKKoZIhvdjZAYCAQQCBQAwDQYJKoZIhvcNAQELBQADggEBAK1lE+j24IF3RAJH
Qr5fpTkg6mKp/cWQyXMT1Z6b0KoPjY3L7QHPbChAW8dVJEH4/M/BtSPp3Ozxb8qA
HXfCxGFJJWevD8o5Ja3T43rMMygNDi6hV0Bz+uZcrgZRKe3jhQxPYdwyFot30ETK
XXIDMUacrptAGvr04NM++i+MZp+XxFRZ79JI9AeZSWBZGcfdlNHAwWx/eCHvDOs7
bJmCS1JgOLU5gm3sUjFTvg+RTElJdI+mUcuER04ddSduvfnSXPN/wmwLCTbiZOTC
NwMUGdXqapSqqdv+9poIZ4vvK7iqF0mDr8/LvOnP6pVxsLRFoszlh6oKw0E6eVza
UDSdlTs=
-----END CERTIFICATE-----'

# The search list without this keychain and without keychains of ended leases.
others() {
	security list-keychains -d user | sed -e 's/^[[:space:]]*"//' -e 's/"[[:space:]]*$//' | while IFS= read -r k; do
		[ "$k" = "$kc" ] && continue
		case "$k" in */rooms/*/signing.keychain-db) [ -e "$k" ] || continue ;; esac
		printf '%s\n' "$k"
	done
}

if [ -e "$kc" ]; then
	security delete-keychain "$kc" 2>/dev/null || rm -f "$kc"
fi
pw=$(openssl rand -hex 24)
security create-keychain -p "$pw" "$kc"
security set-keychain-settings "$kc" # no auto-lock
security unlock-keychain -p "$pw" "$kc"
printf '%s\n' "$wwdr" > "$s/wwdr-g3.pem"
security import "$s/wwdr-g3.pem" -k "$kc" -t cert >/dev/null
# -x and no "-T /usr/bin/security": "security export" cannot give out the
# private key (with that -T it can, also with -x). The files go at once.
security import "$s/apple-signing-p12" -k "$kc" -f pkcs12 -x -T /usr/bin/codesign \
	-P "$(cat "$s/apple-signing-p12-password")" >/dev/null
rm -f "$s/apple-signing-p12" "$s/apple-signing-p12-password"
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$pw" "$kc" >/dev/null
list=$(others)
set --
while IFS= read -r k; do
	[ -n "$k" ] && set -- "$@" "$k"
done <<LIST
$list
LIST
security list-keychains -d user -s "$@" "$kc"

if ! security find-identity -v -p codesigning "$kc" | grep -q '^ *1) '; then
	echo "the certificate in the secret apple-signing-p12 is not a valid signing identity (expired or revoked?):" >&2
	security find-identity -p codesigning "$kc" >&2
	exit 1
fi
# The team ID is the OU of the identity's certificate (the intermediate has
# OU=G3).
security find-certificate -a -p "$kc" | awk -v d="$s" '/BEGIN CERTIFICATE/ { n++ } n { print > (d "/cert-" n ".pem") }'
team=$(for c in "$s"/cert-*.pem; do openssl x509 -in "$c" -noout -subject 2>/dev/null; done |
	sed -n 's/.*OU *= *\([A-Z0-9]\{10\}\).*/\1/p' | head -n 1)
key_id=$(tr -d '[:space:]' < "$s/asc-key-id")
# xcodebuild finds the key by this file name too.
cp "$s/asc-key" "$s/AuthKey_$key_id.p8"
{
	echo "HOSTEL_SIGNING_KEYCHAIN=$kc"
	# Each SSH session has its own security session: the unlock above does not
	# reach the jobs. A job unlocks the keychain with this random password.
	echo "HOSTEL_SIGNING_KEYCHAIN_PASSWORD=$pw"
	echo "HOSTEL_SIGNING_TEAM=$team"
	echo "ASC_KEY_ID=$key_id"
	echo "ASC_ISSUER_ID=$(tr -d '[:space:]' < "$s/asc-issuer-id")"
	echo "ASC_KEY_PATH=$s/AuthKey_$key_id.p8"
} > tool.env

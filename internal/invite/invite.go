// Package invite encodes the invite codes of "hostel connect" (design:
// multiplayer.md, section 4): the server's addresses, the hash of its TLS
// public key, and a one-time secret.
package invite

import (
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const prefix = "hostel1-"

const flagKey = 1

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// Code is the content of an invite code. KeyHash is the SHA-256 of the
// server's TLS SubjectPublicKeyInfo, or nil when the server has no own TLS
// (behind a proxy).
type Code struct {
	Addresses []string
	KeyHash   []byte
	Secret    []byte
}

// SecretSize is the size of the one-time secret.
const SecretSize = 16

// Encode returns the code as a string.
func Encode(c Code) (string, error) {
	if len(c.Secret) != SecretSize {
		return "", fmt.Errorf("invite secret must be %d bytes", SecretSize)
	}
	if c.KeyHash != nil && len(c.KeyHash) != 32 {
		return "", errors.New("invite key hash must be 32 bytes")
	}
	if len(c.Addresses) == 0 {
		return "", errors.New("invite needs at least one server address")
	}
	b := []byte{0}
	if c.KeyHash != nil {
		b[0] |= flagKey
		b = append(b, c.KeyHash...)
	}
	b = append(b, c.Secret...)
	for _, a := range c.Addresses {
		b = binary.AppendUvarint(b, uint64(len(a)))
		b = append(b, a...)
	}
	return prefix + strings.ToLower(enc.EncodeToString(b)), nil
}

// ErrInvalid is returned for a string that is not an invite code.
var ErrInvalid = errors.New("not a valid invite code (it starts with \"hostel1-\"; copy all of it)")

// Decode parses a code from Encode.
func Decode(s string) (Code, error) {
	var c Code
	body, ok := strings.CutPrefix(strings.TrimSpace(s), prefix)
	if !ok {
		return c, ErrInvalid
	}
	b, err := enc.DecodeString(strings.ToUpper(body))
	if err != nil || len(b) < 1 {
		return c, ErrInvalid
	}
	flags, b := b[0], b[1:]
	if flags&^flagKey != 0 {
		return c, ErrInvalid
	}
	if flags&flagKey != 0 {
		if len(b) < 32 {
			return c, ErrInvalid
		}
		c.KeyHash, b = b[:32], b[32:]
	}
	if len(b) < SecretSize {
		return c, ErrInvalid
	}
	c.Secret, b = b[:SecretSize], b[SecretSize:]
	for len(b) > 0 {
		n, k := binary.Uvarint(b)
		if k <= 0 || n == 0 || uint64(len(b)-k) < n {
			return c, ErrInvalid
		}
		c.Addresses = append(c.Addresses, string(b[k:k+int(n)]))
		b = b[k+int(n):]
	}
	if len(c.Addresses) == 0 {
		return c, ErrInvalid
	}
	return c, nil
}

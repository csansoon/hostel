// Command signrelease signs the SHA256SUMS of a release (m10d-implementation.md):
//
//	HOSTEL_RELEASE_KEY=<base64 Ed25519 seed> go run ./tools/signrelease <tag> <SHA256SUMS> > SHA256SUMS.sig
//
// The release workflow runs it; the key is the repository secret
// HOSTEL_RELEASE_KEY.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/csansoon/hostel/internal/update"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: signrelease <tag> <SHA256SUMS>")
		os.Exit(2)
	}
	tag, file := os.Args[1], os.Args[2]
	if _, ok := update.ParseTag(tag); !ok {
		fail("the tag %q is not vX.Y.Z", tag)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("HOSTEL_RELEASE_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		fail("HOSTEL_RELEASE_KEY is not a base64 Ed25519 seed")
	}
	sums, err := os.ReadFile(file)
	if err != nil {
		fail("%v", err)
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), update.SignedText(tag, sums))
	fmt.Println(base64.StdEncoding.EncodeToString(sig))
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "signrelease: "+format+"\n", a...)
	os.Exit(1)
}

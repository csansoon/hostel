package invite

// A unit test is allowed here (AGENTS.md): the encoding is small and
// deterministic, and E2E tests cannot cover all malformed codes.

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	secret := bytes.Repeat([]byte{9}, SecretSize)
	for _, c := range []Code{
		{Addresses: []string{"https://100.80.1.2:7410", "https://hostel.example.com"}, KeyHash: key, Secret: secret},
		{Addresses: []string{"https://hostel.example.com"}, Secret: secret},
	} {
		s, err := Encode(c)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s, "hostel1-") || s != strings.ToLower(s) {
			t.Fatalf("code %q", s)
		}
		got, err := Decode("  " + s + "\n")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.KeyHash, c.KeyHash) || !bytes.Equal(got.Secret, c.Secret) || strings.Join(got.Addresses, " ") != strings.Join(c.Addresses, " ") {
			t.Fatalf("round trip: %+v, want %+v", got, c)
		}
	}
}

func TestInvalid(t *testing.T) {
	good, _ := Encode(Code{Addresses: []string{"https://a"}, KeyHash: make([]byte, 32), Secret: make([]byte, SecretSize)})
	for _, s := range []string{"", "hostel1-", "hostel2-" + good[8:], good[:len(good)-3], good + "aaaa", "hostel1-!!", strings.Replace(good, "hostel1-a", "hostel1-e", 1)} {
		if _, err := Decode(s); err == nil {
			t.Errorf("Decode(%q) succeeded", s)
		}
	}
}

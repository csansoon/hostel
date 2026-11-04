package update

// A unit test (AGENTS.md): the version order is small, deterministic code
// that decides whether an update or a refusal happens; E2E tests use only a
// few versions.

import "testing"

func TestVersionOrder(t *testing.T) {
	for _, c := range []struct {
		have, want string
		older      bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.9", "v0.1.10", true},
		{"v0.9.0", "v1.0.0", true},
		{"v0.1.0", "v0.1.0-3-gabc123", true},
		{"v0.1.0-3-gabc123", "v0.1.0", false},
		{"v0.1.0-3-gabc123-dirty", "v0.1.1", true},
		{"v0.1.0-dirty", "v0.1.0", false},
		{"dev", "v0.1.0", false},
		{"v0.1.0", "dev", false},
		{"abc123", "v9.0.0", false},
		{"", "v0.1.0", false},
	} {
		if got := Older(c.have, c.want); got != c.older {
			t.Errorf("Older(%q, %q) = %v, want %v", c.have, c.want, got, c.older)
		}
	}
	if Newer("v0.2.0-1-gabc", "v0.1.0") {
		t.Error("a describe version is not a release tag")
	}
	if !Newer("v0.2.0", "v0.1.0-5-gabc") {
		t.Error("v0.2.0 is newer than v0.1.0-5-gabc")
	}
}

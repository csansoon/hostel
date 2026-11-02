package config

import (
	"strings"
	"testing"
)

// SetKey and RemoveTable edit TOML text in place for many layouts; the E2E
// tests use a few of them, so this unit test covers the others (AGENTS.md,
// unit tests).
func TestSetKey(t *testing.T) {
	doc := `idle_timeout = "10m"
intervals = { scheduler = "5s" }

# Home machines.
[providers.home]
kind = "static"

[providers.home.machines.mac]
os = "macos"
labels = ["a"]

[providers.home.machines.win]
os = "windows"
`
	cases := []struct {
		name  string
		path  []string
		value string
		want  string
	}{
		{"replace a top-level key", []string{"idle_timeout"}, `"20m"`, `idle_timeout = "20m"
intervals = { scheduler = "5s" }
`},
		{"add a top-level key before the tables", []string{"poll_timeout"}, `"1m"`, `poll_timeout = "1m"
# Home machines.
`},
		{"replace a key in a table", []string{"providers", "home", "machines", "mac", "labels"}, `["a", "b"]`, `os = "macos"
labels = ["a", "b"]
`},
		{"add a key to a table", []string{"providers", "home", "machines", "win", "labels"}, `["x"]`, `os = "windows"
labels = ["x"]
`},
		{"add a table at the end", []string{"providers", "home", "machines", "new", "os"}, `"linux"`, `os = "windows"

[providers.home.machines.new]
os = "linux"
`},
	}
	for _, c := range cases {
		out, err := SetKey([]byte(doc), c.path, c.value)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.Contains(string(out), c.want) {
			t.Errorf("%s: got\n%s\nwant it to contain\n%s", c.name, out, c.want)
		}
		if _, err := parseExprs(out); err != nil {
			t.Errorf("%s: the result is not valid TOML: %v", c.name, err)
		}
	}
	if _, err := SetKey([]byte(doc), []string{"intervals", "scheduler"}, `"1s"`); err == nil {
		t.Error("a key in an inline table: want an error")
	}

	out, ok, err := RemoveTable([]byte(doc), []string{"providers", "home", "machines", "mac"})
	if err != nil || !ok {
		t.Fatalf("remove: %v %v", ok, err)
	}
	want := `[providers.home]
kind = "static"

[providers.home.machines.win]
`
	if !strings.Contains(string(out), want) || strings.Contains(string(out), "macos") {
		t.Errorf("remove: got\n%s", out)
	}
	out, _, _ = RemoveTable([]byte(doc), []string{"providers", "home"})
	if strings.Contains(string(out), "Home machines") || strings.Contains(string(out), "providers") {
		t.Errorf("remove a provider with its machines and comment: got\n%s", out)
	}
}

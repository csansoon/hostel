package config

import "testing"

// SplitServerFile is a small text transformation with many layouts (comments,
// multi-line arrays, inline tables, tables); an E2E test covers one layout at
// the import, so this unit test covers the others (AGENTS.md, unit tests).
func TestSplitServerFile(t *testing.T) {
	in := `#:schema ./hostel.schema.json
# Hostel server.

server_id = "exe"
listen = "0.0.0.0:8000"
# The addresses that clients use.
public_urls = [
  "https://hostel.example.com",
]
idle_timeout = "20m"
intervals = { scheduler = "5s" }
gateway_listen = "127.0.0.1:7422"

# The home machines.
[providers.home]
kind = "static"

[providers.home.machines.macbook]
address = "hostel@localhost"
os = "macos"

[mise]
idiomatic_tools = []
`
	wantLocal := `#:schema ./hostel.schema.json
# Hostel server.

server_id = "exe"
listen = "0.0.0.0:8000"
# The addresses that clients use.
public_urls = [
  "https://hostel.example.com",
]
gateway_listen = "127.0.0.1:7422"

`
	wantDoc := `idle_timeout = "20m"
intervals = { scheduler = "5s" }
# The home machines.
[providers.home]
kind = "static"

[providers.home.machines.macbook]
address = "hostel@localhost"
os = "macos"

[mise]
idiomatic_tools = []
`
	local, doc, err := SplitServerFile([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(local) != wantLocal {
		t.Errorf("local:\n%s\nwant:\n%s", local, wantLocal)
	}
	if string(doc) != wantDoc {
		t.Errorf("document:\n%s\nwant:\n%s", doc, wantDoc)
	}
	if has, err := HasDocumentKeys(local); err != nil || has {
		t.Errorf("local part has document keys (%v)", err)
	}
}

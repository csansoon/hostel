//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/csansoon/hostel/internal/protocol"
)

const staticMachine = `
[providers.home]
kind = "static"

[providers.home.machines.box]
address = "hostel@127.0.0.1:2222"
os = "linux"
`

// TestServerStart: a valid configuration starts the server, and the API
// accepts only a valid token.
func TestServerStart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	token := e.initServer(serverHeader() + staticMachine)
	s := e.startServer(token)

	var info protocol.Info
	if code := s.get("/v1/info", token, &info); code != http.StatusOK {
		t.Fatalf("GET /v1/info: HTTP %d", code)
	}
	if !strings.HasPrefix(info.GatewayHostKey, "ssh-ed25519 ") || !strings.HasPrefix(info.ServerPublicKey, "ssh-ed25519 ") {
		t.Fatalf("info = %+v, want ed25519 keys", info)
	}
	if code := s.get("/v1/info", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/info without a token: HTTP %d, want 401", code)
	}
	if code := s.get("/v1/info", "hostel_wrong", nil); code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/info with a wrong token: HTTP %d, want 401", code)
	}
}

// TestConfigErrors is scenario 24: configuration errors stop the server with the
// file and the line or key path. "admin config check" gives the same errors.
func TestConfigErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		config    string
		platforms map[string]string // file below platforms_dir → content
		want      []string          // each must be in stderr; {dir} is the config directory
		// {line} in want is the line number of the first config line that
		// contains marker.
		marker string
	}{
		{
			name:   "unknown key",
			config: staticMachine + "colour = \"blue\"\n",
			marker: "colour",
			want:   []string{"{dir}/server.toml:{line}:1: unknown key \"providers.home.machines.box.colour\""},
		},
		{
			name:   "unknown platform",
			config: staticMachine + "platforms = [\"andriod\"]\n",
			want:   []string{"{dir}/server.toml: providers.home.machines.box.platforms[0]: unknown platform \"andriod\""},
		},
		{
			name:   "OS platform in platforms",
			config: staticMachine + "platforms = [\"linux\"]\n",
			want:   []string{"{dir}/server.toml: providers.home.machines.box.platforms[0]: \"linux\" is an OS platform"},
		},
		{
			name:   "static machine without os",
			config: "[providers.home]\nkind = \"static\"\n[providers.home.machines.box]\naddress = \"h\"\n",
			want:   []string{"{dir}/server.toml: providers.home.machines.box.os: required for static machines"},
		},
		{
			name: "external machine without os",
			config: "[providers.cloud]\nkind = \"external\"\ncommand = [\"/bin/false\"]\n" +
				"[providers.cloud.machines.small]\nspec = { image = \"x\" }\n",
			want: []string{"{dir}/server.toml: providers.cloud.machines.small.os: required for external machines"},
		},
		{
			name:      "OS platform directory with devices",
			config:    staticMachine,
			platforms: map[string]string{"linux/platform.toml": "description = \"Linux\"\ndevices = [\"x\"]\n"},
			want:      []string{"{dir}/platforms/linux/platform.toml:2:1: unknown key \"devices\""},
		},
		{
			name:   "device platform without prepare for a serving OS",
			config: staticMachine + "platforms = [\"fakedevice\"]\n",
			platforms: map[string]string{
				"fakedevice/platform.toml":       "devices = [\"phone\"]\n",
				"fakedevice/prepare.macos.sh":    "#!/bin/sh\n",
				"fakedevice/screenshot.linux.sh": "#!/bin/sh\n",
			},
			want: []string{"{dir}/server.toml: providers.home.machines.box.platforms[0]: platform \"fakedevice\" has no prepare script for linux; add {dir}/platforms/fakedevice/prepare.linux.sh"},
		},
		{
			name:   "number as a duration",
			config: staticMachine + "max_lease = 5\n",
			marker: "max_lease",
			want:   []string{"{dir}/server.toml:{line}:13: providers.home.machines.box.max_lease: wrong type: got a number, want a duration string"},
		},
		{
			name: "several errors at once",
			config: staticMachine + "platforms = [\"andriod\"]\n" +
				"[providers.home.machines.other]\naddress = \"h\"\n",
			want: []string{
				"{dir}/server.toml: providers.home.machines.box.platforms[0]: unknown platform \"andriod\"",
				"{dir}/server.toml: providers.home.machines.other.os: required for static machines",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			config := serverHeader() + c.config
			e.initServer(config)
			line := 0
			for i, l := range strings.Split(config, "\n") {
				if c.marker != "" && line == 0 && strings.Contains(l, c.marker) {
					line = i + 1
				}
			}
			for file, body := range c.platforms {
				e.writeFile(filepath.Join(e.configDir(), "platforms", file), body)
			}
			run := e.hostel("server", "run")
			check := e.hostel("admin", "config", "check")
			for _, r := range []result{run, check} {
				if r.code != 1 {
					t.Fatalf("want exit code 1: %s", r)
				}
				for _, w := range c.want {
					w = strings.ReplaceAll(w, "{dir}", e.configDir())
					w = strings.ReplaceAll(w, "{line}", fmt.Sprint(line))
					if !strings.Contains(r.stderr, w) {
						t.Fatalf("stderr does not contain %q: %s", w, r)
					}
				}
			}
			if !strings.Contains(run.stderr, "the server does not start") {
				t.Fatalf("server run: %s", run)
			}
		})
	}
}

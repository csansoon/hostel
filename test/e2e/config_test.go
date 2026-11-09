//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/csansoon/hostel/internal/protocol"
)

const staticTemplate = `
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
	e.initServer(serverHeader() + staticTemplate)
	s := e.startServer("")
	token := e.admin

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
		name   string
		config string
		want   []string // each must be in stderr; {dir} is the config directory
		// {line} in want is the line number of the first config line that
		// contains marker.
		marker string
	}{
		{
			name:   "unknown key",
			config: staticTemplate + "colour = \"blue\"\n",
			marker: "colour",
			want:   []string{"{dir}/server.toml:{line}:1: unknown key \"providers.home.machines.box.colour\""},
		},
		{
			name:   "unknown tool",
			config: staticTemplate + "tools = [\"andriod-sdk\"]\n",
			want:   []string{"{dir}/server.toml: providers.home.machines.box.tools[0]: unknown tool \"andriod-sdk\""},
		},
		{
			name:   "a mise tool without a version",
			config: staticTemplate + "tools = [\"node\"]\n",
			want:   []string{"providers.home.machines.box.tools[0]: unknown tool \"node\"", "a mise tool needs its version, for example \"node:22\""},
		},
		{
			name:   "an OS in tools",
			config: staticTemplate + "tools = [\"linux\"]\n",
			want:   []string{"{dir}/server.toml: providers.home.machines.box.tools[0]: \"linux\" is an OS"},
		},
		{
			name:   "a tool without scripts for the OS of the machine",
			config: staticTemplate + "tools = [\"windows-desktop\"]\n",
			want:   []string{"providers.home.machines.box.tools[0]: windows-desktop has no scripts for linux; it works on windows"},
		},
		{
			name:   "an option that a screen tool does not have",
			config: strings.Replace(staticTemplate, `os = "linux"`, `os = "macos"`, 1) + "tools = [\"ios-simulator:watch\"]\n",
			want:   []string{"providers.home.machines.box.tools[0]: \"ios-simulator:watch\": ios-simulator has no option \"watch\""},
		},
		{
			name:   "the words before M10g",
			config: staticTemplate + "max_rooms = 1\n",
			want:   []string{"max_rooms is now max_machines"},
		},
		{
			name:   "templates in a static provider",
			config: "[providers.home]\nkind = \"static\"\n\n[providers.home.templates.box]\nos = \"linux\"\n",
			want:   []string{"a static provider has real machines"},
		},
		{
			name:   "the keys before M10e",
			config: staticTemplate + "platforms = [\"ios\"]\nlabels = [\"desktop\"]\n",
			want: []string{
				"{dir}/server.toml: providers.home.machines.box.platforms: removed in M10e: list the tools of the machine instead",
				"{dir}/server.toml: providers.home.machines.box.labels: removed in M10e: labels are gone; list tools instead",
			},
		},
		{
			name:   "the tools map before M10e",
			config: staticTemplate + "tools = { jq = \"1.7.1\" }\n",
			want:   []string{"{dir}/server.toml: providers.home.machines.box.tools: the map form before M10e; tools is now a list"},
		},
		{
			name:   "static machine without os",
			config: "[providers.home]\nkind = \"static\"\n[providers.home.machines.box]\naddress = \"h\"\n",
			want:   []string{"{dir}/server.toml: providers.home.machines.box.os: required for static machines"},
		},
		{
			name: "external machine without os",
			config: "[providers.cloud]\nkind = \"external\"\ncommand = [\"/bin/false\"]\n" +
				"[providers.cloud.templates.small]\nspec = { image = \"x\" }\n",
			want: []string{"{dir}/server.toml: providers.cloud.templates.small.os: required for external machines"},
		},
		{
			name: "devbox without max_lifetime and size",
			config: "[providers.ns]\nkind = \"namespace\"\n" +
				"[providers.ns.templates.mac]\nspec = { product = \"devbox\", platform = \"macos\", image = \"tahoe-slim\" }\n",
			want: []string{
				"{dir}/server.toml: providers.ns.templates.mac.spec.size: required for a devbox",
				"{dir}/server.toml: providers.ns.templates.mac.max_lifetime: required for a devbox",
			},
		},
		{
			name:   "WebSocket gateway without its path",
			config: "gateway_public = \"wss://hostel.example.com\"\n" + staticTemplate,
			want:   []string{"{dir}/server.toml: gateway_public: \"wss://hostel.example.com\" must end with the path /v1/gateway"},
		},
		{
			name:   "number as a duration",
			config: staticTemplate + "max_lease = 5\n",
			marker: "max_lease",
			want:   []string{"{dir}/server.toml:{line}:13: providers.home.machines.box.max_lease: wrong type: got a number, want a duration string"},
		},
		{
			name: "several errors at once",
			config: staticTemplate + "tools = [\"andriod-sdk\"]\n" +
				"[providers.home.machines.other]\naddress = \"h\"\n",
			want: []string{
				"{dir}/server.toml: providers.home.machines.box.tools[0]: unknown tool \"andriod-sdk\"",
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

// TestShippedTools: the default tools of the binary (tools/) pass the
// configuration check for a static Mac with all of them; the needs add
// android-sdk, jdk, and xcode. Their scripts run only on real machines; the
// smoke tests run them there.
func TestShippedTools(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.initServer(serverHeader() + `
[providers.home]
kind = "static"

[providers.home.machines.macbook]
address = "hostel@macbook.local"
os = "macos"
tools = ["ios-simulator", "android-emulator", "macos-desktop", "jdk:21"]
`)
	r := e.hostel("admin", "config", "check")
	if r.code != 0 || !strings.Contains(r.stdout, "iphone-17-pro") || !strings.Contains(r.stdout, "# home/macbook: the needs of its tools add xcode, android-sdk") {
		t.Fatalf("config check of the default tools: %s", r)
	}
	if !strings.Contains(r.stdout, "[providers.home.machines.macbook]") || strings.Contains(r.stdout, "[providers.home.templates") {
		t.Fatalf("the effective configuration of a static provider: %s", r)
	}
	effective := filepath.Join(t.TempDir(), "effective.toml")
	// After the configuration, the output lists the default of each tool.
	document, _, _ := strings.Cut(r.stdout, "\n# tool ")
	if err := os.WriteFile(effective, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	if again := e.hostel("admin", "config", "check", "--config", effective); again.code != 0 {
		t.Fatalf("config check of the effective configuration: %s", again)
	}
}

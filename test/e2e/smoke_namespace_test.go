//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSmokeNamespace runs against the real Namespace account and costs money
// (a macOS devbox for about 10 minutes). It runs only with
// HOSTEL_SMOKE_NAMESPACE=1 ("make test-smoke-namespace", which builds the
// macOS helper first). The devbox CLI must be installed and logged in.
//
// A lease on a new macOS devbox: a command, the task marker, a desktop
// screenshot, kill -9 of the server and a restart during the lease, release.
// At the end no devbox of the test remains, also when the test fails.
// HOSTEL_SMOKE_OUT (optional) is a directory for the screenshot.
func TestSmokeNamespace(t *testing.T) {
	if os.Getenv("HOSTEL_SMOKE_NAMESPACE") != "1" {
		t.Skip("set HOSTEL_SMOKE_NAMESPACE=1 to run it against the real Namespace account (it costs money)")
	}
	// The plan allows one devbox. Another devbox (for example the owner's)
	// means: stop and ask the owner.
	if names := realDevboxes(t); len(names) > 0 {
		t.Fatalf("devboxes exist (%s); the test needs an account without devboxes", strings.Join(names, ", "))
	}
	dist, err := filepath.Abs("../../dist")
	if err != nil {
		t.Fatal(err)
	}
	platforms, err := filepath.Abs("../../platforms")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	// The CLI keeps its login in the real home; the key files go to the test
	// home.
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	login := filepath.Join("Library", "Application Support", "ns")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(e.home, login)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(realHome, login), filepath.Join(e.home, login)); err != nil {
		t.Fatal(err)
	}
	e.init()
	id := fmt.Sprintf("smoke%x", time.Now().UnixNano()&0xffffff)
	prefix := "hostel-" + id + "-"
	t.Cleanup(func() {
		for _, name := range realDevboxes(t) {
			if strings.HasPrefix(name, prefix) {
				t.Logf("delete the devbox %s that the test left", name)
				devboxCLI(t, "delete", name, "--force")
			}
		}
	})
	e.writeConfig(fmt.Sprintf(`server_id = %q
listen = "127.0.0.1:0"
gateway_listen = "127.0.0.1:0"
helper_dir = %q
platforms_dir = %q
mise = { agent_device = false }

[providers.ns]
kind = "namespace"
warm_running = "1m"

[providers.ns.machines.mac]
spec = { product = "devbox", platform = "macos", size = "m", image = "tahoe-slim" }
labels = ["desktop"]
max_lifetime = "3h"
`, id, dist, platforms))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())

	start := time.Now()
	for {
		r := e.hostel("acquire", "macos", "--require", "desktop", "--task", "smoke test", "--wait", "1m")
		if r.code == 0 {
			break
		}
		if r.code != 75 || time.Since(start) > 15*time.Minute {
			t.Fatalf("acquire: %s", r)
		}
	}
	t.Logf("lease ready after %s", time.Since(start).Round(time.Second))
	r := e.mustHostel("run", "--", "sw_vers -productVersion; id -un; ls $NAMESPACE_DEVBOX_TASKS_DIR")
	if lines := strings.Split(strings.TrimSpace(r.stdout), "\n"); len(lines) != 3 || lines[1] != "runner" || lines[2] != "hostel-a" {
		t.Fatalf("run on the devbox: %q", r.stdout)
	}
	shot := filepath.Join(t.TempDir(), "desktop.png")
	if dir := os.Getenv("HOSTEL_SMOKE_OUT"); dir != "" {
		shot = filepath.Join(dir, "smoke-desktop.png")
	}
	e.mustHostel("screenshot", "-o", shot)
	if data, err := os.ReadFile(shot); err != nil || !bytes.HasPrefix(data, []byte("\x89PNG")) || len(data) < 10000 {
		t.Fatalf("desktop screenshot: %d bytes, %v", len(data), err)
	}

	// A crash during the lease: the restarted server continues it.
	s = s.restartAfterKill()
	if r := e.mustHostel("run", "--", "echo after-restart"); r.stdout != "after-restart\n" {
		t.Fatalf("run after the restart: %q", r.stdout)
	}
	e.mustHostel("release")
	e.waitFor("the devbox is deleted", 10*time.Minute, func() bool {
		for _, name := range realDevboxes(t) {
			if strings.HasPrefix(name, prefix) {
				return false
			}
		}
		return true
	})
	t.Logf("total %s", time.Since(start).Round(time.Second))
}

// devboxCLI runs the real devbox CLI with the real home directory.
func devboxCLI(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "devbox", args...).Output()
	if err != nil {
		t.Errorf("devbox %s: %v", args[0], err)
	}
	return out
}

// realDevboxes returns the names of all devboxes of the account.
func realDevboxes(t *testing.T) []string {
	t.Helper()
	out := devboxCLI(t, "list", "-o", "json")
	if i := bytes.IndexByte(out, '['); i >= 0 {
		out = out[i:]
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		t.Fatalf("devbox list: %v", err)
	}
	var names []string
	for _, d := range list {
		names = append(names, d.Name)
	}
	return names
}

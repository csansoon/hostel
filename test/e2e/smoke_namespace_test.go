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
	e, s, prefix := namespaceDevboxSmoke(t, `warm_running = "1m"`, `
[providers.ns.templates.mac]
spec = { product = "devbox", platform = "macos", size = "m", image = "tahoe-slim" }
tools = ["macos-desktop"]
max_lifetime = "3h"
`, false)
	e.newRepo(defaultRepoFiles())

	start := time.Now()
	for {
		r := e.hostel("acquire", "macos-desktop", "--task", "smoke test", "--wait", "1m")
		if r.code == 0 {
			break
		}
		if r.code != 75 || time.Since(start) > 15*time.Minute {
			t.Fatalf("acquire: %s", r)
		}
	}
	t.Logf("lease ready after %s", time.Since(start).Round(time.Second))
	r := e.mustHostel("run", "--", "sw_vers -productVersion; id -un; ls $NAMESPACE_DEVBOX_TASKS_DIR")
	// The job's own task marker.
	if lines := strings.Split(strings.TrimSpace(r.stdout), "\n"); len(lines) != 3 || lines[1] != "runner" || !strings.HasPrefix(lines[2], "hostel-a.j-") {
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

// namespaceDevboxSmoke starts a server with a namespace provider "ns" (the
// given provider keys and machines) for a smoke test on the real account. It
// stops if a devbox exists (the plan allows one; another one can be the
// owner's), and deletes the test's devboxes at the end, also when the test
// fails. It returns the devbox name prefix of the test.
func namespaceDevboxSmoke(t *testing.T, providerKeys, templates string, agentDevice bool) (*env, *serverProc, string) {
	t.Helper()
	if names := realDevboxes(t); len(names) > 0 {
		t.Fatalf("devboxes exist (%s); the test needs an account without devboxes", strings.Join(names, ", "))
	}
	dist, err := filepath.Abs("../../dist")
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
mise = { agent_device = %t }

[providers.ns]
kind = "namespace"
%s
%s`, id, dist, agentDevice, providerKeys, templates))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	return e, s, prefix
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

// TestSmokeNamespaceInstance runs against the real Namespace account and costs
// money (a small Linux instance for a few minutes). It runs only with
// HOSTEL_SMOKE_NAMESPACE=1 ("make test-smoke-namespace"). The nsc CLI must be
// installed and logged in.
//
// A lease on a new instance in the default machine container: a command as the
// machine user, a tunnel to a guest server, kill -9 of the server and a restart
// during the lease, release. At the end no instance of the test remains, also
// when the test fails.
func TestSmokeNamespaceInstance(t *testing.T) {
	if os.Getenv("HOSTEL_SMOKE_NAMESPACE") != "1" {
		t.Skip("set HOSTEL_SMOKE_NAMESPACE=1 to run it against the real Namespace account (it costs money)")
	}
	// Instances that Hostel did not create mean: stop and ask the owner.
	if names := realInstances(t); len(names) > 0 {
		t.Fatalf("instances exist (%s); the test needs an account without instances", strings.Join(names, ", "))
	}
	dist, err := filepath.Abs("../../dist")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
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
		for _, i := range realInstanceList(t) {
			if strings.HasPrefix(i.Labels["hostel.name"], prefix) {
				t.Logf("destroy the instance %s that the test left", i.ID)
				nscCLI(t, "destroy", i.ID, "--force")
			}
		}
	})
	e.writeConfig(fmt.Sprintf(`server_id = %q
listen = "127.0.0.1:0"
gateway_listen = "127.0.0.1:0"
helper_dir = %q
mise = { agent_device = false }

[providers.ns]
kind = "namespace"
warm_running = "1m"

[providers.ns.templates.lx]
spec = { product = "instance", machine_type = "linux/amd64:2x4" }
`, id, dist))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	free := freePort(t)
	e.newRepo(map[string]string{
		"hostel.toml": "include = [\".env\"]\n\n[tunnels.api]\nlocal_port_env = \"PORT\"\n",
		".gitignore":  ".env\n",
		".env":        fmt.Sprintf("PORT=%d\n", free),
	})
	localHTTP(t, fmt.Sprintf("127.0.0.1:%d", free), "guest")

	start := time.Now()
	for {
		r := e.hostel("acquire", "--task", "smoke test", "--wait", "1m")
		if r.code == 0 {
			break
		}
		if r.code != 75 || time.Since(start) > 15*time.Minute {
			t.Fatalf("acquire: %s", r)
		}
	}
	t.Logf("lease ready after %s", time.Since(start).Round(time.Second))
	if r := e.mustHostel("run", "--", "id -un; . /etc/os-release; echo $ID"); r.stdout != "hostel\nubuntu\n" {
		t.Fatalf("run on the instance: %q", r.stdout)
	}
	// The default image has no nc; bash has /dev/tcp.
	tunnel := `exec 3<>/dev/tcp/127.0.0.1/$HOSTEL_TUNNEL_API_PORT; printf "GET /smoke HTTP/1.0\r\n\r\n" >&3; cat <&3`
	if r := e.mustHostel("run", "--", "bash -c '"+tunnel+"'"); !strings.Contains(r.stdout, "hello from guest /smoke") {
		t.Fatalf("tunnel from the instance: %q", r.stdout)
	}

	s = s.restartAfterKill()
	if r := e.mustHostel("run", "--", "echo after-restart"); r.stdout != "after-restart\n" {
		t.Fatalf("run after the restart: %q", r.stdout)
	}
	e.mustHostel("release")
	e.waitFor("the instance is destroyed", 10*time.Minute, func() bool {
		for _, i := range realInstanceList(t) {
			if strings.HasPrefix(i.Labels["hostel.name"], prefix) {
				return false
			}
		}
		return true
	})
	t.Logf("total %s", time.Since(start).Round(time.Second))
}

// nscCLI runs the real nsc CLI with the real home directory.
func nscCLI(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nsc", args...).Output()
	if err != nil {
		t.Errorf("nsc %s: %v", args[0], err)
	}
	return out
}

type realInstance struct {
	ID     string            `json:"cluster_id"`
	Labels map[string]string `json:"labels"`
}

func realInstanceList(t *testing.T) []realInstance {
	t.Helper()
	var list []realInstance
	if err := json.Unmarshal(bytes.TrimSpace(nscCLI(t, "list", "-o", "json")), &list); err != nil {
		t.Fatalf("nsc list: %v", err)
	}
	return list
}

// realInstances returns the IDs of all instances of the account.
func realInstances(t *testing.T) []string {
	t.Helper()
	var ids []string
	for _, i := range realInstanceList(t) {
		ids = append(ids, i.ID)
	}
	return ids
}

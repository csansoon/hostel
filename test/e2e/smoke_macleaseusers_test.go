//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// TestSmokeMacLeaseUsers (M11) runs against a real Mac with the root step of
// lease users (machine-setup.md, "Lease users"). It runs only with
// HOSTEL_SMOKE_MAC=<user@host> and HOSTEL_SMOKE_MAC_KEY, the private key that
// the machine user accepts and whose public key is in
// /etc/hostel/server_key.pub; it becomes the test server's key. The machine
// must have no other lease during the test.
//
// A lease runs as its own user, cannot read the machine user's rooms or write
// its cache; its backdoors (a process without HOSTEL_LEASE, a crontab entry, a
// file in /tmp) are gone after the release, with the user.
func TestSmokeMacLeaseUsers(t *testing.T) {
	addr := os.Getenv("HOSTEL_SMOKE_MAC")
	if addr == "" {
		t.Skip("set HOSTEL_SMOKE_MAC=<user@host> and HOSTEL_SMOKE_MAC_KEY to run it against a Mac")
	}
	keyFile := os.Getenv("HOSTEL_SMOKE_MAC_KEY")
	key, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read HOSTEL_SMOKE_MAC_KEY: %v", err)
	}
	pub, err := os.ReadFile(keyFile + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	dist, err := filepath.Abs("../../dist")
	if err != nil {
		t.Fatal(err)
	}
	// machineSh runs a command as the machine user, outside Hostel.
	machineSh := func(cmd string) string {
		t.Helper()
		out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-i", keyFile, addr, cmd).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	e := newEnv(t)
	e.writeFile(filepath.Join(e.dataDir(), "server_ed25519"), string(key))
	e.writeFile(filepath.Join(e.dataDir(), "server_ed25519.pub"), string(pub))
	if err := os.Chmod(filepath.Join(e.dataDir(), "server_ed25519"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.init()
	cfg := serverHeader() + fmt.Sprintf(`
[providers.home]
kind = "static"

[providers.home.machines.mac]
address = %q
os = "macos"
tools = ["xcode"]
lease_users = true
`, addr)
	e.writeConfig(strings.Replace(cfg, fmt.Sprintf("helper_dir = %q", helperDir), fmt.Sprintf("helper_dir = %q", dist), 1))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	e.newRepo(map[string]string{"a.txt": "one\n"})

	start := time.Now()
	var lease string
	for {
		r := e.hostel("acquire", "xcode", "--task", "mac lease user smoke test", "--wait", "1m")
		if r.code == 0 {
			lease = leaseIDRE.FindStringSubmatch(r.stdout + r.stderr)[1]
			break
		}
		if r.code != 75 || time.Since(start) > 15*time.Minute {
			t.Fatalf("acquire: %s", r)
		}
	}
	t.Logf("lease ready after %s", time.Since(start).Round(time.Second))
	user := "hostel-" + lease[:12]
	if r := e.mustHostel("run", "--", "id -un; echo $HOME; xcodebuild -version | head -1"); !strings.HasPrefix(r.stdout, user+"\n/Users/"+user+"\nXcode ") {
		t.Fatalf("user, home, and Xcode of the lease: %q", r.stdout)
	}
	if r := e.mustHostel("run", "--", "(echo x > /Users/hostel/hostel/cache/m11-door) 2>/dev/null || echo refused; "+
		"ls /Users/hostel/hostel/rooms >/dev/null 2>&1 || echo hidden"); r.stdout != "refused\nhidden\n" {
		t.Fatalf("machine cache and rooms seen by the lease user: %q", r.stdout)
	}
	e.mustHostel("run", "--", "nohup env -i /bin/sleep 1005 > /dev/null 2>&1 < /dev/null & "+
		"echo '* * * * * true door' | crontab - && touch /tmp/door-$(id -un)")
	e.mustHostel("release")
	e.waitFor("the Mac free", 5*time.Minute, func() bool {
		r := s.machine("home/mac")
		if r.State == protocol.MachineBroken {
			t.Fatalf("the Mac is broken: %s", r.Reason)
		}
		return r.State == protocol.MachineRunning && len(r.Rooms) == 1 && r.Rooms[0].State == protocol.RoomFree
	})
	got := machineSh("id " + user + " >/dev/null 2>&1 && echo exists; ls -d /Users/" + user + " /tmp/door-" + user + " 2>/dev/null; " +
		"pgrep -f '^/bin/sleep 1005' || true; sudo -n /etc/hostel/hostel lease-user list; dscl . -read /Groups/hostel-leases GroupMembership")
	if strings.Contains(got, "exists") || strings.Contains(got, "/Users/"+user) || strings.Contains(got, "door") || !strings.Contains(got, `{"users":null}`) ||
		strings.Contains(strings.SplitN(got, "GroupMembership:", 2)[1], user) {
		t.Fatalf("after the release (user, home, /tmp, processes, lease users, group):\n%s", got)
	}
}

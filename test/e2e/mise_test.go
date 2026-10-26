//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMise is scenario 28: a room without mise gets the Hostel mise binary;
// machine-level tools are installed at room provisioning and platform scripts
// can use them; a repository mise.toml with an [env] value and a tool makes
// both available in "hostel run"; a second lease does not download the tool
// again. HOSTEL_E2E_OFFLINE=1 skips the downloads.
func TestMise(t *testing.T) {
	t.Parallel()
	offline := os.Getenv("HOSTEL_E2E_OFFLINE") == "1"
	machineKeys, repoTools := "tools = { jq = \"1.7.1\" }\n", "[tools]\nripgrep = \"14.1.1\"\n"
	// The setup uses a tool that the same setup job installs.
	repoConfig := "[platforms.fakedevice]\nsetup = [\"rg --version | head -1 > setup-rg.txt\"]\n"
	if offline {
		machineKeys, repoTools, repoConfig = "", "", ""
	}
	files := map[string]string{
		"mise.toml":   repoTools + "[env]\nREPO_MISE_VAR = \"from-mise\"\n",
		"hostel.toml": repoConfig,
		"a.txt":       "hello\n",
	}
	e := newEnv(t)
	e.init()
	if got, _ := docker("run", "--rm", "--label", dockerLabel+"="+e.runID, "--entrypoint", "sh", roomImage, "-c", "command -v mise || echo none"); got != "none" {
		t.Fatalf("the room image has mise: %q", got)
	}
	f := newFixtureWithEnv(t, e, &fixtureOpts{machineKeys: machineKeys, files: files, agentDevice: !offline,
		platforms: map[string]map[string]string{"linux": {"health.linux.sh": "#!/bin/sh\njq --version > \"$HOSTEL_CACHE_DIR/jq-from-health\" 2>&1 || true\n"}}})
	if got := f.c.sh("/home/hostel/hostel/bin/mise --version | cut -d' ' -f1"); got != "2026.10.17" {
		t.Fatalf("mise on the room: %q", got)
	}
	if !offline {
		// Installed at provisioning, before a lease; a platform script used it.
		if got := f.c.sh("cat /home/hostel/hostel/cache/jq-from-health"); got != "jq-1.7.1" {
			t.Fatalf("machine-level tool in a platform script: %q", got)
		}
	}
	f.acquire()
	r := f.e.mustHostel("run", "--", "echo $REPO_MISE_VAR")
	if r.stdout != "from-mise\n" {
		t.Fatalf("mise [env] in run: %q", r.stdout)
	}
	// [mise] in server.toml sets only agent_device; idiomatic_tools keeps its default.
	if r := f.e.mustHostel("run", "--", "echo $MISE_IDIOMATIC_VERSION_FILE_ENABLE_TOOLS"); r.stdout != "node,python\n" {
		t.Fatalf("idiomatic tools in run: %q", r.stdout)
	}
	if offline {
		return
	}
	if got := f.work("cat setup-rg.txt"); !strings.HasPrefix(got, "ripgrep 14.1.1") {
		t.Fatalf("tool in the setup: %q", got)
	}
	// agent-device is on each room, with its own Node; the job PATH has no Node.
	if r := f.e.mustHostel("run", "--", "agent-device --version; echo $AGENT_DEVICE_STATE_DIR; command -v node || echo no-node"); r.stdout != "0.21.25\n/home/hostel/hostel/slots/a/agent-device\nno-node\n" {
		t.Fatalf("agent-device in run: %q", r.stdout)
	}
	if r := f.e.mustHostel("run", "--", "rg --version | head -1; jq --version"); !strings.HasPrefix(r.stdout, "ripgrep 14.1.1") || !strings.HasSuffix(r.stdout, "jq-1.7.1\n") {
		t.Fatalf("tools in run: %q", r.stdout)
	}
	installed := "stat -c %Y /home/hostel/hostel/cache/mise/installs/ripgrep/14.1.1"
	before := f.c.sh(installed)
	f.e.mustHostel("release")
	f.waitSlotFree()
	r = f.e.mustHostel("acquire", "fakedevice", "--task", "second")
	if strings.Contains(r.stderr, "ripgrep-14.1.1") {
		t.Fatalf("the second lease downloaded the tool again: %s", r)
	}
	if after := f.c.sh(installed); after != before {
		t.Fatalf("the tool install changed: %s → %s", before, after)
	}
	if r := f.e.mustHostel("run", "--", "rg --version | head -1"); !strings.HasPrefix(r.stdout, "ripgrep 14.1.1") {
		t.Fatalf("tool in the second lease: %q", r.stdout)
	}
	// With agent_device = false and no machine-level tools, the next
	// provisioning removes the command.
	f.e.mustHostel("release")
	f.waitSlotFree()
	cfg, err := os.ReadFile(filepath.Join(f.e.configDir(), "server.toml"))
	if err != nil {
		t.Fatal(err)
	}
	off := strings.Replace(string(cfg), "agent_device = true", "agent_device = false", 1)
	f.e.writeConfig(strings.Replace(off, machineKeys, "", 1))
	f.s = f.s.restart()
	f.e.waitFor("agent-device removed", time.Minute, func() bool {
		return f.c.sh("test -e /home/hostel/hostel/bin/agent-device && echo found || echo removed") == "removed"
	})
}

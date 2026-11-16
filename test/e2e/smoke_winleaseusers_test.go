//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// TestSmokeWindowsLeaseUsers runs against a real Windows machine with the
// service of lease users (machine-setup.md, "Lease users"; m11w-implementation.md),
// as TestSmokeWindows (HOSTEL_SMOKE_WINDOWS, HOSTEL_SMOKE_WINDOWS_KEY). The key's
// public key must be the one that "hostel machine join --lease-users" wrote. The
// machine keeps windows-desktop, whose files the test server writes; the lease
// does not use it, so it gets a lease user.
//
// A lease runs as its own user and cannot write the machine user's cache or
// read its rooms; its backdoors (a scheduled task, a Run value, files in
// C:\ProgramData and at the top of the system drive, a process) are gone after
// the release, with the user and its profile.
func TestSmokeWindowsLeaseUsers(t *testing.T) {
	e, s := windowsSmoke(t, true, "tools = [\"windows-desktop\"]\nlease_users = true\n", func(int) map[string]string {
		return map[string]string{"a.txt": "one\n"}
	})
	addr, keyFile := os.Getenv("HOSTEL_SMOKE_WINDOWS"), os.Getenv("HOSTEL_SMOKE_WINDOWS_KEY")
	// machineSh runs a PowerShell command as the machine user, outside Hostel.
	machineSh := func(cmd string) string {
		t.Helper()
		out, err := exec.Command("ssh", "-o", "BatchMode=yes", "-i", keyFile, addr, cmd).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	start := time.Now()
	var lease string
	for {
		r := e.hostel("acquire", "--task", "windows lease user smoke test", "--wait", "1m")
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
	if r := e.mustHostel("run", "--", `"$(whoami)|$env:USERPROFILE"`); !strings.HasSuffix(strings.TrimSpace(r.stdout), `\`+user+`|C:\Users\`+user) {
		t.Fatalf("user and home of the lease: %q", r.stdout)
	}
	if r := e.mustHostel("run", "--", `try { 'x' | Set-Content C:\Users\hostel\hostel\cache\door.txt -ErrorAction Stop; 'written' } catch { 'refused' }
try { Get-ChildItem C:\Users\hostel\hostel\rooms -ErrorAction Stop | Out-Null; 'seen' } catch { 'hidden' }`); strings.Fields(r.stdout)[0] != "refused" || strings.Fields(r.stdout)[1] != "hidden" {
		t.Fatalf("machine cache and rooms seen by the lease user: %q", r.stdout)
	}
	e.mustHostel("run", "--", `$u = $env:USERNAME
'x' | Set-Content "C:\ProgramData\door-$u.txt"
New-Item -ItemType Directory "C:\door-$u" | Out-Null
schtasks /create /tn "door-$u" /tr "cmd /c echo door" /sc daily /st 03:00 /f | Out-Null
Set-ItemProperty HKCU:\Software\Microsoft\Windows\CurrentVersion\Run door 'cmd /c echo door'
Start-Process -WindowStyle Hidden powershell -ArgumentList '-NoProfile', '-Command', 'Start-Sleep 1005'`)
	e.mustHostel("release")
	e.waitFor("the Windows machine free", 5*time.Minute, func() bool {
		r := s.machine("home/win")
		if r.State == protocol.MachineBroken {
			t.Fatalf("the Windows machine is broken: %s", r.Reason)
		}
		return r.State == protocol.MachineRunning && len(r.Rooms) == 1 && r.Rooms[0].State == protocol.RoomFree
	})
	got := machineSh(`if (Get-LocalUser | Where-Object Name -eq '` + user + `') { 'user exists' }
Test-Path 'C:\Users\` + user + `', 'C:\ProgramData\door-` + user + `.txt', 'C:\door-` + user + `'
& C:\Users\hostel\hostel\bin\hostel.exe lease-user list`)
	if strings.Contains(got, "exists") || strings.Contains(got, "True") || !strings.Contains(got, `{"users":null}`) {
		t.Fatalf("after the release (user, profile, files, lease users):\n%s", got)
	}
}

//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"slices"
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
// C:\ProgramData, at the top of the system drive, and in a folder there that
// another account made, a process) are gone after
// the release, with the user and its profile. The Run value goes with the
// profile's hive. The machine user cannot see the lease user's task: with
// HOSTEL_SMOKE_WINDOWS_ADMIN (an SSH destination of an administrator whose
// shell is PowerShell, with keys from the SSH agent or config), the test
// checks the task by its name before and after the release.
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
	// A folder at the top of the system drive that another account made: the
	// drive's access list lets all users write in it.
	machineSh(`New-Item -ItemType Directory -Force C:\hostel-smoke-shared | Out-Null`)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-i", keyFile, addr, `Remove-Item -Recurse -Force C:\hostel-smoke-shared`).Run()
	})
	e.mustHostel("run", "--", `$u = $env:USERNAME
'x' | Set-Content "C:\ProgramData\door-$u.txt"
'x' | Set-Content "C:\hostel-smoke-shared\door-$u.txt"
New-Item -ItemType Directory "C:\door-$u" | Out-Null
schtasks /create /tn "door-$u" /tr "cmd /c echo door" /sc daily /st 03:00 /f | Out-Null
Set-ItemProperty HKCU:\Software\Microsoft\Windows\CurrentVersion\Run door 'cmd /c echo door'`)
	adminAddr := os.Getenv("HOSTEL_SMOKE_WINDOWS_ADMIN")
	taskExists := func() bool {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", adminAddr,
			`schtasks /query /tn "door-`+user+`" 2>&1 | Out-Null; if ($LASTEXITCODE -eq 0) { 'task-exists' } else { 'no-task' }; exit 0`).CombinedOutput()
		if err != nil {
			t.Fatalf("query the task as the administrator: %v\n%s", err, out)
		}
		return strings.Contains(string(out), "task-exists")
	}
	if adminAddr == "" {
		t.Log("HOSTEL_SMOKE_WINDOWS_ADMIN is not set: the task is checked only by the deletion")
	} else if !taskExists() {
		t.Fatalf("the administrator does not see the task door-%s before the release", user)
	}
	// A detached job stays until the release; the machine user sees its process
	// (Get-Process lists the processes of all users), and its name guards
	// against a PID that Windows gives again after the release.
	e.mustHostel("run", "--detach", "--", `$PID | Set-Content $HOME\door.pid; Start-Sleep 1005`)
	var pid string
	e.waitFor("the detached job's process", time.Minute, func() bool {
		pid = strings.TrimSpace(e.hostel("run", "--", `if (Test-Path $HOME\door.pid) { Get-Content $HOME\door.pid }`).stdout)
		return pid != ""
	})
	procName := machineSh(`(Get-Process -Id ` + pid + ` -ErrorAction SilentlyContinue).ProcessName`)
	if procName == "" {
		t.Fatalf("the machine user does not see the process %s of the lease before the release", pid)
	}
	alive := `if (Get-Process -Id ` + pid + ` -ErrorAction SilentlyContinue | Where-Object ProcessName -eq '` + procName + `') { 'process alive' }`
	e.mustHostel("release")
	e.waitFor("the Windows machine free", 5*time.Minute, func() bool {
		r := s.machine("home/win")
		if r.State == protocol.MachineBroken {
			t.Fatalf("the Windows machine is broken: %s", r.Reason)
		}
		return r.State == protocol.MachineRunning && len(r.Rooms) == 1 && r.Rooms[0].State == protocol.RoomFree
	})
	got := machineSh(alive + `
if (Get-LocalUser | Where-Object Name -eq '` + user + `') { 'user exists' }
Test-Path 'C:\Users\` + user + `', 'C:\ProgramData\door-` + user + `.txt', 'C:\door-` + user + `', 'C:\hostel-smoke-shared\door-` + user + `.txt'
& C:\Users\hostel\hostel\bin\hostel.exe lease-user list`)
	if strings.Contains(got, "alive") || strings.Contains(got, "exists") || strings.Contains(got, "True") || !strings.Contains(got, `{"users":null}`) {
		t.Fatalf("after the release (process, user, profile, files, lease users):\n%s", got)
	}
	if adminAddr != "" && taskExists() {
		t.Fatalf("the task door-%s is still there after the release", user)
	}
}

// TestSmokeWindowsCleanupAudit runs against a real Windows machine, as
// TestSmokeWindows. A lease on the machine user leaves a Run value, a user
// environment variable, a file in the Startup folder, a PowerShell profile, and
// a scheduled task; after the release they are gone, and the event log names
// them.
func TestSmokeWindowsCleanupAudit(t *testing.T) {
	e, s := windowsSmoke(t, true, "tools = [\"windows-desktop\"]\n", func(int) map[string]string {
		return map[string]string{"a.txt": "one\n"}
	})
	addr, keyFile := os.Getenv("HOSTEL_SMOKE_WINDOWS"), os.Getenv("HOSTEL_SMOKE_WINDOWS_KEY")
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
		r := e.hostel("acquire", "--task", "windows cleanup audit smoke test", "--wait", "1m")
		if r.code == 0 {
			lease = leaseIDRE.FindStringSubmatch(r.stdout + r.stderr)[1]
			break
		}
		if r.code != 75 || time.Since(start) > 15*time.Minute {
			t.Fatalf("acquire: %s", r)
		}
	}
	e.mustHostel("run", "--", `Set-ItemProperty HKCU:\Software\Microsoft\Windows\CurrentVersion\Run door 'cmd /c echo door'
Set-ItemProperty HKCU:\Environment HOSTEL_DOOR door
'echo door' | Set-Content "$HOME\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup\door.cmd"
New-Item -ItemType Directory -Force "$HOME\Documents\WindowsPowerShell" | Out-Null
'# door' | Add-Content "$HOME\Documents\WindowsPowerShell\profile.ps1"
schtasks /create /tn door-audit /tr "cmd /c echo door" /sc daily /st 03:00 /f | Out-Null`)
	e.mustHostel("release")
	e.waitFor("the Windows machine free", 5*time.Minute, func() bool {
		r := s.machine("home/win")
		if r.State == protocol.MachineBroken {
			t.Fatalf("the Windows machine is broken: %s", r.Reason)
		}
		return r.State == protocol.MachineRunning && len(r.Rooms) == 1 && r.Rooms[0].State == protocol.RoomFree
	})
	got := machineSh(`(Get-ItemProperty HKCU:\Software\Microsoft\Windows\CurrentVersion\Run).door
(Get-ItemProperty HKCU:\Environment).HOSTEL_DOOR
Test-Path "$HOME\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup\door.cmd"
if (Test-Path "$HOME\Documents\WindowsPowerShell\profile.ps1") { Select-String -Quiet door "$HOME\Documents\WindowsPowerShell\profile.ps1" }
schtasks.exe /query /tn door-audit 2>&1 | Out-Null; if ($LASTEXITCODE -eq 0) { 'task door-audit' }
exit 0`)
	if strings.Contains(got, "door") || strings.Contains(got, "True") {
		t.Fatalf("after the release (Run value, environment, Startup file, profile, task):\n%s", got)
	}
	var d protocol.MachineDetails
	if code := s.get("/v1/machines/home/win", s.token, &d); code != 200 {
		t.Fatalf("GET /v1/machines/home/win: HTTP %d", code)
	}
	for _, item := range []string{`HKCU\Software\Microsoft\Windows\CurrentVersion\Run\door`, `HKCU\Environment\HOSTEL_DOOR`,
		"~/AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/door.cmd", "~/Documents/WindowsPowerShell", `scheduled task \door-audit`} {
		if !slices.ContainsFunc(d.Events, func(ev protocol.Event) bool {
			return ev.LeaseID == lease && strings.HasPrefix(ev.Message, "removed persistence of the lease: ") && strings.Contains(ev.Message, item)
		}) {
			t.Fatalf("no event names %q: %+v", item, d.Events)
		}
	}
}

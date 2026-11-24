package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// The root step of lease users on Windows (m11w-implementation.md): the
// service HostelLeaseUsers runs as SYSTEM and does the lease-user operations
// for the machine user, which asks through a named pipe that only it can open.
// "hostel lease-user ..." of the machine user is the pipe's client. As on
// macOS and Linux, the service takes only a user name of a fixed form from the
// caller, and it follows no junction or link of a user.

const (
	leaseService = "HostelLeaseUsers"
	// deleteTimeout bounds one deletion (the search of the shared
	// directories); opTimeout, one request with a deletion and a creation.
	deleteTimeout = 10 * time.Minute
	opTimeout     = 25 * time.Minute
	pipeTimeout   = 30 * time.Second
	leasePipe     = `\\.\pipe\hostel-lease-user`
	// leaseRootDir has the service's binary and the server key; only SYSTEM
	// and Administrators can write it.
	leaseRootDir = `C:\Program Files\Hostel`
	profileList  = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`
	// pipeAccess is what the machine user gets on the pipe and what the
	// client asks for: read and write data, read its security descriptor.
	pipeAccess = windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.READ_CONTROL | windows.SYNCHRONIZE
)

var (
	procImpersonateNamedPipeClient = windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")
	userenv                        = windows.NewLazySystemDLL("userenv.dll")
	procCreateProfile              = userenv.NewProc("CreateProfile")
	procDeleteProfile              = userenv.NewProc("DeleteProfileW")
	procNetUserDel                 = windows.NewLazySystemDLL("netapi32.dll").NewProc("NetUserDel")
	procNtUnloadKey2               = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtUnloadKey2")
	procCancelSynchronousIo        = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")
)

// afterAnswer runs after the answer to the current request (the restarts of
// a remote update).
var afterAnswer func()

type leaseRequest struct {
	Op   string `json:"op"`
	Name string `json:"name,omitempty"`
}

type leaseAnswer struct {
	Out   json.RawMessage `json:"out,omitempty"`
	Error string          `json:"error,omitempty"`
}

func leaseUserWindows(args []string, stdout, stderr io.Writer) int {
	var req leaseRequest
	switch {
	case len(args) == 1 && args[0] == "serve":
		return serveLeaseUsers(stderr)
	case len(args) == 1 && (args[0] == "check" || args[0] == "list"):
		req.Op = args[0]
	case len(args) == 2 && (args[0] == "create" || args[0] == "delete"):
		req = leaseRequest{Op: args[0], Name: args[1]}
	default:
		fmt.Fprintln(stderr, "hostel: usage: hostel lease-user check | list | create <name> | delete <name> (as the machine user; the service "+leaseService+" does them)")
		return 2
	}
	out, err := callLeaseService(req)
	if err != nil {
		fmt.Fprintln(stderr, "hostel: lease-user:", err)
		return 1
	}
	stdout.Write(append(out, '\n'))
	return 0
}

// pipeConn reads and writes a pipe handle that the caller closes.
type pipeConn windows.Handle

func (p pipeConn) Read(b []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(windows.Handle(p), b, &n, nil)
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
		return int(n), io.EOF
	}
	return int(n), err
}

func (p pipeConn) Write(b []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(windows.Handle(p), b, &n, nil)
	return int(n), err
}

// callLeaseService sends one request to the service and returns its output.
func callLeaseService(req leaseRequest) (json.RawMessage, error) {
	name, err := windows.UTF16PtrFromString(leasePipe)
	if err != nil {
		return nil, err
	}
	// The service does one request at a time; a deletion can take minutes.
	// The wait for the pipe and the answer have their own deadlines.
	deadline := time.Now().Add(opTimeout)
	var h windows.Handle
	for {
		// Identification only: the service learns who calls, and cannot act
		// as the caller.
		h, err = windows.CreateFile(name, pipeAccess, 0, nil, windows.OPEN_EXISTING,
			windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
		if err == nil {
			break
		}
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, fmt.Errorf("the service %s does not run: run \"hostel machine update\" as an administrator once (or \"hostel machine join --lease-users\")", leaseService)
		}
		if !errors.Is(err, windows.ERROR_PIPE_BUSY) || time.Now().After(deadline) {
			return nil, fmt.Errorf("open %s: %w", leasePipe, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer windows.CloseHandle(h)
	// Only SYSTEM can make a pipe that SYSTEM owns: a process that takes the
	// name while the service is stopped cannot answer for it.
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("the owner of %s: %w", leasePipe, err)
	}
	if owner, _, err := sd.Owner(); err != nil || !owner.IsWellKnown(windows.WinLocalSystemSid) {
		return nil, fmt.Errorf("%s does not belong to SYSTEM (owner %v): it is not the service %s", leasePipe, owner, leaseService)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := withTimeout(pipeTimeout, func() error {
		_, err := pipeConn(h).Write(append(data, '\n'))
		return err
	}); err != nil {
		return nil, fmt.Errorf("write to %s: %w", leasePipe, err)
	}
	var ans leaseAnswer
	if err := withTimeout(opTimeout+pipeTimeout, func() error {
		return json.NewDecoder(io.LimitReader(pipeConn(h), 1<<20)).Decode(&ans)
	}); err != nil {
		return nil, fmt.Errorf("the answer of the service %s: %w", leaseService, err)
	}
	if ans.Error != "" {
		return nil, errors.New(ans.Error)
	}
	return ans.Out, nil
}

func serveLeaseUsers(stderr io.Writer) int {
	if ok, err := svc.IsWindowsService(); err != nil || !ok {
		fmt.Fprintln(stderr, "hostel: lease-user serve runs only as the Windows service "+leaseService)
		return 2
	}
	if err := svc.Run(leaseService, leaseHandler{}); err != nil {
		return 1
	}
	return 0
}

type leaseHandler struct{}

func (leaseHandler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	// Backup and restore: SYSTEM reads, lists, and deletes a lease user's
	// files also when the user's access lists leave SYSTEM out.
	// Take ownership and debug: SYSTEM reads the user of each process
	// (processOf).
	if err := enablePrivileges("SeBackupPrivilege", "SeRestorePrivilege", "SeTakeOwnershipPrivilege", "SeDebugPrivilege"); err != nil {
		return true, 1
	}
	pipe, err := newLeasePipe()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
		// A process of a lease user can take the name while the service
		// is stopped (at a restart), and then nobody can delete that user.
		s <- svc.Status{State: svc.StartPending, WaitHint: 5 * 60 * 1000}
		endLeaseUserProcesses()
		if pipe, err = newLeasePipe(); err != nil {
			return true, 2
		}
	}
	go servePipe(pipe)
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			s <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			s <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

// newLeasePipe makes the one instance of the pipe, owned by SYSTEM, that only
// SYSTEM and the machine user can open, and only on this machine. The
// service keeps it for its whole run, so no other process gets the name.
func newLeasePipe() (windows.Handle, error) {
	machine, _, _, err := windows.LookupSID("", machineUser)
	if err != nil {
		return 0, fmt.Errorf("the machine user %s: %w", machineUser, err)
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:SYG:SYD:P(A;;GA;;;SY)(A;;0x%x;;;%s)", pipeAccess|windows.FILE_READ_ATTRIBUTES, machine))
	if err != nil {
		return 0, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(leasePipe)
	if err != nil {
		return 0, err
	}
	return windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		1, 64<<10, 64<<10, 0, sa)
}

func servePipe(h windows.Handle) {
	for {
		err := windows.ConnectNamedPipe(h, nil)
		if err == nil || errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
			answerLeaseRequest(h)
			// A client that does not read must not keep the one instance.
			withTimeout(pipeTimeout, func() error { return windows.FlushFileBuffers(h) })
			if f := afterAnswer; f != nil {
				afterAnswer = nil
				windows.DisconnectNamedPipe(h)
				f()
			}
		} else {
			time.Sleep(100 * time.Millisecond)
		}
		windows.DisconnectNamedPipe(h)
	}
}

// endLeaseUserProcesses ends the processes of all lease users.
func endLeaseUserProcesses() {
	users, err := leaseUsersWindows()
	if err != nil {
		return
	}
	for _, u := range users {
		if sid, _, _, err := windows.LookupSID("", u); err == nil {
			killUserProcesses(sid)
		}
	}
}

func answerLeaseRequest(h windows.Handle) {
	out, err := func() (any, error) {
		var line []byte
		// One instance of the pipe: a client that does not write must not
		// keep it.
		if err := withTimeout(pipeTimeout, func() (err error) {
			line, err = bufio.NewReader(io.LimitReader(pipeConn(h), 4096)).ReadBytes('\n')
			return err
		}); err != nil {
			return nil, err
		}
		// After the read: Windows lets a server identify its client only
		// after the client wrote.
		if err := checkPipeClient(h); err != nil {
			return nil, err
		}
		var req leaseRequest
		if err := json.Unmarshal(line, &req); err != nil {
			return nil, err
		}
		return runLeaseOp(req)
	}()
	var ans leaseAnswer
	if err != nil {
		ans.Error = err.Error()
	} else if ans.Out, err = json.Marshal(out); err != nil {
		ans.Error = err.Error()
	}
	data, _ := json.Marshal(ans)
	withTimeout(pipeTimeout, func() error {
		_, err := pipeConn(h).Write(append(data, '\n'))
		return err
	})
}

// withTimeout runs f, whose I/O is synchronous, and cancels that I/O
// (CancelSynchronousIo) after d, until f returns.
func withTimeout(d time.Duration, f func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	th, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		return err
	}
	defer windows.CloseHandle(th)
	var mu sync.Mutex
	finished := false
	timedOut := false
	stop := make(chan struct{})
	go func() {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-stop:
			return
		case <-t.C:
		}
		// Again until f returns: a cancel between two calls of f cancels
		// nothing.
		for {
			mu.Lock()
			if finished {
				mu.Unlock()
				return
			}
			timedOut = true
			procCancelSynchronousIo.Call(uintptr(th))
			mu.Unlock()
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	err = f()
	mu.Lock()
	finished = true
	mu.Unlock()
	close(stop)
	if timedOut && err != nil {
		return fmt.Errorf("no answer in %s: %w", d, err)
	}
	return err
}

// checkPipeClient checks that the client is the machine user (the pipe's
// access list allows only it and SYSTEM; this does not depend on it).
func checkPipeClient(h windows.Handle) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if r, _, err := procImpersonateNamedPipeClient.Call(uintptr(h)); r == 0 {
		return fmt.Errorf("identify the client: %w", err)
	}
	var tok windows.Token
	err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &tok)
	if rerr := windows.RevertToSelf(); rerr != nil {
		// The thread must not go on as the client.
		panic(rerr)
	}
	if err != nil {
		return fmt.Errorf("identify the client: %w", err)
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return err
	}
	machine, _, _, err := windows.LookupSID("", machineUser)
	if err != nil {
		return err
	}
	if !u.User.Sid.Equals(machine) {
		return fmt.Errorf("only the machine user %s can use the service", machineUser)
	}
	return nil
}

func runLeaseOp(req leaseRequest) (any, error) {
	switch req.Op {
	case "check":
		return protocol.LeaseUserCheck{Version: leaseUserVersion}, nil
	case "list":
		users, err := leaseUsersWindows()
		return protocol.LeaseUserList{Users: users}, err
	case "update":
		// The root step of a remote update (M13). The connection restarts,
		// and the service when the update replaced its binary, after the
		// answer.
		res, restart, err := updateSigned(config.OSWindows)
		if err == nil && restart != nil {
			afterAnswer = func() {
				restart()
				if selfReplaced {
					// Ends the process without a stop: the recovery action
					// starts the new binary.
					os.Exit(3)
				}
			}
		}
		return res, err
	case "create", "delete":
		// Also an open that waits (an oplock of another user's file) ends
		// at the deadline: the service does one request at a time.
		var out any
		err := withTimeout(opTimeout, func() (err error) {
			if req.Op == "create" {
				out, err = createLeaseUserWindows(req.Name)
			} else {
				out, err = struct{}{}, deleteLeaseUserWindows(req.Name)
			}
			return err
		})
		return out, err
	}
	return nil, fmt.Errorf("unknown operation %q", req.Op)
}

// systemPowerShell runs a script as SYSTEM with Windows PowerShell from the
// system directory. Values go in environment variables, not in the script.
func systemPowerShell(script string, env ...string) (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(sys, `WindowsPowerShell\v1.0\powershell.exe`),
		"-NoProfile", "-NonInteractive", "-Command",
		// The error text on the output: without a console, Windows PowerShell
		// loses its error records (checked as SYSTEM).
		"$ErrorActionPreference = 'Stop'\n$ProgressPreference = 'SilentlyContinue'\ntry {\n"+script+"\n} catch { 'error: ' + $_.ToString(); exit 1 }")
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = sys
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("PowerShell: %v: %s", err, strings.TrimSpace(stderr.String()+"\n"+string(out)))
	}
	return string(out), nil
}

func leaseUsersWindows() ([]string, error) {
	out, err := systemPowerShell(`Get-LocalUser | Where-Object { $_.FullName -eq '` + leaseUserMarker + `' } | ForEach-Object { $_.Name }`)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, n := range strings.Fields(out) {
		if leaseUserName.MatchString(n) {
			users = append(users, n)
		}
	}
	return users, nil
}

// isLeaseUserWindows reports whether name exists, and fails for a user that
// this command did not create.
func isLeaseUserWindows(name string) (bool, error) {
	// Not Get-LocalUser -Name: with ErrorActionPreference Stop, Windows
	// PowerShell 5.1 throws for a missing user.
	out, err := systemPowerShell(`$u = Get-LocalUser | Where-Object { $_.Name -eq $env:HOSTEL_NAME }
if ($u) { 'exists:' + $u.FullName }`, "HOSTEL_NAME="+name)
	if err != nil {
		return false, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return false, nil
	}
	if out != "exists:"+leaseUserMarker {
		return false, fmt.Errorf("the user %s exists and is not a Hostel lease user", name)
	}
	return true, nil
}

// createLeaseUserWindows makes a new user (an existing one of the same name
// goes first), with a password that nobody keeps, its profile, and the server
// key from the service's directory. Windows does not give a SID out twice.
func createLeaseUserWindows(name string) (protocol.LeaseUserCreated, error) {
	var resp protocol.LeaseUserCreated
	if err := checkLeaseUserName(name); err != nil {
		return resp, err
	}
	keyFile := filepath.Join(leaseRootDir, "server_key.pub")
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		return resp, fmt.Errorf("the server key: %w (hostel machine join --lease-users writes it)", err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(keyData)
	if err != nil {
		return resp, fmt.Errorf("the server key in %s: %w", keyFile, err)
	}
	if exists, err := isLeaseUserWindows(name); err != nil {
		return resp, err
	} else if exists {
		if err := deleteLeaseUserWindows(name); err != nil {
			return resp, err
		}
	}
	// The marker comes with the user: a user without it cannot be deleted.
	if _, err := systemPowerShell(`$n = $env:HOSTEL_NAME
if (-not (Get-LocalGroup | Where-Object { $_.Name -eq '`+leaseGroup+`' })) { New-LocalGroup -Name `+leaseGroup+` -Description 'Hostel lease users' | Out-Null }
$pw = ConvertTo-SecureString $env:HOSTEL_PASSWORD -AsPlainText -Force
New-LocalUser -Name $n -FullName '`+leaseUserMarker+`' -Password $pw -PasswordNeverExpires -AccountNeverExpires -UserMayNotChangePassword | Out-Null
Add-LocalGroupMember -SID S-1-5-32-545 -Member $n
Add-LocalGroupMember -Group `+leaseGroup+` -Member $n`, "HOSTEL_NAME="+name, "HOSTEL_PASSWORD="+randomPassword()); err != nil {
		return resp, err
	}
	sid, _, _, err := windows.LookupSID("", name)
	if err != nil {
		return resp, err
	}
	// The profile before the first login, so that Windows does not choose
	// another directory, and the key in it: the user cannot log in before.
	home, err := createProfile(sid, name)
	if err != nil {
		return resp, fmt.Errorf("create the profile of %s: %w", name, err)
	}
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return resp, err
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "authorized_keys"), ssh.MarshalAuthorizedKey(key), 0o600); err != nil {
		return resp, err
	}
	resp.Home = home
	return resp, nil
}

func createProfile(sid *windows.SID, name string) (string, error) {
	s, err := windows.UTF16PtrFromString(sid.String())
	if err != nil {
		return "", err
	}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH)
	if hr, _, _ := procCreateProfile.Call(uintptr(unsafe.Pointer(s)), uintptr(unsafe.Pointer(n)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); hr != 0 {
		return "", fmt.Errorf("CreateProfile: HRESULT 0x%08x", uint32(hr))
	}
	return windows.UTF16ToString(buf), nil
}

// deleteLeaseUserWindows ends the user's processes, removes its scheduled
// tasks and its files in the places where all users can write, then its
// profile and the user. A missing user is not an error.
func deleteLeaseUserWindows(name string) error {
	if err := checkLeaseUserName(name); err != nil {
		return err
	}
	deadline := time.Now().Add(deleteTimeout)
	exists, err := isLeaseUserWindows(name)
	if err != nil {
		return err
	}
	if !exists {
		// A deletion that stopped after the user record went.
		return removeLeftProfiles(name, nil, deadline)
	}
	sid, _, _, err := windows.LookupSID("", name)
	if err != nil {
		return err
	}
	// No new logon and no task of the user from here, then no process: a
	// process that starts later could write files after the search. The tasks
	// go again after the processes, for a task that a process made between;
	// only then does a task that stays stop the deletion (before, a process
	// that makes tasks again would block it).
	disable := func(check string) error {
		_, err := systemPowerShell(`$n = $env:HOSTEL_NAME; $s = $env:HOSTEL_SID
Disable-LocalUser -SID $s
$mine = {
  $u = $_.Principal.UserId
  if (-not $u) { return $false }
  if ($u -eq $s) { return $true }
  try { (New-Object Security.Principal.NTAccount $u).Translate([Security.Principal.SecurityIdentifier]).Value -eq $s } catch { $u -eq $n -or $u -like "*\$n" }
}
Get-ScheduledTask | Where-Object $mine | Unregister-ScheduledTask -Confirm:$false
$left = @(Get-ScheduledTask | Where-Object $mine)
if ($env:HOSTEL_CHECK -and $left.Count) { throw "scheduled tasks of the user stay: $(($left | ForEach-Object { $_.TaskPath + $_.TaskName }) -join ', ')" }`,
			"HOSTEL_NAME="+name, "HOSTEL_SID="+sid.String(), "HOSTEL_CHECK="+check)
		if err != nil {
			return fmt.Errorf("disable %s and remove its scheduled tasks: %w", name, err)
		}
		return nil
	}
	if err := disable(""); err != nil {
		return err
	}
	if err := killUserProcesses(sid); err != nil {
		return err
	}
	if err := disable("1"); err != nil {
		return err
	}
	for _, d := range sharedDirs() {
		if err := filesOf(sid, d.path, d.deep, true, deadline); err != nil {
			return err
		}
	}
	if err := killUserProcesses(sid); err != nil {
		return err
	}
	for _, d := range sharedDirs() {
		if err := filesOf(sid, d.path, d.deep, false, deadline); err != nil {
			return err
		}
	}
	if err := deleteProfile(sid, name, deadline); err != nil {
		return err
	}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	const nerrUserNotFound = 2221
	if r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(n))); r != 0 && r != nerrUserNotFound {
		return fmt.Errorf("NetUserDel %s: error %d", name, r)
	}
	return nil
}

func usersDir() string {
	return filepath.Join(os.Getenv("SystemDrive")+`\`, "Users")
}

type sharedDir struct {
	path string
	// deep: the whole tree; else only the entries at the top.
	deep bool
}

// sharedDirs are the places where a user without administrator rights can
// make files that are not in its profile.
func sharedDirs() []sharedDir {
	win, _ := windows.GetWindowsDirectory()
	drive := filepath.VolumeName(win) + `\`
	dirs := []sharedDir{
		{drive, false},
		{filepath.Join(drive, "ProgramData"), true},
		{filepath.Join(drive, `Users\Public`), true},
		{filepath.Join(win, "Temp"), true},
		{filepath.Join(win, "Tasks"), true},
		{filepath.Join(win, "tracing"), true},
		{filepath.Join(win, `System32\spool\drivers\color`), true},
		{filepath.Join(win, `System32\spool\SERVERS`), true},
		{filepath.Join(win, `System32\Microsoft\Crypto\RSA\MachineKeys`), true},
		{filepath.Join(win, `System32\FxsTmp`), true},
		{filepath.Join(win, `System32\com\dmp`), true},
		{filepath.Join(win, `System32\Tasks`), true},
		{filepath.Join(win, `SysWOW64\FxsTmp`), true},
		{filepath.Join(win, `SysWOW64\com\dmp`), true},
		{filepath.Join(win, `SysWOW64\Tasks`), true},
		{filepath.Join(win, `Registration\CRMLog`), true},
		{filepath.Join(win, `System32\Tasks_Migrated`), true},
		// The user's own recycle bin, by its SID.
		{filepath.Join(drive, `$Recycle.Bin`), false},
	}
	// The access list of the drive's top gives Authenticated Users write
	// access to the folders that others make there (for example an installer's
	// C:\tools), so these get the whole search. Windows, Users, ProgramData,
	// and the recycle bin have their own entries above.
	skip := map[string]bool{"windows": true, "users": true, "programdata": true, "$recycle.bin": true,
		"program files": true, "program files (x86)": true, "system volume information": true, "recovery": true}
	entries, _ := os.ReadDir(drive)
	for _, e := range entries {
		if e.Type() == fs.ModeDir && !skip[strings.ToLower(e.Name())] {
			dirs = append(dirs, sharedDir{filepath.Join(drive, e.Name()), true})
		}
	}
	return dirs
}

// filesOf removes (remove) or finds the entries that sid owns in dir. It
// does not follow junctions or links, and it opens each entry through the
// handle of its directory: the lease user (or another user) can replace a
// directory on the way with a junction while the walk runs.
func filesOf(sid *windows.SID, dir string, deep, remove bool, deadline time.Time) error {
	h, err := openNoFollow(dir, windows.FILE_LIST_DIRECTORY)
	if notFound(err) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer windows.CloseHandle(h)
	return filesAt(sid, h, dir, deep, remove, deadline)
}

func filesAt(sid *windows.SID, dir windows.Handle, dirPath string, deep, remove bool, deadline time.Time) error {
	if time.Now().After(deadline) {
		return fmt.Errorf("the search for the files of the lease user took too long (at %s)", dirPath)
	}
	entries, err := listDir(dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", dirPath, err)
	}
	for _, e := range entries {
		p := filepath.Join(dirPath, e.name)
		// READ_CONTROL only: the sharing mode of other opens does not
		// apply to it.
		h, err := openAt(dir, e.name, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
		if notFound(err) ||
			// A file that the system protects also from SYSTEM, or a
			// paging file (no open at all, also with READ_CONTROL only):
			// not a lease user's file.
			errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			continue
		}
		if err != nil {
			return fmt.Errorf("open %s: %w", p, err)
		}
		err = func() error {
			defer windows.CloseHandle(h)
			owner, err := ownerOf(h)
			if err != nil {
				return fmt.Errorf("the owner of %s: %w", p, err)
			}
			if owner.Equals(sid) {
				if !remove {
					return fmt.Errorf("a file of the lease user is left: %s", p)
				}
				return removeAt(dir, dirPath, e.name, deadline)
			}
			// The attributes of the open entry, not of the listing:
			// the entry can have changed between them.
			var info windows.ByHandleFileInformation
			if err := windows.GetFileInformationByHandle(h, &info); err != nil {
				return fmt.Errorf("the attributes of %s: %w", p, err)
			}
			if !deep || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return nil
			}
			l, err := openAt(h, "", windows.FILE_LIST_DIRECTORY)
			if errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED) || notFound(err) {
				// Another user's directory that a process holds without
				// sharing, or that a filter protects: the lease user's
				// files in it stay (m11w-implementation.md, limits).
				return nil
			}
			if err != nil {
				return fmt.Errorf("open %s: %w", p, err)
			}
			defer windows.CloseHandle(l)
			return filesAt(sid, l, p, true, remove, deadline)
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

func notFound(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

type dirEntry struct {
	name  string
	attrs uint32
}

// openNoFollow opens path itself (also a junction or link), with the backup
// privilege, so that the file's access list does not matter. Only for paths
// whose directories only administrators can change.
func openNoFollow(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access|windows.SYNCHRONIZE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

// openAt opens the entry name of the directory dir itself (also a junction or
// link), with the backup privilege. One name relative to a handle: no part of
// the path can be a junction. An empty name opens dir again.
func openAt(dir windows.Handle, name string, access uint32) (windows.Handle, error) {
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: dir, ObjectName: n}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, access|windows.SYNCHRONIZE, &oa, &iosb, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT|fileCompleteIfOplocked, 0, 0)
	if st, ok := err.(windows.NTStatus); ok {
		if st == statusOplockBreakInProgress {
			// Open, without a wait for the holder of the oplock.
			return h, nil
		}
		return 0, st.Errno()
	}
	return h, err
}

const (
	fileCompleteIfOplocked      = 0x100
	statusOplockBreakInProgress = windows.NTStatus(0x108)
)

// listDir lists a directory through its handle (FILE_FULL_DIR_INFO).
func listDir(h windows.Handle) ([]dirEntry, error) {
	var out []dirEntry
	buf := make([]byte, 64<<10)
	for {
		err := windows.GetFileInformationByHandleEx(h, windows.FileFullDirectoryInfo, &buf[0], uint32(len(buf)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		for off := 0; ; {
			// FILE_FULL_DIR_INFO: NextEntryOffset at 0, FileAttributes at
			// 56, FileNameLength (bytes) at 60, FileName at 68.
			e := buf[off:]
			next := binary.LittleEndian.Uint32(e[0:])
			attrs := binary.LittleEndian.Uint32(e[56:])
			nlen := int(binary.LittleEndian.Uint32(e[60:]))
			name := make([]uint16, nlen/2)
			for i := range name {
				name[i] = binary.LittleEndian.Uint16(e[68+2*i:])
			}
			if n := windows.UTF16ToString(name); n != "." && n != ".." {
				out = append(out, dirEntry{n, attrs})
			}
			if next == 0 {
				break
			}
			off += int(next)
		}
	}
}

func ownerOf(h windows.Handle) (*windows.SID, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, err
	}
	return owner.Copy()
}

// removeTree removes path, which must be in a directory that only
// administrators can change, and everything in it (removeAt).
func removeTree(path string, deadline time.Time) error {
	dir, err := openNoFollow(filepath.Dir(path), windows.FILE_LIST_DIRECTORY)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Dir(path), err)
	}
	defer windows.CloseHandle(dir)
	return removeAt(dir, filepath.Dir(path), filepath.Base(path), deadline)
}

// removeAt removes the entry name of dir and, for a directory that is not a
// junction or a link, everything in it, each entry through the handle of its
// directory. A junction or link goes itself, not its target.
func removeAt(dir windows.Handle, dirPath, name string, deadline time.Time) error {
	path := filepath.Join(dirPath, name)
	if time.Now().After(deadline) {
		return fmt.Errorf("the deletion took too long (at %s)", path)
	}
	h, err := openAt(dir, name, windows.DELETE|windows.FILE_READ_ATTRIBUTES)
	if notFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		l, err := openAt(h, "", windows.FILE_LIST_DIRECTORY)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		entries, err := listDir(l)
		if err == nil {
			for _, e := range entries {
				if err = removeAt(l, path, e.name, deadline); err != nil {
					break
				}
			}
		} else {
			err = fmt.Errorf("list %s: %w", path, err)
		}
		windows.CloseHandle(l)
		if err != nil {
			return err
		}
	}
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	if err := windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), 4); err != nil {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	return nil
}

// killUserProcesses ends all processes of sid until none is left.
func killUserProcesses(sid *windows.SID) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		found, err := endProcessesOf(sid)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes of %s still run after TerminateProcess", sid)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func endProcessesOf(sid *windows.SID) (bool, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	found := false
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == 0 || e.ProcessID == 4 {
			continue
		}
		ended, perr := processOf(e.ProcessID, sid)
		if perr != nil {
			return false, perr
		}
		found = found || ended
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return false, err
	}
	return found, nil
}

// processOf ends the process pid when sid runs it, and reports whether it did.
// A process whose user it cannot read is an error, not another user's process:
// the lease user owns its token and can take SYSTEM out of its access list.
func processOf(pid uint32, sid *windows.SID) (bool, error) {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		// Ended, or a protected process of the system (SeDebugPrivilege
		// opens all others).
		return false, nil
	}
	defer windows.CloseHandle(p)
	user, err := processUser(p)
	if err != nil {
		if ev, _ := windows.WaitForSingleObject(p, 0); ev == windows.WAIT_OBJECT_0 {
			return false, nil // ended
		}
		return false, fmt.Errorf("the user of process %d: %w", pid, err)
	}
	if !user.Equals(sid) {
		return false, nil
	}
	windows.TerminateProcess(p, 1)
	return true, nil
}

func processUser(p windows.Handle) (*windows.SID, error) {
	var tok windows.Token
	err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		if err = takeToken(p); err == nil {
			err = windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok)
		}
	}
	if err != nil {
		return nil, err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid.Copy()
}

// takeToken makes SYSTEM the owner of the process's token (the take-ownership
// privilege) and gives SYSTEM access to it.
func takeToken(p windows.Handle) error {
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.WRITE_OWNER, &tok); err != nil {
		return err
	}
	err = windows.SetSecurityInfo(windows.Handle(tok), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION, system, nil, nil, nil)
	tok.Close()
	if err != nil {
		return err
	}
	if err := windows.OpenProcessToken(p, windows.WRITE_DAC, &tok); err != nil {
		return err
	}
	defer tok.Close()
	sd, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;SY)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(windows.Handle(tok), windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// deleteProfile removes the user's profile directory and its entry in the
// registry. Windows unloads the profile some seconds after the last process.
func deleteProfile(sid *windows.SID, name string, deadline time.Time) error {
	key := profileList + `\` + sid.String()
	home := ""
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE); err == nil {
		home, _, _ = k.GetStringValue("ProfileImagePath")
		k.Close()
	}
	// Only a profile directory of this user's name in the users' directory.
	if home != "" && !profileDirOf(home, name) {
		return fmt.Errorf("the profile of %s is %s, not %s in %s", name, home, name, usersDir())
	}
	for _, hive := range []string{sid.String() + "_Classes", sid.String()} {
		if err := unloadHive(hive); err != nil {
			return fmt.Errorf("unload the registry of %s: %w", name, err)
		}
	}
	if home == "" {
		// No entry (a creation or a deletion that stopped): DeleteProfile
		// cannot find the profile.
		return removeLeftProfiles(name, sid, deadline)
	}
	s, err := windows.UTF16PtrFromString(sid.String())
	if err != nil {
		return err
	}
	// The directory goes first, through removeTree, which follows no link:
	// the user controlled the profile, and DeleteProfile does not document
	// what it does with a junction. A file in use for a short time (a process
	// that is ending) gets more attempts.
	var rmErr error
	for wait := time.Now().Add(time.Minute); ; time.Sleep(time.Second) {
		if rmErr = removeTree(home, deadline); rmErr == nil || time.Now().After(wait) {
			break
		}
	}
	if rmErr != nil {
		return fmt.Errorf("remove the profile of %s: %w", name, rmErr)
	}
	// DeleteProfile then removes what Windows keeps of the profile beside the
	// directory. Its result does not matter: the entry goes below in any case.
	procDeleteProfile.Call(uintptr(unsafe.Pointer(s)), 0, 0)
	if err := registry.DeleteKey(registry.LOCAL_MACHINE, key); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return fmt.Errorf("remove the profile entry of %s: %w", name, err)
	}
	return removeLeftProfiles(name, sid, deadline)
}

// profileDirOf reports whether dir is a profile directory of the user name:
// <users>\<name> or <users>\<name>.<suffix> (Windows adds the computer's name
// when the directory exists).
func profileDirOf(dir, name string) bool {
	base := strings.ToLower(filepath.Base(dir))
	return strings.EqualFold(filepath.Dir(dir), usersDir()) &&
		(base == strings.ToLower(name) || strings.HasPrefix(base, strings.ToLower(name)+"."))
}

// removeLeftProfiles removes the profile directories of the user name and the
// profile entries that point to them, also after the user record went (own is
// then nil). An entry of another account that still exists (a domain user of
// the same name) stays, with its directory. The hives go first and the entries
// last, so that a failed removal can be repeated.
func removeLeftProfiles(name string, own *windows.SID, deadline time.Time) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileList, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return err
	}
	sids, err := k.ReadSubKeyNames(0)
	k.Close()
	if err != nil {
		return err
	}
	var gone []string
	keep := map[string]bool{}
	for _, sidText := range sids {
		sk, err := registry.OpenKey(registry.LOCAL_MACHINE, profileList+`\`+sidText, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		home, _, _ := sk.GetStringValue("ProfileImagePath")
		sk.Close()
		if home == "" || !profileDirOf(home, name) {
			continue
		}
		sid, err := windows.StringToSid(sidText)
		if err != nil {
			continue
		}
		if !(own != nil && sid.Equals(own)) {
			if _, _, _, err := sid.LookupAccount(""); err == nil {
				keep[strings.ToLower(filepath.Base(home))] = true
				continue
			}
		}
		for _, hive := range []string{sidText + "_Classes", sidText} {
			if err := unloadHive(hive); err != nil {
				return fmt.Errorf("unload the registry of %s: %w", name, err)
			}
		}
		gone = append(gone, sidText)
	}
	dir, err := openNoFollow(usersDir(), windows.FILE_LIST_DIRECTORY)
	if err != nil {
		return fmt.Errorf("open %s: %w", usersDir(), err)
	}
	defer windows.CloseHandle(dir)
	entries, err := listDir(dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", usersDir(), err)
	}
	for _, e := range entries {
		if profileDirOf(filepath.Join(usersDir(), e.name), name) && !keep[strings.ToLower(e.name)] {
			if err := removeAt(dir, usersDir(), e.name, deadline); err != nil {
				return fmt.Errorf("remove the profile of %s: %w", name, err)
			}
		}
	}
	for _, sidText := range gone {
		if err := registry.DeleteKey(registry.LOCAL_MACHINE, profileList+`\`+sidText); err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return fmt.Errorf("remove the profile entry %s: %w", sidText, err)
		}
	}
	return nil
}

// unloadHive unloads the hive HKEY_USERS\<hive> by force if it is loaded.
// A service can keep a user's registry open after its last process (Windows
// Search, checked on Windows 11), and then nobody can delete the profile until
// a restart.
func unloadHive(hive string) error {
	// NtUnloadKey2 answers STATUS_INVALID_PARAMETER, not "not found", for a
	// hive that is not loaded (checked on Windows 11).
	k, err := registry.OpenKey(registry.USERS, hive, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err == nil {
		k.Close()
	}
	return forceUnloadHive(`\Registry\User\` + hive)
}

// forceUnloadHive unloads a loaded registry hive also when processes have it
// open (REG_FORCE_UNLOAD; it needs the restore privilege). A hive that is not
// loaded is not an error.
func forceUnloadHive(path string) error {
	name, err := windows.NewNTUnicodeString(path)
	if err != nil {
		return err
	}
	oa := windows.OBJECT_ATTRIBUTES{ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	const regForceUnload = 1
	st, _, _ := procNtUnloadKey2.Call(uintptr(unsafe.Pointer(&oa)), regForceUnload)
	switch windows.NTStatus(st) {
	case windows.STATUS_SUCCESS, windows.STATUS_OBJECT_NAME_NOT_FOUND:
		return nil
	}
	return windows.NTStatus(st)
}

func enablePrivileges(names ...string) error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		return err
	}
	defer tok.Close()
	for _, n := range names {
		var luid windows.LUID
		p, err := windows.UTF16PtrFromString(n)
		if err != nil {
			return err
		}
		if err := windows.LookupPrivilegeValue(nil, p, &luid); err != nil {
			return err
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1, Privileges: [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}}}
		if err := windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// installLeaseUserService installs the root step on Windows: the service's
// directory, a copy of this binary, the server key, and the service.
func installLeaseUserService(serverKey string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return installRootService(self, serverKey)
}

// installRootService installs the SYSTEM service with the binary src. The
// service does the lease-user operations only with serverKey (--lease-users:
// it writes server_key.pub), and the root step of a remote update (M13).
func installRootService(src, serverKey string) error {
	var pk ssh.PublicKey
	if serverKey != "" {
		var err error
		if pk, _, _, _, err = ssh.ParseAuthorizedKey([]byte(serverKey)); err != nil {
			return fmt.Errorf("the server key: %w", err)
		}
	}
	if err := powershell(`$d = $env:HOSTEL_DIR
if (Test-Path $d) {
  $o = (Get-Acl $d).GetOwner([Security.Principal.SecurityIdentifier]).Value
  if ($o -notin 'S-1-5-32-544', 'S-1-5-18') { throw "$d belongs to $o, not to the administrators; remove it and run again" }
} else { New-Item -ItemType Directory $d | Out-Null }
# Only these entries, also on the files: no access that an earlier owner added.
icacls $d /setowner '*S-1-5-32-544' /T /C /L | Out-Null
if ($LASTEXITCODE) { throw "icacls /setowner $d failed" }
icacls $d /reset /T /C /L | Out-Null
if ($LASTEXITCODE) { throw "icacls /reset $d failed" }
icacls $d /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-32-545:(OI)(CI)RX' | Out-Null
if ($LASTEXITCODE) { throw "icacls /grant $d failed" }
# Again: a file made before the grant has another owner.
icacls $d /setowner '*S-1-5-32-544' /T /C /L | Out-Null
if ($LASTEXITCODE) { throw "icacls /setowner $d failed" }`, "HOSTEL_DIR="+leaseRootDir); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(leaseService)
	if err == nil {
		if err := stopService(s); err != nil {
			return err
		}
	} else {
		s = nil
	}
	exe := filepath.Join(leaseRootDir, "hostel.exe")
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(exe+".tmp", data, 0o755); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = os.Rename(exe+".tmp", exe)
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return err
	}
	if pk != nil {
		if err := os.WriteFile(filepath.Join(leaseRootDir, "server_key.pub"), ssh.MarshalAuthorizedKey(pk), 0o644); err != nil {
			return err
		}
	}
	if s == nil {
		s, err = m.CreateService(leaseService, exe, mgr.Config{
			DisplayName: "Hostel lease users",
			Description: "Creates and deletes the users of Hostel leases for the machine user (hostel machine join --lease-users).",
			StartType:   mgr.StartAutomatic,
		}, "lease-user", "serve")
		if err != nil {
			return fmt.Errorf("create the service %s: %w", leaseService, err)
		}
	}
	defer s.Close()
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{{Type: mgr.ServiceRestart, Delay: 5 * time.Second}}, 24*60*60); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("start the service %s: %w", leaseService, err)
	}
	return waitService(s, svc.Running)
}

func stopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
		return err
	}
	return waitService(s, svc.Stopped)
}

func waitService(s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the service %s is in state %d, not %d, after 30 s", leaseService, st.State, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

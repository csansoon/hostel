package helper

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows has no sessions or process groups as macOS and Linux have. A job
// command runs in a job object; cleanup finds the other lease processes by
// HOSTEL_LEASE in their environment and by their parent (design, section 17;
// m6-implementation.md, section 2).

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procCreateJob      = kernel32.NewProc("CreateJobObjectW")
	procOpenJob        = kernel32.NewProc("OpenJobObjectW")
	procNtResume       = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")
	groupsMu           sync.Mutex
	groups             = map[*exec.Cmd]windows.Handle{}
	errWindowsSessions = errors.New("Windows has no sessions")
)

const (
	jobObjectQuery     = 0x0004
	jobObjectTerminate = 0x0008
)

// setSession starts the job supervisor outside the job object of the SSH
// session, which ends all its processes when the session ends.
func setSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}

// startInGroup starts cmd suspended, puts it in a new job object, and resumes
// it, so all its descendants are in the job too. A named job ends its
// processes when this process ends (the supervisor holds the only handle); a
// guest process cannot break away from it.
func startInGroup(cmd *exec.Cmd, name string) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
	job, err := createJob(name)
	if err != nil {
		return err
	}
	if name != "" {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			windows.CloseHandle(job)
			return fmt.Errorf("set the job object limits: %w", err)
		}
	}
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return err
	}
	p, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err == nil {
		defer windows.CloseHandle(p)
		if err = windows.AssignProcessToJobObject(job, p); err == nil {
			if r, _, _ := procNtResume.Call(uintptr(p)); r != 0 {
				err = fmt.Errorf("resume the process: NTSTATUS %#x", r)
			}
		}
	}
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		windows.CloseHandle(job)
		return fmt.Errorf("start %s in a job object: %w", cmd.Path, err)
	}
	groupsMu.Lock()
	groups[cmd] = job
	groupsMu.Unlock()
	return nil
}

// createJob creates a job object. A named job must be new: a job with the
// same name could belong to another process.
func createJob(name string) (windows.Handle, error) {
	var np *uint16
	if name != "" {
		var err error
		if np, err = windows.UTF16PtrFromString(name); err != nil {
			return 0, err
		}
	}
	r, _, lastErr := procCreateJob.Call(0, uintptr(unsafe.Pointer(np)))
	if r == 0 {
		return 0, fmt.Errorf("create a job object: %w", lastErr)
	}
	if name != "" && errors.Is(lastErr, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(windows.Handle(r))
		return 0, fmt.Errorf("create the job object %s: it exists already", name)
	}
	return windows.Handle(r), nil
}

// confine puts the helper in a new job object without BREAKAWAY_OK, before it
// starts a guest command: the SSH session's job allows breakaway, and a process
// that broke away and cleared its environment would escape cleanup.
func confine() error {
	job, err := createJob("")
	if err != nil {
		return err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		windows.CloseHandle(job)
		return fmt.Errorf("put the guest command in a job object: %w", err)
	}
	return nil
}

func killProcessGroup(cmd *exec.Cmd) error {
	groupsMu.Lock()
	job, ok := groups[cmd]
	groupsMu.Unlock()
	if !ok {
		return cmd.Process.Kill()
	}
	return windows.TerminateJobObject(job, 1)
}

// releaseGroup closes the job object of cmd; a job with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE ends its processes.
func releaseGroup(cmd *exec.Cmd) {
	groupsMu.Lock()
	job, ok := groups[cmd]
	delete(groups, cmd)
	groupsMu.Unlock()
	if ok {
		windows.CloseHandle(job)
	}
}

// jobGroupName is the name of the job object of a job command. The user SID
// keeps the names of room users apart (all SSH sessions are in Windows
// session 0, so they share the Local namespace).
func jobGroupName(room, id string) string {
	return `Local\hostel-` + userSID() + "-" + room + "-" + id
}

// stopGroup ends all processes of a job command's job object. Windows has no
// SIGTERM for processes without a console, so it ends them at once.
func stopGroup(_ int, group string, _ bool) {
	if group == "" {
		return
	}
	np, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return
	}
	r, _, _ := procOpenJob.Call(jobObjectTerminate|jobObjectQuery, 0, uintptr(unsafe.Pointer(np)))
	if r == 0 {
		return // the supervisor has ended, and with it the job
	}
	defer windows.CloseHandle(windows.Handle(r))
	windows.TerminateJobObject(windows.Handle(r), 1)
}

// processName returns the image path of a process, or "unknown".
func processName(pid int) string {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "unknown"
	}
	defer windows.CloseHandle(p)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(p, 0, &buf[0], &n); err != nil {
		return "unknown"
	}
	return windows.UTF16ToString(buf[:n])
}

func signalProcess(pid int, _ bool) {
	p, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(p)
	windows.TerminateProcess(p, 1)
}

func exitCode(st *os.ProcessState) int { return st.ExitCode() }

// execProcess runs the command with the helper's stdin, stdout, and stderr and
// exits with its exit code (Windows cannot replace a process).
func execProcess(path string, argv, env []string) error {
	cmd := &exec.Cmd{Path: path, Args: argv, Env: env, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return err
	}
	os.Exit(cmd.ProcessState.ExitCode())
	return nil
}

// shellCommand returns the program and arguments that run a command string
// with PowerShell. The wrapper makes the exit code that of the last command:
// PowerShell itself exits with 0 or 1.
func shellCommand(command string, env []string) (string, []string) {
	ps := powerShell(env)
	script := "$ProgressPreference = 'SilentlyContinue'\nif ($PSStyle) { $PSStyle.OutputRendering = 'PlainText' }\n" + command + "\n$hostelOK = $?\nif (-not $hostelOK) { if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1 }\nexit 0\n"
	// Not -EncodedCommand: with it, Windows PowerShell writes stderr as CLIXML.
	return ps, []string{ps, "-NoProfile", "-NonInteractive", "-Command", script}
}

func interactiveShell(env []string) (string, []string) {
	ps := powerShell(env)
	return ps, []string{ps, "-NoLogo", "-NoProfile"}
}

// scriptCommand returns the program and arguments that run a .ps1 tool
// script.
func scriptCommand(script string, args, env []string) (string, []string) {
	ps := powerShell(env)
	return ps, append([]string{ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}, args...)
}

// powerShell returns PowerShell 7 if it is in the PATH, else Windows
// PowerShell 5.1, which every Windows has.
func powerShell(env []string) string {
	if p, err := lookPath("pwsh", env); err == nil {
		return p
	}
	if p, err := lookPath("powershell", env); err == nil {
		return p
	}
	return `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
}

func currentSession() (int, error)                 { return 0, errWindowsSessions }
func processSession(int) (int, error)              { return 0, errWindowsSessions }
func sessionProcesses(map[int]bool) ([]int, error) { return nil, nil }

var (
	sidOnce sync.Once
	sid     *windows.SID
	sidText string
)

func userSID() string {
	sidOnce.Do(func() {
		if tu, err := windows.GetCurrentProcessToken().GetTokenUser(); err == nil {
			if sid, err = tu.User.Sid.Copy(); err == nil {
				sidText = sid.String()
			}
		}
	})
	return sidText
}

// openOwn opens a process of the machine user. It fails for processes of other
// users, also when the machine user could open them.
func openOwn(pid int, access uint32) (windows.Handle, error) {
	p, err := windows.OpenProcess(access|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		windows.CloseHandle(p)
		return 0, err
	}
	tu, err := tok.GetTokenUser()
	tok.Close()
	if userSID(); err != nil || sid == nil || !tu.User.Sid.Equals(sid) {
		windows.CloseHandle(p)
		return 0, errors.New("not a process of the machine user")
	}
	return p, nil
}

func startOf(p windows.Handle) (string, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(p, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.Nanoseconds(), 10), nil
}

// processStart returns the creation time of a process, to detect reused PIDs.
func processStart(pid int) (string, error) {
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(p)
	var code uint32
	if err := windows.GetExitCodeProcess(p, &code); err == nil && code != 259 { // STILL_ACTIVE
		return "", fmt.Errorf("no process %d", pid)
	}
	return startOf(p)
}

// processStarts returns the start times of the processes in pids that run. A
// process that cannot be opened is not running for the caller.
func processStarts(pids []int) (map[int]string, error) {
	out := map[int]string{}
	for _, pid := range pids {
		if s, err := processStart(pid); err == nil {
			out[pid] = s
		}
	}
	return out, nil
}

type procEntry struct {
	ppid  int
	start string
}

// userProcesses returns the machine user's processes (without this one), with
// their parent PIDs and start times, from one Toolhelp snapshot.
func userProcesses() (map[int]procEntry, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("list the processes: %w", err)
	}
	defer windows.CloseHandle(snap)
	out := map[int]procEntry{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		pid := int(e.ProcessID)
		if pid == os.Getpid() || pid == 0 {
			continue
		}
		p, err := openOwn(pid, 0)
		if err != nil {
			continue
		}
		start, err := startOf(p)
		windows.CloseHandle(p)
		if err == nil {
			out[pid] = procEntry{ppid: int(e.ParentProcessID), start: start}
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("list the processes: %w", err)
	}
	return out, nil
}

// descendants returns the machine user's processes below one of pids. Windows
// keeps the parent PID of a process after the parent ends, and can give that
// PID to a new process; a child must therefore be younger than its parent.
func descendants(pids []int) ([]int, error) {
	procs, err := userProcesses()
	if err != nil {
		return nil, err
	}
	in := map[int]bool{}
	for _, p := range pids {
		in[p] = true
	}
	var out []int
	for changed := true; changed; {
		changed = false
		for pid, e := range procs {
			parent, ok := procs[e.ppid]
			if in[e.ppid] && !in[pid] && (!ok || olderOrSame(parent.start, e.start)) {
				in[pid] = true
				out = append(out, pid)
				changed = true
			}
		}
	}
	return out, nil
}

func olderOrSame(a, b string) bool {
	x, err1 := strconv.ParseInt(a, 10, 64)
	y, err2 := strconv.ParseInt(b, 10, 64)
	return err1 == nil && err2 == nil && x <= y
}

// envProcesses returns the machine user's processes whose environment has
// HOSTEL_LEASE=<id>.
func envProcesses(leaseID string) ([]int, error) {
	procs, err := userProcesses()
	if err != nil {
		return nil, err
	}
	want := "HOSTEL_LEASE=" + leaseID
	var out []int
	for pid := range procs {
		p, err := openOwn(pid, windows.PROCESS_VM_READ)
		if err != nil {
			continue // ended, or a protected process
		}
		env, err := processEnv(p)
		windows.CloseHandle(p)
		if err != nil {
			continue
		}
		for _, kv := range env {
			if kv == want {
				out = append(out, pid)
				break
			}
		}
	}
	return out, nil
}

// processBasicInformation is PROCESS_BASIC_INFORMATION with addresses as
// integers: they are addresses in another process, not Go pointers.
type processBasicInformation struct {
	ExitStatus                   uintptr
	PebBaseAddress               uintptr
	AffinityMask                 uintptr
	BasePriority                 uintptr
	UniqueProcessID              uintptr
	InheritedFromUniqueProcessID uintptr
}

// processEnv reads the environment block of another process from its PEB (as
// "ps -E" does on macOS). Only 64-bit layouts: the helper is amd64 or arm64.
func processEnv(p windows.Handle) ([]string, error) {
	var pbi processBasicInformation
	if err := windows.NtQueryInformationProcess(p, windows.ProcessBasicInformation, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), nil); err != nil {
		return nil, err
	}
	params, err := readPtr(p, pbi.PebBaseAddress+unsafe.Offsetof((*windows.PEB)(nil).ProcessParameters))
	if err != nil {
		return nil, err
	}
	envAddr, err := readPtr(p, params+unsafe.Offsetof((*windows.RTL_USER_PROCESS_PARAMETERS)(nil).Environment))
	if err != nil {
		return nil, err
	}
	size, err := readPtr(p, params+unsafe.Offsetof((*windows.RTL_USER_PROCESS_PARAMETERS)(nil).EnvironmentSize))
	if err != nil {
		return nil, err
	}
	if size == 0 || size > 1<<20 {
		return nil, fmt.Errorf("unexpected environment size %d", size)
	}
	buf := make([]byte, size&^1)
	if err := windows.ReadProcessMemory(p, envAddr, &buf[0], uintptr(len(buf)), nil); err != nil {
		return nil, err
	}
	u := make([]uint16, len(buf)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(buf[2*i:])
	}
	var env []string
	for start, i := 0, 0; i < len(u); i++ {
		if u[i] != 0 {
			continue
		}
		if i == start {
			break // the block ends with an empty string
		}
		env = append(env, string(utf16.Decode(u[start:i])))
		start = i + 1
	}
	return env, nil
}

func readPtr(p windows.Handle, addr uintptr) (uintptr, error) {
	var b [8]byte
	if err := windows.ReadProcessMemory(p, addr, &b[0], uintptr(len(b)), nil); err != nil {
		return 0, err
	}
	return uintptr(binary.LittleEndian.Uint64(b[:])), nil
}

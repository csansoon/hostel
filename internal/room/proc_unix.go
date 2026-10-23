//go:build !windows

package room

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// setSession starts the command in a new session, so it survives the end of
// the SSH session that started it.
func setSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killProcessGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// signalGroup sends SIGTERM (or SIGKILL) to a process group.
func signalGroup(pgid int, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	syscall.Kill(-pgid, sig)
}

func signalProcess(pid int, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	syscall.Kill(pid, sig)
}

// exitCode returns the exit code, or 128 + the signal number.
func exitCode(st *os.ProcessState) int {
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return st.ExitCode()
}

func currentSession() (int, error) { return processSession(os.Getpid()) }

// sessionProcesses returns the room user's processes in one of the sessions.
func sessionProcesses(sids map[int]bool) ([]int, error) {
	if len(sids) == 0 {
		return nil, nil
	}
	parents, err := userParents()
	if err != nil {
		return nil, err
	}
	var out []int
	for pid := range parents {
		if pid == os.Getpid() {
			continue
		}
		if sid, err := processSession(pid); err == nil && sids[sid] {
			out = append(out, pid)
		}
	}
	return out, nil
}

// descendants returns the room user's processes below one of pids, also in
// other sessions.
func descendants(pids []int) ([]int, error) {
	parents, err := userParents()
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
		for pid, ppid := range parents {
			if in[ppid] && !in[pid] && pid != os.Getpid() {
				in[pid] = true
				out = append(out, pid)
				changed = true
			}
		}
	}
	return out, nil
}

func execProcess(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}

//go:build !windows

package helper

import (
	"os"
	"os/exec"
	"syscall"
)

// startInGroup starts cmd in a new process group. The name is for Windows.
func startInGroup(cmd *exec.Cmd, _ string) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Start()
}

func jobGroupName(string, string) string { return "" }

// confine is for Windows; on macOS and Linux, cleanup finds the processes of a
// guest command by its session.
func confine() error { return nil }

// setSession starts the command in a new session, so it survives the end of
// the SSH session that started it.
func setSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func killProcessGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// stopGroup sends SIGTERM (or SIGKILL) to a process group.
func stopGroup(pgid int, _ string, kill bool) {
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

// sessionProcesses returns the machine user's processes in one of the sessions.
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

// descendants returns the machine user's processes below one of pids, also in
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

func shellCommand(command string, _ []string) (string, []string) {
	return "/bin/sh", []string{"sh", "-c", command}
}

// interactiveShell returns the machine user's shell.
func interactiveShell(env []string) (string, []string) {
	sh := envMap(env)["SHELL"]
	if sh == "" {
		sh = "/bin/sh"
	}
	return sh, []string{sh}
}

func scriptCommand(script string, args, _ []string) (string, []string) {
	return "/bin/sh", append([]string{"sh", script}, args...)
}

// releaseGroup is for Windows (the job object handle).
func releaseGroup(*exec.Cmd) {}

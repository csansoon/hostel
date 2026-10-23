package room

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func psOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", args...).Output()
	return string(out), err
}

// processStart returns the start time of a process, to detect reused PIDs.
func processStart(pid int) (string, error) {
	out, err := psOutput("-o", "lstart=", "-p", strconv.Itoa(pid))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(out)
	if s == "" {
		return "", fmt.Errorf("no process %d", pid)
	}
	return s, nil
}

func processSession(pid int) (int, error) { return syscall.Getsid(pid) }

// userParents returns the processes of the room user, with their parent PIDs.
func userParents() (map[int]int, error) {
	out, err := psOutput("-U", strconv.Itoa(os.Getuid()), "-o", "pid=,ppid=")
	if err != nil {
		return nil, err
	}
	parents := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			parents[pid] = ppid
		}
	}
	return parents, nil
}

// envProcesses returns the processes of the room user whose environment has
// HOSTEL_LEASE=<id>. "ps -E" does not show the environment of Apple's own
// binaries (for example sh and sleep; macOS 27.2), so cleanup also finds
// processes by their session (design, section 17).
func envProcesses(leaseID string) ([]int, error) {
	out, err := psOutput("-E", "-ww", "-U", strconv.Itoa(os.Getuid()), "-o", "pid=,command=")
	if err != nil {
		return nil, err
	}
	want := "HOSTEL_LEASE=" + leaseID
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == os.Getpid() {
			continue
		}
		for _, f := range fields[1:] {
			if f == want {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

package room

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// leaseProcesses returns the processes of the room user whose environment has
// HOSTEL_LEASE=<id>. "ps -E" shows the environment of the user's own processes
// (verify on each macOS version; design, section 17).
func leaseProcesses(leaseID string) ([]int, error) {
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

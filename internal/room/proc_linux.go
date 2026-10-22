package room

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// processStart returns the start time of a process (field 22 of
// /proc/<pid>/stat), to detect reused PIDs.
func processStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// The command name (field 2) can contain spaces; it ends with the last ")".
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return "", fmt.Errorf("parse /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("parse /proc/%d/stat", pid)
	}
	return fields[19], nil
}

// leaseProcesses returns the processes of the room user whose environment has
// HOSTEL_LEASE=<id>.
func leaseProcesses(leaseID string) ([]int, error) {
	want := []byte("HOSTEL_LEASE=" + leaseID + "\x00")
	dirs, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return nil, err
	}
	var out []int
	for _, d := range dirs {
		pid, err := strconv.Atoi(filepath.Base(d))
		if err != nil || pid == os.Getpid() {
			continue
		}
		env, err := os.ReadFile(d + "/environ")
		if err != nil {
			continue // another user's process, or gone
		}
		if bytes.HasPrefix(env, want) || bytes.Contains(env, append([]byte{0}, want...)) {
			out = append(out, pid)
		}
	}
	return out, nil
}

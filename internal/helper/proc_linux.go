package helper

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// statFields returns the fields of /proc/<pid>/stat after the command name:
// the state is fields[0], the session fields[3], and the start time fields[19].
func statFields(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	// The command name (field 2) can contain spaces; it ends with the last ")".
	i := bytes.LastIndexByte(data, ')')
	if i < 0 {
		return nil, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 {
		return nil, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	return fields, nil
}

// processStart returns the start time of a process, to detect reused PIDs.
func processStart(pid int) (string, error) {
	fields, err := statFields(pid)
	if err != nil {
		return "", err
	}
	return fields[19], nil
}

// processStarts returns the start times of the processes in pids that run. A
// PID that is not in the map has no process.
func processStarts(pids []int) (map[int]string, error) {
	starts := map[int]string{}
	for _, pid := range pids {
		start, err := processStart(pid)
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return nil, err
		}
		starts[pid] = start
	}
	return starts, nil
}

func processSession(pid int) (int, error) {
	fields, err := statFields(pid)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(fields[3])
}

// userParents returns the processes of the machine user, with their parent PIDs.
func userParents() (map[int]int, error) {
	dirs, err := filepath.Glob("/proc/[0-9]*")
	if err != nil {
		return nil, err
	}
	parents := map[int]int{}
	for _, d := range dirs {
		pid, err := strconv.Atoi(filepath.Base(d))
		if err != nil {
			continue
		}
		if fi, err := os.Stat(d); err != nil || int(fi.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
			continue
		}
		if fields, err := statFields(pid); err == nil {
			if ppid, err := strconv.Atoi(fields[1]); err == nil {
				parents[pid] = ppid
			}
		}
	}
	return parents, nil
}

// envProcesses returns the processes of the machine user whose environment has
// HOSTEL_LEASE=<id>.
func envProcesses(leaseID string) ([]int, error) {
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

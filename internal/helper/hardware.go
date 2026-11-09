package helper

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// hardware reports the machine's CPU, memory, free disk of the home, and OS
// version for the status view (M10). Each part is best effort: an empty field
// means that it is not known.
func (h *helper) hardware() (protocol.Hardware, error) {
	hw := protocol.Hardware{Cores: runtime.NumCPU()}
	switch runtime.GOOS {
	case "linux":
		hw.CPU = procField("/proc/cpuinfo", "model name")
		if kb := procField("/proc/meminfo", "MemTotal"); kb != "" {
			n, _ := strconv.ParseInt(strings.Fields(kb)[0], 10, 64)
			hw.MemoryBytes = n * 1024
		}
		hw.OSVersion = osRelease()
	case "darwin":
		hw.CPU = command("sysctl", "-n", "machdep.cpu.brand_string")
		hw.MemoryBytes, _ = strconv.ParseInt(command("sysctl", "-n", "hw.memsize"), 10, 64)
		if v := command("sw_vers", "-productVersion"); v != "" {
			hw.OSVersion = "macOS " + v
		}
	case "windows":
		windowsHardware(&hw)
	}
	hw.DiskFreeBytes = diskFree(h.root)
	return hw, nil
}

// command runs a short command (deadline 20 s) and returns its trimmed output,
// or "".
func command(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(out), "\r", ""))
}

// procField returns the value of the first "key: value" line of a /proc file.
func procField(file, key string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func osRelease() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

package room

import (
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// cpuSample is the time over which the CPU use is measured.
const cpuSample = time.Second

// usage reports the room's CPU use (over cpuSample), memory use, and free disk
// of the home for the Hostel app (M10b). Each part is best effort.
func (h *helper) usage() (protocol.Usage, error) {
	u := protocol.Usage{DiskFreeBytes: diskFree(h.root)}
	switch runtime.GOOS {
	case "linux":
		idle1, total1 := procCPU()
		time.Sleep(cpuSample)
		idle2, total2 := procCPU()
		if total2 > total1 {
			u.CPUPercent = 100 * (1 - float64(idle2-idle1)/float64(total2-total1))
		}
		total, avail := procKB("MemTotal"), procKB("MemAvailable")
		if total > 0 && avail > 0 {
			u.MemoryTotalBytes, u.MemoryUsedBytes = total*1024, (total-avail)*1024
		}
	case "darwin":
		// top's second sample is the use over the last second.
		u.CPUPercent = topCPU(command("top", "-l", "2", "-n", "0", "-s", "1"))
		u.MemoryTotalBytes, _ = strconv.ParseInt(command("sysctl", "-n", "hw.memsize"), 10, 64)
		u.MemoryUsedBytes = vmStatUsed(command("vm_stat"))
	case "windows":
		windowsUsage(&u)
	}
	return u, nil
}

// procCPU returns the idle and total CPU time of /proc/stat.
func procCPU() (idle, total int64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	line, _, _ := strings.Cut(string(data), "\n")
	f := strings.Fields(line)
	for i, v := range f[1:] {
		n, _ := strconv.ParseInt(v, 10, 64)
		total += n
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}
	return idle, total
}

func procKB(key string) int64 {
	v := strings.Fields(procField("/proc/meminfo", key))
	if len(v) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(v[0], 10, 64)
	return n
}

var topIdle = regexp.MustCompile(`CPU usage: .*?([\d.]+)% idle`)

// topCPU is the CPU use of the last "CPU usage" line of macOS top.
func topCPU(out string) float64 {
	m := topIdle.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		return 0
	}
	idle, err := strconv.ParseFloat(m[len(m)-1][1], 64)
	if err != nil {
		return 0
	}
	return max(100-idle, 0)
}

var (
	vmPageSize = regexp.MustCompile(`page size of (\d+) bytes`)
	vmPages    = regexp.MustCompile(`(?m)^(Pages active|Pages wired down|Pages occupied by compressor):\s+(\d+)`)
)

// vmStatUsed is the used memory from macOS vm_stat: active, wired, and
// compressed pages.
func vmStatUsed(out string) int64 {
	m := vmPageSize.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	size, _ := strconv.ParseInt(m[1], 10, 64)
	var pages int64
	for _, p := range vmPages.FindAllStringSubmatch(out, -1) {
		n, _ := strconv.ParseInt(p[2], 10, 64)
		pages += n
	}
	return pages * size
}

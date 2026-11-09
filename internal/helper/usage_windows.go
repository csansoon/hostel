package helper

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/csansoon/hostel/internal/protocol"
)

// memoryStatus is MEMORYSTATUSEX.
type memoryStatus struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func globalMemory() (memoryStatus, bool) {
	var mem memoryStatus
	mem.Length = uint32(unsafe.Sizeof(mem))
	ok, _, _ := kernel32.NewProc("GlobalMemoryStatusEx").Call(uintptr(unsafe.Pointer(&mem)))
	return mem, ok != 0
}

// systemTimes returns the idle and total (kernel + user; kernel includes
// idle) CPU time.
func systemTimes() (idle, total uint64, ok bool) {
	var i, k, u windows.Filetime
	r, _, _ := kernel32.NewProc("GetSystemTimes").Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	ft := func(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	return ft(i), ft(k) + ft(u), r != 0
}

// windowsUsage reads the CPU use (two samples of the system times) and the
// memory from the kernel.
func windowsUsage(u *protocol.Usage) {
	i1, t1, ok1 := systemTimes()
	time.Sleep(cpuSample)
	i2, t2, ok2 := systemTimes()
	if ok1 && ok2 && t2 > t1 {
		u.CPUPercent = 100 * (1 - float64(i2-i1)/float64(t2-t1))
	}
	if mem, ok := globalMemory(); ok {
		u.MemoryTotalBytes, u.MemoryUsedBytes = int64(mem.TotalPhys), int64(mem.TotalPhys-mem.AvailPhys)
	}
}

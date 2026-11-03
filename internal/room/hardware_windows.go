package room

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/csansoon/hostel/internal/protocol"
)

// windowsHardware reads the CPU and the OS from the registry and the memory
// from the kernel: WMI (CIM) refuses SSH sessions of a standard user, such as
// the room user.
func windowsHardware(hw *protocol.Hardware) {
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		name, _, _ := k.GetStringValue("ProcessorNameString")
		hw.CPU = strings.TrimSpace(name)
		k.Close()
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		product, _, _ := k.GetStringValue("ProductName")
		build, _, _ := k.GetStringValue("CurrentBuild")
		display, _, _ := k.GetStringValue("DisplayVersion")
		k.Close()
		// ProductName says "Windows 10" also on Windows 11 (build 22000 and
		// later).
		if len(build) >= 5 && build >= "22000" {
			product = strings.Replace(product, "Windows 10", "Windows 11", 1)
		}
		hw.OSVersion = strings.Join(strings.Fields(product+" "+display+" (build "+build+")"), " ")
	}
	var mem struct {
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
	mem.Length = uint32(unsafe.Sizeof(mem))
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalMemoryStatusEx")
	if ok, _, _ := proc.Call(uintptr(unsafe.Pointer(&mem))); ok != 0 {
		hw.MemoryBytes = int64(mem.TotalPhys)
	}
}

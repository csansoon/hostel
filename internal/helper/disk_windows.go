package helper

import "golang.org/x/sys/windows"

// diskFree returns the free bytes of the volume of path, or 0.
func diskFree(path string) int64 {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var free, total, all uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &all); err != nil {
		return 0
	}
	return int64(free)
}

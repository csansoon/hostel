//go:build !windows

package room

import "syscall"

// diskFree returns the free bytes of the file system of path, or 0.
func diskFree(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

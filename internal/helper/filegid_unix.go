//go:build !windows

package helper

import (
	"io/fs"
	"syscall"
)

func fileGID(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Gid), true
}

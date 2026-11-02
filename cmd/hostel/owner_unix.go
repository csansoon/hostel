//go:build !windows

package main

import (
	"os"
	"syscall"
)

// fileOwner returns the user ID of a file's owner.
func fileOwner(fi os.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

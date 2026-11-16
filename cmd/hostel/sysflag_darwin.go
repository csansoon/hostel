package main

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// sfNoUnlink is SF_NOUNLINK ("sunlnk", sys/stat.h): only root sets it.
const sfNoUnlink = 0x00100000

// isSystemDir reports whether path (with its Lstat fi) is a directory that
// macOS made and protects, such as the skeleton of a user's directory in
// /private/var/folders: the flag sunlnk, or the System Integrity Protection
// attribute com.apple.rootless. A user can set neither, and root cannot
// remove such a directory. It must be on the device of its parent, so that a
// disk image that the user mounts there does not pass.
func isSystemDir(path string, fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.IsDir() {
		return false
	}
	pfi, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return false
	}
	pst, ok := pfi.Sys().(*syscall.Stat_t)
	if !ok || pst.Dev != st.Dev {
		return false
	}
	if st.Flags&sfNoUnlink != 0 {
		return true
	}
	_, err = unix.Lgetxattr(path, "com.apple.rootless", nil)
	return err == nil
}

package main

import (
	"os"
	"syscall"
)

// sfNoUnlink is SF_NOUNLINK ("sunlnk", sys/stat.h): only the system sets it.
const sfNoUnlink = 0x00100000

// isSystemDir reports whether fi is a directory that macOS made and protects
// (the flag sunlnk), such as the skeleton of a user's directory in
// /private/var/folders. Root cannot remove it either.
func isSystemDir(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.IsDir() && st.Flags&sfNoUnlink != 0
}

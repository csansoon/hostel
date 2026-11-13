package helper

import "syscall"

// clearFlags clears the user immutable and append flags (chflags uchg, uappnd).
func clearFlags(path string) { _ = syscall.Chflags(path, 0) }

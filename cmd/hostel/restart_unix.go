//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// restartSelf starts the new server binary in this process, with the same
// arguments and environment (a remote update, m10d-implementation.md).
func restartSelf(exe string) error {
	return fmt.Errorf("start the new binary %s: %w", exe, syscall.Exec(exe, os.Args, os.Environ()))
}

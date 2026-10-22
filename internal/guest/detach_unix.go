//go:build !windows

package guest

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts a process in a new session, so it survives the command
// that started it.
func startDetached(exe string, args []string, logFile *os.File) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

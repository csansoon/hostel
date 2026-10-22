package guest

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts a process without a console, so it survives the
// command that started it.
func startDetached(exe string, args []string, logFile *os.File) error {
	const detachedProcess, newProcessGroup = 0x00000008, 0x00000200
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | newProcessGroup}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

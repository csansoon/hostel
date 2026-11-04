package guest

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts a process without a console, so it survives the
// command that started it. It also asks to leave the job object of the
// caller: an agent tool that runs commands in a job that kills its processes
// on close would otherwise kill it too. A job that forbids that makes the
// start fail; then it starts inside the job.
func startDetached(exe string, args []string, logFile *os.File) error {
	const detachedProcess, newProcessGroup, breakawayFromJob = 0x00000008, 0x00000200, 0x01000000
	start := func(flags uint32) error {
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = logFile, logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	if err := start(detachedProcess | newProcessGroup | breakawayFromJob); err == nil {
		return nil
	}
	return start(detachedProcess | newProcessGroup)
}

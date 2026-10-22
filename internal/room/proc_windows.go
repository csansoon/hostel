package room

import (
	"os"
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd)                {}
func setSession(cmd *exec.Cmd)                     {}
func killProcessGroup(cmd *exec.Cmd) error         { return errWindows }
func signalGroup(pgid int, kill bool)              {}
func signalProcess(pid int, kill bool)             {}
func exitCode(st *os.ProcessState) int             { return st.ExitCode() }
func execProcess(string, []string, []string) error { return errWindows }
func processStart(pid int) (string, error)         { return "", errWindows }
func envProcesses(string) ([]int, error)           { return nil, errWindows }
func currentSession() (int, error)                 { return 0, errWindows }
func sessionProcesses(map[int]bool) ([]int, error) { return nil, nil }

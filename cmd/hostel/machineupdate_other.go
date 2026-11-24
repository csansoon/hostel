//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// updateRuleFile lets the machine user run only the root step of a remote
// update (M13) as root.
const updateRuleFile = "/etc/sudoers.d/hostel-update"

func callUpdateService() (string, error) {
	return "", errors.New("the update service is for Windows machines")
}

// installUpdateStep writes the sudo rule that lets the machine user run only
// "<bin> machine update --signed" as root, checked with visudo.
func installUpdateStep(_, bin string) error {
	rule := fmt.Sprintf("%s ALL=(root) NOPASSWD: %s machine update --signed\n", machineUser, bin)
	tmp := "/etc/sudoers.d/.hostel-update.tmp"
	if err := os.WriteFile(tmp, []byte(rule), 0o440); err != nil {
		return err
	}
	if err := runSetup("visudo", "-cf", tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, updateRuleFile)
}

// updateStepDir is the directory of the root step's files (release-key).
func updateStepDir(osName string) string { return serviceDir(osName) }

// readRootFile reads a regular file that only root can write.
func readRootFile(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !fi.Mode().IsRegular() || !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s must be a regular file that only root can write", path)
	}
	return os.ReadFile(path)
}

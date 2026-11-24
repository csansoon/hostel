package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/csansoon/hostel/internal/update"
)

// inRootService is true in the SYSTEM service; selfReplaced, when the update
// replaced the service's own binary (it restarts after its answer).
var inRootService, selfReplaced bool

// callUpdateService asks the SYSTEM service for the root step of a remote
// update (M13).
func callUpdateService() (string, error) {
	out, err := callLeaseService(leaseRequest{Op: "update"})
	if err != nil {
		return "", err
	}
	var res string
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	return res, nil
}

// installUpdateStep installs the SYSTEM service with bin. In the service
// itself it only replaces the service's binary: the service restarts after
// its answer (its recovery action).
func installUpdateStep(_, bin string) error {
	if !inRootService {
		return installRootService(bin, "")
	}
	exe := filepath.Join(leaseRootDir, "hostel.exe")
	data, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	f, err := update.TempNext(exe)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = update.Replace(f.Name(), exe)
	}
	if err != nil {
		os.Remove(f.Name())
		return err
	}
	selfReplaced = true
	return nil
}

// updateStepDir is the directory of the root step's files (release-key): the
// one that only SYSTEM and administrators can write, not the service
// directory, which the connection's account can change.
func updateStepDir(string) string { return leaseRootDir }

// readRootFile reads a file of updateStepDir (its access list protects it).
func readRootFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	return []byte(strings.TrimSpace(string(data))), err
}

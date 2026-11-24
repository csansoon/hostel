package main

import (
	"encoding/json"
	"os"
	"strings"
)

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

// installUpdateStep installs the SYSTEM service with bin (an administrator's
// "hostel machine update" or join; the signed update in the service replaces
// the service's binary itself).
func installUpdateStep(_, bin string) error {
	return installRootService(bin, "")
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

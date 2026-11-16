//go:build !windows

package main

import (
	"errors"
	"io"
)

func leaseUserWindows([]string, io.Writer, io.Writer) int { return 2 }

func installLeaseUserService(string) error {
	return errors.New("the lease user service is for Windows machines")
}

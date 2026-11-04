package main

import "errors"

// restartSelf: the server refuses a remote update on Windows.
func restartSelf(exe string) error {
	return errors.New("a server on Windows cannot restart itself; start it again")
}

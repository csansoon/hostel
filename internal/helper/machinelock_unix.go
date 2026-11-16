//go:build !windows

package helper

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockMachine holds a lock for the whole machine user (all rooms) until
// unlock: begin and cleanup decide in it whether another room has a lease.
func (h *helper) lockMachine() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(h.root, "machine.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

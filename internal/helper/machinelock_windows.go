package helper

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// lockMachine holds a lock for the whole machine user (all rooms) until
// unlock: begin and cleanup decide in it whether another room has a lease.
func (h *helper) lockMachine() (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(h.root, "machine.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

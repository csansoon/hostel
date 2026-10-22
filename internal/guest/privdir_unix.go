//go:build !windows

package guest

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir fails if the directory is not owned by the current user or
// can be used by other users.
func checkPrivateDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s must be a directory of the current user; remove it", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must have mode 0700 (it has %o); fix it with chmod 700", dir, info.Mode().Perm())
	}
	return nil
}

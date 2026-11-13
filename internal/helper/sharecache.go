package helper

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// shareCache runs as the machine user before a lease user is created (M11,
// design section 18): members of the group hostel-leases may traverse the home
// and hostel directories, read hostel/bin, tools, mise (to copy them), and the
// cache; nothing else of the machine user is open to the group or to other
// users.
func (h *helper) shareCache() (struct{}, error) {
	grp, err := user.LookupGroup("hostel-leases")
	if err != nil {
		return struct{}{}, fmt.Errorf("the group hostel-leases: %w", err)
	}
	gid, _ := strconv.Atoi(grp.Gid)
	home := h.home()
	share := map[string]bool{"bin": true, "tools": true, "mise": true, "cache": true}
	for _, dir := range []string{home, h.root} {
		des, err := os.ReadDir(dir)
		if err != nil {
			return struct{}{}, err
		}
		for _, de := range des {
			p := filepath.Join(dir, de.Name())
			switch {
			case dir == home && de.Name() == filepath.Base(h.root):
			case dir == h.root && share[de.Name()]:
				if err := shareTree(p, gid); err != nil {
					return struct{}{}, err
				}
			default:
				if err := chmodNoFollow(p, func(m fs.FileMode) fs.FileMode { return m &^ 0o077 }); err != nil {
					return struct{}{}, err
				}
			}
		}
		if err := os.Lchown(dir, -1, gid); err != nil {
			return struct{}{}, err
		}
		if err := os.Chmod(dir, mustMode(dir)&^0o077|0o010); err != nil {
			return struct{}{}, err
		}
	}
	return struct{}{}, nil
}

// shareTree gives the group read rights on the tree at p, and takes write
// rights from the group and other users: lease users read it, and cannot
// change it. It changes only what differs (the cache can be large).
func shareTree(p string, gid int) error {
	return filepath.WalkDir(p, func(q string, de fs.DirEntry, err error) error {
		if err != nil || de.Type()&fs.ModeSymlink != 0 {
			return err
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		if g, ok := fileGID(fi); !ok || g != gid {
			if err := os.Lchown(q, -1, gid); err != nil {
				return err
			}
		}
		m := fi.Mode().Perm()
		want := m&^0o022 | 0o040
		if fi.IsDir() || m&0o100 != 0 {
			want |= 0o010
		}
		if want == m {
			return nil
		}
		return os.Chmod(q, want)
	})
}

func chmodNoFollow(p string, mode func(fs.FileMode) fs.FileMode) error {
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
		return err
	}
	return os.Chmod(p, mode(fi.Mode())&fs.ModePerm)
}

func mustMode(p string) fs.FileMode {
	if fi, err := os.Stat(p); err == nil {
		return fi.Mode().Perm()
	}
	return 0o700
}

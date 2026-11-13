package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Persistence locations of the machine user (design, section 18, "Backdoors"):
// a program there runs after the lease without HOSTEL_LEASE. begin copies them
// when the machine has no other lease; cleanup of the last lease restores them.
var persistenceFiles = []string{
	".profile", ".bashrc", ".bash_profile", ".bash_login", ".bash_logout", ".bash_aliases",
	".gitconfig", ".config/git/config",
	".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zlogout",
	".config/fish/config.fish", ".ssh/rc", ".ssh/environment",
}

func persistenceDirs() []string {
	switch runtime.GOOS {
	case "linux":
		return []string{".config/systemd/user", ".config/autostart"}
	case "darwin":
		return []string{"Library/LaunchAgents"}
	}
	return nil
}

func (h *helper) persistenceBase() string { return filepath.Join(h.root, "persistence") }

// home is the machine user's home directory. Not $HOME: the shell startup
// files that a guest can change run before the helper and can set it.
func (h *helper) home() string { return filepath.Dir(h.root) }

// System commands by absolute path: a changed startup file can change PATH.
func systemCommand(name string) string {
	for _, dir := range []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if p := filepath.Join(dir, name); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// snapshotPersistence copies the persistence locations and the crontab. The
// file "complete" marks a full copy.
func (h *helper) snapshotPersistence() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	home := h.home()
	base := h.persistenceBase()
	if err := os.RemoveAll(base); err != nil {
		return err
	}
	files := filepath.Join(base, "files")
	if err := os.MkdirAll(files, 0o700); err != nil {
		return err
	}
	for _, rel := range append(persistenceDirs(), persistenceFiles...) {
		if err := copyTree(filepath.Join(home, rel), filepath.Join(files, rel)); err != nil {
			return fmt.Errorf("copy ~/%s: %w", rel, err)
		}
		// Links (dotfile managers, units linked from elsewhere): the copy has
		// the link; the content behind it is copied too, with its path.
		err := filepath.WalkDir(filepath.Join(home, rel), func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.Type()&fs.ModeSymlink == 0 {
				return nil
			}
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return nil // a dangling link: the link itself is compared
			}
			rp, _ := filepath.Rel(home, p)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(base, "links", rp)), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(base, "links", rp), []byte(real), 0o600); err != nil {
				return err
			}
			if err := copyTree(real, filepath.Join(base, "targets", rp)); err != nil {
				return fmt.Errorf("copy the target of ~/%s: %w", rp, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if tab, ok, err := readCrontab(); err != nil {
		return err
	} else if ok {
		if err := os.WriteFile(filepath.Join(base, "crontab"), tab, 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(base, "complete"), nil, 0o600)
}

// restorePersistence brings the persistence locations back to the copy of
// begin, when no other room has a lease (that lease's begin made no copy). It
// returns the items that it removed or restored.
func (h *helper) restorePersistence(room string) ([]string, error) {
	base := h.persistenceBase()
	if _, err := os.Stat(filepath.Join(base, "complete")); err != nil {
		return nil, nil
	}
	unlock, err := h.lockMachine()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if busy, err := h.otherRoomBusy(room); err != nil || busy {
		return nil, err
	}
	home := h.home()
	var items, failed []string
	// Stopping units and agents has one deadline; after it, cleanup fails.
	deadline := time.Now().Add(2 * time.Minute)
	// unloadAt is the path by which units are known (a link), when cur is the
	// target behind it.
	repair := func(cur, copyRoot, unloadAt, name string) {
		changed, err := diffTree(cur, copyRoot, "")
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", name, err))
			return
		}
		for _, c := range changed {
			p := filepath.Join(cur, c)
			at := p
			if unloadAt != "" {
				at = unloadAt
			}
			if err := unloadAgents(at, deadline); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			if err := restoreEntry(p, filepath.Join(copyRoot, c)); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", filepath.Join(name, c), err))
				continue
			}
			items = append(items, filepath.ToSlash(filepath.Join(name, c)))
		}
	}
	for _, rel := range append(persistenceDirs(), persistenceFiles...) {
		// A link to Hostel's own directory would compare the copy with itself.
		if dir, err := filepath.EvalSymlinks(filepath.Dir(filepath.Join(home, rel))); err == nil {
			if r, err := filepath.Rel(h.root, dir); err == nil && !strings.HasPrefix(r, "..") {
				failed = append(failed, fmt.Sprintf("~/%s: its directory is a link into %s", rel, h.root))
				continue
			}
		}
		repair(filepath.Join(home, rel), filepath.Join(base, "files", rel), "", "~/"+rel)
	}
	// The links are as in the copy now: the content behind them must be too.
	_ = filepath.WalkDir(filepath.Join(base, "links"), func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return nil
		}
		real, err := os.ReadFile(p)
		if err != nil {
			failed = append(failed, err.Error())
			return nil
		}
		rp, _ := filepath.Rel(filepath.Join(base, "links"), p)
		repair(string(real), filepath.Join(base, "targets", rp), filepath.Join(home, rp), string(real))
		return nil
	})
	if len(items) > 0 && runtime.GOOS == "linux" {
		runQuiet("systemctl", "--user", "daemon-reload")
	}
	if item, err := restoreCrontab(base); err != nil {
		failed = append(failed, "crontab: "+err.Error())
	} else if item != "" {
		items = append(items, item)
	}
	sort.Strings(items)
	if len(failed) > 0 {
		return items, fmt.Errorf("remove persistence: %s", strings.Join(failed, "; "))
	}
	return items, nil
}

// diffTree returns the paths under cur (relative, "" for cur itself) that are
// new, changed, or missing compared with the copy in base. A new directory is
// one entry.
func diffTree(cur, base, rel string) ([]string, error) {
	ci, cerr := os.Lstat(filepath.Join(cur, rel))
	bi, berr := os.Lstat(filepath.Join(base, rel))
	switch {
	case cerr != nil && !errors.Is(cerr, fs.ErrNotExist):
		return nil, cerr
	case berr != nil && !errors.Is(berr, fs.ErrNotExist):
		return nil, berr
	case cerr != nil && berr != nil:
		return nil, nil
	case cerr != nil || berr != nil:
		return []string{rel}, nil
	case ci.IsDir() && bi.IsDir():
		names := map[string]bool{}
		for _, d := range []string{filepath.Join(cur, rel), filepath.Join(base, rel)} {
			des, err := os.ReadDir(d)
			if err != nil && d == filepath.Join(cur, rel) {
				return []string{rel}, nil // for example mode 000: restore all of it
			} else if err != nil {
				return nil, err
			}
			for _, de := range des {
				names[de.Name()] = true
			}
		}
		var out []string
		for name := range names {
			sub, err := diffTree(cur, base, filepath.Join(rel, name))
			if err != nil {
				return nil, err
			}
			out = append(out, sub...)
		}
		return out, nil
	}
	same, err := sameEntry(filepath.Join(cur, rel), ci, filepath.Join(base, rel), bi)
	if err != nil || same {
		return nil, err
	}
	return []string{rel}, nil
}

func sameEntry(a string, ai fs.FileInfo, b string, bi fs.FileInfo) (bool, error) {
	if ai.Mode().Type() != bi.Mode().Type() || ai.Mode().Perm() != bi.Mode().Perm() {
		return false, nil
	}
	if ai.Mode()&fs.ModeSymlink != 0 {
		at, err := os.Readlink(a)
		if err != nil {
			return false, err
		}
		bt, err := os.Readlink(b)
		return at == bt, err
	}
	if !ai.Mode().IsRegular() || ai.Size() != bi.Size() {
		return false, nil
	}
	ad, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	bd, err := os.ReadFile(b)
	return bytes.Equal(ad, bd), err
}

// restoreEntry removes the entry at path and puts the copy back, if there is one.
func restoreEntry(path, copyPath string) error {
	if err := forceRemove(path); err != nil {
		return err
	}
	if _, err := os.Lstat(copyPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return copyTree(copyPath, path)
}

// copyTree copies a file, a symbolic link, or a directory tree. A missing src
// copies nothing.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if p == src && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		info, err := de.Info()
		if err != nil {
			return err
		}
		switch {
		case de.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		return nil // sockets and devices are not copied; diffTree reports them
	})
}

// unloadAgents stops the user units or agents at path (a file, or the files
// directly in a new directory) before they go, and checks that none of them
// still runs. A machine without a user manager (no systemd user instance) has
// no running unit. After the deadline, it fails, so that many files cannot
// leave units running.
func unloadAgents(path string, deadline time.Time) error {
	files := []string{path}
	if des, err := os.ReadDir(path); err == nil {
		files = files[:0]
		for _, de := range des {
			files = append(files, filepath.Join(path, de.Name()))
		}
	}
	uid := strconv.Itoa(os.Getuid())
	for _, f := range files {
		if time.Now().After(deadline) {
			return fmt.Errorf("too many units or agents to stop (stopped until %s)", f)
		}
		switch {
		case runtime.GOOS == "linux" && strings.Contains(f, "/.config/systemd/user/"):
			if runQuiet("systemctl", "--user", "show-environment") != nil {
				continue // no user manager
			}
			unit := filepath.Base(f)
			runQuiet("systemctl", "--user", "disable", "--now", "--", unit)
			if runQuiet("systemctl", "--user", "is-active", "--quiet", "--", unit) == nil {
				return fmt.Errorf("the user unit %s still runs", unit)
			}
		case runtime.GOOS == "darwin" && strings.HasSuffix(f, ".plist"):
			label, err := plistLabel(f)
			if err != nil || label == "" {
				continue // no label: launchd cannot have loaded it
			}
			for _, domain := range []string{"gui/" + uid, "user/" + uid} {
				runQuiet("launchctl", "bootout", domain+"/"+label)
				if runQuiet("launchctl", "print", domain+"/"+label) == nil {
					return fmt.Errorf("the agent %s still is loaded in %s", label, domain)
				}
			}
		}
	}
	return nil
}

func plistLabel(file string) (string, error) {
	cmd := systemCommand("plutil")
	if cmd == "" {
		return "", errors.New("no plutil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, cmd, "-extract", "Label", "raw", "-o", "-", file).Output()
	return strings.TrimSpace(string(out)), err
}

// runQuiet runs a system command with a deadline, without its output.
func runQuiet(name string, args ...string) error {
	cmd := systemCommand(name)
	if cmd == "" {
		return fmt.Errorf("no %s", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, cmd, args...).Run()
}

// forceRemove removes path also when the guest took away the write rights or
// set the macOS user immutable flag.
func forceRemove(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, de fs.DirEntry, werr error) error {
		if de != nil && de.Type()&fs.ModeSymlink == 0 {
			clearFlags(p)
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	clearFlags(filepath.Dir(path))
	return os.RemoveAll(path)
}

// readCrontab returns the user's crontab; ok is false when the machine has no
// crontab command.
func readCrontab() (tab []byte, ok bool, err error) {
	crontab := systemCommand("crontab")
	if crontab == "" {
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, crontab, "-l")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// "no crontab for <user>" (cronie, Debian, macOS), "can't open" (BusyBox).
		if msg := stderr.String(); strings.Contains(msg, "no crontab") || strings.Contains(msg, "can't open") {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("crontab -l: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, true, nil
}

func restoreCrontab(base string) (string, error) {
	want, err := os.ReadFile(filepath.Join(base, "crontab"))
	if errors.Is(err, fs.ErrNotExist) {
		if systemCommand("crontab") == "" {
			return "", nil
		}
		want = nil // the machine had no crontab
	} else if err != nil {
		return "", err
	}
	cur, ok, err := readCrontab()
	if err != nil || !ok || bytes.Equal(cur, want) {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if len(want) == 0 {
		cmd = exec.CommandContext(ctx, systemCommand("crontab"), "-r")
	} else {
		cmd = exec.CommandContext(ctx, systemCommand("crontab"), "-")
		cmd.Stdin = bytes.NewReader(want)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return "crontab", nil
}

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
	"slices"
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
		return []string{".config/systemd/user", ".config/systemd/user.control", ".local/share/systemd/user",
			".config/systemd/user-generators", ".config/systemd/user-environment-generators",
			".config/environment.d", ".config/autostart", ".config/fish/conf.d"}
	case "darwin":
		return []string{"Library/LaunchAgents", ".config/fish/conf.d"}
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
		if err := copyLinkTargets(home, rel, base, 0); err != nil {
			return err
		}
	}
	// The services, and the managers that existed ("#present" lines): a
	// manager that starts during the lease is checked too.
	names, present, err := loadedServices()
	if err != nil {
		return err
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		var lines []string
		for _, p := range present {
			lines = append(lines, "#present "+p)
		}
		lines = append(lines, names...)
		if err := os.WriteFile(filepath.Join(base, "services"), []byte(strings.Join(lines, "\n")), 0o600); err != nil {
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

// copyLinkTargets records each link under ~/rel (rel is relative to home)
// with the path and a copy of its target, and follows links to directories
// (at most 8 deep: a cycle ends there) for the links inside them.
func copyLinkTargets(home, rel, base string, depth int) error {
	if depth > 8 {
		return nil // a cycle of links, or a deep chain: its links are compared
	}
	return filepath.WalkDir(filepath.Join(home, rel), func(p string, de fs.DirEntry, err error) error {
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
		if fi, err := os.Stat(real); err == nil && fi.IsDir() {
			// WalkDir does not enter a linked directory: the links inside it
			// are reached through this link's path.
			des, err := os.ReadDir(real)
			if err != nil {
				return err
			}
			for _, d := range des {
				if err := copyLinkTargets(home, filepath.Join(rp, d.Name()), base, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// restorePersistence brings the persistence locations back to the copy of
// begin, when no other room has a lease (that lease's begin made no copy). It
// returns the items that it removed or restored.
func (h *helper) restorePersistence(room string) ([]string, error) {
	unlock, err := h.lockMachine()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return h.restorePersistenceLocked(room)
}

// restorePersistenceLocked is restorePersistence for a caller that holds the
// machine lock.
func (h *helper) restorePersistenceLocked(room string) ([]string, error) {
	base := h.persistenceBase()
	if _, err := os.Stat(filepath.Join(base, "complete")); err != nil {
		return nil, nil
	}
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
				at = filepath.Join(unloadAt, c)
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
	if len(items) > 0 && runtime.GOOS == "linux" && userManager() {
		if err := runQuiet("systemctl", "--user", "daemon-reload"); err != nil {
			failed = append(failed, "systemctl --user daemon-reload: "+err.Error())
		}
	}
	// Services loaded since the copy, also from a changed file or with no
	// file (systemd-run --user).
	if before, err := os.ReadFile(filepath.Join(base, "services")); err == nil {
		stopped, err := stopNewServices(strings.Split(string(before), "\n"))
		items = append(items, stopped...)
		if err != nil {
			failed = append(failed, err.Error())
		}
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
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		des, err := os.ReadDir(path)
		if err != nil {
			// A guest can take away the rights (mode 000); the user owns it.
			if cerr := os.Chmod(path, 0o700); cerr != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			if des, err = os.ReadDir(path); err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
		}
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
		case runtime.GOOS == "linux" && unitFile(f):
			if !userManager() {
				continue // no user manager: no unit runs
			}
			// A drop-in (foo.service.d/x.conf) changes its parent unit.
			unit := filepath.Base(f)
			if d := filepath.Base(filepath.Dir(f)); strings.HasSuffix(d, ".d") {
				unit = strings.TrimSuffix(d, ".d")
			} else if strings.HasSuffix(unit, ".d") {
				unit = strings.TrimSuffix(unit, ".d")
			}
			runQuiet("systemctl", "--user", "disable", "--now", "--", unit)
			if err := unitStopped(unit); err != nil {
				return err
			}
		case runtime.GOOS == "darwin" && strings.HasSuffix(f, ".plist"):
			label, err := plistLabel(f)
			if err != nil {
				// A guest can change the file of a loaded agent.
				return fmt.Errorf("read the label of %s: %w", f, err)
			}
			for _, domain := range []string{"gui/" + uid, "user/" + uid} {
				runQuiet("launchctl", "bootout", domain+"/"+label)
				if err := agentGone(domain + "/" + label); err != nil {
					return err
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

// loadedServices lists the loaded user units and services: systemd units of
// the user manager (present: "systemd"), or launchd services of the user's
// gui and user domains ("<domain>/<label>"; present: each domain).
func loadedServices() (names, present []string, err error) {
	switch runtime.GOOS {
	case "linux":
		if !userManager() {
			return nil, nil, nil
		}
		out, err := outputQuiet("systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "--no-pager",
			"--type=service,timer,socket,path,scope")
		if err != nil {
			return nil, nil, fmt.Errorf("list the user units: %w", err)
		}
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) > 0 {
				names = append(names, f[0])
			}
		}
		return names, []string{"systemd"}, nil
	case "darwin":
		uid := strconv.Itoa(os.Getuid())
		for _, domain := range []string{"gui/" + uid, "user/" + uid} {
			out, err := outputQuiet("launchctl", "print", domain)
			var exit *exec.ExitError
			if errors.As(err, &exit) && (exit.ExitCode() == 112 || strings.Contains(string(exit.Stderr), "Could not find domain")) {
				continue // no such domain (no gui session)
			}
			if err != nil {
				return nil, nil, fmt.Errorf("launchctl print %s: %w", domain, err)
			}
			present = append(present, domain)
			in := false
			for _, line := range strings.Split(out, "\n") {
				switch t := strings.TrimSpace(line); {
				case t == "services = {":
					in = true
				case in && t == "}":
					in = false
				case in:
					if f := strings.Fields(t); len(f) > 0 {
						name := domain + "/" + f[len(f)-1]
						// Not Apple's: with the plist that it came from, so
						// that a replaced service of the same label is new.
						if !strings.HasPrefix(f[len(f)-1], "com.apple.") {
							name += "\t" + servicePath(name)
						}
						names = append(names, name)
					}
				}
			}
		}
		return names, present, nil
	}
	return nil, nil, nil
}

// servicePath is the plist path of a loaded launchd service ("path = ...").
func servicePath(service string) string {
	out, err := outputQuiet("launchctl", "print", service)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "path = "); ok {
			return p
		}
	}
	return ""
}

// unitStopped checks that a user unit does not run: is-active exits with 3
// (inactive, failed) or 4 (no such unit); another failure is no answer.
func unitStopped(unit string) error {
	err := runQuiet("systemctl", "--user", "is-active", "--quiet", "--", unit)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return fmt.Errorf("the user unit %s still runs", unit)
	case errors.As(err, &exit) && (exit.ExitCode() == 3 || exit.ExitCode() == 4):
		return nil
	}
	return fmt.Errorf("the state of the user unit %s: %w", unit, err)
}

// unitFile reports whether f is in a directory of systemd user units.
func unitFile(f string) bool {
	for _, d := range []string{"/.config/systemd/user/", "/.config/systemd/user.control/", "/.local/share/systemd/user/"} {
		if strings.Contains(f, d) {
			return true
		}
	}
	return false
}

// agentGone checks that a launchd service is not loaded: print exits with 113
// ("Could not find service") or 112 (no such domain); another failure is no
// answer.
func agentGone(target string) error {
	err := runQuiet("launchctl", "print", target)
	var exit *exec.ExitError
	switch {
	case err == nil:
		return fmt.Errorf("the agent %s still is loaded", target)
	case errors.As(err, &exit) && (exit.ExitCode() == 113 || exit.ExitCode() == 112):
		return nil
	}
	return fmt.Errorf("the state of the agent %s: %w", target, err)
}

// userManager reports whether the systemd user manager of this user runs: it
// has its private socket. Then a failed systemctl call is a failure, not a
// machine without units.
func userManager() bool {
	_, err := os.Stat(filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "systemd", "private"))
	return err == nil
}

// stopNewServices stops the units and services that were not loaded at the
// copy (before: its lines), and fails for one that still runs. A systemd user
// manager that did not exist then is stopped as a whole. On macOS, Apple's
// services (macOS starts them at any time) and a domain that did not exist then
// (a person signed in) are left.
func stopNewServices(before []string) ([]string, error) {
	had, present := map[string]bool{}, map[string]bool{}
	for _, l := range before {
		if p, ok := strings.CutPrefix(l, "#present "); ok {
			present[p] = true
		} else if l != "" {
			had[l] = true
		}
	}
	now, nowPresent, err := loadedServices()
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "linux" && slices.Contains(nowPresent, "systemd") && !present["systemd"] {
		runQuiet("systemctl", "--user", "exit")
		for deadline := time.Now().Add(15 * time.Second); userManager(); time.Sleep(200 * time.Millisecond) {
			if time.Now().After(deadline) {
				return nil, errors.New("the user manager that started during the lease still runs")
			}
		}
		return []string{"the user manager"}, nil
	}
	var stopped, failed []string
	for _, n := range now {
		if had[n] {
			continue
		}
		switch runtime.GOOS {
		case "linux":
			runQuiet("systemctl", "--user", "stop", "--", n)
			if err := unitStopped(n); err != nil {
				failed = append(failed, err.Error())
				continue
			}
		case "darwin":
			n, _, _ = strings.Cut(n, "\t")
			domain, label := n[:strings.LastIndex(n, "/")], n[strings.LastIndex(n, "/")+1:]
			if !present[domain] || strings.HasPrefix(label, "com.apple.") {
				continue
			}
			runQuiet("launchctl", "bootout", n)
			if err := agentGone(n); err != nil {
				failed = append(failed, err.Error())
				continue
			}
		}
		stopped = append(stopped, "service "+n)
	}
	if len(failed) > 0 {
		return stopped, errors.New(strings.Join(failed, "; "))
	}
	return stopped, nil
}

func outputQuiet(name string, args ...string) (string, error) {
	cmd := systemCommand(name)
	if cmd == "" {
		return "", fmt.Errorf("no %s", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	c.Env = systemEnv()
	out, err := c.Output()
	return string(out), err
}

// systemEnv is the environment of system commands: systemctl --user finds
// the user manager through XDG_RUNTIME_DIR, which an SSH session can lack.
func systemEnv() []string {
	env := os.Environ()
	if os.Getenv("XDG_RUNTIME_DIR") == "" && runtime.GOOS == "linux" {
		env = append(env, "XDG_RUNTIME_DIR=/run/user/"+strconv.Itoa(os.Getuid()))
	}
	return env
}

// runQuiet runs a system command with a deadline, without its output.
func runQuiet(name string, args ...string) error {
	cmd := systemCommand(name)
	if cmd == "" {
		return fmt.Errorf("no %s", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	c.Env = systemEnv()
	return c.Run()
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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/protocol"
)

// The root step of lease users (M11, design section 18): a root-owned copy of
// this binary runs "hostel lease-user ..." through a sudo rule that allows
// only this command for the machine user. It trusts nothing from the caller
// but a user name of a fixed form and a public key.

const (
	leaseUserVersion = 1
	leaseUserMarker  = "Hostel lease"
	leaseGroup       = "hostel-leases"
)

var leaseUserName = regexp.MustCompile(`^hostel-[0-9a-z]{12}$`)

func leaseUserCmd(args []string, stdout, stderr io.Writer) int {
	usage := "usage: hostel lease-user check | list | create <name> | delete <name> (as root, through sudo)"
	if len(args) == 0 {
		fmt.Fprintln(stderr, "hostel:", usage)
		return 2
	}
	var out any
	var err error
	switch {
	case runtime.GOOS != "linux" && runtime.GOOS != "darwin":
		err = errors.New("lease users are for Linux and macOS machines")
	case os.Geteuid() != 0:
		err = errors.New("lease-user must run as root (sudo -n /etc/hostel/hostel lease-user ...)")
	case args[0] == "check" && len(args) == 1:
		out = protocol.LeaseUserCheck{Version: leaseUserVersion}
	case args[0] == "list" && len(args) == 1:
		var users []string
		users, err = leaseUsers()
		out = protocol.LeaseUserList{Users: users}
	case args[0] == "create" && len(args) == 2:
		out, err = createLeaseUser(args[1], os.Stdin)
	case args[0] == "delete" && len(args) == 2:
		err = deleteLeaseUser(args[1])
		out = struct{}{}
	default:
		fmt.Fprintln(stderr, "hostel:", usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "hostel: lease-user:", err)
		return 1
	}
	json.NewEncoder(stdout).Encode(out)
	return 0
}

// machineUserOf is the user that called sudo: the machine user.
func machineUserOf() (*user.User, error) {
	uid := os.Getenv("SUDO_UID")
	if uid == "" || uid == "0" {
		return nil, errors.New("run it through sudo as the machine user")
	}
	return user.LookupId(uid)
}

func checkLeaseUserName(name string) error {
	if !leaseUserName.MatchString(name) {
		return fmt.Errorf("invalid lease user name %q", name)
	}
	return nil
}

// isLeaseUser reports whether name exists, and fails for a user that this
// command did not create.
func isLeaseUser(name string) (bool, error) {
	if _, err := user.Lookup(name); err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return false, nil
		}
		return false, err
	}
	var gecos string
	if runtime.GOOS == "darwin" {
		out, err := setupOutput("dscl", ".", "-read", "/Users/"+name, "RealName")
		if err != nil {
			return false, err
		}
		gecos = out
	} else {
		out, err := setupOutput("getent", "passwd", name)
		if err != nil {
			return false, err
		}
		if f := strings.Split(strings.TrimSpace(out), ":"); len(f) >= 5 {
			gecos = f[4]
		}
	}
	if !strings.Contains(gecos, leaseUserMarker) {
		return false, fmt.Errorf("the user %s exists and is not a Hostel lease user", name)
	}
	return true, nil
}

func leaseUsers() ([]string, error) {
	var names []string
	if runtime.GOOS == "darwin" {
		out, err := setupOutput("dscl", ".", "-list", "/Users")
		if err != nil {
			return nil, err
		}
		names = strings.Fields(out)
	} else {
		data, err := os.ReadFile("/etc/passwd")
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			names = append(names, strings.SplitN(line, ":", 2)[0])
		}
	}
	var out []string
	for _, n := range names {
		if !leaseUserName.MatchString(n) {
			continue
		}
		if ok, err := isLeaseUser(n); err == nil && ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func createLeaseUser(name string, stdin io.Reader) (protocol.LeaseUserCreated, error) {
	var resp protocol.LeaseUserCreated
	if err := checkLeaseUserName(name); err != nil {
		return resp, err
	}
	var req protocol.LeaseUserCreate
	if err := json.NewDecoder(io.LimitReader(stdin, 64<<10)).Decode(&req); err != nil {
		return resp, fmt.Errorf("read the request: %w", err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(req.PublicKey))
	if err != nil {
		return resp, fmt.Errorf("the public key: %w", err)
	}
	mu, err := machineUserOf()
	if err != nil {
		return resp, err
	}
	if err := ensureLeaseGroup(); err != nil {
		return resp, err
	}
	exists, err := isLeaseUser(name)
	if err != nil {
		return resp, err
	}
	if !exists {
		if err := addLeaseUser(name); err != nil {
			return resp, err
		}
	}
	u, err := user.Lookup(name)
	if err != nil {
		return resp, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if err := shareMachineCache(mu.HomeDir); err != nil {
		return resp, fmt.Errorf("share the machine's cache: %w", err)
	}
	if err := fillLeaseHome(u.HomeDir, mu.HomeDir, ssh.MarshalAuthorizedKey(key)); err != nil {
		return resp, err
	}
	if err := chownTree(u.HomeDir, uid, gid); err != nil {
		return resp, err
	}
	if err := os.Chmod(u.HomeDir, 0o700); err != nil {
		return resp, err
	}
	resp.Home = u.HomeDir
	return resp, nil
}

func ensureLeaseGroup() error {
	if _, err := user.LookupGroup(leaseGroup); err == nil {
		return nil
	}
	if runtime.GOOS == "darwin" {
		return runSetup("dseditgroup", "-o", "create", "-r", "Hostel leases", leaseGroup)
	}
	return runSetup("groupadd", "-f", leaseGroup)
}

func addLeaseUser(name string) error {
	if runtime.GOOS != "darwin" {
		if err := runSetup("useradd", "--create-home", "--shell", "/bin/bash", "--comment", leaseUserMarker,
			"--groups", leaseGroup, name); err != nil {
			return err
		}
		// "*": no password login, but key login (as for the machine user).
		return runSetup("usermod", "--password", "*", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// sysadminctl takes the password as an argument: the error must not show
	// the arguments.
	if out, err := exec.CommandContext(ctx, "sysadminctl", "-addUser", name, "-fullName", leaseUserMarker,
		"-password", randomPassword(), "-home", "/Users/"+name).CombinedOutput(); err != nil {
		return fmt.Errorf("sysadminctl -addUser %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	if err := runSetup("createhomedir", "-c", "-u", name); err != nil {
		return err
	}
	if err := runSetup("dscl", ".", "-create", "/Users/"+name, "IsHidden", "1"); err != nil {
		return err
	}
	return runSetup("dseditgroup", "-o", "edit", "-a", name, "-t", "user", leaseGroup)
}

// shareMachineCache lets members of the lease group read the machine user's
// shared cache (hostel/cache: mise installs, SDKs) and nothing else there: the
// home and hostel directories get the group with traverse rights only, and
// their other entries lose the rights of other users.
func shareMachineCache(home string) error {
	grp, err := user.LookupGroup(leaseGroup)
	if err != nil {
		return err
	}
	gid, _ := strconv.Atoi(grp.Gid)
	hostel := filepath.Join(home, "hostel")
	for _, dir := range []string{home, hostel} {
		des, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, de := range des {
			if dir == hostel && de.Name() == "cache" {
				continue
			}
			if dir == home && de.Name() == "hostel" {
				continue
			}
			p := filepath.Join(dir, de.Name())
			fi, err := os.Lstat(p)
			if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
				continue
			}
			if err := os.Chmod(p, fi.Mode().Perm()&^0o007); err != nil {
				return err
			}
		}
	}
	cache := filepath.Join(hostel, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	for _, dir := range []string{home, hostel, cache} {
		fi, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", dir)
		}
		if err := os.Lchown(dir, -1, gid); err != nil {
			return err
		}
		if err := os.Chmod(dir, fi.Mode().Perm()|0o010); err != nil {
			return err
		}
	}
	return nil
}

// fillLeaseHome gives the lease user the server key and its own hostel
// directory: copies of the machine's helper, mise, tools, and mise
// configuration, its own cache with links to the machine's shared entries
// (not mise's own directories), and an empty rooms directory.
func fillLeaseHome(home, machineHome string, key []byte) error {
	ssh := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ssh, "authorized_keys"), key, 0o600); err != nil {
		return err
	}
	src := filepath.Join(machineHome, "hostel")
	dst := filepath.Join(home, "hostel")
	for _, d := range []string{"bin", "tools", "mise"} {
		if err := os.RemoveAll(filepath.Join(dst, d)); err != nil {
			return err
		}
		if err := copyDir(filepath.Join(src, d), filepath.Join(dst, d)); err != nil {
			return fmt.Errorf("copy hostel/%s: %w", d, err)
		}
	}
	cache := filepath.Join(dst, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return err
	}
	des, err := os.ReadDir(filepath.Join(src, "cache"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, de := range des {
		if de.Name() == "mise" || de.Name() == "mise-cache" {
			continue
		}
		link := filepath.Join(cache, de.Name())
		if _, err := os.Lstat(link); err == nil {
			continue
		}
		if err := os.Symlink(filepath.Join(src, "cache", de.Name()), link); err != nil {
			return err
		}
	}
	return os.MkdirAll(filepath.Join(dst, "rooms"), 0o755)
}

// copyDir copies regular files, directories, and symbolic links. A missing src
// copies nothing.
func copyDir(src, dst string) error {
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
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
		return nil
	})
}

func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}

// deleteLeaseUser ends all processes of the user, removes its crontab, its
// files in the shared temporary directories, the user, and its home
// directory. A missing user is not an error.
func deleteLeaseUser(name string) error {
	if err := checkLeaseUserName(name); err != nil {
		return err
	}
	exists, err := isLeaseUser(name)
	if err != nil || !exists {
		return err
	}
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		runQuietSetup("launchctl", "bootout", "user/"+u.Uid)
		runQuietSetup("launchctl", "bootout", "gui/"+u.Uid)
	} else {
		runQuietSetup("loginctl", "terminate-user", name)
	}
	if err := killUser(name); err != nil {
		return err
	}
	runQuietSetup("crontab", "-u", name, "-r")
	tmp := []string{"/tmp", "/var/tmp", "/dev/shm"}
	if runtime.GOOS == "darwin" {
		tmp = []string{"/private/tmp", "/private/var/tmp", "/private/var/folders"}
	}
	for _, d := range tmp {
		if _, err := os.Stat(d); err == nil {
			runQuietSetup("find", d, "-xdev", "-user", name, "-prune", "-exec", "rm", "-rf", "{}", "+")
		}
	}
	if runtime.GOOS == "darwin" {
		if err := runSetup("sysadminctl", "-deleteUser", name); err != nil {
			return err
		}
	} else if err := runSetup("userdel", "--remove", name); err != nil {
		if _, lerr := user.Lookup(name); lerr == nil {
			return err
		}
		// userdel can fail after it removed the user (for example without a
		// mail spool); the home directory goes below.
	}
	if filepath.Base(u.HomeDir) == name {
		return os.RemoveAll(u.HomeDir)
	}
	return nil
}

// killUser sends SIGKILL to all processes of the user until none is left.
func killUser(name string) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		runQuietSetup("pkill", "-KILL", "-u", name)
		if _, err := setupOutput("pgrep", "-u", name); err != nil {
			return nil // exit code 1: no process
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes of %s still run after SIGKILL", name)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func setupOutput(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

func runQuietSetup(name string, args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, name, args...).Run()
}

// installLeaseUserRule writes the sudo rule that lets the machine user run
// only "<bin> lease-user ..." as root, and checks it with visudo.
func installLeaseUserRule(osName, bin string) error {
	if osName != "linux" && osName != "macos" {
		return errors.New("lease users are for macOS and Linux machines")
	}
	rule := fmt.Sprintf("%s ALL=(root) NOPASSWD: %s lease-user *\n", machineUser, bin)
	tmp := "/etc/sudoers.d/.hostel.tmp"
	if err := os.WriteFile(tmp, []byte(rule), 0o440); err != nil {
		return err
	}
	if err := runSetup("visudo", "-cf", tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, "/etc/sudoers.d/hostel")
}

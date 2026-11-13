package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
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
// only this command for the machine user. Its caller can be a guest on the
// machine user, so it takes nothing from the caller but a user name of a fixed
// form, and it never follows a path that a user other than root controls: it
// creates and deletes users, and writes only into a home directory that it
// has just created. The machine user shares its cache itself, and the lease
// user fills its own home.

const (
	leaseUserVersion = 2
	leaseUserMarker  = "Hostel lease"
	leaseGroup       = "hostel-leases"
	// leaseServerKey has the server key for lease users (root-owned; "machine
	// join --lease-users" writes it).
	leaseServerKey = "/etc/hostel/server_key.pub"
	// leaseUIDFile has the next user ID: IDs are not used twice, so a file of
	// an earlier lease user never belongs to a later one.
	leaseUIDFile  = "/etc/hostel/lease-uid"
	firstLeaseUID = 61000
)

var leaseUserName = regexp.MustCompile(`^hostel-[0-9a-z]{12}$`)

func leaseUserCmd(args []string, stdout, stderr io.Writer) int {
	usage := "usage: hostel lease-user check | list | create <name> | delete <name> (as root, through sudo)"
	if len(args) == 0 {
		fmt.Fprintln(stderr, "hostel:", usage)
		return 2
	}
	// Commands only from the system directories: sudo can keep the caller's PATH.
	os.Setenv("PATH", "/usr/sbin:/usr/bin:/sbin:/bin")
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
		out, err = createLeaseUser(args[1])
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
		gecos = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "RealName:"))
	} else {
		out, err := setupOutput("getent", "passwd", name)
		if err != nil {
			return false, err
		}
		if f := strings.Split(strings.TrimSpace(out), ":"); len(f) >= 5 {
			gecos = f[4]
		}
	}
	if gecos != leaseUserMarker {
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
		ok, err := isLeaseUser(n)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// createLeaseUser makes a new user (an existing one of the same name goes
// first: nothing of its lease began), with the server key from a root-owned
// file. The machine user joins the lease group, so that it can share its cache.
func createLeaseUser(name string) (protocol.LeaseUserCreated, error) {
	var resp protocol.LeaseUserCreated
	if err := checkLeaseUserName(name); err != nil {
		return resp, err
	}
	keyData, err := os.ReadFile(leaseServerKey)
	if err != nil {
		return resp, fmt.Errorf("the server key: %w (hostel machine join --lease-users writes it)", err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(keyData)
	if err != nil {
		return resp, fmt.Errorf("the server key in %s: %w", leaseServerKey, err)
	}
	machineUser := os.Getenv("SUDO_USER")
	if machineUser == "" || machineUser == "root" {
		return resp, errors.New("run it through sudo as the machine user")
	}
	if err := ensureLeaseGroup(machineUser); err != nil {
		return resp, err
	}
	if exists, err := isLeaseUser(name); err != nil {
		return resp, err
	} else if exists {
		if err := deleteLeaseUser(name); err != nil {
			return resp, err
		}
	}
	uid, err := nextLeaseUID()
	if err != nil {
		return resp, err
	}
	if err := addLeaseUser(name, uid); err != nil {
		return resp, err
	}
	u, err := user.Lookup(name)
	if err != nil {
		return resp, err
	}
	gid, _ := strconv.Atoi(u.Gid)
	// The home is new, and the user cannot log in before its key exists.
	home, err := os.OpenRoot(u.HomeDir)
	if err != nil {
		return resp, err
	}
	defer home.Close()
	if err := home.Mkdir(".ssh", 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return resp, err
	}
	if err := home.WriteFile(".ssh/authorized_keys", ssh.MarshalAuthorizedKey(key), 0o600); err != nil {
		return resp, err
	}
	for _, p := range []string{".", ".ssh", ".ssh/authorized_keys"} {
		if err := home.Lchown(p, uid, gid); err != nil {
			return resp, err
		}
	}
	if err := home.Chmod(".", 0o700); err != nil {
		return resp, err
	}
	resp.Home = u.HomeDir
	return resp, nil
}

func ensureLeaseGroup(machineUser string) error {
	if _, err := user.LookupGroup(leaseGroup); err != nil {
		if runtime.GOOS == "darwin" {
			err = runSetup("dseditgroup", "-o", "create", "-r", "Hostel leases", leaseGroup)
		} else {
			err = runSetup("groupadd", "-f", leaseGroup)
		}
		if err != nil {
			return err
		}
	}
	if runtime.GOOS == "darwin" {
		return runSetup("dseditgroup", "-o", "edit", "-a", machineUser, "-t", "user", leaseGroup)
	}
	return runSetup("usermod", "-aG", leaseGroup, machineUser)
}

func nextLeaseUID() (int, error) {
	uid := firstLeaseUID
	data, err := os.ReadFile(leaseUIDFile)
	switch {
	case err == nil:
		// Fail closed: a damaged counter must not start again at the first ID.
		n, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if perr != nil || n < firstLeaseUID {
			return 0, fmt.Errorf("%s is damaged (%q); write the next free user ID into it", leaseUIDFile, strings.TrimSpace(string(data)))
		}
		uid = n
	case !errors.Is(err, os.ErrNotExist):
		return 0, err
	}
	for {
		if _, err := user.LookupId(strconv.Itoa(uid)); err != nil {
			break
		}
		uid++
	}
	tmp := leaseUIDFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(uid+1)+"\n"), 0o644); err != nil {
		return 0, err
	}
	if f, err := os.Open(tmp); err == nil {
		f.Sync()
		f.Close()
	}
	return uid, os.Rename(tmp, leaseUIDFile)
}

func addLeaseUser(name string, uid int) error {
	id := strconv.Itoa(uid)
	if runtime.GOOS != "darwin" {
		// Its own primary group, and the lease group for the shared cache.
		if err := runSetup("useradd", "--create-home", "--uid", id, "--shell", "/bin/bash", "--comment", leaseUserMarker,
			"--groups", leaseGroup, name); err != nil {
			return err
		}
		// "*": no password login, but key login (as for the machine user).
		return runSetup("usermod", "--password", "*", name)
	}
	grp, err := user.LookupGroup(leaseGroup)
	if err != nil {
		return err
	}
	home := "/Users/" + name
	for _, kv := range [][]string{
		// The marker first: a record without it cannot be deleted later.
		{"RealName", leaseUserMarker}, {"UniqueID", id}, {"PrimaryGroupID", grp.Gid},
		{"UserShell", "/bin/zsh"}, {"NFSHomeDirectory", home}, {"IsHidden", "1"}, {"Password", "*"},
	} {
		if err := runSetup("dscl", ".", "-create", "/Users/"+name, kv[0], kv[1]); err != nil {
			return err
		}
	}
	for _, g := range macGroups() {
		if err := runSetup("dseditgroup", "-o", "edit", "-a", name, "-t", "user", g); err != nil {
			return err
		}
	}
	return runSetup("createhomedir", "-c", "-u", name)
}

// deleteLeaseUser ends all processes of the user, removes its crontab, its at
// jobs, and its files in the shared temporary directories (as the user, so a
// link there cannot make root delete other files), then the user and its home
// directory. A missing user is not an error.
func deleteLeaseUser(name string) error {
	if err := checkLeaseUserName(name); err != nil {
		return err
	}
	exists, err := isLeaseUser(name)
	if err != nil {
		return err
	}
	if !exists {
		// A deletion that stopped after the user record went.
		return removeLeaseHome(leaseHome(name))
	}
	u, err := user.Lookup(name)
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	if runtime.GOOS == "darwin" {
		runQuietSetup("launchctl", "bootout", "user/"+u.Uid)
		runQuietSetup("launchctl", "bootout", "gui/"+u.Uid)
	} else {
		runQuietSetup("loginctl", "terminate-user", name)
	}
	if err := killUser(name); err != nil {
		return err
	}
	if err := removeScheduledJobs(name); err != nil {
		return err
	}
	tmp := []string{"/tmp", "/var/tmp", "/dev/shm"}
	if runtime.GOOS == "darwin" {
		// /Users/Shared: all local users can write it.
		tmp = []string{"/private/tmp", "/private/var/tmp", "/private/var/folders", "/Users/Shared"}
	}
	for _, d := range tmp {
		if _, err := os.Stat(d); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			// As the user: its own rights and flags first (mode 000; macOS
			// uchg, uappnd), then its files.
			script := `find "$1" -xdev -user "$2" -type d -exec chmod u+rwx {} + 2>/dev/null; find "$1" -xdev -user "$2" -delete`
			if runtime.GOOS == "darwin" {
				script = `find "$1" -xdev -user "$2" -exec chflags -h nouchg,nouappnd {} + 2>/dev/null; ` + script
			}
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script, "sh", d, name)
			asUser(cmd, uid, gid)
			// BSD find opens the current directory, which the user may not read.
			cmd.Dir = "/"
			_ = cmd.Run() // fails for directories of other users that it cannot read
			cancel()
			// Root only looks: is a file of the user left?
			if left, _ := setupOutput("find", d, "-xdev", "-user", name, "-print", "-quit"); strings.TrimSpace(left) != "" {
				return fmt.Errorf("a file of %s is left in %s: %s", name, d, strings.TrimSpace(left))
			}
		}
	}
	if err := killUser(name); err != nil {
		return err
	}
	// The home before the user record: a repeated deletion finds the user
	// again until both are gone.
	if u.HomeDir != leaseHome(name) {
		return fmt.Errorf("the home directory of %s is %s, not %s", name, u.HomeDir, leaseHome(name))
	}
	if err := removeLeaseHome(u.HomeDir); err != nil {
		return err
	}
	if runtime.GOOS == "darwin" {
		// A deleted user stays in the member lists of its groups.
		for _, g := range macGroups() {
			runQuietSetup("dseditgroup", "-o", "edit", "-d", name, "-t", "user", g)
		}
		if err := runSetup("dscl", ".", "-delete", "/Users/"+name); err != nil {
			return err
		}
	} else if err := runSetup("userdel", name); err != nil {
		return err
	}
	return nil
}

func leaseHome(name string) string {
	if runtime.GOOS == "darwin" {
		return "/Users/" + name
	}
	return "/home/" + name
}

// removeLeaseHome removes a lease user's home directory (its parent belongs
// to root; RemoveAll does not follow links). On macOS the user can set flags
// that stop the removal: chflags -P does not follow links either.
func removeLeaseHome(home string) error {
	if _, err := os.Lstat(home); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if runtime.GOOS == "darwin" {
		runQuietSetup("chflags", "-R", "-P", "nouchg,nouappnd", home)
	}
	return os.RemoveAll(home)
}

// macGroups are the groups of a lease user on macOS: the lease group, and
// the group of users that Remote Login allows, when Remote Login allows only
// some users.
func macGroups() []string {
	groups := []string{leaseGroup}
	if _, err := user.LookupGroup("com.apple.access_ssh"); err == nil {
		groups = append(groups, "com.apple.access_ssh")
	}
	return groups
}

// removeScheduledJobs removes the crontab and the at jobs of the user ("atq"
// lines: job, date, queue, user; the format differs, the user is the last
// field). A machine without at has no at jobs.
func removeScheduledJobs(name string) error {
	if _, err := exec.LookPath("crontab"); err == nil {
		if _, err := setupOutput("crontab", "-u", name, "-l"); err == nil {
			if err := runSetup("crontab", "-u", name, "-r"); err != nil {
				return err
			}
		}
	}
	if _, err := exec.LookPath("atq"); err != nil {
		return nil
	}
	out, err := setupOutput("atq")
	if err != nil {
		return fmt.Errorf("atq: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 1 && f[len(f)-1] == name {
			if err := runSetup("atrm", f[0]); err != nil {
				return err
			}
		}
	}
	return nil
}

// killUser sends SIGKILL to all processes of the user until none is left.
func killUser(name string) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		runQuietSetup("pkill", "-KILL", "-u", name)
		_, err := setupOutput("pgrep", "-u", name)
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil // no process
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

// installLeaseUserRule writes the server key for lease users and the sudo
// rule that lets the machine user run only "<bin> lease-user ..." as root,
// checked with visudo.
func installLeaseUserRule(osName, bin, serverKey string) error {
	if osName != "linux" && osName != "macos" {
		return errors.New("lease users are for macOS and Linux machines")
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(serverKey)); err != nil {
		return fmt.Errorf("the server key: %w", err)
	}
	if err := os.WriteFile(leaseServerKey, []byte(strings.TrimSpace(serverKey)+"\n"), 0o644); err != nil {
		return err
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

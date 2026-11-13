package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/invite"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/tlspin"
)

const templateUsage = `hostel: usage:
  hostel machine join <machine code> [--no-service]   add this computer as a machine (as an administrator)
  hostel machine connect --server <url> --token-file <file> [--server-key k] [--ssh 127.0.0.1:22]
  hostel machine update                               replace the binary of the connect service with the server's (as an administrator)
An admin makes the code with "hostel config machines add <name>".`

// templateCmd is "hostel machine": commands on a machine (M9).
func templateCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "join":
			return machineJoin(args[1:], stdout, stderr)
		case "connect":
			return helperCmd(append([]string{"connect"}, args[1:]...), stdout, stderr)
		case "update":
			return machineUpdate(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintln(stderr, templateUsage)
	return 2
}

// machineUser is the local user that the server logs in as (no administrator
// rights).
const machineUser = "hostel"

// runSetup runs a setup command with a deadline; the error has its output.
func runSetup(name string, args ...string) error {
	return runSetupEnv(nil, name, args...)
}

// runSetupEnv runs a setup command with extra environment variables (for
// secrets: they are not in the error, and not in the process list).
func runSetupEnv(env []string, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		what := name + " " + strings.Join(args, " ")
		if name == "powershell.exe" {
			what = "PowerShell"
		}
		return fmt.Errorf("%s: %v: %s", what, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func powershell(script string, env ...string) error {
	return runSetupEnv(env, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
}

func isAdministrator() bool {
	if runtime.GOOS == "windows" {
		return runSetup("net.exe", "session") == nil
	}
	return os.Geteuid() == 0
}

// joinState is the result of a join, next to the token: a second run after a
// failure continues with it (the code works once).
type joinState struct {
	// Code is the SHA-256 of the code's secret: another code joins again.
	Code            string `json:"code"`
	Server          string `json:"server"`
	Key             string `json:"key,omitempty"`
	Template        string `json:"template"`
	ServerPublicKey string `json:"server_public_key"`
}

func machineJoin(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel machine join", stderr)
	noService := fs.Bool("no-service", false, "do not install the service; print the command that it would run")
	leaseUsers := fs.Bool("lease-users", false, "allow the machine user to run only \"hostel lease-user\" as root, for lease_users (macOS and Linux)")
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, templateUsage)
		return 2
	}
	code, err := invite.Decode(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 2
	}
	if !isAdministrator() {
		fmt.Fprintln(stderr, "hostel: run this as an administrator (sudo on macOS and Linux, an administrator PowerShell on Windows)")
		return 1
	}
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = config.OSMacOS
	}
	fail := func(step string, err error) int {
		fmt.Fprintf(stderr, "hostel: %s: %v\nFix this, and run the same command again: it continues where it stopped.\n", step, err)
		return 1
	}
	dir := serviceDir(osName)
	tokenFile := filepath.Join(dir, "machine-token")
	stateFile := filepath.Join(dir, "join.json")

	fmt.Fprintf(stdout, "1. The machine user %q and the directory %s.\n", machineUser, dir)
	if err := ensureMachineUser(osName); err != nil {
		return fail("create the user "+machineUser, err)
	}
	if err := secureServiceDir(osName, dir); err != nil {
		return fail("prepare "+dir, err)
	}
	var st joinState
	data, stateErr := os.ReadFile(stateFile)
	_, tokenErr := os.Stat(tokenFile)
	sum := sha256.Sum256(code.Secret)
	codeHash := hex.EncodeToString(sum[:])
	if stateErr == nil && tokenErr == nil && json.Unmarshal(data, &st) == nil && st.Template != "" && st.Code == codeHash {
		fmt.Fprintf(stdout, "2. Joined already as %s (%s).\n", st.Template, stateFile)
	} else {
		fmt.Fprintln(stdout, "2. Join the server.")
		resp, server, key, err := joinServer(code, osName)
		if err != nil {
			fmt.Fprintln(stderr, "hostel: join:", err)
			return 1
		}
		st = joinState{Code: codeHash, Server: server, Key: key, Template: resp.Template, ServerPublicKey: resp.ServerPublicKey}
		// The code is used now: without these files, a second run cannot
		// continue.
		lost := func(what string, err error) int {
			fmt.Fprintf(stderr, "hostel: %s: %v\nThe server added %s, but this machine could not save its token. Remove the machine (hostel config machines remove %s), ask for a new code, fix the error, and run the new command.\n", what, err, resp.Template, resp.Template)
			return 1
		}
		if err := writeToken(osName, tokenFile, resp.Token, !*noService); err != nil {
			return lost("write the machine token", err)
		}
		data, _ := json.MarshalIndent(st, "", "  ")
		if err := os.WriteFile(stateFile, append(data, '\n'), 0o644); err != nil {
			return lost("write "+stateFile, err)
		}
		fmt.Fprintf(stdout, "   The server %s added the machine %s.\n", resp.ServerID, resp.Template)
	}

	if !*noService {
		if err := serviceAccess(osName, tokenFile); err != nil {
			return fail("prepare the service account", err)
		}
	}

	fmt.Fprintln(stdout, "3. The server's key for the machine user.")
	if err := installServerKey(osName, st.ServerPublicKey); err != nil {
		return fail("install the server key", err)
	}

	fmt.Fprintln(stdout, "4. The connection to the server.")
	// After the join: on Windows the copy stops a running connection, which
	// must stay up when the server refuses the code.
	bin, err := installBinary(osName, dir)
	if err != nil {
		return fail("install hostel", err)
	}
	connectArgs := []string{"machine", "connect", "--server", st.Server, "--token-file", tokenFile}
	if st.Key != "" {
		connectArgs = append(connectArgs, "--server-key", st.Key)
	}
	if *leaseUsers {
		fmt.Fprintln(stdout, "   Lease users: the sudo rule for \"hostel lease-user\".")
		if err := installLeaseUserRule(osName, bin); err != nil {
			return fail("install the sudo rule for lease users", err)
		}
	}
	if *noService {
		fmt.Fprintf(stdout, "   No service (--no-service). Run as an administrator:\n   %s %s\n", bin, strings.Join(connectArgs, " "))
	} else if err := installService(osName, dir, bin, connectArgs); err != nil {
		return fail("install the service", err)
	}

	fmt.Fprintf(stdout, "\nDone: %s connects to %s. \"hostel status\" shows its machine within a minute.\n", st.Template, st.Server)
	switch osName {
	case config.OSMacOS:
		fmt.Fprintln(stdout, `Steps that only a person can do (docs/machine-setup.md, section 3):
  - System Settings > General > Sharing > Remote Login: on, for the user "hostel".
  - For xcode and ios-simulator: install Xcode and its iOS Simulator runtime.
  - For ios-simulator and macos-desktop: automatic login of "hostel", no screen lock.
  - For macos-desktop: Screen Recording for sshd-keygen-wrapper, and Automation Mode.
  - For Mac app QA: apps in /Applications, privacy permissions, and signing (optional).`)
	case config.OSWindows:
		fmt.Fprintln(stdout, `Steps that only a person can do (docs/machine-setup.md, section 4):
  - OpenSSH Server must run and start automatically (Get-Service sshd).
  - The build tools for all users (.NET SDK, Windows SDK, PowerShell 7).
  - For windows-desktop: a signed-in desktop and the "Hostel desktop" task.
  - For signed packages: read access for "hostel" to the certificate's private key (optional).`)
	default:
		fmt.Fprintln(stdout, "The SSH server must run on this machine (port 22) and allow key login for \"hostel\". For android-emulator, \"hostel\" needs /dev/kvm (docs/machine-setup.md, section 5).")
	}
	return 0
}

// joinServer sends the code; it tries each address of the code.
func joinServer(code invite.Code, osName string) (protocol.MachineJoinResponse, string, string, error) {
	g := &config.Guest{}
	if code.KeyHash != nil {
		g.Key = tlspin.Fingerprint(code.KeyHash)
	}
	host, _ := os.Hostname()
	req := protocol.MachineJoinRequest{Secret: code.Secret, OS: osName, Arch: runtime.GOARCH, Hostname: host}
	var errs []string
	for _, addr := range code.Addresses {
		g.Addresses = []string{addr}
		c, err := client.New(g)
		if err != nil {
			return protocol.MachineJoinResponse{}, "", "", err
		}
		var resp protocol.MachineJoinResponse
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		err = c.Do(ctx, "POST", "/v1/machines/join", req, &resp)
		cancel()
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status < 500 {
			return resp, "", "", err
		}
		if err == nil {
			return resp, g.URL(), g.Key, nil
		}
		errs = append(errs, err.Error())
	}
	return protocol.MachineJoinResponse{}, "", "", fmt.Errorf("no address of the server answered:\n  %s", strings.Join(errs, "\n  "))
}

func randomPassword() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b) + "aA1!"
}

// ensureMachineUser creates the machine user if it is missing: no administrator
// rights, and no password that anybody knows (the server logs in with its key).
func ensureMachineUser(osName string) error {
	switch osName {
	case config.OSLinux:
		if runSetup("id", machineUser) == nil {
			return nil
		}
		if err := runSetup("useradd", "--create-home", "--shell", "/bin/bash", machineUser); err != nil {
			return err
		}
		// "*": no password login, but key login (a locked "!" account can fail
		// with PAM).
		return runSetup("usermod", "--password", "*", machineUser)
	case config.OSMacOS:
		if runSetup("id", machineUser) == nil {
			return nil
		}
		// sysadminctl takes the password as an argument: the error must not
		// show the arguments.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "sysadminctl", "-addUser", machineUser, "-fullName", "Hostel",
			"-password", randomPassword()).CombinedOutput(); err != nil {
			return fmt.Errorf("sysadminctl -addUser %s: %v: %s", machineUser, err, strings.TrimSpace(string(out)))
		}
		return nil
	case config.OSWindows:
		return powershell(`if (-not (Get-LocalUser -Name hostel -ErrorAction SilentlyContinue)) {
  $pw = ConvertTo-SecureString $env:HOSTEL_PASSWORD -AsPlainText -Force
  New-LocalUser -Name hostel -Password $pw -PasswordNeverExpires -AccountNeverExpires | Out-Null
  Add-LocalGroupMember -SID S-1-5-32-545 -Member hostel
  # The profile must exist before the first SSH login.
  $cred = New-Object Management.Automation.PSCredential('hostel', $pw)
  Start-Process cmd.exe -ArgumentList '/c','exit' -Credential $cred -LoadUserProfile -WorkingDirectory C:\Windows -Wait
}`, "HOSTEL_PASSWORD="+randomPassword())
	}
	return fmt.Errorf("unsupported OS %s", osName)
}

// installServerKey puts the server's public key into the machine user's
// authorized_keys and authorized_keys.base (cleanup restores the first from
// the second). The machine user owns these files, and a lease can make them
// links: on macOS and Linux the files are written as the machine user itself, so
// a link cannot make root write elsewhere.
func installServerKey(osName, key string) error {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
	if err != nil {
		return fmt.Errorf("the server sent an invalid key: %w", err)
	}
	key = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
	if osName == config.OSWindows {
		return powershell(`$k = $env:HOSTEL_KEY; $h = 'C:\Users\hostel'
foreach ($p in "$h\.ssh", "$h\hostel", "$h\.ssh\authorized_keys", "$h\hostel\authorized_keys.base") {
  if ((Test-Path $p) -and ((Get-Item -Force $p).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw "$p is a link; remove it and run again" }
}
foreach ($f in "$h\.ssh\authorized_keys", "$h\hostel\authorized_keys.base") {
  New-Item -ItemType Directory -Force (Split-Path $f) | Out-Null
  if (-not ((Test-Path $f) -and (Select-String -Path $f -SimpleMatch $k -Quiet))) { Add-Content -Path $f -Value $k }
}
icacls "$h\.ssh\authorized_keys" /inheritance:r /grant '*S-1-5-18:F' /grant 'hostel:F' | Out-Null
icacls "$h\hostel" /grant 'hostel:(OI)(CI)F' | Out-Null`, "HOSTEL_KEY="+key)
	}
	// The key has only letters, digits, "+/=", "-", and a space.
	script := `umask 077; mkdir -p "$HOME/.ssh" "$HOME/hostel" && for f in "$HOME/.ssh/authorized_keys" "$HOME/hostel/authorized_keys.base"; do touch "$f" && { grep -qxF '` + key + `' "$f" || printf '%s\n' '` + key + `' >> "$f"; } || exit 1; done`
	if osName == config.OSLinux {
		return runSetup("runuser", "-l", machineUser, "-c", script)
	}
	return runSetup("sudo", "-u", machineUser, "-H", "sh", "-c", script)
}

// serviceDir has the token, the join state, and the binary of the service;
// only administrators (and on Windows the service account) can write it.
func serviceDir(osName string) string {
	if osName == config.OSWindows {
		return `C:\ProgramData\hostel`
	}
	return "/etc/hostel"
}

// secureServiceDir creates the service directory, or checks that an
// administrator owns it (the machine user must not own it: the owner can read
// the token and replace the service's binary).
func secureServiceDir(osName, dir string) error {
	if osName == config.OSWindows {
		return powershell(`$d = $env:HOSTEL_DIR
if (Test-Path $d) {
  $o = (Get-Acl $d).GetOwner([Security.Principal.SecurityIdentifier]).Value
  if ($o -notin 'S-1-5-32-544', 'S-1-5-18') { throw "$d belongs to $o, not to the administrators; remove it and run again" }
} else { New-Item -ItemType Directory $d | Out-Null }
icacls $d /setowner '*S-1-5-32-544' /T /C | Out-Null
icacls $d /inheritance:r /grant '*S-1-5-18:(OI)(CI)F' /grant '*S-1-5-32-544:(OI)(CI)F' /grant '*S-1-5-19:(OI)(CI)M' | Out-Null`, "HOSTEL_DIR="+dir)
	}
	fi, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		return os.Chmod(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if uid, ok := fileOwner(fi); !ok || uid != 0 {
		return fmt.Errorf("%s does not belong to root; remove it and run again", dir)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 || fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must be a directory that only root can write; remove it and run again", dir)
	}
	return nil
}

// installBinary copies this binary into the service directory, the binary
// that the service runs.
func installBinary(osName, dir string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "hostel")
	if osName == config.OSWindows {
		dst += ".exe"
		// A running service holds the file.
		runSetup("schtasks.exe", "/End", "/TN", "Hostel machine connect")
	}
	if self == dst {
		return dst, nil
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst+".tmp", data, 0o755); err != nil {
		return "", err
	}
	// The service's account must run it, whatever the umask of root.
	if err := os.Chmod(dst+".tmp", 0o755); err != nil {
		return "", err
	}
	// On Windows, a service that just ended can hold the file for some
	// seconds.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = os.Rename(dst+".tmp", dst)
		if err == nil || osName != config.OSWindows || time.Now().After(deadline) {
			return dst, err
		}
		time.Sleep(time.Second)
	}
}

// connectAccount is the account of the service on Linux: not the machine user,
// which a lease controls.
const connectAccount = "hostel-connect"

// writeToken writes the machine token so that only the service's account and
// administrators can read it.
func writeToken(osName, file, token string, service bool) error {
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	if service {
		return serviceAccess(osName, file)
	}
	return nil
}

// serviceAccess lets the service's account read the token: on Linux the
// account hostel-connect (created if it is missing) owns it. On macOS the
// LaunchDaemon runs as root; on Windows the directory's rules apply. A second
// run (also after --no-service) does it again.
func serviceAccess(osName, file string) error {
	if osName != config.OSLinux {
		return nil
	}
	if runSetup("id", connectAccount) != nil {
		if err := runSetup("useradd", "--system", "--no-create-home", "--shell", "/usr/sbin/nologin", connectAccount); err != nil {
			return err
		}
	}
	return runSetup("chown", connectAccount, file)
}

// installService installs and starts the service that runs "hostel machine
// connect" (deploy-vps.md, section 6).
func installService(osName, dir, bin string, args []string) error {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strconv.Quote(a)
	}
	switch osName {
	case config.OSLinux:
		unit := fmt.Sprintf(`[Unit]
Description=Hostel machine connect
After=network-online.target
Wants=network-online.target

[Service]
User=%s
ExecStart=%s %s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, connectAccount, bin, strings.Join(quoted, " "))
		if err := os.WriteFile("/etc/systemd/system/hostel-machine-connect.service", []byte(unit), 0o644); err != nil {
			return err
		}
		if err := runSetup("systemctl", "daemon-reload"); err != nil {
			return err
		}
		if err := runSetup("systemctl", "enable", "hostel-machine-connect.service"); err != nil {
			return err
		}
		// A running service has the old token in memory.
		return runSetup("systemctl", "restart", "hostel-machine-connect.service")
	case config.OSMacOS:
		var items strings.Builder
		for _, a := range append([]string{bin}, args...) {
			fmt.Fprintf(&items, "\t\t<string>%s</string>\n", xmlEscape(a))
		}
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>so.hostel.machine-connect</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardErrorPath</key><string>/var/log/hostel-machine-connect.log</string>
	<key>StandardOutPath</key><string>/var/log/hostel-machine-connect.log</string>
</dict>
</plist>
`, items.String())
		file := "/Library/LaunchDaemons/so.hostel.machine-connect.plist"
		runSetup("launchctl", "bootout", "system/so.hostel.machine-connect")
		if err := os.WriteFile(file, []byte(plist), 0o644); err != nil {
			return err
		}
		return runSetup("launchctl", "bootstrap", "system", file)
	case config.OSWindows:
		// schtasks /TR takes at most 261 characters: the command is in a file
		// of the service directory.
		var line strings.Builder
		line.WriteString("@\"" + bin + "\"")
		for _, a := range args {
			line.WriteString(" \"" + a + "\"")
		}
		line.WriteString(" >> \"" + filepath.Join(dir, "machine-connect.log") + "\" 2>&1\r\n")
		script := filepath.Join(dir, "machine-connect.cmd")
		if err := os.WriteFile(script, []byte(line.String()), 0o644); err != nil {
			return err
		}
		if err := runSetup("schtasks.exe", "/Create", "/F", "/TN", "Hostel machine connect", "/SC", "ONSTART",
			"/RU", `NT AUTHORITY\LOCALSERVICE`, "/RL", "LIMITED", "/TR", `"`+script+`"`); err != nil {
			return err
		}
		return runSetup("schtasks.exe", "/Run", "/TN", "Hostel machine connect")
	}
	return fmt.Errorf("unsupported OS %s", osName)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/update"
	"github.com/csansoon/hostel/internal/version"
)

// machineUpdateTimeout bounds the download of the binary from the server.
const machineUpdateTimeout = 10 * time.Minute

// machineUpdate is "hostel machine update" (M10d), as an administrator on a
// machine that joined: it replaces the binary of the connect service with
// the server's binary for this machine and restarts the service. The service
// does not replace itself: it cannot write its directory, which protects the
// machine token. It also installs the root step of a remote update (M13).
//
// With --signed it is that root step (M13): the server runs it through the
// sudo rule (Windows: through the SYSTEM service). It installs only the newer
// release that the server offers with a valid signature.
func machineUpdate(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel machine update", stderr)
	signed := fs.Bool("signed", false, "install only the signed release that the server offers, if it is newer (the step of a remote update)")
	if rest, err := cli.Parse(fs, args); err != nil || len(rest) > 0 {
		fmt.Fprintln(stderr, templateUsage)
		return 2
	}
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = config.OSMacOS
	}
	if *signed && osName == config.OSWindows && !isAdministrator() {
		// The machine user asks the SYSTEM service.
		res, err := callUpdateService()
		if err != nil {
			fmt.Fprintln(stderr, "hostel machine update:", err)
			return 1
		}
		fmt.Fprintln(stdout, res)
		return 0
	}
	if !isAdministrator() {
		fmt.Fprintln(stderr, "hostel: run this as an administrator (sudo on macOS and Linux, an administrator PowerShell on Windows)")
		return 1
	}
	if osName != config.OSWindows {
		// Commands only from the system directories: sudo can keep the
		// caller's PATH.
		os.Setenv("PATH", "/usr/sbin:/usr/bin:/sbin:/bin")
	}
	update := updateServiceBinary
	if *signed {
		update = updateSigned
	}
	res, restart, err := update(osName)
	if err != nil {
		fmt.Fprintln(stderr, "hostel machine update:", err)
		return 1
	}
	// The result first: the restart can end the connection that carries this
	// command (a remote update).
	fmt.Fprintln(stdout, res)
	if restart == nil {
		return 0
	}
	if f, ok := stdout.(*os.File); ok {
		f.Sync()
	}
	msg, err := restart()
	if err != nil {
		fmt.Fprintln(stderr, "hostel machine update: the service did not start again:", err)
		return 1
	}
	fmt.Fprintln(stdout, msg)
	return 0
}

// joinedServer reads the join state of the machine and an HTTP client with
// the TLS of the join.
func joinedServer(osName string) (joinState, *http.Client, error) {
	dir := serviceDir(osName)
	var st joinState
	data, err := os.ReadFile(filepath.Join(dir, "join.json"))
	if err != nil || json.Unmarshal(data, &st) != nil || st.Server == "" {
		return st, nil, fmt.Errorf("this machine has not joined a server (%s/join.json is missing). A machine that joined runs this command; on a machine that you set up by hand (\"hostel helper connect\"), install the new binary yourself and restart its connection", dir)
	}
	tlsConfig, err := client.TLSConfig(&config.Guest{Key: st.Key})
	if err != nil {
		return st, nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return st, &http.Client{Transport: transport}, nil
}

// serviceBinary is the binary of the connect service.
func serviceBinary(osName string) string {
	bin := filepath.Join(serviceDir(osName), "hostel")
	if osName == config.OSWindows {
		bin += ".exe"
	}
	return bin
}

// restartFunc starts the connect service again with the new binary.
func restartFunc(osName string) func() (string, error) {
	return func() (string, error) {
		restarted, err := restartService(osName)
		switch {
		case err != nil:
			return "", err
		case !restarted:
			return "No service is installed (--no-service): stop \"hostel machine connect\" and start it again.", nil
		}
		return "The service runs the new binary.", nil
	}
}

// updateServiceBinary (an administrator on the machine) replaces the service
// binary with the server's binary for this machine and installs the root step
// of a remote update. restart (nil when nothing changed) starts the service
// again with the new binary.
func updateServiceBinary(osName string) (res string, restart func() (string, error), err error) {
	st, hc, err := joinedServer(osName)
	if err != nil {
		return "", nil, err
	}
	bin := serviceBinary(osName)
	ctx, cancel := context.WithTimeout(context.Background(), machineUpdateTimeout)
	defer cancel()
	have := binaryVersion(ctx, bin)
	name := update.BinaryName(runtime.GOOS, runtime.GOARCH)
	// Into a directory of this administrator only: on Windows the connection's
	// account can change the service directory, and the SYSTEM service gets
	// a copy of this file.
	priv, err := os.MkdirTemp("", "hostel-update-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(priv)
	tmp, err := downloadFromServer(ctx, hc, st.Server, name, filepath.Join(priv, filepath.Base(bin)))
	if err != nil {
		return "", nil, err
	}
	newer := binaryVersion(ctx, tmp)
	data, err := os.ReadFile(tmp)
	switch {
	case err != nil:
		return "", nil, err
	case newer == "":
		return "", nil, fmt.Errorf("the server's %s does not run on this machine", name)
	case update.FileSHA256(tmp) == update.FileSHA256(bin):
		if err := installUpdateStep(osName, bin, tmp); err != nil {
			return "", nil, fmt.Errorf("install the step of a remote update: %w", err)
		}
		return fmt.Sprintf("%s is up to date (%s, the same as the server's)", bin, newer), nil, nil
	}
	if osName == config.OSWindows {
		// A running service holds the file.
		runSetup("schtasks.exe", "/End", "/TN", "Hostel machine connect")
	}
	if err := writeReplace(bin, data); err != nil {
		if osName == config.OSWindows {
			// The old binary is in place again: start the service again.
			restartService(osName)
		}
		return "", nil, err
	}
	res = fmt.Sprintf("updated %s from %s to %s", bin, have, newer)
	if err := installUpdateStep(osName, bin, tmp); err != nil {
		return "", nil, fmt.Errorf("%s, but the step of a remote update was not installed: %w", res, err)
	}
	return res, restartFunc(osName), nil
}

// updateSigned is the root step of a remote update (M13), as root through the
// sudo rule, or in the SYSTEM service on Windows. It trusts only this binary
// (the installed one: its version and its release key) and files that only
// root (SYSTEM) can write: the server's release must have a valid signature
// and be newer, and the server's binary must have the signed SHA-256 before
// it runs. A machine with the release only restarts the connection (the
// server asks when the connection has another version).
func updateSigned(osName string) (res string, restart func() (string, error), err error) {
	st, hc, err := joinedServer(osName)
	if err != nil {
		return "", nil, err
	}
	bin := serviceBinary(osName)
	ctx, cancel := context.WithTimeout(context.Background(), machineUpdateTimeout)
	defer cancel()
	rel, err := serverRelease(ctx, hc, st.Server, osName)
	if err != nil {
		return "", nil, err
	}
	have := version.Version
	if _, known := update.Parse(have); known && rel.Tag != have && !update.Newer(rel.Tag, have) {
		return "", nil, fmt.Errorf("the server offers %s, which is older than %s on this machine: not installed", rel.Tag, have)
	}
	name := update.BinaryName(runtime.GOOS, runtime.GOARCH)
	want := rel.Sums[name]
	if want == "" {
		return "", nil, fmt.Errorf("release %s has no %s", rel.Tag, name)
	}
	if rel.Tag == have && update.FileSHA256(bin) == want {
		if osName != config.OSWindows {
			if err := installUpdateStep(osName, bin, ""); err != nil {
				return "", nil, fmt.Errorf("install the step of a remote update: %w", err)
			}
		}
		return fmt.Sprintf("%s is up to date (%s, the server's release)", bin, have), restartFunc(osName), nil
	}
	// Downloaded into the directory that only root (SYSTEM) can write, and
	// checked before it runs.
	trusted := filepath.Join(updateStepDir(osName), "hostel")
	if osName == config.OSWindows {
		trusted += ".exe"
	}
	tmp, err := downloadFromServer(ctx, hc, st.Server, name, trusted)
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(tmp)
	if update.FileSHA256(tmp) != want {
		return "", nil, fmt.Errorf("the server's %s is not the one of release %s (its SHA-256 differs from the signed SHA256SUMS): not installed", name, rel.Tag)
	}
	if v := binaryVersion(ctx, tmp); v != rel.Tag {
		return "", nil, fmt.Errorf("the server's %s prints %q, not %s: not installed", name, v, rel.Tag)
	}
	if osName == config.OSWindows {
		// The connection's copy, from the checked file; then the service's
		// own binary, which restarts after its answer.
		data, err := os.ReadFile(tmp)
		if err != nil {
			return "", nil, err
		}
		runSetup("schtasks.exe", "/End", "/TN", "Hostel machine connect")
		if err := writeReplace(bin, data); err != nil {
			restartService(osName)
			return "", nil, err
		}
		if err := update.Replace(tmp, trusted); err != nil {
			restartService(osName)
			return "", nil, err
		}
		selfReplaced = true
	} else {
		if err := update.Replace(tmp, bin); err != nil {
			return "", nil, err
		}
		if err := installUpdateStep(osName, bin, ""); err != nil {
			return "", nil, fmt.Errorf("updated %s to %s, but the step of a remote update was not installed: %w", bin, rel.Tag, err)
		}
	}
	return fmt.Sprintf("updated %s from %s to %s (signed release)", bin, have, rel.Tag), restartFunc(osName), nil
}

// selfReplaced: the signed update in the Windows SYSTEM service replaced the
// service's own binary; the service restarts after its answer.
var selfReplaced bool

// writeReplace writes data to a new file next to dst and renames it over dst.
func writeReplace(dst string, data []byte) error {
	f, err := update.TempNext(dst)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o755)
	}
	if err == nil {
		err = update.Replace(f.Name(), dst)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// serverRelease reads the signed release that the server offers (its tag,
// SHA256SUMS, and the signature) and checks it with the release key of this
// binary, or with the key in the root-owned file release-key (a mirror with
// its own key).
func serverRelease(ctx context.Context, hc *http.Client, server, osName string) (*update.Release, error) {
	files := map[string][]byte{}
	for _, name := range []string{update.TagFile, update.SumsFile, update.SigFile} {
		data, err := smallFromServer(ctx, hc, server, name)
		if err != nil {
			return nil, fmt.Errorf("%w (the server must run a release to update machines; on a build that is not a release, update the machine by hand)", err)
		}
		files[name] = data
	}
	key := update.ReleaseKey()
	if data, err := readRootFile(filepath.Join(updateStepDir(osName), "release-key")); err == nil {
		if key, err = update.ParseKey(string(data)); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return update.Verify(key, strings.TrimSpace(string(files[update.TagFile])), files[update.SumsFile], files[update.SigFile])
}

// smallFromServer reads a small file of /v1/download (at most 1 MB).
func smallFromServer(ctx context.Context, hc *http.Client, server, name string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(server, "/")+"/v1/download/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s from %s: %w", name, server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s from %s: HTTP %d", name, server, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// downloadFromServer downloads a binary of /v1/download into a new file next
// to bin.
func downloadFromServer(ctx context.Context, hc *http.Client, server, name, bin string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(server, "/")+"/v1/download/"+name, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s from %s: %w", name, server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s from %s: HTTP %d", name, server, resp.StatusCode)
	}
	f, err := update.TempNext(bin)
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// The service's account must run it, whatever the umask of root.
		err = os.Chmod(tmp, 0o755)
	}
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	return tmp, nil
}

// binaryVersion is the version that bin prints ("" when it does not run).
func binaryVersion(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "hostel ")
}

// restartService starts the connect service again with the new binary; it
// reports false when no service is installed.
func restartService(osName string) (bool, error) {
	switch osName {
	case config.OSLinux:
		if _, err := os.Stat("/etc/systemd/system/hostel-machine-connect.service"); err != nil {
			return false, nil
		}
		return true, runSetup("systemctl", "restart", "hostel-machine-connect.service")
	case config.OSMacOS:
		if _, err := os.Stat("/Library/LaunchDaemons/so.hostel.machine-connect.plist"); err != nil {
			return false, nil
		}
		return true, runSetup("launchctl", "kickstart", "-k", "system/so.hostel.machine-connect")
	case config.OSWindows:
		if runSetup("schtasks.exe", "/Query", "/TN", "Hostel machine connect") != nil {
			return false, nil
		}
		// A running task ignores /Run: end the old process first.
		runSetup("schtasks.exe", "/End", "/TN", "Hostel machine connect")
		return true, runSetup("schtasks.exe", "/Run", "/TN", "Hostel machine connect")
	}
	return false, fmt.Errorf("unsupported OS %s", osName)
}

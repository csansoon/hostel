package main

import (
	"context"
	"encoding/json"
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
)

// machineUpdateTimeout bounds the download of the binary from the server.
const machineUpdateTimeout = 10 * time.Minute

// machineUpdate is "hostel machine update" (M10d), as an administrator on a
// machine that joined: it replaces the binary of the connect service with
// the server's binary for this machine and restarts the service. The service
// does not replace itself: it cannot write its directory, which protects the
// room token.
func machineUpdate(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel machine update", stderr)
	if rest, err := cli.Parse(fs, args); err != nil || len(rest) > 0 {
		fmt.Fprintln(stderr, machineUsage)
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
	res, err := updateServiceBinary(osName)
	if err != nil {
		fmt.Fprintln(stderr, "hostel machine update:", err)
		return 1
	}
	fmt.Fprintln(stdout, res)
	return 0
}

func updateServiceBinary(osName string) (string, error) {
	dir := serviceDir(osName)
	var st joinState
	data, err := os.ReadFile(filepath.Join(dir, "join.json"))
	if err != nil || json.Unmarshal(data, &st) != nil || st.Server == "" {
		return "", fmt.Errorf("this machine has not joined a server (%s/join.json is missing). A machine that joined runs this command; on a machine that you set up by hand (\"hostel room connect\"), install the new binary yourself and restart its connection", dir)
	}
	bin := filepath.Join(dir, "hostel")
	if osName == config.OSWindows {
		bin += ".exe"
	}
	tlsConfig, err := client.TLSConfig(&config.Guest{Key: st.Key})
	if err != nil {
		return "", err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	ctx, cancel := context.WithTimeout(context.Background(), machineUpdateTimeout)
	defer cancel()
	name := update.BinaryName(runtime.GOOS, runtime.GOARCH)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(st.Server, "/")+"/v1/download/"+name, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s from %s: %w", name, st.Server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s from %s: HTTP %d", name, st.Server, resp.StatusCode)
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
	have, newer := binaryVersion(ctx, bin), binaryVersion(ctx, tmp)
	if newer == "" {
		os.Remove(tmp)
		return "", fmt.Errorf("the server's %s does not run on this machine", name)
	}
	if update.FileSHA256(tmp) == update.FileSHA256(bin) {
		os.Remove(tmp)
		return fmt.Sprintf("%s is up to date (%s, the same as the server's)", bin, newer), nil
	}
	if osName == config.OSWindows {
		// A running service holds the file.
		runSetup("schtasks.exe", "/End", "/TN", "Hostel machine connect")
	}
	if err := update.Replace(tmp, bin); err != nil {
		if osName == config.OSWindows {
			// The old binary is in place again: start the service again.
			restartService(osName)
		}
		return "", err
	}
	res := fmt.Sprintf("updated %s from %s to %s", bin, have, newer)
	restarted, err := restartService(osName)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s, but the service did not start again: %w", res, err)
	case !restarted:
		return res + "; no service is installed (--no-service): stop \"hostel machine connect\" and start it again", nil
	}
	return res + "; the service runs the new binary", nil
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
		return true, runSetup("schtasks.exe", "/Run", "/TN", "Hostel machine connect")
	}
	return false, fmt.Errorf("unsupported OS %s", osName)
}

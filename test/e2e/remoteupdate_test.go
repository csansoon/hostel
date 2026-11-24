//go:build e2e

package e2e

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/update"
)

// TestRemoteMachineUpdate (M13): an admin updates a machine that joined from
// the client. A server that is not a release refuses. After a server update,
// "hostel config machines update" ends the active lease of the machine, the
// server runs the root step (the sudo rule of the join) as the machine user,
// and the machine connects with the new version, provisions, and takes the
// next lease; its drain state stays. The machine user cannot run the root
// step with other arguments. A server binary that is not the signed one is
// refused on the machine (broken, then repair). "server update --remote
// --machines" updates the server, then the machine.
func TestRemoteMachineUpdate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rel := newFakeRelease(t)
	rel.use(e)
	e.serverBin = copyBin(t, hostBin(t, "v1.0.0"), filepath.Join(e.home, "opt", "bin"))
	e.init()
	e.useTools("fake-sdk", "fake-screen")
	linux := "hostel-linux-" + dockerArch
	own := ownHelperDir(t, filepath.Join(e.home, "opt", "helpers"))
	data, err := os.ReadFile(versionBin(t, "linux", dockerArch, "v1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, linux), data, 0o755); err != nil {
		t.Fatal(err)
	}
	e.writeConfig(strings.Replace(serverHeader(), fmt.Sprintf("helper_dir = %q", helperDir), fmt.Sprintf("helper_dir = %q", own), 1))
	s := e.startServer("")
	s.pinPorts()

	// A machine that joins (v1.0.0), with the test's release key for its
	// root step, and a shell loop as its service manager.
	port := freePort(t)
	name := "hostel-e2e-" + e.runID + "-remote"
	if _, err := docker("run", "-d", "--name", name, "--label", dockerLabel+"="+e.runID, "--network", "host",
		"--entrypoint", "/bin/sh", machineImage, "-c",
		fmt.Sprintf("ssh-keygen -A && exec /usr/sbin/sshd -D -e -p %d -o ListenAddress=127.0.0.1", port)); err != nil {
		t.Fatal(err)
	}
	if _, err := docker("cp", versionBin(t, "linux", dockerArch, "v1.0.0"), name+":/usr/local/bin/hostel"); err != nil {
		t.Fatal(err)
	}
	r := e.adminHostel("config", "machines", "add", "box", "--tool", "fake-screen")
	m := regexp.MustCompile(`machine join (hostel1-\S+)`).FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil {
		t.Fatalf("config machines add: %s", r)
	}
	out, err := docker("exec", name, "/usr/local/bin/hostel", "machine", "join", m[1], "--no-service")
	if err != nil || !strings.Contains(out, `Remote updates: the sudo rule for "hostel machine update --signed"`) {
		t.Fatalf("machine join: %v\n%s", err, out)
	}
	connect := regexp.MustCompile(`Run as an administrator:\n   (\S+ machine connect .*)\n`).FindStringSubmatch(out)
	if connect == nil {
		t.Fatalf("machine join printed no connect command:\n%s", out)
	}
	key := base64.StdEncoding.EncodeToString(rel.key.Public().(ed25519.PublicKey))
	if _, err := docker("exec", name, "sh", "-c", "printf '%s\\n' '"+key+"' > /etc/hostel/release-key && chmod 644 /etc/hostel/release-key"); err != nil {
		t.Fatal(err)
	}
	loop := fmt.Sprintf("while :; do %s --ssh 127.0.0.1:%d & echo $! > /tmp/connect.pid; wait $!; done", connect[1], port)
	if _, err := docker("exec", "-d", name, "sh", "-c", loop); err != nil {
		t.Fatal(err)
	}
	connectVersion := func() string {
		for _, ms := range s.status().Templates {
			if ms.Key == "home/box" {
				return ms.ConnectVersion
			}
		}
		return ""
	}
	e.waitFor("the connect version v1.0.0", machineReady, func() bool { return connectVersion() == "v1.0.0" })
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)

	// The machine user may run only the root step, with its fixed argument.
	if out, err := docker("exec", "-u", "hostel", name, "sudo", "-n", "/etc/hostel/hostel", "machine", "update"); err == nil {
		t.Fatalf("sudo machine update without --signed as the machine user: %s", out)
	}

	// A server that is not a release cannot update machines.
	if r := e.adminHostel("config", "machines", "update", "home/box"); r.code != 1 || !strings.Contains(r.stderr, "has no signed release in its helper_dir") {
		t.Fatalf("machines update on a server that is not a release: %s", r)
	}

	// The server updates to v1.1.0 (it keeps the signed release for its
	// machines); the machine still has v1.0.0.
	rel.add("v1.1.0", map[string]string{
		update.BinaryName(runtime.GOOS, runtime.GOARCH): hostBin(t, "v1.1.0"),
		linux: versionBin(t, "linux", dockerArch, "v1.1.0"),
	})
	if r := e.adminHostel("server", "update", "--remote"); r.code != 0 || !strings.Contains(r.stdout, "the server runs v1.1.0") {
		t.Fatalf("server update --remote: %s", r)
	}
	e.waitFor("the machine connects again (v1.0.0)", time.Minute, func() bool { return connectVersion() == "v1.0.0" })
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)

	// A guest's lease on the machine ends with the update.
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	lease := createdID(t, e.mustHostel("acquire", "fake-screen", "--task", "before the update"))
	killed := make(chan struct{})
	go func() {
		// The service manager of a real machine: it starts the new binary
		// when the root step replaced it.
		defer close(killed)
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			if v, _ := docker("exec", name, "/etc/hostel/hostel", "--version"); v == "hostel v1.1.0" {
				docker("exec", name, "sh", "-c", "kill $(cat /tmp/connect.pid)")
				return
			}
			time.Sleep(time.Second)
		}
	}()
	r = e.adminHostel("config", "machines", "update", "home/box")
	<-killed
	if r.code != 0 || !strings.Contains(r.stdout, "Machine home/box: update to v1.1.0 started") || !strings.Contains(r.stdout, "Machine home/box: updated to v1.1.0.") {
		t.Fatalf("machines update: %s\nserver log:\n%s", r, s.log.String())
	}
	var l protocol.Lease
	if code := s.get("/v1/leases/"+lease, e.admin, &l); code != 200 || l.State != protocol.LeaseReleased || !strings.Contains(l.EndReason, "the machine home/box is updated") {
		t.Fatalf("the lease after the update: HTTP %d, %+v", code, l)
	}
	if v := connectVersion(); v != "v1.1.0" {
		t.Fatalf("the connect version after the update: %q", v)
	}
	ms := s.waitMachine("home/box", protocol.MachineRunning, machineReady)
	if ms.Drained || ms.UpdateTo != "" {
		t.Fatalf("the machine after the update: %+v, want not drained", ms)
	}
	e.newRepoNamed("after", defaultRepoFiles())
	e.mustHostel("acquire", "fake-screen", "--task", "after the update")
	if out, _ := docker("exec", name, "/home/hostel/hostel/bin/hostel", "--version"); out != "hostel v1.1.0" {
		t.Fatalf("the machine's helper after the update: %q", out)
	}
	e.mustHostel("release")
	if r := e.adminHostel("config", "machines", "update", "home/box"); r.code != 0 || !strings.Contains(r.stdout, "Up to date: machine home/box has v1.1.0 already.") {
		t.Fatalf("a second machines update: %s", r)
	}

	// A server binary that is not the one of the signed release: the root
	// step refuses it; the machine is broken until a repair.
	rel.add("v1.2.0", map[string]string{
		update.BinaryName(runtime.GOOS, runtime.GOARCH): hostBin(t, "v1.2.0"),
		linux: versionBin(t, "linux", dockerArch, "v1.2.0"),
	})
	if r := e.adminHostel("server", "update", "--remote"); r.code != 0 || !strings.Contains(r.stdout, "the server runs v1.2.0") {
		t.Fatalf("server update --remote to v1.2.0: %s", r)
	}
	e.waitFor("the machine connects again (v1.1.0)", time.Minute, func() bool { return connectVersion() == "v1.1.0" })
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)
	good, err := os.ReadFile(filepath.Join(own, linux))
	if err != nil {
		t.Fatal(err)
	}
	// A server that someone took over serves its own program with the
	// signed files: the root step must not run it before the SHA-256 check.
	if err := os.WriteFile(filepath.Join(own, linux), []byte("#!/bin/sh\nid -u > /tmp/ran-as-$(id -u)\necho hostel v1.2.0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := e.adminHostel("config", "machines", "update", "home/box"); r.code != 1 || !strings.Contains(r.stderr, "is not the one of release v1.2.0") {
		t.Fatalf("machines update with a changed binary: %s", r)
	}
	if out, _ := docker("exec", name, "sh", "-c", "ls /tmp/ran-as-* 2>/dev/null"); out != "" {
		t.Fatalf("the root step ran the server's unchecked binary: %s", out)
	}
	if ms := s.machine("home/box"); ms.State != protocol.MachineBroken || !strings.HasPrefix(ms.Reason, "update failed: ") || ms.Drained {
		t.Fatalf("the machine after a refused update: %+v", ms)
	}
	if err := os.WriteFile(filepath.Join(own, linux), good, 0o755); err != nil {
		t.Fatal(err)
	}
	e.adminHostel("config", "machines", "repair", "home/box")
	s.waitMachine("home/box", protocol.MachineRunning, machineReady)

	// A signature that the machine's release key does not accept, and an
	// older release (the root step directly, as the machine user can run
	// it): refused, and nothing changes.
	_, other, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	otherKey := base64.StdEncoding.EncodeToString(other.Public().(ed25519.PublicKey))
	if _, err := docker("exec", name, "sh", "-c", "printf '%s\\n' '"+otherKey+"' > /etc/hostel/release-key"); err != nil {
		t.Fatal(err)
	}
	if out, err := docker("exec", "-u", "hostel", name, "sudo", "-n", "/etc/hostel/hostel", "machine", "update", "--signed"); err == nil ||
		!strings.Contains(err.Error()+out, "the signature of release v1.2.0 is not valid") {
		t.Fatalf("the root step with another release key: %v\n%s", err, out)
	}
	if _, err := docker("exec", name, "sh", "-c", "printf '%s\\n' '"+key+"' > /etc/hostel/release-key"); err != nil {
		t.Fatal(err)
	}
	signed := func(tag string, files map[string]string) {
		t.Helper()
		var sums strings.Builder
		for name, path := range files {
			fmt.Fprintf(&sums, "%s  %s\n", update.FileSHA256(path), name)
		}
		sig := base64.StdEncoding.EncodeToString(ed25519.Sign(rel.key, update.SignedText(tag, []byte(sums.String()))))
		for name, data := range map[string]string{update.TagFile: tag + "\n", update.SumsFile: sums.String(), update.SigFile: sig + "\n"} {
			if err := os.WriteFile(filepath.Join(own, name), []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	release, err := os.ReadFile(filepath.Join(own, update.TagFile))
	if err != nil {
		t.Fatal(err)
	}
	sums, _ := os.ReadFile(filepath.Join(own, update.SumsFile))
	sig, _ := os.ReadFile(filepath.Join(own, update.SigFile))
	signed("v1.0.0", map[string]string{linux: versionBin(t, "linux", dockerArch, "v1.0.0")})
	if out, err := docker("exec", "-u", "hostel", name, "sudo", "-n", "/etc/hostel/hostel", "machine", "update", "--signed"); err == nil ||
		!strings.Contains(err.Error()+out, "the server offers v1.0.0, which is older than v1.1.0 on this machine: not installed") {
		t.Fatalf("the root step with an older release: %v\n%s", err, out)
	}
	if r := e.adminHostel("config", "machines", "update", "home/box"); r.code != 0 || !strings.Contains(r.stdout, "Up to date: machine home/box has v1.1.0 already.") {
		t.Fatalf("machines update when the server offers an older release: %s", r)
	}
	for name, data := range map[string][]byte{update.TagFile: release, update.SumsFile: sums, update.SigFile: sig} {
		if err := os.WriteFile(filepath.Join(own, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := docker("exec", name, "/etc/hostel/hostel", "--version"); v != "hostel v1.1.0" {
		t.Fatalf("the machine's binary after the refused steps: %q", v)
	}

	// A drained machine stays drained after the update.
	e.adminHostel("config", "machines", "drain", "home/box")

	// "server update --remote --machines": the new server updates the
	// machine when it connects.
	rel.add("v1.3.0", map[string]string{
		update.BinaryName(runtime.GOOS, runtime.GOARCH): hostBin(t, "v1.3.0"),
		linux: versionBin(t, "linux", dockerArch, "v1.3.0"),
	})
	killed = make(chan struct{})
	go func() {
		defer close(killed)
		deadline := time.Now().Add(5 * time.Minute)
		for time.Now().Before(deadline) {
			if v, _ := docker("exec", name, "/etc/hostel/hostel", "--version"); v == "hostel v1.3.0" {
				docker("exec", name, "sh", "-c", "kill $(cat /tmp/connect.pid)")
				return
			}
			time.Sleep(time.Second)
		}
	}()
	if r := e.adminHostel("server", "update", "--remote", "--machines"); r.code != 0 || !strings.Contains(r.stdout, "the server updates each machine that joined") {
		t.Fatalf("server update --remote --machines: %s", r)
	}
	<-killed
	e.waitFor("the connect version v1.3.0", 5*time.Minute, func() bool { return connectVersion() == "v1.3.0" })
	if ms := s.waitMachine("home/box", protocol.MachineRunning, machineReady); !ms.Drained || ms.UpdateTo != "" {
		t.Fatalf("a drained machine after the update: %+v, want drained", ms)
	}
	if !strings.Contains(s.log.String(), `"msg":"machine updates after the server update"`) {
		t.Fatal("the new server did not read the machine updates of --machines")
	}
}

//go:build e2e

package e2e

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// TestMachineJoin (M9): an admin makes a machine code from a client; on a new
// Linux machine without the machine user, "hostel machine join <code>" (as root)
// creates the user, installs the server key, and gets a machine token; the
// server adds the machine to the configuration (connect = true). Then
// "hostel machine connect" connects it, and a lease runs on its machine. The code
// works once; the install script and the binary come from the server. The
// machine is a container with host networking (it reaches the server on
// 127.0.0.1), with its SSH server on a free port; systemd is not in the
// container, so the test joins with --no-service and runs the connection.
func TestMachineJoin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	e.useTools("fake-sdk", "fake-screen")
	e.writeConfig(serverHeader())
	s := e.startServer("")

	port := freePort(t)
	name := "hostel-e2e-" + e.runID + "-joined"
	if _, err := docker("run", "-d", "--name", name, "--label", dockerLabel+"="+e.runID, "--network", "host",
		"--entrypoint", "/bin/sh", machineImage, "-c",
		fmt.Sprintf("ssh-keygen -A && exec /usr/sbin/sshd -D -e -p %d -o ListenAddress=127.0.0.1", port)); err != nil {
		t.Fatal(err)
	}
	rootSh := func(cmd string) string {
		t.Helper()
		out, err := docker("exec", name, "sh", "-c", cmd)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if _, err := docker("exec", name, "userdel", "--remove", "hostel"); err != nil {
		t.Fatal(err)
	}
	if _, err := docker("cp", filepath.Join(helperDir, "hostel-linux-"+dockerArch), name+":/usr/local/bin/hostel"); err != nil {
		t.Fatal(err)
	}

	r := e.adminHostel("config", "machines", "add", "box2", "--tool", "fake-screen")
	m := regexp.MustCompile(`machine join (hostel1-\S+)`).FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil || !strings.Contains(r.stdout, "--pinnedpubkey sha256//") || !strings.Contains(r.stdout, "/v1/install.sh | sudo sh -s -- ") {
		t.Fatalf("config machines add: %s", r)
	}
	code := m[1]
	if c := s.send("GET", "/v1/install.sh", "", nil, nil); c != 200 {
		t.Fatalf("GET /v1/install.sh: HTTP %d", c)
	}
	if c := s.send("GET", "/v1/download/hostel-linux-"+dockerArch, "", nil, nil); c != 200 {
		t.Fatalf("download the binary: HTTP %d", c)
	}
	if c := s.send("GET", "/v1/download/server.json", "", nil, nil); c != 404 {
		t.Fatalf("download another file: HTTP %d, want 404", c)
	}

	code2 := regexp.MustCompile(`machine join (hostel1-\S+)`).FindStringSubmatch(e.adminHostel("config", "machines", "add", "box2").stdout)[1]
	join := func(c string) (string, error) {
		return docker("exec", name, "/usr/local/bin/hostel", "machine", "join", c, "--no-service")
	}
	out, err := join(code)
	if err != nil || !strings.Contains(out, "added the machine home/box2") {
		t.Fatalf("machine join: %v\n%s", err, out)
	}
	connect := regexp.MustCompile(`Run as an administrator:\n   (\S+ machine connect .*)\n`).FindStringSubmatch(out)
	if connect == nil {
		t.Fatalf("machine join printed no connect command:\n%s", out)
	}
	// A second run continues with the result of the first.
	if out, err := join(code); err != nil || !strings.Contains(out, "Joined already as home/box2") {
		t.Fatalf("a second run of machine join: %v\n%s", err, out)
	}
	pub := strings.Join(strings.Fields(readFile(t, filepath.Join(e.dataDir(), "server_ed25519.pub")))[:2], " ")
	for _, f := range []string{"/home/hostel/.ssh/authorized_keys", "/home/hostel/hostel/authorized_keys.base"} {
		if got := rootSh("cat " + f); !strings.Contains(got, pub) {
			t.Fatalf("%s: %q, want the server key", f, got)
		}
	}
	if got := rootSh("stat -c %U:%a /etc/hostel/machine-token /etc/hostel/hostel"); got != "root:600\nroot:755" {
		t.Fatalf("machine token: %s", got)
	}
	doc := s.configDoc().Document
	for _, want := range []string{"[providers.home.machines.box2]", `connect = true`, `os = "linux"`, `tools = ["fake-screen"]`} {
		if !strings.Contains(doc, want) {
			t.Fatalf("the document after the join has no %s:\n%s", want, doc)
		}
	}

	// The command that join printed, with the test's SSH port.
	if _, err := docker(append([]string{"exec", "-d", name}, append(strings.Fields(connect[1]), "--ssh", fmt.Sprintf("127.0.0.1:%d", port))...)...); err != nil {
		t.Fatal(err)
	}
	s.waitMachine("home/box2", protocol.MachineRunning, machineReady)

	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	r = e.mustHostel("acquire", "fake-screen", "--task", "joined")
	if !strings.Contains(r.stdout, "is ready") {
		t.Fatalf("acquire on the joined machine: %s", r)
	}
	if r := e.mustHostel("run", "--", "id -un"); strings.TrimSpace(r.stdout) != "hostel" {
		t.Fatalf("user on the joined machine: %q", r.stdout)
	}
	e.mustHostel("release")
	// The lease stays active during its cleanup, and the server refuses to
	// remove a machine with an active lease (below): wait for the end.
	e.waitFor("the end of the lease on the joined machine", time.Minute, func() bool { return len(s.status().Leases) == 0 })

	// On a machine without the result of the join: the used code is refused,
	// and a second code for the same name too.
	fresh := func(c string) error {
		_, err := docker("exec", name, "sh", "-c", "mv /etc/hostel /tmp/joined && /usr/local/bin/hostel machine join "+c+
			" --no-service; rc=$?; rm -rf /etc/hostel; mv /tmp/joined /etc/hostel; exit $rc")
		return err
	}
	if err := fresh(code); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("a used machine code: %v", err)
	}
	if err := fresh(code2); err == nil || !strings.Contains(err.Error(), "home/box2 exists already") {
		t.Fatalf("a second code for the same machine: %v", err)
	}
	// Removed: its machine token stops working. A new code joins the machine
	// again, also with a new host key (the code proves the host).
	token := strings.TrimSpace(rootSh("cat /etc/hostel/machine-token"))
	if code := s.get("/v1/info", token, nil); code != 200 {
		t.Fatalf("the machine token before the removal: HTTP %d", code)
	}
	if r := e.adminHostel("config", "machines", "remove", "box2"); r.code != 0 {
		t.Fatalf("remove the machine: %s", r)
	}
	s.waitMachine("home/box2", protocol.MachineRetired, time.Minute)
	if code := s.get("/v1/info", token, nil); code != 401 {
		t.Fatalf("the machine token after the removal: HTTP %d, want 401", code)
	}
	rootSh("pkill -f '[h]ostel machine connect' || true; rm -f /etc/ssh/ssh_host_*; ssh-keygen -A")
	code3 := regexp.MustCompile(`machine join (hostel1-\S+)`).FindStringSubmatch(e.adminHostel("config", "machines", "add", "box2", "--tool", "fake-screen").stdout)[1]
	out, err = join(code3)
	if err != nil || !strings.Contains(out, "added the machine home/box2") {
		t.Fatalf("join again with a new code: %v\n%s", err, out)
	}
	connect = regexp.MustCompile(`Run as an administrator:\n   (\S+ machine connect .*)\n`).FindStringSubmatch(out)
	if _, err := docker(append([]string{"exec", "-d", name}, append(strings.Fields(connect[1]), "--ssh", fmt.Sprintf("127.0.0.1:%d", port))...)...); err != nil {
		t.Fatal(err)
	}
	s.waitMachine("home/box2", protocol.MachineRunning, machineReady)

	// A machine code is not a user code.
	if r := e.hostelAt(t.TempDir(), "connect", code2, "--name", "x"); r.code != 1 || !strings.Contains(r.stderr, "not valid") {
		t.Fatalf("hostel connect with a machine code: %s", r)
	}
}

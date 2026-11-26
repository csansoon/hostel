//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// secretTool is a tool that is not a screen but starts for each lease (M14):
// its start writes the hash of its secret (never the value) and the modes of
// the secret file and directory; its stop records whether the secret was
// still there.
var secretTool = map[string]string{
	"tool.toml": "description = \"Tool with a secret\"\nsecrets = [\"e2e-secret\"]\nenv = { SECRET_TOOL_ON = \"yes\" }\n",
	"start.linux.sh": `#!/bin/sh
set -e
f="$HOSTEL_SECRETS_DIR/e2e-secret"
echo "start secret $HOSTEL_LEASE_ID $(stat -c %a "$f") $(stat -c %a "$HOSTEL_SECRETS_DIR")" >> "$HOSTEL_CACHE_DIR/secret-tool.log"
echo "SECRET_SHA256=$(sha256sum "$f" | cut -d' ' -f1)" > tool.env
`,
	"stop.linux.sh": `#!/bin/sh
p=gone
if test -e "$HOSTEL_SECRETS_DIR/e2e-secret"; then p=present; fi
echo "stop secret $HOSTEL_LEASE_ID $p" >> "$HOSTEL_CACHE_DIR/secret-tool.log"
`,
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestSecrets (M14): admins store secrets on the server, and only the start
// and stop scripts of a tool that declares a secret get it, in the room of a
// lease with that tool. A lease for a tool whose secret is not set fails at
// once. Members cannot read or change secrets. The value is never in the
// server log, the database, or the configuration history. Cleanup deletes the
// secrets with the room; a removal is refused while a tool uses the secret.
func TestSecrets(t *testing.T) {
	t.Parallel()
	f := newFixture(t, &fixtureOpts{templateTools: []string{"fake-screen", "secret-tool"},
		tools: map[string]map[string]string{"secret-tool": secretTool}})
	e := f.e
	log := func() string { return f.c.sh("cat " + cacheDir + "/secret-tool.log 2>/dev/null; true") }
	adminIn := func(stdin string, args ...string) result {
		t.Helper()
		saved := e.vars["HOSTEL_TOKEN"]
		e.vars["HOSTEL_TOKEN"] = e.admin
		defer func() { e.vars["HOSTEL_TOKEN"] = saved }()
		return e.hostelIn(stdin, args...)
	}

	// Scenario 2: the secret is not set; the request fails at once.
	r := e.hostel("acquire", "secret-tool", "--task", "no secret")
	if r.code != 69 || !strings.Contains(r.stderr, `the tool secret-tool needs the secret e2e-secret, which is not set; an admin runs "hostel config secrets set e2e-secret <file>"`) {
		t.Fatalf("acquire of a tool whose secret is not set: %s", r)
	}
	if r := e.adminHostel("config", "secrets"); r.code != 0 || !regexp.MustCompile(`(?m)^e2e-secret\s+-\s+not set\s+secret-tool$`).MatchString(r.stdout) {
		t.Fatalf("config secrets before a set: %s", r)
	}

	// Scenario 1: a member cannot list, set, or remove; an admin sets a
	// secret from a file and one from stdin; the list has no value.
	value := randomHex(t)
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "secrets"}, {"config", "secrets", "set", "e2e-secret", file}, {"config", "secrets", "remove", "e2e-secret"}} {
		if r := e.hostel(args...); r.code == 0 || !strings.Contains(r.stderr, "admin") {
			t.Fatalf("hostel %s as a member: %s", strings.Join(args, " "), r)
		}
	}
	if r := e.adminHostel("config", "secrets", "set", "e2e-secret", file); r.code != 0 || !strings.Contains(r.stdout, "Stored the secret e2e-secret (48 B)") {
		t.Fatalf("config secrets set from a file: %s", r)
	}
	password := randomHex(t)
	if r := adminIn(password+"\n", "config", "secrets", "set", "other-secret", "-"); r.code != 0 || !strings.Contains(r.stdout, "Stored the secret other-secret (48 B)") {
		t.Fatalf("config secrets set from stdin: %s", r)
	}
	if r := e.adminHostel("config", "secrets", "set", "Bad_Name", file); r.code == 0 || !strings.Contains(r.stderr, "must match") {
		t.Fatalf("config secrets set with an invalid name: %s", r)
	}
	r = e.adminHostel("config", "secrets")
	if r.code != 0 || !regexp.MustCompile(`(?m)^e2e-secret\s+48 B\s+\d{4}-\d\d-\d\d \d\d:\d\d\s+secret-tool$`).MatchString(r.stdout) ||
		!regexp.MustCompile(`(?m)^other-secret\s+48 B\s+\d{4}-\d\d-\d\d \d\d:\d\d\s+-$`).MatchString(r.stdout) ||
		strings.Contains(r.stdout, value) || strings.Contains(r.stdout, password) {
		t.Fatalf("config secrets after two sets: %s", r)
	}

	// Scenario 3: the start script gets the secret in a private directory;
	// the commands of the guest do not get the directory.
	id := f.acquire("secret-tool")
	r = e.mustHostel("run", "--", `echo "$SECRET_SHA256|$HOSTEL_SECRETS_DIR|$SECRET_TOOL_ON|$HOSTEL_TOOLS"`)
	if r.stdout != sha256Hex(value)+"||yes|secret-tool\n" {
		t.Fatalf("the environment of a secret-tool lease: %q", r.stdout)
	}
	if !strings.Contains(log(), "start secret "+id+" 600 700") {
		t.Fatalf("the start of secret-tool:\n%s", log())
	}
	if l := f.lease(id); len(l.Started) != 1 || l.Started[0].Name != "secret-tool" || len(l.Started[0].Options) != 0 {
		t.Fatalf("started: %+v", l.Started)
	}
	if r := e.hostel("screenshot"); r.code != 1 || !strings.Contains(r.stderr, "the lease has no screen tool") {
		t.Fatalf("screenshot of a lease whose only started tool is not a screen: %s", r)
	}

	// Scenario 5: cleanup runs stop while the secret is there, then deletes it.
	e.mustHostel("release")
	f.waitRoomFree()
	if !strings.Contains(log(), "stop secret "+id+" present") {
		t.Fatalf("the stop of secret-tool:\n%s", log())
	}
	if got := f.c.sh("test -e /home/hostel/hostel/rooms/a/secrets && echo yes || echo no"); got != "no" {
		t.Fatalf("the secrets after the cleanup: %s", got)
	}

	// Scenario 6: a lease without the tool gets no secret, no start, and none
	// of the tool's env.
	id = f.acquire("fake-screen")
	if r := e.mustHostel("run", "--", `echo "$SECRET_SHA256|$SECRET_TOOL_ON"`); r.stdout != "|\n" {
		t.Fatalf("the environment of a lease without secret-tool: %q", r.stdout)
	}
	if got := f.c.sh("test -e /home/hostel/hostel/rooms/a/secrets && echo yes || echo no"); got != "no" {
		t.Fatalf("secrets in the room of a lease without secret-tool: %s", got)
	}
	if strings.Contains(log(), id) {
		t.Fatalf("secret-tool started for a lease without it:\n%s", log())
	}
	e.mustHostel("release")
	f.waitRoomFree()

	// Scenario 4: a new value reaches the next lease.
	newValue := randomHex(t)
	if r := adminIn(newValue, "config", "secrets", "set", "e2e-secret", "-"); r.code != 0 {
		t.Fatalf("replace the secret: %s", r)
	}
	// With a screen in the same lease, a screenshot needs no --tool.
	f.acquire("secret-tool", "fake-screen")
	if r := e.mustHostel("run", "--", "echo $SECRET_SHA256"); r.stdout != sha256Hex(newValue)+"\n" {
		t.Fatalf("the hash after a new value: %q, want the new one", r.stdout)
	}
	e.mustHostel("screenshot", "-o", filepath.Join(t.TempDir(), "shot.png"))
	e.mustHostel("release")
	f.waitRoomFree()

	// Scenario 7: a removal is refused while a tool uses the secret.
	if r := e.adminHostel("config", "secrets", "remove", "e2e-secret"); r.code == 0 || !strings.Contains(r.stderr, "the tool secret-tool uses the secret e2e-secret") {
		t.Fatalf("remove a secret that a tool uses: %s", r)
	}
	if r := e.adminHostel("config", "secrets", "remove", "other-secret"); r.code != 0 {
		t.Fatalf("remove a secret: %s", r)
	}
	if r := e.adminHostel("config", "secrets"); strings.Contains(r.stdout, "other-secret") {
		t.Fatalf("config secrets after a removal: %s", r)
	}

	// The values are only in the secret files of the server.
	hist := e.adminHostel("config", "history")
	for what, text := range map[string]string{"server log": f.s.log.String(), "config history": hist.stdout + hist.stderr} {
		for _, v := range []string{value, password, newValue} {
			if strings.Contains(text, v) {
				t.Fatalf("a secret value is in the %s", what)
			}
		}
	}
	secretsDir := filepath.Join(e.dataDir(), "secrets")
	err := filepath.WalkDir(e.dataDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(path, secretsDir+string(filepath.Separator)) {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, v := range []string{value, password, newValue} {
			if strings.Contains(string(data), v) {
				t.Fatalf("a secret value is in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(secretsDir, "e2e-secret")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the secret file on the server: %v %v", info, err)
	}
}

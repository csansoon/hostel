//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProviderLogin (M9): the server is not logged in to Namespace, so it
// refuses a Namespace provider; an admin runs "hostel config providers login
// namespace" on their computer: the server runs "nsc login --browser=false",
// the client prints its address, and ends when the login is complete. Then the
// provider is accepted, and a second login says that the server is logged in.
// The fake nsc completes the login when the test writes login-approved (the
// real devbox can be on this computer's PATH, so the test logs in nsc only).
func TestProviderLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	e.usePlatform("fakedevice")
	state := e.useFakeNsc(map[string]string{"FAKE_LOGGED_OUT": "1"})
	e.writeConfig(serverHeader())
	s := e.startServer("")

	doc := s.configDoc().Document + "\n[providers.ns]\nkind = \"namespace\"\n" + instanceMachine
	if r := e.applyDoc(doc); r.code != 1 || !strings.Contains(r.stderr, "nsc CLI is not ready") {
		t.Fatalf("a Namespace provider before the login: %s", r)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hostelBin, "config", "providers", "login", "namespace", "--cli", "nsc")
	cmd.Env = e.environ()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var lines []string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if strings.Contains(sc.Text(), "https://cloud.namespace.so/login/fake-nsc") {
			break
		}
	}
	if err := os.WriteFile(filepath.Join(state, "login-approved"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := cmd.Wait(); err != nil || !strings.Contains(strings.Join(lines, "\n"), "nsc: signed in.") {
		t.Fatalf("providers login: %v\nstdout:\n%s\nstderr:\n%s", err, strings.Join(lines, "\n"), stderr.String())
	}

	if r := e.adminHostel("config", "providers", "login", "namespace", "--cli", "nsc"); r.code != 0 || !strings.Contains(r.stdout, "logged in already") {
		t.Fatalf("a second login: %s", r)
	}
	if r := e.applyDoc(doc); r.code != 0 {
		t.Fatalf("a Namespace provider after the login: %s", r)
	}
}

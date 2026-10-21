//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Docker objects of the tests have the label hostel-e2e=<run id>. Each test
// removes its own objects; at start, the harness removes objects with the label
// that are older than 2 hours (left by a crashed run). Nothing else is touched.
const (
	roomImage   = "hostel-e2e-room"
	staleAfter  = 2 * time.Hour
	dockerLabel = "hostel-e2e"
)

var (
	// helperDir has the helper and mise binaries for the Docker architecture.
	helperDir string
	// dockerArch is the Go architecture of the Docker rooms.
	dockerArch string
)

func docker(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func setupDocker(binDir string) error {
	arch, err := docker("info", "--format", "{{.Architecture}}")
	if err != nil {
		return fmt.Errorf("Docker is necessary for the E2E tests: %w", err)
	}
	switch arch {
	case "aarch64", "arm64":
		dockerArch = "arm64"
	case "x86_64", "amd64":
		dockerArch = "amd64"
	default:
		return fmt.Errorf("unsupported Docker architecture %q", arch)
	}
	removeStale()
	if _, err := docker("build", "-q", "-t", roomImage, "--label", dockerLabel+"-image=room", "../roomimage"); err != nil {
		return err
	}
	helperDir = filepath.Join(binDir, "helpers")
	if err := os.MkdirAll(helperDir, 0o755); err != nil {
		return err
	}
	build := exec.Command("go", "build", "-o", filepath.Join(helperDir, "hostel-linux-"+dockerArch), "../../cmd/hostel")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+dockerArch)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("build the linux helper: %w", err)
	}
	mise, err := filepath.Abs("../../dist/mise-linux-" + dockerArch)
	if err != nil {
		return err
	}
	if _, err := os.Stat(mise); err != nil {
		return fmt.Errorf("%s is missing; run \"make dist\" (or \"make test-e2e\")", mise)
	}
	return os.Symlink(mise, filepath.Join(helperDir, "mise-linux-"+dockerArch))
}

// removeStale removes test containers older than staleAfter.
func removeStale() {
	out, err := docker("ps", "-a", "--filter", "label="+dockerLabel, "--format", "{{.ID}}\t{{.CreatedAt}}")
	if err != nil || out == "" {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		id, created, _ := strings.Cut(line, "\t")
		t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", created)
		if err == nil && time.Since(t) > staleAfter {
			docker("rm", "-f", "-v", id)
		}
	}
}

func (e *env) removeContainers() {
	out, err := docker("ps", "-a", "-q", "--filter", "label="+dockerLabel+"="+e.runID)
	if err != nil || out == "" {
		return
	}
	docker(append([]string{"rm", "-f", "-v"}, strings.Fields(out)...)...)
}

// freePort returns a free TCP port on 127.0.0.1.
func freePort(t interface{ Fatal(...any) }) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// container is a test room container.
type container struct {
	e    *env
	name string
	port int
}

// startStaticRoom starts a room container that accepts the server key. The
// explicit port mapping survives "docker stop" and "docker start".
func (e *env) startStaticRoom(name string) *container {
	e.t.Helper()
	c := e.createStaticRoom(name)
	c.start()
	return c
}

// createStaticRoom creates the room container but does not start it.
func (e *env) createStaticRoom(name string) *container {
	e.t.Helper()
	pub, err := os.ReadFile(filepath.Join(e.dataDir(), "server_ed25519.pub"))
	if err != nil {
		e.t.Fatalf("read the server key (run init first): %v", err)
	}
	c := &container{e: e, name: "hostel-e2e-" + e.runID + "-" + name, port: freePort(e.t)}
	if _, err := docker("create", "--name", c.name, "--label", dockerLabel+"="+e.runID,
		"-p", fmt.Sprintf("127.0.0.1:%d:22", c.port), "-e", "HOSTEL_AUTHORIZED_KEY="+strings.TrimSpace(string(pub)), roomImage); err != nil {
		e.t.Fatal(err)
	}
	return c
}

// start starts the container and waits until its SSH server sends its banner.
func (c *container) start() {
	c.e.t.Helper()
	if _, err := docker("start", c.name); err != nil {
		c.e.t.Fatal(err)
	}
	c.e.waitFor("SSH banner of "+c.name, time.Minute, func() bool {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", c.port), time.Second)
		if err != nil {
			return false
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4)
		_, err = io.ReadFull(conn, buf)
		return err == nil && string(buf) == "SSH-"
	})
}

func (c *container) stop() {
	c.e.t.Helper()
	if _, err := docker("stop", "-t", "1", c.name); err != nil {
		c.e.t.Fatal(err)
	}
}

// address is the static machine address for server.toml.
func (c *container) address() string { return fmt.Sprintf("hostel@127.0.0.1:%d", c.port) }

// sh runs a shell command in the container as the hostel user.
func (c *container) sh(cmd string) string {
	c.e.t.Helper()
	out, err := docker("exec", "-u", "hostel", c.name, "sh", "-c", cmd)
	if err != nil {
		c.e.t.Fatal(err)
	}
	return out
}

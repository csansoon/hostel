//go:build !windows

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// devboxMain is a fake Namespace devbox CLI for the E2E tests. It makes Docker
// rooms from test/roomimage and copies the output formats of the real CLI
// (devbox 0.0.204, docs/namespace.md): the list JSON, the text line of an
// empty list, and the error lines. Environment: FAKE_RUN_ID, FAKE_STATE_DIR,
// FAKE_IMAGE, FAKE_DELAY, and FAKE_LIMIT as for the provider mode, and
// FAKE_LOGGED_OUT ("1": "auth check-login" fails).
//
// FAKE_STATE_DIR keeps what Namespace stores: the key of each devbox, and the
// arguments of each create (create-<name>.args) for the tests.
func devboxMain(args []string) {
	if len(args) == 0 {
		devboxFail("usage: devbox <command>")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "auth":
		if os.Getenv("FAKE_LOGGED_OUT") == "1" {
			devboxFail("not logged in; run devbox login")
		}
	case "create":
		devboxCreate(rest)
	case "list":
		devboxList()
	case "delete":
		name := devboxName(rest)
		if err := exists(name); err != nil {
			devboxFail(fmt.Sprintf("rpc error: code = NotFound desc = no such devbox with id or name %q (rid=fake)", name))
		}
		if _, err := docker("rm", "-f", "-v", name); err != nil {
			devboxFail(err.Error())
		}
		fmt.Println("done.")
	case "configure-ssh":
		name := devboxName(rest)
		if err := exists(name); err != nil {
			devboxFail(fmt.Sprintf("rpc error: code = NotFound desc = no such devbox with id or name %q (rid=fake)", name))
		}
		key, err := os.ReadFile(filepath.Join(os.Getenv("FAKE_STATE_DIR"), name+".key"))
		if err != nil {
			devboxFail(err.Error())
		}
		if err := writeDevboxSSH(name, key); err != nil {
			devboxFail(err.Error())
		}
		fmt.Printf("Try\n  ssh %s.devbox.namespace\n", name)
	case "ssh-proxy":
		devboxProxy(devboxName(rest))
	default:
		devboxFail("unknown command " + cmd)
	}
}

// devboxFail prints an error as the real CLI does, with its update notice.
func devboxFail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	fmt.Fprint(os.Stderr, "\n A new version of devbox is available: 0.0.205. \n\n Run `devbox update` to upgrade.\n")
	os.Exit(1)
}

func devboxName(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	devboxFail("a devbox name is required")
	return ""
}

func devboxCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	name := fs.String("name", "", "")
	for _, f := range []string{"platform", "size", "image", "auto_stop_idle_timeout", "purpose"} {
		fs.String(f, "", "")
	}
	fs.Bool("ephemeral", false, "")
	fs.Bool("no_checkout", false, "")
	if err := fs.Parse(args); err != nil || *name == "" {
		devboxFail("create: bad flags")
	}
	state := os.Getenv("FAKE_STATE_DIR")
	if err := os.WriteFile(filepath.Join(state, "create-"+*name+".args"), []byte(strings.Join(args, " ")+"\n"), 0o644); err != nil {
		devboxFail(err.Error())
	}
	cs, err := containers()
	if err != nil {
		devboxFail(err.Error())
	}
	if n, err := strconv.Atoi(os.Getenv("FAKE_LIMIT")); err == nil && len(cs) >= n {
		devboxFail(fmt.Sprintf("rpc error: code = ResourceExhausted desc = failed to create devbox: per-user devbox limit reached (%d) (rid=fake)", n))
	}
	if _, ok := cs[*name]; ok {
		devboxFail(fmt.Sprintf("rpc error: code = AlreadyExists desc = devbox %q already exists (rid=fake)", *name))
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_DELAY")); err == nil {
		time.Sleep(d)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		devboxFail(err.Error())
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		devboxFail(err.Error())
	}
	key := pem.EncodeToMemory(block)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		devboxFail(err.Error())
	}
	image := os.Getenv("FAKE_IMAGE")
	if image == "" {
		image = "hostel-e2e-room"
	}
	port, err := freePort()
	if err != nil {
		devboxFail(err.Error())
	}
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if _, err := docker("run", "-d", "--name", *name, "--label", "hostel-e2e="+runID(), "--label", "hostel-fake=1",
		"-p", fmt.Sprintf("127.0.0.1:%d:22", port), "-e", "HOSTEL_AUTHORIZED_KEY="+pub,
		"-e", "NAMESPACE_DEVBOX_TASKS_DIR=/var/run/devbox/tasks", image); err != nil {
		devboxFail(err.Error())
	}
	// Only the call that made the container stores its key.
	if err := os.WriteFile(filepath.Join(state, *name+".key"), key, 0o600); err != nil {
		devboxFail(err.Error())
	}
	if err := waitSSH(*name); err != nil {
		devboxFail(err.Error())
	}
	if err := writeDevboxSSH(*name, key); err != nil {
		devboxFail(err.Error())
	}
	fmt.Printf(" You're now all setup! Next steps:\n\n ssh %s.devbox.namespace\n", *name)
}

// writeDevboxSSH writes the files of "devbox configure-ssh" in the home
// directory.
func writeDevboxSSH(name string, key []byte) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".namespace", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	base := filepath.Join(dir, name+".devbox.namespace")
	if err := os.WriteFile(base+".key", key, 0o600); err != nil {
		return err
	}
	conf := fmt.Sprintf("\nHost %s.devbox.namespace\n  IdentityFile %s.key\n  ProxyCommand devbox ssh-proxy %s\n  User hostel\n", name, base, name)
	return os.WriteFile(base+".ssh", []byte(conf), 0o644)
}

func devboxList() {
	cs, err := containers()
	if err != nil {
		devboxFail(err.Error())
	}
	type devbox struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		MainUser          string `json:"main_user"`
		DocumentedPurpose string `json:"documented_purpose"`
	}
	list := []devbox{}
	for name := range cs {
		list = append(list, devbox{ID: "id-" + name, Name: name, MainUser: "hostel", DocumentedPurpose: "fake"})
	}
	if len(list) == 0 {
		fmt.Println("No devbox available yet. Try running `devbox create`.")
	}
	out, _ := json.MarshalIndent(list, "", " ")
	fmt.Println(string(out))
}

// devboxProxy connects stdin and stdout to the room's SSH server.
func devboxProxy(name string) {
	if err := exists(name); err != nil {
		devboxFail(fmt.Sprintf("rpc error: code = NotFound desc = no such devbox with id or name %q (rid=fake)", name))
	}
	port, err := hostPort(name)
	if err != nil {
		devboxFail(err.Error())
	}
	fmt.Fprintf(os.Stderr, "\nConnecting to %s ...\n\n", name)
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		devboxFail(err.Error())
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(c, os.Stdin); c.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(os.Stdout, c); done <- struct{}{} }()
	<-done
}

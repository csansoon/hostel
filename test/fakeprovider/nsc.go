//go:build !windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// nscMain is a fake Namespace nsc CLI for the E2E tests. It copies the formats
// of the real CLI (nsc 0.0.587, docs/m4-implementation.md): the create and list
// JSON, "null" for an empty list, the "Listening on" line of port-forward, and a
// create that blocks at the plan limit. Environment: FAKE_RUN_ID,
// FAKE_STATE_DIR, FAKE_IMAGE, FAKE_DELAY (the create takes this long; the
// instance exists from the start), FAKE_LIMIT (the number of instances at which
// a create blocks), and FAKE_LOGGED_OUT.
//
// An instance is a state file. "nsc ssh" runs the command with a shell and a
// "docker" shim (this binary under the name docker), whose "run" starts the
// room container from FAKE_IMAGE with the instance's room name. So the tests
// run the provider's real start script.
//
// FAKE_STATE_DIR also gets the arguments of the calls for the tests:
// create-<room>.args, docker-<room>.args, and extend-<room>.args (one line for
// each call).
func nscMain(args []string) {
	if len(args) == 0 {
		nscFail("usage: nsc <command>")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "auth":
		if os.Getenv("FAKE_LOGGED_OUT") == "1" {
			nscFail("You are not logged in. Run `nsc login`.")
		}
	case "create":
		nscCreate(rest)
	case "list":
		nscList()
	case "destroy":
		nscDestroy(firstArg(rest))
	case "ssh":
		nscSSH(rest)
	case "instance":
		if len(rest) == 0 {
			nscFail("usage: nsc instance <command>")
		}
		switch rest[0] {
		case "extend-duration":
			nscExtend(rest[1:])
		case "port-forward":
			nscPortForward(rest[1:])
		default:
			nscFail("unknown command instance " + rest[0])
		}
	default:
		nscFail("unknown command " + cmd)
	}
}

func nscFail(msg string) {
	fmt.Fprintf(os.Stderr, "========================================\nFailed: %s\n========================================\n\nSee https://namespace.so/docs for more help.\n", msg)
	os.Exit(1)
}

func firstArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	nscFail("an instance ID is required")
	return ""
}

type fakeInstance struct {
	ID       string            `json:"cluster_id"`
	Labels   map[string]string `json:"labels"`
	Created  time.Time         `json:"created_at"`
	Deadline time.Time         `json:"deadline"`
}

func nscDir() string { return filepath.Join(os.Getenv("FAKE_STATE_DIR"), "nsc") }

func readInstance(id string) (fakeInstance, error) {
	var i fakeInstance
	data, err := os.ReadFile(filepath.Join(nscDir(), id+".json"))
	if err != nil {
		return i, err
	}
	return i, json.Unmarshal(data, &i)
}

func writeInstance(i fakeInstance) error {
	data, err := json.Marshal(i)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(nscDir(), i.ID+".json"), data, 0o644)
}

// instancesNow returns the instances and destroys those whose duration ended,
// as Namespace does.
func instancesNow() ([]fakeInstance, error) {
	files, err := filepath.Glob(filepath.Join(nscDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []fakeInstance
	for _, f := range files {
		i, err := readInstance(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			continue
		}
		if time.Now().After(i.Deadline) {
			removeInstance(i)
			continue
		}
		out = append(out, i)
	}
	return out, nil
}

func removeInstance(i fakeInstance) {
	if name := i.Labels["hostel.name"]; name != "" && exists(name) == nil {
		docker("rm", "-f", "-v", name)
	}
	os.Remove(filepath.Join(nscDir(), i.ID+".json"))
}

func appendArgs(file string, line string) {
	f, err := os.OpenFile(filepath.Join(os.Getenv("FAKE_STATE_DIR"), file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		nscFail(err.Error())
	}
	_, err = fmt.Fprintln(f, line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		nscFail(err.Error())
	}
}

type labelFlag map[string]string

func (l labelFlag) String() string { return "" }
func (l labelFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New("label must be K=V")
	}
	l[k] = val
	return nil
}

func nscCreate(args []string) {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	labels := labelFlag{}
	fs.Var(labels, "label", "")
	duration := fs.Duration("duration", 0, "")
	sshKey := fs.String("ssh_key", "", "")
	for _, f := range []string{"machine_type", "purpose", "o"} {
		fs.String(f, "", "")
	}
	fs.Bool("bare", false, "")
	if err := fs.Parse(args); err != nil || *duration <= 0 {
		nscFail("create: bad flags")
	}
	name := labels["hostel.name"]
	appendArgs("create-"+name+".args", strings.Join(args, " "))
	if _, err := os.ReadFile(*sshKey); err != nil {
		nscFail(fmt.Sprintf("failed to load keys: %v", err))
	}
	if err := os.MkdirAll(nscDir(), 0o755); err != nil {
		nscFail(err.Error())
	}
	list, err := instancesNow()
	if err != nil {
		nscFail(err.Error())
	}
	// At the plan limit the real call blocks without output until the caller
	// stops it.
	if n, err := strconv.Atoi(os.Getenv("FAKE_LIMIT")); err == nil && len(list) >= n {
		time.Sleep(10 * time.Minute)
		nscFail("still at the limit")
	}
	b := make([]byte, 7)
	rand.Read(b)
	i := fakeInstance{ID: hex.EncodeToString(b)[:13], Labels: labels, Created: time.Now(), Deadline: time.Now().Add(*duration)}
	if err := writeInstance(i); err != nil {
		nscFail(err.Error())
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_DELAY")); err == nil {
		time.Sleep(d)
	}
	json.NewEncoder(os.Stdout).Encode(map[string]string{"cluster_id": i.ID, "instance_id": i.ID})
}

func nscList() {
	list, err := instancesNow()
	if err != nil {
		nscFail(err.Error())
	}
	if len(list) == 0 {
		fmt.Println("null")
		return
	}
	out, _ := json.MarshalIndent(list, "", "  ")
	fmt.Println(string(out))
}

func nscDestroy(id string) {
	if i, err := readInstance(id); err == nil {
		removeInstance(i)
	}
}

func instanceArg(args []string) fakeInstance {
	id := firstArg(args)
	i, err := readInstance(id)
	if err != nil || time.Now().After(i.Deadline) {
		nscFail(fmt.Sprintf("instance %q not found", id))
	}
	return i
}

func nscExtend(args []string) {
	i := instanceArg(args)
	fs := flag.NewFlagSet("extend-duration", flag.ContinueOnError)
	minimum := fs.Duration("ensure_minimum", 0, "")
	if err := fs.Parse(args[1:]); err != nil || *minimum <= 0 {
		nscFail("extend-duration: bad flags")
	}
	appendArgs("extend-"+i.Labels["hostel.name"]+".args", "--ensure_minimum "+minimum.String())
	if d := time.Now().Add(*minimum); d.After(i.Deadline) {
		i.Deadline = d
	}
	if err := writeInstance(i); err != nil {
		nscFail(err.Error())
	}
}

// nscSSH runs the command with a shell, as root on the instance would, with
// the docker shim first on PATH.
func nscSSH(args []string) {
	i := instanceArg(args)
	var command []string
	for k, a := range args {
		if a == "--" {
			command = args[k+1:]
		}
	}
	self, err := os.Executable()
	if err != nil {
		nscFail(err.Error())
	}
	realDocker, err := exec.LookPath("docker")
	if err != nil {
		nscFail(err.Error())
	}
	bin := filepath.Join(os.Getenv("FAKE_STATE_DIR"), "nsc-bin")
	os.MkdirAll(bin, 0o755)
	if _, err := os.Lstat(filepath.Join(bin, "docker")); err != nil {
		if err := os.Symlink(self, filepath.Join(bin, "docker")); err != nil && !os.IsExist(err) {
			nscFail(err.Error())
		}
	}
	cmd := exec.Command("/bin/sh", "-c", strings.Join(command, " "))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_DOCKER="+realDocker, "FAKE_NSC_ROOM="+i.Labels["hostel.name"])
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		nscFail("Process exited with status 1")
	}
}

// dockerShimMain handles "docker run" on a fake instance: it starts the room
// container from FAKE_IMAGE with the instance's room name and publishes the
// room's SSH port. Host networking and devices do not apply.
func dockerShimMain(args []string) {
	room := os.Getenv("FAKE_NSC_ROOM")
	if len(args) == 0 || args[0] != "run" || room == "" {
		fmt.Fprintln(os.Stderr, "fake docker: only \"docker run\" on a fake instance")
		os.Exit(1)
	}
	appendArgs("docker-"+room+".args", strings.Join(args, " "))
	run := []string{"run", "-d", "--name", room, "--label", "hostel-e2e=" + runID(), "--label", "hostel-fake=1"}
	rest := args[1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		switch rest[0] {
		case "-d":
			rest = rest[1:]
		case "-e", "--entrypoint":
			run = append(run, rest[0], rest[1])
			rest = rest[2:]
		default: // --name, --restart, --network, --device
			rest = rest[2:]
		}
	}
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "fake docker: no image")
		os.Exit(1)
	}
	port, err := freePort()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	image := os.Getenv("FAKE_IMAGE")
	if image == "" {
		image = "hostel-e2e-room"
	}
	run = append(run, "-p", fmt.Sprintf("127.0.0.1:%d:2222", port), image)
	run = append(run, rest[1:]...)
	cmd := exec.Command(os.Getenv("FAKE_DOCKER"), run...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}

// nscPortForward listens on a free local port and forwards each connection to
// the room container's published port.
func nscPortForward(args []string) {
	i := instanceArg(args)
	fs := flag.NewFlagSet("port-forward", flag.ContinueOnError)
	target := fs.Int("target_port", 0, "")
	if err := fs.Parse(args[1:]); err != nil || *target == 0 {
		nscFail("port-forward: bad flags")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		nscFail(err.Error())
	}
	fmt.Printf("Listening on %s\n", ln.Addr())
	name := i.Labels["hostel.name"]
	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		go func() {
			defer c.Close()
			out, err := docker("port", name, fmt.Sprintf("%d/tcp", *target))
			if err != nil {
				return
			}
			r, err := net.Dial("tcp", strings.Split(out, "\n")[0])
			if err != nil {
				return
			}
			defer r.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(r, c); r.(*net.TCPConn).CloseWrite(); done <- struct{}{} }()
			go func() { io.Copy(c, r); done <- struct{}{} }()
			<-done
		}()
	}
}

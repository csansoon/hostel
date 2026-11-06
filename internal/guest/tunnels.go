package guest

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
)

// startTunnels writes the tunnels of hostel.toml with their room ports, starts
// the port process, and waits until the room confirms each tunnel.
func startTunnels(st *State, rc *config.Repo, deadline time.Time) error {
	tunnels := map[string]*TunnelConfig{}
	for name, t := range rc.Tunnels {
		tunnels[name] = &TunnelConfig{LocalPort: t.LocalPort, LocalPortEnv: t.LocalPortEnv, EnvValue: os.Getenv(t.LocalPortEnv),
			FixedRoomPort: t.RoomPort, RoomPort: st.Access.TunnelPorts[name]}
	}
	if len(tunnels) == 0 {
		return nil
	}
	if err := saveTunnels(st, tunnels); err != nil {
		return err
	}
	if err := ensurePorts(st); err != nil {
		return err
	}
	for {
		resp, err := callPorts(st, portRequest{Op: "status"})
		if err != nil {
			return err
		}
		confirmed := 0
		for _, t := range resp.Tunnels {
			if t.Error != "" {
				return fmt.Errorf("tunnel %s: %s", t.Name, t.Error)
			}
			if t.Confirmed {
				confirmed++
			}
		}
		if confirmed == len(tunnels) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the tunnels are not confirmed yet; see %s/ports.log", st.Dir)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func printTunnels(w io.Writer, resp portResponse) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TUNNEL\tROOM PORT\tLOCAL\tSTATE")
	for _, t := range resp.Tunnels {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", t.Name, t.RoomPort, t.Local, t.state())
	}
	tw.Flush()
}

// state is what a tunnel needs now, or "confirmed": both sides work.
func (t tunnelStatus) state() string {
	switch {
	case t.Error != "":
		return t.Error
	case t.Problem != "":
		return t.Problem
	case !t.Confirmed:
		return "connecting"
	}
	return "confirmed"
}

// Tunnel is "hostel tunnel list|add|set|remove".
func Tunnel(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "hostel: usage: hostel tunnel list | add <name> (--local-port N | --local-port-env VAR) [--room-port P] | set <name> [--local-port N] [--room-port P] | remove <name>")
		return ExitUsage
	}
	if len(args) == 0 {
		return usage()
	}
	fs := cli.NewFlagSet("hostel tunnel "+args[0], stderr)
	localPort := fs.Int("local-port", 0, "local port")
	localEnv := fs.String("local-port-env", "", "variable in .env (or the environment) with the local port")
	roomPort := fs.Int("room-port", 0, "fixed room port (1024 to 65535)")
	pos, err := cli.Parse(fs, args[1:])
	if err != nil {
		return ExitUsage
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	req := portRequest{Op: "tunnel_" + args[0]}
	switch args[0] {
	case "list":
		if len(pos) != 0 {
			return usage()
		}
		req.Op = "check"
	case "add":
		if len(pos) != 1 || set["local-port"] == set["local-port-env"] {
			return usage()
		}
		req.LocalPortEnv, req.EnvValue = *localEnv, os.Getenv(*localEnv)
	case "set":
		if len(pos) != 1 || (!set["local-port"] && !set["room-port"]) {
			return usage()
		}
	case "remove":
		if len(pos) != 1 {
			return usage()
		}
	default:
		return usage()
	}
	if len(pos) == 1 {
		req.Name = pos[0]
	}
	if set["local-port"] {
		req.LocalPort = localPort
	}
	if set["room-port"] {
		req.RoomPort = roomPort
	}
	l, code := openLease(stderr)
	if l == nil {
		return code
	}
	if err := ensurePorts(l.st); err != nil {
		return l.fail(stderr, err)
	}
	resp, err := callPorts(l.st, req)
	if err != nil {
		return l.fail(stderr, err)
	}
	printTunnels(stdout, resp)
	if args[0] != "list" && (set["room-port"] || args[0] == "add" || args[0] == "remove") {
		fmt.Fprintln(stderr, "hostel: new jobs get the new HOSTEL_TUNNEL_<NAME>_PORT values; running jobs keep the old ones")
		if args[0] == "set" {
			fmt.Fprintln(stderr, "hostel: warning: build again if a build embedded the old room address")
		}
	}
	return 0
}

// Forward is "hostel forward <room-port> [--local-port N] | forward list |
// forward stop <room-port>".
func Forward(args []string, stdout, stderr io.Writer) int {
	usage := func() int {
		fmt.Fprintln(stderr, "hostel: usage: hostel forward <room-port> [--local-port N] | hostel forward list | hostel forward stop <room-port>")
		return ExitUsage
	}
	fs := cli.NewFlagSet("hostel forward", stderr)
	localPort := fs.Int("local-port", 0, "local port (default: the room port, or a free port)")
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) == 0 {
		return usage()
	}
	var req portRequest
	portArg := func(s string) (*int, bool) {
		p, err := strconv.Atoi(s)
		return &p, err == nil
	}
	switch {
	case pos[0] == "list" && len(pos) == 1:
		req.Op = "status"
	case pos[0] == "stop" && len(pos) == 2:
		p, ok := portArg(pos[1])
		if !ok {
			return usage()
		}
		req = portRequest{Op: "forward_stop", RoomPort: p}
	case len(pos) == 1:
		p, ok := portArg(pos[0])
		if !ok {
			return usage()
		}
		req = portRequest{Op: "forward_add", RoomPort: p}
		if *localPort != 0 {
			req.LocalPort = localPort
		}
	default:
		return usage()
	}
	l, code := openLease(stderr)
	if l == nil {
		return code
	}
	if err := ensurePorts(l.st); err != nil {
		return l.fail(stderr, err)
	}
	resp, err := callPorts(l.st, req)
	if err != nil {
		return l.fail(stderr, err)
	}
	if req.Op == "forward_add" {
		fmt.Fprintf(stdout, "127.0.0.1:%d\n", resp.LocalPort)
		fmt.Fprintf(stderr, "hostel: forward 127.0.0.1:%d → room 127.0.0.1:%d\n", resp.LocalPort, *req.RoomPort)
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROOM PORT\tLOCAL")
	for _, f := range resp.Forwards {
		fmt.Fprintf(tw, "%d\t127.0.0.1:%d\n", f.RoomPort, f.LocalPort)
	}
	tw.Flush()
	return 0
}

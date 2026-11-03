// Command hostel is the single binary for the Hostel server, guests, and rooms.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/guest"
	"github.com/csansoon/hostel/internal/room"
	"github.com/csansoon/hostel/internal/version"
)

type command struct {
	name    string
	summary string
	hidden  bool
	run     func(args []string, stdout, stderr io.Writer) int
}

var commands = []command{
	{name: "connect", summary: "Connect this computer to a Hostel server with an invite code", run: connectCmd},
	{name: "disconnect", summary: "Remove this computer's connection", run: disconnectCmd},
	{name: "platforms", summary: "List platforms with free slots, queue length, and estimated wait", run: guest.Platforms},
	{name: "acquire", summary: "Create or continue a lease for a platform", run: guest.Acquire},
	{name: "sync", summary: "Send changed files to the lease again", run: guest.Sync},
	{name: "run", summary: "Run a command in the lease work directory", run: guest.Run},
	{name: "attach", summary: "Attach to the output of a job", run: guest.Attach},
	{name: "jobs", summary: "List the jobs of the lease", run: guest.Jobs},
	{name: "stop", summary: "Stop a job", run: guest.Stop},
	{name: "screenshot", summary: "Take a screenshot of a device", run: guest.Screenshot},
	{name: "cp", summary: "Copy files to or from the lease work directory", run: guest.Cp},
	{name: "diff", summary: "Show changes on the room since the last sync", run: guest.Diff},
	{name: "ssh", summary: "Open an interactive shell in the lease", run: guest.SSH},
	{name: "ssh-config", summary: "Write an ssh_config file for the lease", run: guest.SSHConfig},
	{name: "tunnel", summary: "List or change tunnels from the room to this machine", run: guest.Tunnel},
	{name: "forward", summary: "Forward a local port to a port on the room", run: guest.Forward},
	{name: "status", summary: "Show the rooms, leases, and queues; \"hostel status <room|platform>\" shows details", run: statusCmd},
	{name: "ls", hidden: true, run: statusCmd},
	{name: "rooms", hidden: true, run: statusCmd},
	{name: "release", summary: "End the lease", run: guest.Release},
	{name: "agent-guide", summary: "Print usage instructions for coding agents", run: guest.AgentGuide},
	{name: "config", summary: "The server configuration: machines, providers, settings, users (admins change it)", run: configCmd},
	{name: "admin", summary: "Admin commands: room tokens, leases, rooms, configuration", run: adminCmd},
	{name: "server", summary: "Initialize or run the Hostel server", run: serverCmd},
	{name: "ports", hidden: true, run: guest.Ports},
	{name: "gateway-proxy", hidden: true, run: guest.GatewayProxy},
	{name: "machine", summary: "On a room machine: join a server (hostel machine join <code>)", run: machineCmd},
	{name: "room", hidden: true, run: roomCmd},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return 2
	}
	switch args[0] {
	case "--version":
		fmt.Fprintln(stdout, "hostel", version.Version)
		return 0
	case "-h", "--help", "help":
		printHelp(stdout)
		return 0
	}
	for _, c := range commands {
		if c.name == args[0] {
			if needsConnection(c.name, args[1:]) {
				if _, err := config.LoadGuest(); errors.Is(err, config.ErrNotConnected) {
					fmt.Fprintln(stderr, "hostel:", err)
					return ExitNotConnected
				}
			}
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "hostel: unknown command %q; run \"hostel help\" for the list of commands\n", args[0])
	return 2
}

// needsConnection: commands that talk to a server need a connection; these do
// not (they run on the server or a room machine, or set up the connection).
func needsConnection(name string, args []string) bool {
	switch name {
	// The others report the missing connection themselves.
	case "connect", "disconnect", "server", "room", "machine", "agent-guide", "ports", "gateway-proxy":
		return false
	case "admin":
		// "admin config check|schema" is local; without a subcommand, admin
		// prints its usage.
		return len(args) > 0 && args[0] != "config" && !strings.HasPrefix(args[0], "-")
	}
	return true
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "Hostel gives short, temporary, exclusive access to remote machines.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: hostel <command> [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands {
		if !c.hidden {
			fmt.Fprintf(w, "  %-12s %s\n", c.name, c.summary)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run \"hostel --version\" to print the version.")
}

func roomCmd(args []string, stdout, stderr io.Writer) int {
	return room.Main(args, os.Stdin, stdout, stderr)
}

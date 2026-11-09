// Command hostel is the single binary for the Hostel server, guests, and machines.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/csansoon/hostel/internal/app"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/guest"
	"github.com/csansoon/hostel/internal/helper"
	"github.com/csansoon/hostel/internal/update"
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
	{name: "tools", summary: "List the tools of the machines with free rooms, waiting leases, and estimated wait", run: guest.Tools},
	{name: "acquire", summary: "Create or continue the lease of this worktree, with the tools that it needs", run: guest.Acquire},
	{name: "info", summary: "Print the facts of the lease: machine, tools, and started screens", run: guest.Info},
	{name: "sync", summary: "Send changed files to the lease again", run: guest.Sync},
	{name: "run", summary: "Run a command in the lease work directory", run: guest.Run},
	{name: "attach", summary: "Attach to the output of a job", run: guest.Attach},
	{name: "jobs", summary: "List the jobs of the lease", run: guest.Jobs},
	{name: "stop", summary: "Stop a job", run: guest.Stop},
	{name: "screenshot", summary: "Take a screenshot of a screen tool of the lease", run: guest.Screenshot},
	{name: "cp", summary: "Copy files to or from the lease work directory", run: guest.Cp},
	{name: "diff", summary: "Show changes on the machine since the last sync", run: guest.Diff},
	{name: "ssh", summary: "Open an interactive shell in the lease", run: guest.SSH},
	{name: "ssh-config", summary: "Write an ssh_config file for the lease", run: guest.SSHConfig},
	{name: "tunnel", summary: "List or change tunnels from the machine to this computer", run: guest.Tunnel},
	{name: "forward", summary: "Forward a local port to a port on the machine", run: guest.Forward},
	{name: "status", summary: "Show the machines, leases, and the waiting line; \"hostel status <machine|tool>\" shows details", run: statusCmd},
	{name: "ls", hidden: true, run: statusCmd},
	{name: "machines", hidden: true, run: statusCmd},
	{name: "release", summary: "End the lease", run: guest.Release},
	{name: "update", summary: "Replace this hostel binary with the latest release", run: updateCmd},
	{name: "agent-guide", summary: "Print usage instructions for coding agents", run: guest.AgentGuide},
	{name: "config", summary: "The server configuration: machines, providers, settings, users (admins change it)", run: configCmd},
	{name: "admin", summary: "Admin commands: machine tokens, leases, machines, configuration", run: adminCmd},
	{name: "server", summary: "Initialize or run the Hostel server", run: serverCmd},
	{name: "ports", hidden: true, run: guest.Ports},
	{name: "keep-place", hidden: true, run: guest.KeepPlace},
	{name: "gateway-proxy", hidden: true, run: guest.GatewayProxy},
	{name: "machine", summary: "On a machine: join a server (hostel machine join <code>)", run: templateCmd},
	{name: "helper", hidden: true, run: helperCmd},
	// A desktop task that was installed before M10g runs "room desktop serve".
	{name: "room", hidden: true, run: helperCmd},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--demo" {
		// The app with made-up data, to try its screens without a server; it
		// changes nothing on this computer.
		if err := app.RunDemo(version.Version, stdout); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		return 0
	}
	update.CleanOld()
	if len(args) == 0 {
		// People get the Hostel app; without a terminal (an agent, a pipe),
		// the help.
		if isTerminal(os.Stdin) && isTerminalWriter(stdout) {
			if err := app.Run(version.Version, stdout); err != nil {
				fmt.Fprintln(stderr, "hostel:", err)
				return 1
			}
			return 0
		}
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
				var once sync.Once
				client.ServerVersion = func(v string) {
					once.Do(func() { update.Notice(stderr, v, version.Version, noticeFile()) })
				}
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
// not (they run on the server or a machine, or set up the connection).
func needsConnection(name string, args []string) bool {
	switch name {
	// The others report the missing connection themselves.
	case "connect", "disconnect", "server", "helper", "room", "machine", "agent-guide", "ports", "gateway-proxy", "update":
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
	fmt.Fprintln(w, "People: run \"hostel\" alone in a terminal to open the Hostel app.")
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

func helperCmd(args []string, stdout, stderr io.Writer) int {
	return helper.Main(args, os.Stdin, stdout, stderr)
}

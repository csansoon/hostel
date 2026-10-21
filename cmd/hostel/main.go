// Command hostel is the single binary for the Hostel server, guests, and rooms.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/csansoon/hostel/internal/version"
)

type command struct {
	name    string
	summary string
	hidden  bool
	run     func(args []string, stdout, stderr io.Writer) int
}

var commands = []command{
	{name: "platforms", summary: "List platforms with free slots, queue length, and estimated wait"},
	{name: "acquire", summary: "Create or continue a lease for a platform"},
	{name: "sync", summary: "Send changed files to the lease again"},
	{name: "run", summary: "Run a command in the lease work directory"},
	{name: "attach", summary: "Attach to the output of a job"},
	{name: "jobs", summary: "List the jobs of the lease"},
	{name: "stop", summary: "Stop a job"},
	{name: "screenshot", summary: "Take a screenshot of a device"},
	{name: "cp", summary: "Copy files to or from the lease work directory"},
	{name: "diff", summary: "Show changes on the room since the last sync"},
	{name: "ssh", summary: "Open an interactive shell in the lease"},
	{name: "ssh-config", summary: "Write an ssh_config file for the lease"},
	{name: "tunnel", summary: "List or change tunnels from the room to this machine"},
	{name: "forward", summary: "Forward a local port to a port on the room"},
	{name: "status", summary: "Show all leases, rooms, and the queue"},
	{name: "release", summary: "End the lease"},
	{name: "agent-guide", summary: "Print usage instructions for coding agents"},
	{name: "admin", summary: "Admin commands: tokens, leases, rooms, configuration", run: adminCmd},
	{name: "server", summary: "Initialize or run the Hostel server", run: serverCmd},
	{name: "ports", hidden: true},
	{name: "room", hidden: true},
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
			if c.run == nil {
				return notImplemented(c.name, stderr)
			}
			return c.run(args[1:], stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "hostel: unknown command %q; run \"hostel help\" for the list of commands\n", args[0])
	return 2
}

func notImplemented(name string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "hostel: %s: not implemented yet\n", name)
	return 1
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

package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/protocol"
)

// FormatWait formats an estimated wait.
func FormatWait(seconds int) string {
	if seconds <= 0 {
		return "now"
	}
	d := time.Duration(seconds) * time.Second
	if d < time.Minute {
		return "<1 min"
	}
	return fmt.Sprintf("~%d min", int(d.Round(time.Minute).Minutes()))
}

// FormatDuration formats a timeout: "45 s", "5 min".
func FormatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	// Down, not to the nearest minute: the message must not promise more.
	return fmt.Sprintf("%d min", int(d.Minutes()))
}

// Tools is "hostel tools [<tool>] [--json]" (R1).
func Tools(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel tools", stderr)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) > 1 {
		fmt.Fprintln(stderr, "hostel: usage: hostel tools [<tool>] [--json]")
		return ExitUsage
	}
	api, err := apiClient()
	if err != nil {
		return fail(stderr, err)
	}
	var list protocol.Tools
	if err := api.Do(context.Background(), "GET", "/v1/tools", nil, &list); err != nil {
		return fail(stderr, err)
	}
	if len(pos) == 1 {
		i := slices.IndexFunc(list.Tools, func(t protocol.Tool) bool { return t.Name == pos[0] })
		if i < 0 {
			fmt.Fprintf(stderr, "hostel: no machine of this server has the tool %q; run \"hostel tools\" for the list\n", pos[0])
			return ExitUnavailable
		}
		if *asJSON {
			return printJSON(stdout, stderr, list.Tools[i])
		}
		printTool(stdout, list.Tools[i])
		return 0
	}
	if *asJSON {
		return printJSON(stdout, stderr, list)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tOPTIONS\tSCREEN\tMACHINES\tFREE\tWAITING\tEST. WAIT")
	for _, t := range list.Tools {
		screen := ""
		if t.Screen {
			screen = "yes"
		}
		var machines []string
		for _, m := range t.Machines {
			machines = append(machines, m.Key)
		}
		free := fmt.Sprint(t.FreeSlots)
		wait := FormatWait(t.EstimatedWaitSeconds)
		if t.FreeSlots == 0 && t.NewRoom {
			free, wait = "-", wait+" (new room)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", t.Name, dash(strings.Join(t.Options, ", ")), screen,
			strings.Join(machines, ", "), free, t.Waiting, wait)
	}
	tw.Flush()
	fmt.Fprintln(stdout, list.Note)
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printJSON(stdout, stderr io.Writer, v any) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// printTool prints the details of one tool and its guide.
func printTool(w io.Writer, t protocol.Tool) {
	head := t.Name
	if len(t.Options) > 0 {
		head += " " + strings.Join(t.Options, ", ")
	}
	desc := t.Description
	if t.Mise {
		desc = "installed with mise"
	}
	fmt.Fprintf(w, "%s: %s\n", head, desc)
	if t.Screen {
		kind := "a screen: it starts for each lease that asks for it"
		if t.WholeMachine {
			kind += "; the lease takes the whole machine"
		}
		fmt.Fprintf(w, "  screen       %s\n", kind)
	}
	if len(t.Needs) > 0 {
		fmt.Fprintf(w, "  needs        %s\n", strings.Join(t.Needs, ", "))
	}
	if len(t.Env) > 0 {
		fmt.Fprintf(w, "  environment  %s\n", strings.Join(t.Env, ", "))
	}
	var machines []string
	for _, m := range t.Machines {
		desc := m.OS
		if len(m.Options) > 0 {
			desc = strings.Join(m.Options, ", ") + ", " + desc
		}
		if size := m.Hardware.Size(); size != "" {
			desc += ", " + size
		}
		if m.OnDemand {
			desc += "; on demand"
		}
		if !m.Online {
			desc += "; offline"
		}
		machines = append(machines, m.Key+" ("+desc+")")
	}
	fmt.Fprintf(w, "  machines     %s\n", strings.Join(machines, ", "))
	fmt.Fprintf(w, "  free         %d slots, %d waiting, estimated wait %s\n", t.FreeSlots, t.Waiting, FormatWait(t.EstimatedWaitSeconds))
	if g := strings.TrimSpace(t.Guide); g != "" {
		fmt.Fprintf(w, "\n%s\n", g)
	}
}

// Info is "hostel info": the lease facts again.
func Info(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel info", stderr)
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	l, code := openLease(stderr)
	if l == nil {
		return code
	}
	var pl protocol.Lease
	if err := l.api.Do(context.Background(), "GET", "/v1/leases/"+l.st.LeaseID, nil, &pl); err == nil && pl.Access != nil {
		l.st.Access, l.st.Started, l.st.Machine = pl.Access, pl.Started, pl.Provider+"/"+pl.Machine
		if err := l.st.Save(); err != nil {
			return l.fail(stderr, err)
		}
	}
	rc, err := l.repo.RepoConfig()
	if err != nil {
		return l.fail(stderr, err)
	}
	printFacts(l.st, rc, stdout)
	return 0
}

// AgentGuide is "hostel agent-guide": the general rules, then the guide of
// each tool that the machines of this server have.
func AgentGuide(args []string, stdout, stderr io.Writer) int {
	fmt.Fprint(stdout, agentGuide)
	api, err := apiClient()
	if err != nil {
		fmt.Fprint(stdout, "\nTools of this server\n  This computer is not connected to a Hostel server, so this guide has no\n  tool guides. Ask the user to run \"hostel connect <invite code>\".\n")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var list protocol.Tools
	if err := api.Do(ctx, "GET", "/v1/tools", nil, &list); err != nil {
		fmt.Fprint(stdout, "\nTools of this server\n  Not available now; run \"hostel tools\" later.\n")
		return fail(stderr, fmt.Errorf("read the tools of the server: %w", err))
	}
	fmt.Fprintln(stdout, "\nTools of this server (\"hostel tools <tool>\" shows one with its machines)")
	if len(list.Tools) == 0 {
		fmt.Fprintln(stdout, "  The machines of this server list no tools: acquire without tools.")
	}
	for _, t := range list.Tools {
		head := t.Name
		switch {
		case t.Mise:
			head += " (installed with mise)"
		case t.Screen:
			head += " (screen)"
		}
		if len(t.Options) > 0 {
			head += ": " + strings.Join(t.Options, ", ")
		}
		fmt.Fprintf(stdout, "\n%s\n", head)
		if g := strings.TrimSpace(t.Guide); g != "" {
			for _, line := range strings.Split(g, "\n") {
				fmt.Fprintln(stdout, strings.TrimRight("  "+line, " "))
			}
		} else if t.Description != "" {
			fmt.Fprintf(stdout, "  %s\n", t.Description)
		}
	}
	return 0
}

const agentGuide = `Hostel: short, exclusive use of a remote machine for builds, UI QA, and
device tests. Your code stays local; Hostel sends it to the room.

The rule
  Ask for the tools that you need. Ask for a screen tool (an emulator, a
  simulator, a desktop) only when you must see or drive the app: it takes
  time and memory. All software of the machine is usable in each lease, also
  the tools that you did not ask for; asking for a tool only selects a
  machine that has it. The tools of the repository's mise files (node, pnpm,
  python, ...) are set up on every machine: do not ask for them. One lease for
  each worktree: for work on two machines at the same time, use a second
  worktree (git worktree add). When the lease is ready, read the machine
  facts that acquire prints (OS, hardware, tools, started screens).

Start
  hostel tools [<tool>]                    the tools, their options, machines,
                                           and wait; one tool with its guide
  hostel status                            rooms, leases, and the waiting line
  hostel acquire <tool[:option]>... --task "..."
                                           one lease for this worktree; without
                                           tools: the tools of hostel.toml, or
                                           any machine
    An option is a version of a tool (node:22 matches 22.11.0) or a phone or
    tablet of a screen tool ("hostel tools" lists them). Without an option,
    any version matches, and a screen tool starts its first option.
    --priority      high or low, only when the user asks for it
    --background    return at once; Hostel keeps the place in the waiting line
  When acquire exits with code 75, run the SAME command again. It continues
  the same lease. Code 69: no machine can serve the request (the message says
  why); do not retry, change the tools. A waiting lease keeps its place only
  while you run that command: after a few minutes without it (5 by default;
  acquire prints the time), the lease ends. To do other work while you wait,
  add --background: Hostel keeps the place for you; run the same command again
  at least every 10 minutes (by default; acquire prints the time): a ready
  lease waits only that long.
  Code 78: this computer is not connected to a Hostel server. Do not try to
  connect; ask the user to run "hostel connect <invite code>" with a code from
  an admin of their server.

Work
  hostel info                   the machine facts of the lease again
  hostel run -- "<command>"     runs in the work directory; one argument runs
                                with sh -c; output streams back
  hostel sync                   send local changes again (do this after edits)
  hostel sync --setup           run the repository setup again
  hostel screenshot [--tool <tool[:option]>] [-o file.png]
                                a screen of the lease; --tool only when the
                                lease has more than one screen
  hostel cp <local> :<path> | hostel cp :<path> <local>
  hostel diff                   changes on the room since the last sync
  hostel jobs | hostel attach <job> | hostel stop <job>

Long commands
  run and attach wait 100 seconds, then return; the job goes on. Keep each
  hostel command shorter than the time limit of your own shell tool, and give
  the wait of a long build yourself:
    hostel run --detach -- "./gradlew build"   prints the job ID at once
    hostel attach <job> --wait 9m              shows the output until the job
                                               ends or the wait ends
  Run attach again until the job ends. A running job alone does not keep the
  lease; a running attach does. Between two commands, the time without
  activity that acquire prints applies (10 minutes by default).

Exit codes of run and attach
  The last line on stderr says what happened. Always read it:
    "job <id> exited with code N"   the command ended; N is also the exit code
    "job <id> is still running"     exit code 75; run "hostel attach <id>"
                                    Do NOT run "hostel run" again: that starts
                                    a second job.

Facts and the local API
  Commands get the environment of the machine's tools and the facts of the
  screens that started (the guides below name them; acquire and "hostel
  info" list them). To tap, type, and read a screen, use the driver that its
  guide names (agent-device; winapp on Windows).
  Tunnels in hostel.toml make your local servers reachable on the room at
  localhost:$HOSTEL_TUNNEL_<NAME>_PORT.

End
  hostel release                when you are done. A lease also ends after
                                10 minutes without a hostel command.
Changes on the room are temporary. Make durable changes in your local files.
Cleanup deletes the work directory and the lease's files, but not the caches
outside it (mise tools, $HOSTEL_CACHE_DIR, ~/.gradle, and other files in the
home directory). The next lease on the room, also of another user, can read
them. They make builds faster; never write a secret there.
If a command says 'run "hostel update"', tell the user; do not update hostel
yourself (other agents on this computer use the same binary).
`

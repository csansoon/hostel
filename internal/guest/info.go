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
			free, wait = "-", "new room"
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

// AgentGuide is "hostel agent-guide".
func AgentGuide(args []string, stdout, stderr io.Writer) int {
	fmt.Fprint(stdout, agentGuide)
	return 0
}

const agentGuide = `Hostel: short, exclusive use of a remote machine for builds, UI QA, and
device tests. Your code stays local; Hostel sends it to the room.

Start
  hostel platforms                         what you can request now
  hostel status --plain                    rooms, leases, and queues (who uses
                                           what); "hostel status <room>" details
  hostel acquire <platform> --task "..."   one lease for this worktree
    --device <d>    a device of the platform (repeat for several)
    --require <l>   a machine label, for example desktop
    --priority      high or low, only when the user asks for it
    --background    return at once; Hostel keeps the place in the queue
  When acquire exits with code 75, run the SAME command again. It continues
  the same lease. Code 69: no machine can serve the request; do not retry.
  A waiting lease keeps its place only while you run that command: after a
  few minutes without it (5 by default; acquire prints the time), the lease
  ends. To do other work while you wait, add --background: Hostel keeps the
  place for you; run the same command again at least every 10 minutes (by
  default; acquire prints the time): a ready lease waits only that long. One worktree for each agent: agents in the same worktree share
  one lease.
  Code 78: this computer is not connected to a Hostel server. Do not try to
  connect; ask the user to run "hostel connect <invite code>" with a code from
  an admin of their server.

Work
  hostel run -- "<command>"     runs in the work directory; one argument runs
                                with sh -c; output streams back
  hostel sync                   send local changes again (do this after edits)
  hostel sync --setup           run the repository setup again
  hostel screenshot [--device d] [-o file.png]
  hostel cp <local> :<path> | hostel cp :<path> <local>
  hostel diff                   changes on the room since the last sync
  hostel jobs | hostel attach <job> | hostel stop <job>

Exit codes of run and attach
  The last line on stderr says what happened. Always read it:
    "job <id> exited with code N"   the command ended; N is also the exit code
    "job <id> is still running"     exit code 75; run "hostel attach <id>"
                                    Do NOT run "hostel run" again: that starts
                                    a second job.

Devices and the local API
  Commands get the device facts in their environment. iOS: SIMULATOR_UDID (the
  first device) and SIMULATOR_UDIDS. Each lease gets new, clean simulators:
    xcodebuild ... -destination "id=$SIMULATOR_UDID" build
    xcrun simctl install "$SIMULATOR_UDID" <path to the .app>
    xcrun simctl launch "$SIMULATOR_UDID" <bundle id>
  To tap, type, and read the screen, use agent-device on the room. Open the
  app on the lease device once; later commands use that session:
    hostel run -- 'agent-device open <bundle id> --platform ios --udid "$SIMULATOR_UDID" --foreground'
    hostel run -- 'agent-device click "label=\"Sign in\"" --settle'
    hostel run -- 'agent-device fill "role=text-field" "me@example.com" --settle'
  "hostel run -- agent-device help" lists all commands.
  macOS desktop apps: hostel acquire macos --require desktop (the lease takes
  the whole machine). Build the app, start it with "open", then control it:
    hostel run -- 'xcodebuild -scheme MyApp -derivedDataPath build build'
    hostel run -- 'open build/Build/Products/Debug/MyApp.app'
    hostel run -- 'agent-device open <bundle id> --platform macos'
    hostel run -- 'agent-device snapshot -i --platform macos'
    hostel run -- 'agent-device click "id=save" --platform macos'
  The first agent-device command of a lease takes about 30 s. If a command
  says the runner is still finishing an earlier command, wait a few seconds and
  run it again. "hostel screenshot" shows the whole desktop. At the release,
  Hostel ends the apps that the lease started.
  Windows desktop apps: hostel acquire windows --require desktop. Commands
  run over SSH and cannot show windows; "desktop" runs a command on the
  desktop, and winapp (Microsoft's winapp CLI) always runs there:
    hostel run -- 'dotnet build -c Release -o out'
    hostel run -- 'desktop --detach out\MyApp.exe'
    hostel run -- 'winapp ui inspect -a MyApp --interactive'
    hostel run -- 'winapp ui invoke save -a MyApp'
    hostel run -- 'winapp ui set-value email "me@example.com" -a MyApp'
    hostel run -- 'winapp ui click save -a MyApp'
    hostel run -- 'winapp ui send-keys "text" -a MyApp --via send-input'
  Prefer invoke and set-value (UI Automation). click and send-keys are real
  input: they need the app in the foreground and fail if it is not. Start an
  app in a normal window (not hidden or minimized). "hostel run -- winapp ui
  --help" lists all commands. At the release, Hostel ends the apps that
  "desktop" started.
  Tunnels in hostel.toml make your local servers reachable on the room at
  localhost:$HOSTEL_TUNNEL_<NAME>_PORT.

End
  hostel release                when you are done. A lease also ends after
                                10 minutes without a hostel command.
Changes on the room are temporary. Make durable changes in your local files.
If a command says 'run "hostel update"', tell the user; do not update hostel
yourself (other agents on this computer use the same binary).
`

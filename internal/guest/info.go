package guest

import (
	"context"
	"fmt"
	"io"
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

// Platforms is "hostel platforms" (R1).
func Platforms(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel platforms", stderr)
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	api, err := apiClient()
	if err != nil {
		return fail(stderr, err)
	}
	var list []protocol.Platform
	if err := api.Do(context.Background(), "GET", "/v1/platforms", nil, &list); err != nil {
		return fail(stderr, err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PLATFORM\tKIND\tDEVICES\tFREE\tQUEUE\tEST. WAIT\tDESCRIPTION")
	for _, p := range list {
		devices := "-"
		if len(p.Devices) > 0 {
			devices = strings.Join(p.Devices, ", ")
		}
		free := fmt.Sprint(p.FreeSlots)
		wait := FormatWait(p.EstimatedWaitSeconds)
		if p.FreeSlots == 0 && p.NewRoom {
			free, wait = "-", "new room"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", p.Name, p.Kind, devices, free, p.Queue, wait, p.Description)
	}
	tw.Flush()
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
  When acquire exits with code 75, run the SAME command again. It continues
  the same lease. Code 69: no machine can serve the request; do not retry.
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
`

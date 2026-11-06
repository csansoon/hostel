package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// pollWait is the long-poll time of one request (design, section 10).
const pollWait = 50 * time.Second

// backgroundLook bounds the look of acquire --background at the lease.
const backgroundLook = 5 * time.Second

// factsTimeout bounds the look at the room for the lease facts.
const factsTimeout = 40 * time.Second

// maxPollRetry is the longest pause before a poll after a network error.
const maxPollRetry = 10 * time.Second

// keepNote says how the place of a waiting lease is kept.
func keepNote(l protocol.Lease, background bool, stderr io.Writer) {
	idle := "a few minutes"
	if l.IdleTimeoutMS > 0 {
		idle = FormatDuration(time.Duration(l.IdleTimeoutMS) * time.Millisecond)
	}
	if background {
		fmt.Fprintf(stderr, "hostel: Hostel keeps the place of lease %s in the background. Do other work, and run the same command again at least every %s: it continues the lease, and takes the room when it is ready (a ready room waits only %s for you)\n", l.ID, idle, idle)
		return
	}
	keep := "a few minutes"
	if l.PollTimeoutMS > 0 {
		keep = FormatDuration(time.Duration(l.PollTimeoutMS) * time.Millisecond)
	}
	fmt.Fprintf(stderr, "hostel: the server keeps the place of lease %s for %s without this command; run it again before then, or add --background to keep the place while you do other work\n", l.ID, keep)
}

func notFinished(stderr io.Writer) int {
	fmt.Fprintln(stderr, "hostel: not finished; run the same command again")
	return ExitNotFinished
}

// Acquire is "hostel acquire [<tool[:option]>...] --task ...". It is
// resumable: each step records its result in the local state, and the same
// command continues. A worktree has one lease (design, section 10).
func Acquire(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel acquire", stderr)
	task := fs.String("task", "", "what the lease is for (required)")
	priority := fs.String("priority", "normal", "low, normal, or high; change it only when the user asks for it")
	fresh := fs.Bool("fresh", false, "use a new room with empty caches")
	wait := fs.Duration("wait", 100*time.Second, "time to wait before returning; then run the same command again")
	background := fs.Bool("background", false, "return at once and keep the place in the waiting line in the background; run the same command again to continue")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		fmt.Fprintln(stderr, "hostel: usage: hostel acquire [<tool[:option]>...] --task \"...\" [--priority low|normal|high] [--fresh] [--wait 100s] [--background]")
		return ExitUsage
	}
	for _, raw := range pos {
		if _, err := config.ParseToolEntry(raw); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return ExitUsage
		}
	}
	deadline := time.Now().Add(*wait)
	ctx := context.Background()

	repo, err := FindRepo()
	if err != nil {
		return fail(stderr, err)
	}
	rc, err := repo.RepoConfig()
	if err != nil {
		return fail(stderr, err)
	}
	api, err := apiClient()
	if err != nil {
		return fail(stderr, err)
	}
	st, ok, err := repo.LoadState()
	if err != nil {
		return fail(stderr, err)
	}
	tools := pos
	if len(tools) == 0 {
		tools = rc.Tools
	}
	if ok && !st.Final {
		if len(pos) > 0 && !sameTools(pos, st.Tools) {
			return fail(stderr, fmt.Errorf("this worktree has the lease %s (%s); release it first (hostel release), or use another worktree (git worktree add)",
				st.LeaseID, toolsText(st.Tools)))
		}
		warnDifferences(st, *task, *priority, *fresh, stderr)
	} else {
		if strings.TrimSpace(*task) == "" {
			fmt.Fprintln(stderr, "hostel: --task is required: describe what the lease is for")
			return ExitUsage
		}
		code, err := createLease(ctx, api, repo, rc, st, tools, *task, *priority, *fresh)
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return code
		}
		fmt.Fprintf(stderr, "hostel: lease %s created for %s\n", st.LeaseID, toolsText(tools))
	}
	l := &lease{repo: repo, st: st, api: api, background: *background}

	if st.Access == nil {
		until, look := deadline, ctx
		if *background {
			// A short look at the lease, with its own deadline; then the
			// keeper polls it.
			if err := ensureKeeper(st); err != nil {
				return fail(stderr, err)
			}
			until = time.Now().Add(2 * time.Second)
			var cancel context.CancelFunc
			look, cancel = context.WithTimeout(ctx, backgroundLook)
			defer cancel()
		}
		code := waitReady(look, l, until, stderr)
		if code >= 0 {
			return code
		}
		// --background only means "do not wait in the queue": a ready lease
		// goes on to the sync and the setup, so that the same command, run
		// again, always gets further.
	}
	steps := []struct {
		name string
		run  func() (int, error)
	}{
		{StepEnv, func() (int, error) { return 0, setLeaseEnv(ctx, st, rc) }},
		{StepPorts, func() (int, error) { return 0, startTunnels(st, rc, deadline) }},
		{StepSynced, func() (int, error) { _, err := syncFiles(ctx, st, repo, rc, stderr); return 0, err }},
		{StepSetup, func() (int, error) {
			return runSetup(ctx, l, setupCommand(rc, st.leaseTools(), st.Access.RoomOS, true), deadline, stderr)
		}},
	}
	order := []string{StepCreated, StepEnv, StepPorts, StepSynced, StepSetup, StepDone}
	for _, step := range steps {
		if slices.Index(order, st.Step) >= slices.Index(order, step.name) {
			continue
		}
		if time.Now().After(deadline) {
			return notFinished(stderr)
		}
		code, err := step.run()
		if err != nil {
			return fail(stderr, err)
		}
		if code == ExitNotFinished {
			return notFinished(stderr)
		}
		st.Step = step.name
		if err := st.Save(); err != nil {
			return fail(stderr, err)
		}
		if code != 0 {
			return code
		}
	}
	st.Step = StepDone
	if err := st.Save(); err != nil {
		return fail(stderr, err)
	}
	printFacts(st, rc, stdout)
	return 0
}

// sameTools reports whether two requests have the same tools (in any order).
func sameTools(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

func toolsText(tools []string) string {
	if len(tools) == 0 {
		return "any machine"
	}
	return strings.Join(tools, " ")
}

func warnDifferences(st *State, task, priority string, fresh bool, stderr io.Writer) {
	var diffs []string
	if task != "" && task != st.Task {
		diffs = append(diffs, "--task")
	}
	if priority != st.Priority {
		diffs = append(diffs, "--priority")
	}
	if fresh != st.Fresh {
		diffs = append(diffs, "--fresh")
	}
	if len(diffs) > 0 {
		fmt.Fprintf(stderr, "hostel: warning: %s differ from the existing lease %s; it continues unchanged (release it first to change them)\n",
			strings.Join(diffs, ", "), st.LeaseID)
	}
}

func createLease(ctx context.Context, api *client.Client, repo *Repo, rc *config.Repo, st *State, tools []string, task, priority string,
	fresh bool) (int, error) {
	pub, err := st.NewKey()
	if err != nil {
		return ExitError, err
	}
	var tunnels []protocol.TunnelRequest
	for _, name := range rc.TunnelNames() {
		tunnels = append(tunnels, protocol.TunnelRequest{Name: name, RoomPort: rc.Tunnels[name].RoomPort})
	}
	host, _ := os.Hostname()
	req := protocol.CreateLeaseRequest{Tools: tools, Task: task, Priority: priority, Tunnels: tunnels, Fresh: fresh, PublicKey: pub,
		GuestHost: host, Repo: filepath.Base(repo.Root), Worktree: repo.Root, Branch: currentBranch(repo.Root)}
	var l protocol.Lease
	if err := api.Do(ctx, "POST", "/v1/leases", req, &l); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity {
			return ExitUnavailable, err
		}
		return ExitError, err
	}
	*st = State{Dir: st.Dir, LeaseID: l.ID, Tools: l.Tools, Root: repo.Root, Task: task, Priority: priority,
		Fresh: fresh, Step: StepCreated}
	return 0, st.Save()
}

// waitReady long-polls the lease until it is ready. It returns -1 when the
// lease is ready, else the exit code of the command.
func waitReady(ctx context.Context, l *lease, deadline time.Time, stderr io.Writer) int {
	var since int64
	var retry time.Duration
	last := ""
	var seen protocol.Lease
	// polled: a poll was sent (also a failed one). Each acquire sends at
	// least one; after that, the deadline counts.
	polled := false
	for {
		remaining := time.Until(deadline)
		// Less than 1 s left after a poll: another one would ask for wait=0
		// and answer at once (a burst of polls); stop here. The first poll
		// always runs: it is what keeps the place of a waiting lease.
		if remaining < time.Second && polled {
			if seen.State == protocol.LeaseQueued || seen.State == protocol.LeaseAssigned ||
				seen.State == protocol.LeaseProvisioning || seen.State == protocol.LeasePreparing {
				keepNote(seen, l.background || keeperAlive(l.st), stderr)
			}
			return notFinished(stderr)
		}
		w := min(pollWait, remaining)
		var lease protocol.Lease
		path := fmt.Sprintf("/v1/leases/%s?since=%d&wait=%ds", l.st.LeaseID, since, int(w.Seconds()))
		polled = true
		if err := l.api.Do(ctx, "GET", path, nil, &lease); err != nil {
			if l.background && ctx.Err() != nil {
				// The server did not answer the short look: the keeper
				// keeps the place.
				fmt.Fprintf(stderr, "hostel: the server did not answer within %s; Hostel keeps the place of lease %s in the background\n", FormatDuration(backgroundLook), l.st.LeaseID)
				return notFinished(stderr)
			}
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				l.st.Final = true
				l.st.Save()
			}
			if !transient(err) {
				return fail(stderr, err)
			}
			if retry == 0 {
				fmt.Fprintf(stderr, "hostel: %v; retrying\n", err)
			}
			retry = min(max(2*retry, time.Second), maxPollRetry)
			if time.Until(deadline) < time.Second {
				// No time for another try: the loop ends at the deadline.
				continue
			}
			select {
			case <-time.After(min(retry, time.Until(deadline))):
			case <-ctx.Done():
				return fail(stderr, ctx.Err())
			}
			continue
		}
		retry = 0
		since, seen = lease.Revision, lease
		line := describe(lease)
		if line != last {
			fmt.Fprintln(stderr, "hostel: "+line)
			last = line
		}
		switch {
		case lease.State == protocol.LeaseReady:
			l.st.Access, l.st.Started, l.st.Machine = lease.Access, lease.Started, lease.Provider+"/"+lease.Machine
			if err := l.st.Save(); err != nil {
				return fail(stderr, err)
			}
			return -1
		case isFinal(lease.State):
			l.st.Final = true
			l.st.Save()
			fmt.Fprintf(stderr, "hostel: lease %s ended (%s: %s); run the command again for a new lease\n", lease.ID, lease.State, lease.EndReason)
			return ExitError
		}
	}
}

// transient reports whether a poll error is a network error or a proxy
// error, which the next poll can survive. Other errors (a TLS certificate, an
// unknown host, a bad response) fail at once.
func transient(err error) bool {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	var opErr *net.OpError
	return errors.As(err, &opErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || os.IsTimeout(err)
}

func isFinal(state string) bool {
	switch state {
	case protocol.LeaseReleased, protocol.LeaseExpired, protocol.LeaseCancelled, protocol.LeaseFailed:
		return true
	}
	return false
}

func describe(l protocol.Lease) string {
	s := fmt.Sprintf("lease %s %s", l.ID, l.State)
	if l.State == protocol.LeaseQueued {
		if l.Position > 0 {
			s += fmt.Sprintf(", position %d", l.Position)
		}
		if l.Reason != "" {
			s += " (" + l.Reason + ")"
		}
		if l.Position > 0 {
			s += fmt.Sprintf(", waiting behind %d leases, estimated wait %s", l.WaitingBehind, FormatWait(l.EstimatedWaitSeconds))
		}
	}
	if l.Machine != "" && l.State != protocol.LeaseQueued {
		s += " on " + l.Provider + "/" + l.Machine
	}
	return s
}

// leaseEnv computes lease.env: the repository env for the lease's tools with
// the tunnel ports, and HOSTEL_TUNNEL_<NAME>_PORT for each tunnel.
func leaseEnv(tools []string, rc *config.Repo, ports map[string]int) map[string]string {
	_, repoEnv := rc.LeaseSetup(tools)
	env := config.ExpandEnv(repoEnv, ports)
	for name, port := range ports {
		env["HOSTEL_TUNNEL_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))+"_PORT"] = fmt.Sprint(port)
	}
	return env
}

func setLeaseEnv(ctx context.Context, st *State, rc *config.Repo) error {
	conn, err := Dial(ctx, st, "cmd")
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Helper(ctx, []string{"set-env"}, map[string]any{"env": leaseEnv(st.leaseTools(), rc, st.Access.TunnelPorts)}, nil)
}

// miseFiles are the files that make the setup job run "mise install" again.
var miseFiles = []string{"mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml", ".tool-versions",
	".nvmrc", ".python-version", ".config/mise.toml", ".config/mise/config.toml", "mise/config.toml", ".mise/config.toml"}

// setupCommand returns the setup job: "mise install" (unless mise = false) and,
// with full, the repository setup commands for the lease's tools. The first
// failure stops the job.
func setupCommand(rc *config.Repo, tools []string, roomOS string, full bool) string {
	if roomOS == config.OSWindows {
		return setupCommandPS(rc, tools, full)
	}
	var parts []string
	if rc.MiseEnabled() {
		// The job PATH comes from "mise env" at the job start, so it does not
		// have the tools that this "mise install" adds.
		parts = append(parts, `mise install`, `PATH="$(mise bin-paths | tr '\n' ':')$PATH"`)
	}
	if full {
		setup, _ := rc.LeaseSetup(tools)
		for _, c := range setup {
			parts = append(parts, "("+c+")")
		}
	}
	return strings.Join(parts, " && ")
}

// setupCommandPS is setupCommand for PowerShell, which the helper uses on
// Windows. Windows PowerShell 5.1 has no "&&".
func setupCommandPS(rc *config.Repo, tools []string, full bool) string {
	const stop = "\nif (-not $?) { if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1 }"
	var parts []string
	if rc.MiseEnabled() {
		parts = append(parts, "mise install"+stop, `$env:PATH = ((mise bin-paths) -join ';') + ';' + $env:PATH`)
	}
	if full {
		setup, _ := rc.LeaseSetup(tools)
		for _, c := range setup {
			parts = append(parts, c+stop)
		}
	}
	return strings.Join(parts, "\n")
}

// runSetup continues the running setup job, or starts cmd as the setup job
// (if not empty), and streams its output to stderr.
func runSetup(ctx context.Context, l *lease, cmd string, deadline time.Time, stderr io.Writer) (int, error) {
	st := l.st
	if cmd == "" && st.SetupJob == "" {
		return 0, nil
	}
	if st.SetupJob == "" {
		id, err := startJob(ctx, l, protocol.JobRequest{Command: []string{cmd}, Shell: true}, deadline)
		if err != nil {
			return 0, fmt.Errorf("start the setup job: %w", err)
		}
		st.SetupJob, st.SetupOffset = id, 0
		if err := st.Save(); err != nil {
			return 0, err
		}
		fmt.Fprintf(stderr, "hostel: setup job %s: %s\n", st.SetupJob, cmd)
	}
	res, err := attachJob(ctx, l, st.SetupJob, st.SetupOffset, stderr, deadline)
	st.SetupOffset = res.offset
	if serr := st.Save(); err == nil {
		err = serr
	}
	if err != nil {
		return 0, err
	}
	job := st.SetupJob
	switch res.Status {
	case JobRunning:
		return ExitNotFinished, nil
	case JobLost:
		st.SetupJob = ""
		return ExitError, fmt.Errorf("setup job %s was lost; run \"hostel sync --setup\"", job)
	}
	st.SetupJob = ""
	if res.ExitCode != 0 {
		fmt.Fprintf(stderr, "hostel: setup job %s failed with code %d; the lease stays ready; fix the code and run \"hostel sync --setup\"\n", job, res.ExitCode)
		return ExitError, nil
	}
	return 0, nil
}

// printFacts prints the lease facts (design, section 10): the machine, its
// OS and hardware, all its tools with their options and environment names,
// the screen tools that started, and the next commands.
func printFacts(st *State, rc *config.Repo, w io.Writer) {
	a := st.Access
	fmt.Fprintf(w, "Lease %s is ready on %s.\n", st.LeaseID, st.Machine)
	machine := a.RoomOS
	if a.Hardware != nil && a.Hardware.OSVersion != "" {
		machine += " " + a.Hardware.OSVersion
	}
	machine += " (" + a.RoomArch + ")"
	if size := a.Hardware.Size(); size != "" {
		machine += ", " + size
	}
	fmt.Fprintf(w, "  machine:    %s\n", machine)
	var tools, screens []string
	for _, t := range a.Tools {
		if t.Screen {
			screens = append(screens, t.Name)
			continue
		}
		tools = append(tools, toolFact(t.Name, t.Options, t.Env))
	}
	if len(tools) == 0 {
		tools = []string{"-"}
	}
	fmt.Fprintf(w, "  tools:      %s\n", strings.Join(tools, " · "))
	if repo := repositoryTools(st, rc); len(repo) > 0 {
		fmt.Fprintf(w, "  repository: %s   (your mise files; jobs use these first)\n", strings.Join(repo, " · "))
	}
	var started []string
	for _, s := range st.Started {
		started = append(started, toolFact(s.Name, s.Options, s.Facts))
	}
	switch {
	case len(started) > 0:
		fmt.Fprintf(w, "  started:    %s\n", strings.Join(started, " · "))
	case len(screens) > 0:
		fmt.Fprintf(w, "  started:    -   (no screen tools; add %s to get one)\n", strings.Join(screens, " or "))
	default:
		fmt.Fprintf(w, "  started:    -   (this machine has no screen tools)\n")
	}
	fmt.Fprintf(w, "  slot:       %s\n", a.Slot)
	fmt.Fprintf(w, "  work dir:   %s\n", a.WorkDir)
	fmt.Fprintf(w, "  expires:    %s at the latest; without activity at %s (each hostel command extends it)\n",
		a.ExpiresAt.Local().Format(time.DateTime), a.IdleExpiresAt.Local().Format(time.DateTime))
	tunnels, _ := loadTunnels(st)
	if len(tunnels) > 0 {
		if resp, err := callPorts(st, portRequest{Op: "check"}); err == nil {
			fmt.Fprintln(w, "  tunnels:")
			for _, t := range resp.Tunnels {
				fmt.Fprintf(w, "    %s: room port %d → local %s: %s\n", t.Name, t.RoomPort, t.Local, t.state())
			}
		}
	}
	env := leaseEnv(st.leaseTools(), rc, tunnelPorts(tunnels))
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintln(w, "  environment:")
		for _, k := range keys {
			fmt.Fprintf(w, "    %s=%s\n", k, env[k])
		}
	}
	next := "Next: hostel run -- <command>   hostel sync   hostel release"
	if len(st.Started) > 0 {
		next = "Next: hostel run -- <command>   hostel sync   hostel screenshot   hostel release"
	}
	fmt.Fprintln(w, next)
}

// repositoryTools returns the tools that the repository's mise files select in
// the work directory, now. The facts come first: an error only omits the line.
func repositoryTools(st *State, rc *config.Repo) []string {
	if !rc.MiseEnabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), factsTimeout)
	defer cancel()
	conn, err := Dial(ctx, st, "cmd")
	if err != nil {
		return nil
	}
	defer conn.Close()
	var tools []protocol.MiseTool
	if err := conn.Helper(ctx, []string{"mise-current"}, nil, &tools); err != nil {
		return nil
	}
	var out []string
	for _, t := range tools {
		out = append(out, t.Name+" "+t.Version)
	}
	return out
}

// toolFact is "name options (environment names)".
func toolFact(name string, options, env []string) string {
	s := name
	if len(options) > 0 {
		s += " " + strings.Join(options, ", ")
	}
	if len(env) > 0 {
		s += " (" + strings.Join(env, ", ") + ")"
	}
	return s
}

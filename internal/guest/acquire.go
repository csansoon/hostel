package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func notFinished(stderr io.Writer) int {
	fmt.Fprintln(stderr, "hostel: not finished; run the same command again")
	return ExitNotFinished
}

// Acquire is "hostel acquire <platform> --task ...". It is resumable: each step
// records its result in the local state, and the same command continues.
func Acquire(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel acquire", stderr)
	task := fs.String("task", "", "what the lease is for (required)")
	var devices, require cli.StringList
	fs.Var(&devices, "device", "device to use (repeat for several)")
	priority := fs.String("priority", "normal", "low, normal, or high; change it only when the user asks for it")
	fs.Var(&require, "require", "label that the machine must have (repeat for several)")
	fresh := fs.Bool("fresh", false, "use a new room with empty caches")
	wait := fs.Duration("wait", 100*time.Second, "time to wait before returning; then run the same command again")
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, "hostel: usage: hostel acquire <platform> --task \"...\" [--device d]... [--priority low|normal|high] [--require label]... [--fresh] [--wait 100s]")
		return ExitUsage
	}
	platform := pos[0]
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
	st, ok, err := repo.LoadState(platform)
	if err != nil {
		return fail(stderr, err)
	}
	if ok && !st.Final {
		warnDifferences(st, *task, *priority, devices, require, *fresh, stderr)
	} else {
		if strings.TrimSpace(*task) == "" {
			fmt.Fprintln(stderr, "hostel: --task is required: describe what the lease is for")
			return ExitUsage
		}
		code, err := createLease(ctx, api, repo, rc, st, platform, *task, *priority, devices, require, *fresh)
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return code
		}
		fmt.Fprintf(stderr, "hostel: lease %s created for %s\n", st.LeaseID, platform)
	}
	l := &lease{repo: repo, st: st, api: api}

	if st.Access == nil {
		code := waitReady(ctx, l, deadline, stderr)
		if code >= 0 {
			return code
		}
	}
	steps := []struct {
		name string
		run  func() (int, error)
	}{
		{StepEnv, func() (int, error) { return 0, setLeaseEnv(ctx, st, rc) }},
		{StepSynced, func() (int, error) { _, err := syncFiles(ctx, st, repo, rc, stderr); return 0, err }},
		{StepSetup, func() (int, error) {
			return runSetup(ctx, st, setupCommand(rc, platform, true), deadline, stderr)
		}},
	}
	order := []string{StepCreated, StepEnv, StepSynced, StepSetup, StepDone}
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
	printSummary(st, rc, stdout)
	return 0
}

func warnDifferences(st *State, task, priority string, devices, require []string, fresh bool, stderr io.Writer) {
	var diffs []string
	if task != "" && task != st.Task {
		diffs = append(diffs, "--task")
	}
	if priority != st.Priority {
		diffs = append(diffs, "--priority")
	}
	if len(devices) > 0 && !slices.Equal(devices, st.Devices) {
		diffs = append(diffs, "--device")
	}
	if !slices.Equal(require, st.Require) {
		diffs = append(diffs, "--require")
	}
	if fresh != st.Fresh {
		diffs = append(diffs, "--fresh")
	}
	if len(diffs) > 0 {
		fmt.Fprintf(stderr, "hostel: warning: %s differ from the existing lease %s; it continues unchanged (release it first to change them)\n",
			strings.Join(diffs, ", "), st.LeaseID)
	}
}

func createLease(ctx context.Context, api *client.Client, repo *Repo, rc *config.Repo, st *State, platform, task, priority string,
	devices, require []string, fresh bool) (int, error) {
	pub, err := st.NewKey()
	if err != nil {
		return ExitError, err
	}
	rp := rc.Platforms[platform]
	if rp == nil {
		rp = &config.RepoPlatform{}
	}
	reqDevices := []string(devices)
	if len(reqDevices) == 0 {
		reqDevices = rp.Devices
	}
	var tunnels []protocol.TunnelRequest
	for _, name := range rc.TunnelNames() {
		tunnels = append(tunnels, protocol.TunnelRequest{Name: name, RoomPort: rc.Tunnels[name].RoomPort})
	}
	host, _ := os.Hostname()
	req := protocol.CreateLeaseRequest{Platform: platform, Devices: reqDevices, Task: task, Priority: priority,
		RequiredLabels: append(slices.Clone(rp.Requires), require...), Tunnels: tunnels, Fresh: fresh, PublicKey: pub,
		GuestHost: host, Repo: filepath.Base(repo.Root), Worktree: repo.Root}
	var l protocol.Lease
	if err := api.Do(ctx, "POST", "/v1/leases", req, &l); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity {
			return ExitUnavailable, err
		}
		return ExitError, err
	}
	*st = State{Dir: st.Dir, LeaseID: l.ID, Platform: platform, Task: task, Priority: priority, Devices: devices,
		Require: require, Fresh: fresh, Step: StepCreated}
	return 0, st.Save()
}

// waitReady long-polls the lease until it is ready. It returns -1 when the
// lease is ready, else the exit code of the command.
func waitReady(ctx context.Context, l *lease, deadline time.Time, stderr io.Writer) int {
	var since int64
	last := ""
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return notFinished(stderr)
		}
		w := min(pollWait, remaining)
		var lease protocol.Lease
		path := fmt.Sprintf("/v1/leases/%s?since=%d&wait=%ds", l.st.LeaseID, since, int(w.Seconds()))
		if err := l.api.Do(ctx, "GET", path, nil, &lease); err != nil {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				l.st.Final = true
				l.st.Save()
			}
			return fail(stderr, err)
		}
		since = lease.Revision
		line := describe(lease)
		if line != last {
			fmt.Fprintln(stderr, "hostel: "+line)
			last = line
		}
		switch {
		case lease.State == protocol.LeaseReady:
			l.st.Access = lease.Access
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
		s += fmt.Sprintf(", position %d", l.Position)
		if l.Reason != "" {
			s += " (" + l.Reason + ")"
		}
	}
	if l.Machine != "" && l.State != protocol.LeaseQueued {
		s += " on " + l.Provider + "/" + l.Machine
	}
	return s
}

// leaseEnv computes lease.env: the repository platform env with the tunnel
// ports, and HOSTEL_TUNNEL_<NAME>_PORT for each tunnel.
func leaseEnv(st *State, rc *config.Repo) map[string]string {
	ports := st.Access.TunnelPorts
	env := map[string]string{}
	if rp := rc.Platforms[st.Platform]; rp != nil {
		env = config.ExpandEnv(rp.Env, ports)
	}
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
	return conn.Helper(ctx, []string{"set-env"}, map[string]any{"env": leaseEnv(st, rc)}, nil)
}

// miseFiles are the files that make the setup job run "mise install" again.
var miseFiles = []string{"mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml", ".tool-versions",
	".nvmrc", ".python-version", ".config/mise.toml", ".config/mise/config.toml", "mise/config.toml", ".mise/config.toml"}

// setupCommand returns the setup job: "mise install" (unless mise = false) and,
// with full, the repository setup commands of the platform.
func setupCommand(rc *config.Repo, platform string, full bool) string {
	var parts []string
	if rc.MiseEnabled() {
		parts = append(parts, "mise install")
	}
	if rp := rc.Platforms[platform]; full && rp != nil {
		for _, c := range rp.Setup {
			parts = append(parts, "("+c+")")
		}
	}
	return strings.Join(parts, " && ")
}

// runSetup continues the running setup job, or starts cmd as the setup job
// (if not empty), and streams its output to stderr.
func runSetup(ctx context.Context, st *State, cmd string, deadline time.Time, stderr io.Writer) (int, error) {
	if cmd == "" && st.SetupJob == "" {
		return 0, nil
	}
	if st.SetupJob == "" {
		conn, err := Dial(ctx, st, "cmd")
		if err != nil {
			return 0, err
		}
		var started map[string]string
		err = conn.Helper(ctx, []string{"job", "start"}, protocol.JobRequest{Command: []string{cmd}, Shell: true}, &started)
		conn.Close()
		if err != nil {
			return 0, fmt.Errorf("start the setup job: %w", err)
		}
		st.SetupJob, st.SetupOffset = started["job_id"], 0
		if err := st.Save(); err != nil {
			return 0, err
		}
		fmt.Fprintf(stderr, "hostel: setup job %s: %s\n", st.SetupJob, cmd)
	}
	res, err := attachJob(ctx, st, st.SetupJob, st.SetupOffset, stderr, deadline)
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

func printSummary(st *State, rc *config.Repo, w io.Writer) {
	a := st.Access
	fmt.Fprintf(w, "Lease %s is ready.\n", st.LeaseID)
	fmt.Fprintf(w, "  platform:   %s", st.Platform)
	if len(st.Devices) > 0 {
		fmt.Fprintf(w, " (devices: %s)", strings.Join(st.Devices, ", "))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  room:       %s/%s, slot %s\n", a.RoomOS, a.RoomArch, a.Slot)
	fmt.Fprintf(w, "  work dir:   %s\n", a.WorkDir)
	fmt.Fprintf(w, "  expires:    %s at the latest; without activity at %s (each hostel command extends it)\n",
		a.ExpiresAt.Local().Format(time.DateTime), a.IdleExpiresAt.Local().Format(time.DateTime))
	env := leaseEnv(st, rc)
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
	fmt.Fprintln(w, "Next: hostel run -- <command>   hostel sync   hostel screenshot   hostel release")
}

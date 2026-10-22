package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/room"
)

// Job states that run and attach report.
const (
	JobExited  = "exited"
	JobRunning = "running"
	JobLost    = "lost"
)

type jobResult struct {
	Status   string `json:"status"`
	JobID    string `json:"job_id"`
	ExitCode int    `json:"exit_code"`
	offset   int64
}

// trailerWriter forwards job output and keeps back a possible exit trailer at
// its end. Job output can contain trailer bytes too, so a trailer counts only
// when the helper ends the stream right after it (finish).
type trailerWriter struct {
	out     io.Writer
	pending []byte
	written int64
}

const maxTrailer = 64

func (w *trailerWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	n := len(w.pending)
	if i := bytes.LastIndexByte(w.pending, 0); i >= 0 && maybeTrailer(w.pending[i:]) {
		n = i
	}
	_, err := w.out.Write(w.pending[:n])
	w.written += int64(n)
	w.pending = w.pending[n:]
	return len(p), err
}

// maybeTrailer reports whether b is a trailer or the start of one.
func maybeTrailer(b []byte) bool {
	s := string(b)
	if len(s) > maxTrailer {
		return false
	}
	if strings.HasPrefix(protocol.TrailerLost, s) || strings.HasPrefix(protocol.TrailerExit, s) {
		return true
	}
	rest, ok := strings.CutPrefix(s, protocol.TrailerExit)
	if !ok {
		return false
	}
	rest = strings.TrimPrefix(strings.TrimSuffix(rest, "\n"), "-")
	return strings.Trim(rest, "0123456789") == ""
}

// finish reads the trailer after the helper ended the stream normally.
func (w *trailerWriter) finish() (status string, code int, ok bool) {
	s := string(w.pending)
	if s == protocol.TrailerLost {
		return JobLost, 0, true
	}
	rest, found := strings.CutPrefix(s, protocol.TrailerExit)
	if !found || !strings.HasSuffix(rest, "\n") {
		return "", 0, false
	}
	code, err := strconv.Atoi(strings.TrimSuffix(rest, "\n"))
	return JobExited, code, err == nil
}

// attachJob streams a job's output from offset until the job ends or the
// deadline passes. It connects again when the connection drops.
func attachJob(ctx context.Context, l *lease, jobID string, offset int64, out io.Writer, deadline time.Time) (jobResult, error) {
	res := jobResult{JobID: jobID, Status: JobRunning, offset: offset}
	for time.Now().Before(deadline) {
		actx, cancel := context.WithDeadline(ctx, deadline)
		conn, err := Dial(actx, l.st, "cmd")
		if err != nil {
			cancel()
			// The gateway refuses a lease that ended; then waiting is useless.
			var ended *leaseEndedError
			if terr := l.touch(); errors.As(terr, &ended) {
				return res, ended
			}
			if !sleepUntil(ctx, time.Second, deadline) {
				break
			}
			continue
		}
		w := &trailerWriter{out: out}
		err = conn.Stream(actx, []string{"job", "attach", jobID, "--offset", strconv.FormatInt(res.offset, 10)}, nil, w)
		conn.Close()
		cancel()
		res.offset += w.written
		if err == nil {
			status, code, ok := w.finish()
			if !ok {
				return res, errors.New("the job output ended without an exit status")
			}
			res.Status, res.ExitCode = status, code
			return res, nil
		}
		// A dropped connection or the deadline: the job continues; attach
		// again from the offset, or report it as running.
		if !errors.Is(err, ErrConnLost) && !errors.Is(err, context.DeadlineExceeded) {
			return res, err
		}
	}
	return res, ctx.Err()
}

func sleepUntil(ctx context.Context, d time.Duration, deadline time.Time) bool {
	if time.Now().Add(d).After(deadline) {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// startJob starts a job with an ID that the guest chooses. When the connection
// drops during the start, it connects again and repeats the start with the same
// ID, so the job runs once and the guest knows its ID.
func startJob(ctx context.Context, l *lease, req protocol.JobRequest, deadline time.Time) (string, error) {
	req.JobID = room.NewJobID()
	for {
		conn, err := Dial(ctx, l.st, "cmd")
		if err == nil {
			err = conn.Helper(ctx, []string{"job", "start"}, req, nil)
			conn.Close()
			if err == nil {
				return req.JobID, nil
			}
		}
		var ended *leaseEndedError
		if terr := l.touch(); errors.As(terr, &ended) {
			return "", ended
		}
		if !errors.Is(err, ErrConnLost) {
			return "", err
		}
		if !sleepUntil(ctx, time.Second, deadline) {
			return "", fmt.Errorf("%w; the job %s may run: run \"hostel attach %s\"", err, req.JobID, req.JobID)
		}
	}
}

// reportJob prints the last line of run and attach and returns the exit code
// (design, section 10, "Exit codes and output").
func reportJob(res jobResult, asJSON bool, stdout, stderr io.Writer) int {
	if asJSON {
		json.NewEncoder(stdout).Encode(res)
	}
	switch res.Status {
	case JobExited:
		fmt.Fprintf(stderr, "hostel: job %s exited with code %d\n", res.JobID, res.ExitCode)
		return res.ExitCode
	case JobLost:
		fmt.Fprintf(stderr, "hostel: job %s was lost\n", res.JobID)
		return 1
	}
	fmt.Fprintf(stderr, "hostel: job %s is still running; run \"hostel attach %s\"\n", res.JobID, res.JobID)
	return ExitNotFinished
}

// Run is "hostel run [--detach] [--wait 100s] [--json] -- <command>". One
// argument runs with sh -c; several arguments run without a shell.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel run", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	detach := fs.Bool("detach", false, "start the job and print its ID")
	wait := fs.Duration("wait", 100*time.Second, "time to stream the output before returning")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout (the job output goes to stderr)")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	if len(pos) == 0 {
		fmt.Fprintln(stderr, "hostel: usage: hostel run [--detach] [--wait 100s] -- <command>")
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	deadline := time.Now().Add(*wait)
	ctx := context.Background()
	id, err := startJob(ctx, l, protocol.JobRequest{Command: pos, Shell: len(pos) == 1}, deadline)
	if err != nil {
		return l.fail(stderr, fmt.Errorf("start the job: %w", err))
	}
	if *detach {
		fmt.Fprintln(stdout, id)
		fmt.Fprintf(stderr, "hostel: job %s started; run \"hostel attach %s\" for its output\n", id, id)
		return 0
	}
	out := stdout
	if *asJSON {
		out = stderr
	}
	res, err := attachJob(ctx, l, id, 0, out, deadline)
	if serr := l.saveOffset(id, res); err == nil {
		err = serr
	}
	if err != nil {
		return l.fail(stderr, fmt.Errorf("job %s: %w; run \"hostel attach %s\"", id, err, id))
	}
	return reportJob(res, *asJSON, stdout, stderr)
}

// saveOffset records the printed output of a running job; a finished job
// needs no offset.
func (l *lease) saveOffset(id string, res jobResult) error {
	if res.Status == JobRunning {
		if l.st.JobOffsets == nil {
			l.st.JobOffsets = map[string]int64{}
		}
		l.st.JobOffsets[id] = res.offset
	} else {
		delete(l.st.JobOffsets, id)
	}
	return l.st.Save()
}

// Attach is "hostel attach <job> [--wait 100s] [--json]".
func Attach(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel attach", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	wait := fs.Duration("wait", 100*time.Second, "time to stream the output before returning")
	asJSON := fs.Bool("json", false, "print the result as JSON on stdout (the job output goes to stderr)")
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, "hostel: usage: hostel attach <job> [--wait 100s]")
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	out := stdout
	if *asJSON {
		out = stderr
	}
	res, err := attachJob(context.Background(), l, pos[0], l.st.JobOffsets[pos[0]], out, time.Now().Add(*wait))
	if serr := l.saveOffset(pos[0], res); err == nil {
		err = serr
	}
	if err != nil {
		return l.fail(stderr, fmt.Errorf("job %s: %w", pos[0], err))
	}
	return reportJob(res, *asJSON, stdout, stderr)
}

// Jobs is "hostel jobs".
func Jobs(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel jobs", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	conn, err := Dial(context.Background(), l.st, "cmd")
	if err != nil {
		return l.fail(stderr, err)
	}
	defer conn.Close()
	var list struct {
		Jobs []protocol.JobInfo `json:"jobs"`
	}
	if err := conn.Helper(context.Background(), []string{"job", "list"}, nil, &list); err != nil {
		return l.fail(stderr, err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "JOB\tSTATE\tEXIT\tSTARTED\tCOMMAND")
	for _, j := range list.Jobs {
		exit := "-"
		if j.ExitCode != nil {
			exit = strconv.Itoa(*j.ExitCode)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", j.ID, j.State, exit, j.StartedAt.Local().Format(time.TimeOnly), strings.Join(j.Command, " "))
	}
	tw.Flush()
	return 0
}

// Stop is "hostel stop <job>".
func Stop(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel stop", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, "hostel: usage: hostel stop <job>")
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	conn, err := Dial(context.Background(), l.st, "cmd")
	if err != nil {
		return l.fail(stderr, err)
	}
	defer conn.Close()
	if err := conn.Helper(context.Background(), []string{"job", "stop", pos[0]}, nil, nil); err != nil {
		return l.fail(stderr, err)
	}
	fmt.Fprintf(stdout, "stopped job %s\n", pos[0])
	return 0
}

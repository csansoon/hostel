package room

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

type jobMeta struct {
	Command   []string  `json:"command"`
	Shell     bool      `json:"shell"`
	StartedAt time.Time `json:"started_at"`
}

// supervisorInfo is supervisor.json. Start times guard against reused PIDs.
type supervisorInfo struct {
	SupervisorPID   int    `json:"supervisor_pid"`
	SupervisorStart string `json:"supervisor_start"`
	PGID            int    `json:"pgid"`
	CommandStart    string `json:"command_start"`
	// Group is the name of the command's job object (Windows).
	Group string `json:"group,omitempty"`
}

func (h *helper) jobsDir(slot string) string { return filepath.Join(h.slotDir(slot), "jobs") }

func (h *helper) jobDir(slot, id string) (string, error) {
	if !strings.HasPrefix(id, "j-") || strings.ContainsAny(id, "/\\.") {
		return "", fmt.Errorf("invalid job ID %q", id)
	}
	return filepath.Join(h.jobsDir(slot), id), nil
}

// jobStart starts a supervisor in its own session and returns the job ID when
// the supervisor has started the command.
func (h *helper) jobStart(args []string, stdin io.Reader) (map[string]string, error) {
	slot, _, err := slotFlag("job start", args)
	if err != nil {
		return nil, err
	}
	var req protocol.JobRequest
	if err := json.NewDecoder(stdin).Decode(&req); err != nil {
		return nil, fmt.Errorf("read the request: %w", err)
	}
	if len(req.Command) == 0 || (req.Shell && len(req.Command) != 1) {
		return nil, errors.New("a job needs a command (one string with shell = true)")
	}
	if _, err := h.readLease(slot); err != nil {
		return nil, err
	}
	id := req.JobID
	if id == "" {
		id = NewJobID()
	}
	dir, err := h.jobDir(slot, id)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(h.jobsDir(slot), 0o755); err != nil {
		return nil, err
	}
	// Mkdir is the atomic claim of the ID: a repeated start with the same ID
	// waits for the first start's supervisor and returns the same job.
	if err := os.Mkdir(dir, 0o755); err == nil {
		meta, _ := json.Marshal(jobMeta{Command: req.Command, Shell: req.Shell, StartedAt: time.Now().UTC()})
		if err := writeFileAtomic(filepath.Join(dir, "job.json"), meta, 0o644); err != nil {
			return nil, err
		}
		cmd := exec.Command(h.exe, "room", "job", "supervise", "--slot", slot, id)
		setSession(cmd)
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start the job supervisor: %w", err)
		}
		go cmd.Wait()
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "supervisor.json")); err == nil {
			return map[string]string{"job_id": id}, nil
		}
		// A command that could not start has an exit code and no supervisor.json.
		if _, ok := readExit(dir); ok {
			return map[string]string{"job_id": id}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the supervisor of job %s did not start in 10 seconds", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// jobSupervise runs the command of a job, writes its output to jobs/<id>/output
// and its exit code to jobs/<id>/exit.
func (h *helper) jobSupervise(args []string) error {
	slot, pos, err := slotFlag("job supervise", args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: job supervise --slot <slot> <id>", cli.ErrUsage)
	}
	dir, err := h.jobDir(slot, pos[0])
	if err != nil {
		return err
	}
	var meta jobMeta
	data, err := os.ReadFile(filepath.Join(dir, "job.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	out, err := os.OpenFile(filepath.Join(dir, "output"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	writeExit := func(code int) error {
		return writeFileAtomic(filepath.Join(dir, "exit"), []byte(strconv.Itoa(code)+"\n"), 0o644)
	}
	env, err := h.leaseEnv(slot, out)
	if err != nil {
		fmt.Fprintf(out, "hostel: %v\n", err)
		return writeExit(127)
	}
	h.trackSessions(slot, envMap(env)["HOSTEL_LEASE"], out)
	if marker := devboxTaskMarker(slot, pos[0]); marker != "" {
		if err := os.WriteFile(marker, nil, 0o644); err != nil {
			fmt.Fprintf(out, "hostel: warning: write the devbox task marker: %v; the devbox can stop on idle during this job\n", err)
		}
		defer os.Remove(marker)
	}
	var path string
	var argv []string
	if meta.Shell {
		path, argv = shellCommand(meta.Command[0], env)
	} else {
		argv = meta.Command
		// The command is found in the PATH of the job environment (with the
		// mise tools), not in the helper's PATH.
		if path, err = lookPath(argv[0], env); err != nil {
			fmt.Fprintf(out, "hostel: %s: command not found in the job PATH\n", argv[0])
			return writeExit(127)
		}
	}
	cmd := &exec.Cmd{Path: path, Args: argv, Dir: h.workDir(slot), Env: env, Stdout: out, Stderr: out}
	group := jobGroupName(slot, pos[0])
	if err := startInGroup(cmd, group); err != nil {
		fmt.Fprintf(out, "hostel: start the command: %v\n", err)
		return writeExit(127)
	}
	selfStart, _ := processStart(os.Getpid())
	cmdStart, _ := processStart(cmd.Process.Pid)
	info, _ := json.Marshal(supervisorInfo{SupervisorPID: os.Getpid(), SupervisorStart: selfStart, PGID: cmd.Process.Pid, CommandStart: cmdStart, Group: group})
	if err := writeFileAtomic(filepath.Join(dir, "supervisor.json"), info, 0o644); err != nil {
		return err
	}
	cmd.Wait()
	return writeExit(exitCode(cmd.ProcessState))
}

// devboxTaskMarker is a Namespace task marker for a running job: while it
// exists, the devbox does not stop on idle, so a long job without an SSH
// connection keeps running. Only jobs have one, so a devbox that a dead server
// leaked still stops on idle when its jobs end (design, section 7). "." is not
// in slot names, so the markers of one slot have their own prefix. It is
// empty on other rooms.
func devboxTaskMarker(slot, job string) string {
	dir := os.Getenv("NAMESPACE_DEVBOX_TASKS_DIR")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "hostel-"+slot+"."+job)
}

// lookPath finds a command in the PATH of the job environment (not the
// helper's). On Windows, it also tries the extensions in PATHEXT, and a file
// needs no executable bit.
func lookPath(name string, env []string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		return name, nil
	}
	m := envMap(env)
	exts := []string{""}
	if runtime.GOOS == "windows" {
		pathext := m["PATHEXT"]
		if pathext == "" {
			pathext = ".COM;.EXE;.BAT;.CMD"
		}
		if filepath.Ext(name) == "" {
			exts = nil
		}
		for _, ext := range strings.Split(strings.ToLower(pathext), ";") {
			if ext != "" {
				exts = append(exts, ext)
			}
		}
	}
	for _, dir := range filepath.SplitList(m["PATH"]) {
		for _, ext := range exts {
			p := filepath.Join(dir, name+ext)
			if info, err := os.Stat(p); err == nil && !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				return p, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

func (h *helper) readSupervisor(dir string) (supervisorInfo, error) {
	var s supervisorInfo
	data, err := os.ReadFile(filepath.Join(dir, "supervisor.json"))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(data, &s)
}

// alive reports whether a process is running with the recorded start time.
func alive(pid int, start string) bool {
	got, err := processStart(pid)
	return err == nil && got == start
}

func readExit(dir string) (int, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "exit"))
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	return code, err == nil
}

// jobAttach streams the output from an offset; at the end it writes the exit
// trailer, or the lost trailer if the supervisor died without an exit code.
func (h *helper) jobAttach(args []string, stdout io.Writer) error {
	fs := cli.NewFlagSet("job attach", io.Discard)
	slot := fs.String("slot", "", "slot name")
	offset := fs.Int64("offset", 0, "byte offset")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !config.ValidName(*slot) {
		return fmt.Errorf("%w: job attach --slot <slot> [--offset N] <id>", cli.ErrUsage)
	}
	dir, err := h.jobDir(*slot, pos[0])
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "job.json")); err != nil {
		return fmt.Errorf("no job %s in this lease", pos[0])
	}
	sup, _ := h.readSupervisor(dir)
	buf := make([]byte, 64<<10)
	var f *os.File
	for {
		if f == nil {
			if f, err = os.Open(filepath.Join(dir, "output")); err == nil {
				defer f.Close()
				if _, err := f.Seek(*offset, io.SeekStart); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		// Read the exit state before the output, so no output after the
		// exit is missed.
		code, exited := readExit(dir)
		lost := !exited && sup.SupervisorPID != 0 && !alive(sup.SupervisorPID, sup.SupervisorStart)
		if sup.SupervisorPID == 0 {
			sup, _ = h.readSupervisor(dir)
		}
		n := 0
		if f != nil {
			for {
				m, err := f.Read(buf)
				if m > 0 {
					if _, werr := stdout.Write(buf[:m]); werr != nil {
						return nil
					}
					n += m
				}
				if err != nil {
					break
				}
			}
		}
		if n > 0 {
			continue
		}
		if exited {
			_, err := fmt.Fprintf(stdout, "%s%d\n", protocol.TrailerExit, code)
			return err
		}
		if lost {
			if _, exited := readExit(dir); !exited {
				_, err := io.WriteString(stdout, protocol.TrailerLost)
				return err
			}
			continue
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (h *helper) jobList(args []string) (map[string][]protocol.JobInfo, error) {
	slot, _, err := slotFlag("job list", args)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(h.jobsDir(slot))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	jobs := []protocol.JobInfo{}
	for _, de := range des {
		dir := filepath.Join(h.jobsDir(slot), de.Name())
		var meta jobMeta
		data, err := os.ReadFile(filepath.Join(dir, "job.json"))
		if err != nil || json.Unmarshal(data, &meta) != nil {
			continue
		}
		j := protocol.JobInfo{ID: de.Name(), Command: meta.Command, StartedAt: meta.StartedAt, State: "running"}
		if code, ok := readExit(dir); ok {
			j.State, j.ExitCode = "exited", &code
		} else if sup, err := h.readSupervisor(dir); err != nil || !alive(sup.SupervisorPID, sup.SupervisorStart) {
			j.State = "lost"
		}
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return map[string][]protocol.JobInfo{"jobs": jobs}, nil
}

// jobStop sends SIGTERM to the job's process group, and SIGKILL after 10 seconds.
func (h *helper) jobStop(args []string) (struct{}, error) {
	slot, pos, err := slotFlag("job stop", args)
	if err != nil {
		return struct{}{}, err
	}
	if len(pos) != 1 {
		return struct{}{}, fmt.Errorf("%w: job stop --slot <slot> <id>", cli.ErrUsage)
	}
	dir, err := h.jobDir(slot, pos[0])
	if err != nil {
		return struct{}{}, err
	}
	sup, err := h.readSupervisor(dir)
	if err != nil {
		return struct{}{}, fmt.Errorf("no running job %s", pos[0])
	}
	if !alive(sup.PGID, sup.CommandStart) {
		return struct{}{}, nil
	}
	stopGroup(sup.PGID, sup.Group, false)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := readExit(dir); ok {
			return struct{}{}, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	stopGroup(sup.PGID, sup.Group, true)
	return struct{}{}, nil
}

// NewJobID returns a job ID: j-<unix ms>-<4 hex digits>.
func NewJobID() string {
	return fmt.Sprintf("j-%d-%s", time.Now().UnixMilli(), randHex()[:4])
}

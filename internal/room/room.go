// Package room is the helper: the "hostel room" commands that run on rooms
// (design, section 8).
package room

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/protocol"
)

// Main runs a helper command. Control commands write one JSON object to stdout;
// on failure it is {"error": "..."} and the exit code is 1.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "hostel room: missing command")
		return 2
	}
	// A service that the owner installs; it needs no room layout.
	if args[0] == "connect" {
		return Connect(args[1:], stderr)
	}
	h, err := newHelper()
	if err != nil {
		return fail(stdout, err)
	}
	var out any
	switch args[0] {
	case "info":
		out, err = h.info()
	case "install":
		out, err = h.install(args[1:], stdin)
	case "run-script":
		out, err = h.runScript(args[1:], stdin)
	case "mise-install":
		out, err = h.miseInstall(stdin)
	case "begin":
		out, err = h.begin(args[1:], stdin)
	case "cleanup":
		out, err = h.cleanup(args[1:], stdin)
	case "set-env":
		out, err = h.setEnv(args[1:], stdin)
	case "sync-plan":
		out, err = h.syncPlan(args[1:], stdin)
	case "sync-apply":
		out, err = h.syncApply(args[1:], stdin)
	case "tar-in":
		out, err = h.tarIn(args[1:], stdin)
	case "job":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "hostel room: job start|attach|list|stop|supervise")
			return 2
		}
		switch args[1] {
		case "start":
			out, err = h.jobStart(args[2:], stdin)
		case "list":
			out, err = h.jobList(args[2:])
		case "stop":
			out, err = h.jobStop(args[2:])
		case "attach":
			return streamResult(h.jobAttach(args[2:], stdout), stderr)
		case "supervise":
			return streamResult(h.jobSupervise(args[2:]), stderr)
		default:
			fmt.Fprintf(stderr, "hostel room: unknown job command %q\n", args[1])
			return 2
		}
	// Streaming commands: their output is not JSON; errors go to stderr.
	case "diff":
		return streamResult(h.diff(args[1:], stdout), stderr)
	case "tar-out":
		return streamResult(h.tarOut(args[1:], stdout), stderr)
	case "exec":
		return streamResult(h.execCommand(args[1:], stderr), stderr)
	case "shell":
		return streamResult(h.shell(args[1:], stderr), stderr)
	default:
		fmt.Fprintf(stderr, "hostel room: unknown command %q\n", args[0])
		return 2
	}
	if errors.Is(err, cli.ErrUsage) {
		fmt.Fprintln(stderr, "hostel room:", err)
		return 2
	}
	if err != nil {
		return fail(stdout, err)
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return 1
	}
	return 0
}

// streamResult reports the error of a streaming command on stderr.
func streamResult(err error, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, cli.ErrUsage):
		fmt.Fprintln(stderr, "hostel room:", err)
		return 2
	}
	fmt.Fprintln(stderr, "hostel:", err)
	return 1
}

func fail(stdout io.Writer, err error) int {
	json.NewEncoder(stdout).Encode(protocol.HelperError{Error: err.Error()})
	return 1
}

// helper knows the room layout. root is the "hostel" directory in the room
// user's home directory; the helper binary is root/bin/hostel.
type helper struct {
	root string
	exe  string
	os   string
}

func newHelper() (*helper, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find the helper path: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, fmt.Errorf("find the helper path: %w", err)
	}
	bin := filepath.Dir(exe)
	if filepath.Base(bin) != "bin" {
		return nil, fmt.Errorf("the helper must be at hostel/bin/, not %s", exe)
	}
	h := &helper{root: filepath.Dir(bin), exe: exe, os: OSName()}
	// The machine's configured OS selects the platform scripts.
	if re, err := h.readRoomEnv(); err == nil && re.OS != "" {
		h.os = re.OS
	}
	return h, nil
}

// OSName returns the Hostel OS name of this machine.
func OSName() string {
	if runtime.GOOS == "darwin" {
		return "macos"
	}
	return runtime.GOOS
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func (h *helper) misePath() string               { return filepath.Join(h.root, "bin", "mise"+exeSuffix()) }
func (h *helper) miseConfigPath() string         { return filepath.Join(h.root, "mise", "config.toml") }
func (h *helper) roomEnvPath() string            { return filepath.Join(h.root, "room-env.json") }
func (h *helper) platformsDir() string           { return filepath.Join(h.root, "platforms") }
func (h *helper) platformDir(name string) string { return filepath.Join(h.platformsDir(), name) }
func (h *helper) slotDir(slot string) string     { return filepath.Join(h.root, "slots", slot) }

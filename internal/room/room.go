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

func fail(stdout io.Writer, err error) int {
	json.NewEncoder(stdout).Encode(protocol.HelperError{Error: err.Error()})
	return 1
}

var errWindows = errors.New("Windows rooms are not supported yet (milestone M6)")

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
	return &helper{root: filepath.Dir(bin), exe: exe, os: OSName()}, nil
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

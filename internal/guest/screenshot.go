package guest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// screenshotTimeout bounds the screenshot script, so the command returns
// before a typical agent tool timeout.
const screenshotTimeout = 90 * time.Second

// Screenshot is "hostel screenshot [--device d] [-o file.png]".
func Screenshot(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel screenshot", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	device := fs.String("device", "", "device of the lease (default: the first)")
	out := fs.String("o", "", "output file (default: screenshot-<platform>-<device>-<time>.png)")
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	devices := l.leaseDevices()
	if *device != "" && !slices.Contains(devices, *device) {
		return l.fail(stderr, fmt.Errorf("device %q is not in this lease (devices: %s)", *device, strings.Join(devices, ", ")))
	}
	name := *device
	if name == "" && len(devices) > 0 {
		name = devices[0]
	}
	if *out == "" {
		label := l.st.Platform
		if name != "" {
			label += "-" + name
		}
		*out = fmt.Sprintf("screenshot-%s-%s.png", label, time.Now().Format("20060102-150405"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), screenshotTimeout+15*time.Second)
	defer cancel()
	conn, err := Dial(ctx, l.st, "cmd")
	if err != nil {
		return l.fail(stderr, err)
	}
	defer conn.Close()
	remote := path.Join(path.Dir(l.st.Access.WorkDir), fmt.Sprintf("screenshot-%d.png", time.Now().UnixNano()))
	scriptArgs := []string{remote}
	if *device != "" {
		scriptArgs = append(scriptArgs, *device)
	}
	var resp protocol.RunScriptResponse
	err = conn.Helper(ctx, []string{"run-script", l.st.Platform, config.StepScreenshot},
		protocol.RunScriptRequest{Args: scriptArgs, TimeoutSeconds: int(screenshotTimeout.Seconds())}, &resp)
	if err != nil {
		return l.fail(stderr, err)
	}
	switch {
	case resp.Missing:
		return l.fail(stderr, fmt.Errorf("platform %s has no screenshot script for %s", l.st.Platform, l.st.Access.RoomOS))
	case resp.TimedOut || resp.ExitCode != 0:
		return l.fail(stderr, fmt.Errorf("the screenshot script failed (exit code %d): %s", resp.ExitCode, strings.TrimSpace(resp.OutputTail)))
	}
	var png, errb bytes.Buffer
	code, err = conn.Run(ctx, "cat "+quote(remote)+" && rm -f "+quote(remote), nil, &png, &errb)
	if err != nil {
		return l.fail(stderr, err)
	}
	if code != 0 {
		return l.fail(stderr, fmt.Errorf("the screenshot script wrote no file: %s", strings.TrimSpace(errb.String())))
	}
	if err := os.WriteFile(*out, png.Bytes(), 0o644); err != nil {
		return l.fail(stderr, err)
	}
	fmt.Fprintln(stdout, *out)
	return 0
}

// leaseDevices reads the lease's devices from the server.
func (l *lease) leaseDevices() []string {
	var pl protocol.Lease
	if err := l.api.Do(context.Background(), "GET", "/v1/leases/"+l.st.LeaseID, nil, &pl); err != nil {
		return nil
	}
	return pl.Devices
}

package guest

import (
	"bytes"
	"context"
	"errors"
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

// Screenshot is "hostel screenshot [--tool <tool[:option]>] [-o file.png]".
func Screenshot(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel screenshot", stderr)
	toolFlag := fs.String("tool", "", "screen tool of the lease, with an option: <tool[:option]> (needed only when the lease has several screens)")
	out := fs.String("o", "", "output file (default: screenshot-<tool>-<option>-<time>.png)")
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	l, code := openLease(stderr)
	if l == nil {
		return code
	}
	tool, option, err := screenOf(l.st.Started, *toolFlag)
	if err != nil {
		return l.fail(stderr, err)
	}
	if *out == "" {
		label := tool
		if option != "" {
			label += "-" + option
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
	file := fmt.Sprintf("screenshot-%d.png", time.Now().UnixNano())
	remote := path.Join(path.Dir(l.st.Access.WorkDir), file)
	// The room reads the file: sh, or PowerShell with a binary copy
	// (Get-Content would change the bytes).
	read := "cat " + quote(remote) + " && rm -f " + quote(remote)
	if l.st.Access.RoomOS == config.OSWindows {
		dir := l.st.Access.WorkDir[:max(strings.LastIndex(l.st.Access.WorkDir, `\`), 0)]
		remote = dir + `\` + file
		p := "'" + strings.ReplaceAll(remote, "'", "''") + "'"
		read = "$f = [IO.File]::OpenRead(" + p + "); $o = [Console]::OpenStandardOutput(); $f.CopyTo($o); $o.Flush(); $f.Close(); Remove-Item -LiteralPath " + p
	}
	scriptArgs := []string{remote}
	if option != "" {
		scriptArgs = append(scriptArgs, option)
	}
	var resp protocol.RunScriptResponse
	err = conn.Helper(ctx, []string{"run-script", tool, config.StepScreenshot},
		protocol.RunScriptRequest{Args: scriptArgs, TimeoutSeconds: int(screenshotTimeout.Seconds())}, &resp)
	if err != nil {
		return l.fail(stderr, err)
	}
	switch {
	case resp.Missing:
		return l.fail(stderr, fmt.Errorf("the tool %s has no screenshot script for %s", tool, l.st.Access.RoomOS))
	case resp.TimedOut || resp.ExitCode != 0:
		return l.fail(stderr, fmt.Errorf("the screenshot script failed (exit code %d): %s", resp.ExitCode, strings.TrimSpace(resp.OutputTail)))
	}
	var png, errb bytes.Buffer
	code, err = conn.Run(ctx, read, nil, &png, &errb)
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

// screenOf selects the screen of a screenshot: the --tool value, or the only
// screen of the lease.
func screenOf(started []protocol.StartedTool, flag string) (string, string, error) {
	var names []string
	for _, st := range started {
		for _, o := range st.Options {
			names = append(names, st.Name+":"+o)
		}
		if len(st.Options) == 0 {
			names = append(names, st.Name)
		}
	}
	if len(started) == 0 {
		return "", "", errors.New("the lease has no screen tool; acquire one with a screen tool (\"hostel tools\" lists them) for screenshots")
	}
	if flag == "" {
		if len(names) > 1 {
			return "", "", fmt.Errorf("the lease has several screens; select one with --tool (%s)", strings.Join(names, ", "))
		}
		return started[0].Name, strings.Join(started[0].Options, ""), nil
	}
	e, err := config.ParseToolEntry(flag)
	if err != nil {
		return "", "", err
	}
	i := slices.IndexFunc(started, func(st protocol.StartedTool) bool { return st.Name == e.Name })
	switch {
	case i < 0:
		return "", "", fmt.Errorf("the lease has no screen tool %s; screens: %s", e.Name, strings.Join(names, ", "))
	case e.Option != "" && !slices.Contains(started[i].Options, e.Option):
		return "", "", fmt.Errorf("%s is not a screen of the lease; screens: %s", flag, strings.Join(names, ", "))
	case e.Option == "" && len(started[i].Options) > 1:
		return "", "", fmt.Errorf("%s has several screens in the lease; select one with --tool (%s)", e.Name, strings.Join(names, ", "))
	case e.Option == "":
		return e.Name, strings.Join(started[i].Options, ""), nil
	}
	return e.Name, e.Option, nil
}

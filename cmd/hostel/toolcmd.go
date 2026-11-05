package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

const toolsUsage = `hostel: usage:
  hostel config tools                           the defined tools, their machines, and where they come from
  hostel config tools new <name> <dir> [--os linux|macos|windows] [--screen]
                                                a folder with a template for a new tool
  hostel config tools pull <name> <dir>         the tool's files into a new folder
  hostel config tools push <dir> [--name n]     check and save the folder as the tool
  hostel config tools reset <name>              a default tool back to the version of this release
  hostel config tools remove <name>             remove a tool (no machine may list it)`

func toolsCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fs := cli.NewFlagSet("hostel config tools "+sub, stderr)
	osName := fs.String("os", "linux", "with new: the OS of the scripts")
	screen := fs.Bool("screen", false, "with new: a screen tool (start, screenshot, and stop for each lease)")
	name := fs.String("name", "", "with push: the tool name (default: the folder name)")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return 2
	}
	switch {
	case sub == "" && len(pos) == 0:
		return toolsList(stdout, stderr)
	case sub == "new" && len(pos) == 2:
		return toolNew(pos[0], pos[1], *osName, *screen, stdout, stderr)
	case sub == "pull" && len(pos) == 2:
		return toolPull(pos[0], pos[1], stdout, stderr)
	case sub == "push" && len(pos) == 1:
		n := *name
		if n == "" {
			abs, _ := filepath.Abs(pos[0])
			n = filepath.Base(abs)
		}
		return toolPush(n, pos[0], stdout, stderr)
	case sub == "reset" && len(pos) == 1:
		return toolRequest("POST", pos[0], "/reset", nil, fmt.Sprintf("%s is the default tool of this release again.", pos[0]), stdout, stderr)
	case sub == "remove" && len(pos) == 1:
		return toolRequest("DELETE", pos[0], "", nil, fmt.Sprintf("Removed the tool %s.", pos[0]), stdout, stderr)
	}
	fmt.Fprintln(stderr, toolsUsage)
	return 2
}

func toolsList(stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var list []protocol.ConfigTool
	if err := c.Do(ctx, "GET", "/v1/config/tools", nil, &list); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSCREEN\tMACHINES\tSOURCE")
	for _, t := range list {
		machines := strings.Join(t.Machines, ", ")
		if len(t.Machines) > 2 {
			machines = fmt.Sprintf("%d machines", len(t.Machines))
		}
		if machines == "" {
			machines = "-"
		}
		screen := "-"
		if t.Screen {
			screen = "yes"
		}
		source := t.Source
		if t.Source != "default" && t.Revision > 0 {
			source = fmt.Sprintf("%s (rev %d, %s, %s)", t.Source, t.Revision, t.User, t.ChangedAt.Local().Format(time.DateOnly))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.Name, screen, machines, source)
	}
	tw.Flush()
	fmt.Fprintln(stdout, "Any mise tool (for example node:22 or jdk:21) needs no definition: list it on a machine.")
	return 0
}

// toolTemplates are the scripts of a new tool, for each step (docs/design.md,
// section 8, "Tool directories"). start, screenshot, and stop are for screen
// tools only.
var toolTemplates = []struct {
	step, what string
	screen     bool
}{
	{"install", "once for each room, and again when the files change: install what the tool needs", false},
	{"check", "after install and cleanup, and at repair: exit with an error if the tool does not work; print the version (or the options of a screen tool) on the last line", false},
	{"start", "each lease that asks for the tool, before it is ready: start the screen and write its facts to tool.env", true},
	{"screenshot", "hostel screenshot: write a PNG to $1", true},
	{"stop", "at cleanup: stop and delete what start started for this slot", true},
}

func toolNew(name, dir, osName string, screen bool, stdout, stderr io.Writer) int {
	if !config.ValidName(name) {
		fmt.Fprintf(stderr, "hostel: %q is not a valid tool name: use lowercase letters, digits, and -, starting with a letter\n", name)
		return 2
	}
	if !config.IsOSName(osName) {
		fmt.Fprintf(stderr, "hostel: --os must be linux, macos, or windows\n")
		return 2
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	toml := fmt.Sprintf(`description = "%s"
# needs = ["other-tool"]          # tools that this tool uses; a machine that lists it gets them too
# env = { MY_HOME = "{cache}/%s" }   # in every lease on a machine with this tool
# path = ["{cache}/%s/bin"]
# script_timeouts = { install = "20m" }
`, name, name, name)
	if screen {
		toml = fmt.Sprintf(`description = "%s"
screen = true
options = ["%s-1"]               # the options that a lease can ask for; the first is the default
# max_options = 1
# whole_machine = false          # true: the lease takes the whole room
# needs = ["other-tool"]
# script_timeouts = { start = "5m" }
`, name, name)
	}
	files := map[string]string{config.ToolFile: toml,
		config.GuideFile: fmt.Sprintf("%s: what it gives, its environment variables, and how to use it.\n", name)}
	ext := config.ScriptExt(osName)
	for _, t := range toolTemplates {
		if t.screen && !screen {
			continue
		}
		body := fmt.Sprintf("#!/bin/sh\n# %s: %s.\n# Variables: HOSTEL_TOOL, HOSTEL_TOOL_OPTIONS, HOSTEL_SLOT_DIR, HOSTEL_CACHE_DIR\n# (docs/design.md, section 8). It must be safe to run again.\nset -eu\n", t.step, t.what)
		if ext == ".ps1" {
			body = fmt.Sprintf("# %s: %s.\n# Variables: $env:HOSTEL_TOOL, $env:HOSTEL_TOOL_OPTIONS, $env:HOSTEL_SLOT_DIR\n# (docs/design.md, section 8). It must be safe to run again.\n$ErrorActionPreference = 'Stop'\n", t.step, t.what)
		}
		switch t.step {
		case "check":
			if ext == ".ps1" {
				body += "Write-Output '1.0'\n"
			} else {
				body += "echo 1.0\n"
			}
		case "start":
			if ext == ".ps1" {
				body += "Set-Content -Path tool.env -Value \"SCREEN=$($env:HOSTEL_TOOL_OPTIONS)\"\n"
			} else {
				body += "echo \"SCREEN=$HOSTEL_TOOL_OPTIONS\" > tool.env\n"
			}
		}
		files[t.step+"."+osName+ext] = body
	}
	for f, body := range files {
		mode := os.FileMode(0o644)
		if ext := filepath.Ext(f); ext == ".sh" || ext == ".ps1" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), mode); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "Wrote the tool %s to %s. Change the files, then run:\n  hostel config tools push %s\n", name, dir, dir)
	fmt.Fprintf(stdout, "Then add %q to the tools of a machine (hostel config rooms set <machine> tools=...).\n", name)
	return 0
}

func toolPull(name, dir string, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var p protocol.ToolFiles
	if err := c.Do(ctx, "GET", "/v1/config/tools/"+url.PathEscape(name), nil, &p); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		fmt.Fprintln(stderr, "hostel:", err, "(pull writes a new folder)")
		return 1
	}
	names := make([]string, 0, len(p.Files))
	for f := range p.Files {
		if !config.IsToolFile(f) {
			fmt.Fprintf(stderr, "hostel: the server sent the file %q, which is not a tool file\n", f)
			return 1
		}
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		mode := os.FileMode(0o644)
		if ext := filepath.Ext(f); ext == ".sh" || ext == ".ps1" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte(p.Files[f]), mode); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "Wrote %d files of the tool %s to %s.\n", len(names), name, dir)
	return 0
}

func toolPush(name, dir string, stdout, stderr io.Writer) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	files := map[string]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !config.IsToolFile(e.Name()) {
			fmt.Fprintf(stdout, "Not sent: %s (not a tool file: a tool has tool.toml, GUIDE.md, and scripts)\n", e.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		files[e.Name()] = string(data)
	}
	return toolRequest("PUT", name, "", protocol.ToolFiles{Name: name, Files: files},
		fmt.Sprintf("Saved the tool %s. Rooms install it again when they are free.", name), stdout, stderr)
}

func toolRequest(method, name, suffix string, body any, done string, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := c.Do(ctx, method, "/v1/config/tools/"+url.PathEscape(name)+suffix, body, nil); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	fmt.Fprintln(stdout, done)
	return 0
}

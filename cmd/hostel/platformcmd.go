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

const platformsUsage = `hostel: usage:
  hostel config platforms                       the platforms, their machines, and where they come from
  hostel config platforms new <name> <dir> [--os linux|macos|windows]
                                                a folder with a template for a new platform
  hostel config platforms pull <name> <dir>     the platform's files into a new folder
  hostel config platforms push <dir> [--name n] check and save the folder as the platform
  hostel config platforms reset <name>          a default platform back to the version of this release
  hostel config platforms remove <name>         remove a platform (no machine may list it)`

func platformsCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fs := cli.NewFlagSet("hostel config platforms "+sub, stderr)
	osName := fs.String("os", "linux", "with new: the OS of the scripts")
	name := fs.String("name", "", "with push: the platform name (default: the folder name)")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return 2
	}
	switch {
	case sub == "" && len(pos) == 0:
		return platformsList(stdout, stderr)
	case sub == "new" && len(pos) == 2:
		return platformNew(pos[0], pos[1], *osName, stdout, stderr)
	case sub == "pull" && len(pos) == 2:
		return platformPull(pos[0], pos[1], stdout, stderr)
	case sub == "push" && len(pos) == 1:
		n := *name
		if n == "" {
			abs, _ := filepath.Abs(pos[0])
			n = filepath.Base(abs)
		}
		return platformPush(n, pos[0], stdout, stderr)
	case sub == "reset" && len(pos) == 1:
		return platformRequest("POST", pos[0], "/reset", nil, fmt.Sprintf("%s is the default platform of this release again.", pos[0]), stdout, stderr)
	case sub == "remove" && len(pos) == 1:
		return platformRequest("DELETE", pos[0], "", nil, fmt.Sprintf("Removed the platform %s.", pos[0]), stdout, stderr)
	}
	fmt.Fprintln(stderr, platformsUsage)
	return 2
}

func platformsList(stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var list []protocol.ConfigPlatform
	if err := c.Do(ctx, "GET", "/v1/config/platforms", nil, &list); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tMACHINES\tSOURCE")
	for _, p := range list {
		machines := strings.Join(p.Machines, ", ")
		if len(p.Machines) > 2 {
			machines = fmt.Sprintf("%d machines", len(p.Machines))
		}
		if machines == "" {
			machines = "-"
		}
		source := p.Source
		if p.Source != "default" && p.Revision > 0 {
			source = fmt.Sprintf("%s (rev %d, %s, %s)", p.Source, p.Revision, p.User, p.ChangedAt.Local().Format(time.DateOnly))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, p.Kind, machines, source)
	}
	tw.Flush()
	return 0
}

// platformTemplates are the scripts of a new platform, for each step
// (docs/design.md, section 8, "Platform directories").
var platformTemplates = []struct{ step, what string }{
	{"provision", "once for each room, and again when the scripts change: install and check what the platform needs"},
	{"prepare", "each lease, before it is ready: start the lease's devices and write their facts to device.env"},
	{"reset", "at cleanup: stop and delete what prepare started for this slot"},
	{"health", "after cleanup and at repair: exit with an error if the room cannot serve the platform"},
}

func platformNew(name, dir, osName string, stdout, stderr io.Writer) int {
	if !config.ValidName(name) {
		fmt.Fprintf(stderr, "hostel: %q is not a valid platform name: use lowercase letters, digits, and -, starting with a letter\n", name)
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
	files := map[string]string{"platform.toml": fmt.Sprintf(`description = "%s"
devices = ["%s-1"]          # the devices that a lease can ask for; the first is the default
# max_devices = 1
# requires = ["label"]     # labels that each machine must have
# exclusive = false        # true: the lease takes the whole room
# script_timeouts = { provision = "20m" }
`, name, name)}
	ext := config.ScriptExt(osName)
	for _, t := range platformTemplates {
		body := fmt.Sprintf("#!/bin/sh\n# %s: %s.\n# Variables: HOSTEL_PLATFORM, HOSTEL_DEVICES, HOSTEL_SLOT_DIR, HOSTEL_CACHE_DIR\n# (docs/design.md, section 8). It must be safe to run again.\nset -eu\n", t.step, t.what)
		if ext == ".ps1" {
			body = fmt.Sprintf("# %s: %s.\n# Variables: $env:HOSTEL_PLATFORM, $env:HOSTEL_DEVICES, $env:HOSTEL_SLOT_DIR\n# (docs/design.md, section 8). It must be safe to run again.\n$ErrorActionPreference = 'Stop'\n", t.step, t.what)
		}
		if t.step == "prepare" {
			if ext == ".ps1" {
				body += "Set-Content -Path device.env -Value \"DEVICE=$($env:HOSTEL_DEVICES)\"\n"
			} else {
				body += "echo \"DEVICE=$HOSTEL_DEVICES\" > device.env\n"
			}
		}
		files[t.step+"."+osName+ext] = body
	}
	for f, body := range files {
		mode := os.FileMode(0o644)
		if filepath.Ext(f) != ".toml" {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), mode); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "Wrote the platform %s to %s. Change the files, then run:\n  hostel config platforms push %s\n", name, dir, dir)
	fmt.Fprintf(stdout, "Then add %q to the platforms of a machine (hostel config edit).\n", name)
	return 0
}

func platformPull(name, dir string, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var p protocol.PlatformFiles
	if err := c.Do(ctx, "GET", "/v1/config/platforms/"+url.PathEscape(name), nil, &p); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		fmt.Fprintln(stderr, "hostel:", err, "(pull writes a new folder)")
		return 1
	}
	names := make([]string, 0, len(p.Files))
	for f := range p.Files {
		if !config.IsPlatformFile(f) {
			fmt.Fprintf(stderr, "hostel: the server sent the file %q, which is not a platform file\n", f)
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
	fmt.Fprintf(stdout, "Wrote %d files of the platform %s to %s.\n", len(names), name, dir)
	return 0
}

func platformPush(name, dir string, stdout, stderr io.Writer) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	files := map[string]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !config.IsPlatformFile(e.Name()) {
			fmt.Fprintf(stdout, "Not sent: %s (not a platform file: a platform has regular files only)\n", e.Name())
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		files[e.Name()] = string(data)
	}
	return platformRequest("PUT", name, "", protocol.PlatformFiles{Name: name, Files: files},
		fmt.Sprintf("Saved the platform %s. Rooms provision it again when they are free.", name), stdout, stderr)
}

func platformRequest(method, name, suffix string, body any, done string, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := c.Do(ctx, method, "/v1/config/platforms/"+url.PathEscape(name)+suffix, body, nil); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	fmt.Fprintln(stdout, done)
	return 0
}

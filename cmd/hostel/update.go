package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/guest"
	"github.com/csansoon/hostel/internal/update"
	"github.com/csansoon/hostel/internal/version"
)

const updateUsage = `Usage: hostel update [--check] [--version vX.Y.Z]

Replaces this hostel binary with the latest release (or --version), after a
check of the release signature and checksum. --check only prints whether a
newer release exists. Exit codes: 0 done or up to date, 1 error, 69 the
release source cannot be reached.`

func updateCmd(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel update", stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, updateUsage) }
	check := fs.Bool("check", false, "only print whether a newer release exists")
	want := fs.String("version", "", "the release to install (default: the latest)")
	if rest, err := cli.Parse(fs, args); err != nil || len(rest) > 0 {
		fmt.Fprintln(stderr, updateUsage)
		return 2
	}
	ctx := context.Background()
	res, err := selfUpdate(ctx, *want, *check)
	if err != nil {
		fmt.Fprintln(stderr, "hostel update:", err)
		if errors.Is(err, update.ErrUnreachable) {
			return guest.ExitUnavailable
		}
		return 1
	}
	fmt.Fprintln(stdout, res)
	return 0
}

// selfUpdate checks for, or installs, a release for this binary; it returns
// the result in one line. The app uses it too.
func selfUpdate(ctx context.Context, want string, check bool) (string, error) {
	src, err := update.Default()
	if err != nil {
		return "", err
	}
	have := version.Version
	tag := want
	if tag == "" {
		if tag, err = src.Latest(ctx); err != nil {
			return "", err
		}
	}
	_, known := update.Parse(have)
	newer := update.Newer(tag, have)
	switch {
	case check && newer:
		return fmt.Sprintf("Hostel %s is available (this computer has %s); run \"hostel update\"", tag, have), nil
	case check && !known:
		return fmt.Sprintf("this computer has the build %s; the latest release is %s (\"hostel update\" installs it)", have, tag), nil
	case known && !newer && want != "":
		return "", fmt.Errorf("this computer has %s; %s is not newer (to install an older release, use the install command with --version)", have, tag)
	case known && !newer || check:
		return fmt.Sprintf("Hostel %s is up to date (the latest release is %s)", have, tag), nil
	}
	r, err := src.Release(ctx, tag)
	if err != nil {
		return "", err
	}
	exe, err := update.Executable()
	if err != nil {
		return "", err
	}
	if err := update.InstallBinary(ctx, src, r, exe); err != nil {
		if update.CheckWritable(filepath.Dir(exe)) != nil {
			hint := "sudo hostel update"
			if runtime.GOOS == "windows" {
				hint = "the install command (irm …/install.ps1 | iex) in a PowerShell as an administrator"
			}
			return "", fmt.Errorf("%w; this user cannot write %s: run %s", err, filepath.Dir(exe), hint)
		}
		return "", err
	}
	return fmt.Sprintf("updated %s from %s to %s", exe, have, tag), nil
}

// noticeFile keeps the time of the last update notice.
func noticeFile() string {
	p, err := config.GuestPath()
	if err != nil {
		return filepath.Join(os.TempDir(), "hostel-update-notice")
	}
	return filepath.Join(filepath.Dir(p), "update-notice")
}

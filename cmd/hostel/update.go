package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	res, err := update.Self(ctx, version.Version, *want, *check)
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

// noticeFile keeps the time of the last update notice.
func noticeFile() string {
	p, err := config.GuestPath()
	if err != nil {
		return filepath.Join(os.TempDir(), "hostel-update-notice")
	}
	return filepath.Join(filepath.Dir(p), "update-notice")
}

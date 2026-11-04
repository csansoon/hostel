package update

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
)

// Self checks for, or installs, a release (want, or the latest) in place of
// this binary, whose version is have; it returns the result in one line
// (hostel update, and the app's Update).
func Self(ctx context.Context, have, want string, check bool) (string, error) {
	src, err := Default()
	if err != nil {
		return "", err
	}
	tag := want
	if tag == "" {
		if tag, err = src.Latest(ctx); err != nil {
			return "", err
		}
	}
	_, known := Parse(have)
	newer := Newer(tag, have)
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
	exe, err := Executable()
	if err != nil {
		return "", err
	}
	if err := InstallBinary(ctx, src, r, exe); err != nil {
		if CheckWritable(filepath.Dir(exe)) != nil {
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

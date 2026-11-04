package update

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// BinaryName is the release file of the hostel binary for an OS and an
// architecture (Go names).
func BinaryName(goos, goarch string) string {
	if goos == "windows" {
		return "hostel-windows-" + goarch + ".exe"
	}
	return "hostel-" + goos + "-" + goarch
}

// Executable is the running binary (symbolic links resolved).
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// CheckWritable reports a directory in which the user cannot create a file
// (a rename into it then fails).
func CheckWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".hostel-write-check-")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// InstallBinary downloads the hostel binary of r for this OS and architecture,
// checks that it runs and prints the release version, and puts it in place of
// target.
func InstallBinary(ctx context.Context, s *Source, r *Release, target string) error {
	if err := CheckWritable(filepath.Dir(target)); err != nil {
		return fmt.Errorf("cannot write %s: %w", filepath.Dir(target), err)
	}
	tmp, err := s.Download(ctx, r, BinaryName(runtime.GOOS, runtime.GOARCH), target)
	if err != nil {
		return err
	}
	if err := checkRuns(ctx, tmp, r.Tag); err != nil {
		os.Remove(tmp)
		return err
	}
	return Replace(tmp, target)
}

// checkRuns runs bin --version and checks that it prints the version.
func checkRuns(ctx context.Context, bin, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("the new binary does not run: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hostel "+tag {
		return fmt.Errorf("the new binary prints %q, not %q", got, "hostel "+tag)
	}
	return nil
}

// Replace renames src over dst; src is removed on an error. On Windows a
// running binary cannot be replaced, only renamed: dst moves aside first to a
// new name (an older one can still be in use), and CleanOld removes it later.
func Replace(src, dst string) error {
	if runtime.GOOS == "windows" {
		old := fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano())
		if err := os.Rename(dst, old); err != nil && !os.IsNotExist(err) {
			os.Remove(src)
			return fmt.Errorf("move %s aside: %w", dst, err)
		}
		if err := os.Rename(src, dst); err != nil {
			os.Rename(old, dst)
			os.Remove(src)
			return fmt.Errorf("put the new binary at %s: %w", dst, err)
		}
		return nil
	}
	if err := os.Rename(src, dst); err != nil {
		os.Remove(src)
		return fmt.Errorf("put the new binary at %s: %w", dst, err)
	}
	return nil
}

// CleanOld removes the binaries that updates on Windows moved aside (best
// effort: a removal fails while an old binary still runs).
func CleanOld() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := Executable(); err == nil {
		olds, _ := filepath.Glob(exe + ".old-*")
		for _, o := range olds {
			os.Remove(o)
		}
	}
}

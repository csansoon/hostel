package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

// helperRE: the release files that go into helper_dir (design, section 8).
var helperRE = regexp.MustCompile(`^(hostel|mise)-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$`)

// Server updates a server to release r: first each file of helperDir that
// differs from the release (rooms get the new helper before their next
// lease), then the server binary exe. The platform defaults are in the
// binary. The caller restarts the server.
func Server(ctx context.Context, s *Source, r *Release, exe, helperDir string) error {
	for _, dir := range []string{helperDir, filepath.Dir(exe)} {
		if err := CheckWritable(dir); err != nil {
			return fmt.Errorf("this user cannot write %s (%v): give the server's user that directory, or run \"sudo hostel server update\" on the server machine", dir, err)
		}
	}
	for name, sum := range r.Sums {
		if !helperRE.MatchString(name) {
			continue
		}
		dst := filepath.Join(helperDir, name)
		if FileSHA256(dst) == sum {
			continue
		}
		tmp := tempNext(dst)
		if err := s.Download(ctx, r, name, tmp); err != nil {
			return err
		}
		if err := Replace(tmp, dst); err != nil {
			return err
		}
	}
	// The server's own binary is also a helper file (checked above).
	own := filepath.Join(helperDir, BinaryName(runtime.GOOS, runtime.GOARCH))
	tmp := tempNext(exe)
	if err := copyFile(own, tmp); err != nil {
		return err
	}
	if err := checkRuns(ctx, tmp, r.Tag); err != nil {
		os.Remove(tmp)
		return err
	}
	return Replace(tmp, exe)
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Chmod(dst, 0o755)
}

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

// Server updates a server to release r: it downloads and checks each file of
// helperDir that differs from the release, and only then renames them into
// place (a failed download changes nothing), then the server binary exe. The
// default tools are in the binary. The caller restarts the server.
func Server(ctx context.Context, s *Source, r *Release, exe, helperDir string) (err error) {
	for _, dir := range []string{helperDir, filepath.Dir(exe)} {
		if err := CheckWritable(dir); err != nil {
			return fmt.Errorf("this user cannot write %s (%v): give the server's user that directory, or run \"sudo hostel server update\" on the server machine", dir, err)
		}
	}
	// tmps: final path → downloaded file.
	tmps := map[string]string{}
	defer func() {
		for _, tmp := range tmps {
			os.Remove(tmp)
		}
	}()
	for name, sum := range r.Sums {
		if !helperRE.MatchString(name) {
			continue
		}
		dst := filepath.Join(helperDir, name)
		if FileSHA256(dst) == sum {
			continue
		}
		tmp, err := s.Download(ctx, r, name, dst)
		if err != nil {
			return err
		}
		tmps[dst] = tmp
	}
	// The server's own binary is also a helper file (checked above).
	own := filepath.Join(helperDir, BinaryName(runtime.GOOS, runtime.GOARCH))
	if t, ok := tmps[own]; ok {
		own = t
	}
	tmp, err := copyNext(own, exe)
	if err != nil {
		return err
	}
	tmps[exe] = tmp
	if err := checkRuns(ctx, tmp, r.Tag); err != nil {
		return err
	}
	for dst, tmp := range tmps {
		if dst == exe {
			continue
		}
		if err := Replace(tmp, dst); err != nil {
			return err
		}
		delete(tmps, dst)
	}
	delete(tmps, exe)
	return Replace(tmp, exe)
}

// copyNext copies src to a new temporary file next to dst.
func copyNext(src, dst string) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	f, err := TempNext(dst)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o755)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

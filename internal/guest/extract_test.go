// Unit tests are allowed here (AGENTS.md): the path safety of "hostel cp" from
// the machine is small, deterministic code, and an E2E test cannot produce a
// crafted stream (the real helper never sends one).
package guest

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func stream(t *testing.T, entries ...*tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(make([]byte, h.Size))
		}
	}
	tw.Close()
	return &buf
}

func TestExtractStaysInsideTarget(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(t.TempDir(), "out")
	// A link to a directory outside the target, then a file below the link.
	err := extract(stream(t,
		&tar.Header{Name: "out/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "out/x", Typeflag: tar.TypeSymlink, Linkname: outside},
		&tar.Header{Name: "out/x/authorized_keys", Typeflag: tar.TypeReg, Mode: 0o600, Size: 4},
	), target)
	if err == nil {
		t.Fatal("extract wrote below a symbolic link")
	}
	if des, _ := os.ReadDir(outside); len(des) != 0 {
		t.Fatalf("a file was written outside the target: %v", des)
	}

	// A link at a file path is replaced, not followed.
	target2 := filepath.Join(t.TempDir(), "out")
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = extract(stream(t,
		&tar.Header{Name: "out/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "out/f", Typeflag: tar.TypeSymlink, Linkname: victim},
		&tar.Header{Name: "out/f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3},
	), target2)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep" {
		t.Fatalf("the file behind the link changed: %q", data)
	}
	if info, err := os.Lstat(filepath.Join(target2, "f")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("out/f is not a regular file: %v", err)
	}

	// Paths with .. are refused.
	if err := extract(stream(t, &tar.Header{Name: "out/../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}), filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("extract accepted a path with ..")
	}
}

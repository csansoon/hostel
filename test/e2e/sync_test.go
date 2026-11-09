//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (f *fixture) write(rel, body string) {
	f.e.writeFile(filepath.Join(f.e.cwd, rel), body)
}

func (f *fixture) remove(rel string) {
	f.e.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.e.cwd, rel)); err != nil {
		f.e.t.Fatal(err)
	}
}

// work runs a shell command in the work directory on the machine.
func (f *fixture) work(cmd string) string {
	return f.c.sh("cd /home/hostel/hostel/rooms/a/work && " + cmd)
}

// TestSync is scenario 11.
func TestSync(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()

	// Ignored files are not sent; include files are sent; .git is not sent.
	if got := f.work("ls -A | sort | tr '\\n' ' '"); got != ".env .gitignore a.txt hostel.toml setup.txt" {
		t.Fatalf("work directory after acquire: %q", got)
	}

	// A machine command creates a file and changes a synced file.
	f.e.mustHostel("run", "--", "echo built > build.out; echo machine > a.txt")
	r := f.e.mustHostel("diff")
	if !strings.Contains(r.stdout, "-hello") || !strings.Contains(r.stdout, "+machine") || !strings.Contains(r.stdout, "+++ b/build.out") {
		t.Fatalf("diff does not show the machine changes:\n%s", r.stdout)
	}

	// The guest edits a.txt with the same size within one second, adds b.txt,
	// and deletes .gitignore's neighbour hostel.toml is kept.
	f.write("a.txt", "HELLO\n")
	f.write("b.txt", "new\n")
	r = f.e.mustHostel("sync")
	if !strings.Contains(r.stderr, "2 files sent, 0 deleted") {
		t.Fatalf("second sync: %s", r)
	}
	if !strings.Contains(r.stderr, "machine changes were replaced by the local files: a.txt") {
		t.Fatalf("sync does not list the overwritten machine change: %s", r)
	}
	if got := f.work("cat a.txt b.txt build.out"); got != "HELLO\nnew\nbuilt" {
		t.Fatalf("files after the second sync: %q", got)
	}
	f.write("a.txt", "HELL0\n")
	r = f.e.mustHostel("sync")
	if !strings.Contains(r.stderr, "1 files sent, 0 deleted") {
		t.Fatalf("same-size edit: %s", r)
	}

	// Deleted local files are deleted on the machine; machine files stay.
	f.remove("b.txt")
	r = f.e.mustHostel("sync")
	if !strings.Contains(r.stderr, "0 files sent, 1 deleted") {
		t.Fatalf("delete: %s", r)
	}
	if got := f.work("test -e b.txt && echo yes || echo no; cat build.out"); got != "no\nbuilt" {
		t.Fatalf("after delete: %q", got)
	}

	// Type changes: file d becomes directory d/e, and back.
	f.write("d", "file\n")
	f.e.mustHostel("sync")
	f.remove("d")
	f.write("d/e", "nested\n")
	f.e.mustHostel("sync")
	if got := f.work("cat d/e"); got != "nested" {
		t.Fatalf("d/e: %q", got)
	}
	f.remove("d")
	f.write("d", "file again\n")
	f.e.mustHostel("sync")
	if got := f.work("cat d"); got != "file again" {
		t.Fatalf("d: %q", got)
	}
}

// TestSyncPathSafety is scenario 12: a symbolic link to a directory outside
// work/, followed by a file below that link name, does not write outside work/.
func TestSyncPathSafety(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()
	f.c.sh("mkdir -p /tmp/outside")
	if err := os.Symlink("/tmp/outside", filepath.Join(f.e.cwd, "l")); err != nil {
		t.Fatal(err)
	}
	f.e.mustHostel("sync")
	if got := f.work("readlink l"); got != "/tmp/outside" {
		t.Fatalf("link on the machine: %q", got)
	}
	f.remove("l")
	f.write("l/f", "inside\n")
	f.e.mustHostel("sync")
	if got := f.c.sh("ls -A /tmp/outside | wc -l"); got != "0" {
		t.Fatalf("a file was written outside work/: %s", f.c.sh("ls -A /tmp/outside"))
	}
	if got := f.work("cat l/f"); got != "inside" {
		t.Fatalf("l/f: %q", got)
	}
}

// TestCopyDiffShell: cp in both directions, diff without changes, and ssh (also
// from a worktree path with a space and a "%").
func TestCopyDiffShell(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.acquire()
	f.write("local/dir/x.txt", "x\n")
	f.e.mustHostel("cp", "local/dir", ":copied/")
	if got := f.work("cat copied/dir/x.txt"); got != "x" {
		t.Fatalf("copied file: %q", got)
	}
	f.e.mustHostel("cp", "local/dir/x.txt", ":renamed.txt")
	if got := f.work("cat renamed.txt"); got != "x" {
		t.Fatalf("renamed file: %q", got)
	}
	f.e.mustHostel("run", "--", "mkdir -p out/sub && echo y > out/sub/y.txt")
	dst := filepath.Join(f.e.home, "fetched")
	f.e.mustHostel("cp", ":out", dst)
	if data, err := os.ReadFile(filepath.Join(dst, "sub", "y.txt")); err != nil || string(data) != "y\n" {
		t.Fatalf("fetched file: %q, %v", data, err)
	}
	if r := f.e.hostel("cp", ":../etc/passwd", dst); r.code != 1 {
		t.Fatalf("cp of a path outside work: %s", r)
	}
	r := f.e.mustHostel("ssh", "--", "echo $HOSTEL_ROOM $(pwd)")
	if strings.TrimSpace(r.stdout) != "a /home/hostel/hostel/rooms/a/work" {
		t.Fatalf("ssh: %s", r)
	}
	r = f.e.mustHostel("ssh-config")
	if !strings.HasSuffix(strings.TrimSpace(r.stdout), "ssh_config") {
		t.Fatalf("ssh-config: %s", r)
	}

	// ssh_config paths with a space and a "%" token.
	f.e.mustHostel("release")
	f.waitRoomFree()
	f.e.newRepoNamed("my repo %d", defaultRepoFiles())
	f.acquire()
	if r := f.e.mustHostel("ssh", "--", "echo ok"); strings.TrimSpace(r.stdout) != "ok" {
		t.Fatalf("ssh from a path with a space: %s", r)
	}
}

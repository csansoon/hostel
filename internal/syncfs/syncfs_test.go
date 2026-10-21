// Unit tests are allowed here (AGENTS.md): the sync plan and its path safety
// are small, complex, deterministic code with many edge cases.
package syncfs

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func manifest(t *testing.T, root string, paths ...string) Manifest {
	t.Helper()
	m, err := BuildManifest(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// sync runs one full sync from guest to work and returns the plan.
func sync(t *testing.T, guest, work string, old, next Manifest) Plan {
	t.Helper()
	plan, err := MakePlan(work, old, next)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteTar(&buf, guest, next, plan.Send); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Apply(work, plan, next, &buf); err != nil {
		t.Fatalf("apply %+v: %v", plan, err)
	}
	return plan
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheckPath(t *testing.T) {
	for _, bad := range []string{"", ".", "/etc/passwd", "../x", "a/../../x", "a/./b", "a//b", "a/", `a\b`, "a\x00b", ".."} {
		if CheckPath(bad) == nil {
			t.Errorf("CheckPath(%q) = nil, want an error", bad)
		}
	}
	for _, good := range []string{"a", "a/b", ".env", "a/.b/c", "x..y"} {
		if err := CheckPath(good); err != nil {
			t.Errorf("CheckPath(%q) = %v", good, err)
		}
	}
}

func TestSyncSequence(t *testing.T) {
	guest, work := t.TempDir(), t.TempDir()
	write(t, guest, "a.txt", "one")
	write(t, guest, "dir/b.txt", "two")
	m1 := manifest(t, guest, "a.txt", "dir/b.txt", "missing.txt")
	p := sync(t, guest, work, Manifest{}, m1)
	if !reflect.DeepEqual(p.Send, []string{"a.txt", "dir/b.txt"}) || len(p.Overwrite) != 0 {
		t.Fatalf("first plan = %+v", p)
	}

	// A room command creates a file, and changes a synced file.
	write(t, work, "build/out.bin", "built")
	write(t, work, "a.txt", "room edit")

	// The guest edits b.txt with the same size, and deletes nothing.
	write(t, guest, "dir/b.txt", "TWO")
	m2 := manifest(t, guest, "a.txt", "dir/b.txt")
	p = sync(t, guest, work, m1, m2)
	if !reflect.DeepEqual(p.Send, []string{"a.txt", "dir/b.txt"}) {
		t.Fatalf("second plan send = %v", p.Send)
	}
	if !reflect.DeepEqual(p.Overwrite, []string{"a.txt"}) {
		t.Fatalf("second plan overwrite = %v, want the room edit of a.txt", p.Overwrite)
	}
	if read(t, work, "a.txt") != "one" || read(t, work, "dir/b.txt") != "TWO" || read(t, work, "build/out.bin") != "built" {
		t.Fatal("wrong files after the second sync")
	}

	// Nothing changed: nothing to send.
	if p, err := MakePlan(work, m2, m2); err != nil || len(p.Send)+len(p.Delete)+len(p.Overwrite) != 0 {
		t.Fatalf("unchanged plan = %+v, %v", p, err)
	}

	// The guest deletes dir/b.txt: it is deleted, and the empty dir too.
	os.Remove(filepath.Join(guest, "dir/b.txt"))
	m3 := manifest(t, guest, "a.txt")
	p = sync(t, guest, work, m2, m3)
	if !reflect.DeepEqual(p.Delete, []string{"dir/b.txt"}) {
		t.Fatalf("delete = %v", p.Delete)
	}
	if _, err := os.Lstat(filepath.Join(work, "dir")); !os.IsNotExist(err) {
		t.Fatalf("empty dir is still there: %v", err)
	}
}

func TestTypeChanges(t *testing.T) {
	guest, work := t.TempDir(), t.TempDir()
	write(t, guest, "a", "file")
	m1 := manifest(t, guest, "a")
	sync(t, guest, work, Manifest{}, m1)

	// File a becomes directory a with a/b.
	os.Remove(filepath.Join(guest, "a"))
	write(t, guest, "a/b", "nested")
	m2 := manifest(t, guest, "a/b")
	sync(t, guest, work, m1, m2)
	if read(t, work, "a/b") != "nested" {
		t.Fatal("a/b not written")
	}

	// And back: a/b goes, file a comes.
	os.RemoveAll(filepath.Join(guest, "a"))
	write(t, guest, "a", "file again")
	m3 := manifest(t, guest, "a")
	sync(t, guest, work, m2, m3)
	if read(t, work, "a") != "file again" {
		t.Fatal("a not written")
	}

	// Back to a/b while the room added a/x to the directory: the plan lists the
	// directory in overwrite (the room object blocks a send path).
	os.Remove(filepath.Join(guest, "a"))
	write(t, guest, "a/b", "nested")
	m4 := manifest(t, guest, "a/b")
	sync(t, guest, work, m3, m4)
	write(t, work, "a/x", "room file")
	os.RemoveAll(filepath.Join(guest, "a"))
	write(t, guest, "a", "file")
	m5 := manifest(t, guest, "a")
	p := sync(t, guest, work, m4, m5)
	if !reflect.DeepEqual(p.Overwrite, []string{"a"}) {
		t.Fatalf("overwrite = %v, want [a]", p.Overwrite)
	}
}

func TestSymlinkStaysInsideWork(t *testing.T) {
	guest, work, outside := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(guest, "l")); err != nil {
		t.Fatal(err)
	}
	m1 := manifest(t, guest, "l")
	sync(t, guest, work, Manifest{}, m1)
	if target, err := os.Readlink(filepath.Join(work, "l")); err != nil || target != outside {
		t.Fatalf("link = %q, %v", target, err)
	}

	// The guest replaces the link with a directory l with a file.
	os.Remove(filepath.Join(guest, "l"))
	write(t, guest, "l/f", "inside")
	m2 := manifest(t, guest, "l/f")
	sync(t, guest, work, m1, m2)
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("a file was written outside work: %v", entries)
	}
	if read(t, work, "l/f") != "inside" {
		t.Fatal("l/f not written in work")
	}

	// A crafted manifest with a link and a file below it fails.
	os.RemoveAll(filepath.Join(work, "l"))
	crafted := Manifest{Entries: []Entry{
		{Path: "l", Type: TypeSymlink, Target: outside, SHA256: m1.Entries[0].SHA256},
		{Path: "l/f", Type: TypeFile, Mode: 0o644, SHA256: m2.Entries[0].SHA256, Size: 6},
	}}
	plan, err := MakePlan(work, Manifest{}, crafted)
	if err != nil {
		t.Fatal(err)
	}
	// The stream has the link first, then the file below it.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: outside})
	tw.WriteHeader(&tar.Header{Name: "l/f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6})
	tw.Write([]byte("inside"))
	tw.Close()
	if _, _, err := Apply(work, plan, crafted, &buf); err == nil {
		t.Fatal("apply of a file below a link succeeded")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("a file was written outside work: %v", entries)
	}
}

func TestRejectsUnplannedPaths(t *testing.T) {
	guest, work := t.TempDir(), t.TempDir()
	write(t, guest, "a", "x")
	write(t, guest, "b", "y")
	m := manifest(t, guest, "a", "b")
	plan, err := MakePlan(work, Manifest{}, m)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteTar(&buf, guest, m, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Apply(work, plan, m, &buf); err == nil {
		t.Fatal("apply with a missing file succeeded")
	}
	if _, err := MakePlan(work, Manifest{}, Manifest{Entries: []Entry{{Path: "../x", Type: TypeFile}}}); err == nil {
		t.Fatal("plan with ../x succeeded")
	}
}

func TestApplyKeepsUnplannedBlockers(t *testing.T) {
	guest, work := t.TempDir(), t.TempDir()
	write(t, guest, "a/b", "x")
	m := manifest(t, guest, "a/b")
	plan, err := MakePlan(work, Manifest{}, m)
	if err != nil {
		t.Fatal(err)
	}
	// After the plan, a room job writes a file where the directory a must go.
	write(t, work, "a", "room data")
	var buf bytes.Buffer
	if err := WriteTar(&buf, guest, m, plan.Send); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Apply(work, plan, m, &buf); err == nil {
		t.Fatal("apply removed a room file that the plan does not list")
	}
	if read(t, work, "a") != "room data" {
		t.Fatal("the room file changed")
	}
}

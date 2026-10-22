// Package syncfs has the file sync of design section 12: manifests, sync plans,
// and writes that never leave the work directory.
package syncfs

import (
	"archive/tar"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Entry types.
const (
	TypeFile    = "file"
	TypeSymlink = "symlink"
)

// Entry is one file or symbolic link. Paths use "/" and are relative to the
// repository root (guest) or to work/ (room).
type Entry struct {
	Path   string      `json:"path"`
	Type   string      `json:"type"`
	Mode   fs.FileMode `json:"mode"`
	Size   int64       `json:"size"`
	SHA256 string      `json:"sha256"`
	Target string      `json:"target,omitempty"`
}

type Manifest struct {
	Entries []Entry `json:"entries"`
}

// Plan is the result of sync-plan.
type Plan struct {
	PlanID string `json:"plan_id"`
	// Send: entries that are missing or different in work/.
	Send []string `json:"send"`
	// Delete: paths of the old manifest that are not in the new manifest.
	Delete []string `json:"delete"`
	// Overwrite: paths in Send that a room command changed after the last sync,
	// and room objects that block a Send path.
	Overwrite []string `json:"overwrite"`
}

// CheckPath rejects paths that could leave the work directory.
func CheckPath(p string) error {
	switch {
	case p == "" || p == ".":
		return errors.New("empty path")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("absolute path %q", p)
	case strings.ContainsAny(p, "\\\x00"):
		return fmt.Errorf("path %q contains a backslash or NUL", p)
	case path.Clean(p) != p:
		return fmt.Errorf("path %q is not clean", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." {
			return fmt.Errorf("path %q has a %q component", p, part)
		}
	}
	return nil
}

// FileEntry reads one entry from the file system. ok is false if the path does
// not exist.
func FileEntry(root, rel string) (Entry, bool, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	e := Entry{Path: rel, Mode: info.Mode().Perm()}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return Entry{}, false, err
		}
		e.Type, e.Target, e.Mode = TypeSymlink, target, 0
		sum := sha256.Sum256([]byte(target))
		e.SHA256 = hex.EncodeToString(sum[:])
	case info.Mode().IsRegular():
		e.Type, e.Size = TypeFile, info.Size()
		f, err := os.Open(full)
		if err != nil {
			return Entry{}, false, err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return Entry{}, false, err
		}
		e.SHA256 = hex.EncodeToString(h.Sum(nil))
	default:
		e.Type = "other"
	}
	return e, true, nil
}

// same reports whether two entries have the same content.
func same(a, b Entry) bool {
	return a.Type == b.Type && a.SHA256 == b.SHA256 && (a.Type == TypeSymlink || a.Mode == b.Mode)
}

// safeLstat returns the room object at rel. blocked is true if a parent
// component is not a directory (a file or a symbolic link), so the object at
// rel cannot be reached without leaving work/ or removing the parent.
func safeLstat(root, rel string) (info fs.FileInfo, blockedBy string, err error) {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		parent := strings.Join(parts[:i], "/")
		pi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(parent)))
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		if !pi.IsDir() {
			return nil, parent, nil
		}
	}
	info, err = os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	return info, "", err
}

// MakePlan compares the new manifest with work/ and with the old manifest.
func MakePlan(work string, old, next Manifest) (Plan, error) {
	plan := Plan{PlanID: randomID(), Send: []string{}, Delete: []string{}, Overwrite: []string{}}
	oldBy := map[string]Entry{}
	for _, e := range old.Entries {
		oldBy[e.Path] = e
	}
	nextBy := map[string]bool{}
	overwrite := map[string]bool{}
	for _, e := range next.Entries {
		if err := CheckPath(e.Path); err != nil {
			return plan, err
		}
		if e.Type != TypeFile && e.Type != TypeSymlink {
			return plan, fmt.Errorf("%s: unsupported type %q", e.Path, e.Type)
		}
		nextBy[e.Path] = true
	}
	for _, e := range old.Entries {
		if !nextBy[e.Path] {
			plan.Delete = append(plan.Delete, e.Path)
		}
	}
	deleted := map[string]bool{}
	for _, p := range plan.Delete {
		deleted[p] = true
	}
	for _, e := range next.Entries {
		info, blockedBy, err := safeLstat(work, e.Path)
		if err != nil {
			return plan, err
		}
		if blockedBy != "" {
			plan.Send = append(plan.Send, e.Path)
			// A blocking file of the old manifest is deleted in step 1 anyway.
			if !deleted[blockedBy] {
				overwrite[blockedBy] = true
			}
			continue
		}
		if info == nil {
			plan.Send = append(plan.Send, e.Path)
			continue
		}
		if info.IsDir() {
			plan.Send = append(plan.Send, e.Path)
			if keep, err := dirKeepsFiles(work, e.Path, deleted); err != nil {
				return plan, err
			} else if keep {
				overwrite[e.Path] = true
			}
			continue
		}
		room, _, err := FileEntry(work, e.Path)
		if err != nil {
			return plan, err
		}
		if same(room, e) {
			continue
		}
		plan.Send = append(plan.Send, e.Path)
		if prev, ok := oldBy[e.Path]; !ok || !same(prev, room) {
			overwrite[e.Path] = true
		}
	}
	for p := range overwrite {
		plan.Overwrite = append(plan.Overwrite, p)
	}
	sort.Strings(plan.Send)
	sort.Strings(plan.Delete)
	sort.Strings(plan.Overwrite)
	return plan, nil
}

// dirKeepsFiles reports whether a room directory has an object that step 1
// (delete) does not remove.
func dirKeepsFiles(work, rel string, deleted map[string]bool) (bool, error) {
	keep := false
	err := filepath.WalkDir(filepath.Join(work, filepath.FromSlash(rel)), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		r, err := filepath.Rel(work, p)
		if err != nil {
			return err
		}
		if !deleted[filepath.ToSlash(r)] {
			keep = true
			return filepath.SkipAll
		}
		return nil
	})
	return keep, err
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Apply runs a plan (design, section 12, sync-apply): delete, remove blockers,
// write the files from the tar stream. The stream must have exactly the Send
// paths. It never writes or deletes outside work.
func Apply(work string, plan Plan, next Manifest, r io.Reader) (written, deleted int, err error) {
	overwrite := map[string]bool{}
	for _, p := range plan.Overwrite {
		overwrite[p] = true
	}
	send := map[string]Entry{}
	nextBy := map[string]Entry{}
	for _, e := range next.Entries {
		nextBy[e.Path] = e
	}
	for _, p := range plan.Send {
		if err := CheckPath(p); err != nil {
			return 0, 0, err
		}
		send[p] = nextBy[p]
	}

	// Step 1: delete, deepest first, then empty directories up to work/.
	dels := append([]string(nil), plan.Delete...)
	sort.Slice(dels, func(i, j int) bool { return strings.Count(dels[i], "/") > strings.Count(dels[j], "/") })
	for _, p := range dels {
		if err := CheckPath(p); err != nil {
			return 0, 0, err
		}
		info, blockedBy, err := safeLstat(work, p)
		if err != nil {
			return 0, deleted, err
		}
		if blockedBy != "" || info == nil || info.IsDir() {
			continue // already gone, or replaced by the room
		}
		if err := os.Remove(filepath.Join(work, filepath.FromSlash(p))); err != nil {
			return 0, deleted, err
		}
		deleted++
		removeEmptyParents(work, p)
	}

	// Step 2: remove objects that block a send path.
	for _, p := range sortedKeys(send) {
		parts := strings.Split(p, "/")
		for i := 1; i <= len(parts); i++ {
			sub := strings.Join(parts[:i], "/")
			info, err := os.Lstat(filepath.Join(work, filepath.FromSlash(sub)))
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				return 0, deleted, err
			}
			full := filepath.Join(work, filepath.FromSlash(sub))
			switch {
			case i < len(parts) && !info.IsDir():
				// A file or a symbolic link where a directory must be. Only a
				// blocker that the plan lists goes; a newer room object stays.
				if !overwrite[sub] {
					return 0, deleted, fmt.Errorf("%s: a room file is in the way of %s; it is not in the plan's overwrite list (run the sync again)", sub, p)
				}
				if err := os.Remove(full); err != nil {
					return 0, deleted, err
				}
			case i == len(parts) && info.IsDir():
				if !overwrite[sub] {
					return 0, deleted, fmt.Errorf("%s: a directory on the room is in the way; it is not in the plan's overwrite list", sub)
				}
				if err := os.RemoveAll(full); err != nil {
					return 0, deleted, err
				}
			}
			if i < len(parts) && !info.IsDir() {
				break
			}
		}
	}

	// Step 3: write the files.
	tr := tar.NewReader(r)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return written, deleted, fmt.Errorf("read the file stream: %w", err)
		}
		e, ok := send[hdr.Name]
		if !ok || seen[hdr.Name] {
			return written, deleted, fmt.Errorf("unexpected path %q in the file stream", hdr.Name)
		}
		seen[hdr.Name] = true
		if err := writeEntry(work, e, hdr, tr); err != nil {
			return written, deleted, fmt.Errorf("write %s: %w", hdr.Name, err)
		}
		written++
	}
	if len(seen) != len(send) {
		return written, deleted, fmt.Errorf("the file stream has %d of %d files", len(seen), len(send))
	}
	return written, deleted, nil
}

func sortedKeys(m map[string]Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func removeEmptyParents(work, rel string) {
	for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
		if os.Remove(filepath.Join(work, filepath.FromSlash(dir))) != nil {
			return
		}
	}
}

// MkdirSafe creates the parent directories of rel below root and fails if a
// component is a symbolic link or a file.
func MkdirSafe(root, rel string) error {
	parts := strings.Split(rel, "/")
	cur := root
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}

func writeEntry(work string, e Entry, hdr *tar.Header, r io.Reader) error {
	if err := MkdirSafe(work, e.Path); err != nil {
		return err
	}
	full := filepath.Join(work, filepath.FromSlash(e.Path))
	switch e.Type {
	case TypeSymlink:
		if hdr.Typeflag != tar.TypeSymlink || hdr.Linkname != e.Target {
			return errors.New("the stream entry does not match the manifest")
		}
		if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		// The link is written as a link; it is never followed.
		return os.Symlink(e.Target, full)
	case TypeFile:
		if hdr.Typeflag != tar.TypeReg {
			return errors.New("the stream entry does not match the manifest")
		}
		tmp, err := os.CreateTemp(filepath.Dir(full), ".hostel-sync-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, h), r)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
			return fmt.Errorf("hash mismatch: got %s, want %s", got, e.SHA256)
		}
		if err := os.Chmod(tmp.Name(), e.Mode.Perm()); err != nil {
			return err
		}
		// Rename replaces a file or a symbolic link; it does not follow a link.
		return os.Rename(tmp.Name(), full)
	}
	return fmt.Errorf("unsupported type %q", e.Type)
}

// BuildManifest reads the entries for paths below root. Paths that do not exist
// (deleted, not yet staged) and objects that are not files or symbolic links
// (for example a submodule directory) are skipped. It hashes every file at each
// sync: a cache keyed by size and time misses fast edits.
func BuildManifest(root string, paths []string) (Manifest, error) {
	m := Manifest{Entries: []Entry{}}
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] || p == ".git" || strings.HasPrefix(p, ".git/") {
			continue
		}
		seen[p] = true
		if err := CheckPath(p); err != nil {
			return m, err
		}
		e, ok, err := FileEntry(root, p)
		if err != nil {
			return m, err
		}
		if ok && (e.Type == TypeFile || e.Type == TypeSymlink) {
			m.Entries = append(m.Entries, e)
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m, nil
}

// WriteTar writes the entries with the given paths from root as a tar stream.
func WriteTar(w io.Writer, root string, m Manifest, paths []string) error {
	byPath := map[string]Entry{}
	for _, e := range m.Entries {
		byPath[e.Path] = e
	}
	tw := tar.NewWriter(w)
	for _, p := range paths {
		e, ok := byPath[p]
		if !ok {
			return fmt.Errorf("plan path %q is not in the manifest", p)
		}
		full := filepath.Join(root, filepath.FromSlash(p))
		if e.Type == TypeSymlink {
			if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeSymlink, Linkname: e.Target}); err != nil {
				return err
			}
			continue
		}
		f, err := os.Open(full)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Mode: int64(e.Mode.Perm()), Size: info.Size()}); err != nil {
			f.Close()
			return err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("send %s: %w (did it change during the sync? run the sync again)", p, err)
		}
	}
	return tw.Close()
}

// CheckParents fails if a parent component of rel below root is a symbolic link
// or a file.
func CheckParents(root, rel string) error {
	parts := strings.Split(rel, "/")
	cur := root
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}

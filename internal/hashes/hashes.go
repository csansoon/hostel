// Package hashes computes the hashes that the server and the helper compare
// (design, section 8, "Probe and upload").
package hashes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// File returns the SHA-256 of a file, or "" if it does not exist.
func File(path string) (string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Bytes returns the SHA-256 of b.
func Bytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Entry is one file of a tool directory.
type Entry struct {
	Path   string // relative, with "/"
	Mode   fs.FileMode
	SHA256 string
}

// Dir returns the hash of a directory: the SHA-256 of one line
// "<path> <mode> <sha256>\n" for each file, sorted by path.
func Dir(entries []Entry) string {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	var b strings.Builder
	for _, e := range sorted {
		fmt.Fprintf(&b, "%s %o %s\n", e.Path, e.Mode.Perm(), e.SHA256)
	}
	return Bytes([]byte(b.String()))
}

// DirEntries lists the regular files in dir (not recursive) with their hashes.
// The names filter selects files; nil selects all. Without modes (for Windows,
// which has none), every Mode is 0.
func DirEntries(dir string, names func(string) bool, modes bool) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		if !de.Type().IsRegular() || (names != nil && !names(de.Name())) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			return nil, err
		}
		sum, err := File(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, err
		}
		e := Entry{Path: de.Name(), SHA256: sum}
		if modes {
			e.Mode = info.Mode()
		}
		out = append(out, e)
	}
	return out, nil
}

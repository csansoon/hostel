package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/store"
)

// effectiveTools returns the server's defined tools: each default unless a
// revision replaces or removes it, and the own tools.
func effectiveTools(latest map[string]store.ToolRevision) map[string]map[string]string {
	out := config.DefaultTools()
	for name, r := range latest {
		switch r.State {
		case store.ToolFiles:
			out[name] = r.Files
		case store.ToolRemoved:
			delete(out, name)
		}
	}
	return out
}

// isDefault reports whether files are the same as the default tool name.
func isDefault(name string, files map[string]string) bool {
	def, ok := config.DefaultTools()[name]
	return ok && maps.Equal(def, files)
}

// writeTools writes a tool set to data_dir/tools/<hash> (once for each
// content) and returns the directory. Scripts get mode 0755, so the hashes of
// the uploads (with modes) do not change between starts.
func writeTools(dataDir string, set map[string]map[string]string) (string, error) {
	h := sha256.New()
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		files := make([]string, 0, len(set[n]))
		for f := range set[n] {
			files = append(files, f)
		}
		sort.Strings(files)
		for _, f := range files {
			fmt.Fprintf(h, "%s/%s %d\n%s", n, f, len(set[n][f]), set[n][f])
		}
	}
	root := filepath.Join(dataDir, "tools")
	dir := filepath.Join(root, hex.EncodeToString(h.Sum(nil))[:16])
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, ".tmp-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o755); err != nil {
		return "", err
	}
	for n, files := range set {
		if err := os.Mkdir(filepath.Join(tmp, n), 0o755); err != nil {
			return "", err
		}
		for f, data := range files {
			mode := os.FileMode(0o644)
			if ext := filepath.Ext(f); ext == ".sh" || ext == ".ps1" {
				mode = 0o755
			}
			if err := os.WriteFile(filepath.Join(tmp, n, f), []byte(data), mode); err != nil {
				return "", err
			}
		}
	}
	if err := os.Rename(tmp, dir); err != nil && !os.IsExist(err) {
		return "", err
	}
	return dir, nil
}

// removeOldToolDirs removes the tool directories of earlier starts, and the
// platform directories of the servers before M10e.
func removeOldToolDirs(dataDir, keep string) {
	entries, _ := os.ReadDir(filepath.Join(dataDir, "tools"))
	for _, e := range entries {
		if p := filepath.Join(dataDir, "tools", e.Name()); p != keep {
			os.RemoveAll(p)
		}
	}
	os.RemoveAll(filepath.Join(dataDir, "platforms"))
}

// toolSource describes where a tool comes from, for the list.
func toolSource(name string, latest map[string]store.ToolRevision) string {
	r, ok := latest[name]
	_, isDef := config.DefaultTools()[name]
	switch {
	case isDef && (!ok || r.State == store.ToolDefault):
		return "default"
	case isDef:
		return "changed"
	default:
		return "own"
	}
}

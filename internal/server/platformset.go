package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/store"
	"github.com/csansoon/hostel/platforms"
)

// effectivePlatforms returns the server's platforms: each default unless a
// revision replaces or removes it, and the own platforms.
func effectivePlatforms(latest map[string]store.PlatformRevision) map[string]map[string]string {
	out := map[string]map[string]string{}
	for name, files := range platforms.Defaults() {
		out[name] = map[string]string{}
		for f, data := range files {
			out[name][f] = string(data)
		}
	}
	for name, r := range latest {
		switch r.State {
		case store.PlatformFiles:
			out[name] = r.Files
		case store.PlatformRemoved:
			delete(out, name)
		}
	}
	return out
}

// isDefault reports whether files are the same as the default platform name.
func isDefault(name string, files map[string]string) bool {
	def, ok := platforms.Defaults()[name]
	if !ok || len(def) != len(files) {
		return false
	}
	for f, data := range def {
		if files[f] != string(data) {
			return false
		}
	}
	return true
}

// writePlatforms writes a platform set to data_dir/platforms/<hash> (once for
// each content) and returns the directory. Scripts get mode 0755, so the
// hashes of the uploads (with modes) do not change between starts.
func writePlatforms(dataDir string, set map[string]map[string]string) (string, error) {
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
	root := filepath.Join(dataDir, "platforms")
	dir := filepath.Join(root, hex.EncodeToString(h.Sum(nil))[:16])
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}
	tmp, err := os.MkdirTemp(root, ".tmp-")
	if os.IsNotExist(err) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		tmp, err = os.MkdirTemp(root, ".tmp-")
	}
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

// removeOldPlatformDirs removes the platform directories of earlier starts.
func removeOldPlatformDirs(dataDir, keep string) {
	entries, _ := os.ReadDir(filepath.Join(dataDir, "platforms"))
	for _, e := range entries {
		if p := filepath.Join(dataDir, "platforms", e.Name()); p != keep {
			os.RemoveAll(p)
		}
	}
}

// importPlatforms stores the platforms of platforms_dir that are not the same
// as a default (the first start of M9; loadConfig calls it only before the
// configuration import). A platform that has a revision already (an import
// that stopped) is skipped, so it can repeat.
func importPlatforms(ctx context.Context, st *store.Store, dir string, now time.Time) error {
	latest, err := st.LatestPlatforms(ctx)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		files, err := readPlatformDir(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		if _, done := latest[e.Name()]; done || isDefault(e.Name(), files) {
			continue
		}
		if _, err := st.AddPlatformRevision(ctx, e.Name(), store.PlatformFiles, files, 0, now); err != nil {
			return err
		}
	}
	return nil
}

func readPlatformDir(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !config.IsPlatformFile(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files[e.Name()] = string(data)
	}
	return files, nil
}

// platformSource describes where a platform comes from, for the list.
func platformSource(name string, latest map[string]store.PlatformRevision) string {
	r, ok := latest[name]
	_, isDef := platforms.Defaults()[name]
	switch {
	case isDef && (!ok || r.State == store.PlatformDefault):
		return "default"
	case isDef:
		return "changed"
	case slices.Contains(config.OSNames, name) && (!ok || r.State != store.PlatformFiles):
		return "default"
	case slices.Contains(config.OSNames, name):
		return "changed"
	default:
		return "own"
	}
}

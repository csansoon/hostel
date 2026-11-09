package helper

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/hashes"
)

// install receives an item from the server on stdin and replaces the installed
// item only if the received bytes have the expected hash:
//
//	install mise --sha256 H          gzip stream of the mise binary
//	install mise-config --sha256 H   hostel/mise/config.toml
//	install machine-env --sha256 H      hostel/machine-env.json
//	install tool NAME --sha256 H     tar stream of the tool directory
func (h *helper) install(args []string, stdin io.Reader) (struct{}, error) {
	fs := cli.NewFlagSet("hostel helper install", io.Discard)
	sum := fs.String("sha256", "", "expected SHA-256")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return struct{}{}, err
	}
	if len(pos) == 0 || *sum == "" {
		return struct{}{}, fmt.Errorf("%w: install <item> [name] --sha256 <hash>", cli.ErrUsage)
	}
	switch {
	case pos[0] == "mise" && len(pos) == 1:
		gz, err := gzip.NewReader(stdin)
		if err != nil {
			return struct{}{}, fmt.Errorf("read the mise stream: %w", err)
		}
		return struct{}{}, writeVerified(h.misePath(), gz, *sum, 0o755)
	case pos[0] == "mise-config" && len(pos) == 1:
		return struct{}{}, writeVerified(h.miseConfigPath(), stdin, *sum, 0o644)
	case pos[0] == "machine-env" && len(pos) == 1:
		return struct{}{}, writeVerified(h.machineEnvPath(), stdin, *sum, 0o600)
	case pos[0] == "tool" && len(pos) == 2 && config.ValidName(pos[1]):
		return struct{}{}, h.installTool(pos[1], stdin, *sum)
	}
	return struct{}{}, fmt.Errorf("%w: unknown install item %q", cli.ErrUsage, strings.Join(pos, " "))
}

// writeVerified writes r to a temporary file next to path, checks its hash, and
// renames it to path.
func writeVerified(path string, r io.Reader, want string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
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
		return fmt.Errorf("write %s: %w", path, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("install %s: hash mismatch: got %s, want %s", filepath.Base(path), got, want)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return replaceFile(tmp.Name(), path)
}

// replaceFile renames src to dst. Windows cannot replace an executable that
// runs (for example mise.exe in another room's lease), but it can rename it:
// the old file moves aside and is deleted at a later install.
func replaceFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	old, _ := filepath.Glob(filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".old-*"))
	for _, f := range old {
		os.Remove(f)
	}
	if os.Rename(dst, filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".old-"+randHex())) != nil {
		return err
	}
	return os.Rename(src, dst)
}

func (h *helper) installTool(name string, r io.Reader, want string) error {
	if err := os.MkdirAll(h.toolsDir(), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(h.toolsDir(), "."+name+".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read the tool stream: %w", err)
		}
		// Tool directories are flat: only regular files without a path.
		if hdr.Typeflag != tar.TypeReg || hdr.Name != filepath.Base(hdr.Name) || strings.HasPrefix(hdr.Name, ".") {
			return fmt.Errorf("tool %s: unexpected entry %q", name, hdr.Name)
		}
		f, err := os.OpenFile(filepath.Join(tmp, hdr.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if err := os.Chmod(f.Name(), os.FileMode(hdr.Mode).Perm()); err != nil {
			return err
		}
	}
	entries, err := hashes.DirEntries(tmp, nil, runtime.GOOS != "windows")
	if err != nil {
		return err
	}
	if got := hashes.Dir(entries); got != want {
		return fmt.Errorf("install tool %s: hash mismatch: got %s, want %s", name, got, want)
	}
	dst := h.toolDir(name)
	if len(entries) == 0 {
		// The tool has no files for this OS: remove the old ones.
		return os.RemoveAll(dst)
	}
	old := filepath.Join(h.toolsDir(), "."+name+".old-"+randHex())
	if err := os.Rename(dst, old); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

func randHex() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

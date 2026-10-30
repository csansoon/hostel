package room

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/csansoon/hostel/internal/config"
)

// writeDesktopCommand writes hostel/bin/desktop, which runs a command in the
// desktop session ("room desktop run").
func (h *helper) writeDesktopCommand() error {
	path := filepath.Join(h.root, "bin", "desktop")
	content := fmt.Sprintf("#!/bin/sh\nexec '%s' room desktop run \"$@\"\n", strings.ReplaceAll(h.exe, "'", `'\''`))
	if runtime.GOOS == "windows" {
		path += ".cmd"
		content = fmt.Sprintf("@\"%s\" room desktop run %%*\r\n", h.exe)
	}
	if err := writeFileAtomic(path, []byte(content), 0o755); err != nil {
		return fmt.Errorf("write the desktop command: %w", err)
	}
	return nil
}

// installWinApp installs the pinned winapp CLI (Microsoft; "winapp ui" drives
// Windows apps with UI Automation) and writes hostel/bin/winapp.cmd, which
// runs it in the desktop session: UI Automation does not work from SSH.
func (h *helper) installWinApp(ctx context.Context) error {
	dir := filepath.Join(h.root, "tools", "winapp-"+config.WinAppVersion)
	exe := filepath.Join(dir, "winapp.exe")
	if _, err := os.Stat(exe); err != nil {
		if err := downloadWinApp(ctx, dir); err != nil {
			return fmt.Errorf("install winapp %s: %w", config.WinAppVersion, err)
		}
	}
	content := fmt.Sprintf("@\"%s\" room desktop run -- \"%s\" %%*\r\n", h.exe, exe)
	if err := writeFileAtomic(filepath.Join(h.root, "bin", "winapp.cmd"), []byte(content), 0o755); err != nil {
		return fmt.Errorf("write the winapp command: %w", err)
	}
	return nil
}

func downloadWinApp(ctx context.Context, dir string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.WinAppURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", config.WinAppURL, resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dir), ".winapp-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), resp.Body); err != nil {
		return fmt.Errorf("download %s: %w", config.WinAppURL, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != config.WinAppSHA256 {
		return fmt.Errorf("download %s: SHA-256 %s, want %s", config.WinAppURL, got, config.WinAppSHA256)
	}
	part := dir + ".part"
	os.RemoveAll(part)
	if err := unzip(tmp.Name(), part); err != nil {
		os.RemoveAll(part)
		return err
	}
	os.RemoveAll(dir)
	return os.Rename(part, dir)
}

func unzip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		path := filepath.Join(dst, f.Name)
		if !strings.HasPrefix(path, filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("unzip: bad path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := unzipFile(f, path); err != nil {
			return err
		}
	}
	return nil
}

func unzipFile(f *zip.File, path string) error {
	r, err := f.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

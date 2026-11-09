package helper

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/syncfs"
)

// workPath checks a path relative to work/ ("." is work/ itself) and that no
// parent component is a symbolic link or a file.
func (h *helper) workPath(room, rel string) (string, error) {
	if rel == "." {
		return h.workDir(room), nil
	}
	if err := syncfs.CheckPath(rel); err != nil {
		return "", err
	}
	if err := syncfs.CheckParents(h.workDir(room), rel); err != nil {
		return "", err
	}
	return filepath.Join(h.workDir(room), filepath.FromSlash(rel)), nil
}

// tarOut writes a file or a directory below work/ as a tar stream. Entry names
// start with the base name of the path. Symbolic links are copied as links.
func (h *helper) tarOut(args []string, stdout io.Writer) error {
	room, pos, err := roomFlag("tar-out", args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("%w: tar-out --room <room> <path>", cli.ErrUsage)
	}
	src, err := h.workPath(room, pos[0])
	if err != nil {
		return err
	}
	if _, err := os.Lstat(src); err != nil {
		return fmt.Errorf("%s does not exist in the work directory", pos[0])
	}
	tw := tar.NewWriter(stdout)
	base := filepath.Dir(src)
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// tarIn extracts a tar stream into a directory below work/.
func (h *helper) tarIn(args []string, stdin io.Reader) (struct{}, error) {
	room, pos, err := roomFlag("tar-in", args)
	if err != nil {
		return struct{}{}, err
	}
	if len(pos) != 1 {
		return struct{}{}, fmt.Errorf("%w: tar-in --room <room> <dir>", cli.ErrUsage)
	}
	if _, err := h.workPath(room, pos[0]); err != nil {
		return struct{}{}, err
	}
	tr := tar.NewReader(stdin)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return struct{}{}, nil
		}
		if err != nil {
			return struct{}{}, fmt.Errorf("read the file stream: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		rel := name
		if pos[0] != "." {
			rel = path.Join(pos[0], name)
		}
		if err := syncfs.CheckPath(name); err != nil {
			return struct{}{}, err
		}
		if err := syncfs.MkdirSafe(h.workDir(room), rel); err != nil {
			return struct{}{}, err
		}
		full := filepath.Join(h.workDir(room), filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if info, err := os.Lstat(full); err == nil && !info.IsDir() {
				return struct{}{}, fmt.Errorf("%s exists and is not a directory", rel)
			}
			if err := os.MkdirAll(full, 0o755); err != nil {
				return struct{}{}, err
			}
		case tar.TypeSymlink:
			os.Remove(full)
			if err := os.Symlink(hdr.Linkname, full); err != nil {
				return struct{}{}, err
			}
		case tar.TypeReg:
			if info, err := os.Lstat(full); err == nil && info.Mode()&fs.ModeSymlink != 0 {
				os.Remove(full)
			}
			f, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
			if err != nil {
				return struct{}{}, err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return struct{}{}, err
			}
		default:
			return struct{}{}, fmt.Errorf("%s: unsupported file type", rel)
		}
	}
}

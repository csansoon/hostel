package guest

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/syncfs"
)

// Release is "hostel release": it ends the lease, or cancels it if it is
// still queued.
func Release(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel release", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	repo, err := FindRepo()
	if err != nil {
		return fail(stderr, err)
	}
	st, err := repo.SelectState(*platform)
	if err != nil {
		return fail(stderr, err)
	}
	api, err := apiClient()
	if err != nil {
		return fail(stderr, err)
	}
	var l protocol.Lease
	if err := api.Do(context.Background(), "POST", "/v1/leases/"+st.LeaseID+"/release", nil, &l); err != nil {
		return fail(stderr, err)
	}
	stopPorts(st)
	st.Final = true
	if err := st.Save(); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "lease %s ended (%s)\n", st.LeaseID, l.State)
	return 0
}

// Diff is "hostel diff": the room changes against the last sync.
func Diff(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel diff", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	conn, err := Dial(context.Background(), l.st, "cmd")
	if err != nil {
		return l.fail(stderr, err)
	}
	defer conn.Close()
	if err := conn.Stream(context.Background(), []string{"diff"}, nil, stdout); err != nil {
		return l.fail(stderr, err)
	}
	return 0
}

// Cp is "hostel cp <from> <to>". A ":" prefix marks a path in the work directory.
func Cp(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel cp", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) != 2 || strings.HasPrefix(pos[0], ":") == strings.HasPrefix(pos[1], ":") {
		fmt.Fprintln(stderr, "hostel: usage: hostel cp <local> :<room path> | hostel cp :<room path> <local>")
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	conn, err := Dial(context.Background(), l.st, "cmd")
	if err != nil {
		return l.fail(stderr, err)
	}
	defer conn.Close()
	if strings.HasPrefix(pos[1], ":") {
		err = copyIn(conn, pos[0], strings.TrimPrefix(pos[1], ":"))
	} else {
		err = copyOut(conn, strings.TrimPrefix(pos[0], ":"), pos[1])
	}
	if err != nil {
		return l.fail(stderr, err)
	}
	return 0
}

// cleanRoomPath turns a path relative to the work directory into a clean path
// ("." for the work directory itself).
func cleanRoomPath(p string) (string, error) {
	p = path.Clean(strings.TrimPrefix(p, "./"))
	if p == "." {
		return p, nil
	}
	return p, syncfs.CheckPath(p)
}

// copyIn copies a local file or directory to the room path dst. If dst ends
// with "/", the copy goes into that directory with its own name.
func copyIn(conn *Conn, src, dst string) error {
	if _, err := os.Lstat(src); err != nil {
		return err
	}
	if strings.HasSuffix(dst, "/") || dst == "" {
		dst = path.Join(dst, filepath.Base(src))
	}
	dst, err := cleanRoomPath(dst)
	if err != nil {
		return err
	}
	if dst == "." {
		return errors.New("the target cannot be the work directory itself; name a path in it")
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			name := path.Base(dst)
			if rel != "." {
				name = path.Join(name, filepath.ToSlash(rel))
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			link := ""
			if fi.Mode()&fs.ModeSymlink != 0 {
				if link, err = os.Readlink(p); err != nil {
					return err
				}
			}
			hdr, err := tar.FileInfoHeader(fi, link)
			if err != nil {
				return err
			}
			hdr.Name = name
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if fi.Mode().IsRegular() {
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
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	err = conn.Helper(context.Background(), []string{"tar-in", path.Dir(dst)}, pr, nil)
	pr.Close()
	return err
}

// copyOut copies a room file or directory to a local path. If dst is an
// existing directory or ends with "/", the copy goes into it with its own name.
func copyOut(conn *Conn, src, dst string) error {
	src, err := cleanRoomPath(src)
	if err != nil {
		return err
	}
	target := dst
	if info, err := os.Stat(dst); (err == nil && info.IsDir()) || strings.HasSuffix(dst, "/") {
		base := path.Base(src)
		if src == "." {
			base = "work"
		}
		target = filepath.Join(dst, base)
	}
	pr, pw := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		err := conn.Stream(context.Background(), []string{"tar-out", src}, nil, pw)
		pw.CloseWithError(err)
		errc <- err
	}()
	err = extract(pr, target)
	pr.CloseWithError(err)
	if serr := <-errc; serr != nil {
		return serr
	}
	return err
}

// extract writes a tar stream whose first path component is replaced by target.
func extract(r io.Reader, target string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		_, rest, _ := strings.Cut(name, "/")
		if rest != "" {
			if err := syncfs.CheckPath(rest); err != nil {
				return err
			}
		}
		full := filepath.Join(target, filepath.FromSlash(rest))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.Remove(full)
			if err := os.Symlink(hdr.Linkname, full); err != nil {
				return err
			}
		case tar.TypeReg:
			f, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode).Perm())
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
		}
	}
}

// writeSSHConfig writes an ssh_config file for "ssh -F" and returns its path
// and the host alias.
func writeSSHConfig(st *State) (string, string, error) {
	host, port, err := net.SplitHostPort(st.Access.GatewayAddress)
	if err != nil {
		return "", "", err
	}
	alias := "hostel-" + st.LeaseID
	known := filepath.Join(st.Dir, "known_hosts")
	hostEntry := host
	if port != "22" {
		hostEntry = "[" + host + "]:" + port
	}
	if err := os.WriteFile(known, []byte(hostEntry+" "+st.Access.GatewayHostKey+"\n"), 0o600); err != nil {
		return "", "", err
	}
	cfg := fmt.Sprintf(`Host %s
  HostName %s
  Port %s
  User %s.cmd
  IdentityFile %s
  IdentitiesOnly yes
  IdentityAgent none
  ForwardAgent no
  UserKnownHostsFile %s
  StrictHostKeyChecking yes
`, alias, host, port, st.LeaseID, st.KeyPath(), known)
	file := filepath.Join(st.Dir, "ssh_config")
	if err := os.WriteFile(file, []byte(cfg), 0o600); err != nil {
		return "", "", err
	}
	return file, alias, nil
}

// SSHConfig is "hostel ssh-config".
func SSHConfig(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel ssh-config", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	file, alias, err := writeSSHConfig(l.st)
	if err != nil {
		return l.fail(stderr, err)
	}
	fmt.Fprintln(stdout, file)
	fmt.Fprintf(stderr, "hostel: use it with: ssh -F %s %s\n", file, alias)
	return 0
}

// SSH is "hostel ssh [-- command]": an interactive shell (or one command) with
// the system ssh client.
func SSH(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel ssh", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	file, alias, err := writeSSHConfig(l.st)
	if err != nil {
		return l.fail(stderr, err)
	}
	cmd := exec.Command("ssh", append([]string{"-F", file, alias}, pos...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		return l.fail(stderr, fmt.Errorf("run ssh: %w", err))
	}
	return 0
}

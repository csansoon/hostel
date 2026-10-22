package roomops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/csansoon/hostel/internal/hashes"
	"github.com/csansoon/hostel/internal/protocol"
)

// Room is a connected room with its probe result.
type Room struct {
	Conn *Conn
	OS   string // Hostel OS name
	Arch string // Go architecture name
	Home string
}

// HelperPath returns the absolute helper path on the room.
func (r *Room) HelperPath() string { return r.Home + "/hostel/bin/hostel" }

// Probe finds the room's OS, architecture, and home directory.
func Probe(ctx context.Context, c *Conn) (*Room, error) {
	var out bytes.Buffer
	code, stderr, err := c.Run(ctx, `uname -s -m && printf '%s\n' "$HOME"`, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	if code != 0 {
		out.Reset()
		if code, _, err := c.Run(ctx, `cmd /c echo windows %PROCESSOR_ARCHITECTURE%`, nil, &out); err == nil && code == 0 {
			return nil, errors.New("probe: the room runs Windows; Windows rooms are not supported yet (milestone M6)")
		}
		return nil, fmt.Errorf("probe: uname exited with code %d: %s", code, strings.TrimSpace(stderr))
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		return nil, fmt.Errorf("probe: unexpected output %q", out.String())
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 {
		return nil, fmt.Errorf("probe: unexpected uname output %q", lines[0])
	}
	r := &Room{Conn: c, Home: strings.TrimSpace(lines[1])}
	switch fields[0] {
	case "Linux":
		r.OS = "linux"
	case "Darwin":
		r.OS = "macos"
	default:
		return nil, fmt.Errorf("probe: unsupported OS %q", fields[0])
	}
	switch fields[1] {
	case "x86_64", "amd64":
		r.Arch = "amd64"
	case "aarch64", "arm64":
		r.Arch = "arm64"
	default:
		return nil, fmt.Errorf("probe: unsupported architecture %q", fields[1])
	}
	if !strings.HasPrefix(r.Home, "/") {
		return nil, fmt.Errorf("probe: home directory %q is not absolute", r.Home)
	}
	return r, nil
}

// ErrNoHelper: the helper is not installed on the room.
var ErrNoHelper = errors.New("the helper is not installed")

// Helper runs a helper control command. in (if not nil) is sent as JSON on
// stdin, unless it is an io.Reader; out (if not nil) receives the JSON output.
func (r *Room) Helper(ctx context.Context, args []string, in, out any) error {
	cmd := Quote(r.HelperPath()) + " room"
	for _, a := range args {
		cmd += " " + Quote(a)
	}
	var stdin io.Reader
	switch v := in.(type) {
	case nil:
	case io.Reader:
		stdin = v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		stdin = bytes.NewReader(data)
	}
	var stdout bytes.Buffer
	code, stderr, err := r.Conn.Run(ctx, cmd, stdin, &stdout)
	if err != nil {
		return fmt.Errorf("helper %s: %w", args[0], err)
	}
	switch code {
	case 0:
		if out != nil {
			if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
				return fmt.Errorf("helper %s: decode output: %w", args[0], err)
			}
		}
		return nil
	case 126, 127:
		return ErrNoHelper
	}
	var he protocol.HelperError
	if json.Unmarshal(stdout.Bytes(), &he) == nil && he.Error != "" {
		return fmt.Errorf("helper %s: %s", args[0], he.Error)
	}
	return fmt.Errorf("helper %s: exit code %d: %s", args[0], code, strings.TrimSpace(stderr))
}

// Info returns the helper info, with empty hashes if the helper is missing.
func (r *Room) Info(ctx context.Context) (protocol.HelperInfo, error) {
	var info protocol.HelperInfo
	err := r.Helper(ctx, []string{"info"}, nil, &info)
	if errors.Is(err, ErrNoHelper) {
		return protocol.HelperInfo{Hashes: protocol.HelperHashes{Platforms: map[string]string{}}}, nil
	}
	return info, err
}

// Platform is a platform directory to upload: platform.toml and the scripts for
// the room's OS.
type Platform struct {
	Dir   string
	Files []string
	Hash  string
}

// PlatformUpload selects the files of a platform directory for an OS and
// computes their hash. A platform without files has the empty hash.
func PlatformUpload(dir string, files []string) (Platform, error) {
	p := Platform{Dir: dir, Files: files}
	if len(files) == 0 {
		return p, nil
	}
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	entries, err := hashes.DirEntries(dir, func(n string) bool { return set[n] })
	if err != nil {
		return p, fmt.Errorf("read platform directory %s: %w", dir, err)
	}
	p.Hash = hashes.Dir(entries)
	return p, nil
}

// Upload is what the server installs on a room.
type Upload struct {
	HelperFile string
	MiseFile   string
	MiseConfig []byte
	RoomEnv    []byte
	Platforms  map[string]Platform
}

// Sync uploads each item whose hash differs from the room (design, section 8).
// It returns the helper hash.
func (r *Room) Sync(ctx context.Context, u Upload) (string, error) {
	info, err := r.Info(ctx)
	if err != nil {
		return "", err
	}
	helperSum, err := cachedFileHash(u.HelperFile)
	if err != nil {
		return "", err
	}
	if info.Hashes.Helper != helperSum {
		if err := r.uploadHelper(ctx, u.HelperFile); err != nil {
			return "", err
		}
		if info, err = r.Info(ctx); err != nil {
			return "", err
		}
		if info.Hashes.Helper != helperSum {
			return "", fmt.Errorf("upload helper: hash on the room is %q, want %s", info.Hashes.Helper, helperSum)
		}
	}
	miseSum, err := cachedFileHash(u.MiseFile)
	if err != nil {
		return "", err
	}
	if info.Hashes.Mise != miseSum {
		if err := r.uploadMise(ctx, u.MiseFile, miseSum); err != nil {
			return "", err
		}
	}
	for _, item := range []struct {
		name string
		data []byte
		have string
	}{
		{"mise-config", u.MiseConfig, info.Hashes.MiseConfig},
		{"room-env", u.RoomEnv, info.Hashes.RoomEnv},
	} {
		if sum := hashes.Bytes(item.data); sum != item.have {
			if err := r.Helper(ctx, []string{"install", item.name, "--sha256", sum}, bytes.NewReader(item.data), nil); err != nil {
				return "", err
			}
		}
	}
	for name, p := range u.Platforms {
		have := info.Hashes.Platforms[name]
		if have == p.Hash {
			continue
		}
		if p.Hash == "" {
			// Remove the files of an earlier version with an empty upload.
			p.Hash = hashes.Dir(nil)
		}
		if err := r.uploadPlatform(ctx, name, p); err != nil {
			return "", err
		}
	}
	return helperSum, nil
}

func (r *Room) uploadHelper(ctx context.Context, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open helper: %w", err)
	}
	defer f.Close()
	dir := Quote(r.Home + "/hostel/bin")
	tmp := Quote(r.Home + "/hostel/bin/.hostel.upload")
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && chmod 755 %s && mv %s %s", dir, tmp, tmp, tmp, Quote(r.HelperPath()))
	code, stderr, err := r.Conn.Run(ctx, cmd, f, io.Discard)
	if err != nil {
		return fmt.Errorf("upload helper: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("upload helper: exit code %d: %s", code, strings.TrimSpace(stderr))
	}
	return nil
}

// uploadMise sends the mise binary compressed with gzip; the helper
// decompresses it and checks the hash.
func (r *Room) uploadMise(ctx context.Context, file, sum string) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open mise: %w", err)
	}
	defer f.Close()
	pr, pw := io.Pipe()
	go func() {
		gz, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
		_, err := io.Copy(gz, f)
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
		pw.CloseWithError(err)
	}()
	err = r.Helper(ctx, []string{"install", "mise", "--sha256", sum}, pr, nil)
	pr.Close()
	return err
}

func (r *Room) uploadPlatform(ctx context.Context, name string, p Platform) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range p.Files {
		path := filepath.Join(p.Dir, f)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: f, Mode: int64(info.Mode().Perm()), Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return r.Helper(ctx, []string{"install", "platform", name, "--sha256", p.Hash}, &buf, nil)
}

// RunScript runs a platform script step.
func (r *Room) RunScript(ctx context.Context, platform, step, slot string, req protocol.RunScriptRequest) (protocol.RunScriptResponse, error) {
	args := []string{"run-script", platform, step}
	if slot != "" {
		args = append(args, "--slot", slot)
	}
	var resp protocol.RunScriptResponse
	err := r.Helper(ctx, args, req, &resp)
	return resp, err
}

var hashCache struct {
	sync.Mutex
	m map[string]cachedHash
}

type cachedHash struct {
	size, mtime int64
	sum         string
}

// cachedFileHash hashes a large local file once per size and modification time.
// The files are the server's own helper and mise binaries.
func cachedFileHash(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	hashCache.Lock()
	defer hashCache.Unlock()
	if c, ok := hashCache.m[path]; ok && c.size == info.Size() && c.mtime == info.ModTime().UnixNano() {
		return c.sum, nil
	}
	sum, err := hashes.File(path)
	if err != nil {
		return "", err
	}
	if hashCache.m == nil {
		hashCache.m = map[string]cachedHash{}
	}
	hashCache.m[path] = cachedHash{size: info.Size(), mtime: info.ModTime().UnixNano(), sum: sum}
	return sum, nil
}

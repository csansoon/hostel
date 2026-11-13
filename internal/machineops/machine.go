package machineops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/hashes"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/shell"
)

// Machine is a connected machine with its probe result.
type Machine struct {
	Conn  *Conn
	OS    string // Hostel OS name
	Arch  string // Go architecture name
	Home  string
	Shell string // the SSH server's shell (shell.SH on macOS and Linux)
	// SharedHome is the machine user's home when the connection is a lease
	// user's (M11): its mise uses the machine's installs, read-only.
	SharedHome string
}

// HelperPath returns the absolute helper path on the machine.
func (r *Machine) HelperPath() string { return HelperPath(r.OS, r.Home) }

// Path returns a path below the hostel directory of a machine user's home
// directory, with the separator of the machine's OS.
func Path(osName, home string, elem ...string) string {
	sep := "/"
	if osName == config.OSWindows {
		sep = `\`
	}
	return home + sep + strings.Join(append([]string{"hostel"}, elem...), sep)
}

// HelperPath returns the helper path in a machine user's home directory.
func HelperPath(osName, home string) string {
	if osName == config.OSWindows {
		return Path(osName, home, "bin", "hostel.exe")
	}
	return Path(osName, home, "bin", "hostel")
}

// WorkDir returns the work directory of a room in a machine user's home directory.
func WorkDir(osName, home, room string) string { return Path(osName, home, "rooms", room, "work") }

// Probe finds the machine's OS, architecture, home directory, and shell. On
// Windows, "cmd /c echo" runs the same in cmd and in PowerShell; "echo %OS%"
// does not: cmd expands the variable, PowerShell prints it unchanged.
func Probe(ctx context.Context, c *Conn) (*Machine, error) {
	var out bytes.Buffer
	code, stderr, err := c.Run(ctx, `uname -s -m && printf '%s\n' "$HOME"`, nil, &out)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	if code != 0 {
		out.Reset()
		if code, _, err := c.Run(ctx, `cmd /c echo windows %PROCESSOR_ARCHITECTURE% %USERPROFILE%`, nil, &out); err == nil && code == 0 {
			return probeWindows(ctx, c, out.String())
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
	r := &Machine{Conn: c, Home: strings.TrimSpace(lines[1])}
	switch fields[0] {
	case "Linux":
		r.OS = "linux"
	case "Darwin":
		r.OS = "macos"
	default:
		return nil, fmt.Errorf("probe: unsupported OS %q", fields[0])
	}
	if r.Arch, err = goArch(fields[1]); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(r.Home, "/") {
		return nil, fmt.Errorf("probe: home directory %q is not absolute", r.Home)
	}
	return r, nil
}

func probeWindows(ctx context.Context, c *Conn, out string) (*Machine, error) {
	line := strings.TrimSpace(out)
	fields := strings.SplitN(line, " ", 3)
	if len(fields) != 3 || fields[0] != "windows" {
		return nil, fmt.Errorf("probe: unexpected output %q", line)
	}
	r := &Machine{Conn: c, OS: config.OSWindows, Home: strings.TrimSpace(fields[2])}
	var err error
	if r.Arch, err = goArch(fields[1]); err != nil {
		return nil, err
	}
	if len(r.Home) < 3 || r.Home[1] != ':' || r.Home[2] != '\\' {
		return nil, fmt.Errorf("probe: home directory %q is not absolute", r.Home)
	}
	var echo bytes.Buffer
	if code, stderr, err := c.Run(ctx, `echo %OS%`, nil, &echo); err != nil || code != 0 {
		return nil, fmt.Errorf("probe: find the shell: exit code %d, %v: %s", code, err, strings.TrimSpace(stderr))
	}
	r.Shell = shell.PowerShell
	if strings.TrimSpace(echo.String()) != "%OS%" {
		r.Shell = shell.Cmd
	}
	return r, nil
}

func goArch(s string) (string, error) {
	switch strings.ToLower(s) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("probe: unsupported architecture %q", s)
}

// ErrNoHelper: the helper is not installed on the machine.
var ErrNoHelper = errors.New("the helper is not installed")

// Helper runs a helper control command. in (if not nil) is sent as JSON on
// stdin, unless it is an io.Reader; out (if not nil) receives the JSON output.
func (r *Machine) Helper(ctx context.Context, args []string, in, out any) error {
	cmd, err := shell.Command(r.Shell, r.HelperPath(), append([]string{"helper"}, args...)...)
	if err != nil {
		return fmt.Errorf("helper %s: %w", args[0], err)
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
	}
	// A binary before M10g has no "helper" command: the server replaces it.
	if shell.NotFound(r.Shell, code) || strings.Contains(stderr, `unknown command "helper"`) {
		return ErrNoHelper
	}
	var he protocol.HelperError
	if json.Unmarshal(stdout.Bytes(), &he) == nil && he.Error != "" {
		return fmt.Errorf("helper %s: %s", args[0], he.Error)
	}
	return fmt.Errorf("helper %s: exit code %d: %s", args[0], code, strings.TrimSpace(stderr))
}

// Info returns the helper info, with empty hashes if the helper is missing.
func (r *Machine) Info(ctx context.Context) (protocol.HelperInfo, error) {
	var info protocol.HelperInfo
	err := r.Helper(ctx, []string{"info"}, nil, &info)
	if errors.Is(err, ErrNoHelper) {
		return protocol.HelperInfo{Hashes: protocol.HelperHashes{Tools: map[string]string{}}}, nil
	}
	return info, err
}

// Tool is a tool directory to upload: tool.toml and the scripts for the
// machine's OS.
type Tool struct {
	Dir   string
	Files []string
	Hash  string
}

// ToolUpload selects the files of a tool directory for an OS and computes
// their hash. A tool without files has the empty hash. The hash of a Windows
// machine has no file modes: Windows has none.
func ToolUpload(dir string, files []string, osName string) (Tool, error) {
	p := Tool{Dir: dir, Files: files}
	if len(files) == 0 {
		return p, nil
	}
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	entries, err := hashes.DirEntries(dir, func(n string) bool { return set[n] }, osName != config.OSWindows)
	if err != nil {
		return p, fmt.Errorf("read tool directory %s: %w", dir, err)
	}
	p.Hash = hashes.Dir(entries)
	return p, nil
}

// Upload is what the server installs on a machine.
type Upload struct {
	HelperFile string
	MiseFile   string
	MiseConfig []byte
	MachineEnv []byte
	Tools      map[string]Tool
}

// Sync uploads each item whose hash differs from the machine (design, section 8).
// It returns the helper hash.
func (r *Machine) Sync(ctx context.Context, u Upload) (string, error) {
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
			return "", fmt.Errorf("upload helper: hash on the machine is %q, want %s", info.Hashes.Helper, helperSum)
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
		{"machine-env", u.MachineEnv, info.Hashes.MachineEnv},
	} {
		if sum := hashes.Bytes(item.data); sum != item.have {
			if err := r.Helper(ctx, []string{"install", item.name, "--sha256", sum}, bytes.NewReader(item.data), nil); err != nil {
				return "", err
			}
		}
	}
	// A tool that the machine no longer lists leaves the machine.
	for name := range info.Hashes.Tools {
		if _, ok := u.Tools[name]; !ok && config.ValidName(name) {
			if err := r.uploadTool(ctx, name, Tool{Hash: hashes.Dir(nil)}); err != nil {
				return "", err
			}
		}
	}
	for name, p := range u.Tools {
		have := info.Hashes.Tools[name]
		if have == p.Hash {
			continue
		}
		if p.Hash == "" {
			// Remove the files of an earlier version with an empty upload.
			p.Hash = hashes.Dir(nil)
		}
		if err := r.uploadTool(ctx, name, p); err != nil {
			return "", err
		}
	}
	return helperSum, nil
}

func (r *Machine) uploadHelper(ctx context.Context, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open helper: %w", err)
	}
	defer f.Close()
	if r.OS == config.OSWindows {
		return r.uploadHelperWindows(ctx, f)
	}
	dir := Quote(r.Home + "/hostel/bin")
	tmp := Quote(r.Home + "/hostel/bin/.hostel.upload")
	cmd := fmt.Sprintf("mkdir -p %s && cat > %s && chmod 755 %s && mv %s %s", dir, tmp, tmp, tmp, Quote(r.HelperPath()))
	return r.run(ctx, "upload helper", cmd, f)
}

// uploadHelperWindows sends the helper as a tar stream to tar.exe (part of
// Windows since Windows 10 1803): Windows PowerShell does not give its stdin to
// a script. Then PowerShell moves it into place. Windows cannot replace a
// running executable (a job supervisor of another room), but it can rename
// it: the old file moves aside and is deleted at a later upload.
func (r *Machine) uploadHelperWindows(ctx context.Context, f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("upload helper: %w", err)
	}
	bin := Path(r.OS, r.Home, "bin")
	tmp := Path(r.OS, r.Home, "bin", ".upload")
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	prepare := "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'\n" +
		"Remove-Item -Recurse -Force -ErrorAction SilentlyContinue " + q(tmp) + "\n" +
		"New-Item -ItemType Directory -Force " + q(tmp) + " | Out-Null\n"
	finish := "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'\n" +
		"$dst = " + q(r.HelperPath()) + "; $dir = " + q(bin) + "\n" +
		"Get-ChildItem $dir -Filter '.hostel.exe.old-*' -Force | Remove-Item -Force -ErrorAction SilentlyContinue\n" +
		"try { Move-Item -Force " + q(tmp+`\hostel.exe`) + " $dst }\n" +
		"catch { Move-Item -Force $dst (Join-Path $dir ('.hostel.exe.old-' + [guid]::NewGuid())); Move-Item -Force " + q(tmp+`\hostel.exe`) + " $dst }\n" +
		"Remove-Item -Recurse -Force " + q(tmp) + "\n"
	cmd, err := r.powerShell(prepare)
	if err != nil {
		return err
	}
	if err := r.run(ctx, "upload helper", cmd, nil); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{Name: "hostel.exe", Mode: 0o755, Size: info.Size(), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	cmd, err = shell.Command(r.Shell, "tar", "-x", "-f", "-", "-C", tmp)
	if err == nil {
		err = r.run(ctx, "upload helper", cmd, pr)
	}
	pr.Close()
	if err != nil {
		return err
	}
	if cmd, err = r.powerShell(finish); err != nil {
		return err
	}
	return r.run(ctx, "upload helper", cmd, nil)
}

// powerShell returns a command line that runs a Windows PowerShell script.
func (r *Machine) powerShell(script string) (string, error) {
	return shell.Command(r.Shell, "powershell", "-NoProfile", "-NonInteractive", "-EncodedCommand", encodePS(script))
}

func (r *Machine) run(ctx context.Context, what, cmd string, stdin io.Reader) error {
	code, stderr, err := r.Conn.Run(ctx, cmd, stdin, io.Discard)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if code != 0 {
		return fmt.Errorf("%s: exit code %d: %s", what, code, strings.TrimSpace(stderr))
	}
	return nil
}

// encodePS encodes a script for powershell -EncodedCommand (UTF-16LE, base64).
func encodePS(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// uploadMise sends the mise binary compressed with gzip; the helper
// decompresses it and checks the hash.
func (r *Machine) uploadMise(ctx context.Context, file, sum string) error {
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

func (r *Machine) uploadTool(ctx context.Context, name string, p Tool) error {
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
	return r.Helper(ctx, []string{"install", "tool", name, "--sha256", p.Hash}, &buf, nil)
}

// RunScript runs a tool script step.
func (r *Machine) RunScript(ctx context.Context, tool, step, room string, req protocol.RunScriptRequest) (protocol.RunScriptResponse, error) {
	args := []string{"run-script", tool, step}
	if room != "" {
		args = append(args, "--room", room)
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

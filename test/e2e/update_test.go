//go:build e2e

package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/update"
)

var (
	versionBinsMu sync.Mutex
	versionBins   = map[string]string{}
)

// versionBin builds hostel for an OS and architecture with a version (once
// for each run of the suite).
func versionBin(t *testing.T, goos, goarch, ver string) string {
	t.Helper()
	versionBinsMu.Lock()
	defer versionBinsMu.Unlock()
	key := goos + "/" + goarch + "/" + ver
	if bin, ok := versionBins[key]; ok {
		return bin
	}
	bin := filepath.Join(binDir, ver, update.BinaryName(goos, goarch))
	build := exec.Command("go", "build", "-ldflags", "-X github.com/csansoon/hostel/internal/version.Version="+ver, "-o", bin, "../../cmd/hostel")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hostel %s: %v\n%s", key, err, out)
	}
	versionBins[key] = bin
	return bin
}

// hostBin is versionBin for this computer.
func hostBin(t *testing.T, ver string) string {
	return versionBin(t, runtime.GOOS, runtime.GOARCH, ver)
}

// fakeRelease is a release source as GitHub serves it: <url>/latest redirects
// to <url>/tag/<latest>; <url>/download/<tag>/<file> are the files, with
// SHA256SUMS and SHA256SUMS.sig (signed with the test key).
type fakeRelease struct {
	t      *testing.T
	srv    *httptest.Server
	key    ed25519.PrivateKey
	mu     sync.Mutex
	latest string
	files  map[string]map[string][]byte // tag → file → bytes
	// served, when set, replaces a file's bytes after SHA256SUMS (a changed
	// download).
	served map[string][]byte
	// badSig signs another text.
	badSig bool
}

func newFakeRelease(t *testing.T) *fakeRelease {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRelease{t: t, key: key, files: map[string]map[string][]byte{}, served: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// use makes the guest commands (and the server) of e use the source.
func (f *fakeRelease) use(e *env) {
	e.vars["HOSTEL_UPDATE_URL"] = f.srv.URL
	e.vars["HOSTEL_UPDATE_KEY"] = base64.StdEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey))
}

// add publishes a release with files (release name → local file) and makes it
// the latest.
func (f *fakeRelease) add(tag string, files map[string]string) {
	f.t.Helper()
	m := map[string][]byte{}
	for name, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			f.t.Fatal(err)
		}
		m[name] = data
	}
	f.mu.Lock()
	f.files[tag], f.latest = m, tag
	f.mu.Unlock()
}

// set changes the source while it serves.
func (f *fakeRelease) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

func (f *fakeRelease) sums(tag string) []byte {
	var names []string
	for n := range f.files[tag] {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		h := sha256.Sum256(f.files[tag][n])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	return []byte(b.String())
}

func (f *fakeRelease) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/latest" {
		http.Redirect(w, r, "/tag/"+f.latest, http.StatusFound)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/download/"), "/")
	if len(parts) != 2 || f.files[parts[0]] == nil {
		http.NotFound(w, r)
		return
	}
	tag, name := parts[0], parts[1]
	switch name {
	case "SHA256SUMS":
		w.Write(f.sums(tag))
	case "SHA256SUMS.sig":
		text := update.SignedText(tag, f.sums(tag))
		if f.badSig {
			text = append(text, '!')
		}
		w.Write([]byte(base64.StdEncoding.EncodeToString(ed25519.Sign(f.key, text)) + "\n"))
	default:
		if data, ok := f.served[name]; ok {
			w.Write(data)
			return
		}
		data, ok := f.files[tag][name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}
}

// copyBin copies a binary to dir/hostel and returns the copy.
func copyBin(t *testing.T, src, dir string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "hostel")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

func binVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("%s --version: %v", bin, err)
	}
	return strings.TrimSpace(string(out))
}

// TestUpdate: hostel update checks the signature and the checksum, replaces
// the running binary, and refuses a release that is not newer.
func TestUpdate(t *testing.T) {
	e := newEnv(t)
	rel := newFakeRelease(t)
	rel.use(e)
	name := update.BinaryName(runtime.GOOS, runtime.GOARCH)
	rel.add("v1.1.0", map[string]string{name: hostBin(t, "v1.1.0")})
	e.bin = copyBin(t, hostBin(t, "v1.0.0"), filepath.Join(e.home, "bin"))

	r := e.mustHostel("update", "--check")
	if !strings.Contains(r.stdout, "Hostel v1.1.0 is available (this computer has v1.0.0)") {
		t.Fatalf("update --check: %s", r)
	}

	rel.set(func() { rel.badSig = true })
	r = e.hostel("update")
	if r.code != 1 || !strings.Contains(r.stderr, "the signature of release v1.1.0 is not valid") {
		t.Fatalf("update with a wrong signature: %s", r)
	}
	rel.set(func() { rel.badSig, rel.served[name] = false, []byte("not the release binary") })
	r = e.hostel("update")
	if r.code != 1 || !strings.Contains(r.stderr, "the checksum of "+name+" is not the one of release v1.1.0") {
		t.Fatalf("update with a changed binary: %s", r)
	}
	rel.set(func() { delete(rel.served, name) })
	if v := binVersion(t, e.bin); v != "hostel v1.0.0" {
		t.Fatalf("after the refused updates, the binary is %q", v)
	}
	if left, _ := filepath.Glob(filepath.Join(e.home, "bin", ".*")); len(left) > 0 {
		t.Fatalf("the refused updates left files: %v", left)
	}

	r = e.mustHostel("update")
	if !strings.Contains(r.stdout, "from v1.0.0 to v1.1.0") {
		t.Fatalf("update: %s", r)
	}
	if v := binVersion(t, e.bin); v != "hostel v1.1.0" {
		t.Fatalf("after the update, the binary is %q", v)
	}
	r = e.mustHostel("update")
	if !strings.Contains(r.stdout, "Hostel v1.1.0 is up to date") {
		t.Fatalf("update on the latest version: %s", r)
	}
	r = e.hostel("update", "--version", "v1.0.0")
	if r.code != 1 || !strings.Contains(r.stderr, "v1.0.0 is not newer") {
		t.Fatalf("update to an older version: %s", r)
	}

	e.vars["HOSTEL_UPDATE_URL"] = "http://127.0.0.1:1"
	r = e.hostel("update", "--check")
	if r.code != 69 || !strings.Contains(r.stderr, "cannot be reached") {
		t.Fatalf("update without a release source: %s", r)
	}
}

// TestUpdateNotice: a guest that is older than the server gets one notice in
// 24 hours; a guest older than the minimum version gets HTTP 426.
func TestUpdateNotice(t *testing.T) {
	e := newEnv(t)
	e.serverBin = hostBin(t, "v1.1.0")
	e.bin = e.serverBin
	e.initServer(serverHeader())
	s := e.startServer("")

	e.bin = hostBin(t, "v1.0.0")
	r := e.mustHostel("platforms")
	if !strings.Contains(r.stderr, `the server runs Hostel v1.1.0 and this computer has v1.0.0; run "hostel update"`) {
		t.Fatalf("no notice: %s", r)
	}
	if r = e.mustHostel("platforms"); strings.Contains(r.stderr, "hostel update") {
		t.Fatalf("a second notice within 24 hours: %s", r)
	}
	e.bin = hostBin(t, "v1.1.0")
	if r = e.mustHostel("platforms"); strings.Contains(r.stderr, "hostel update") {
		t.Fatalf("a notice for the same version: %s", r)
	}

	req, err := http.NewRequest(http.MethodGet, s.url+"/v1/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set(protocol.VersionHeader, "v0.0.9")
	resp, err := insecureClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired || resp.Header.Get(protocol.VersionHeader) != "v1.1.0" {
		t.Fatalf("a guest v0.0.9: HTTP %d, server version %q", resp.StatusCode, resp.Header.Get(protocol.VersionHeader))
	}
}

// ownHelperDir is a helper_dir for one test: a copy of the hostel helper and a
// link to the shared mise.
func ownHelperDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "hostel-linux-" + dockerArch
	data, err := os.ReadFile(filepath.Join(helperDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	mise, err := filepath.EvalSymlinks(filepath.Join(helperDir, "mise-linux-"+dockerArch))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(mise, filepath.Join(dir, "mise-linux-"+dockerArch)); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestServerUpdate: an admin updates the server from a computer (--remote);
// the server replaces its binary and helper_dir and starts the new binary in
// the same process, on the same ports; a ready lease continues. A member
// cannot; an update that is not newer is refused. Then the local command on
// the server machine.
func TestServerUpdate(t *testing.T) {
	e := newEnv(t)
	e.init()
	rel := newFakeRelease(t)
	rel.use(e)
	own := ownHelperDir(t, filepath.Join(e.home, "opt", "helpers"))
	e.serverBin = copyBin(t, hostBin(t, "v1.0.0"), filepath.Join(e.home, "opt", "bin"))
	f := newFixtureWithEnv(t, e, &fixtureOpts{helperDir: own})
	f.acquire()
	f.s.pinPorts()
	pid := f.s.cmd.Process.Pid

	linux := "hostel-linux-" + dockerArch
	newHelper := versionBin(t, "linux", dockerArch, "v1.1.0")
	rel.add("v1.1.0", map[string]string{
		update.BinaryName(runtime.GOOS, runtime.GOARCH): hostBin(t, "v1.1.0"),
		linux: newHelper,
	})

	r := e.hostel("server", "update", "--remote")
	if r.code != 1 || !strings.Contains(r.stderr, "only an admin") {
		t.Fatalf("server update --remote as a member: %s", r)
	}
	member := e.vars["HOSTEL_TOKEN"]
	e.vars["HOSTEL_TOKEN"] = e.admin
	r = e.mustHostel("server", "update", "--remote")
	if !strings.Contains(r.stdout, "the server updates from v1.0.0 to v1.1.0") || !strings.Contains(r.stdout, "the server runs v1.1.0") {
		t.Fatalf("server update --remote: %s", r)
	}
	select {
	case <-f.s.done:
		t.Fatalf("the server process ended:\n%s", f.s.log.String())
	default:
	}
	if f.s.cmd.Process.Pid != pid || binVersion(t, e.serverBin) != "hostel v1.1.0" {
		t.Fatalf("after the update: pid %d (was %d), binary %q", f.s.cmd.Process.Pid, pid, binVersion(t, e.serverBin))
	}
	if update.FileSHA256(filepath.Join(own, linux)) != update.FileSHA256(newHelper) {
		t.Fatal("helper_dir does not have the new helper")
	}
	r = e.hostel("server", "update", "--remote")
	if r.code != 1 || !strings.Contains(r.stderr, "the server has v1.1.0; v1.1.0 is not newer") {
		t.Fatalf("a second server update: %s", r)
	}

	e.vars["HOSTEL_TOKEN"] = member
	if r := e.mustHostel("run", "--", "echo still here"); !strings.Contains(r.stdout, "still here") {
		t.Fatalf("the lease after the update: %s", r)
	}
	e.mustHostel("release")
	f.waitSlotFree()
	// The next lease gets the new helper.
	f.acquire()
	if v := f.c.sh("/home/hostel/hostel/bin/hostel --version"); v != "hostel v1.1.0" {
		t.Fatalf("the room's helper after the next lease: %q", v)
	}
	e.mustHostel("release")

	// On the server machine: the local command, with the server binary.
	f.s.stop()
	rel.add("v1.2.0", map[string]string{
		update.BinaryName(runtime.GOOS, runtime.GOARCH): hostBin(t, "v1.2.0"),
		linux: versionBin(t, "linux", dockerArch, "v1.2.0"),
	})
	e.bin = e.serverBin
	r = e.mustHostel("server", "update")
	if !strings.Contains(r.stdout, "from v1.1.0 to v1.2.0") || !strings.Contains(r.stdout, "restart the server") {
		t.Fatalf("server update: %s", r)
	}
	if binVersion(t, e.serverBin) != "hostel v1.2.0" {
		t.Fatalf("after the local update, the server binary is %q", binVersion(t, e.serverBin))
	}
}

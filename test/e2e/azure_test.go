//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

const (
	azureTenant = "11111111-2222-3333-4444-555555555555"
	azureClient = "66666666-7777-8888-9999-000000000000"
	azureSecret = "e2e-client-secret-Zq8~value"
	azureVM     = "/subscriptions/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee/resourceGroups/rg-e2e/providers/Microsoft.Compute/virtualMachines/box"
)

// fakeAzure is the Azure API at its adapter boundary (sign-in, start,
// deallocate, instance view, operation status) for one virtual machine: a
// machine container that "start" and "deallocate" start and stop.
type fakeAzure struct {
	t   *testing.T
	srv *httptest.Server
	c   *container

	mu          sync.Mutex
	failStart   bool
	off         bool // a stopped container is "stopped" (shut down inside, still billed), not "deallocated"
	hung        bool // "running" while the container is stopped (it runs but does not answer)
	starts      int
	deallocates int
	views       int               // instanceView requests
	busy        string            // "starting" or "deallocating" while an operation runs
	ops         map[string]string // operation ID → InProgress, Succeeded, Failed
	tokens      map[string]bool
}

func newFakeAzure(t *testing.T, c *container) *fakeAzure {
	f := &fakeAzure{t: t, c: c, ops: map[string]string{}, tokens: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAzure) providerKeys() string {
	return fmt.Sprintf("tenant = %q\nclient_id = %q\nlogin_url = %q\nmanagement_url = %q\n", azureTenant, azureClient, f.srv.URL, f.srv.URL)
}

func (f *fakeAzure) counts() (starts, deallocates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.deallocates
}

func (f *fakeAzure) viewCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.views
}

func (f *fakeAzure) isOff() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.off
}

func (f *fakeAzure) setOff(v bool) {
	f.mu.Lock()
	f.off = v
	f.mu.Unlock()
}

func (f *fakeAzure) setHung(v bool) {
	f.mu.Lock()
	f.hung = v
	f.mu.Unlock()
}

func (f *fakeAzure) setFailStart(v bool) {
	f.mu.Lock()
	f.failStart = v
	f.mu.Unlock()
}

func azureJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeAzure) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && r.URL.Path == "/"+azureTenant+"/oauth2/v2.0/token" {
		r.ParseForm()
		if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_id") != azureClient ||
			r.PostForm.Get("scope") != f.srv.URL+"/.default" || r.PostForm.Get("client_secret") != azureSecret {
			azureJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client",
				"error_description": "AADSTS7000215: Invalid client secret provided.\r\nTrace ID: 0000\r\nCorrelation ID: 0000"})
			return
		}
		f.mu.Lock()
		tok := fmt.Sprintf("token-%d", len(f.tokens))
		f.tokens[tok] = true
		f.mu.Unlock()
		azureJSON(w, http.StatusOK, map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": tok})
		return
	}
	f.mu.Lock()
	ok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	f.mu.Unlock()
	if !ok {
		azureJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "InvalidAuthenticationToken", "message": "no valid token"}})
		return
	}
	if r.URL.Query().Get("api-version") == "" {
		azureJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "MissingApiVersionParameter", "message": "api-version"}})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == azureVM+"/instanceView":
		f.mu.Lock()
		f.views++
		state := f.busy
		f.mu.Unlock()
		if state == "" {
			state = "deallocated"
			if f.running() || f.hung {
				state = "running"
			} else if f.isOff() {
				state = "stopped"
			}
		}
		azureJSON(w, http.StatusOK, map[string]any{"statuses": []map[string]string{
			{"code": "ProvisioningState/succeeded"}, {"code": "PowerState/" + state}}})
	case r.Method == http.MethodPost && (r.URL.Path == azureVM+"/start" || r.URL.Path == azureVM+"/deallocate"):
		start := strings.HasSuffix(r.URL.Path, "/start")
		f.mu.Lock()
		id := fmt.Sprintf("op-%d", len(f.ops))
		f.ops[id] = "InProgress"
		fail := start && f.failStart
		if start {
			f.starts++
			f.busy = "starting"
		} else {
			f.deallocates++
			f.busy = "deallocating"
		}
		f.mu.Unlock()
		go f.run(id, start, fail)
		w.Header().Set("Azure-AsyncOperation", f.srv.URL+"/providers/Microsoft.Compute/locations/e2e/operations/"+id+"?api-version=2024-07-01")
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/providers/Microsoft.Compute/locations/e2e/operations/"):
		f.mu.Lock()
		st, found := f.ops[filepath.Base(r.URL.Path)]
		f.mu.Unlock()
		if !found {
			azureJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "NotFound", "message": "no operation"}})
			return
		}
		body := map[string]any{"status": st}
		if st == "Failed" {
			body["error"] = map[string]string{"code": "AllocationFailed", "message": "No capacity for the size in this region (e2e)."}
		}
		azureJSON(w, http.StatusOK, body)
	default:
		azureJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "ResourceNotFound", "message": r.Method + " " + r.URL.Path}})
	}
}

// run does an operation: the container starts or stops. As in Azure, a start
// ends before the SSH server answers (the machine still boots).
func (f *fakeAzure) run(id string, start, fail bool) {
	status := "Succeeded"
	switch {
	case fail:
		status = "Failed"
	case start:
		if _, err := docker("start", f.c.name); err != nil {
			status = "Failed"
		}
	default:
		if _, err := docker("stop", "-t", "1", f.c.name); err != nil {
			status = "Failed"
		}
	}
	f.mu.Lock()
	f.ops[id], f.busy = status, ""
	if !start && !fail {
		f.off, f.hung = false, false
	}
	f.mu.Unlock()
}

func (f *fakeAzure) running() bool {
	out, err := docker("inspect", "-f", "{{.State.Running}}", f.c.name)
	return err == nil && out == "true"
}

func azureConfig(f *fakeAzure, warm string) string {
	return serverHeader() + fmt.Sprintf(`
[providers.cloud]
kind = "azure"
%s
[providers.cloud.machines.box]
vm = %q
address = %q
os = "linux"
tools = ["probe"]
warm_running = %q
`, f.providerKeys(), azureVM, f.c.address(), warm)
}

func (e *env) writeProbeTool() {
	e.writeTool("probe", map[string]string{
		"tool.toml":        "description = \"Probe\"\n",
		"install.linux.sh": installScript,
		"check.linux.sh":   "#!/bin/sh\necho 1.0\n",
	})
}

// azureLogin gives the server the client secret with "hostel config providers
// login": a wrong secret is stored but Azure refuses it (the first line of
// the Azure message only); the right one works.
func azureLogin(t *testing.T, e *env) {
	t.Helper()
	r := e.adminHostel("config", "providers", "login", "cloud")
	if r.code != 2 || !strings.Contains(r.stderr, "no secret given") {
		t.Fatalf("providers login without a secret: %s", r)
	}
	saved := e.vars["HOSTEL_TOKEN"]
	e.vars["HOSTEL_TOKEN"] = e.admin
	r = e.hostelIn("wrong-secret\n", "config", "providers", "login", "cloud")
	if r.code != 1 || !strings.Contains(r.stderr, "Azure sign-in failed (HTTP 401): invalid_client AADSTS7000215: Invalid client secret provided.") ||
		strings.Contains(r.stderr, "Trace ID") {
		t.Fatalf("providers login with a wrong secret: %s", r)
	}
	r = e.hostelIn(azureSecret+"\n", "config", "providers", "login", "cloud")
	e.vars["HOSTEL_TOKEN"] = saved
	if r.code != 0 || !strings.Contains(r.stdout, "Azure accepted it") {
		t.Fatalf("providers login: %s", r)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(e.dataDir(), "provider-secrets"):          0o700 | os.ModeDir,
		filepath.Join(e.dataDir(), "provider-secrets", "cloud"): 0o600,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode() != want {
			t.Fatalf("%s: mode %v, want %v", path, st.Mode(), want)
		}
	}
}

// machineEvents are the event messages of the machine, as an admin sees them.
func (s *serverProc) machineEvents(key string) string {
	s.e.t.Helper()
	var d protocol.MachineDetails
	if code := s.get("/v1/machines/"+key, s.token, &d); code != 200 {
		s.e.t.Fatalf("GET /v1/machines/%s: HTTP %d", key, code)
	}
	var msgs []string
	for _, ev := range d.Events {
		msgs = append(msgs, ev.Message)
	}
	return strings.Join(msgs, "\n")
}

// TestAzure (M12): an azure machine is deallocated until a lease needs it.
// Without a client secret Hostel cannot read the power state; "providers
// login" stores the secret on the server only. A lease starts the machine,
// and after warm_running without a lease Hostel deallocates it; the next
// lease starts it again without a new install. A failed start makes the
// machine broken with the Azure message and is not tried again until repair.
// A change of the machine while it is deallocated applies at its next start.
// A drained stopped machine is not started. A machine that runs in Azure but
// does not answer is deallocated after warm_running and stays broken until a
// repair. The secret is not in the configuration, the events, or the server
// log.
func TestAzure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.createStaticMachine("box")
	az := newFakeAzure(t, c)
	e.writeProbeTool()
	e.writeConfig(azureConfig(az, "4s"))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.waitFor("the server says that the client secret is missing", time.Minute, func() bool {
		return strings.Contains(s.log.String(), `no client secret for provider cloud: run \"hostel config providers login cloud\"`)
	})
	azureLogin(t, e)
	s.waitMachine("cloud/box", protocol.MachineStopped, time.Minute)
	if r := e.hostel("status", "cloud/box"); r.code != 0 || !strings.Contains(r.stdout, "Azure, started when a lease needs it") {
		t.Fatalf("status cloud/box: %s", r)
	}
	// An unreachable idle machine can also be deallocated (warm_running): a
	// machine that was shut down inside is still billed.
	starts0, deallocates0 := az.counts()
	if starts0 != 0 {
		t.Fatalf("starts before a lease: %d", starts0)
	}

	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	f := &fixture{e: e, s: s, c: c}
	f.acquire("probe")
	if r := e.mustHostel("run", "--", "echo", "on-azure"); !strings.Contains(r.stdout, "on-azure") {
		t.Fatalf("run: %s", r)
	}
	e.mustHostel("release")
	s.waitMachine("cloud/box", protocol.MachineStopped, 2*time.Minute)
	if starts, deallocates := az.counts(); starts != 1 || deallocates != deallocates0+1 || az.running() {
		t.Fatalf("after warm_running: starts %d, deallocates %d, running %t; want 1, %d, false", starts, deallocates, az.running(), deallocates0+1)
	}

	f.acquire("probe")
	if got := strings.Count(c.sh("cat "+cacheDir+"/install.log"), "install probe"); got != 1 {
		t.Fatalf("installs of probe after a second start: %d, want 1", got)
	}
	e.mustHostel("release")
	s.waitMachine("cloud/box", protocol.MachineStopped, 2*time.Minute)

	// A change of the machine while it is deallocated: the next start
	// provisions with it (below, after the repair).
	doc := s.configDoc().Document
	if r := e.applyDoc(strings.Replace(doc, "[providers.cloud.machines.box]\n", "[providers.cloud.machines.box]\nenv = { CHANGED = \"1\" }\n", 1)); r.code != 0 {
		t.Fatalf("change the machine: %s", r)
	}

	// A failed start: broken, with the Azure message; no new start until repair.
	az.setFailStart(true)
	e.newRepoNamed("fail", defaultRepoFiles())
	if r := e.hostel("acquire", "probe", "--task", "start fails", "--wait", "40s"); r.code != 75 || !strings.Contains(r.stderr, "AllocationFailed") {
		t.Fatalf("acquire with a failing start: %s", r)
	}
	m := s.waitMachine("cloud/box", protocol.MachineBroken, time.Minute)
	if !strings.Contains(m.Reason, "start failed: start: Azure: AllocationFailed: No capacity for the size in this region (e2e).") {
		t.Fatalf("machine after the failed start: %+v", m)
	}
	if ev := s.machineEvents("cloud/box"); !strings.Contains(ev, "AllocationFailed") || strings.Contains(ev, azureSecret) {
		t.Fatalf("machine events after the failed start:\n%s", ev)
	}
	failed, _ := az.counts()
	// Two reconcile passes read the power state (deallocated); the machine
	// stays broken and is not started again.
	views := az.viewCount()
	e.waitFor("two reconcile passes", time.Minute, func() bool { return az.viewCount() >= views+2 })
	if starts, _ := az.counts(); starts != failed || s.machine("cloud/box").State != protocol.MachineBroken {
		t.Fatalf("after a failed start without repair: starts %d (want %d), machine %+v", starts, failed, s.machine("cloud/box"))
	}
	az.setFailStart(false)
	saved := e.vars["HOSTEL_TOKEN"]
	e.vars["HOSTEL_TOKEN"] = e.admin
	e.mustHostel("config", "machines", "repair", "cloud/box")
	e.vars["HOSTEL_TOKEN"] = saved
	f.acquire("probe")
	if r := e.mustHostel("run", "--", "printenv", "CHANGED"); strings.TrimSpace(r.stdout) != "1" {
		t.Fatalf("the changed env after the start: %s", r)
	}
	e.mustHostel("release")
	s.waitMachine("cloud/box", protocol.MachineStopped, 2*time.Minute)

	// Drained and stopped: not started.
	before, _ := az.counts()
	if r := e.adminHostel("config", "machines", "drain", "cloud/box"); r.code != 0 {
		t.Fatalf("drain: %s", r)
	}
	e.newRepoNamed("drained", defaultRepoFiles())
	if r := e.hostel("acquire", "probe", "--task", "drained", "--wait", "5s"); r.code != 75 {
		t.Fatalf("acquire with the machine drained: %s, want exit code 75", r)
	}
	if starts, _ := az.counts(); starts != before || s.machine("cloud/box").State != protocol.MachineStopped {
		t.Fatalf("a drained machine: starts %d (before %d), state %s", starts, before, s.machine("cloud/box").State)
	}
	e.mustHostel("release")

	// A machine joins an azure provider with its resource ID.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--provider", "cloud"}, "a machine of an azure provider needs --vm"},
		{[]string{"--provider", "home", "--vm", azureVM}, "a machine of an azure provider needs --vm"},
		{[]string{"--provider", "cloud", "--vm", "/subscriptions/x"}, "is not the resource ID of an Azure virtual machine"},
	} {
		if r := e.adminHostel(append([]string{"config", "machines", "add", "win"}, c.args...)...); r.code != 1 || !strings.Contains(r.stderr, c.want) {
			t.Fatalf("machines add win %v: %s", c.args, r)
		}
	}
	if r := e.adminHostel("config", "machines", "add", "win", "--provider", "cloud", "--vm", strings.Replace(azureVM, "/box", "/win", 1)); r.code != 0 ||
		!strings.Contains(r.stdout, "machine join") {
		t.Fatalf("machines add win --vm: %s", r)
	}
	if r := e.adminHostel("config", "providers", "add", "cloud2", "--kind", "azure", "--tenant", azureTenant, "--client-id", azureClient); r.code != 0 ||
		!strings.Contains(r.stdout, "hostel config providers login cloud2") {
		t.Fatalf("providers add --kind azure: %s", r)
	}
	if r := e.adminHostel("config", "providers", "add", "cloud3", "--kind", "azure"); r.code == 0 || !strings.Contains(r.stderr, "tenant") {
		t.Fatalf("providers add --kind azure without IDs: %s", r)
	}
	if r := e.adminHostel("config", "providers", "remove", "cloud2"); r.code != 0 {
		t.Fatalf("providers remove cloud2: %s", r)
	}

	// Runs in Azure but does not answer (here: an idle running machine that
	// stops answering): after warm_running unreachable, counted from when it
	// became unreachable (not from the start of the idle time), it is
	// deallocated and stays broken (a waiting lease does not start it again)
	// until a repair.
	e.adminHostel("config", "machines", "undrain", "cloud/box")
	f.acquire("probe")
	e.mustHostel("release")
	e.waitFor("the room is free", time.Minute, func() bool {
		m := s.machine("cloud/box")
		return m.State == protocol.MachineRunning && len(m.Rooms) > 0 && m.Rooms[0].State == protocol.RoomFree
	})
	// Part of warm_running passes idle (not an assertion: if the machine is
	// deallocated first, the check below still holds).
	time.Sleep(2 * time.Second)
	az.setHung(true)
	c.stop()
	e.waitFor("deallocated and still broken", 2*time.Minute, func() bool {
		m = s.machine("cloud/box")
		return m.State == protocol.MachineBroken && m.Reason == "deallocated: unreachable"
	})
	var brokenAt, stoppingAt time.Time
	var d protocol.MachineDetails
	if code := s.get("/v1/machines/cloud/box", s.token, &d); code != 200 {
		t.Fatalf("GET /v1/machines/cloud/box: HTTP %d", code)
	}
	for _, ev := range d.Events {
		switch {
		case strings.HasSuffix(ev.Message, "running → broken: unreachable"):
			brokenAt = ev.Time
		case strings.HasSuffix(ev.Message, "broken → stopping: keep broken: unreachable") && !brokenAt.IsZero():
			stoppingAt = ev.Time
		}
	}
	if brokenAt.IsZero() || stoppingAt.IsZero() || stoppingAt.Sub(brokenAt) < 4*time.Second {
		t.Fatalf("an idle machine that stopped answering was deallocated %s after it became unreachable, want at least warm_running (4s); events:\n%s",
			stoppingAt.Sub(brokenAt), s.machineEvents("cloud/box"))
	}
	before, _ = az.counts()
	e.newRepoNamed("hung", defaultRepoFiles())
	if r := e.hostel("acquire", "probe", "--task", "hung", "--wait", "5s"); r.code != 75 {
		t.Fatalf("acquire with the machine broken: %s, want exit code 75", r)
	}
	if starts, _ := az.counts(); starts != before || s.machine("cloud/box").State != protocol.MachineBroken {
		t.Fatalf("a machine that did not answer was started again: starts %d (before %d), %+v", starts, before, s.machine("cloud/box"))
	}
	e.vars["HOSTEL_TOKEN"], saved = e.admin, e.vars["HOSTEL_TOKEN"]
	e.mustHostel("config", "machines", "repair", "cloud/box")
	e.vars["HOSTEL_TOKEN"] = saved
	f.acquire("probe")
	e.mustHostel("release")

	// The secret is on the server only.
	doc = s.configDoc().Document
	cfg := e.adminHostel("config")
	events := s.machineEvents("cloud/box")
	for what, text := range map[string]string{"document": doc, "hostel config": cfg.stdout + cfg.stderr, "events": events, "server log": s.log.String()} {
		if strings.Contains(text, azureSecret) {
			t.Fatalf("the client secret is in the %s", what)
		}
	}

	// A change of the sign-in settings (here login_url) removes the stored
	// client secret: it never goes to another address.
	secretFile := filepath.Join(e.dataDir(), "provider-secrets", "cloud")
	if _, err := os.Stat(secretFile); err != nil {
		t.Fatalf("the client secret before the change: %v", err)
	}
	other := strings.Replace(az.srv.URL, "127.0.0.1", "localhost", 1)
	if r := e.applyDoc(strings.Replace(doc, fmt.Sprintf("login_url = %q", az.srv.URL), fmt.Sprintf("login_url = %q", other), 1)); r.code != 0 {
		t.Fatalf("change login_url: %s", r)
	}
	if _, err := os.Stat(secretFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the client secret after a change of login_url: %v, want removed", err)
	}
}

// TestAzureOutside (M12): a static machine becomes an azure machine in place
// (the same machine row). A machine that someone deallocates outside Hostel
// is stopped; one that someone starts outside Hostel is probed and used.
func TestAzureOutside(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.startStaticMachine("box")
	az := newFakeAzure(t, c)
	e.writeProbeTool()
	e.writeConfig(serverHeader() + fmt.Sprintf(`
[providers.cloud]
kind = "static"

[providers.cloud.machines.box]
address = %q
os = "linux"
tools = ["probe"]
`, c.address()))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	id := s.waitMachine("cloud/box", protocol.MachineRunning, machineReady).ID
	doc := s.configDoc().Document
	doc = strings.Replace(doc, `kind = "static"`, `kind = "azure"`+"\n"+az.providerKeys(), 1)
	doc = strings.Replace(doc, "[providers.cloud.machines.box]\n", fmt.Sprintf("[providers.cloud.machines.box]\nvm = %q\nwarm_running = \"1h\"\n", azureVM), 1)
	if r := e.applyDoc(doc); r.code != 0 {
		t.Fatalf("apply the azure provider: %s", r)
	}
	azureLogin(t, e)
	// vm is a machine setting: the machine provisions again (no new install).
	e.waitFor("the machine provisions for the vm change", time.Minute, func() bool {
		return strings.Contains(s.machineEvents("cloud/box"), "running → provisioning: configuration changed")
	})
	if m := s.waitMachine("cloud/box", protocol.MachineRunning, machineReady); m.ID != id {
		t.Fatalf("machine after the change to azure: %+v, want ID %d", m, id)
	}

	// A running azure machine is not removed (its VM would run on).
	if r := e.adminHostel("config", "machines", "remove", "cloud/box"); r.code == 0 || !strings.Contains(r.stderr, "the azure machine cloud/box is running; remove it or change its vm when it is stopped") {
		t.Fatalf("remove a running azure machine: %s", r)
	}

	// Shut down inside the OS: Azure still bills it, so Hostel deallocates it.
	az.setOff(true)
	c.stop()
	m := s.waitMachine("cloud/box", protocol.MachineStopped, 2*time.Minute)
	if _, deallocates := az.counts(); m.ID != id || deallocates != 1 || az.isOff() {
		t.Fatalf("after a shutdown inside: machine %+v, deallocates %d, off %t", m, deallocates, az.isOff())
	}

	c.start() // started in the portal
	s.waitMachine("cloud/box", protocol.MachineRunning, machineReady)
	c.stop() // deallocated in the portal
	s.waitMachine("cloud/box", protocol.MachineStopped, time.Minute)
	if ev := s.machineEvents("cloud/box"); !strings.Contains(ev, "deallocated outside Hostel") {
		t.Fatalf("machine events:\n%s", ev)
	}
	c.start()
	s.waitMachine("cloud/box", protocol.MachineRunning, machineReady)
	if starts, deallocates := az.counts(); starts != 0 || deallocates != 1 {
		t.Fatalf("Hostel called start %d and deallocate %d times, want 0 and 1", starts, deallocates)
	}

	// Deallocated during a lease (for example an auto-shutdown): the room is
	// dirty. The next lease starts the machine, which cleans the room first.
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	f := &fixture{e: e, s: s, c: c}
	f.acquire("probe")
	e.mustHostel("run", "--", "touch", "left-by-the-first-lease")
	c.stop()
	e.hostel("release") // the cleanup cannot reach the machine
	e.waitFor("the room is dirty and the machine stopped", 2*time.Minute, func() bool {
		m := s.machine("cloud/box")
		return m.State == protocol.MachineStopped && len(m.Rooms) == 1 && m.Rooms[0].State == protocol.RoomDirty
	})
	// A lease that the machine cannot take (here --fresh: only an on-demand
	// machine; the on-demand provider is at its limit, so the lease waits)
	// does not start it.
	doc = s.configDoc().Document
	if r := e.applyDoc(doc + e.externalProvider("od", "", map[string]string{"FAKE_LIMIT": "0"}, `
[providers.od.templates.small]
os = "linux"
spec = { image = "machine" }
tools = ["probe"]
`)); r.code != 0 {
		t.Fatalf("add an on-demand provider: %s", r)
	}
	e.newRepoNamed("fresh", defaultRepoFiles())
	views := az.viewCount()
	if r := e.hostel("acquire", "probe", "--fresh", "--task", "fresh", "--wait", "6s"); r.code != 75 {
		t.Fatalf("acquire --fresh: %s, want exit code 75 (it waits)", r)
	}
	if starts, _ := az.counts(); starts != 0 || s.machine("cloud/box").State != protocol.MachineStopped || az.viewCount() < views+2 {
		t.Fatalf("a queued lease that the machine cannot take: starts %d, machine %s, power reads %d; want 0, stopped, at least 2",
			starts, s.machine("cloud/box").State, az.viewCount()-views)
	}
	if r := e.applyDoc(doc); r.code != 0 {
		t.Fatalf("remove the on-demand provider: %s", r)
	}
	e.newRepoNamed("after", defaultRepoFiles())
	f.acquire("probe")
	if starts, _ := az.counts(); starts != 1 {
		t.Fatalf("starts for the dirty room: %d, want 1", starts)
	}
	if out := c.sh("find /home/hostel -name left-by-the-first-lease"); out != "" {
		t.Fatalf("a file of the first lease is left: %s", out)
	}
	e.mustHostel("release")
}

// TestAzureCLI (M12): with auth = "cli", the server signs in with the Azure
// CLI ("az account get-access-token"; a fake az here). Before "az login" the
// server reports the error of az and its next step, and "providers login"
// says what to do instead of asking for a secret. After "az login", a lease
// starts the machine and Hostel deallocates it after warm_running. The
// access token is not in the server log.
func TestAzureCLI(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.init()
	c := e.createStaticMachine("box")
	az := newFakeAzure(t, c)
	az.mu.Lock()
	az.tokens["cli-token-Zq8"] = true
	az.mu.Unlock()
	bin := filepath.Join(e.home, "az-bin")
	loggedIn, calls := filepath.Join(e.home, "az-logged-in"), filepath.Join(e.home, "az-calls")
	e.writeFile(filepath.Join(bin, "az"), fmt.Sprintf(`#!/bin/sh
echo "$*" >> %[1]q
if [ ! -f %[2]q ]; then
  echo "WARNING: a warning of az" >&2
  echo "ERROR: Please run 'az login' to setup account." >&2
  exit 1
fi
echo '{"accessToken": "cli-token-Zq8", "expiresOn": "2099-01-01 00:00:00.000000", "expires_on": '$(($(date +%%s) + 3600))', "tokenType": "Bearer"}'
`, calls, loggedIn))
	if err := os.Chmod(filepath.Join(bin, "az"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.vars["PATH"] = bin + string(os.PathListSeparator) + os.Getenv("PATH")
	e.writeProbeTool()
	e.writeConfig(serverHeader() + fmt.Sprintf(`
[providers.cloud]
kind = "azure"
auth = "cli"
tenant = %q
management_url = %q

[providers.cloud.machines.box]
vm = %q
address = %q
os = "linux"
tools = ["probe"]
warm_running = "4s"
`, azureTenant, az.srv.URL, azureVM, c.address()))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	e.waitFor("the server reports the error of az", time.Minute, func() bool {
		return strings.Contains(s.log.String(), `Azure CLI sign-in failed: exit status 1: ERROR: Please run 'az login' to setup account.; run \"az login\" as the server's user`)
	})
	if r := e.adminHostel("config", "providers", "login", "cloud"); r.code != 1 || !strings.Contains(r.stderr, `signs in with the Azure CLI of the server (auth = "cli"): run "az login" there`) {
		t.Fatalf("providers login of a cli provider: %s", r)
	}

	e.writeFile(loggedIn, "")
	s.waitMachine("cloud/box", protocol.MachineStopped, time.Minute)
	e.useGuestToken()
	e.newRepo(defaultRepoFiles())
	f := &fixture{e: e, s: s, c: c}
	f.acquire("probe")
	if r := e.mustHostel("run", "--", "echo", "on-azure"); !strings.Contains(r.stdout, "on-azure") {
		t.Fatalf("run: %s", r)
	}
	e.mustHostel("release")
	s.waitMachine("cloud/box", protocol.MachineStopped, 2*time.Minute)
	if starts, deallocates := az.counts(); starts != 1 || deallocates < 1 || az.running() {
		t.Fatalf("after warm_running: starts %d, deallocates %d, running %t; want 1, at least 1, false", starts, deallocates, az.running())
	}

	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("account get-access-token --resource %s/ --output json --tenant %s", az.srv.URL, azureTenant)
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != want {
			t.Fatalf("az %s; want az %s", line, want)
		}
	}
	if strings.Contains(s.log.String(), "cli-token-Zq8") || strings.Contains(s.log.String(), "a warning of az") {
		t.Fatal("the server log has the access token or a warning of az")
	}

	// Started outside Hostel after the last reconcile (the next one is in an
	// hour): the row is still stopped, but the removal reads the power state
	// in Azure and is refused, else the machine would run on without Hostel.
	doc := s.configDoc().Document
	slow := strings.Replace(doc, `reconcile = "2s"`, `reconcile = "1h"`, 1)
	if r := e.applyDoc(slow); r.code != 0 {
		t.Fatalf("reconcile 1h: %s", r)
	}
	c.start()
	removed := slow[:strings.Index(slow, "[providers.cloud.machines.box]")]
	if r := e.applyDoc(removed); r.code == 0 || !strings.Contains(r.stderr, "cloud/box: its virtual machine is running in Azure") {
		t.Fatalf("remove a machine that runs in Azure with a stopped row: %s", r)
	}
	if m := s.machine("cloud/box"); m.State != protocol.MachineStopped {
		t.Fatalf("the row: %s, want still stopped (the check, not the row, refused it)", m.State)
	}
	c.stop()
	if r := e.applyDoc(removed); r.code != 0 {
		t.Fatalf("remove the deallocated machine: %s", r)
	}
}

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/csansoon/hostel/internal/invite"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/tlspin"
)

// hostelAt runs hostel as another computer: its own home, without the test's
// HOSTEL_* variables (only the connection file of that home).
func (e *env) hostelAt(home string, args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, hostelBin, args...)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + os.TempDir()}
	cmd.Dir = home
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		e.t.Fatal(err)
	}
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// inviteCode makes an invite as the admin and returns the code.
func (e *env) inviteCode(args ...string) string {
	e.t.Helper()
	saved := e.vars["HOSTEL_TOKEN"]
	e.vars["HOSTEL_TOKEN"] = e.admin
	r := e.mustHostel(append([]string{"config", "users", "invite"}, args...)...)
	e.vars["HOSTEL_TOKEN"] = saved
	m := inviteRE.FindStringSubmatch(r.stdout)
	if m == nil {
		e.t.Fatalf("users invite printed no code: %s", r)
	}
	return m[1]
}

// TestConnect (M8): a client connects with an invite code to a server with its
// own TLS and pins its key; a code works once; a name is unique; a user adds
// a second computer; disconnect removes the computer's token, and an admin
// removes another user's computer; another server
// with another key on the pinned address is refused.
func TestConnect(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.initServer(serverHeader() + staticMachine)
	s := e.startServer("")

	ana := t.TempDir()
	if r := e.hostelAt(ana, "status"); r.code != 78 || !strings.Contains(r.stderr, "hostel connect <invite code>") {
		t.Fatalf("status without a connection: %s, want exit code 78 and the connect hint", r)
	}
	if r := e.hostelAt(ana, "admin", "pubkey"); r.code != 78 {
		t.Fatalf("admin pubkey without a connection: %s, want exit code 78", r)
	}
	// "admin config check" is local: it checks the file without a connection.
	if r := e.hostelAt(ana, "admin", "config", "check", "--config", filepath.Join(e.configDir(), "server.toml")); r.code == 78 || strings.Contains(r.stderr, "not connected") {
		t.Fatalf("admin config check without a connection: %s, want no connection error", r)
	}
	// An address whose proxy fails (HTTP 503) does not stop connect: it tries
	// the next address of the code.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad gateway", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	c, err := invite.Decode(e.inviteCode("--name", "Ana Pérez"))
	if err != nil {
		t.Fatal(err)
	}
	c.Addresses = append([]string{down.URL}, c.Addresses...)
	code, _ := invite.Encode(c)
	r := e.hostelAt(ana, "connect", code, "--name", "ignored")
	if r.code != 0 || !strings.Contains(r.stdout, "Connected as Ana Pérez (member)") || !strings.Contains(r.stdout, "Key:     sha256:") {
		t.Fatalf("connect: %s", r)
	}
	if r := e.hostelAt(ana, "status"); r.code != 0 {
		t.Fatalf("status with the connection file: %s", r)
	}
	if r := e.hostelAt(t.TempDir(), "connect", code, "--name", "Other"); r.code != 1 || !strings.Contains(r.stderr, "not valid") {
		t.Fatalf("second use of a code: %s, want a refusal", r)
	}
	c, err = invite.Decode(e.inviteCode())
	if err != nil {
		t.Fatal(err)
	}
	c.Secret[0] ^= 0xff
	wrong, _ := invite.Encode(c)
	if r := e.hostelAt(t.TempDir(), "connect", wrong, "--name", "Guess"); r.code != 1 || !strings.Contains(r.stderr, "not valid") {
		t.Fatalf("wrong secret: %s, want a refusal", r)
	}
	if r := e.hostelAt(t.TempDir(), "connect", e.inviteCode(), "--name", "ana pérez"); r.code != 1 || !strings.Contains(r.stderr, "another user has the name") {
		t.Fatalf("a name that another user has: %s, want a refusal", r)
	}
	if r := e.hostelAt(ana, "connect", e.inviteCode(), "--name", "Ana 2"); r.code != 1 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("connect over an existing connection without --yes: %s", r)
	}
	if r := e.hostelAt(t.TempDir(), "connect", e.inviteCode("--name", "ANA PÉREZ"), "--name", "x"); r.code != 1 || !strings.Contains(r.stderr, "ask the admin for a new invite") {
		t.Fatalf("an invite whose name is taken: %s", r)
	}
	// Connecting again with --yes replaces the connection and revokes the old token.
	bob := t.TempDir()
	e.hostelAt(bob, "connect", e.inviteCode(), "--name", "Bob")
	bobOld := regexp.MustCompile(`(?m)^token = '?"?([^'"\n]+)`).FindStringSubmatch(readFile(t, filepath.Join(bob, ".config", "hostel", "config.toml")))
	m := inviteRE.FindStringSubmatch(e.hostelAt(bob, "config", "users", "code").stdout)
	r = e.hostelAt(bob, "connect", m[1], "--name", "Bob", "--yes")
	computer := regexp.MustCompile(`from (\S+)\.`).FindStringSubmatch(r.stdout)
	if r.code != 0 || computer == nil {
		t.Fatalf("connect --yes: %s", r)
	}
	if code := s.get("/v1/me", bobOld[1], nil); code != 401 {
		t.Fatalf("the replaced connection's token: HTTP %d, want 401", code)
	}
	// An admin removes one computer of a user (a computer that cannot
	// disconnect itself); the user stays.
	bobTok := regexp.MustCompile(`(?m)^token = '?"?([^'"\n]+)`).FindStringSubmatch(readFile(t, filepath.Join(bob, ".config", "hostel", "config.toml")))
	if r := e.hostelAt(bob, "config", "users", "remove", "Bob", "--computer", computer[1]); r.code != 1 || !strings.Contains(r.stderr, "only an admin") {
		t.Fatalf("a member removes a computer: %s, want a refusal", r)
	}
	if r := e.hostel("config", "users", "remove", "Bob", "--computer", "nothing"); r.code != 1 || !strings.Contains(r.stderr, `no computer "nothing"`) {
		t.Fatalf("remove an unknown computer: %s", r)
	}
	if r := e.hostel("config", "users", "remove", "Bob", "--computer", computer[1]); r.code != 0 {
		t.Fatalf("remove Bob's computer: %s", r)
	}
	if code := s.get("/v1/me", bobTok[1], nil); code != 401 {
		t.Fatalf("the removed computer's token: HTTP %d, want 401", code)
	}
	if r := e.hostel("config", "users"); !strings.Contains(r.stdout, "Bob") {
		t.Fatalf("users after the removal of Bob's computer: %s, want Bob", r)
	}

	// A second computer of Ana.
	r = e.hostelAt(ana, "config", "users", "code")
	m = inviteRE.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil {
		t.Fatalf("users code: %s", r)
	}
	laptop := t.TempDir()
	if r := e.hostelAt(laptop, "connect", m[1], "--name", "whatever"); r.code != 0 || !strings.Contains(r.stdout, "Connected as Ana Pérez (member)") {
		t.Fatalf("connect a second computer: %s", r)
	}
	if r := e.hostelAt(ana, "config", "users"); r.code != 0 || !regexp.MustCompile(`Ana Pérez\s+member\s+\S+, \S+`).MatchString(r.stdout) {
		t.Fatalf("users with two computers of Ana:\n%s", r.stdout)
	}
	tok := regexp.MustCompile(`(?m)^token = '?"?([^'"\n]+)`).FindStringSubmatch(readFile(t, filepath.Join(laptop, ".config", "hostel", "config.toml")))
	if r := e.hostelAt(laptop, "disconnect"); r.code != 0 {
		t.Fatalf("disconnect: %s", r)
	}
	if code := s.get("/v1/me", tok[1], nil); code != 401 {
		t.Fatalf("token of the disconnected computer: HTTP %d, want 401", code)
	}
	if r := e.hostelAt(laptop, "status"); r.code != 78 {
		t.Fatalf("status after disconnect: %s, want exit code 78", r)
	}
	if r := e.hostelAt(ana, "status"); r.code != 0 {
		t.Fatalf("Ana's first computer after the second disconnected: %s", r)
	}

	// Another server (another key) on the address that Ana pinned.
	e2 := newEnv(t)
	e2.initServer(serverHeader() + staticMachine)
	s2 := e2.startServer("")
	conn := filepath.Join(ana, ".config", "hostel", "config.toml")
	data := strings.Replace(readFile(t, conn), s.url, s2.url, 1)
	if err := os.WriteFile(conn, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := e.hostelAt(ana, "status"); r.code != 1 || !strings.Contains(r.stderr, "another TLS key") {
		t.Fatalf("a server with another key: %s, want a refusal", r)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestUsersRoles (M8): the status shows each lease's user; a member cannot
// invite or end another user's lease; the last admin stays; an admin ends a
// member's lease, and the member's client says who did it; removing a user
// ends their lease and their token.
func TestUsersRoles(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	guest := f.e.vars["HOSTEL_TOKEN"] // the fixture's member "guest"
	lease := f.acquire()
	f.e.vars["HOSTEL_TOKEN"] = f.e.admin
	if r := f.e.mustHostel("status"); !regexp.MustCompile(lease + `\s+ready\s+fakedevice\s+\S*\s+normal\s+guest\s`).MatchString(r.stdout) {
		t.Fatalf("status has no user for the lease:\n%s", r.stdout)
	}

	f.e.vars["HOSTEL_TOKEN"] = f.e.newUser("Other", "member")
	if r := f.e.hostel("config", "users", "invite"); r.code != 1 || !strings.Contains(r.stderr, "only an admin") {
		t.Fatalf("invite by a member: %s", r)
	}
	if r := f.e.hostel("release", lease); r.code != 1 || !strings.Contains(r.stderr, "belongs to another user") {
		t.Fatalf("a member releases another user's lease: %s", r)
	}

	f.e.vars["HOSTEL_TOKEN"] = f.e.admin
	if r := f.e.hostel("config", "users", "set", "admin", "--role", "member"); r.code != 1 || !strings.Contains(r.stderr, "last admin") {
		t.Fatalf("demote the last admin: %s", r)
	}
	if r := f.e.hostel("config", "users", "remove", "admin"); r.code != 1 || !strings.Contains(r.stderr, "last admin") {
		t.Fatalf("remove the last admin: %s", r)
	}
	// The open invites of an admin end when the admin is removed.
	f.e.vars["HOSTEL_TOKEN"] = f.e.newUser("Boss", "admin")
	r := f.e.mustHostel("config", "users", "invite", "--role", "admin")
	bossCode := inviteRE.FindStringSubmatch(r.stdout)[1]
	f.e.vars["HOSTEL_TOKEN"] = f.e.admin
	f.e.mustHostel("config", "users", "remove", "Boss")
	if r := f.e.hostelAt(t.TempDir(), "connect", bossCode, "--name", "Later"); r.code != 1 || !strings.Contains(r.stderr, "not valid") {
		t.Fatalf("an invite of a removed admin: %s, want a refusal", r)
	}

	f.e.mustHostel("release", lease)
	f.e.vars["HOSTEL_TOKEN"] = guest
	if r := f.e.hostel("run", "--", "true"); r.code != 1 || !strings.Contains(r.stderr, "released by admin (admin)") {
		t.Fatalf("the member's client after an admin release: %s", r)
	}
	f.waitSlotFree()

	// Removing a user ends their lease and their token.
	f.e.newRepoNamed("second", defaultRepoFiles())
	second := f.acquire()
	f.e.vars["HOSTEL_TOKEN"] = f.e.admin
	f.e.mustHostel("config", "users", "remove", "guest")
	f.e.waitFor("the removed user's lease ends", time.Minute, func() bool {
		var l protocol.Lease
		f.s.get("/v1/leases/"+second, f.e.admin, &l)
		return l.State != protocol.LeaseReady
	})
	if code := f.s.get("/v1/me", guest, nil); code != 401 {
		t.Fatalf("the removed user's token: HTTP %d, want 401", code)
	}
	if r := f.e.mustHostel("config", "users"); strings.Contains(r.stdout, "guest") {
		t.Fatalf("users after the removal:\n%s", r.stdout)
	}
	f.waitSlotFree()
}

var queuedRE = regexp.MustCompile(`lease ([a-z0-9]+) queued`)

// TestQueueLanes (M8): with the only room busy, queued leases come out in their
// lanes (high, normal, low), first in, first out in each lane, without aging;
// an admin moves a lease first and changes a lane; the placement ends when the
// lease gets a slot.
func TestQueueLanes(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	busy := f.acquire()
	ids := map[string]string{}
	for _, q := range []struct{ name, prio string }{{"low1", "low"}, {"normal1", "normal"}, {"high1", "high"}, {"normal2", "normal"}} {
		f.e.newRepoNamed(q.name, defaultRepoFiles())
		r := f.e.hostel("acquire", "fakedevice", "--task", q.name, "--priority", q.prio, "--wait", "1s")
		m := queuedRE.FindStringSubmatch(r.stdout + r.stderr)
		if r.code != 75 || m == nil {
			t.Fatalf("queue %s: %s", q.name, r)
		}
		ids[q.name] = m[1]
	}
	order := func(want ...string) {
		t.Helper()
		f.e.waitFor("queue order "+strings.Join(want, ", "), 30*time.Second, func() bool {
			pos := map[string]int{}
			for _, l := range f.s.status().Leases {
				pos[l.ID] = l.Position
			}
			for i, n := range want {
				if pos[ids[n]] != i+1 {
					return false
				}
			}
			return true
		})
	}
	order("high1", "normal1", "normal2", "low1")

	if code := f.s.send("POST", "/v1/leases/"+ids["low1"]+"/move", f.e.admin, strings.NewReader(`{"first": true}`), nil); code != 200 {
		t.Fatalf("move: HTTP %d", code)
	}
	if code := f.s.send("POST", "/v1/leases/"+ids["normal2"]+"/priority", f.e.admin, strings.NewReader(`{"priority": "high"}`), nil); code != 200 {
		t.Fatalf("priority: HTTP %d", code)
	}
	order("low1", "high1", "normal2", "normal1")
	// A second move first passes the first one; a move before another lease.
	if code := f.s.send("POST", "/v1/leases/"+ids["normal1"]+"/move", f.e.admin, strings.NewReader(`{"first": true}`), nil); code != 200 {
		t.Fatalf("second move: HTTP %d", code)
	}
	order("normal1", "low1", "high1", "normal2")
	if code := f.s.send("POST", "/v1/leases/"+ids["normal2"]+"/move", f.e.admin, strings.NewReader(`{"before": "`+ids["low1"]+`"}`), nil); code != 200 {
		t.Fatalf("move before: HTTP %d", code)
	}
	order("normal1", "normal2", "low1", "high1")
	if code := f.s.send("POST", "/v1/leases/"+ids["normal1"]+"/move", f.e.vars["HOSTEL_TOKEN"], strings.NewReader(`{"first": true}`), nil); code != 403 {
		t.Fatalf("move by a member: HTTP %d, want 403", code)
	}

	f.useRepo("repo")
	f.e.mustHostel("release")
	f.e.waitFor("the lease that was moved first starts first", time.Minute, func() bool {
		var l protocol.Lease
		f.s.get("/v1/leases/"+ids["normal1"], f.e.admin, &l)
		return l.State == protocol.LeaseReady
	})
	// The placement ends when the lease gets a slot: a lease that goes back to
	// the queue (a provider limit, a failed start) does not jump ahead again.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.e.dataDir(), "hostel.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var manual int
	if err := db.QueryRow(`SELECT manual_position FROM leases WHERE id = ?`, ids["normal1"]).Scan(&manual); err != nil || manual != 0 {
		t.Fatalf("manual_position of the started lease = %d (err %v), want 0", manual, err)
	}
	_ = busy
}

// TestTokenMigration (M8): a database from before M8 (migration 002) with guest
// and admin tokens: after the start, the tokens of one name and role are one
// user, an admin token's user is admin, a guest token with an admin's name gets
// its own member user, and the old tokens still work.
func TestTokenMigration(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.initServer(serverHeader() + staticMachine)
	for _, f := range []string{"hostel.db", "hostel.db-wal", "hostel.db-shm"} {
		os.Remove(filepath.Join(e.dataDir(), f))
	}
	db, err := sql.Open("sqlite", filepath.Join(e.dataDir(), "hostel.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS schema_version`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"001_initial.sql", "002_m6.sql"} {
		if _, err := db.Exec(readFile(t, filepath.Join("..", "..", "internal", "store", "migrations", f))); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	hash := func(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
	now := time.Now().UnixMilli()
	for _, tok := range []struct{ name, role, secret string }{
		{"owner", "admin", "hostel_owner"}, {"Owner", "guest", "hostel_owner_ci"},
		{"agents", "guest", "hostel_agents1"}, {"agents", "guest", "hostel_agents2"},
		{"owner (member)", "guest", "hostel_owner_member"},
		{"Élodie", "guest", "hostel_elodie1"}, {"élodie", "guest", "hostel_elodie2"},
		{"ci/bot", "guest", "hostel_cibot"},
	} {
		if _, err := db.Exec(`INSERT INTO tokens (name, role, machine, sha256, created_at) VALUES (?, ?, '', ?, ?)`, tok.name, tok.role, hash(tok.secret), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER NOT NULL); INSERT INTO schema_version VALUES (1), (2)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	e.admin = "hostel_owner"
	s := e.startServer("")
	var me protocol.Me
	if code := s.get("/v1/me", "hostel_owner", &me); code != 200 || me.User != "owner" || me.Role != "admin" {
		t.Fatalf("old admin token: HTTP %d, %+v", code, me)
	}
	for _, secret := range []string{"hostel_agents1", "hostel_agents2"} {
		if code := s.get("/v1/me", secret, &me); code != 200 || me.User != "agents" || me.Role != "member" {
			t.Fatalf("old guest token %s: HTTP %d, %+v", secret, code, me)
		}
	}
	// A guest token with an admin's name gets its own member user, not admin;
	// "Owner (member)" is taken by another guest token, so it is "(member 2)".
	if code := s.get("/v1/me", "hostel_owner_ci", &me); code != 200 || me.User != "Owner (member 2)" || me.Role != "member" {
		t.Fatalf("old guest token with the admin's name: HTTP %d, %+v", code, me)
	}
	if code := s.get("/v1/me", "hostel_owner_member", &me); code != 200 || me.User != "owner (member)" || me.Role != "member" {
		t.Fatalf("old guest token named \"owner (member)\": HTTP %d, %+v", code, me)
	}
	// Names that differ only in Unicode case are one user, as at run time.
	var first protocol.Me
	for _, secret := range []string{"hostel_elodie1", "hostel_elodie2"} {
		if code := s.get("/v1/me", secret, &me); code != 200 || !strings.EqualFold(me.User, "élodie") || (first.User != "" && me.User != first.User) {
			t.Fatalf("old guest token %s: HTTP %d, %+v (first %+v)", secret, code, me, first)
		}
		first = me
	}
	var users []protocol.User
	s.get("/v1/users", "hostel_owner", &users)
	if got := fmt.Sprint(len(users)); got != "6" {
		t.Fatalf("users after the migration: %+v, want 6", users)
	}
	// A migrated name that the new rule does not allow can still be changed
	// and removed by name.
	_, keyHash, err := tlspin.Load(e.dataDir())
	if err != nil {
		t.Fatal(err)
	}
	e.vars["HOSTEL_SERVER_KEY"] = tlspin.Fingerprint(keyHash)
	if r := e.hostel("config", "users", "set", "ci/bot", "--role", "admin"); r.code != 0 {
		t.Fatalf("set the role of ci/bot: %s", r)
	}
	if r := e.hostel("config", "users", "remove", "ci/bot"); r.code != 0 {
		t.Fatalf("remove ci/bot: %s", r)
	}
	if code := s.get("/v1/me", "hostel_cibot", &me); code != 401 {
		t.Fatalf("token of the removed user ci/bot: HTTP %d, want 401", code)
	}
}

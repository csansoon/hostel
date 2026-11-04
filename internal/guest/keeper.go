package guest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/protocol"
)

const (
	// keeperFresh: a heartbeat older than this means that no keeper runs (a
	// keeper writes one after each poll, at most pollWait apart).
	keeperFresh = 2*pollWait + 15*time.Second
	// keeperGiveUp: a keeper stops after this long without an answer from the
	// server. It is long: after a restart, the server gives waiting leases a
	// new poll_timeout, so a keeper that waits out an outage keeps the place.
	keeperGiveUp = time.Hour
	// keeperMaxLife bounds a keeper in any case.
	keeperMaxLife = 24 * time.Hour
)

// keeperBeat is keeper.json in the lease's state directory: the keeper's own
// ID, the lease that it keeps, and its last heartbeat.
type keeperBeat struct {
	ID      string    `json:"id"`
	LeaseID string    `json:"lease_id"`
	At      time.Time `json:"at"`
}

func keeperPath(st *State) string { return filepath.Join(st.Dir, "keeper.json") }

func readBeat(st *State) (keeperBeat, bool) {
	var b keeperBeat
	data, err := os.ReadFile(keeperPath(st))
	if err != nil || json.Unmarshal(data, &b) != nil {
		return b, false
	}
	return b, true
}

func writeBeat(st *State, b keeperBeat) error {
	data, _ := json.Marshal(b)
	tmp := keeperPath(st) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, keeperPath(st))
}

// ensureKeeper starts the keeper of a waiting lease (acquire --background) if
// none runs: a detached process that polls the lease, so that the server keeps
// its place in the queue while the agent does other work.
func ensureKeeper(st *State) error {
	if keeperAlive(st) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(st.Dir, "keeper.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	if err := startDetached(exe, []string{"keep-place", "--dir", st.Dir}, logFile); err != nil {
		return fmt.Errorf("start the process that keeps the place: %w", err)
	}
	return nil
}

// keeperAlive reports a keeper with a fresh heartbeat for the lease of st.
func keeperAlive(st *State) bool {
	b, ok := readBeat(st)
	return ok && b.LeaseID == st.LeaseID && time.Since(b.At) < keeperFresh
}

// KeepPlace is the hidden command "hostel keep-place --dir <state dir>": it
// polls a waiting lease until it is ready or ends. It stops when another
// keeper takes over, when the worktree has another lease, after keeperGiveUp
// without an answer, and after keeperMaxLife.
func KeepPlace(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel keep-place", stderr)
	dir := fs.String("dir", "", "the lease's state directory")
	if _, err := cli.Parse(fs, args); err != nil || *dir == "" {
		return ExitUsage
	}
	st, ok, err := loadStateDir(*dir)
	if err != nil || !ok || st.Final {
		fmt.Fprintf(stderr, "hostel keep-place: no waiting lease in %s (%v); stopping\n", *dir, err)
		return 0
	}
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return fail(stderr, err)
	}
	me := hex.EncodeToString(id)
	beat := func() {
		// A failed write (for example a reader that holds the file on
		// Windows) is not fatal: the next poll writes again.
		if err := writeBeat(st, keeperBeat{ID: me, LeaseID: st.LeaseID, At: time.Now()}); err != nil {
			fmt.Fprintf(stderr, "hostel keep-place: heartbeat: %v\n", err)
		}
	}
	beat()
	// When it stops, the keeper removes its own heartbeat, so that the next
	// acquire --background starts a new keeper at once.
	defer func() {
		if b, ok := readBeat(st); ok && b.ID == me {
			os.Remove(keeperPath(st))
		}
	}()
	api, err := apiClient()
	if err != nil {
		return fail(stderr, err)
	}
	leaseID := st.LeaseID
	start, lastOK := time.Now(), time.Now()
	var since int64
	var retry time.Duration
	for time.Since(start) < keeperMaxLife {
		if b, ok := readBeat(st); !ok || b.ID != me {
			return 0 // another keeper took over
		}
		if cur, ok, err := loadStateDir(*dir); err != nil || !ok || cur.Final || cur.LeaseID != leaseID || cur.Access != nil {
			return 0 // the lease ended, or acquire took it over
		}
		ctx, cancel := context.WithTimeout(context.Background(), pollWait+20*time.Second)
		var l protocol.Lease
		err := api.Do(ctx, "GET", fmt.Sprintf("/v1/leases/%s?since=%d&wait=%ds", leaseID, since, int(pollWait.Seconds())), nil, &l)
		cancel()
		if err != nil {
			var apiErr *client.APIError
			// Server errors (5xx) and network errors pass; a refused token,
			// a gone lease, or a bad request end the keeper.
			serverErr := errors.As(err, &apiErr) && apiErr.Status >= 500
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound || !serverErr && !transient(err) || time.Since(lastOK) > keeperGiveUp {
				fmt.Fprintf(stderr, "hostel keep-place: lease %s: %v; stopping\n", leaseID, err)
				return 0
			}
			retry = min(max(2*retry, time.Second), maxPollRetry)
			time.Sleep(retry)
			continue
		}
		retry, lastOK, since = 0, time.Now(), l.Revision
		beat()
		if l.State != protocol.LeaseQueued && l.State != protocol.LeaseAssigned && l.State != protocol.LeaseProvisioning && l.State != protocol.LeasePreparing {
			fmt.Fprintf(stderr, "hostel keep-place: lease %s is %s; stopping\n", leaseID, l.State)
			return 0
		}
	}
	return 0
}

// loadStateDir reads a lease state from its directory.
func loadStateDir(dir string) (*State, bool, error) {
	s := &State{Dir: dir}
	data, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, false, fmt.Errorf("read %s: %w", s.path(), err)
	}
	return s, true, nil
}

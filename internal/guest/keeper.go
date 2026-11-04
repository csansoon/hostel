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
	// server (the server ends the waiting lease anyway after poll_timeout).
	keeperGiveUp = 10 * time.Minute
	// keeperMaxLife bounds a keeper in any case.
	keeperMaxLife = 24 * time.Hour
)

// keeperBeat is keeper.json in the lease's state directory.
type keeperBeat struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
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
	if b, ok := readBeat(st); ok && time.Since(b.At) < keeperFresh {
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
		return 0
	}
	id := make([]byte, 8)
	rand.Read(id)
	me := hex.EncodeToString(id)
	beat := func() error { return writeBeat(st, keeperBeat{ID: me, At: time.Now()}) }
	if err := beat(); err != nil {
		return fail(stderr, err)
	}
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
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound || !transient(err) || time.Since(lastOK) > keeperGiveUp {
				fmt.Fprintf(stderr, "hostel keep-place: lease %s: %v; stopping\n", leaseID, err)
				return 0
			}
			retry = min(max(2*retry, time.Second), maxPollRetry)
			time.Sleep(retry)
			continue
		}
		retry, lastOK, since = 0, time.Now(), l.Revision
		if err := beat(); err != nil {
			return fail(stderr, err)
		}
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

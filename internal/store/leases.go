package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/scheduler"
)

type Lease struct {
	ID       string
	TokenID  int64
	State    string
	Revision int64
	Position int
	Reason   string
	// Tools is the request: "name" or "name:option" entries.
	Tools []string
	// Started are the screen tools that started for the lease, in the order
	// of their start.
	Started        []protocol.StartedTool
	Task           string
	Priority       int
	Tunnels        []protocol.TunnelRequest
	Fresh          bool
	PublicKey      string
	GuestHost      string
	Repo           string
	Worktree       string
	Branch         string
	Provider       string
	Template       string
	MachineID      int64
	RoomName       string
	Attempts       int
	BeganAt        time.Time
	CreatedAt      time.Time
	LastPollAt     time.Time
	LastActivityAt time.Time
	ReadyAt        time.Time
	ExpiresAt      time.Time
	EndedAt        time.Time
	EndState       string
	EndReason      string
	ManualPosition int
}

// Final reports whether the lease state is final.
func Final(state string) bool {
	switch state {
	case protocol.LeaseReleased, protocol.LeaseExpired, protocol.LeaseCancelled, protocol.LeaseFailed:
		return true
	}
	return false
}

const leaseColumns = `id, token_id, state, revision, position, reason, tools, started, task, priority,
	tunnels, fresh, public_key, guest_host, repo, worktree, provider, template, machine_id,
	room_name, attempts, began_at, created_at, last_poll_at, last_activity_at, ready_at, expires_at,
	ended_at, end_state, end_reason, manual_position, branch`

func scanLease(row interface{ Scan(...any) error }) (Lease, error) {
	var l Lease
	var tools, started, tunnels string
	var machineID, began, ready, expires, ended sql.NullInt64
	var created, poll, activity int64
	err := row.Scan(&l.ID, &l.TokenID, &l.State, &l.Revision, &l.Position, &l.Reason, &tools, &started,
		&l.Task, &l.Priority, &tunnels, &l.Fresh, &l.PublicKey, &l.GuestHost, &l.Repo, &l.Worktree,
		&l.Provider, &l.Template, &machineID, &l.RoomName, &l.Attempts, &began, &created, &poll, &activity,
		&ready, &expires, &ended, &l.EndState, &l.EndReason, &l.ManualPosition, &l.Branch)
	if err != nil {
		return l, err
	}
	for _, f := range []struct {
		data string
		dst  any
	}{{tools, &l.Tools}, {started, &l.Started}, {tunnels, &l.Tunnels}} {
		if err := json.Unmarshal([]byte(f.data), f.dst); err != nil {
			return l, fmt.Errorf("lease %s: decode: %w", l.ID, err)
		}
	}
	l.MachineID = machineID.Int64
	l.BeganAt, l.ReadyAt, l.ExpiresAt, l.EndedAt = fromMS(began), fromMS(ready), fromMS(expires), fromMS(ended)
	l.CreatedAt, l.LastPollAt, l.LastActivityAt = time.UnixMilli(created), time.UnixMilli(poll), time.UnixMilli(activity)
	return l, nil
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// InsertLease stores a new queued lease.
func (s *Store) InsertLease(ctx context.Context, l Lease, now time.Time) error {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO leases (id, token_id, state, tools, task, priority,
			tunnels, fresh, public_key, guest_host, repo, worktree, branch, created_at, last_poll_at, last_activity_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.ID, l.TokenID, protocol.LeaseQueued, jsonText(nonNil(l.Tools)), l.Task, l.Priority,
			jsonText(l.Tunnels), l.Fresh, l.PublicKey, l.GuestHost, l.Repo, l.Worktree, l.Branch,
			now.UnixMilli(), now.UnixMilli(), now.UnixMilli())
		if err != nil {
			return err
		}
		return insertEvent(ctx, tx, now, l.ID, 0, "lease", fmt.Sprintf("lease %s queued for %s: %s", l.ID, ToolsText(l.Tools), l.Task))
	})
	if err != nil {
		return fmt.Errorf("insert lease: %w", err)
	}
	s.leaseChanged(l.ID)
	return nil
}

// ToolsText names the tools of a request for messages.
func ToolsText(tools []string) string {
	if len(tools) == 0 {
		return "any machine"
	}
	return strings.Join(tools, " ")
}

// SetStarted records the screen tools that started for a lease.
func (s *Store) SetStarted(ctx context.Context, id string, started []protocol.StartedTool) error {
	if started == nil {
		started = []protocol.StartedTool{}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE leases SET started = ? WHERE id = ?`, jsonText(started), id)
	return err
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (s *Store) Lease(ctx context.Context, id string) (Lease, error) {
	l, err := scanLease(s.db.QueryRowContext(ctx, `SELECT `+leaseColumns+` FROM leases WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNotFound
	}
	if err != nil {
		return Lease{}, fmt.Errorf("read lease %s: %w", id, err)
	}
	return l, nil
}

// ActiveLeases returns all leases that are not final, oldest first.
func (s *Store) ActiveLeases(ctx context.Context) ([]Lease, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+leaseColumns+` FROM leases
		WHERE state NOT IN ('released', 'expired', 'cancelled', 'failed') ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list leases: %w", err)
	}
	defer rows.Close()
	var out []Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetLeaseHook sets a function that is called after each committed change of
// a lease's state, reason, or position (to wake long polls).
func (s *Store) SetLeaseHook(fn func(id string)) { s.hook = fn }

func (s *Store) leaseChanged(id string) {
	if s.hook != nil {
		s.hook(id)
	}
}

// SetQueue records the position and the reason of a queued lease. It changes
// the revision only when a value changes.
func (s *Store) SetQueue(ctx context.Context, id string, position int, reason string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE leases SET position = ?, reason = ?, revision = revision + 1
		WHERE id = ? AND state = 'queued' AND (position != ? OR reason != ?)`, position, reason, id, position, reason)
	if err != nil {
		return fmt.Errorf("set queue position: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.leaseChanged(id)
	}
	return nil
}

// Assign reserves rooms of a machine for a queued lease and gives its tunnels
// their machine ports (compare-and-set: the lease must be queued and the rooms
// free). With drain, the machine gets no later lease (reuse = false). It reports
// whether the assignment happened.
func (s *Store) Assign(ctx context.Context, leaseID string, machine Machine, rooms []string, ports map[string]int, drain bool, now time.Time) (bool, error) {
	ok := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'assigned', machine_id = ?, room_name = ?, provider = ?,
			template = ?, position = 0, manual_position = 0, reason = '', revision = revision + 1 WHERE id = ? AND state = 'queued'`,
			machine.ID, rooms[0], machine.Provider, machine.Template, leaseID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		for _, sl := range rooms {
			// A lease user of an earlier lease that cleanup did not delete
			// (a failed preparing) is not this lease's; a later cleanup or
			// provisioning deletes it (M11).
			res, err := tx.ExecContext(ctx, `UPDATE rooms SET state = 'assigned', lease_id = ?,
				home = CASE WHEN lease_user != '' THEN '' ELSE home END, lease_user = ''
				WHERE machine_id = ? AND name = ? AND state = 'free'`,
				leaseID, machine.ID, sl)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errRoomTaken
			}
		}
		for name, port := range ports {
			if _, err := tx.ExecContext(ctx, `INSERT INTO lease_tunnels (lease_id, name, machine_port) VALUES (?, ?, ?)`, leaseID, name, port); err != nil {
				return err
			}
		}
		if drain {
			// Compare-and-set: one scheduler pass can give several rooms of
			// the machine to several leases; only the first gets it.
			res, err := tx.ExecContext(ctx, `UPDATE machines SET drained = 1 WHERE id = ? AND drained = 0`, machine.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errRoomTaken
			}
		}
		ok = true
		return insertEvent(ctx, tx, now, leaseID, machine.ID, "lease",
			fmt.Sprintf("lease %s assigned to machine %s room %s", leaseID, machine.Name, strings.Join(rooms, ",")))
	})
	if errors.Is(err, errRoomTaken) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("assign lease %s: %w", leaseID, err)
	}
	if ok {
		s.leaseChanged(leaseID)
	}
	return ok, nil
}

var errRoomTaken = errors.New("room taken")

// MoveLease changes the state if it is one of from (compare-and-set).
func (s *Store) MoveLease(ctx context.Context, id string, from []string, to string, now time.Time) (bool, error) {
	return s.updateLease(ctx, id, from, now, fmt.Sprintf("→ %s", to), `state = ?`, to)
}

// SetBegan records that the helper "begin" command ran for the lease.
func (s *Store) SetBegan(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE leases SET began_at = ? WHERE id = ? AND began_at IS NULL`, now.UnixMilli(), id)
	return err
}

// MarkReady moves a preparing lease to ready.
func (s *Store) MarkReady(ctx context.Context, id string, now, expires time.Time) (bool, error) {
	return s.updateLease(ctx, id, []string{protocol.LeasePreparing}, now, "→ ready",
		`state = 'ready', ready_at = ?, expires_at = ?, last_activity_at = ?`, now.UnixMilli(), expires.UnixMilli(), now.UnixMilli())
}

// EndLease ends a lease that is in one of from. A lease with a room goes to
// cleaning with endState; a lease without a room goes to endState at once.
func (s *Store) EndLease(ctx context.Context, id string, from []string, endState, reason string, now time.Time) (bool, error) {
	ok := false
	// The decision reads machine_id in the same transaction as the update, so a
	// concurrent assignment cannot leave a reserved room behind.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var machineID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT state, machine_id FROM leases WHERE id = ?`, id).Scan(&state, &machineID); err != nil {
			return err
		}
		if !contains(from, state) {
			return nil
		}
		var msg string
		if machineID.Valid {
			msg = fmt.Sprintf("→ cleaning (%s: %s)", endState, reason)
			_, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'cleaning', end_state = ?, end_reason = ?,
				revision = revision + 1 WHERE id = ?`, endState, reason, id)
			if err != nil {
				return err
			}
		} else {
			msg = fmt.Sprintf("→ %s: %s", endState, reason)
			_, err := tx.ExecContext(ctx, `UPDATE leases SET state = ?, end_state = ?, end_reason = ?, ended_at = ?,
				revision = revision + 1 WHERE id = ?`, endState, endState, reason, now.UnixMilli(), id)
			if err != nil {
				return err
			}
		}
		ok = true
		return insertEvent(ctx, tx, now, id, 0, "lease", fmt.Sprintf("lease %s %s %s", id, state, msg))
	})
	if err != nil {
		return false, fmt.Errorf("end lease %s: %w", id, err)
	}
	if ok {
		s.leaseChanged(id)
	}
	return ok, nil
}

// FinishCleaning moves a cleaning lease to its final state and sets its rooms
// to roomState (free, or broken after a failed cleanup; a broken room keeps
// cleanupData for the repair).
func (s *Store) FinishCleaning(ctx context.Context, id, roomState, cleanupData string, now time.Time) (bool, error) {
	ok := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE leases SET state = end_state, ended_at = ?, revision = revision + 1
			WHERE id = ? AND state = 'cleaning'`, now.UnixMilli(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ok = true
		leaseIDCol := "''"
		if roomState != protocol.RoomFree {
			leaseIDCol = "lease_id"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET state = ?, lease_id = `+leaseIDCol+`, cleanup_data = ? WHERE `+leaseRooms,
			roomState, cleanupData, id, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM lease_tunnels WHERE lease_id = ?`, id); err != nil {
			return err
		}
		return insertEvent(ctx, tx, now, id, 0, "lease", fmt.Sprintf("lease %s cleaned; room %s", id, roomState))
	})
	if err != nil {
		return false, fmt.Errorf("finish cleaning of lease %s: %w", id, err)
	}
	if ok {
		s.leaseChanged(id)
	}
	return ok, nil
}

// leaseRooms selects the rooms of a lease in its current machine (two arguments:
// the lease ID twice). A room of an earlier machine never matches.
const leaseRooms = `lease_id = ? AND machine_id = (SELECT machine_id FROM leases WHERE id = ?)`

// SetRooms sets the state of the lease's rooms.
func (s *Store) SetRooms(ctx context.Context, leaseID, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET state = ? WHERE `+leaseRooms, state, leaseID, leaseID)
	return err
}

// Poll records that the guest polled the lease.
func (s *Store) Poll(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE leases SET last_poll_at = ? WHERE id = ?`, now.UnixMilli(), id)
	return err
}

// Touch records guest activity.
func (s *Store) Touch(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE leases SET last_activity_at = ? WHERE id = ?`, now.UnixMilli(), id)
	return err
}

// LeaseTunnels returns the machine port of each tunnel of a lease.
func (s *Store) LeaseTunnels(ctx context.Context, id string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, machine_port FROM lease_tunnels WHERE lease_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("read tunnels: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var port int
		if err := rows.Scan(&name, &port); err != nil {
			return nil, err
		}
		out[name] = port
	}
	return out, rows.Err()
}

// updateLease applies set (with args) to a lease in one of from, increments the
// revision, and records an event.
func (s *Store) updateLease(ctx context.Context, id string, from []string, now time.Time, msg, set string, args ...any) (bool, error) {
	ok := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, id).Scan(&state); err != nil {
			return err
		}
		if !contains(from, state) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET `+set+`, revision = revision + 1 WHERE id = ?`, append(args, id)...); err != nil {
			return err
		}
		ok = true
		return insertEvent(ctx, tx, now, id, 0, "lease", fmt.Sprintf("lease %s %s %s", id, state, msg))
	})
	if err != nil {
		return false, fmt.Errorf("update lease %s: %w", id, err)
	}
	if ok {
		s.leaseChanged(id)
	}
	return ok, nil
}

// ResetTimers sets last_activity_at of each ready lease and last_poll_at of
// each lease before ready to now, so a server restart does not expire or
// cancel leases.
func (s *Store) ResetTimers(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE leases SET last_activity_at = ? WHERE state = 'ready'`, now.UnixMilli()); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE leases SET last_poll_at = ?
		WHERE state IN ('queued', 'assigned', 'provisioning', 'preparing')`, now.UnixMilli())
	return err
}

// RecentDurations returns the durations (from ready to end) of the last n
// ended leases on the machines ("<provider>/<machine>"), or, with a tool, of
// the last n ended leases that asked for that tool.
func (s *Store) RecentDurations(ctx context.Context, templates []string, tool string, n int) ([]time.Duration, error) {
	query := `SELECT ended_at - ready_at FROM leases WHERE ready_at IS NOT NULL AND ended_at IS NOT NULL AND `
	var args []any
	if tool != "" {
		// Tool names have no LIKE wildcards (config.ValidName).
		query += `EXISTS (SELECT 1 FROM json_each(leases.tools) WHERE value = ? OR value LIKE ?)`
		args = append(args, tool, tool+":%")
	} else {
		query += `provider || '/' || template IN (SELECT value FROM json_each(?))`
		args = append(args, jsonText(nonNil(templates)))
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY ended_at DESC LIMIT ?`, append(args, n)...)
	if err != nil {
		return nil, fmt.Errorf("read lease durations: %w", err)
	}
	defer rows.Close()
	var out []time.Duration
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, time.Duration(d)*time.Millisecond)
	}
	return out, rows.Err()
}

// MachineTunnelPorts returns the machine ports of the tunnels of other active
// leases on a machine.
func (s *Store) MachineTunnelPorts(ctx context.Context, machineID int64, exceptLease string) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.machine_port FROM lease_tunnels t JOIN leases l ON l.id = t.lease_id
		WHERE l.machine_id = ? AND l.id != ? AND l.state NOT IN ('released', 'expired', 'cancelled', 'failed')`, machineID, exceptLease)
	if err != nil {
		return nil, fmt.Errorf("read machine tunnels: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// SetTunnels replaces the tunnels of a ready lease.
func (s *Store) SetTunnels(ctx context.Context, id string, req []protocol.TunnelRequest, ports map[string]int, now time.Time) (bool, error) {
	ok := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE leases SET tunnels = ?, revision = revision + 1 WHERE id = ? AND state = 'ready'`, jsonText(req), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM lease_tunnels WHERE lease_id = ?`, id); err != nil {
			return err
		}
		for name, port := range ports {
			if _, err := tx.ExecContext(ctx, `INSERT INTO lease_tunnels (lease_id, name, machine_port) VALUES (?, ?, ?)`, id, name, port); err != nil {
				return err
			}
		}
		ok = true
		return insertEvent(ctx, tx, now, id, 0, "lease", fmt.Sprintf("lease %s tunnels: %v", id, ports))
	})
	if err != nil {
		return false, fmt.Errorf("set tunnels of lease %s: %w", id, err)
	}
	if ok {
		// The lease response has the tunnel ports, so long polls wake.
		s.leaseChanged(id)
	}
	return ok, nil
}

// Requeue returns a lease that was not ready to the queue (design, section 6).
// Its rooms get roomState (free, broken, or dirty with cleanupData). With
// attempt, the lease's attempts increase; at maxAttempts it fails instead.
// It reports the new state, or "" if the lease was not in one of from.
func (s *Store) Requeue(ctx context.Context, id string, from []string, reason string, attempt bool, maxAttempts int,
	roomState, cleanupData string, now time.Time) (string, error) {
	newState := ""
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var attempts int
		if err := tx.QueryRowContext(ctx, `SELECT state, attempts FROM leases WHERE id = ?`, id).Scan(&state, &attempts); err != nil {
			return err
		}
		if !contains(from, state) {
			return nil
		}
		if attempt {
			attempts++
		}
		// The lease can get another room, so the old room keeps no lease_id;
		// its cleanup_data has the lease data for the cleanup.
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET state = ?, lease_id = '', cleanup_data = ? WHERE `+leaseRooms,
			roomState, cleanupData, id, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM lease_tunnels WHERE lease_id = ?`, id); err != nil {
			return err
		}
		if attempts >= maxAttempts {
			newState = protocol.LeaseFailed
			_, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'failed', end_state = 'failed', end_reason = ?, ended_at = ?,
				attempts = ?, revision = revision + 1 WHERE id = ?`, reason, now.UnixMilli(), attempts, id)
			if err != nil {
				return err
			}
			return insertEvent(ctx, tx, now, id, 0, "lease", fmt.Sprintf("lease %s %s → failed after %d attempts: %s", id, state, attempts, reason))
		}
		newState = protocol.LeaseQueued
		// The screen tools that started belong to the old room: its cleanup
		// stopped them, or its cleanup_data has them.
		_, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'queued', machine_id = NULL, room_name = '', provider = '', template = '',
			began_at = NULL, started = '[]', attempts = ?, reason = ?, revision = revision + 1 WHERE id = ?`, attempts, reason, id)
		if err != nil {
			return err
		}
		return insertEvent(ctx, tx, now, id, 0, "lease", fmt.Sprintf("lease %s %s → queued (attempt %d): %s", id, state, attempts, reason))
	})
	if err != nil {
		return "", fmt.Errorf("requeue lease %s: %w", id, err)
	}
	if newState != "" {
		s.leaseChanged(id)
	}
	return newState, nil
}

// FailLease ends a lease at once, without cleanup (the machine is lost or
// unreachable). Its rooms get roomState.
func (s *Store) FailLease(ctx context.Context, id string, from []string, reason, roomState, cleanupData string, now time.Time) (bool, error) {
	ok := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM leases WHERE id = ?`, id).Scan(&state); err != nil {
			return err
		}
		if !contains(from, state) {
			return nil
		}
		leaseIDCol := "''"
		if roomState != protocol.RoomFree {
			leaseIDCol = "lease_id"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET state = ?, lease_id = `+leaseIDCol+`, cleanup_data = ? WHERE `+leaseRooms,
			roomState, cleanupData, id, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM lease_tunnels WHERE lease_id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'failed', end_state = 'failed', end_reason = ?, ended_at = ?,
			revision = revision + 1 WHERE id = ?`, reason, now.UnixMilli(), id); err != nil {
			return err
		}
		ok = true
		return insertEvent(ctx, tx, now, id, 0, "lease", fmt.Sprintf("lease %s %s → failed: %s", id, state, reason))
	})
	if err != nil {
		return false, fmt.Errorf("fail lease %s: %w", id, err)
	}
	if ok {
		s.leaseChanged(id)
	}
	return ok, nil
}

// PlaceLease is an admin's move of a queued lease inside the one waiting line
// (order: scheduler.Order): first, or before another queued lease.
// The lease and the leases before it get manual positions, so they keep this
// order until they start. It reports false when the lease is not queued.
func (s *Store) PlaceLease(ctx context.Context, id, before string, now time.Time) (bool, error) {
	var moved bool
	var changed []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM leases WHERE id = ? AND state = 'queued'`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, priority, manual_position, created_at FROM leases WHERE state = 'queued'`)
		if err != nil {
			return err
		}
		var queue []scheduler.Lease
		for rows.Next() {
			var l scheduler.Lease
			var created int64
			if err := rows.Scan(&l.ID, &l.Priority, &l.ManualPosition, &created); err != nil {
				rows.Close()
				return err
			}
			l.CreatedAt = time.UnixMilli(created)
			queue = append(queue, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		scheduler.Order(queue)
		ids := make([]string, 0, len(queue))
		placed := 0
		for _, l := range queue {
			if l.ID != id {
				ids = append(ids, l.ID)
				if l.ManualPosition > 0 {
					placed++
				}
			}
		}
		at := 0
		if before != "" {
			at = slices.Index(ids, before)
			if at < 0 {
				return fmt.Errorf("%w: lease %s is not queued", ErrNotFound, before)
			}
		}
		ids = slices.Insert(ids, at, id)
		// The placed leases are first in the order; all of them and the moved
		// lease get new positions, so no two have the same one.
		for i, lid := range ids[:max(at, placed)+1] {
			if _, err := tx.ExecContext(ctx, `UPDATE leases SET manual_position = ?, revision = revision + 1 WHERE id = ?`, i+1, lid); err != nil {
				return err
			}
			changed = append(changed, lid)
		}
		moved = true
		return nil
	})
	for _, lid := range changed {
		s.leaseChanged(lid)
	}
	return moved, err
}

// SetPriority changes the lane of a queued lease. It reports false when the
// lease is not queued.
func (s *Store) SetPriority(ctx context.Context, id string, priority int) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE leases SET priority = ?, revision = revision + 1 WHERE id = ? AND state = 'queued'`, priority, id)
	if err != nil {
		return false, fmt.Errorf("set priority: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.leaseChanged(id)
	}
	return n > 0, nil
}

// UserTokenIDs returns the IDs of a user's tokens (also revoked ones).
func (s *Store) UserTokenIDs(ctx context.Context, userID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tokens WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TokenUserID returns the user of a token (0 for a machine token).
func (s *Store) TokenUserID(ctx context.Context, tokenID int64) (int64, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM tokens WHERE id = ?`, tokenID).Scan(&id)
	return id.Int64, err
}

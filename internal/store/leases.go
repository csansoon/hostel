package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

type Lease struct {
	ID             string
	TokenID        int64
	State          string
	Revision       int64
	Position       int
	Reason         string
	Platform       string
	Devices        []string
	Task           string
	Priority       int
	RequiredLabels []string
	Tunnels        []protocol.TunnelRequest
	Fresh          bool
	PublicKey      string
	GuestHost      string
	Repo           string
	Worktree       string
	Provider       string
	Machine        string
	RoomID         int64
	SlotName       string
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
}

// Final reports whether the lease state is final.
func Final(state string) bool {
	switch state {
	case protocol.LeaseReleased, protocol.LeaseExpired, protocol.LeaseCancelled, protocol.LeaseFailed:
		return true
	}
	return false
}

const leaseColumns = `id, token_id, state, revision, position, reason, platform, devices, task, priority,
	required_labels, tunnels, fresh, public_key, guest_host, repo, worktree, provider, machine, room_id,
	slot_name, attempts, began_at, created_at, last_poll_at, last_activity_at, ready_at, expires_at,
	ended_at, end_state, end_reason`

func scanLease(row interface{ Scan(...any) error }) (Lease, error) {
	var l Lease
	var devices, labels, tunnels string
	var roomID, began, ready, expires, ended sql.NullInt64
	var created, poll, activity int64
	err := row.Scan(&l.ID, &l.TokenID, &l.State, &l.Revision, &l.Position, &l.Reason, &l.Platform, &devices,
		&l.Task, &l.Priority, &labels, &tunnels, &l.Fresh, &l.PublicKey, &l.GuestHost, &l.Repo, &l.Worktree,
		&l.Provider, &l.Machine, &roomID, &l.SlotName, &l.Attempts, &began, &created, &poll, &activity,
		&ready, &expires, &ended, &l.EndState, &l.EndReason)
	if err != nil {
		return l, err
	}
	for _, f := range []struct {
		data string
		dst  any
	}{{devices, &l.Devices}, {labels, &l.RequiredLabels}, {tunnels, &l.Tunnels}} {
		if err := json.Unmarshal([]byte(f.data), f.dst); err != nil {
			return l, fmt.Errorf("lease %s: decode: %w", l.ID, err)
		}
	}
	l.RoomID = roomID.Int64
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
		_, err := tx.ExecContext(ctx, `INSERT INTO leases (id, token_id, state, platform, devices, task, priority,
			required_labels, tunnels, fresh, public_key, guest_host, repo, worktree, created_at, last_poll_at, last_activity_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.ID, l.TokenID, protocol.LeaseQueued, l.Platform, jsonText(nonNil(l.Devices)), l.Task, l.Priority,
			jsonText(nonNil(l.RequiredLabels)), jsonText(l.Tunnels), l.Fresh, l.PublicKey, l.GuestHost, l.Repo, l.Worktree,
			now.UnixMilli(), now.UnixMilli(), now.UnixMilli())
		if err != nil {
			return err
		}
		return insertEvent(ctx, tx, now, l.ID, 0, "lease", fmt.Sprintf("lease %s queued for %s: %s", l.ID, l.Platform, l.Task))
	})
	if err != nil {
		return fmt.Errorf("insert lease: %w", err)
	}
	s.leaseChanged(l.ID)
	return nil
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

// Assign reserves slots of a room for a queued lease and gives its tunnels
// their room ports (compare-and-set: the lease must be queued and the slots
// free). It reports whether the assignment happened.
func (s *Store) Assign(ctx context.Context, leaseID string, room Room, slots []string, ports map[string]int, now time.Time) (bool, error) {
	ok := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'assigned', room_id = ?, slot_name = ?, provider = ?,
			machine = ?, position = 0, reason = '', revision = revision + 1 WHERE id = ? AND state = 'queued'`,
			room.ID, slots[0], room.Provider, room.Machine, leaseID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		for _, sl := range slots {
			res, err := tx.ExecContext(ctx, `UPDATE slots SET state = 'assigned', lease_id = ? WHERE room_id = ? AND name = ? AND state = 'free'`,
				leaseID, room.ID, sl)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errSlotTaken
			}
		}
		for name, port := range ports {
			if _, err := tx.ExecContext(ctx, `INSERT INTO lease_tunnels (lease_id, name, room_port) VALUES (?, ?, ?)`, leaseID, name, port); err != nil {
				return err
			}
		}
		ok = true
		return insertEvent(ctx, tx, now, leaseID, room.ID, "lease",
			fmt.Sprintf("lease %s assigned to room %s slot %s", leaseID, room.Name, strings.Join(slots, ",")))
	})
	if errors.Is(err, errSlotTaken) {
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

var errSlotTaken = errors.New("slot taken")

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

// EndLease ends a lease that is in one of from. A lease with a slot goes to
// cleaning with endState; a lease without a slot goes to endState at once.
func (s *Store) EndLease(ctx context.Context, id string, from []string, endState, reason string, now time.Time) (bool, error) {
	ok := false
	// The decision reads room_id in the same transaction as the update, so a
	// concurrent assignment cannot leave a reserved slot behind.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var roomID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT state, room_id FROM leases WHERE id = ?`, id).Scan(&state, &roomID); err != nil {
			return err
		}
		if !contains(from, state) {
			return nil
		}
		var msg string
		if roomID.Valid {
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

// FinishCleaning moves a cleaning lease to its final state and sets its slots
// to slotState (free, or broken after a failed cleanup; a broken slot keeps
// cleanupData for the repair).
func (s *Store) FinishCleaning(ctx context.Context, id, slotState, cleanupData string, now time.Time) (bool, error) {
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
		if slotState != protocol.SlotFree {
			leaseIDCol = "lease_id"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state = ?, lease_id = `+leaseIDCol+`, cleanup_data = ? WHERE `+leaseSlots,
			slotState, cleanupData, id, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM lease_tunnels WHERE lease_id = ?`, id); err != nil {
			return err
		}
		return insertEvent(ctx, tx, now, id, 0, "lease", fmt.Sprintf("lease %s cleaned; slot %s", id, slotState))
	})
	if err != nil {
		return false, fmt.Errorf("finish cleaning of lease %s: %w", id, err)
	}
	if ok {
		s.leaseChanged(id)
	}
	return ok, nil
}

// leaseSlots selects the slots of a lease in its current room (two arguments:
// the lease ID twice). A slot of an earlier room never matches.
const leaseSlots = `lease_id = ? AND room_id = (SELECT room_id FROM leases WHERE id = ?)`

// SetSlots sets the state of the lease's slots.
func (s *Store) SetSlots(ctx context.Context, leaseID, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE slots SET state = ? WHERE `+leaseSlots, state, leaseID, leaseID)
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

// LeaseTunnels returns the room port of each tunnel of a lease.
func (s *Store) LeaseTunnels(ctx context.Context, id string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, room_port FROM lease_tunnels WHERE lease_id = ?`, id)
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
// ended leases of a platform.
func (s *Store) RecentDurations(ctx context.Context, platform string, n int) ([]time.Duration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ended_at - ready_at FROM leases
		WHERE platform = ? AND ready_at IS NOT NULL AND ended_at IS NOT NULL ORDER BY ended_at DESC LIMIT ?`, platform, n)
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

// RoomTunnelPorts returns the room ports of the tunnels of other active
// leases on a room.
func (s *Store) RoomTunnelPorts(ctx context.Context, roomID int64, exceptLease string) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.room_port FROM lease_tunnels t JOIN leases l ON l.id = t.lease_id
		WHERE l.room_id = ? AND l.id != ? AND l.state NOT IN ('released', 'expired', 'cancelled', 'failed')`, roomID, exceptLease)
	if err != nil {
		return nil, fmt.Errorf("read room tunnels: %w", err)
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO lease_tunnels (lease_id, name, room_port) VALUES (?, ?, ?)`, id, name, port); err != nil {
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
// Its slots get slotState (free, broken, or dirty with cleanupData). With
// attempt, the lease's attempts increase; at maxAttempts it fails instead.
// It reports the new state, or "" if the lease was not in one of from.
func (s *Store) Requeue(ctx context.Context, id string, from []string, reason string, attempt bool, maxAttempts int,
	slotState, cleanupData string, now time.Time) (string, error) {
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
		// The lease can get another slot, so the old slot keeps no lease_id;
		// its cleanup_data has the lease data for the cleanup.
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state = ?, lease_id = '', cleanup_data = ? WHERE `+leaseSlots,
			slotState, cleanupData, id, id); err != nil {
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
		_, err := tx.ExecContext(ctx, `UPDATE leases SET state = 'queued', room_id = NULL, slot_name = '', provider = '', machine = '',
			began_at = NULL, attempts = ?, reason = ?, revision = revision + 1 WHERE id = ?`, attempts, reason, id)
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

// FailLease ends a lease at once, without cleanup (the room is lost or
// unreachable). Its slots get slotState.
func (s *Store) FailLease(ctx context.Context, id string, from []string, reason, slotState, cleanupData string, now time.Time) (bool, error) {
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
		if slotState != protocol.SlotFree {
			leaseIDCol = "lease_id"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE slots SET state = ?, lease_id = `+leaseIDCol+`, cleanup_data = ? WHERE `+leaseSlots,
			slotState, cleanupData, id, id); err != nil {
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

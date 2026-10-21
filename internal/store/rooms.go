package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Room struct {
	ID               int64
	Name             string
	Provider         string
	Machine          string
	ProviderRef      string
	State            string
	OS               string
	Arch             string
	Home             string
	SSHHostKey       string
	HelperSHA256     string
	ProviderDeadline time.Time
	WarmUntil        time.Time
	Drained          bool
	BrokenReason     string
	CreatedAt        time.Time
	StateChangedAt   time.Time
}

type Slot struct {
	RoomID      int64
	Name        string
	State       string
	LeaseID     string
	CleanupData string
}

type RoomPlatform struct {
	RoomID        int64
	Platform      string
	ScriptsSHA256 string
	ProvisionedAt time.Time
}

const roomColumns = `id, name, provider, machine, provider_ref, state, os, arch, home, ssh_host_key,
	helper_sha256, provider_deadline, warm_until, drained, broken_reason, created_at, state_changed_at`

func scanRoom(row interface{ Scan(...any) error }) (Room, error) {
	var r Room
	var deadline, warm sql.NullInt64
	var created, changed int64
	err := row.Scan(&r.ID, &r.Name, &r.Provider, &r.Machine, &r.ProviderRef, &r.State, &r.OS, &r.Arch, &r.Home,
		&r.SSHHostKey, &r.HelperSHA256, &deadline, &warm, &r.Drained, &r.BrokenReason, &created, &changed)
	r.ProviderDeadline, r.WarmUntil = fromMS(deadline), fromMS(warm)
	r.CreatedAt, r.StateChangedAt = time.UnixMilli(created), time.UnixMilli(changed)
	return r, err
}

// InsertRoom inserts a room row with its slots. If name is empty, the name is
// hostel-<serverID>-<id>, made from the new row ID in the same transaction.
func (s *Store) InsertRoom(ctx context.Context, r Room, serverID string, slots []string, now time.Time) (Room, error) {
	r.CreatedAt, r.StateChangedAt = now, now
	err := s.tx(ctx, func(tx *sql.Tx) error {
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("hostel-%s-pending", serverID)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO rooms (name, provider, machine, state, os, created_at, state_changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, name, r.Provider, r.Machine, r.State, r.OS, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			return err
		}
		if r.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if r.Name == "" {
			r.Name = fmt.Sprintf("hostel-%s-%d", serverID, r.ID)
			if _, err := tx.ExecContext(ctx, `UPDATE rooms SET name = ? WHERE id = ?`, r.Name, r.ID); err != nil {
				return err
			}
		}
		for _, sl := range slots {
			if _, err := tx.ExecContext(ctx, `INSERT INTO slots (room_id, name, state) VALUES (?, ?, 'free')`, r.ID, sl); err != nil {
				return err
			}
		}
		return insertEvent(ctx, tx, now, "", r.ID, "room", "room "+r.Name+" "+r.State)
	})
	if err != nil {
		return Room{}, fmt.Errorf("insert room: %w", err)
	}
	return r, nil
}

// EnsureSlots adds the slots that a room does not have yet.
func (s *Store) EnsureSlots(ctx context.Context, roomID int64, slots []string) error {
	for _, sl := range slots {
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO slots (room_id, name, state) VALUES (?, ?, 'free')`, roomID, sl); err != nil {
			return fmt.Errorf("insert slot: %w", err)
		}
	}
	return nil
}

func (s *Store) Room(ctx context.Context, id int64) (Room, error) {
	r, err := scanRoom(s.db.QueryRowContext(ctx, `SELECT `+roomColumns+` FROM rooms WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Room{}, ErrNotFound
	}
	if err != nil {
		return Room{}, fmt.Errorf("read room %d: %w", id, err)
	}
	return r, nil
}

// Rooms returns all rooms that are not destroyed, in ID order.
func (s *Store) Rooms(ctx context.Context) ([]Room, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+roomColumns+` FROM rooms WHERE state != 'destroyed' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	defer rows.Close()
	var out []Room
	for rows.Next() {
		r, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetRoomState changes the state if it is one of from (compare-and-set). It
// reports whether the row changed, and records an event.
func (s *Store) SetRoomState(ctx context.Context, id int64, from []string, to, reason string, now time.Time) (bool, error) {
	changed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state, name string
		if err := tx.QueryRowContext(ctx, `SELECT state, name FROM rooms WHERE id = ?`, id).Scan(&state, &name); err != nil {
			return err
		}
		if !contains(from, state) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rooms SET state = ?, broken_reason = ?, state_changed_at = ? WHERE id = ?`,
			to, reason, now.UnixMilli(), id); err != nil {
			return err
		}
		changed = true
		msg := "room " + name + " " + state + " → " + to
		if reason != "" {
			msg += ": " + reason
		}
		return insertEvent(ctx, tx, now, "", id, "room", msg)
	})
	if err != nil {
		return false, fmt.Errorf("set room %d state: %w", id, err)
	}
	return changed, nil
}

// SetRoomProbe records the probe result.
func (s *Store) SetRoomProbe(ctx context.Context, id int64, osName, arch, home string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET os = ?, arch = ?, home = ? WHERE id = ?`, osName, arch, home, id)
	return err
}

// PinHostKey records the room host key if none is recorded (compare-and-set).
func (s *Store) PinHostKey(ctx context.Context, id int64, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET ssh_host_key = ? WHERE id = ? AND ssh_host_key = ''`, key, id)
	return err
}

func (s *Store) SetHelperSHA256(ctx context.Context, id int64, sum string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET helper_sha256 = ? WHERE id = ?`, sum, id)
	return err
}

// Slots returns the slots of all rooms that are not destroyed.
func (s *Store) Slots(ctx context.Context) ([]Slot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.room_id, s.name, s.state, s.lease_id, s.cleanup_data
		FROM slots s JOIN rooms r ON r.id = s.room_id WHERE r.state != 'destroyed' ORDER BY s.room_id, s.name`)
	if err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	defer rows.Close()
	var out []Slot
	for rows.Next() {
		var sl Slot
		if err := rows.Scan(&sl.RoomID, &sl.Name, &sl.State, &sl.LeaseID, &sl.CleanupData); err != nil {
			return nil, err
		}
		out = append(out, sl)
	}
	return out, rows.Err()
}

// RoomPlatforms returns the room_platforms rows of all rooms.
func (s *Store) RoomPlatforms(ctx context.Context) ([]RoomPlatform, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT room_id, platform, scripts_sha256, provisioned_at FROM room_platforms ORDER BY room_id, platform`)
	if err != nil {
		return nil, fmt.Errorf("list room platforms: %w", err)
	}
	defer rows.Close()
	var out []RoomPlatform
	for rows.Next() {
		var rp RoomPlatform
		var at int64
		if err := rows.Scan(&rp.RoomID, &rp.Platform, &rp.ScriptsSHA256, &at); err != nil {
			return nil, err
		}
		rp.ProvisionedAt = time.UnixMilli(at)
		out = append(out, rp)
	}
	return out, rows.Err()
}

// SetRoomPlatform records that a room is ready for a platform with these scripts.
func (s *Store) SetRoomPlatform(ctx context.Context, roomID int64, platform, sum string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO room_platforms (room_id, platform, scripts_sha256, provisioned_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (room_id, platform) DO UPDATE SET scripts_sha256 = excluded.scripts_sha256, provisioned_at = excluded.provisioned_at`,
		roomID, platform, sum, now.UnixMilli())
	return err
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ms converts a time to Unix milliseconds; the zero time is NULL.
func ms(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// fromMS converts nullable Unix milliseconds to a time; NULL is the zero time.
func fromMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

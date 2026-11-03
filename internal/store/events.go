package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type Event struct {
	ID      int64
	Time    time.Time
	LeaseID string
	RoomID  int64
	Kind    string
	Message string
}

// eventRoom is the room of an event: the given room, else the room of the
// lease at this time (so the room details show the lease's events on this
// room only, also when the lease moves to another room later).
const eventRoom = `CASE WHEN ? != 0 THEN ? ELSE COALESCE((SELECT room_id FROM leases WHERE id = ?), 0) END`

func insertEvent(ctx context.Context, tx *sql.Tx, now time.Time, leaseID string, roomID int64, kind, msg string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events (time, lease_id, room_id, kind, message) VALUES (?, ?, `+eventRoom+`, ?, ?)`,
		now.UnixMilli(), leaseID, roomID, roomID, leaseID, kind, msg)
	return err
}

// AddEvent appends an event.
func (s *Store) AddEvent(ctx context.Context, now time.Time, leaseID string, roomID int64, kind, msg string) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return insertEvent(ctx, tx, now, leaseID, roomID, kind, msg) })
}

// AddEventBy appends an event that a user caused (an admin action).
func (s *Store) AddEventBy(ctx context.Context, now time.Time, leaseID string, roomID int64, kind, msg string, userID int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO events (time, lease_id, room_id, kind, message, user_id) VALUES (?, ?, `+eventRoom+`, ?, ?, ?)`,
		now.UnixMilli(), leaseID, roomID, roomID, leaseID, kind, msg, userID)
	return err
}

// RoomEvents returns the last n events of a room, the oldest first (with the
// events of the leases while they were on the room).
func (s *Store) RoomEvents(ctx context.Context, roomID int64, n int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, time, lease_id, room_id, kind, message FROM
		(SELECT * FROM events WHERE room_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, roomID, n)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.LeaseID, &e.RoomID, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Events returns the last n events, oldest first.
func (s *Store) Events(ctx context.Context, n int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, time, lease_id, room_id, kind, message FROM
		(SELECT * FROM events ORDER BY id DESC LIMIT ?) ORDER BY id`, n)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.LeaseID, &e.RoomID, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(t)
		out = append(out, e)
	}
	return out, rows.Err()
}

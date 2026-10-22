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

func insertEvent(ctx context.Context, tx *sql.Tx, now time.Time, leaseID string, roomID int64, kind, msg string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events (time, lease_id, room_id, kind, message) VALUES (?, ?, ?, ?, ?)`,
		now.UnixMilli(), leaseID, roomID, kind, msg)
	return err
}

// AddEvent appends an event.
func (s *Store) AddEvent(ctx context.Context, now time.Time, leaseID string, roomID int64, kind, msg string) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return insertEvent(ctx, tx, now, leaseID, roomID, kind, msg) })
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

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrConflict: the configuration changed since the base revision.
var ErrConflict = errors.New("the configuration changed in the meantime")

// ConfigRevision is a revision of the configuration document.
type ConfigRevision struct {
	Revision  int
	Document  string
	UserID    int64
	User      string // "" for the server (the import)
	CreatedAt time.Time
}

// LatestConfig returns the latest revision; ErrNotFound if there is none.
func (s *Store) LatestConfig(ctx context.Context) (ConfigRevision, error) {
	var c ConfigRevision
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT revision, document, user_id, created_at FROM config_revisions
		ORDER BY revision DESC LIMIT 1`).Scan(&c.Revision, &c.Document, &c.UserID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.CreatedAt = time.UnixMilli(created)
	return c, err
}

// AddConfig saves a new revision after base (0: the first). ErrConflict if
// the latest revision is not base.
func (s *Store) AddConfig(ctx context.Context, base int, document string, userID int64, now time.Time) (int, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var latest int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) FROM config_revisions`).Scan(&latest); err != nil {
			return err
		}
		if latest != base {
			return fmt.Errorf("%w: revision %d is the latest, not %d", ErrConflict, latest, base)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO config_revisions (revision, document, user_id, created_at) VALUES (?, ?, ?, ?)`,
			base+1, document, userID, now.UnixMilli())
		return err
	})
	if err != nil {
		return 0, err
	}
	return base + 1, nil
}

// ConfigHistory returns the revisions, the latest first, without documents.
func (s *Store) ConfigHistory(ctx context.Context, limit int) ([]ConfigRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.revision, c.user_id, COALESCE(u.name, ''), c.created_at
		FROM config_revisions c LEFT JOIN users u ON u.id = c.user_id ORDER BY c.revision DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfigRevision
	for rows.Next() {
		var c ConfigRevision
		var created int64
		if err := rows.Scan(&c.Revision, &c.UserID, &c.User, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = time.UnixMilli(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConfigAt returns one revision.
func (s *Store) ConfigAt(ctx context.Context, revision int) (ConfigRevision, error) {
	var c ConfigRevision
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT revision, document, user_id, created_at FROM config_revisions WHERE revision = ?`,
		revision).Scan(&c.Revision, &c.Document, &c.UserID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.CreatedAt = time.UnixMilli(created)
	return c, err
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Platform revision states.
const (
	PlatformFiles   = "files"
	PlatformRemoved = "removed"
	PlatformDefault = "default"
)

// PlatformRevision is a revision of one platform.
type PlatformRevision struct {
	Platform  string
	Revision  int
	State     string
	Files     map[string]string
	UserID    int64
	User      string
	CreatedAt time.Time
}

// LatestPlatforms returns the latest revision of each platform that has one.
func (s *Store) LatestPlatforms(ctx context.Context) (map[string]PlatformRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.platform, p.revision, p.state, p.files, p.user_id, COALESCE(u.name, ''), p.created_at
		FROM platform_revisions p LEFT JOIN users u ON u.id = p.user_id
		WHERE p.revision = (SELECT MAX(revision) FROM platform_revisions q WHERE q.platform = p.platform)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PlatformRevision{}
	for rows.Next() {
		var r PlatformRevision
		var files string
		var created int64
		if err := rows.Scan(&r.Platform, &r.Revision, &r.State, &files, &r.UserID, &r.User, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(files), &r.Files); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created)
		out[r.Platform] = r
	}
	return out, rows.Err()
}

// AddPlatformRevision saves the next revision of a platform.
func (s *Store) AddPlatformRevision(ctx context.Context, platform, state string, files map[string]string, userID int64, now time.Time) (int, error) {
	data, err := json.Marshal(files)
	if err != nil {
		return 0, err
	}
	var rev int
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1 FROM platform_revisions WHERE platform = ?`, platform).Scan(&rev); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO platform_revisions (platform, revision, state, files, user_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			platform, rev, state, string(data), userID, now.UnixMilli())
		return err
	})
	return rev, err
}

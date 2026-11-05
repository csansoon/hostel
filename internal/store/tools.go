package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// Tool revision states.
const (
	ToolFiles   = "files"
	ToolRemoved = "removed"
	ToolDefault = "default"
)

// ToolRevision is a revision of one tool.
type ToolRevision struct {
	Tool      string
	Revision  int
	State     string
	Files     map[string]string
	UserID    int64
	User      string
	CreatedAt time.Time
}

// LatestTools returns the latest revision of each tool that has one.
func (s *Store) LatestTools(ctx context.Context) (map[string]ToolRevision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.tool, t.revision, t.state, t.files, t.user_id, COALESCE(u.name, ''), t.created_at
		FROM tool_revisions t LEFT JOIN users u ON u.id = t.user_id
		WHERE t.revision = (SELECT MAX(revision) FROM tool_revisions q WHERE q.tool = t.tool)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ToolRevision{}
	for rows.Next() {
		var r ToolRevision
		var files string
		var created int64
		if err := rows.Scan(&r.Tool, &r.Revision, &r.State, &files, &r.UserID, &r.User, &created); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(files), &r.Files); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created)
		out[r.Tool] = r
	}
	return out, rows.Err()
}

// AddToolRevision saves the next revision of a tool.
func (s *Store) AddToolRevision(ctx context.Context, tool, state string, files map[string]string, userID int64, now time.Time) (int, error) {
	data, err := json.Marshal(files)
	if err != nil {
		return 0, err
	}
	var rev int
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision), 0) + 1 FROM tool_revisions WHERE tool = ?`, tool).Scan(&rev); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO tool_revisions (tool, revision, state, files, user_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			tool, rev, state, string(data), userID, now.UnixMilli())
		return err
	})
	return rev, err
}

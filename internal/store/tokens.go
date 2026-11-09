package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Token is a token with its effective role: for a user token, Name and Role
// are the user's (admin or member); for a machine token, Role is RoleMachine.
type Token struct {
	ID        int64
	Name      string
	Role      string
	Template  string
	UserID    int64
	Computer  string
	CreatedAt time.Time
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateMachineToken stores a new machine token for a machine and returns it with
// its secret. Only the SHA-256 of the secret is stored, so the secret is shown
// only once.
func (s *Store) CreateMachineToken(ctx context.Context, name, template string, now time.Time) (Token, string, error) {
	secret, err := newSecret()
	if err != nil {
		return Token{}, "", err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (name, role, template, sha256, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, RoleMachine, template, hashSecret(secret), now.UnixMilli())
	if err != nil {
		return Token{}, "", fmt.Errorf("insert token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Token{}, "", err
	}
	return Token{ID: id, Name: name, Role: RoleMachine, Template: template, CreatedAt: now}, secret, nil
}

// TokenBySecret returns the token with this secret if it is not revoked.
func (s *Store) TokenBySecret(ctx context.Context, secret string) (Token, error) {
	var t Token
	var created int64
	var userID sql.NullInt64
	var userName, userRole sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT t.id, t.name, t.role, t.template, t.computer, t.created_at, u.id, u.name, u.role
		FROM tokens t LEFT JOIN users u ON u.id = t.user_id AND u.removed_at IS NULL
		WHERE t.sha256 = ? AND t.revoked_at IS NULL`,
		hashSecret(secret)).Scan(&t.ID, &t.Name, &t.Role, &t.Template, &t.Computer, &created, &userID, &userName, &userRole)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, fmt.Errorf("read token: %w", err)
	}
	t.CreatedAt = time.UnixMilli(created)
	if t.Role == tokenUser {
		if !userID.Valid {
			return Token{}, ErrNotFound
		}
		t.UserID, t.Name, t.Role = userID.Int64, userName.String, userRole.String
	}
	return t, nil
}

// TokenActive reports whether the token with the ID exists and is not revoked.
func (s *Store) TokenActive(ctx context.Context, id int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE id = ? AND revoked_at IS NULL`, id).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("read token: %w", err)
	}
	return n > 0, nil
}

// CountTokens returns the number of tokens, revoked or not.
func (s *Store) CountTokens(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens`).Scan(&n)
	return n, err
}

// RevokeTemplateTokens revokes the machine tokens of a machine
// ("<provider>/<machine>") and returns their IDs.
func (s *Store) RevokeTemplateTokens(ctx context.Context, template string, now time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tokens WHERE role = ? AND template = ? AND revoked_at IS NULL`, RoleMachine, template)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now.UnixMilli(), id); err != nil {
			return nil, err
		}
	}
	return ids, rows.Err()
}

// RevokeToken revokes a token. It returns ErrNotFound if no active token has the ID.
func (s *Store) RevokeToken(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

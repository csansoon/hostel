package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

type Token struct {
	ID        int64
	Name      string
	Role      string
	Machine   string
	CreatedAt time.Time
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken stores a new token and returns it with its secret. Only the
// SHA-256 of the secret is stored, so the secret is shown only once.
func (s *Store) CreateToken(ctx context.Context, name, role, machine string, now time.Time) (Token, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Token{}, "", err
	}
	secret := "hostel_" + base64.RawURLEncoding.EncodeToString(b)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (name, role, machine, sha256, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, role, machine, hashSecret(secret), now.UnixMilli())
	if err != nil {
		return Token{}, "", fmt.Errorf("insert token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Token{}, "", err
	}
	return Token{ID: id, Name: name, Role: role, Machine: machine, CreatedAt: now}, secret, nil
}

// TokenBySecret returns the token with this secret if it is not revoked.
func (s *Store) TokenBySecret(ctx context.Context, secret string) (Token, error) {
	var t Token
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, role, machine, created_at FROM tokens WHERE sha256 = ? AND revoked_at IS NULL`,
		hashSecret(secret)).Scan(&t.ID, &t.Name, &t.Role, &t.Machine, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, fmt.Errorf("read token: %w", err)
	}
	t.CreatedAt = time.UnixMilli(created)
	return t, nil
}

// CountTokens returns the number of tokens, revoked or not.
func (s *Store) CountTokens(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens`).Scan(&n)
	return n, err
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

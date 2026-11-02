package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Token roles: a user token has the role of its user; a room token has
// RoleRoom.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleRoom   = "room"
	tokenUser  = "user"
)

var (
	// ErrNameTaken is returned for a user name that another user has.
	ErrNameTaken = errors.New("name taken")
	// ErrLastAdmin is returned when a change would leave the server without an admin.
	ErrLastAdmin = errors.New("last admin")
	// ErrInviteInvalid is returned for an invite that does not exist, was used,
	// or expired.
	ErrInviteInvalid = errors.New("invite invalid")
	// ErrInviteNameTaken: the invite's preset name belongs to another user;
	// the invite stays unused.
	ErrInviteNameTaken = errors.New("invite name taken")
)

type User struct {
	ID        int64
	Name      string
	Role      string
	CreatedAt time.Time
	Computers []string // computers with an active token
}

// Invite kinds.
const (
	InviteUser     = "user"
	InviteComputer = "computer"
	InviteMachine  = "machine"
)

type Invite struct {
	Kind string
	Role string
	Name string
	// Machine is the machine of a machine invite (JSON).
	Machine   string
	UserID    int64
	CreatedBy int64
	ExpiresAt time.Time
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "hostel_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}

// activeUserID returns the user whose name equals name without case (all of
// Unicode; SQLite's NOCASE folds only ASCII), or 0.
func activeUserID(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, name string) (int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name FROM users WHERE removed_at IS NULL`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			return 0, err
		}
		if strings.EqualFold(n, name) {
			return id, nil
		}
	}
	return 0, rows.Err()
}

func insertUser(ctx context.Context, tx *sql.Tx, name, role string, now time.Time) (int64, error) {
	if id, err := activeUserID(ctx, tx, name); err != nil {
		return 0, err
	} else if id != 0 {
		return 0, ErrNameTaken
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO users (name, role, created_at) VALUES (?, ?, ?)`, name, role, now.UnixMilli())
	if isUnique(err) {
		return 0, ErrNameTaken
	}
	if err != nil {
		return 0, fmt.Errorf("insert user: %w", err)
	}
	return res.LastInsertId()
}

func insertUserToken(ctx context.Context, tx *sql.Tx, userID int64, computer string, now time.Time) (int64, string, error) {
	secret, err := newSecret()
	if err != nil {
		return 0, "", err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO tokens (name, role, machine, sha256, created_at, user_id, computer) VALUES (?, ?, '', ?, ?, ?, ?)`,
		computer, tokenUser, hashSecret(secret), now.UnixMilli(), userID, computer)
	if err != nil {
		return 0, "", fmt.Errorf("insert token: %w", err)
	}
	id, err := res.LastInsertId()
	return id, secret, err
}

// InviteSecretSize is the size of an invite's one-time secret.
const InviteSecretSize = 16

// CreateInvite stores an invite and returns its secret (only its hash is
// stored).
func (s *Store) CreateInvite(ctx context.Context, inv Invite, now time.Time) ([]byte, error) {
	secret := make([]byte, InviteSecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO invites (kind, secret_sha256, role, name, user_id, created_by, created_at, expires_at, machine)
		VALUES (?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, 0), ?, ?, ?)`,
		inv.Kind, hashSecret(string(secret)), inv.Role, inv.Name, inv.UserID, inv.CreatedBy, now.UnixMilli(), inv.ExpiresAt.UnixMilli(), inv.Machine)
	if err != nil {
		return nil, fmt.Errorf("insert invite: %w", err)
	}
	return secret, nil
}

// UseInvite uses an invite once: for a user invite it creates the user (the
// invite's name, else name), for a computer invite it uses the invite's user.
// It returns the user and a new token for the computer.
func (s *Store) UseInvite(ctx context.Context, secret []byte, name, computer string, now time.Time) (User, Token, string, error) {
	var u User
	var tok Token
	var tokSecret string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		var kind, role, invName string
		var userID sql.NullInt64
		err := tx.QueryRowContext(ctx, `SELECT id, kind, role, name, user_id FROM invites
			WHERE secret_sha256 = ? AND used_at IS NULL AND expires_at > ? AND kind != 'machine'`, hashSecret(string(secret)), now.UnixMilli()).
			Scan(&id, &kind, &role, &invName, &userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET used_at = ? WHERE id = ?`, now.UnixMilli(), id); err != nil {
			return err
		}
		switch kind {
		case InviteUser:
			if invName != "" {
				name = invName
			}
			if u.ID, err = insertUser(ctx, tx, name, role, now); err != nil {
				if errors.Is(err, ErrNameTaken) && invName != "" {
					return ErrInviteNameTaken
				}
				return err
			}
			u.Name, u.Role = name, role
		case InviteComputer:
			err := tx.QueryRowContext(ctx, `SELECT id, name, role FROM users WHERE id = ? AND removed_at IS NULL`, userID.Int64).Scan(&u.ID, &u.Name, &u.Role)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInviteInvalid
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("invite %d: unknown kind %q", id, kind)
		}
		tok.ID, tokSecret, err = insertUserToken(ctx, tx, u.ID, computer, now)
		return err
	})
	if err != nil {
		return User{}, Token{}, "", err
	}
	tok.Name, tok.Role, tok.UserID, tok.Computer, tok.CreatedAt = u.Name, u.Role, u.ID, computer, now
	return u, tok, tokSecret, nil
}

// MachineInvite returns a valid machine invite (without using it): its ID,
// its machine (JSON), and its creator. ErrInviteInvalid if there is none.
func (s *Store) MachineInvite(ctx context.Context, secret []byte, now time.Time) (int64, string, int64, error) {
	var id int64
	var machine string
	var createdBy sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, machine, created_by FROM invites
		WHERE secret_sha256 = ? AND used_at IS NULL AND expires_at > ? AND kind = 'machine'`, hashSecret(string(secret)), now.UnixMilli()).
		Scan(&id, &machine, &createdBy)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", 0, ErrInviteInvalid
	}
	return id, machine, createdBy.Int64, err
}

// ReopenMachineInvite makes a machine invite usable again after a join that
// failed (compare-and-set on the time that UseMachineInvite set).
func (s *Store) ReopenMachineInvite(ctx context.Context, id int64, usedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE invites SET used_at = NULL WHERE id = ? AND used_at = ?`, id, usedAt.UnixMilli())
	return err
}

// UseMachineInvite marks a machine invite used (compare-and-set).
func (s *Store) UseMachineInvite(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE invites SET used_at = ? WHERE id = ? AND used_at IS NULL`, now.UnixMilli(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrInviteInvalid
	}
	return nil
}

// Users returns the users that are not removed, by name, with the computers
// of their active tokens.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.name, u.role, u.created_at, COALESCE(t.computer, '')
		FROM users u LEFT JOIN tokens t ON t.user_id = u.id AND t.revoked_at IS NULL
		WHERE u.removed_at IS NULL ORDER BY u.name COLLATE NOCASE, t.computer`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created int64
		var computer string
		if err := rows.Scan(&u.ID, &u.Name, &u.Role, &created, &computer); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].ID == u.ID {
			out[n-1].Computers = append(out[n-1].Computers, computer)
			continue
		}
		u.CreatedAt = time.UnixMilli(created)
		if computer != "" {
			u.Computers = []string{computer}
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UserByName returns the user with this name (not case-sensitive).
func (s *Store) UserByName(ctx context.Context, name string) (User, error) {
	var u User
	var created int64
	id, err := activeUserID(ctx, s.db, name)
	if err != nil {
		return User{}, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT id, name, role, created_at FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Name, &u.Role, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	u.CreatedAt = time.UnixMilli(created)
	return u, err
}

// otherAdmins counts the admins other than id.
func otherAdmins(ctx context.Context, tx *sql.Tx, id int64) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = ? AND removed_at IS NULL AND id != ?`, RoleAdmin, id).Scan(&n)
	return n, err
}

// SetUserRole changes a user's role; the server keeps at least one admin.
func (s *Store) SetUserRole(ctx context.Context, id int64, role string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if role != RoleAdmin {
			if n, err := otherAdmins(ctx, tx, id); err != nil {
				return err
			} else if n == 0 {
				return ErrLastAdmin
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ? AND removed_at IS NULL`, role, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if role != RoleAdmin {
			return closeInvites(ctx, tx, id, time.Now())
		}
		return nil
	})
}

// closeInvites ends the open user invites that an admin made, when the admin
// is demoted or removed: they must not let anybody in later.
func closeInvites(ctx context.Context, tx *sql.Tx, adminID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE invites SET used_at = ? WHERE created_by = ? AND kind IN (?, ?) AND used_at IS NULL`,
		now.UnixMilli(), adminID, InviteUser, InviteMachine)
	return err
}

// RemoveUser marks a user removed and revokes its tokens; the server keeps at
// least one admin. Its leases are the server's work (they need cleanup).
func (s *Store) RemoveUser(ctx context.Context, id int64, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if n, err := otherAdmins(ctx, tx, id); err != nil {
			return err
		} else if n == 0 {
			var role string
			if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, id).Scan(&role); err == nil && role == RoleAdmin {
				return ErrLastAdmin
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE users SET removed_at = ? WHERE id = ? AND removed_at IS NULL`, now.UnixMilli(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now.UnixMilli(), id); err != nil {
			return err
		}
		// Also the codes for the user's other computers.
		if _, err := tx.ExecContext(ctx, `UPDATE invites SET used_at = ? WHERE user_id = ? AND used_at IS NULL`, now.UnixMilli(), id); err != nil {
			return err
		}
		return closeInvites(ctx, tx, id, now)
	})
}

// RevokeComputer revokes the user's tokens of one computer; ErrNotFound if
// there is none.
func (s *Store) RevokeComputer(ctx context.Context, userID int64, computer string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE user_id = ? AND computer = ? AND revoked_at IS NULL`,
		now.UnixMilli(), userID, computer)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TokenUserNames maps the IDs of user tokens to their user names (also
// revoked tokens and removed users: leases keep their owner).
func (s *Store) TokenUserNames(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, u.name FROM tokens t JOIN users u ON u.id = t.user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

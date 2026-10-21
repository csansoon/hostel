// Package store keeps the server state in SQLite. Each state change is a
// compare-and-set in one short transaction; no transaction is held during I/O.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the server database.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, creates it if necessary, and applies the
// migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// One connection: all writes are short, so they never wait for each other
	// long, and SQLite never returns SQLITE_BUSY inside the server.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		base := strings.TrimPrefix(f, "migrations/")
		n, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: no number prefix", base)
		}
		if n <= current {
			continue
		}
		body, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		err = s.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, string(body)); err != nil {
				return fmt.Errorf("migration %s: %w", base, err)
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, n)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

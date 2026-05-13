package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"time"

	"github.com/northwatchlabs/northwatch/internal/component"
	_ "modernc.org/sqlite"
)

// Compile-time check that *SQLite satisfies Store.
var _ Store = (*SQLite)(nil)

// SQLite is the SQLite-backed implementation of Store. It is safe for
// concurrent reads but serializes writes via a single-connection pool
// (SQLite serializes writers regardless of pool size).
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens (or creates) a SQLite database at the given path.
// Use ":memory:" for a private in-process database. Pragmas
// (journal_mode=WAL, foreign_keys=ON, busy_timeout=5000ms) are applied
// to every connection via the DSN query string.
func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	dsn := dsnFromPath(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serializes writers itself (busy_timeout=5000 in the DSN
	// lets a waiting writer back off), but WAL mode supports many
	// concurrent readers alongside that one writer. A small pool lets
	// HTTP renders read in parallel with the watcher's writes instead
	// of queueing behind them.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLite{db: db}, nil
}

// Close releases the underlying connection pool.
func (s *SQLite) Close() error { return s.db.Close() }

// DB exposes the underlying *sql.DB for tests in this package only.
// Production callers should never reach for this.
func (s *SQLite) DB() *sql.DB { return s.db }

const stmtUpsertComponent = `
INSERT INTO components (kind, namespace, name, display_name, status, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  display_name = excluded.display_name,
  status       = excluded.status,
  updated_at   = excluded.updated_at;
`

// UpsertComponent inserts or updates a component. The store stamps
// updated_at; the caller passes Kind/Namespace/Name/DisplayName/Status.
// Empty Status defaults to StatusUnknown.
func (s *SQLite) UpsertComponent(ctx context.Context, c component.Component) error {
	if c.Status == "" {
		c.Status = component.StatusUnknown
	}
	now := time.Now().UTC().Unix()
	_, err := s.db.ExecContext(ctx, stmtUpsertComponent,
		c.Kind, c.Namespace, c.Name, c.DisplayName, string(c.Status), now)
	return err
}

const stmtGetComponent = `
SELECT kind, namespace, name, display_name, status, updated_at
FROM components WHERE id = ?;
`

// GetComponent returns the component with the given ID, or ErrNotFound.
func (s *SQLite) GetComponent(ctx context.Context, id string) (component.Component, error) {
	var (
		c      component.Component
		status string
		ts     int64
	)
	err := s.db.QueryRowContext(ctx, stmtGetComponent, id).Scan(
		&c.Kind, &c.Namespace, &c.Name, &c.DisplayName, &status, &ts,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return component.Component{}, ErrNotFound
	}
	if err != nil {
		return component.Component{}, err
	}
	c.Status = component.Status(status)
	c.UpdatedAt = time.Unix(ts, 0).UTC()
	return c, nil
}

const stmtListComponents = `
SELECT kind, namespace, name, display_name, status, updated_at
FROM components
ORDER BY kind, namespace, name;
`

// ListComponents returns all components in (kind, namespace, name)
// order. A nil slice on an empty table is fine — callers range over
// the result.
func (s *SQLite) ListComponents(ctx context.Context) ([]component.Component, error) {
	rows, err := s.db.QueryContext(ctx, stmtListComponents)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []component.Component
	for rows.Next() {
		var (
			c      component.Component
			status string
			ts     int64
		)
		if err := rows.Scan(&c.Kind, &c.Namespace, &c.Name, &c.DisplayName, &status, &ts); err != nil {
			return nil, err
		}
		c.Status = component.Status(status)
		c.UpdatedAt = time.Unix(ts, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func dsnFromPath(p string) string {
	pragmas := "_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	if p == ":memory:" {
		// WAL is not useful for :memory: and modernc.org/sqlite needs
		// the URI form to accept query-string pragmas.
		return "file::memory:?" + pragmas
	}
	// Escape the path so characters meaningful to URI parsers
	// (`?`, `#`, `%`, space, etc.) don't get interpreted as query
	// separators or fragment markers — otherwise a path like
	// `/tmp/db?x=1` would silently open the wrong file and let an
	// attacker-controlled string override our pragmas.
	return "file:" + url.PathEscape(p) + "?_pragma=journal_mode(WAL)&" + pragmas
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/incident"
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
//
// Ownership note: UpsertComponent is the reconciler's tool — it
// overwrites status and bumps updated_at on every call. It does NOT
// touch the active flag, which is owned by SyncComponents (the
// config-driven boot path). A reconciler calling UpsertComponent on
// a soft-deactivated row will update its status silently but the
// row will stay hidden from user-facing views until config returns
// it to active.
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

const stmtHasActiveComponent = `
SELECT 1 FROM components WHERE id = ? AND active = 1;
`

// HasActiveComponent returns true iff a row matching id exists with
// active = 1. The result is intended for guard checks (e.g., the
// incident service rejecting incident creation against deactivated
// components). Returns false (not ErrNotFound) for both missing and
// inactive rows because callers don't need to distinguish.
func (s *SQLite) HasActiveComponent(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, stmtHasActiveComponent, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

const stmtListComponents = `
SELECT kind, namespace, name, display_name, status, updated_at
FROM components
WHERE active = 1
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

// SyncComponents — see Store interface for contract.
//
// Implementation contract: the entire sync (BEGIN IMMEDIATE, the
// would-deactivate SELECT, every INSERT ... ON CONFLICT, the
// deactivation UPDATE, and the final COMMIT or ROLLBACK) runs on a
// single *sql.Conn checked out of the pool. The pool is sized to 8
// (see OpenSQLite), so any statement routed through s.db.Exec* or
// s.db.Query* could land on a different connection that does not
// hold the IMMEDIATE lock — which would silently break both the
// atomicity guarantee and the gate rollback. Stay on `conn`.
func (s *SQLite) SyncComponents(ctx context.Context, specs []ComponentSpec, allowDeactivate bool) (int, error) {
	// 1. Pre-check duplicate IDs in the input.
	if dup, ok := findDuplicateID(specs); ok {
		return 0, fmt.Errorf("store: duplicate component id in specs: %q", dup)
	}

	// 2. Check out a connection and acquire a RESERVED write lock
	// for the lifetime of the sync.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, fmt.Errorf("store: begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Use Background so a cancelled parent ctx can't
			// prevent rollback.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	// 3. Compute the would-deactivate set inside the tx.
	missingIDs, err := selectMissingActiveIDs(ctx, conn, specs)
	if err != nil {
		return 0, fmt.Errorf("store: compute deactivation set: %w", err)
	}

	// 4. Gate.
	if len(missingIDs) > 0 && !allowDeactivate {
		return 0, &DeactivationRefusedError{IDs: missingIDs}
	}

	// 5. Upsert all specs.
	now := time.Now().UTC().Unix()
	for _, sp := range specs {
		if _, err := conn.ExecContext(ctx, stmtSyncUpsert,
			sp.Kind, sp.Namespace, sp.Name, sp.DisplayName, now,
		); err != nil {
			return 0, fmt.Errorf("store: upsert %s/%s/%s: %w",
				sp.Kind, sp.Namespace, sp.Name, err)
		}
	}

	// 6. Predicate-based deactivation. Re-evaluating the predicate
	// at UPDATE time (rather than using the step-3 ID list) is
	// belt-and-suspenders correctness in case anything ran during
	// the tx that changed the active set.
	deactivated := 0
	if len(missingIDs) > 0 {
		query, args := buildDeactivateSQL(specs)
		res, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, fmt.Errorf("store: deactivate: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("store: rows affected: %w", err)
		}
		deactivated = int(n)
	}

	// 7. Commit.
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, fmt.Errorf("store: commit: %w", err)
	}
	committed = true
	return deactivated, nil
}

// stmtSyncUpsert: INSERT with ON CONFLICT updating display_name and
// active. Status is NOT in the SET clause, so existing rows keep
// their status across config syncs. active=1 ensures a returning
// component is re-activated.
//
// updated_at is intentionally omitted from the SET clause too:
// updated_at tracks "when the status last changed," not "when
// config was synced" or "when a component was re-activated." A
// pure rename through config should not bump it. Contrast with
// stmtUpsertComponent above, which is the reconciler's tool and
// always bumps updated_at because the reconciler IS the source of
// status changes.
const stmtSyncUpsert = `
INSERT INTO components (kind, namespace, name, display_name, status, active, updated_at)
VALUES (?, ?, ?, ?, 'unknown', 1, ?)
ON CONFLICT(id) DO UPDATE SET
  display_name = excluded.display_name,
  active       = 1;
`

func findDuplicateID(specs []ComponentSpec) (string, bool) {
	seen := make(map[string]struct{}, len(specs))
	for _, sp := range specs {
		id := sp.Kind + "/" + sp.Namespace + "/" + sp.Name
		if _, ok := seen[id]; ok {
			return id, true
		}
		seen[id] = struct{}{}
	}
	return "", false
}

// selectMissingActiveIDs returns the IDs of currently-active rows
// whose IDs are not in specs. Runs on the open conn within the tx.
func selectMissingActiveIDs(ctx context.Context, conn *sql.Conn, specs []ComponentSpec) ([]string, error) {
	if len(specs) == 0 {
		rows, err := conn.QueryContext(ctx,
			`SELECT id FROM components WHERE active = 1 ORDER BY id`)
		if err != nil {
			return nil, err
		}
		return scanIDs(rows)
	}
	placeholders, args := inListFromSpecs(specs)
	query := `SELECT id FROM components WHERE active = 1 AND id NOT IN (` +
		placeholders + `) ORDER BY id`
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanIDs(rows)
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// inListFromSpecs produces "?,?,?" placeholders and matching arg
// slice (one arg per spec, the spec's computed id).
func inListFromSpecs(specs []ComponentSpec) (string, []any) {
	if len(specs) == 0 {
		return "", nil
	}
	parts := make([]string, len(specs))
	args := make([]any, len(specs))
	for i, sp := range specs {
		parts[i] = "?"
		args[i] = sp.Kind + "/" + sp.Namespace + "/" + sp.Name
	}
	return strings.Join(parts, ","), args
}

// buildDeactivateSQL: UPDATE active=0 WHERE active=1 AND id NOT IN (...).
// Empty specs slice → no placeholders; predicate becomes
// "WHERE active=1" (every active row). This is the "wipe the
// watched set" path used when config legitimately declares zero
// components; selectMissingActiveIDs has the symmetric branch.
func buildDeactivateSQL(specs []ComponentSpec) (string, []any) {
	if len(specs) == 0 {
		return `UPDATE components SET active = 0 WHERE active = 1`, nil
	}
	placeholders, args := inListFromSpecs(specs)
	return `UPDATE components SET active = 0 WHERE active = 1 AND id NOT IN (` +
		placeholders + `)`, args
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

const stmtInsertIncident = `
INSERT INTO incidents (id, component_id, title, status, opened_at)
VALUES (?, ?, ?, ?, ?);
`

const stmtInsertIncidentUpdate = `
INSERT INTO incident_updates (id, incident_id, body, status, created_at)
VALUES (?, ?, ?, ?, ?);
`

// CreateIncident inserts inc and firstUpdate in a single
// BEGIN IMMEDIATE transaction. The pattern matches SyncComponents:
// both writes run on a single *sql.Conn that holds the write lock,
// and a deferred ROLLBACK guard uses context.Background() so a
// cancelled parent ctx cannot bypass cleanup.
func (s *SQLite) CreateIncident(ctx context.Context, inc incident.Incident, firstUpdate incident.Update) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire conn: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if _, err := conn.ExecContext(ctx, stmtInsertIncident,
		inc.ID, inc.ComponentID, inc.Title, string(inc.Status), inc.OpenedAt.UTC().Unix(),
	); err != nil {
		return translateFKAsNotFound(err)
	}
	if _, err := conn.ExecContext(ctx, stmtInsertIncidentUpdate,
		firstUpdate.ID, firstUpdate.IncidentID, firstUpdate.Body,
		string(firstUpdate.Status), firstUpdate.CreatedAt.UTC().Unix(),
	); err != nil {
		return fmt.Errorf("store: insert incident_update: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	committed = true
	return nil
}

// translateFKAsNotFound maps a SQLite FK violation (extended code
// 787, SQLITE_CONSTRAINT_FOREIGNKEY) into ErrNotFound so callers
// don't have to parse driver-specific error text. Any other error
// is returned as-is, wrapped.
func translateFKAsNotFound(err error) error {
	if err == nil {
		return nil
	}
	// modernc.org/sqlite returns an error whose Error() string
	// contains "FOREIGN KEY constraint failed" on FK violation.
	// String-matching is the documented contract for this driver.
	if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		return ErrNotFound
	}
	return fmt.Errorf("store: insert incident: %w", err)
}

const stmtGetIncident = `
SELECT id, component_id, title, status, opened_at, resolved_at
FROM incidents WHERE id = ?;
`

const stmtGetActiveIncident = `
SELECT id, component_id, title, status, opened_at, resolved_at
FROM incidents
WHERE resolved_at IS NULL
ORDER BY opened_at DESC
LIMIT 1;
`

func (s *SQLite) GetIncident(ctx context.Context, id string) (incident.Incident, error) {
	row := s.db.QueryRowContext(ctx, stmtGetIncident, id)
	return scanIncident(row.Scan)
}

func (s *SQLite) GetActiveIncident(ctx context.Context) (incident.Incident, error) {
	row := s.db.QueryRowContext(ctx, stmtGetActiveIncident)
	return scanIncident(row.Scan)
}

// scanIncident is shared between Get* and List* row scans. The
// scan argument is row.Scan or rows.Scan so the helper works for
// both *sql.Row and *sql.Rows.
func scanIncident(scan func(...any) error) (incident.Incident, error) {
	var (
		inc        incident.Incident
		status     string
		openedSecs int64
		resolved   sql.NullInt64
	)
	err := scan(&inc.ID, &inc.ComponentID, &inc.Title, &status, &openedSecs, &resolved)
	if errors.Is(err, sql.ErrNoRows) {
		return incident.Incident{}, ErrNotFound
	}
	if err != nil {
		return incident.Incident{}, err
	}
	inc.Status = incident.Status(status)
	inc.OpenedAt = time.Unix(openedSecs, 0).UTC()
	if resolved.Valid {
		t := time.Unix(resolved.Int64, 0).UTC()
		inc.ResolvedAt = &t
	}
	return inc, nil
}

const stmtListIncidentsActive = `
SELECT id, component_id, title, status, opened_at, resolved_at
FROM incidents WHERE resolved_at IS NULL
ORDER BY opened_at DESC;
`

const stmtListIncidentsAll = `
SELECT id, component_id, title, status, opened_at, resolved_at
FROM incidents
ORDER BY opened_at DESC;
`

func (s *SQLite) ListIncidents(ctx context.Context, includeResolved bool) ([]incident.Incident, error) {
	stmt := stmtListIncidentsActive
	if includeResolved {
		stmt = stmtListIncidentsAll
	}
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []incident.Incident
	for rows.Next() {
		inc, err := scanIncident(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

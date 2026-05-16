// Package store defines NorthWatch's persistence interface and ships
// a SQLite implementation. Postgres lands in v0.2; the interface here
// is shaped so it's additive.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/incident"
)

// ErrNotFound is returned by Get* methods when no row matches.
// It is the same sentinel as incident.ErrNotFound — they share a
// single value so callers can use either reference with errors.Is.
var ErrNotFound = incident.ErrNotFound

// ErrSchemaTooNew is returned by Migrate when the on-disk schema
// version is higher than the maximum migration embedded in this
// binary. Operators must run a newer binary or restore from backup;
// silently continuing would produce hard-to-diagnose errors on the
// first query that touches an unknown column.
var ErrSchemaTooNew = errors.New("store: on-disk schema is newer than this binary supports")

// ComponentSpec is the desired-state shape used by SyncComponents.
// Kept separate from component.Component so the config layer and
// other callers can describe "what should exist" without depending
// on the runtime-state fields (status, updated_at).
type ComponentSpec struct {
	Kind        string
	Namespace   string
	Name        string
	DisplayName string
}

// DeactivationRefusedError is returned by SyncComponents when the
// would-deactivate set is non-empty and the caller did not pass
// allowDeactivate=true. IDs carries the component IDs (in the
// "<kind>/<namespace>/<name>" form) that would have been
// deactivated. The transaction is rolled back; no rows are touched.
type DeactivationRefusedError struct {
	IDs []string
}

func (e *DeactivationRefusedError) Error() string {
	return fmt.Sprintf(
		"store: refusing to deactivate %d component(s) without authorization: %v",
		len(e.IDs), e.IDs,
	)
}

// Store is the interface every storage backend implements. SQLite is
// the only impl in v0.1; Postgres adds a second one in v0.2 without
// reshaping this interface.
type Store interface {
	// Close releases the underlying connection pool. Safe to call once.
	Close() error

	// Migrate brings the schema to the version embedded in this binary.
	// Idempotent. Returns ErrSchemaTooNew if the DB on disk is already
	// at a higher version than this binary supports. The ctx parameter
	// is accepted for forward compatibility; the underlying migrate v4
	// library does not honor cancellation mid-statement.
	Migrate(ctx context.Context) error

	// ListComponents returns active (watched) components ordered by
	// (kind, namespace, name). Inactive rows — components that were
	// once in config but aren't now — are excluded; they're
	// preserved in the table for history but not surfaced.
	ListComponents(ctx context.Context) ([]component.Component, error)

	// GetComponent returns the component with the given ID. Returns
	// ErrNotFound if no row matches.
	GetComponent(ctx context.Context, id string) (component.Component, error)

	// HasActiveComponent returns true if a component with the given ID
	// exists AND has active = 1. Returns (false, nil) when the row is
	// missing or soft-deactivated. Storage errors are returned as
	// (false, err).
	HasActiveComponent(ctx context.Context, id string) (bool, error)

	// UpsertComponent inserts or updates by id. The store computes id
	// from (Kind, Namespace, Name) — SQLite uses a STORED generated
	// column — and stamps updated_at to time.Now().UTC().Unix().
	UpsertComponent(ctx context.Context, c component.Component) error

	// SyncComponents reconciles the components table against the
	// given desired set in a single transaction.
	//
	//   - rows present in specs but not in the DB are inserted with
	//     status='unknown' and active=1
	//   - rows present in both have display_name updated and active
	//     set to 1 (re-activating any previously deactivated match);
	//     status is preserved
	//   - rows present in the DB but not in specs are soft-
	//     deactivated by setting active=0; their incident and
	//     status_history rows are untouched
	//
	// Deactivation is the destructive part. SyncComponents refuses
	// to deactivate unless allowDeactivate is true; on refusal it
	// returns *DeactivationRefusedError with the IDs that would have
	// been deactivated and rolls back. Returns the count of rows
	// newly transitioned from active=1 to active=0 in this call.
	SyncComponents(ctx context.Context, specs []ComponentSpec, allowDeactivate bool) (deactivated int, err error)

	// CreateIncident inserts the incident and its initial
	// incident_updates row in a single BEGIN IMMEDIATE transaction.
	// Returns ErrNotFound if inc.ComponentID does not match a row in
	// the components table (FK violation is translated). Both rows
	// land or neither does.
	CreateIncident(ctx context.Context, inc incident.Incident, firstUpdate incident.Update) error

	// GetIncident returns the incident with the given ID, or
	// ErrNotFound.
	GetIncident(ctx context.Context, id string) (incident.Incident, error)

	// GetActiveIncident returns the most recently opened incident
	// whose resolved_at IS NULL, or ErrNotFound when none exist.
	GetActiveIncident(ctx context.Context) (incident.Incident, error)

	// ListIncidents returns incidents ordered by opened_at DESC.
	// When includeResolved is false, rows with resolved_at IS NOT
	// NULL are excluded.
	ListIncidents(ctx context.Context, includeResolved bool) ([]incident.Incident, error)
}

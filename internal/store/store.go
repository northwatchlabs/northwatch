// Package store defines NorthWatch's persistence interface and ships
// a SQLite implementation. Postgres lands in v0.2; the interface here
// is shaped so it's additive.
package store

import (
	"context"
	"errors"

	"github.com/northwatchlabs/northwatch/internal/component"
)

// ErrNotFound is returned by Get* methods when no row matches.
var ErrNotFound = errors.New("store: not found")

// ErrSchemaTooNew is returned by Migrate when the on-disk schema
// version is higher than the maximum migration embedded in this
// binary. Operators must run a newer binary or restore from backup;
// silently continuing would produce hard-to-diagnose errors on the
// first query that touches an unknown column.
var ErrSchemaTooNew = errors.New("store: on-disk schema is newer than this binary supports")

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

	// ListComponents returns all components ordered by
	// (kind, namespace, name).
	ListComponents(ctx context.Context) ([]component.Component, error)

	// GetComponent returns the component with the given ID. Returns
	// ErrNotFound if no row matches.
	GetComponent(ctx context.Context, id string) (component.Component, error)

	// UpsertComponent inserts or updates by id. The store computes id
	// from (Kind, Namespace, Name) — SQLite uses a STORED generated
	// column — and stamps updated_at to time.Now().UTC().Unix().
	UpsertComponent(ctx context.Context, c component.Component) error
}

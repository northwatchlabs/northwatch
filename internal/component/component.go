// Package component defines the Component domain type and the Status
// enum used across the watcher, store, and HTTP layers.
package component

import "time"

// Status is the public health state of a Component as rendered on the
// status page. Stored in SQLite as TEXT with a matching CHECK
// constraint; mirrored here as compile-time constants.
type Status string

const (
	StatusUnknown     Status = "unknown"
	StatusOperational Status = "operational"
	StatusDegraded    Status = "degraded"
	StatusDown        Status = "down"
)

// Component is one watched workload (a Deployment, HelmRelease, or
// Application) shown on the status page.
type Component struct {
	Kind        string
	Namespace   string
	Name        string
	DisplayName string
	Status      Status
	UpdatedAt   time.Time // UTC; set by the store on write
}

// ID returns the canonical "<kind>/<namespace>/<name>" identifier.
// This is the same string SQLite computes for components.id via the
// generated column, so callers can use it as a lookup key.
func (c Component) ID() string {
	return c.Kind + "/" + c.Namespace + "/" + c.Name
}

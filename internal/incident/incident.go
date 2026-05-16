// Package incident defines the Incident domain type, the Status
// enum used across the service, store, and HTTP layers, and the
// Service that orchestrates incident creation.
package incident

import "time"

// Status is the lifecycle state of an incident. Mirrors the
// SQLite CHECK constraint on incidents.status / incident_updates.status.
type Status string

const (
	StatusInvestigating Status = "investigating"
	StatusIdentified    Status = "identified"
	StatusMonitoring    Status = "monitoring"
	StatusResolved      Status = "resolved"
)

// Incident is one operator-visible event affecting a Component.
// ResolvedAt is nil while the incident is active.
type Incident struct {
	ID          string
	ComponentID string
	Title       string
	Status      Status
	OpenedAt    time.Time
	ResolvedAt  *time.Time
}

// Update is one row in the incident_updates timeline. The service
// auto-creates the first Update when an Incident is created; v0.2
// adds POST /incidents/:id/updates to append more.
type Update struct {
	ID         string
	IncidentID string
	Body       string
	Status     Status
	CreatedAt  time.Time
}

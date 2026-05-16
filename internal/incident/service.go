package incident

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// Store is the consumer-defined persistence interface for the
// incident Service. *store.SQLite satisfies it via duck typing.
// Defining it here (rather than in the store package) avoids the
// import cycle that would arise from internal/store already
// importing internal/incident for its method signatures.
type Store interface {
	HasActiveComponent(ctx context.Context, id string) (bool, error)
	CreateIncident(ctx context.Context, inc Incident, firstUpdate Update) error
	GetActiveIncident(ctx context.Context) (Incident, error)
	ListIncidents(ctx context.Context, includeResolved bool) ([]Incident, error)
}

// Service is the use-case layer for incidents. It validates input,
// generates IDs and timestamps, and orchestrates store calls. The
// HTTP layer depends on *Service rather than Store directly.
type Service struct {
	st     Store
	logger *slog.Logger
	now    func() time.Time
	newID  func() string
}

// NewService wires a Service against the given store. now and newID
// default to time.Now and NewID; tests can override them via the
// SetClockForTest / SetIDForTest helpers (added in Task 8).
func NewService(st Store, logger *slog.Logger) *Service {
	return &Service{
		st:     st,
		logger: logger,
		now:    func() time.Time { return time.Now().UTC() },
		newID:  NewID,
	}
}

// CreateIncident validates input, generates IDs and timestamps,
// and persists the incident plus its first incident_updates row.
// Returns ErrTitleRequired if title is empty after trimming, or
// ErrUnknownComponent if componentID does not match an ACTIVE
// component in the components table (missing IDs and soft-
// deactivated IDs both map to ErrUnknownComponent).
func (s *Service) CreateIncident(ctx context.Context, componentID, title string) (Incident, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Incident{}, ErrTitleRequired
	}
	ok, err := s.st.HasActiveComponent(ctx, componentID)
	if err != nil {
		return Incident{}, err
	}
	if !ok {
		return Incident{}, ErrUnknownComponent
	}

	now := s.now()
	inc := Incident{
		ID:          s.newID(),
		ComponentID: componentID,
		Title:       title,
		Status:      StatusInvestigating,
		OpenedAt:    now,
	}
	upd := Update{
		ID:         s.newID(),
		IncidentID: inc.ID,
		Body:       title,
		Status:     StatusInvestigating,
		CreatedAt:  now,
	}
	if err := s.st.CreateIncident(ctx, inc, upd); err != nil {
		return Incident{}, err
	}
	return inc, nil
}

// GetActiveIncident returns the most recent active incident, or
// ErrNotFound when none.
func (s *Service) GetActiveIncident(ctx context.Context) (Incident, error) {
	return s.st.GetActiveIncident(ctx)
}

// ListIncidents returns incidents ordered by opened_at DESC. When
// includeResolved is false, resolved rows are excluded.
func (s *Service) ListIncidents(ctx context.Context, includeResolved bool) ([]Incident, error) {
	return s.st.ListIncidents(ctx, includeResolved)
}

// SetClockForTest overrides the time source. Test-only.
func (s *Service) SetClockForTest(now func() time.Time) { s.now = now }

// SetIDForTest overrides the ID generator. Test-only.
func (s *Service) SetIDForTest(newID func() string) { s.newID = newID }

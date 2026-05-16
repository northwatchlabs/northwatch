package incident_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/incident"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// newServiceWithStore builds a Service against a fresh in-memory
// SQLite, seeded with one component the tests can target.
func newServiceWithStore(t *testing.T) (*incident.Service, *store.SQLite) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := st.UpsertComponent(ctx, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	}); err != nil {
		t.Fatalf("UpsertComponent: %v", err)
	}
	svc := incident.NewService(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, st
}

func TestCreateIncidentTrimsTitle(t *testing.T) {
	svc, _ := newServiceWithStore(t)
	got, err := svc.CreateIncident(context.Background(),
		"Deployment/default/web", "  Pods down  ")
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if got.Title != "Pods down" {
		t.Errorf("Title = %q, want %q", got.Title, "Pods down")
	}
}

func TestCreateIncidentEmptyTitle(t *testing.T) {
	svc, _ := newServiceWithStore(t)
	_, err := svc.CreateIncident(context.Background(),
		"Deployment/default/web", "")
	if !errors.Is(err, incident.ErrTitleRequired) {
		t.Fatalf("err = %v, want ErrTitleRequired", err)
	}
}

func TestCreateIncidentWhitespaceTitle(t *testing.T) {
	svc, _ := newServiceWithStore(t)
	_, err := svc.CreateIncident(context.Background(),
		"Deployment/default/web", "   \t  ")
	if !errors.Is(err, incident.ErrTitleRequired) {
		t.Fatalf("err = %v, want ErrTitleRequired", err)
	}
}

func TestCreateIncidentUnknownComponent(t *testing.T) {
	svc, _ := newServiceWithStore(t)
	_, err := svc.CreateIncident(context.Background(),
		"Deployment/default/missing", "title")
	if !errors.Is(err, incident.ErrUnknownComponent) {
		t.Fatalf("err = %v, want ErrUnknownComponent", err)
	}
}

func TestCreateIncidentInactiveComponent(t *testing.T) {
	svc, st := newServiceWithStore(t)
	// Soft-deactivate the seeded component.
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE components SET active = 0 WHERE id = ?`,
		"Deployment/default/web"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	_, err := svc.CreateIncident(context.Background(),
		"Deployment/default/web", "title")
	if !errors.Is(err, incident.ErrUnknownComponent) {
		t.Fatalf("err = %v, want ErrUnknownComponent", err)
	}
}

func TestCreateIncidentInitialUpdateMirrorsTitle(t *testing.T) {
	svc, st := newServiceWithStore(t)
	got, err := svc.CreateIncident(context.Background(),
		"Deployment/default/web", "Pods crashlooping")
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	var body string
	if err := st.DB().QueryRow(
		`SELECT body FROM incident_updates WHERE incident_id = ?`,
		got.ID).Scan(&body); err != nil {
		t.Fatalf("query update: %v", err)
	}
	if body != "Pods crashlooping" {
		t.Errorf("update body = %q, want %q", body, "Pods crashlooping")
	}
}

func TestCreateIncidentDeterministicWithInjectedClockAndID(t *testing.T) {
	svc, _ := newServiceWithStore(t)
	fixed := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return fixed })
	ids := []string{"01HXINC0000000000000000Z1", "01HXUPD0000000000000000Z1"}
	i := 0
	svc.SetIDForTest(func() string {
		out := ids[i]
		i++
		return out
	})

	got, err := svc.CreateIncident(context.Background(),
		"Deployment/default/web", "stable")
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if got.ID != ids[0] {
		t.Errorf("incident ID = %s, want %s", got.ID, ids[0])
	}
	if !got.OpenedAt.Equal(fixed) {
		t.Errorf("OpenedAt = %v, want %v", got.OpenedAt, fixed)
	}
}

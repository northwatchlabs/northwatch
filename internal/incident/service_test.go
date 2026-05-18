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

func TestResolveIncidentHappyPath(t *testing.T) {
	svc, _ := newServiceWithStore(t)
	ctx := context.Background()
	created, err := svc.CreateIncident(ctx, "Deployment/default/web", "boom")
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}

	resolveAt := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return resolveAt })
	svc.SetIDForTest(func() string { return "01HXUPD0000000000000RESV1" })

	got, err := svc.ResolveIncident(ctx, created.ID)
	if err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	if got.Status != incident.StatusResolved {
		t.Errorf("Status = %s, want resolved", got.Status)
	}
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(resolveAt) {
		t.Errorf("ResolvedAt = %v, want %v", got.ResolvedAt, resolveAt)
	}
}

func TestResolveIncidentUnknown(t *testing.T) {
	svc, _ := newServiceWithStore(t)
	_, err := svc.ResolveIncident(context.Background(), "ghost-id")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
}

func TestResolveIncidentIdempotent(t *testing.T) {
	svc, st := newServiceWithStore(t)
	ctx := context.Background()
	created, err := svc.CreateIncident(ctx, "Deployment/default/web", "boom")
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}

	firstAt := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return firstAt })
	svc.SetIDForTest(func() string { return "01HXUPD0000000000000IDMP1" })
	first, err := svc.ResolveIncident(ctx, created.ID)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// Second resolve uses a *later* clock and a *different* ID.
	secondAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	svc.SetClockForTest(func() time.Time { return secondAt })
	svc.SetIDForTest(func() string { return "01HXUPD0000000000000IDMP2" })
	second, err := svc.ResolveIncident(ctx, created.ID)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}

	// Both returns reflect the *first* resolve's timestamp.
	if first.ResolvedAt == nil || !first.ResolvedAt.Equal(firstAt) {
		t.Errorf("first.ResolvedAt = %v, want %v", first.ResolvedAt, firstAt)
	}
	if second.ResolvedAt == nil || !second.ResolvedAt.Equal(firstAt) {
		t.Errorf("second.ResolvedAt = %v, want %v (idempotent)", second.ResolvedAt, firstAt)
	}

	// Only one resolve-update row landed (plus the create-time update = 2).
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incident_updates WHERE incident_id = ?`,
		created.ID).Scan(&n); err != nil {
		t.Fatalf("count updates: %v", err)
	}
	if n != 2 {
		t.Errorf("incident_updates count = %d, want 2 (idempotent resolve must not double-write)", n)
	}
}

func TestResolveIncidentUsesInjectedClockAndID(t *testing.T) {
	svc, st := newServiceWithStore(t)
	ctx := context.Background()
	created, err := svc.CreateIncident(ctx, "Deployment/default/web", "boom")
	if err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}

	resolveAt := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	resolveID := "01HXUPD0000000000000RSLV2"
	svc.SetClockForTest(func() time.Time { return resolveAt })
	svc.SetIDForTest(func() string { return resolveID })

	if _, err := svc.ResolveIncident(ctx, created.ID); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}

	var (
		body, status string
		createdAt    int64
	)
	row := st.DB().QueryRowContext(ctx,
		`SELECT body, status, created_at FROM incident_updates WHERE id = ?`, resolveID)
	if err := row.Scan(&body, &status, &createdAt); err != nil {
		t.Fatalf("scan resolve update: %v", err)
	}
	if body != "Incident resolved." {
		t.Errorf("body = %q, want %q", body, "Incident resolved.")
	}
	if status != string(incident.StatusResolved) {
		t.Errorf("status = %q, want %q", status, incident.StatusResolved)
	}
	if createdAt != resolveAt.Unix() {
		t.Errorf("created_at = %d, want %d", createdAt, resolveAt.Unix())
	}
}

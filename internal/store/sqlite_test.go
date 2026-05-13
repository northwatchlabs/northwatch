package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// newTestStore opens an in-memory SQLite and applies migrations.
// Used by every test in this file.
func newTestStore(t *testing.T) *store.SQLite {
	t.Helper()
	ctx := context.Background()
	s, err := store.OpenSQLite(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

func TestOpenInMemoryAndMigrate(t *testing.T) {
	s := newTestStore(t)
	// Calling Migrate a second time must be a no-op (no error).
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	s := newTestStore(t)
	// After newTestStore, schema_migrations is at version 1. Bump it
	// past anything embedded in the binary so the next Migrate call
	// triggers the version gate.
	_, err := s.DB().ExecContext(context.Background(),
		`UPDATE schema_migrations SET version = 999, dirty = 0`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	err = s.Migrate(context.Background())
	if !errors.Is(err, store.ErrSchemaTooNew) {
		t.Fatalf("Migrate err = %v, want ErrSchemaTooNew", err)
	}
}

func seedComponent(t *testing.T, s *store.SQLite, c component.Component) {
	t.Helper()
	if err := s.UpsertComponent(context.Background(), c); err != nil {
		t.Fatalf("UpsertComponent: %v", err)
	}
}

func TestUpsertGetListComponents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind:        "Deployment",
		Namespace:   "default",
		Name:        "web",
		DisplayName: "Web App",
		Status:      component.StatusOperational,
	})
	seedComponent(t, s, component.Component{
		Kind:        "HelmRelease",
		Namespace:   "monitoring",
		Name:        "grafana",
		DisplayName: "Grafana",
		Status:      component.StatusDegraded,
	})

	got, err := s.GetComponent(ctx, "Deployment/default/web")
	if err != nil {
		t.Fatalf("GetComponent: %v", err)
	}
	if got.DisplayName != "Web App" || got.Status != component.StatusOperational {
		t.Errorf("got = %+v, want display=Web App status=operational", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt was not stamped")
	}
	if !got.UpdatedAt.Equal(got.UpdatedAt.UTC()) {
		t.Errorf("UpdatedAt = %v, want UTC", got.UpdatedAt)
	}

	all, err := s.ListComponents(ctx)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want 2", len(all))
	}
	if all[0].Kind != "Deployment" || all[1].Kind != "HelmRelease" {
		t.Errorf("order wrong: %+v", all)
	}
}

func TestUpsertIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c := component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
		DisplayName: "Web", Status: component.StatusOperational,
	}
	seedComponent(t, s, c)
	first, _ := s.GetComponent(ctx, c.ID())

	time.Sleep(time.Second + 50*time.Millisecond)

	c.DisplayName = "Web (renamed)"
	c.Status = component.StatusDegraded
	seedComponent(t, s, c)

	updated, err := s.GetComponent(ctx, c.ID())
	if err != nil {
		t.Fatalf("GetComponent after re-upsert: %v", err)
	}
	if updated.DisplayName != "Web (renamed)" || updated.Status != component.StatusDegraded {
		t.Errorf("re-upsert did not overwrite: %+v", updated)
	}
	if !updated.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance: first=%v updated=%v",
			first.UpdatedAt, updated.UpdatedAt)
	}
}

func TestGetComponentReturnsNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetComponent(context.Background(), "Deployment/default/missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStatusCheckConstraintBlocksGarbage(t *testing.T) {
	s := newTestStore(t)
	_, err := s.DB().ExecContext(context.Background(),
		`INSERT INTO components (kind, namespace, name, status, updated_at)
		 VALUES ('Deployment', 'default', 'foo', 'bogus', 1)`)
	if err == nil {
		t.Fatalf("expected CHECK constraint error, got nil")
	}
}

func TestComponentIDGeneratedFromColumns(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "ns1", Name: "n1",
		Status: component.StatusOperational,
	})
	if _, err := s.GetComponent(ctx, "Deployment/ns1/n1"); err != nil {
		t.Fatalf("GetComponent by generated id: %v", err)
	}

	// Generated columns are read-only — INSERT specifying id must fail.
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO components (kind, namespace, name, id, status, updated_at)
		 VALUES ('Deployment', 'ns1', 'n2', 'X/Y/Z', 'unknown', 1)`)
	if err == nil {
		t.Fatalf("expected error setting generated column, got nil")
	}
}

func TestSlashInKindNamespaceNameRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	cases := []component.Component{
		{Kind: "apps/v1.Deployment", Namespace: "default", Name: "n", Status: component.StatusUnknown},
		{Kind: "Deployment", Namespace: "kube-system/sub", Name: "n", Status: component.StatusUnknown},
		{Kind: "Deployment", Namespace: "default", Name: "we/b", Status: component.StatusUnknown},
	}
	for i, c := range cases {
		if err := s.UpsertComponent(ctx, c); err == nil {
			t.Errorf("case %d: expected CHECK constraint error for %+v, got nil", i, c)
		}
	}
}

func TestForeignKeyCascadeOnComponentDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
		Status: component.StatusOperational,
	})
	// Raw-insert an incident — CreateIncident lands in #20.
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO incidents (id, component_id, title, opened_at)
		 VALUES ('inc1', 'Deployment/default/web', 'Test', 1)`); err != nil {
		t.Fatalf("seed incident: %v", err)
	}

	// Sanity: row is there.
	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE id = 'inc1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("seed: count = %d, want 1", n)
	}

	// Delete the parent; FK CASCADE should drop the child.
	if _, err := s.DB().ExecContext(ctx,
		`DELETE FROM components WHERE id = 'Deployment/default/web'`); err != nil {
		t.Fatal(err)
	}

	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE id = 'inc1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("after CASCADE: count = %d, want 0", n)
	}
}

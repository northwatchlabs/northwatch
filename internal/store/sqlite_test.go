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

func TestMigrate_AppliesActiveColumn(t *testing.T) {
	s := newTestStore(t)
	// Seed via UpsertComponent — should land with active=1 by default.
	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
		DisplayName: "Web", Status: component.StatusOperational,
	})
	var active int
	err := s.DB().QueryRowContext(context.Background(),
		`SELECT active FROM components WHERE id = ?`,
		"Deployment/default/web").Scan(&active)
	if err != nil {
		t.Fatalf("query active: %v", err)
	}
	if active != 1 {
		t.Errorf("active = %d, want 1 (DEFAULT)", active)
	}
}

func TestListComponents_FiltersInactive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Active component.
	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "active",
		DisplayName: "Active", Status: component.StatusOperational,
	})
	// Insert an inactive row directly. UpsertComponent doesn't expose
	// active, and once SyncComponents lands we'll use it; for now,
	// raw SQL is fine in this test.
	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "inactive",
		DisplayName: "Inactive", Status: component.StatusOperational,
	})
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE components SET active = 0 WHERE id = ?`,
		"Deployment/default/inactive"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	got, err := s.ListComponents(ctx)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1; got=%+v", len(got), got)
	}
	if got[0].Name != "active" {
		t.Errorf("got[0].Name = %q, want %q", got[0].Name, "active")
	}
}

func TestSyncComponents_InsertsNew(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	specs := []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
		{Kind: "HelmRelease", Namespace: "flux", Name: "b", DisplayName: "B"},
		{Kind: "Application", Namespace: "argo", Name: "c", DisplayName: "C"},
	}
	deactivated, err := s.SyncComponents(ctx, specs, false)
	if err != nil {
		t.Fatalf("SyncComponents: %v", err)
	}
	if deactivated != 0 {
		t.Errorf("deactivated = %d, want 0", deactivated)
	}

	got, err := s.ListComponents(ctx)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	for _, c := range got {
		if c.Status != component.StatusUnknown {
			t.Errorf("%s status = %q, want unknown", c.ID(), c.Status)
		}
	}
}

func TestSyncComponents_UpdatesDisplayName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "old"},
	}, false); err != nil {
		t.Fatalf("seed SyncComponents: %v", err)
	}

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "new"},
	}, false); err != nil {
		t.Fatalf("SyncComponents: %v", err)
	}

	c, err := s.GetComponent(ctx, "Deployment/default/a")
	if err != nil {
		t.Fatalf("GetComponent: %v", err)
	}
	if c.DisplayName != "new" {
		t.Errorf("DisplayName = %q, want %q", c.DisplayName, "new")
	}
}

func TestSyncComponents_PreservesStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "a",
		DisplayName: "A", Status: component.StatusOperational,
	})

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
	}, false); err != nil {
		t.Fatalf("SyncComponents: %v", err)
	}

	c, err := s.GetComponent(ctx, "Deployment/default/a")
	if err != nil {
		t.Fatalf("GetComponent: %v", err)
	}
	if c.Status != component.StatusOperational {
		t.Errorf("status = %q, want operational", c.Status)
	}
}

func TestSyncComponents_DeactivatesMissing_Allowed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "keep", DisplayName: "Keep"},
		{Kind: "Deployment", Namespace: "default", Name: "drop", DisplayName: "Drop"},
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO incidents (id, component_id, title, opened_at)
		VALUES (?, ?, ?, ?)`,
		"inc-1", "Deployment/default/drop", "outage", time.Now().Unix(),
	); err != nil {
		t.Fatalf("seed incident: %v", err)
	}

	deactivated, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "keep", DisplayName: "Keep"},
	}, true)
	if err != nil {
		t.Fatalf("SyncComponents: %v", err)
	}
	if deactivated != 1 {
		t.Errorf("deactivated = %d, want 1", deactivated)
	}

	var active int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT active FROM components WHERE id = ?`,
		"Deployment/default/drop").Scan(&active); err != nil {
		t.Fatalf("query active: %v", err)
	}
	if active != 0 {
		t.Errorf("drop active = %d, want 0", active)
	}

	var count int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE component_id = ?`,
		"Deployment/default/drop").Scan(&count); err != nil {
		t.Fatalf("query incident: %v", err)
	}
	if count != 1 {
		t.Errorf("incident count = %d, want 1", count)
	}
}

func TestSyncComponents_DeactivatesMissing_Refused(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
		{Kind: "Deployment", Namespace: "default", Name: "b", DisplayName: "B"},
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
	}, false)

	var refused *store.DeactivationRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *DeactivationRefusedError", err)
	}
	if len(refused.IDs) != 1 || refused.IDs[0] != "Deployment/default/b" {
		t.Errorf("IDs = %v, want [Deployment/default/b]", refused.IDs)
	}

	got, err := s.ListComponents(ctx)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2 (both still active)", len(got))
	}
}

func TestSyncComponents_ReactivatesOnReturn(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.UpsertComponent(ctx, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "a",
		DisplayName: "A", Status: component.StatusOperational,
	}); err != nil {
		t.Fatalf("UpsertComponent: %v", err)
	}
	if _, err := s.SyncComponents(ctx, nil, true); err != nil {
		t.Fatalf("deactivate sync: %v", err)
	}
	var active int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT active FROM components WHERE id = ?`,
		"Deployment/default/a").Scan(&active); err != nil {
		t.Fatalf("query: %v", err)
	}
	if active != 0 {
		t.Fatalf("setup: active = %d, want 0", active)
	}

	deactivated, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
	}, false)
	if err != nil {
		t.Fatalf("re-activate SyncComponents: %v", err)
	}
	if deactivated != 0 {
		t.Errorf("deactivated = %d, want 0 (no new transitions to inactive)", deactivated)
	}

	c, err := s.GetComponent(ctx, "Deployment/default/a")
	if err != nil {
		t.Fatalf("GetComponent: %v", err)
	}
	if c.Status != component.StatusOperational {
		t.Errorf("status = %q, want operational (preserved across reactivation)", c.Status)
	}
	var nowActive int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT active FROM components WHERE id = ?`,
		"Deployment/default/a").Scan(&nowActive); err != nil {
		t.Fatalf("query: %v", err)
	}
	if nowActive != 1 {
		t.Errorf("active = %d, want 1", nowActive)
	}
}

func TestSyncComponents_AtomicOnError(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "old"},
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "new"},
		{Kind: "Deployment", Namespace: "default", Name: "bad/name", DisplayName: "Bad"},
	}, false)
	if err == nil {
		t.Fatal("SyncComponents err = nil, want non-nil (CHECK violation)")
	}

	c, err := s.GetComponent(ctx, "Deployment/default/a")
	if err != nil {
		t.Fatalf("GetComponent: %v", err)
	}
	if c.DisplayName != "old" {
		t.Errorf("DisplayName = %q, want %q (tx must have rolled back)", c.DisplayName, "old")
	}
}

func TestSyncComponents_EmptySpecs_Refused(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
		{Kind: "Deployment", Namespace: "default", Name: "b", DisplayName: "B"},
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := s.SyncComponents(ctx, nil, false)
	var refused *store.DeactivationRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *DeactivationRefusedError", err)
	}
	if len(refused.IDs) != 2 {
		t.Errorf("len(IDs) = %d, want 2", len(refused.IDs))
	}

	got, err := s.ListComponents(ctx)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2 (DB unchanged)", len(got))
	}
}

func TestSyncComponents_EmptySpecs_Allowed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
		{Kind: "Deployment", Namespace: "default", Name: "b", DisplayName: "B"},
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	deactivated, err := s.SyncComponents(ctx, nil, true)
	if err != nil {
		t.Fatalf("SyncComponents: %v", err)
	}
	if deactivated != 2 {
		t.Errorf("deactivated = %d, want 2", deactivated)
	}

	got, err := s.ListComponents(ctx)
	if err != nil {
		t.Fatalf("ListComponents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0 (both inactive, filtered out)", len(got))
	}

	var rowCount int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM components`).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 2 {
		t.Errorf("row count = %d, want 2 (soft-delete preserves rows)", rowCount)
	}
}

func TestSyncComponents_DeactivatedCountIgnoresAlreadyInactive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
		{Kind: "Deployment", Namespace: "default", Name: "b", DisplayName: "B"},
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A"},
	}, true); err != nil {
		t.Fatalf("first deactivate: %v", err)
	}
	deactivated, err := s.SyncComponents(ctx, nil, true)
	if err != nil {
		t.Fatalf("SyncComponents: %v", err)
	}
	if deactivated != 1 {
		t.Errorf("deactivated = %d, want 1 (b was already inactive)", deactivated)
	}
}

func TestSyncComponents_RefusalDoesNotApplyUpserts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "old"},
		{Kind: "Deployment", Namespace: "default", Name: "b", DisplayName: "B"},
	}, false); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := s.SyncComponents(ctx, []store.ComponentSpec{
		{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "new"},
	}, false)
	var refused *store.DeactivationRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *DeactivationRefusedError", err)
	}

	c, err := s.GetComponent(ctx, "Deployment/default/a")
	if err != nil {
		t.Fatalf("GetComponent: %v", err)
	}
	if c.DisplayName != "old" {
		t.Errorf("DisplayName = %q, want %q (upsert must have rolled back)", c.DisplayName, "old")
	}
}

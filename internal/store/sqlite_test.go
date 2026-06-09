package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/incident"
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

func TestMigrateConvertsComponentUpdatedAtToMillis(t *testing.T) {
	ctx := context.Background()
	s, err := store.OpenSQLite(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.DB().ExecContext(ctx, `
CREATE TABLE components (
  kind         TEXT    NOT NULL CHECK (kind      NOT LIKE '%/%'),
  namespace    TEXT    NOT NULL CHECK (namespace NOT LIKE '%/%'),
  name         TEXT    NOT NULL CHECK (name      NOT LIKE '%/%'),
  id           TEXT    GENERATED ALWAYS AS (kind || '/' || namespace || '/' || name) STORED
               UNIQUE,
  display_name TEXT    NOT NULL DEFAULT '',
  status       TEXT    NOT NULL DEFAULT 'unknown'
               CHECK (status IN ('unknown','operational','degraded','down')),
  updated_at   INTEGER NOT NULL,
  active       INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1))
) STRICT;
CREATE TABLE schema_migrations (version uint64, dirty bool);
INSERT INTO schema_migrations (version, dirty) VALUES (2, 0);
INSERT INTO components (kind, namespace, name, status, updated_at)
VALUES ('Deployment', 'default', 'web', 'operational', 1710000000);
`)
	if err != nil {
		t.Fatalf("seed v2 schema: %v", err)
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var got int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT updated_at FROM components WHERE id = 'Deployment/default/web'`).Scan(&got); err != nil {
		t.Fatalf("query updated_at: %v", err)
	}
	if want := int64(1710000000000); got != want {
		t.Fatalf("updated_at = %d, want %d", got, want)
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

	_, err := s.DB().ExecContext(ctx,
		`UPDATE components SET updated_at = ? WHERE id = ?`,
		first.UpdatedAt.Add(-time.Second).UnixMilli(), c.ID())
	if err != nil {
		t.Fatalf("force old updated_at: %v", err)
	}
	first, err = s.GetComponent(ctx, c.ID())
	if err != nil {
		t.Fatalf("GetComponent after timestamp reset: %v", err)
	}

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

func TestCreateIncidentInsertsBothRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})

	now := time.Unix(1_700_000_000, 0).UTC()
	inc := incident.Incident{
		ID:          "01HXINC0000000000000000001",
		ComponentID: "Deployment/default/web",
		Title:       "Pods crashlooping",
		Status:      incident.StatusInvestigating,
		OpenedAt:    now,
	}
	upd := incident.Update{
		ID:         "01HXUPD0000000000000000001",
		IncidentID: inc.ID,
		Body:       inc.Title,
		Status:     incident.StatusInvestigating,
		CreatedAt:  now,
	}

	if err := s.CreateIncident(ctx, inc, upd); err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}

	var incidentCount, updateCount int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE id = ?`, inc.ID).Scan(&incidentCount); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	if incidentCount != 1 {
		t.Fatalf("incidents row count = %d, want 1", incidentCount)
	}
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incident_updates WHERE incident_id = ?`, inc.ID).Scan(&updateCount); err != nil {
		t.Fatalf("count updates: %v", err)
	}
	if updateCount != 1 {
		t.Fatalf("incident_updates row count = %d, want 1", updateCount)
	}
}

func TestCreateIncidentUnknownComponent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Unix(1_700_000_000, 0).UTC()
	inc := incident.Incident{
		ID:          "01HXINC0000000000000000002",
		ComponentID: "Deployment/default/missing",
		Title:       "nope",
		Status:      incident.StatusInvestigating,
		OpenedAt:    now,
	}
	upd := incident.Update{
		ID:         "01HXUPD0000000000000000002",
		IncidentID: inc.ID,
		Body:       inc.Title,
		Status:     incident.StatusInvestigating,
		CreatedAt:  now,
	}

	err := s.CreateIncident(ctx, inc, upd)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CreateIncident err = %v, want ErrNotFound", err)
	}

	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("incidents row count = %d after FK failure, want 0", n)
	}
}

func TestCreateIncidentAtomicityRollsBackOnUpdateFailure(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})

	now := time.Unix(1_700_000_000, 0).UTC()
	dupUpdateID := "01HXUPD0000000000000000099"

	first := incident.Incident{
		ID:          "01HXINC0000000000000000010",
		ComponentID: "Deployment/default/web",
		Title:       "first",
		Status:      incident.StatusInvestigating,
		OpenedAt:    now,
	}
	firstUpd := incident.Update{
		ID:         dupUpdateID,
		IncidentID: first.ID,
		Body:       first.Title,
		Status:     incident.StatusInvestigating,
		CreatedAt:  now,
	}
	if err := s.CreateIncident(ctx, first, firstUpd); err != nil {
		t.Fatalf("seed first incident: %v", err)
	}

	collide := incident.Incident{
		ID:          "01HXINC0000000000000000011",
		ComponentID: "Deployment/default/web",
		Title:       "second",
		Status:      incident.StatusInvestigating,
		OpenedAt:    now,
	}
	collideUpd := incident.Update{
		ID:         dupUpdateID, // collision
		IncidentID: collide.ID,
		Body:       collide.Title,
		Status:     incident.StatusInvestigating,
		CreatedAt:  now,
	}

	if err := s.CreateIncident(ctx, collide, collideUpd); err == nil {
		t.Fatal("CreateIncident: want error from duplicate update id, got nil")
	}

	var n int
	if err := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE id = ?`, collide.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("incidents row count = %d after rollback, want 0", n)
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

func TestGetActiveIncidentReturnsMostRecent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})

	mk := func(id string, secs int64) {
		t.Helper()
		opened := time.Unix(secs, 0).UTC()
		if err := s.CreateIncident(ctx,
			incident.Incident{
				ID:          id,
				ComponentID: "Deployment/default/web",
				Title:       id,
				Status:      incident.StatusInvestigating,
				OpenedAt:    opened,
			},
			incident.Update{
				ID:         "u-" + id,
				IncidentID: id,
				Body:       id,
				Status:     incident.StatusInvestigating,
				CreatedAt:  opened,
			},
		); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	mk("01HXINC00000000000000000A1", 100)
	mk("01HXINC00000000000000000A2", 300) // latest
	mk("01HXINC00000000000000000A3", 200)

	got, err := s.GetActiveIncident(ctx)
	if err != nil {
		t.Fatalf("GetActiveIncident: %v", err)
	}
	if got.ID != "01HXINC00000000000000000A2" {
		t.Fatalf("GetActiveIncident.ID = %s, want 01HXINC00000000000000000A2", got.ID)
	}
}

func TestHasActiveComponentTrueForActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	ok, err := s.HasActiveComponent(ctx, "Deployment/default/web")
	if err != nil {
		t.Fatalf("HasActiveComponent: %v", err)
	}
	if !ok {
		t.Fatalf("HasActiveComponent = false, want true")
	}
}

func TestHasActiveComponentFalseForInactive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE components SET active = 0 WHERE id = ?`,
		"Deployment/default/web"); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	ok, err := s.HasActiveComponent(ctx, "Deployment/default/web")
	if err != nil {
		t.Fatalf("HasActiveComponent: %v", err)
	}
	if ok {
		t.Fatalf("HasActiveComponent = true, want false (deactivated)")
	}
}

func TestHasActiveComponentFalseForMissing(t *testing.T) {
	s := newTestStore(t)
	ok, err := s.HasActiveComponent(context.Background(), "Deployment/default/ghost")
	if err != nil {
		t.Fatalf("HasActiveComponent: %v", err)
	}
	if ok {
		t.Fatalf("HasActiveComponent = true, want false (missing)")
	}
}

func TestGetActiveIncidentNoneReturnsErrNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetActiveIncident(context.Background())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetActiveIncident err = %v, want ErrNotFound", err)
	}
}

func TestGetIncidentNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetIncident(context.Background(), "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetIncident err = %v, want ErrNotFound", err)
	}
}

func TestListIncidentsExcludesResolvedByDefault(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})

	now := time.Unix(1_700_000_000, 0).UTC()
	active := incident.Incident{
		ID: "01HXINC0000000000000000B1", ComponentID: "Deployment/default/web",
		Title: "active", Status: incident.StatusInvestigating, OpenedAt: now,
	}
	resolved := incident.Incident{
		ID: "01HXINC0000000000000000B2", ComponentID: "Deployment/default/web",
		Title: "resolved", Status: incident.StatusResolved, OpenedAt: now,
	}
	mkUpd := func(id string) incident.Update {
		return incident.Update{
			ID: "u-" + id, IncidentID: id, Body: "x",
			Status: incident.StatusInvestigating, CreatedAt: now,
		}
	}
	if err := s.CreateIncident(ctx, active, mkUpd(active.ID)); err != nil {
		t.Fatalf("seed active: %v", err)
	}
	if err := s.CreateIncident(ctx, resolved, mkUpd(resolved.ID)); err != nil {
		t.Fatalf("seed resolved: %v", err)
	}
	// Mark the second one resolved.
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE incidents SET resolved_at = ? WHERE id = ?`,
		now.Unix(), resolved.ID); err != nil {
		t.Fatalf("mark resolved: %v", err)
	}

	got, err := s.ListIncidents(ctx, false)
	if err != nil {
		t.Fatalf("ListIncidents(false): %v", err)
	}
	if len(got) != 1 || got[0].ID != active.ID {
		t.Fatalf("ListIncidents(false) = %v, want [active]", got)
	}

	gotAll, err := s.ListIncidents(ctx, true)
	if err != nil {
		t.Fatalf("ListIncidents(true): %v", err)
	}
	if len(gotAll) != 2 {
		t.Fatalf("ListIncidents(true) len = %d, want 2", len(gotAll))
	}
}

func TestListIncidentsOrderedByOpenedAtDesc(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})

	mk := func(id string, secs int64) {
		t.Helper()
		opened := time.Unix(secs, 0).UTC()
		_ = s.CreateIncident(ctx,
			incident.Incident{
				ID: id, ComponentID: "Deployment/default/web",
				Title: id, Status: incident.StatusInvestigating, OpenedAt: opened,
			},
			incident.Update{
				ID: "u-" + id, IncidentID: id, Body: id,
				Status: incident.StatusInvestigating, CreatedAt: opened,
			},
		)
	}
	mk("01HXINC0000000000000000C1", 100)
	mk("01HXINC0000000000000000C2", 300)
	mk("01HXINC0000000000000000C3", 200)

	got, err := s.ListIncidents(ctx, false)
	if err != nil {
		t.Fatalf("ListIncidents: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	wantOrder := []string{
		"01HXINC0000000000000000C2",
		"01HXINC0000000000000000C3",
		"01HXINC0000000000000000C1",
	}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("got[%d].ID = %s, want %s", i, got[i].ID, w)
		}
	}
}

// seedActiveIncident creates one active incident with one initial
// update row. Returns the incident ID and the opened-at timestamp.
func seedActiveIncident(t *testing.T, s *store.SQLite, incID, compID string) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	opened := time.Unix(1_700_000_000, 0).UTC()
	inc := incident.Incident{
		ID: incID, ComponentID: compID,
		Title: "boom", Status: incident.StatusInvestigating, OpenedAt: opened,
	}
	upd := incident.Update{
		ID: "u-" + incID, IncidentID: incID, Body: "boom",
		Status: incident.StatusInvestigating, CreatedAt: opened,
	}
	if err := s.CreateIncident(ctx, inc, upd); err != nil {
		t.Fatalf("seed active incident: %v", err)
	}
	return incID, opened
}

// countIncidentUpdates returns the number of incident_updates rows
// for a given incident_id.
func countIncidentUpdates(t *testing.T, s *store.SQLite, incID string) int {
	t.Helper()
	var n int
	row := s.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM incident_updates WHERE incident_id = ?`, incID)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count incident_updates: %v", err)
	}
	return n
}

func TestResolveIncidentSetsResolvedAtAndAppendsUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	incID, _ := seedActiveIncident(t, s,
		"01HXINC0000000000000000R1", "Deployment/default/web")

	resolvedAt := time.Unix(1_700_001_000, 0).UTC()
	updateID := "u-resolve-1"

	got, err := s.ResolveIncident(ctx, incID, resolvedAt, updateID, "Incident resolved.")
	if err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	if got.Status != incident.StatusResolved {
		t.Errorf("status = %s, want resolved", got.Status)
	}
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(resolvedAt) {
		t.Errorf("ResolvedAt = %v, want %v", got.ResolvedAt, resolvedAt)
	}

	persisted, err := s.GetIncident(ctx, incID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if persisted.Status != incident.StatusResolved {
		t.Errorf("persisted status = %s, want resolved", persisted.Status)
	}
	if persisted.ResolvedAt == nil || !persisted.ResolvedAt.Equal(resolvedAt) {
		t.Errorf("persisted ResolvedAt = %v, want %v", persisted.ResolvedAt, resolvedAt)
	}

	if n := countIncidentUpdates(t, s, incID); n != 2 {
		t.Errorf("incident_updates count = %d, want 2 (initial + resolve)", n)
	}

	var body, status string
	row := s.DB().QueryRowContext(ctx,
		`SELECT body, status FROM incident_updates WHERE id = ?`, updateID)
	if err := row.Scan(&body, &status); err != nil {
		t.Fatalf("scan resolve update: %v", err)
	}
	if body != "Incident resolved." || status != string(incident.StatusResolved) {
		t.Errorf("resolve update = (body=%q, status=%q), want (\"Incident resolved.\", resolved)", body, status)
	}
}

func TestResolveIncidentUnknownReturnsErrNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.ResolveIncident(ctx, "ghost", time.Now().UTC(),
		"u-resolve-x", "Incident resolved.")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	if n := countIncidentUpdates(t, s, "ghost"); n != 0 {
		t.Errorf("incident_updates count = %d, want 0", n)
	}
}

func TestResolveIncidentAlreadyResolvedIsNoOp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	incID, _ := seedActiveIncident(t, s,
		"01HXINC0000000000000000R2", "Deployment/default/web")

	firstResolve := time.Unix(1_700_001_000, 0).UTC()
	if _, err := s.ResolveIncident(ctx, incID, firstResolve,
		"u-resolve-first", "Incident resolved."); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	secondResolve := time.Unix(1_700_009_999, 0).UTC()
	secondUpdateID := "u-resolve-second"
	got, err := s.ResolveIncident(ctx, incID, secondResolve,
		secondUpdateID, "Incident resolved.")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}

	// Returned incident must reflect the *first* resolve.
	if got.ResolvedAt == nil || !got.ResolvedAt.Equal(firstResolve) {
		t.Errorf("returned ResolvedAt = %v, want %v (first resolve)", got.ResolvedAt, firstResolve)
	}

	persisted, err := s.GetIncident(ctx, incID)
	if err != nil {
		t.Fatalf("GetIncident: %v", err)
	}
	if persisted.ResolvedAt == nil || !persisted.ResolvedAt.Equal(firstResolve) {
		t.Errorf("persisted ResolvedAt = %v, want %v (unchanged)", persisted.ResolvedAt, firstResolve)
	}

	// 1 initial + 1 first-resolve = 2 (second resolve must not insert).
	if n := countIncidentUpdates(t, s, incID); n != 2 {
		t.Errorf("incident_updates count = %d, want 2 (no-op resolve must not insert)", n)
	}

	var exists int
	row := s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incident_updates WHERE id = ?`, secondUpdateID)
	if err := row.Scan(&exists); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if exists != 0 {
		t.Errorf("second update ID present in incident_updates; want absent")
	}
}

func TestResolveIncidentConcurrent(t *testing.T) {
	// :memory: is private per-conn under modernc.org/sqlite, so two
	// goroutines acquiring conns from the pool would see different
	// databases. A real file shares state across the pool.
	dbPath := t.TempDir() + "/concurrent.db"
	ctx := context.Background()
	s, err := store.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	seedComponent(t, s, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	incID, _ := seedActiveIncident(t, s,
		"01HXINC0000000000000000R3", "Deployment/default/web")

	t1 := time.Unix(1_700_010_000, 0).UTC()
	t2 := time.Unix(1_700_020_000, 0).UTC()

	type result struct {
		inc incident.Incident
		err error
	}
	results := make(chan result, 2)
	go func() {
		got, err := s.ResolveIncident(ctx, incID, t1, "u-concurrent-1", "Incident resolved.")
		results <- result{got, err}
	}()
	go func() {
		got, err := s.ResolveIncident(ctx, incID, t2, "u-concurrent-2", "Incident resolved.")
		results <- result{got, err}
	}()

	r1 := <-results
	r2 := <-results
	if r1.err != nil {
		t.Errorf("goroutine 1 err: %v", r1.err)
	}
	if r2.err != nil {
		t.Errorf("goroutine 2 err: %v", r2.err)
	}

	if r1.inc.ResolvedAt == nil || r2.inc.ResolvedAt == nil ||
		!r1.inc.ResolvedAt.Equal(*r2.inc.ResolvedAt) {
		t.Errorf("ResolvedAt diverged across concurrent resolvers: %v vs %v",
			r1.inc.ResolvedAt, r2.inc.ResolvedAt)
	}

	// 1 initial + exactly 1 resolve = 2.
	if n := countIncidentUpdates(t, s, incID); n != 2 {
		t.Errorf("incident_updates count = %d, want 2 (one resolve only)", n)
	}
}

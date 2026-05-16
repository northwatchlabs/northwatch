package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/incident"
	"github.com/northwatchlabs/northwatch/internal/server"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// newHandler boots an in-memory store, migrates it, optionally seeds
// components, and returns the wired HTTP handler.
func newHandler(t *testing.T, seed ...component.Component) http.Handler {
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
	for _, c := range seed {
		if err := st.UpsertComponent(ctx, c); err != nil {
			t.Fatalf("UpsertComponent: %v", err)
		}
	}
	h, err := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), st)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return h
}

func TestIndexReturnsStatusPage(t *testing.T) {
	h := newHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q, want text/html prefix", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "All Systems Operational") {
		t.Errorf("body missing %q\nbody=%s", "All Systems Operational", body)
	}
	if !strings.Contains(body, `src="/static/htmx.min.js"`) {
		t.Errorf("body missing HTMX script tag")
	}
}

func TestIndexRendersComponentsFromStore(t *testing.T) {
	h := newHandler(t, component.Component{
		Kind:        "Deployment",
		Namespace:   "default",
		Name:        "web",
		DisplayName: "Web App",
		Status:      component.StatusOperational,
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rr.Body.String()
	if !strings.Contains(body, "Web App") {
		t.Errorf("body missing DisplayName %q\nbody=%s", "Web App", body)
	}
	if !strings.Contains(body, "operational") {
		t.Errorf("body missing status %q\nbody=%s", "operational", body)
	}
}

func TestIndexEmptyStoreStillRenders200(t *testing.T) {
	h := newHandler(t) // no seed
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "All Systems Operational") {
		t.Errorf("body missing page header")
	}
}

func TestHealthz(t *testing.T) {
	h := newHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestStaticAssetServed(t *testing.T) {
	h := newHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.Len() == 0 {
		t.Errorf("empty static body")
	}
}

func TestAPIComponents_Empty(t *testing.T) {
	h := newHandler(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/components", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json prefix", ct)
	}
	if body := strings.TrimSpace(rr.Body.String()); body != "[]" {
		t.Errorf("body = %q, want %q", body, "[]")
	}
}

func TestAPIComponents_Populated(t *testing.T) {
	h := newHandler(t,
		component.Component{
			Kind: "Deployment", Namespace: "default", Name: "web",
			DisplayName: "Web", Status: component.StatusOperational,
		},
		component.Component{
			Kind: "HelmRelease", Namespace: "flux", Name: "cm",
			DisplayName: "CM", Status: component.StatusUnknown,
		},
	)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/components", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var got []struct {
		ID          string    `json:"id"`
		Kind        string    `json:"kind"`
		Namespace   string    `json:"namespace"`
		Name        string    `json:"name"`
		DisplayName string    `json:"displayName"`
		Status      string    `json:"status"`
		UpdatedAt   time.Time `json:"updatedAt"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, rr.Body.String())
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	// ListComponents orders by (kind, namespace, name), so:
	// 0: Deployment/default/web, 1: HelmRelease/flux/cm
	if got[0].ID != "Deployment/default/web" {
		t.Errorf("got[0].ID = %q, want Deployment/default/web", got[0].ID)
	}
	if got[0].Status != "operational" {
		t.Errorf("got[0].Status = %q, want operational", got[0].Status)
	}
	if got[0].UpdatedAt.IsZero() {
		t.Error("got[0].UpdatedAt is zero")
	}
}

// failingStore is a Store impl whose ListComponents always errs.
type failingStore struct{ store.Store }

func (failingStore) ListComponents(ctx context.Context) ([]component.Component, error) {
	return nil, errors.New("synthetic store failure")
}

func TestAPIComponents_StoreError(t *testing.T) {
	ctx := context.Background()
	real, err := store.OpenSQLite(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	h, err := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)),
		failingStore{Store: real})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/components", nil))

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

// newHandlerWithStore is like newHandler but also returns the store
// so tests can seed incidents (which require a store handle rather
// than going through any *server.Server exported API).
func newHandlerWithStore(t *testing.T, seed ...component.Component) (http.Handler, *store.SQLite) {
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
	for _, c := range seed {
		if err := st.UpsertComponent(ctx, c); err != nil {
			t.Fatalf("UpsertComponent: %v", err)
		}
	}
	h, err := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), st)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return h, st
}

// seedIncident inserts one active incident + first update directly via
// the store interface. Returns the incident ID. If resolved is true,
// flips resolved_at via direct SQL after the insert.
func seedIncident(t *testing.T, st *store.SQLite, componentID, title string, openedAt time.Time, resolved bool) string {
	t.Helper()
	incID := incident.NewID()
	inc := incident.Incident{
		ID: incID, ComponentID: componentID, Title: title,
		Status: incident.StatusInvestigating, OpenedAt: openedAt,
	}
	upd := incident.Update{
		ID: incident.NewID(), IncidentID: incID, Body: title,
		Status: incident.StatusInvestigating, CreatedAt: openedAt,
	}
	if err := st.CreateIncident(context.Background(), inc, upd); err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	if resolved {
		if _, err := st.DB().ExecContext(context.Background(),
			`UPDATE incidents SET resolved_at = ? WHERE id = ?`,
			openedAt.Unix(), incID); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	return incID
}

func TestGetAPIIncidentsEmpty(t *testing.T) {
	h, _ := newHandlerWithStore(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/incidents", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := strings.TrimSpace(rr.Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want %q", body, "[]")
	}
}

func TestGetAPIIncidentsActiveOnly(t *testing.T) {
	h, st := newHandlerWithStore(t, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	now := time.Unix(1_700_000_000, 0).UTC()
	_ = seedIncident(t, st, "Deployment/default/web", "active", now, false)
	_ = seedIncident(t, st, "Deployment/default/web", "resolved", now, true)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/incidents", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (active only)", len(got))
	}
	if got[0]["title"] != "active" {
		t.Errorf("title = %v, want active", got[0]["title"])
	}
}

func TestGetAPIIncidentsIncludeResolved(t *testing.T) {
	h, st := newHandlerWithStore(t, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	now := time.Unix(1_700_000_000, 0).UTC()
	_ = seedIncident(t, st, "Deployment/default/web", "active", now, false)
	_ = seedIncident(t, st, "Deployment/default/web", "resolved", now, true)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/incidents?include=resolved", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestGetAPIIncidentsCacheControlNoStore(t *testing.T) {
	h, _ := newHandlerWithStore(t)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/incidents", nil))
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// newPostHandler builds a createIncidentHandler bound to a fresh
// in-memory store seeded with the given components. Returns the
// handler and the store so the test can assert side effects.
func newPostHandler(t *testing.T, seed ...component.Component) (http.HandlerFunc, *store.SQLite) {
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
	for _, c := range seed {
		if err := st.UpsertComponent(ctx, c); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := incident.NewService(st, logger)
	return server.CreateIncidentHandlerForTest(svc, logger), st
}

func TestCreateIncidentHandlerCreated(t *testing.T) {
	h, _ := newPostHandler(t, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	body := strings.NewReader(`{"component":"Deployment/default/web","title":"Pods down"}`)
	req := httptest.NewRequest(http.MethodPost, "/incidents", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "investigating" {
		t.Errorf("status = %v, want investigating", got["status"])
	}
	if got["title"] != "Pods down" {
		t.Errorf("title = %v, want Pods down", got["title"])
	}
	if id, _ := got["id"].(string); len(id) != 26 {
		t.Errorf("id length = %d, want 26 (ULID)", len(id))
	}
	if _, present := got["resolvedAt"]; present {
		t.Errorf("resolvedAt should be omitted when nil")
	}
}

func TestCreateIncidentHandlerMissingTitle(t *testing.T) {
	h, _ := newPostHandler(t, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{"component":"Deployment/default/web","title":""}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "title required") {
		t.Errorf("body = %s, want contains title required", rr.Body.String())
	}
}

func TestCreateIncidentHandlerMissingComponent(t *testing.T) {
	h, _ := newPostHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{"title":"oops"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "component required") {
		t.Errorf("body = %s, want contains component required", rr.Body.String())
	}
}

func TestCreateIncidentHandlerUnknownComponent(t *testing.T) {
	h, _ := newPostHandler(t) // no seeded components
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{"component":"Deployment/default/ghost","title":"x"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unknown component") {
		t.Errorf("body = %s, want contains unknown component", rr.Body.String())
	}
}

func TestCreateIncidentHandlerUnknownJSONField(t *testing.T) {
	h, _ := newPostHandler(t, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{"component":"Deployment/default/web","title":"x","mystery":1}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestCreateIncidentHandlerMalformedJSON(t *testing.T) {
	h, _ := newPostHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{not-json`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestPostIncidentsRouteNotRegistered(t *testing.T) {
	h, _ := newHandlerWithStore(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{"component":"x","title":"y"}`))
	h.ServeHTTP(rr, req)
	// chi returns 405 Method Not Allowed when a method is missing
	// on an otherwise-registered path, or 404 when the path is
	// unregistered. Either confirms the POST route is not wired.
	if rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /incidents status = %d, want 404 or 405 "+
			"(route must be deferred to #21)", rr.Code)
	}
}

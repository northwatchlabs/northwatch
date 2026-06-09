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

// testToken is the fixture bearer token used by every write-side
// test in this file. ≥16 chars so it satisfies the cmd-line
// validation rule even though tests don't go through cmd parsing.
const testToken = "test-fixture-token-1234"

// newHandlerWith boots an in-memory store, migrates it, optionally
// seeds components, and returns the wired HTTP handler + store. The
// token is passed verbatim into server.New: pass testToken to enable
// writes, "" to test writes-disabled mode.
func newHandlerWith(t *testing.T, token string, seed ...component.Component) (http.Handler, *store.SQLite) {
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
	h, err := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), st, token, 5)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	return h, st
}

// newHandler is the GET-only convenience: token is empty (writes
// disabled, which GET-only tests never exercise) and the store is
// discarded.
func newHandler(t *testing.T, seed ...component.Component) http.Handler {
	t.Helper()
	h, _ := newHandlerWith(t, "", seed...)
	return h
}

func assertJSONError(t *testing.T, rr *httptest.ResponseRecorder, code int, msg string) {
	t.Helper()
	if rr.Code != code {
		t.Fatalf("status = %d, want %d; body=%s", rr.Code, code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json prefix", ct)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, rr.Body.String())
	}
	if got["error"] != msg {
		t.Fatalf("error = %q, want %q; body=%s", got["error"], msg, rr.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("error body keys = %v, want only error", got)
	}
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

type failingIncidentListStore struct{ store.Store }

func (failingIncidentListStore) ListIncidents(ctx context.Context, includeResolved bool) ([]incident.Incident, error) {
	return nil, errors.New("synthetic incident list failure")
}

type failingCreateIncidentStore struct{ store.Store }

func (failingCreateIncidentStore) HasActiveComponent(ctx context.Context, id string) (bool, error) {
	return true, nil
}

func (failingCreateIncidentStore) CreateIncident(ctx context.Context, inc incident.Incident, firstUpdate incident.Update) error {
	return errors.New("synthetic create incident failure")
}

type failingResolveIncidentStore struct{ store.Store }

func (failingResolveIncidentStore) ResolveIncident(
	ctx context.Context,
	id string,
	resolvedAt time.Time,
	updateID string,
	updateBody string,
) (incident.Incident, error) {
	return incident.Incident{}, errors.New("synthetic resolve incident failure")
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
		failingStore{Store: real}, "", 5)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/components", nil))

	assertJSONError(t, rr, http.StatusInternalServerError, "store error")
}

// newHandlerWithStore returns the GET-only handler + the store for
// tests that seed incidents directly. Token is empty (writes
// disabled).
func newHandlerWithStore(t *testing.T, seed ...component.Component) (http.Handler, *store.SQLite) {
	t.Helper()
	return newHandlerWith(t, "", seed...)
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

func TestGetAPIIncidents_StoreError(t *testing.T) {
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
		failingIncidentListStore{Store: real}, "", 5)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/incidents", nil))

	assertJSONError(t, rr, http.StatusInternalServerError, "store error")
}

// postIncident is a small helper that POSTs a create-incident body
// through the wired router, with the bearer token attached. Returns
// the recorder so callers can assert on status and body.
func postIncident(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/incidents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestCreateIncidentHandlerCreated(t *testing.T) {
	h, _ := newHandlerWith(t, testToken, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	rr := postIncident(t, h, `{"component":"Deployment/default/web","title":"Pods down"}`)

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
	h, _ := newHandlerWith(t, testToken, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	rr := postIncident(t, h, `{"component":"Deployment/default/web","title":""}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "title required") {
		t.Errorf("body = %s, want contains title required", rr.Body.String())
	}
}

func TestCreateIncidentHandlerMissingComponent(t *testing.T) {
	h, _ := newHandlerWith(t, testToken)
	rr := postIncident(t, h, `{"title":"oops"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "component required") {
		t.Errorf("body = %s, want contains component required", rr.Body.String())
	}
}

func TestCreateIncidentHandlerUnknownComponent(t *testing.T) {
	h, _ := newHandlerWith(t, testToken)
	rr := postIncident(t, h, `{"component":"Deployment/default/ghost","title":"x"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unknown component") {
		t.Errorf("body = %s, want contains unknown component", rr.Body.String())
	}
}

func TestCreateIncidentHandlerUnknownJSONField(t *testing.T) {
	h, _ := newHandlerWith(t, testToken, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	rr := postIncident(t, h, `{"component":"Deployment/default/web","title":"x","mystery":1}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "unknown field") {
		t.Errorf("body = %s, want contains 'unknown field'", rr.Body.String())
	}
}

func TestCreateIncidentHandlerMalformedJSON(t *testing.T) {
	h, _ := newHandlerWith(t, testToken)
	rr := postIncident(t, h, `{not-json`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestCreateIncidentHandler_StoreError(t *testing.T) {
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
		failingCreateIncidentStore{Store: real}, testToken, 5)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	rr := postIncident(t, h, `{"component":"Deployment/default/web","title":"Pods down"}`)

	assertJSONError(t, rr, http.StatusInternalServerError, "store error")
}

func TestPostIncidentsRequiresAuth(t *testing.T) {
	h, _ := newHandlerWith(t, testToken, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{"component":"Deployment/default/web","title":"x"}`))
	// No Authorization header.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestPostIncidentsWrongToken(t *testing.T) {
	h, _ := newHandlerWith(t, testToken, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{"component":"Deployment/default/web","title":"x"}`))
	req.Header.Set("Authorization", "Bearer wrong-token-9999-bad")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestWritesDisabledWhenNoToken(t *testing.T) {
	h, _ := newHandlerWith(t, "", component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	req := httptest.NewRequest(http.MethodPost, "/incidents",
		strings.NewReader(`{"component":"Deployment/default/web","title":"x"}`))
	req.Header.Set("Authorization", "Bearer anything")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "write endpoints disabled") {
		t.Errorf("body = %s, want contains write endpoints disabled", rr.Body.String())
	}
}

func TestGetEndpointsRemainOpenWithoutToken(t *testing.T) {
	h, _ := newHandlerWith(t, "", component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	for _, path := range []string{"/api/components", "/api/incidents", "/"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, rr.Code)
		}
	}
}

// createIncidentVia posts and returns the created incident ID and
// the decoded body, failing the test on non-201.
func createIncidentVia(t *testing.T, h http.Handler, component, title string) (string, map[string]any) {
	t.Helper()
	rr := postIncident(t, h,
		`{"component":"`+component+`","title":"`+title+`"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	id, _ := got["id"].(string)
	if id == "" {
		t.Fatalf("created body has no id: %v", got)
	}
	return id, got
}

func resolveIncident(t *testing.T, h http.Handler, id string, withToken bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/incidents/"+id+"/resolve", nil)
	if withToken {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestPostIncidentsResolveHappyPath(t *testing.T) {
	h, _ := newHandlerWith(t, testToken, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	id, _ := createIncidentVia(t, h, "Deployment/default/web", "boom")

	rr := resolveIncident(t, h, id, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "resolved" {
		t.Errorf("status = %v, want resolved", got["status"])
	}
	if got["resolvedAt"] == nil {
		t.Errorf("resolvedAt missing from body")
	}
}

func TestPostIncidentsResolveNotFound(t *testing.T) {
	h, _ := newHandlerWith(t, testToken)
	rr := resolveIncident(t, h, "01HXNOSUCHINCIDENT00000000", true)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "incident not found") {
		t.Errorf("body = %s, want incident not found", rr.Body.String())
	}
}

func TestResolveIncidentHandler_StoreError(t *testing.T) {
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
		failingResolveIncidentStore{Store: real}, testToken, 5)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	rr := resolveIncident(t, h, "01HXNOSUCHINCIDENT00000000", true)

	assertJSONError(t, rr, http.StatusInternalServerError, "store error")
}

func TestPostIncidentsResolveIdempotent(t *testing.T) {
	h, st := newHandlerWith(t, testToken, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	id, _ := createIncidentVia(t, h, "Deployment/default/web", "boom")

	first := resolveIncident(t, h, id, true)
	if first.Code != http.StatusOK {
		t.Fatalf("first resolve status = %d, want 200", first.Code)
	}
	var firstBody map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	firstResolved, _ := firstBody["resolvedAt"].(string)
	if firstResolved == "" {
		t.Fatalf("first resolvedAt missing")
	}

	second := resolveIncident(t, h, id, true)
	if second.Code != http.StatusOK {
		t.Fatalf("second resolve status = %d, want 200; body=%s",
			second.Code, second.Body.String())
	}
	var secondBody map[string]any
	if err := json.Unmarshal(second.Body.Bytes(), &secondBody); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	secondResolved, _ := secondBody["resolvedAt"].(string)
	if secondResolved != firstResolved {
		t.Errorf("second resolvedAt = %q, want %q (idempotent)",
			secondResolved, firstResolved)
	}

	// Only one resolve-update row landed: 1 initial + 1 resolve = 2.
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM incident_updates WHERE incident_id = ?`,
		id).Scan(&n); err != nil {
		t.Fatalf("count updates: %v", err)
	}
	if n != 2 {
		t.Errorf("incident_updates count = %d, want 2", n)
	}
}

func TestPostIncidentsResolveRequiresAuth(t *testing.T) {
	h, _ := newHandlerWith(t, testToken, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "web",
	})
	id, _ := createIncidentVia(t, h, "Deployment/default/web", "boom")
	rr := resolveIncident(t, h, id, false)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestIndex_BannerOperational(t *testing.T) {
	t.Parallel()
	h := newHandler(t,
		component.Component{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A", Status: component.StatusOperational},
	)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "All Systems Operational") {
		t.Errorf("missing operational banner; body=%s", body)
	}
	if strings.Contains(body, "Some Systems Degraded") {
		t.Errorf("operational state should not render degraded banner")
	}
}

func TestIndex_BannerDegradedWhenComponentDown(t *testing.T) {
	t.Parallel()
	h := newHandler(t,
		component.Component{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A", Status: component.StatusOperational},
		component.Component{Kind: "Deployment", Namespace: "default", Name: "b", DisplayName: "B", Status: component.StatusDown},
	)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "Some Systems Degraded") {
		t.Errorf("missing degraded banner; body=%s", body)
	}
	if strings.Contains(body, "All Systems Operational") {
		t.Errorf("degraded state should not render operational banner")
	}
}

func TestIndex_BannerIncidentTakesPriority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	comp := component.Component{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A", Status: component.StatusDown}
	if err := st.UpsertComponent(ctx, comp); err != nil {
		t.Fatalf("UpsertComponent: %v", err)
	}
	incSvc := incident.NewService(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := incSvc.CreateIncident(ctx, comp.ID(), "Demo outage"); err != nil {
		t.Fatalf("CreateIncident: %v", err)
	}
	h, err := server.New(slog.New(slog.NewTextHandler(io.Discard, nil)), st, "", 5)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "Demo outage") {
		t.Errorf("missing incident title; body=%s", body)
	}
	if strings.Contains(body, "Some Systems Degraded") {
		t.Errorf("incident state should not render degraded banner even when components are down")
	}
	if strings.Contains(body, "All Systems Operational") {
		t.Errorf("incident state should not render operational banner")
	}
}

func TestApiStatus_PartialIncludesPollingAttrs(t *testing.T) {
	t.Parallel()
	h := newHandler(t,
		component.Component{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A", Status: component.StatusOperational},
	)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := rr.Body.String()
	wants := []string{
		`id="status"`,
		`hx-get="/api/status"`,
		`hx-trigger="every 5s"`,
		`hx-swap="outerHTML"`,
		`All Systems Operational`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("partial missing %q; body=%s", w, body)
		}
	}
	for _, banned := range []string{"<html", "<body", "<!DOCTYPE"} {
		if strings.Contains(body, banned) {
			t.Errorf("partial should not contain %q; body=%s", banned, body)
		}
	}
}

func TestApiStatus_StoreError(t *testing.T) {
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
		failingStore{Store: real}, "", 5)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	assertJSONError(t, rr, http.StatusInternalServerError, "store error")
}

func TestApiStatus_RendersDegradedWhenComponentDown(t *testing.T) {
	t.Parallel()
	h := newHandler(t,
		component.Component{Kind: "Deployment", Namespace: "default", Name: "a", DisplayName: "A", Status: component.StatusDown},
	)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "Some Systems Degraded") {
		t.Errorf("partial missing degraded banner; body=%s", rr.Body.String())
	}
}

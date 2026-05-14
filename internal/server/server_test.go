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

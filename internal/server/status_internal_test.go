package server

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/northwatchlabs/northwatch/internal/incident"
	"github.com/northwatchlabs/northwatch/internal/store"
)

func assertInternalJSONError(t *testing.T, rr *httptest.ResponseRecorder, code int, msg string) {
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

func TestStatusSectionHandler_RenderError(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(ctx, ":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	tmpl := template.Must(template.New("test").Parse(
		`{{define "status_section"}}{{template "missing" .}}{{end}}`,
	))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	incSvc := incident.NewService(st, logger)
	h := statusSectionHandler(tmpl, st, incSvc, 5, logger)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	assertInternalJSONError(t, rr, http.StatusInternalServerError, "render error")
}

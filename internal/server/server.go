// Package server wires the HTTP routes for the NorthWatch status page.
package server

import (
	"bytes"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/incident"
	"github.com/northwatchlabs/northwatch/internal/server/auth"
	"github.com/northwatchlabs/northwatch/internal/store"
	"github.com/northwatchlabs/northwatch/internal/ui"
)

type pageData struct {
	Title      string
	Components []component.Component
}

// New returns an http.Handler with routes for the status page,
// healthcheck, embedded static assets, and bearer-token-protected
// write endpoints. The store is consulted on every index render.
//
// apiToken gates POST routes. An empty token boots the server with
// the write side disabled — every POST returns 401. Reads remain
// public regardless of token state.
func New(logger *slog.Logger, st store.Store, apiToken string) (http.Handler, error) {
	tmpl, err := ui.Templates()
	if err != nil {
		return nil, err
	}
	staticFS, err := ui.StaticFS()
	if err != nil {
		return nil, err
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(slogRequestLogger(logger))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	incSvc := incident.NewService(st, logger)

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(staticFS)))
	r.Get("/api/components", apiComponentsHandler(st, logger))
	r.Get("/api/incidents", apiIncidentsHandler(incSvc, logger))
	r.Get("/", indexHandler(tmpl, st, logger))

	r.Group(func(r chi.Router) {
		r.Use(auth.BearerToken(apiToken, logger))
		r.Post("/incidents", createIncidentHandler(incSvc, logger))
		r.Post("/incidents/{id}/resolve", resolveIncidentHandler(incSvc, logger))
	})

	return r, nil
}

func indexHandler(tmpl *template.Template, st store.Store, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		comps, err := st.ListComponents(r.Context())
		if err != nil {
			logger.Error("index: ListComponents failed", "err", err)
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		data := pageData{
			// Title stays static for v0.1. A future PR derives it from
			// component statuses (e.g. "Service Degraded" when any
			// component is non-operational).
			Title:      "All Systems Operational",
			Components: comps,
		}
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = buf.WriteTo(w)
	}
}

// apiComponent is the JSON wire shape for components. Kept separate
// from component.Component so the API can evolve (rename fields,
// hide internals) without touching the domain type.
type apiComponent struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Namespace   string    `json:"namespace"`
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Status      string    `json:"status"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func toAPI(in []component.Component) []apiComponent {
	out := make([]apiComponent, 0, len(in))
	for _, c := range in {
		out = append(out, apiComponent{
			ID:          c.ID(),
			Kind:        c.Kind,
			Namespace:   c.Namespace,
			Name:        c.Name,
			DisplayName: c.DisplayName,
			Status:      string(c.Status),
			UpdatedAt:   c.UpdatedAt,
		})
	}
	return out
}

func apiComponentsHandler(st store.Store, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		comps, err := st.ListComponents(r.Context())
		if err != nil {
			logger.Error("api/components: ListComponents failed", "err", err)
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// Status data is live; prevent browser/intermediary caches
		// from serving stale snapshots once the UI starts polling.
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(toAPI(comps)); err != nil {
			logger.Error("api/components: encode failed", "err", err)
		}
	}
}

func slogRequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"reqID", middleware.GetReqID(r.Context()),
			)
		})
	}
}

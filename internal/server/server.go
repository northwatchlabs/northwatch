// Package server wires the HTTP routes for the NorthWatch status page.
package server

import (
	"bytes"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/northwatchlabs/northwatch/internal/ui"
)

// Component is the minimal shape rendered by the status page. The
// store-backed type lands with the persistence layer; the server keeps
// this local for now to avoid a circular dependency with future store
// packages.
type Component struct {
	Kind        string
	Namespace   string
	Name        string
	DisplayName string
	Status      string
}

type pageData struct {
	Title      string
	Components []Component
}

// New returns an http.Handler with routes for the status page,
// healthcheck, and embedded static assets.
func New(logger *slog.Logger) (http.Handler, error) {
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

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	r.Get("/", indexHandler(tmpl))

	return r, nil
}

func indexHandler(tmpl *template.Template) http.HandlerFunc {
	data := pageData{
		Title: "All Systems Operational",
		Components: []Component{
			{
				Kind:        "Deployment",
				Namespace:   "default",
				Name:        "northwatch",
				DisplayName: "NorthWatch",
				Status:      "operational",
			},
		},
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		// Render into a buffer so a mid-template failure can return
		// a clean 500 instead of a 200 with a truncated body.
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = buf.WriteTo(w)
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

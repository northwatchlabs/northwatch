package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/northwatchlabs/northwatch/internal/incident"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// apiIncident is the JSON wire shape. Kept separate from
// incident.Incident so the API can evolve (rename fields, hide
// internals) without touching the domain type.
type apiIncident struct {
	ID          string     `json:"id"`
	ComponentID string     `json:"component"`
	Title       string     `json:"title"`
	Status      string     `json:"status"`
	OpenedAt    time.Time  `json:"openedAt"`
	ResolvedAt  *time.Time `json:"resolvedAt,omitempty"`
}

type createIncidentReq struct {
	Component string `json:"component"`
	Title     string `json:"title"`
}

func toAPIIncident(in incident.Incident) apiIncident {
	return apiIncident{
		ID:          in.ID,
		ComponentID: in.ComponentID,
		Title:       in.Title,
		Status:      string(in.Status),
		OpenedAt:    in.OpenedAt,
		ResolvedAt:  in.ResolvedAt,
	}
}

func toAPIIncidents(in []incident.Incident) []apiIncident {
	out := make([]apiIncident, 0, len(in))
	for _, i := range in {
		out = append(out, toAPIIncident(i))
	}
	return out
}

// apiIncidentsHandler serves GET /api/incidents. Returns active
// incidents by default; ?include=resolved returns everything.
func apiIncidentsHandler(svc *incident.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		includeResolved := r.URL.Query().Get("include") == "resolved"
		incs, err := svc.ListIncidents(r.Context(), includeResolved)
		if err != nil {
			logger.Error("api/incidents: ListIncidents failed", "err", err)
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(toAPIIncidents(incs)); err != nil {
			logger.Error("api/incidents: encode failed", "err", err)
		}
	}
}

// createIncidentHandler serves POST /incidents. Registered in
// server.New under the bearer-token middleware.
func createIncidentHandler(svc *incident.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var req createIncidentReq
		if err := dec.Decode(&req); err != nil {
			// Surface the decoder's message so clients can
			// distinguish "json: unknown field \"x\"" from a
			// genuinely malformed body. The encoding/json
			// messages are stable across Go versions and don't
			// leak server internals.
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Component == "" {
			writeJSONError(w, http.StatusBadRequest, "component required")
			return
		}
		inc, err := svc.CreateIncident(r.Context(), req.Component, req.Title)
		switch {
		case errors.Is(err, incident.ErrTitleRequired):
			writeJSONError(w, http.StatusBadRequest, "title required")
			return
		case errors.Is(err, incident.ErrUnknownComponent):
			writeJSONError(w, http.StatusBadRequest, "unknown component")
			return
		case err != nil:
			logger.Error("incidents: CreateIncident failed", "err", err)
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(toAPIIncident(inc))
	}
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// resolveIncidentHandler serves POST /incidents/{id}/resolve.
// Idempotent: 200 on success, including the no-op resolve of an
// already-resolved incident (response body carries the *original*
// resolvedAt). 404 if the incident does not exist.
func resolveIncidentHandler(svc *incident.Service, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		inc, err := svc.ResolveIncident(r.Context(), id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeJSONError(w, http.StatusNotFound, "incident not found")
			return
		case err != nil:
			logger.Error("incidents/resolve: failed", "err", err)
			http.Error(w, "store error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(toAPIIncident(inc))
	}
}

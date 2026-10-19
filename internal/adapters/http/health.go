package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

type Health struct {
	checkPostgres func(context.Context) error
	timeout       time.Duration
	checks        []Check
}

type Check struct {
	Name string
	Run  func(context.Context) error
}

func NewHealth(checkPostgres func(context.Context) error, timeout time.Duration, checks ...Check) *Health {
	return &Health{checkPostgres: checkPostgres, timeout: timeout, checks: checks}
}

func (h *Health) Routes() http.Handler {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func (h *Health) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health/live", h.live)
	mux.HandleFunc("GET /health/ready", h.ready)
}

func (h *Health) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (h *Health) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	body := map[string]string{"status": "ready"}
	status := http.StatusOK
	for _, check := range append([]Check{{Name: "postgres", Run: h.checkPostgres}}, h.checks...) {
		body[check.Name] = "up"
		if err := check.Run(ctx); err != nil {
			status = http.StatusServiceUnavailable
			body["status"] = "not_ready"
			body[check.Name] = "unavailable"
		}
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

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
	checks := append([]Check{{Name: "postgres", Run: h.checkPostgres}}, h.checks...)
	type outcome struct {
		name string
		err  error
	}
	results := make(chan outcome, len(checks))
	for _, check := range checks {
		body[check.Name] = "unavailable"
		go func(check Check) { results <- outcome{check.Name, check.Run(ctx)} }(check)
	}
	for range checks {
		result := <-results
		if result.err == nil {
			body[result.name] = "up"
		} else {
			status = http.StatusServiceUnavailable
			body["status"] = "not_ready"
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

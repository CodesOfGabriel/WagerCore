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
}

func NewHealth(checkPostgres func(context.Context) error, timeout time.Duration) *Health {
	return &Health{checkPostgres: checkPostgres, timeout: timeout}
}

func (h *Health) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", h.live)
	mux.HandleFunc("GET /health/ready", h.ready)
	return mux
}

func (h *Health) live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

func (h *Health) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()
	if err := h.checkPostgres(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready", "postgres": "unavailable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "postgres": "up"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

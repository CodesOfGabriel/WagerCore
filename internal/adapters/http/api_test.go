package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"junglegaming/internal/adapters/auth"
	"junglegaming/internal/application"
)

func TestFailureLogsContextWithoutExposingDetails(t *testing.T) {
	for _, tt := range []struct {
		name            string
		err             error
		status          int
		event, category string
	}{
		{"unexpected", errors.New("private-token"), 500, "request_failed", "internal"},
		{"database", fmt.Errorf("%w: %w", application.ErrUnavailable, &pgconn.PgError{Code: "P0001", Message: "private-sql", Detail: "private-payload"}), 503, "request_unavailable", "database"},
		{"identity conflict", application.ErrConflict, 409, "request_conflict", "conflict"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			api := &API{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
			r := httptest.NewRequest("POST", "/wagering/transactions?token=private-query", nil)
			r.Pattern = "POST /wagering/transactions"
			ctx := context.WithValue(r.Context(), correlationKey, "correlation-1")
			ctx = context.WithValue(ctx, principalKey, auth.Principal{ProviderID: "provider-a"})
			r = r.WithContext(ctx)
			response := httptest.NewRecorder()
			api.failure(response, r, tt.err, "process_wager", "walletId", "wallet-1")
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if response.Code != tt.status || entry["msg"] != tt.event || entry["errorCategory"] != tt.category || entry["stage"] != "process_wager" || entry["correlationId"] != "correlation-1" || entry["providerId"] != "provider-a" || entry["walletId"] != "wallet-1" || entry["route"] != "POST /wagering/transactions" {
				t.Fatalf("status=%d log=%s", response.Code, logs.String())
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(response.Body.String(), "private-") {
				t.Fatal("sensitive details leaked")
			}
		})
	}
}

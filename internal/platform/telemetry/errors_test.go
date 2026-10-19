package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5/pgconn"
	"junglegaming/internal/application"
	"junglegaming/internal/platform/telemetry"
)

func TestErrorFieldsPreserveClassificationWithoutMessages(t *testing.T) {
	for _, tt := range []struct {
		name     string
		err      error
		category string
		expected string
	}{
		{"database", fmt.Errorf("%w: %w", application.ErrUnavailable, &pgconn.PgError{Code: "23514", ConstraintName: "wallet_balance_guard", Message: "private-token", Detail: "private-balance"}), "database", "wallet_balance_guard"},
		{"conflict", fmt.Errorf("%w: private-payload", application.ErrConflict), "conflict", ""},
		{"deadline", fmt.Errorf("%w: %w", application.ErrUnavailable, context.DeadlineExceeded), "timeout", ""},
		{"canceled", context.Canceled, "canceled", ""},
		{"invalid input", fmt.Errorf("%w: private-payload", application.ErrInvalidInput), "invalid_input", ""},
		{"lease", application.ErrLeaseLost, "lease_lost", ""},
		{"broker", &smithy.OperationError{ServiceID: "SQS", OperationName: "DeleteMessage", Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "private-credentials"}}, "broker", "DeleteMessage"},
		{"unexpected", errors.New("private-credentials"), "internal", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buffer bytes.Buffer
			slog.New(slog.NewJSONHandler(&buffer, nil)).Warn("failure", telemetry.ErrorFields(tt.err)...)
			logged := buffer.String()
			if !strings.Contains(logged, `"errorCategory":"`+tt.category+`"`) || !strings.Contains(logged, tt.expected) {
				t.Fatalf("diagnostic fields missing: %s", logged)
			}
			if strings.Contains(logged, "private-") {
				t.Fatalf("sensitive error details leaked: %s", logged)
			}
		})
	}
}

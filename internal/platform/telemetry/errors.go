package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"github.com/aws/smithy-go"
	"github.com/jackc/pgx/v5/pgconn"

	"junglegaming/internal/application"
	"junglegaming/internal/domain"
)

// Mensagens de drivers podem incluir credenciais e valores; registre apenas classificação e metadados técnicos.
func ErrorFields(err error) []any {
	category := "internal"
	var database *pgconn.PgError
	var operation *smithy.OperationError
	var api smithy.APIError
	var network net.Error
	var syntax *json.SyntaxError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		category = "timeout"
	case errors.Is(err, context.Canceled):
		category = "canceled"
	case errors.Is(err, application.ErrConflict):
		category = "conflict"
	case errors.Is(err, application.ErrLeaseLost):
		category = "lease_lost"
	case errors.Is(err, application.ErrPermanentInfrastructure):
		category = "permanent_infrastructure"
	case errors.Is(err, application.ErrNotFound):
		category = "not_found"
	case invalidInput(err), errors.As(err, &syntax):
		category = "invalid_input"
	case errors.As(err, &database):
		category = "database"
	case errors.As(err, &network):
		category = "network"
		if network.Timeout() {
			category = "timeout"
		}
	case errors.As(err, &operation), errors.As(err, &api):
		category = "broker"
	case errors.Is(err, application.ErrUnavailable):
		category = "unavailable"
	}
	cause := err
	for errors.Unwrap(cause) != nil {
		cause = errors.Unwrap(cause)
	}
	switch {
	case errors.As(err, &database):
		cause = database
	case errors.As(err, &api):
		cause = api
	case errors.As(err, &network):
		cause = network
	}
	fields := []any{"errorCategory", category, "errorType", fmt.Sprintf("%T", cause)}
	if errors.As(err, &database) {
		fields = append(fields, "sqlState", database.Code, "constraint", database.ConstraintName)
	}
	if errors.As(err, &operation) {
		fields = append(fields, "service", operation.Service(), "sdkOperation", operation.Operation())
	}
	if errors.As(err, &api) {
		fields = append(fields, "errorCode", api.ErrorCode())
	}
	var response interface {
		ServiceRequestID() string
		HTTPStatusCode() int
	}
	if errors.As(err, &response) {
		fields = append(fields, "requestId", response.ServiceRequestID(), "upstreamStatus", response.HTTPStatusCode())
	}
	return fields
}

func invalidInput(err error) bool {
	for _, known := range []error{application.ErrInvalidInput, application.ErrInvalidCursor, application.ErrWalletMismatch,
		domain.ErrInvalidID, domain.ErrInvalidAmount, domain.ErrInvalidCurrency, domain.ErrInvalidTransaction,
		domain.ErrInvalidReference, domain.ErrMoneyOverflow} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}

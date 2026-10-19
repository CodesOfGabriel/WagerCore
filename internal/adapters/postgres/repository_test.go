package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"junglegaming/internal/application"
	"testing"
)

func TestDatabaseFailureClassification(t *testing.T) {
	for _, tt := range []struct {
		code string
		want error
	}{
		{"23514", application.ErrPermanentInfrastructure}, {"23503", application.ErrPermanentInfrastructure},
		{"22003", application.ErrPermanentInfrastructure}, {"42P01", application.ErrPermanentInfrastructure},
		{"40001", application.ErrUnavailable}, {"40P01", application.ErrUnavailable},
		{"57P01", application.ErrUnavailable}, {"08006", application.ErrUnavailable}, {"P0001", application.ErrUnavailable},
		{"23505", application.ErrConflict},
	} {
		t.Run(tt.code, func(t *testing.T) {
			original := &pgconn.PgError{Code: tt.code}
			err := databaseError(original)
			var cause *pgconn.PgError
			if !errors.Is(err, tt.want) || !errors.As(err, &cause) || cause != original {
				t.Fatalf("classification: %v", err)
			}
		})
	}
	if err := databaseError(context.DeadlineExceeded); !errors.Is(err, application.ErrUnavailable) || errors.Is(err, application.ErrPermanentInfrastructure) {
		t.Fatalf("timeout classified as permanent: %v", err)
	}
}

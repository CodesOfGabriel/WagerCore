//go:build integration

package integration_test

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"junglegaming/internal/bootstrap"
	"junglegaming/internal/platform/config"
)

func TestApplicationLifecycleWithPostgres(t *testing.T) {
	db := newDatabase(t, true)
	var pool *pgxpool.Pool
	cfg := config.Defaults()
	cfg.HTTPAddr, cfg.DatabaseURL, cfg.HealthTimeout = "127.0.0.1:0", db.url(), time.Second
	cfg.OIDCIssuerURL, cfg.OIDCJWKSURL = issuerURL(), issuerURL()+"/protocol/openid-connect/certs"
	cfg.SQSEndpoint, cfg.WorkerRoles = brokerEndpoint(), "none"
	app := bootstrap.New(
		fx.Replace(cfg),
		fx.Replace(slog.New(slog.NewJSONHandler(io.Discard, nil))),
		fx.Populate(&pool),
	)
	if err := app.Start(db.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Stop(db.ctx); err != nil {
			t.Error(err)
		}
	})
	if err := pool.Ping(db.ctx); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(db.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Acquire(db.ctx); err == nil {
		t.Fatal("PostgreSQL pool remained open after shutdown")
	}
}

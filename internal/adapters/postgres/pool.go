package postgres

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"junglegaming/internal/platform/config"
)

func NewPool(lifecycle fx.Lifecycle, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	poolConfig.ConnConfig.ConnectTimeout = cfg.HealthTimeout
	poolConfig.MaxConns = int32(cfg.DatabaseMaxConnections)
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	if err != nil {
		return nil, errors.New("cannot create PostgreSQL pool")
	}
	lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, cfg.HealthTimeout)
			defer cancel()
			if err := pool.Ping(ctx); err != nil {
				pool.Close()
				return errors.New("PostgreSQL connection check failed")
			}
			logger.Info("postgres_connected")
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close()
			logger.Info("postgres_closed")
			return nil
		},
	})
	return pool, nil
}

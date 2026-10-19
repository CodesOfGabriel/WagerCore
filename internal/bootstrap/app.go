package bootstrap

import (
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	httpapi "junglegaming/internal/adapters/http"
	"junglegaming/internal/adapters/postgres"
	"junglegaming/internal/platform/config"
)

func New(options ...fx.Option) *fx.App {
	return fx.New(append([]fx.Option{module()}, options...)...)
}

func module() fx.Option {
	return fx.Options(
		fx.StartTimeout(10*time.Second),
		fx.StopTimeout(15*time.Second),
		fx.Provide(func() *slog.Logger {
			return slog.New(slog.NewJSONHandler(os.Stdout, nil))
		}),
		fx.WithLogger(func(logger *slog.Logger) fxevent.Logger {
			events := &fxevent.SlogLogger{Logger: logger}
			events.UseLogLevel(slog.LevelDebug)
			return events
		}),
		fx.Module("config", fx.Provide(config.Load)),
		fx.Module("postgres", fx.Provide(postgres.NewPool)),
		fx.Module("http",
			fx.Provide(func(pool *pgxpool.Pool, cfg config.Config) *httpapi.Health {
				return httpapi.NewHealth(pool.Ping, cfg.HealthTimeout)
			}),
			fx.Invoke(httpapi.RegisterServer),
		),
	)
}

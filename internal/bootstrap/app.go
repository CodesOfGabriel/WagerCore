package bootstrap

import (
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"junglegaming/internal/adapters/auth"
	httpapi "junglegaming/internal/adapters/http"
	"junglegaming/internal/adapters/postgres"
	sqsbroker "junglegaming/internal/adapters/sqs"
	"junglegaming/internal/application"
	"junglegaming/internal/platform/config"
	"junglegaming/internal/platform/fault"
	"junglegaming/internal/platform/telemetry"
	"junglegaming/internal/platform/workers"
)

func New(options ...fx.Option) *fx.App {
	return fx.New(append([]fx.Option{module()}, options...)...)
}

func module() fx.Option {
	return fx.Options(
		fx.StartTimeout(30*time.Second),
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
		fx.Module("postgres", fx.Provide(postgres.NewPool, postgres.NewWorkQueue,
			fx.Annotate(postgres.NewManager, fx.As(new(application.UnitOfWork))),
			fx.Annotate(postgres.NewRepository, fx.As(new(application.Reader))),
		)),
		fx.Module("auth", fx.Provide(auth.NewValidator), fx.Invoke(func(*auth.Validator) {})),
		fx.Module("messaging", fx.Provide(sqsbroker.NewBroker)),
		fx.Module("telemetry", fx.Provide(telemetry.New), fx.Provide(func(metrics *telemetry.Metrics) application.Observer { return metrics })),
		fx.Module("application", fx.Provide(
			fx.Annotate(fault.New, fx.As(new(application.Faults))),
			func(cfg config.Config) application.ReferencePolicy {
				return application.ReferencePolicy{TTL: cfg.ReferenceTTL, RetryBase: cfg.RetryBase, MaxAttempts: cfg.ReferenceMaxAttempts}
			},
			application.NewService,
		)),
		fx.Module("workers", fx.Invoke(workers.Register)),
		fx.Module("http",
			fx.Provide(func(pool *pgxpool.Pool, cfg config.Config, broker *sqsbroker.Broker) *httpapi.Health {
				return httpapi.NewHealth(pool.Ping, cfg.HealthTimeout, httpapi.Check{Name: "sqs", Run: broker.Ready})
			}),
			fx.Provide(httpapi.NewAPI, httpapi.NewRouter),
			fx.Invoke(httpapi.RegisterServer),
		),
	)
}

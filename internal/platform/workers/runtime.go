package workers

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"go.uber.org/fx"

	"junglegaming/internal/adapters/postgres"
	sqsbroker "junglegaming/internal/adapters/sqs"
	"junglegaming/internal/application"
	"junglegaming/internal/platform/config"
	"junglegaming/internal/platform/telemetry"
)

type Runtime struct {
	cfg     config.Config
	service *application.Service
	queue   *postgres.WorkQueue
	broker  *sqsbroker.Broker
	metrics *telemetry.Metrics
	faults  application.Faults
	logger  *slog.Logger
}

func Register(lifecycle fx.Lifecycle, cfg config.Config, service *application.Service, queue *postgres.WorkQueue, broker *sqsbroker.Broker, metrics *telemetry.Metrics, faults application.Faults, logger *slog.Logger) {
	r := &Runtime{cfg, service, queue, broker, metrics, faults, logger}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			var group sync.WaitGroup
			for role, run := range map[string]func(context.Context){"publisher": r.publisher, "consumer": r.consumer, "references": r.references} {
				if cfg.WorkerRoles == "all" || cfg.WorkerRoles == role {
					group.Add(1)
					go func(run func(context.Context)) { defer group.Done(); run(ctx) }(run)
				}
			}
			go func() { group.Wait(); close(done) }()
			logger.Info("workers_started", "roles", cfg.WorkerRoles)
			return nil
		},
		OnStop: func(stop context.Context) error {
			cancel()
			select {
			case <-done:
				logger.Info("workers_stopped")
				return nil
			case <-stop.Done():
				return stop.Err()
			}
		},
	})
}

func pause(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func backoff(base time.Duration, attempt int) time.Duration {
	if attempt > 10 {
		attempt = 10
	}
	delay := base
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay + time.Duration(rand.Int64N(int64(delay/4)+1))
}

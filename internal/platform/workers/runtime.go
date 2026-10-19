package workers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"go.uber.org/fx"

	"junglegaming/internal/adapters/postgres"
	sqsbroker "junglegaming/internal/adapters/sqs"
	"junglegaming/internal/application"
	"junglegaming/internal/identifier"
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

func (r *Runtime) publisher(ctx context.Context) {
	for ctx.Err() == nil {
		work, cancel := context.WithTimeout(ctx, 5*time.Second)
		items, err := r.queue.ClaimOutbox(work, r.cfg.WorkerConcurrency, r.cfg.LeaseDuration)
		cancel()
		if err != nil && ctx.Err() == nil {
			r.logger.Warn("outbox_claim_failed")
			r.metrics.Retry("outbox")
		}
		var group sync.WaitGroup
		for _, item := range items {
			group.Add(1)
			go func(item postgres.OutboxItem) { defer group.Done(); r.publish(ctx, item) }(item)
		}
		group.Wait()
		work, cancel = context.WithTimeout(ctx, 2*time.Second)
		pending, age, err := r.queue.OutboxStats(work)
		if err == nil {
			r.metrics.Pending.Set(float64(pending))
			r.metrics.Oldest.Set(age.Seconds())
		}
		size, err := r.broker.DLQSize(work)
		if err == nil {
			r.metrics.DLQ.Set(float64(size))
		}
		cancel()
		if !pause(ctx, r.cfg.WorkerInterval) {
			return
		}
	}
}

func (r *Runtime) publish(root context.Context, item postgres.OutboxItem) {
	ctx, cancel := context.WithTimeout(root, 5*time.Second)
	defer cancel()
	var envelope struct {
		CorrelationID string `json:"correlationId"`
		Data          struct {
			WalletID              string `json:"walletId"`
			ExternalTransactionID string `json:"externalTransactionId"`
		} `json:"data"`
	}
	err := json.Unmarshal(item.Payload, &envelope)
	if err == nil {
		err = r.broker.Publish(ctx, item.ID, envelope.Data.WalletID, item.Payload)
	}
	if err == nil {
		r.logger.Info("outbox_sent", "eventId", item.ID, "correlationId", envelope.CorrelationID)
		r.faults.Hit("after_event_publish", envelope.Data.ExternalTransactionID)
		err = r.queue.Published(ctx, item)
	}
	if err == nil {
		r.metrics.Published.Inc()
		return
	}
	if !errors.Is(err, application.ErrLeaseLost) {
		r.logger.Warn("outbox_publish_retry", "eventId", item.ID, "attempt", item.Attempts)
	}
	r.metrics.Retry("outbox")
	cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	_ = r.queue.RetryOutbox(cleanup, item, backoff(r.cfg.RetryBase, item.Attempts))
}

func (r *Runtime) references(ctx context.Context) {
	for ctx.Err() == nil {
		work, cancel := context.WithTimeout(ctx, 5*time.Second)
		items, err := r.queue.ClaimReferences(work, r.cfg.WorkerConcurrency, r.cfg.LeaseDuration)
		cancel()
		if err != nil && ctx.Err() == nil {
			r.logger.Warn("reference_claim_failed")
			r.metrics.Retry("references")
		}
		var group sync.WaitGroup
		for _, item := range items {
			group.Add(1)
			go func(item postgres.ReferenceItem) {
				defer group.Done()
				work, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if err := r.service.Resume(work, item.ID, item.LeaseID); err != nil && !errors.Is(err, application.ErrLeaseLost) {
					r.metrics.Retry("references")
					r.logger.Warn("reference_retry", "transactionId", item.ID)
					cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
					defer stop()
					_ = r.queue.RetryReference(cleanup, item, backoff(r.cfg.RetryBase, 1))
				}
			}(item)
		}
		group.Wait()
		if !pause(ctx, r.cfg.WorkerInterval) {
			return
		}
	}
}

func (r *Runtime) consumer(ctx context.Context) {
	for ctx.Err() == nil {
		messages, err := r.broker.Receive(ctx, r.cfg.WorkerConcurrency)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			r.metrics.Retry("sqs")
			r.logger.Warn("sqs_receive_failed")
			if !pause(ctx, backoff(r.cfg.RetryBase, 1)) {
				return
			}
			continue
		}
		var group sync.WaitGroup
		for _, message := range messages {
			group.Add(1)
			go func(message types.Message) { defer group.Done(); r.consume(ctx, message) }(message)
		}
		group.Wait()
	}
}

func (r *Runtime) consume(root context.Context, message types.Message) {
	ctx, cancel := context.WithTimeout(root, 10*time.Second)
	defer cancel()
	count, _ := strconv.Atoi(message.Attributes["ApproximateReceiveCount"])
	request, hash, err := sqsbroker.Decode(aws.ToString(message.Body))
	if err == nil {
		var correlation string
		correlation, err = identifier.New()
		if err == nil {
			r.logger.Info("sqs_received", "messageId", request.MessageID, "receiveCount", count, "correlationId", correlation)
			_, err = r.service.Process(ctx, request.Data.WagerRequest, request.Data.IdempotencyKey, application.Trace{CorrelationID: correlation, CausationID: request.MessageID}, &application.IncomingMessage{Consumer: "wager-transactions", ID: request.MessageID, Hash: hash})
		}
	}
	if err == nil {
		r.faults.Hit("after_sqs_commit", request.Data.ExternalTransactionID)
		err = r.broker.Delete(ctx, message.ReceiptHandle)
	}
	if err == nil {
		r.logger.Info("sqs_completed", "messageId", request.MessageID, "receiveCount", count)
		return
	}
	r.metrics.Retry("sqs")
	r.logger.Warn("sqs_message_retry", "messageId", request.MessageID, "receiveCount", count)
	cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	delay := int32((backoff(r.cfg.RetryBase, count) + time.Second - 1) / time.Second)
	if root.Err() != nil {
		delay = 0
	}
	_ = r.broker.Visibility(cleanup, message.ReceiptHandle, delay)
}

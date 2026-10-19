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
			r.logger.Warn("outbox_claim_failed", append([]any{"stage", "claim_outbox"}, telemetry.ErrorFields(err)...)...)
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
		} else if ctx.Err() == nil {
			r.logger.Warn("outbox_stats_failed", append([]any{"stage", "read_outbox_stats"}, telemetry.ErrorFields(err)...)...)
		}
		size, err := r.broker.DLQSize(work)
		if err == nil {
			r.metrics.DLQ.Set(float64(size))
		} else if ctx.Err() == nil {
			r.logger.Warn("dlq_stats_failed", append([]any{"stage", "read_dlq_stats"}, telemetry.ErrorFields(err)...)...)
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
		CausationID   string `json:"causationId"`
		Data          struct {
			WalletID              string `json:"walletId"`
			TransactionID         string `json:"transactionId"`
			ExternalTransactionID string `json:"externalTransactionId"`
		} `json:"data"`
	}
	stage := "decode_event"
	err := json.Unmarshal(item.Payload, &envelope)
	attrs := []any{"eventId", item.ID, "aggregateId", item.AggregateID, "correlationId", envelope.CorrelationID, "messageId", envelope.CausationID, "walletId", envelope.Data.WalletID, "transactionId", envelope.Data.TransactionID, "attempt", item.Attempts}
	if err == nil {
		stage = "publish"
		err = r.broker.Publish(ctx, item.ID, envelope.Data.WalletID, item.Payload)
	}
	if err == nil {
		r.logger.Info("outbox_sent", attrs...)
		r.faults.Hit("after_event_publish", envelope.Data.ExternalTransactionID)
		stage = "confirm_publication"
		err = r.queue.Published(ctx, item)
	}
	if err == nil {
		r.metrics.Published.Inc()
		return
	}
	delay := backoff(r.cfg.RetryBase, item.Attempts)
	failure := append(append([]any{}, attrs...), "stage", stage, "retryDelayMs", delay.Milliseconds())
	failure = append(failure, telemetry.ErrorFields(err)...)
	if errors.Is(err, application.ErrLeaseLost) {
		r.logger.Debug("outbox_lease_lost", failure...)
	} else {
		r.logger.Warn("outbox_publish_retry", failure...)
	}
	r.metrics.Retry("outbox")
	cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := r.queue.RetryOutbox(cleanup, item, delay); err != nil {
		r.logger.Warn("outbox_retry_schedule_failed", append(append(attrs, "stage", "schedule_retry"), telemetry.ErrorFields(err)...)...)
	}
}

func (r *Runtime) references(ctx context.Context) {
	for ctx.Err() == nil {
		work, cancel := context.WithTimeout(ctx, 5*time.Second)
		items, err := r.queue.ClaimReferences(work, r.cfg.WorkerConcurrency, r.cfg.LeaseDuration)
		cancel()
		if err != nil && ctx.Err() == nil {
			r.logger.Warn("reference_claim_failed", append([]any{"stage", "claim_references"}, telemetry.ErrorFields(err)...)...)
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
					r.logger.Warn("reference_retry", append([]any{"transactionId", item.ID, "stage", "resume_reference"}, telemetry.ErrorFields(err)...)...)
					cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
					defer stop()
					if err := r.queue.RetryReference(cleanup, item, backoff(r.cfg.RetryBase, 1)); err != nil {
						r.logger.Warn("reference_retry_schedule_failed", append([]any{"transactionId", item.ID, "stage", "schedule_retry"}, telemetry.ErrorFields(err)...)...)
					}
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
			r.logger.Warn("sqs_receive_failed", append([]any{"stage", "receive"}, telemetry.ErrorFields(err)...)...)
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
	stage := "create_correlation"
	correlation, err := identifier.New()
	var request sqsbroker.Requested
	var result application.WagerResult
	attrs := []any{"sqsMessageId", aws.ToString(message.MessageId), "receiveCount", count, "correlationId", correlation}
	if err == nil {
		stage = "decode"
		var hash []byte
		request, hash, err = sqsbroker.Decode(aws.ToString(message.Body))
		if err == nil {
			attrs = append(attrs, "messageId", request.MessageID, "walletId", request.Data.WalletID, "providerId", request.Data.ProviderID, "externalTransactionId", request.Data.ExternalTransactionID)
			r.logger.Info("sqs_received", attrs...)
			stage = "process"
			result, err = r.service.Process(ctx, request.Data.WagerRequest, request.Data.IdempotencyKey, application.Trace{CorrelationID: correlation, CausationID: request.MessageID}, &application.IncomingMessage{Consumer: "wager-transactions", ID: request.MessageID, Hash: hash})
			if err == nil {
				attrs = append(attrs, "transactionId", result.TransactionID, "status", result.Status, "idempotentReplay", result.IdempotentReplay)
				r.logger.Info("sqs_processed", attrs...)
			}
		}
	}
	if err == nil {
		r.faults.Hit("after_sqs_commit", request.Data.ExternalTransactionID)
		stage = "acknowledge"
		err = r.broker.Delete(ctx, message.ReceiptHandle)
	}
	if err == nil {
		r.logger.Info("sqs_completed", attrs...)
		return
	}
	r.metrics.Retry("sqs")
	delay := int32((backoff(r.cfg.RetryBase, count) + time.Second - 1) / time.Second)
	if root.Err() != nil {
		delay = 0
	}
	failure := append(append([]any{}, attrs...), "stage", stage, "retryDelaySeconds", delay)
	r.logger.Warn("sqs_message_retry", append(failure, telemetry.ErrorFields(err)...)...)
	cleanup, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	if err := r.broker.Visibility(cleanup, message.ReceiptHandle, delay); err != nil {
		r.logger.Warn("sqs_visibility_failed", append(append(attrs, "stage", "change_visibility"), telemetry.ErrorFields(err)...)...)
	}
}

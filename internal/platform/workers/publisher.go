package workers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"junglegaming/internal/adapters/postgres"
	"junglegaming/internal/application"
	"junglegaming/internal/platform/config"
	"junglegaming/internal/platform/telemetry"
)

func (r *Runtime) publisher(ctx context.Context) {
	var monitoring sync.WaitGroup
	monitoring.Add(2)
	go func() {
		defer monitoring.Done()
		r.publisherStats(ctx)
	}()
	go func() {
		defer monitoring.Done()
		r.dlqStats(ctx)
	}()
	defer monitoring.Wait()
	for ctx.Err() == nil {
		work, cancel := context.WithTimeout(ctx, config.ClaimTimeout)
		started := time.Now()
		items, err := r.queue.ClaimOutbox(work, r.cfg.WorkerConcurrency, r.cfg.LeaseDuration)
		r.metrics.OutboxClaim.Observe(time.Since(started).Seconds())
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
		// Busy publishers immediately claim the next wallet heads. Poll only when idle or failing.
		if (err != nil || len(items) == 0) && !pause(ctx, r.cfg.WorkerInterval) {
			return
		}
	}
}

func (r *Runtime) publisherStats(ctx context.Context) {
	for ctx.Err() == nil {
		r.queue.ObservePool(r.metrics)
		work, cancel := context.WithTimeout(ctx, 2*time.Second)
		pending, age, err := r.queue.OutboxStats(work)
		if err == nil {
			r.metrics.Pending.Set(float64(pending))
			r.metrics.Oldest.Set(age.Seconds())
		} else if ctx.Err() == nil {
			r.logger.Warn("outbox_stats_failed", append([]any{"stage", "read_outbox_stats"}, telemetry.ErrorFields(err)...)...)
		}
		cancel()
		if !pause(ctx, time.Second) {
			return
		}
	}
}

func (r *Runtime) dlqStats(ctx context.Context) {
	for ctx.Err() == nil {
		work, cancel := context.WithTimeout(ctx, 2*time.Second)
		size, err := r.broker.DLQSize(work)
		cancel()
		if err == nil {
			r.metrics.DLQ.Set(float64(size))
		} else if ctx.Err() == nil {
			r.logger.Warn("dlq_stats_failed", append([]any{"stage", "read_dlq_stats"}, telemetry.ErrorFields(err)...)...)
		}
		if !pause(ctx, r.cfg.DLQPollInterval) {
			return
		}
	}
}

func (r *Runtime) publish(root context.Context, item postgres.OutboxItem) {
	ctx, cancel := context.WithTimeout(root, config.PublishTimeout)
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
		started := time.Now()
		err = r.broker.Publish(ctx, item.ID, envelope.Data.WalletID, item.Payload)
		r.metrics.OutboxSend.Observe(time.Since(started).Seconds())
	}
	if err == nil {
		r.logger.Info("outbox_sent", attrs...)
		r.faults.Hit("after_event_publish", envelope.Data.ExternalTransactionID)
		stage = "confirm_publication"
		started := time.Now()
		err = r.queue.Published(ctx, item)
		r.metrics.OutboxConfirm.Observe(time.Since(started).Seconds())
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
	cleanup, stop := context.WithTimeout(context.Background(), config.CleanupTimeout)
	defer stop()
	if err := r.queue.RetryOutbox(cleanup, item, delay); err != nil {
		if errors.Is(err, application.ErrLeaseLost) {
			r.logger.Debug("outbox_retry_lease_lost", attrs...)
		} else {
			r.logger.Warn("outbox_retry_schedule_failed", append(append(attrs, "stage", "schedule_retry"), telemetry.ErrorFields(err)...)...)
		}
	}
}

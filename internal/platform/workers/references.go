package workers

import (
	"context"
	"errors"
	"sync"

	"junglegaming/internal/adapters/postgres"
	"junglegaming/internal/application"
	"junglegaming/internal/platform/config"
	"junglegaming/internal/platform/telemetry"
)

func (r *Runtime) references(ctx context.Context) {
	for ctx.Err() == nil {
		work, cancel := context.WithTimeout(ctx, config.ClaimTimeout)
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
				work, cancel := context.WithTimeout(ctx, config.ReferenceTimeout)
				defer cancel()
				if err := r.service.Resume(work, item.ID, item.LeaseID); err != nil && !errors.Is(err, application.ErrLeaseLost) {
					if errors.Is(err, application.ErrPermanentInfrastructure) && !errors.Is(err, application.ErrUnavailable) {
						r.logger.Error("reference_failed", append([]any{"transactionId", item.ID, "stage", "resume_reference"}, telemetry.ErrorFields(err)...)...)
						return
					}
					r.metrics.Retry("references")
					r.logger.Warn("reference_retry", append([]any{"transactionId", item.ID, "stage", "resume_reference"}, telemetry.ErrorFields(err)...)...)
					cleanup, stop := context.WithTimeout(context.Background(), config.CleanupTimeout)
					defer stop()
					if err := r.queue.RetryReference(cleanup, item, backoff(r.cfg.RetryBase, 1)); err != nil {
						if errors.Is(err, application.ErrLeaseLost) {
							r.logger.Debug("reference_retry_lease_lost", "transactionId", item.ID)
						} else {
							r.logger.Warn("reference_retry_schedule_failed", append([]any{"transactionId", item.ID, "stage", "schedule_retry"}, telemetry.ErrorFields(err)...)...)
						}
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

package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"junglegaming/internal/application"
	"junglegaming/internal/identifier"
	"junglegaming/internal/platform/telemetry"
)

type WorkQueue struct{ pool *pgxpool.Pool }

func NewWorkQueue(pool *pgxpool.Pool) *WorkQueue { return &WorkQueue{pool} }

func (q *WorkQueue) ObservePool(metrics *telemetry.Metrics) {
	stats := q.pool.Stat()
	metrics.PoolAcquired.Set(float64(stats.AcquiredConns()))
	metrics.PoolMax.Set(float64(stats.MaxConns()))
	metrics.PoolWait.Set(stats.EmptyAcquireWaitTime().Seconds())
}

type OutboxItem struct {
	ID, AggregateID, LeaseID string
	Payload                  []byte
	Attempts                 int
}
type ReferenceItem struct{ ID, LeaseID string }

func (q *WorkQueue) ClaimOutbox(ctx context.Context, limit int, lease time.Duration) ([]OutboxItem, error) {
	token, err := identifier.New()
	if err != nil {
		return nil, err
	}
	rows, err := q.pool.Query(ctx, `WITH candidates AS (
		SELECT o.event_id FROM outbox o WHERE o.published_at IS NULL AND o.next_attempt_at<=clock_timestamp()
            AND (o.lease_until IS NULL OR o.lease_until<=clock_timestamp())
            AND NOT EXISTS (SELECT 1 FROM outbox earlier WHERE earlier.wallet_id=o.wallet_id
                AND earlier.sequence<o.sequence AND earlier.published_at IS NULL)
        ORDER BY o.sequence FOR UPDATE OF o SKIP LOCKED LIMIT $1)
        UPDATE outbox o SET lease_id=$2,lease_until=clock_timestamp()+$3::interval,attempts=o.attempts+1
        FROM candidates c WHERE o.event_id=c.event_id
        RETURNING o.event_id::text,o.aggregate_id::text,o.payload,o.attempts`, limit, token, lease.String())
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	items := []OutboxItem{}
	for rows.Next() {
		item := OutboxItem{LeaseID: token}
		if err := rows.Scan(&item.ID, &item.AggregateID, &item.Payload, &item.Attempts); err != nil {
			return nil, databaseError(err)
		}
		items = append(items, item)
	}
	return items, databaseError(rows.Err())
}

func (q *WorkQueue) Published(ctx context.Context, item OutboxItem) error {
	tag, err := q.pool.Exec(ctx, `UPDATE outbox SET published_at=clock_timestamp(),lease_id=NULL,lease_until=NULL
        WHERE event_id=$1 AND lease_id=$2 AND lease_until>clock_timestamp() AND published_at IS NULL`, item.ID, item.LeaseID)
	if err != nil {
		return databaseError(err)
	}
	if tag.RowsAffected() != 1 {
		return application.ErrLeaseLost
	}
	return nil
}

func (q *WorkQueue) RetryOutbox(ctx context.Context, item OutboxItem, delay time.Duration) error {
	tag, err := q.pool.Exec(ctx, `UPDATE outbox SET lease_id=NULL,lease_until=NULL,next_attempt_at=clock_timestamp()+$3::interval
        WHERE event_id=$1 AND lease_id=$2 AND lease_until>clock_timestamp() AND published_at IS NULL`, item.ID, item.LeaseID, delay.String())
	if err != nil {
		return databaseError(err)
	}
	if tag.RowsAffected() != 1 {
		return application.ErrLeaseLost
	}
	return nil
}

func (q *WorkQueue) ClaimReferences(ctx context.Context, limit int, lease time.Duration) ([]ReferenceItem, error) {
	token, err := identifier.New()
	if err != nil {
		return nil, err
	}
	rows, err := q.pool.Query(ctx, `WITH candidates AS (
        SELECT id FROM wager_transactions WHERE status='PENDING_REFERENCE' AND next_retry_at<=clock_timestamp()
            AND (lease_until IS NULL OR lease_until<=clock_timestamp())
        ORDER BY next_retry_at,id FOR UPDATE SKIP LOCKED LIMIT $1)
        UPDATE wager_transactions w SET lease_id=$2,lease_until=clock_timestamp()+$3::interval
        FROM candidates c WHERE w.id=c.id RETURNING w.id::text`, limit, token, lease.String())
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	items := []ReferenceItem{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, databaseError(err)
		}
		items = append(items, ReferenceItem{id, token})
	}
	return items, databaseError(rows.Err())
}

func (q *WorkQueue) RetryReference(ctx context.Context, item ReferenceItem, delay time.Duration) error {
	tag, err := q.pool.Exec(ctx, `UPDATE wager_transactions SET lease_id=NULL,lease_until=NULL,
        next_retry_at=LEAST(clock_timestamp()+$3::interval,reference_expires_at)
        WHERE id=$1 AND lease_id=$2 AND lease_until>clock_timestamp() AND status='PENDING_REFERENCE'`, item.ID, item.LeaseID, delay.String())
	if err != nil {
		return databaseError(err)
	}
	if tag.RowsAffected() != 1 {
		return application.ErrLeaseLost
	}
	return nil
}

func (q *WorkQueue) OutboxStats(ctx context.Context) (int64, time.Duration, error) {
	var pending int64
	var oldest *time.Time
	var now time.Time
	err := q.pool.QueryRow(ctx, "SELECT count(*),min(occurred_at),clock_timestamp() FROM outbox WHERE published_at IS NULL").Scan(&pending, &oldest, &now)
	age := time.Duration(0)
	if oldest != nil {
		age = now.Sub(*oldest)
		if age < 0 {
			age = 0
		}
	}
	return pending, age, databaseError(err)
}

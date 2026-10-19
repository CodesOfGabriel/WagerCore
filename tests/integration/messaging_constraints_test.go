//go:build integration

package integration_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"junglegaming/internal/adapters/postgres"
	"junglegaming/internal/application"
	"junglegaming/internal/domain"
)

func TestMessagingRetentionOrderingAndLeaseFencing(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db)
	internal := token(t, "internal-service")
	w := openAPIWallet(t, p, internal, "100.00")
	other := openAPIWallet(t, p, internal, "10.00")
	for _, query := range []string{"UPDATE outbox SET payload='{}'::jsonb", "DELETE FROM outbox", "TRUNCATE outbox"} {
		db.expectFailure(t, "23514", func(tx pgx.Tx) error { _, err := tx.Exec(db.ctx, query); return err })
	}
	db.expectFailure(t, "23514", func(tx pgx.Tx) error {
		_, err := tx.Exec(db.ctx, `INSERT INTO inbox (consumer_name,message_id,payload_hash,received_at) VALUES ('test','unfinished',decode(repeat('00',32),'hex'),clock_timestamp())`)
		return err
	})
	pool, err := pgxpool.New(db.ctx, db.url())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	queue := postgres.NewWorkQueue(pool)
	first, err := queue.ClaimOutbox(db.ctx, 10, 50*time.Millisecond)
	if err != nil || len(first) != 2 {
		t.Fatalf("claim=%+v err=%v", first, err)
	}
	// Há dois eventos por carteira, mas somente o primeiro de cada carteira pode estar em publicação.
	blocked, err := queue.ClaimOutbox(db.ctx, 10, time.Second)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("wallet event order was bypassed: %+v %v", blocked, err)
	}
	time.Sleep(80 * time.Millisecond)
	second, err := queue.ClaimOutbox(db.ctx, 10, time.Second)
	if err != nil || len(second) != 2 {
		t.Fatalf("expired leases not recovered: %+v %v", second, err)
	}
	for _, item := range first {
		if err := queue.Published(db.ctx, item); !errors.Is(err, application.ErrLeaseLost) {
			t.Fatalf("old owner confirmed event: %v", err)
		}
		if err := queue.RetryOutbox(db.ctx, item, time.Hour); !errors.Is(err, application.ErrLeaseLost) {
			t.Fatalf("old owner rescheduled event: %v", err)
		}
	}
	for _, item := range second {
		if err := queue.Published(db.ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	remaining, err := queue.ClaimOutbox(db.ctx, 10, time.Second)
	if err != nil || len(remaining) != 2 {
		t.Fatalf("next wallet events not released: %+v %v", remaining, err)
	}
	for _, item := range remaining {
		if err := queue.Published(db.ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if db.count(t, "SELECT count(*) FROM outbox WHERE published_at IS NULL") != 0 {
		t.Fatal("events were lost")
	}
	db.assertBalance(t, w, 10000, 1)
	db.assertBalance(t, other, 1000, 1)
}

func TestRetryLeaseFencing(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, p, internal, "100.00")
	pool, err := pgxpool.New(db.ctx, db.url())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	queue := postgres.NewWorkQueue(pool)
	items, err := queue.ClaimOutbox(db.ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim: %v %v", items, err)
	}
	item := items[0]
	if _, err := db.conn.Exec(db.ctx, "UPDATE outbox SET lease_until=clock_timestamp()-interval '1 second' WHERE event_id=$1", item.ID); err != nil {
		t.Fatal(err)
	}
	if err := queue.RetryOutbox(db.ctx, item, time.Hour); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("expired outbox lease accepted: %v", err)
	}
	items, err = queue.ClaimOutbox(db.ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("reclaim: %v %v", items, err)
	}
	item = items[0]
	if err := queue.RetryOutbox(db.ctx, item, time.Hour); err != nil {
		t.Fatal(err)
	}
	if db.count(t, "SELECT count(*) FROM outbox WHERE event_id=$1 AND lease_id IS NULL AND lease_until IS NULL AND next_attempt_at>clock_timestamp()+interval '59 minutes'", item.ID) != 1 {
		t.Fatal("outbox retry not scheduled")
	}
	if err := queue.RetryOutbox(db.ctx, item, 0); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("released outbox lease accepted: %v", err)
	}
	if _, err := db.conn.Exec(db.ctx, "UPDATE outbox SET next_attempt_at=clock_timestamp() WHERE event_id=$1", item.ID); err != nil {
		t.Fatal(err)
	}
	items, err = queue.ClaimOutbox(db.ctx, 10, time.Minute)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim after retry: %v %v", items, err)
	}
	if err := queue.Published(db.ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	if err := queue.RetryOutbox(db.ctx, items[0], 0); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("published event rescheduled: %v", err)
	}

	bet := request(w, domain.Bet, "1.00")
	refund := request(w, domain.Refund, "1.00")
	refund.ReferenceExternalTransactionID = bet.ExternalTransactionID
	pending := wager(t, p, provider, refund, newID(), 202)
	if _, err := db.conn.Exec(db.ctx, "UPDATE wager_transactions SET next_retry_at=clock_timestamp() WHERE id=$1", pending.TransactionID); err != nil {
		t.Fatal(err)
	}
	references, err := queue.ClaimReferences(db.ctx, 10, time.Minute)
	if err != nil || len(references) != 1 {
		t.Fatalf("reference claim: %v %v", references, err)
	}
	first := references[0]
	if _, err := db.conn.Exec(db.ctx, "UPDATE wager_transactions SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := queue.RetryReference(db.ctx, first, time.Hour); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("expired reference lease accepted: %v", err)
	}
	references, err = queue.ClaimReferences(db.ctx, 10, time.Minute)
	if err != nil || len(references) != 1 {
		t.Fatalf("reference reclaim: %v %v", references, err)
	}
	if err := queue.RetryReference(db.ctx, first, 0); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("old reference owner rescheduled work: %v", err)
	}
	if err := queue.RetryReference(db.ctx, references[0], time.Hour); err != nil {
		t.Fatal(err)
	}
	if db.count(t, "SELECT count(*) FROM wager_transactions WHERE id=$1 AND lease_id IS NULL AND lease_until IS NULL AND next_retry_at=reference_expires_at", first.ID) != 1 {
		t.Fatal("reference retry not scheduled at TTL limit")
	}
	if err := queue.RetryReference(db.ctx, references[0], 0); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("released reference lease accepted: %v", err)
	}
	if err := queue.RetryOutbox(db.ctx, postgres.OutboxItem{ID: newID(), LeaseID: newID()}, 0); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("missing event accepted: %v", err)
	}
	if err := queue.RetryReference(db.ctx, postgres.ReferenceItem{ID: newID(), LeaseID: newID()}, 0); !errors.Is(err, application.ErrLeaseLost) {
		t.Fatalf("missing reference accepted: %v", err)
	}
}

func TestOutboxStatsUseDatabaseClock(t *testing.T) {
	db := newDatabase(t, true)
	pool, err := pgxpool.New(db.ctx, db.url())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	queue := postgres.NewWorkQueue(pool)
	if pending, age, err := queue.OutboxStats(db.ctx); err != nil || pending != 0 || age != 0 {
		t.Fatalf("empty outbox: pending=%d age=%v err=%v", pending, age, err)
	}
	p := startServer(t, db)
	openAPIWallet(t, p, token(t, "internal-service"), "100.00")
	var oldest, before, after time.Time
	if err := db.conn.QueryRow(db.ctx, "SELECT min(occurred_at),clock_timestamp() FROM outbox").Scan(&oldest, &before); err != nil {
		t.Fatal(err)
	}
	pending, age, err := queue.OutboxStats(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.conn.QueryRow(db.ctx, "SELECT clock_timestamp()").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if pending != 2 || age < before.Sub(oldest) || age > after.Sub(oldest) {
		t.Fatalf("age must use the database clock: pending=%d age=%v range=[%v,%v]", pending, age, before.Sub(oldest), after.Sub(oldest))
	}
}

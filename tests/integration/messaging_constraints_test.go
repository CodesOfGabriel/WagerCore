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

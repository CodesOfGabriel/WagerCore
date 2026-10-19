//go:build integration

package integration_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"junglegaming/internal/domain"
	"junglegaming/internal/platform/migration"
)

func TestMigrationApplyRevertAndChecksum(t *testing.T) {
	db := newDatabase(t, false)
	for _, tt := range []struct {
		action  string
		version int64
		changed int
	}{
		{"status", 0, 0}, {"up", 2, 2}, {"up", 2, 0}, {"status", 2, 0},
		{"down", 1, 1}, {"down", 0, 1}, {"down", 0, 0}, {"up", 2, 2},
	} {
		result, err := db.runner.Run(db.ctx, tt.action)
		if err != nil || result.Version != tt.version || result.Changed != tt.changed {
			t.Fatalf("action=%s result=%+v error=%v", tt.action, result, err)
		}
		var exists bool
		if err := db.conn.QueryRow(db.ctx, "SELECT to_regclass('wallets') IS NOT NULL").Scan(&exists); err != nil || exists != (tt.version > 0) {
			t.Fatalf("schema state: exists=%t error=%v", exists, err)
		}
	}
	if _, err := db.conn.Exec(db.ctx, "UPDATE schema_migrations SET checksum = decode(repeat('00', 32), 'hex')"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"up", "down", "status"} {
		if _, err := db.runner.Run(db.ctx, action); err == nil || !strings.Contains(err.Error(), "differs") {
			t.Fatalf("changed migration was accepted: %v", err)
		}
	}
}

func TestMigrationFailureRollsBackDDL(t *testing.T) {
	db := newDatabase(t, false)
	if _, err := db.conn.Exec(db.ctx, "CREATE TABLE wager_transactions (placeholder INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.runner.Run(db.ctx, "up"); err == nil {
		t.Fatal("conflicting migration succeeded")
	}
	var clean bool
	if err := db.conn.QueryRow(db.ctx, "SELECT to_regclass('wallets') IS NULL AND to_regclass('schema_migrations') IS NULL").Scan(&clean); err != nil || !clean {
		t.Fatalf("migration left partial DDL: clean=%t error=%v", clean, err)
	}
}

func TestConcurrentMigrations(t *testing.T) {
	db := newDatabase(t, false)
	second, err := migration.New(db.connect(t))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type outcome struct {
		result migration.Result
		err    error
	}
	results := make(chan outcome, 2)
	for _, runner := range []*migration.Runner{db.runner, second} {
		go func(runner *migration.Runner) {
			<-start
			result, err := runner.Run(db.ctx, "up")
			results <- outcome{result, err}
		}(runner)
	}
	close(start)
	changed := 0
	for range 2 {
		outcome := <-results
		if outcome.err != nil || outcome.result.Version != 2 {
			t.Fatalf("concurrent migration result=%+v error=%v", outcome.result, outcome.err)
		}
		changed += outcome.result.Changed
	}
	if changed != 2 {
		t.Fatalf("applied %d migrations, want 2", changed)
	}
}

func TestMigrationRevertWithFinancialHistory(t *testing.T) {
	db := newDatabase(t, true)
	w := db.openWallet(t, 10000)
	db.processAndCommit(t, newOperation(w, "BET", 2500), "DEBIT")
	for _, want := range []int64{1, 0} {
		result, err := db.runner.Run(db.ctx, "down")
		if err != nil || result.Version != want {
			t.Fatalf("result=%+v error=%v", result, err)
		}
	}
	if _, err := db.runner.Run(db.ctx, "up"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.conn.QueryRow(db.ctx, "SELECT count(*) FROM wallets").Scan(&count); err != nil || count != 0 {
		t.Fatalf("recreated schema count=%d error=%v", count, err)
	}
}

func TestWalletDatabaseConstraints(t *testing.T) {
	db := newDatabase(t, true)
	positive := db.openWallet(t, 10000)
	zero := db.openWallet(t, 0)
	db.assertBalance(t, positive, 10000, 1)
	db.assertBalance(t, zero, 0, 1)
	db.expectFailure(t, "23505", func(tx pgx.Tx) error {
		_, err := tx.Exec(db.ctx, `INSERT INTO wager_transactions
            (id, wallet_id, player_id, currency, amount_minor, kind, status, result_balance_minor, created_at, updated_at)
            VALUES ($1,$2,$3,'BRL',10000,'OPENING','PROCESSED',10000,now(),now())`, newID(), positive.id, positive.player)
		return err
	})
	var entries int
	if err := db.conn.QueryRow(db.ctx, "SELECT count(*) FROM wallet_ledger WHERE wallet_id=$1", zero.id).Scan(&entries); err != nil || entries != 0 {
		t.Fatalf("zero opening entries=%d error=%v", entries, err)
	}
	db.processAndCommit(t, newOperation(zero, "WIN", 100), "CREDIT")
	db.assertBalance(t, zero, 100, 2)
	for _, tt := range []struct {
		name, query, code string
		args              []any
	}{
		{"negative balance", "UPDATE wallets SET balance_minor=-1 WHERE id=$1", "23514", []any{positive.id}},
		{"unbacked balance", "UPDATE wallets SET balance_minor=20000 WHERE id=$1", "23514", []any{positive.id}},
		{"unbacked version", "UPDATE wallets SET version=2 WHERE id=$1", "23514", []any{positive.id}},
		{"changed identity", "UPDATE wallets SET currency='USD' WHERE id=$1", "23514", []any{positive.id}},
		{"duplicate wallet", "INSERT INTO wallets VALUES ($1,$2,'BRL',0,1,now(),now())", "23505", []any{newID(), positive.player}},
		{"unsupported currency", "INSERT INTO wallets VALUES ($1,$2,'ZZZ',0,1,now(),now())", "23514", []any{newID(), newID()}},
		{"invalid initial version", "INSERT INTO wallets VALUES ($1,$2,'BRL',0,2,now(),now())", "23514", []any{newID(), newID()}},
		{"opening without ledger", "INSERT INTO wallets VALUES ($1,$2,'BRL',100,1,now(),now())", "23514", []any{newID(), newID()}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db.expectFailure(t, tt.code, func(tx pgx.Tx) error {
				_, err := tx.Exec(db.ctx, tt.query, tt.args...)
				return err
			})
			db.assertBalance(t, positive, 10000, 1)
		})
	}
}

func TestTransactionIdentityAndInputConstraints(t *testing.T) {
	db := newDatabase(t, true)
	w := db.openWallet(t, 10000)
	original := newOperation(w, "BET", 2500)
	if err := insertOperation(db.ctx, db.conn, original); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		edit func(*operation)
		code string
	}{
		{"same key", func(op *operation) { op.key = original.key }, "23505"},
		{"same external id", func(op *operation) { op.external = original.external }, "23505"},
		{"missing provider", func(op *operation) { op.provider = "" }, "23514"},
		{"unknown kind", func(op *operation) { op.kind = "OTHER" }, "23514"},
		{"zero BET", func(op *operation) { op.amount = 0 }, "23514"},
		{"positive LOSS", func(op *operation) { op.kind = "LOSS" }, "23514"},
		{"missing reversal reference", func(op *operation) { op.kind = "REFUND" }, "23514"},
		{"external OPENING", func(op *operation) { op.kind = "OPENING" }, "23514"},
		{"missing result", func(op *operation) { op.status = "PROCESSED" }, "23514"},
		{"missing rejection code", func(op *operation) { op.status = "REJECTED"; balance := int64(10000); op.balance = &balance }, "23514"},
		{"wrong wallet player", func(op *operation) { op.wallet.player = newID() }, "23503"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			op := newOperation(w, "BET", 2500)
			tt.edit(&op)
			db.expectFailure(t, tt.code, func(tx pgx.Tx) error { return insertOperation(db.ctx, tx, op) })
		})
	}
	otherProvider := original
	otherProvider.id, otherProvider.provider = newID(), "provider-b"
	if err := insertOperation(db.ctx, db.conn, otherProvider); err != nil {
		t.Fatal(err)
	}
	db.expectFailure(t, "23514", func(tx pgx.Tx) error {
		_, err := tx.Exec(db.ctx, "UPDATE wager_transactions SET payload_hash=decode('ab','hex') WHERE id=$1", original.id)
		return err
	})
	db.assertBalance(t, w, 10000, 1)
}

func TestLedgerImmutabilityAndAtomicity(t *testing.T) {
	db := newDatabase(t, true)
	w := db.openWallet(t, 10000)
	bet := newOperation(w, "BET", 2500)
	db.processAndCommit(t, bet, "DEBIT")
	db.assertBalance(t, w, 7500, 2)
	for _, query := range []string{
		"UPDATE wallet_ledger SET amount_minor=1 WHERE wallet_id=$1",
		"DELETE FROM wallet_ledger WHERE wallet_id=$1",
	} {
		db.expectFailure(t, "23514", func(tx pgx.Tx) error {
			_, err := tx.Exec(db.ctx, query, w.id)
			return err
		})
	}
	db.expectFailure(t, "23514", func(tx pgx.Tx) error {
		_, err := tx.Exec(db.ctx, "TRUNCATE wallet_ledger")
		return err
	})
	db.expectFailure(t, "23505", func(tx pgx.Tx) error {
		return insertLedger(db.ctx, tx, w.id, bet.id, "DEBIT", 2500, 7500, 5000, 3)
	})
	db.expectFailure(t, "23514", func(tx pgx.Tx) error {
		op := newOperation(w, "BET", 500)
		if err := db.process(db.ctx, tx, op, "DEBIT"); err != nil {
			return err
		}
		_, err := tx.Exec(db.ctx, "UPDATE wallets SET balance_minor=-1 WHERE id=$1", w.id)
		return err
	})
	db.assertBalance(t, w, 7500, 2)
	var count int
	if err := db.conn.QueryRow(db.ctx, "SELECT count(*) FROM wager_transactions WHERE wallet_id=$1", w.id).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rolled back operation remains: count=%d error=%v", count, err)
	}
}

func TestLedgerRequiresMatchingFinancialOperation(t *testing.T) {
	db := newDatabase(t, true)
	for _, tt := range []struct {
		name string
		run  func(wallet, pgx.Tx) error
	}{
		{"processed without ledger", func(w wallet, tx pgx.Tx) error {
			op := newOperation(w, "BET", 2500)
			balance := int64(7500)
			op.status, op.balance = "PROCESSED", &balance
			return insertOperation(db.ctx, tx, op)
		}},
		{"wrong financial direction", func(w wallet, tx pgx.Tx) error {
			return db.process(db.ctx, tx, newOperation(w, "BET", 2500), "CREDIT")
		}},
		{"wrong arithmetic", func(w wallet, tx pgx.Tx) error {
			op := newOperation(w, "BET", 2500)
			if err := insertOperation(db.ctx, tx, op); err != nil {
				return err
			}
			return insertLedger(db.ctx, tx, w.id, op.id, "DEBIT", 2500, 10000, 7501, 2)
		}},
		{"broken balance chain", func(w wallet, tx pgx.Tx) error {
			op := newOperation(w, "BET", 2500)
			if err := insertOperation(db.ctx, tx, op); err != nil {
				return err
			}
			return insertLedger(db.ctx, tx, w.id, op.id, "DEBIT", 2500, 9000, 6500, 2)
		}},
		{"reused version", func(w wallet, tx pgx.Tx) error {
			op := newOperation(w, "BET", 2500)
			if err := insertOperation(db.ctx, tx, op); err != nil {
				return err
			}
			return insertLedger(db.ctx, tx, w.id, op.id, "DEBIT", 2500, 10000, 7500, 1)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := db.openWallet(t, 10000)
			db.expectFailure(t, "23514", func(tx pgx.Tx) error { return tt.run(w, tx) })
			db.assertBalance(t, w, 10000, 1)
		})
	}
}

func TestLossRejectionAndTerminalResult(t *testing.T) {
	db := newDatabase(t, true)
	w := db.openWallet(t, 10000)
	loss := newOperation(w, "LOSS", 0)
	db.processAndCommit(t, loss, "")
	rejected := newOperation(w, "BET", 20000)
	balance, code := int64(10000), "INSUFFICIENT_FUNDS"
	rejected.status, rejected.balance, rejected.failure = "REJECTED", &balance, &code
	if err := insertOperation(db.ctx, db.conn, rejected); err != nil {
		t.Fatal(err)
	}
	db.expectFailure(t, "23514", func(tx pgx.Tx) error {
		if _, err := tx.Exec(db.ctx, "UPDATE wallets SET balance_minor=30000, version=2 WHERE id=$1", w.id); err != nil {
			return err
		}
		return insertLedger(db.ctx, tx, w.id, rejected.id, "CREDIT", 20000, 10000, 30000, 2)
	})
	db.assertBalance(t, w, 10000, 1)
	for _, id := range []string{loss.id, rejected.id, w.opening} {
		db.expectFailure(t, "23514", func(tx pgx.Tx) error {
			_, err := tx.Exec(db.ctx, "UPDATE wager_transactions SET result_balance_minor=1 WHERE id=$1", id)
			return err
		})
		db.expectFailure(t, "23514", func(tx pgx.Tx) error {
			_, err := tx.Exec(db.ctx, "DELETE FROM wager_transactions WHERE id=$1", id)
			return err
		})
	}
	db.expectFailure(t, "23514", func(tx pgx.Tx) error {
		_, err := tx.Exec(db.ctx, "TRUNCATE wager_transactions CASCADE")
		return err
	})
	db.processAndCommit(t, newOperation(w, "WIN", 4000), "CREDIT")
	var original int64
	if err := db.conn.QueryRow(db.ctx, "SELECT result_balance_minor FROM wager_transactions WHERE id=$1", loss.id).Scan(&original); err != nil || original != 10000 {
		t.Fatalf("original result changed: balance=%d error=%v", original, err)
	}
	db.assertBalance(t, w, 14000, 2)
}

func TestMoneyDatabaseRoundTripAtInt64Limit(t *testing.T) {
	db := newDatabase(t, true)
	w := db.openWallet(t, math.MaxInt64)
	db.assertBalance(t, w, math.MaxInt64, 1)
	var units int64
	var currency domain.Currency
	if err := db.conn.QueryRow(db.ctx, "SELECT balance_minor, currency FROM wallets WHERE id=$1", w.id).Scan(&units, &currency); err != nil {
		t.Fatal(err)
	}
	money, err := domain.MoneyFromMinorUnits(units, currency)
	if err != nil {
		t.Fatal(err)
	}
	amount, err := money.Amount()
	if err != nil || amount != "92233720368547758.07" {
		t.Fatalf("amount=%s error=%v", amount, err)
	}
	db.expectFailure(t, "22003", func(tx pgx.Tx) error {
		_, err := tx.Exec(db.ctx, "UPDATE wallets SET balance_minor=balance_minor+1 WHERE id=$1", w.id)
		return err
	})
	db.processAndCommit(t, newOperation(w, "BET", 1), "DEBIT")
	db.assertBalance(t, w, math.MaxInt64-1, 2)
}

func TestReferencesAndReversalUniqueness(t *testing.T) {
	db := newDatabase(t, true)
	w := db.openWallet(t, 10000)
	bet := newOperation(w, "BET", 2500)
	db.processAndCommit(t, bet, "DEBIT")
	for _, tt := range []struct {
		name string
		edit func(*operation)
	}{
		{"wrong amount", func(op *operation) { op.amount = 2000 }},
		{"wrong provider", func(op *operation) { op.provider = "provider-b" }},
		{"wrong round", func(op *operation) { op.round = "round-2" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			op := newOperation(w, "REFUND", 2500)
			op.reference, op.resolved = &bet.external, &bet.id
			tt.edit(&op)
			db.expectFailure(t, "23514", func(tx pgx.Tx) error { return db.process(db.ctx, tx, op, "CREDIT") })
			db.assertBalance(t, w, 7500, 2)
		})
	}
	refund := newOperation(w, "REFUND", 2500)
	refund.reference, refund.resolved = &bet.external, &bet.id
	db.processAndCommit(t, refund, "CREDIT")
	for _, kind := range []string{"REFUND", "ROLLBACK"} {
		op := newOperation(w, kind, 2500)
		op.reference, op.resolved = &bet.external, &bet.id
		db.expectFailure(t, "23505", func(tx pgx.Tx) error { return db.process(db.ctx, tx, op, "CREDIT") })
	}
	rollback := newOperation(w, "ROLLBACK", 2500)
	rollback.reference, rollback.resolved = &refund.external, &refund.id
	db.processAndCommit(t, rollback, "DEBIT")
	db.assertBalance(t, w, 7500, 4)
}

func TestReferenceSchedulingConstraints(t *testing.T) {
	db := newDatabase(t, true)
	w := db.openWallet(t, 0)
	op := newOperation(w, "REFUND", 2500)
	reference := "future-bet"
	op.reference = &reference
	if err := insertOperation(db.ctx, db.conn, op); err != nil {
		t.Fatal(err)
	}
	db.expectFailure(t, "23514", func(tx pgx.Tx) error {
		_, err := tx.Exec(db.ctx, "UPDATE wager_transactions SET status='PENDING_REFERENCE' WHERE id=$1", op.id)
		return err
	})
	_, err := db.conn.Exec(db.ctx, `UPDATE wager_transactions SET status='PENDING_REFERENCE',
        next_retry_at=now()+interval '1 second', reference_expires_at=now()+interval '1 hour', updated_at=now() WHERE id=$1`, op.id)
	if err != nil {
		t.Fatal(err)
	}
	db.expectFailure(t, "23514", func(tx pgx.Tx) error {
		_, err := tx.Exec(db.ctx, "UPDATE wager_transactions SET status='PENDING', next_retry_at=NULL, reference_expires_at=NULL WHERE id=$1", op.id)
		return err
	})
}

func TestWalletLocksAreIndependent(t *testing.T) {
	db := newDatabase(t, true)
	a, b := db.openWallet(t, 0), db.openWallet(t, 0)
	first, err := db.conn.Begin(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(db.ctx)
	if _, err := first.Exec(db.ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", a.id); err != nil {
		t.Fatal(err)
	}
	other := db.connect(t)
	second, err := other.Begin(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(db.ctx)
	if _, err := second.Exec(db.ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", b.id); err != nil {
		t.Fatal(err)
	}
	contender := db.connect(t)
	ctx, cancel := context.WithTimeout(db.ctx, 100*time.Millisecond)
	defer cancel()
	_, err = contender.Exec(ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", a.id)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same wallet was not serialized: %v", err)
	}
}

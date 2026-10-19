//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"junglegaming/internal/platform/migration"
)

type database struct {
	conn   *pgx.Conn
	config *pgx.ConnConfig
	ctx    context.Context
	runner *migration.Runner
}

type executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func newDatabase(t *testing.T, migrate bool) *database {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required; run make test-db-up before integration tests")
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal("invalid TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	schema := "test_" + rand.Text()
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
		conn.Close(ctx)
	})
	if _, err := conn.Exec(ctx, "SET search_path TO "+identifier); err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["search_path"] = identifier
	runner, err := migration.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	db := &database{conn: conn, config: cfg, ctx: ctx, runner: runner}
	if migrate {
		if _, err := runner.Run(ctx, "up"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func (db *database) connect(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.ConnectConfig(db.ctx, db.config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func newID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(err)
	}
	value[6], value[8] = (value[6]&0x0f)|0x40, (value[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

type wallet struct {
	id      string
	player  string
	opening string
}

func (db *database) openWallet(t *testing.T, balance int64) wallet {
	t.Helper()
	w := wallet{id: newID(), player: newID(), opening: newID()}
	tx, err := db.conn.Begin(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(db.ctx)
	_, err = tx.Exec(db.ctx, `INSERT INTO wallets
        (id, player_id, currency, balance_minor, version, created_at, updated_at)
        VALUES ($1, $2, 'BRL', $3, 1, now(), now())`, w.id, w.player, balance)
	if err != nil {
		t.Fatal(err)
	}
	if balance > 0 {
		_, err = tx.Exec(db.ctx, `INSERT INTO wager_transactions
            (id, wallet_id, player_id, currency, amount_minor, kind, status, result_balance_minor, created_at, updated_at)
            VALUES ($1, $2, $3, 'BRL', $4, 'OPENING', 'PROCESSED', $4, now(), now())`, w.opening, w.id, w.player, balance)
		if err != nil {
			t.Fatal(err)
		}
		if err := insertLedger(db.ctx, tx, w.id, w.opening, "CREDIT", balance, 0, balance, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(db.ctx); err != nil {
		t.Fatal(err)
	}
	return w
}

type operation struct {
	id        string
	wallet    wallet
	provider  string
	external  string
	key       string
	kind      string
	status    string
	amount    int64
	balance   *int64
	failure   *string
	reference *string
	resolved  *string
	round     string
}

func newOperation(w wallet, kind string, amount int64) operation {
	id := newID()
	return operation{id: id, wallet: w, provider: "provider-a", external: id, key: "provider-a:" + id,
		kind: kind, status: "PENDING", amount: amount, round: "round-1"}
}

func insertOperation(ctx context.Context, db executor, op operation) error {
	hash := sha256.Sum256([]byte(op.external))
	_, err := db.Exec(ctx, `INSERT INTO wager_transactions
        (id, wallet_id, player_id, currency, amount_minor, kind, status,
         provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id,
         result_balance_minor, failure_code, reference_external_transaction_id, resolved_reference_id, created_at, updated_at)
        VALUES ($1, $2, $3, 'BRL', $4, $5, $6, $7, $8, $9, $10, $11, 'game-1', $12, $13, $14, $15, now(), now())`,
		op.id, op.wallet.id, op.wallet.player, op.amount, op.kind, op.status,
		op.provider, op.external, op.key, hash[:], op.round, op.balance, op.failure, op.reference, op.resolved)
	return err
}

func insertLedger(ctx context.Context, db executor, walletID, transactionID, direction string, amount, before, after, version int64) error {
	_, err := db.Exec(ctx, `INSERT INTO wallet_ledger
        (id, wallet_id, transaction_id, currency, direction, amount_minor, balance_before_minor, balance_after_minor, wallet_version, created_at)
        VALUES ($1, $2, $3, 'BRL', $4, $5, $6, $7, $8, now())`,
		newID(), walletID, transactionID, direction, amount, before, after, version)
	return err
}

func (db *database) process(ctx context.Context, tx pgx.Tx, op operation, direction string) error {
	var before, version int64
	if err := tx.QueryRow(ctx, "SELECT balance_minor, version FROM wallets WHERE id = $1 FOR UPDATE", op.wallet.id).Scan(&before, &version); err != nil {
		return err
	}
	after := before
	if direction == "CREDIT" {
		after += op.amount
	} else if direction == "DEBIT" {
		after -= op.amount
	}
	op.status, op.balance = "PROCESSED", &after
	if err := insertOperation(ctx, tx, op); err != nil {
		return err
	}
	if direction == "" {
		return nil
	}
	version++
	if _, err := tx.Exec(ctx, "UPDATE wallets SET balance_minor=$2, version=$3, updated_at=now() WHERE id=$1", op.wallet.id, after, version); err != nil {
		return err
	}
	return insertLedger(ctx, tx, op.wallet.id, op.id, direction, op.amount, before, after, version)
}

func (db *database) processAndCommit(t *testing.T, op operation, direction string) {
	t.Helper()
	tx, err := db.conn.Begin(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(db.ctx)
	if err := db.process(db.ctx, tx, op, direction); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(db.ctx); err != nil {
		t.Fatal(err)
	}
}

func (db *database) expectFailure(t *testing.T, code string, apply func(pgx.Tx) error) {
	t.Helper()
	tx, err := db.conn.Begin(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(db.ctx)
	err = apply(tx)
	if err == nil {
		err = tx.Commit(db.ctx)
	}
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != code {
		t.Fatalf("got %v, want SQLSTATE %s", err, code)
	}
}

func (db *database) assertBalance(t *testing.T, w wallet, balance, version int64) {
	t.Helper()
	var stored, calculated, actualVersion int64
	err := db.conn.QueryRow(db.ctx, `SELECT balance_minor, version,
        COALESCE((SELECT SUM(CASE direction WHEN 'CREDIT' THEN amount_minor::NUMERIC ELSE -amount_minor::NUMERIC END)
            FROM wallet_ledger WHERE wallet_id = $1), 0)::BIGINT FROM wallets WHERE id = $1`, w.id).Scan(&stored, &actualVersion, &calculated)
	if err != nil || stored != balance || calculated != balance || actualVersion != version {
		t.Fatalf("stored=%d ledger=%d version=%d error=%v; want balance=%d version=%d", stored, calculated, actualVersion, err, balance, version)
	}
}

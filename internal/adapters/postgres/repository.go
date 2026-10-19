package postgres

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"junglegaming/internal/application"
	"junglegaming/internal/domain"
	"junglegaming/internal/identifier"
	"junglegaming/internal/platform/telemetry"
)

type queryer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type Repository struct {
	db      queryer
	metrics *telemetry.Metrics
}

func NewRepository(pool *pgxpool.Pool, metrics *telemetry.Metrics) *Repository {
	return &Repository{db: pool, metrics: metrics}
}

type Manager struct {
	pool    *pgxpool.Pool
	metrics *telemetry.Metrics
}

func NewManager(pool *pgxpool.Pool, metrics *telemetry.Metrics) *Manager {
	return &Manager{pool, metrics}
}

func (m *Manager) Within(ctx context.Context, action func(application.Store) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		started := time.Now()
		tx, err := m.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		m.metrics.TransactionStart.Observe(time.Since(started).Seconds())
		if err != nil {
			return databaseError(err)
		}
		err = action(&Repository{db: tx, metrics: m.metrics})
		if err == nil {
			err = tx.Commit(ctx)
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = tx.Rollback(cleanup)
		cancel()
		if err == nil {
			return nil
		}
		var sqlError *pgconn.PgError
		if errors.As(err, &sqlError) && (sqlError.Code == "40P01" || sqlError.Code == "40001") {
			m.metrics.Concurrency.Inc()
		}
		if !errors.As(err, &sqlError) || (sqlError.Code != "40P01" && sqlError.Code != "40001") || attempt == 2 {
			return databaseError(err)
		}
		m.metrics.Retry("database")
		timer := time.NewTimer(time.Duration(20*(attempt+1)+rand.IntN(20)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return databaseError(ctx.Err())
		case <-timer.C:
		}
	}
	return application.ErrUnavailable
}

func databaseError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return application.ErrNotFound
	}
	for _, known := range []error{application.ErrNotFound, application.ErrConflict, application.ErrWalletMismatch, application.ErrLeaseLost} {
		if errors.Is(err, known) {
			return err
		}
	}
	var sqlError *pgconn.PgError
	if errors.As(err, &sqlError) && sqlError.Code == "23505" {
		return fmt.Errorf("%w: %w", application.ErrConflict, err)
	}
	if errors.As(err, &sqlError) {
		switch sqlError.Code {
		case "23514", "23503", "22003", "22P02", "42P01", "42703", "42883":
			return fmt.Errorf("%w: %w", application.ErrPermanentInfrastructure, err)
		}
	}
	return fmt.Errorf("%w: %w", application.ErrUnavailable, err)
}

func scanWallet(row pgx.Row, at *time.Time) (*domain.Wallet, error) {
	var state domain.WalletSnapshot
	var units int64
	var currency domain.Currency
	args := []any{&state.ID, &state.PlayerID, &currency, &units, &state.Version, &state.CreatedAt, &state.UpdatedAt}
	if at != nil {
		args = append(args, at)
	}
	if err := row.Scan(args...); err != nil {
		return nil, databaseError(err)
	}
	var err error
	state.Balance, err = domain.MoneyFromMinorUnits(units, currency)
	if err != nil {
		return nil, databaseError(err)
	}
	wallet, err := domain.RestoreWallet(state)
	return wallet, databaseError(err)
}

const walletColumns = "id::text, player_id::text, currency, balance_minor, version, created_at, updated_at"

func (r *Repository) Wallet(ctx context.Context, id string) (*domain.Wallet, error) {
	return scanWallet(r.db.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallets WHERE id=$1", id), nil)
}

func (r *Repository) WalletForUpdate(ctx context.Context, id string) (*domain.Wallet, time.Time, error) {
	var at time.Time
	started := time.Now()
	wallet, err := scanWallet(r.db.QueryRow(ctx, "SELECT "+walletColumns+" FROM wallets WHERE id=$1 FOR UPDATE", id), nil)
	r.metrics.LockWait.Observe(time.Since(started).Seconds())
	if err != nil {
		return nil, at, err
	}
	err = r.db.QueryRow(ctx, "SELECT GREATEST(clock_timestamp(),$1::timestamptz)", wallet.Snapshot().UpdatedAt).Scan(&at)
	return wallet, at, err
}

func (r *Repository) CreateWallet(ctx context.Context, wallet *domain.Wallet) error {
	s := wallet.Snapshot()
	_, err := r.db.Exec(ctx, `INSERT INTO wallets (id,player_id,currency,balance_minor,version,created_at,updated_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7)`, s.ID, s.PlayerID, s.Balance.Currency(), s.Balance.MinorUnits(), s.Version, s.CreatedAt, s.UpdatedAt)
	return databaseError(err)
}

func (r *Repository) SaveWallet(ctx context.Context, wallet *domain.Wallet) error {
	s := wallet.Snapshot()
	tag, err := r.db.Exec(ctx, "UPDATE wallets SET balance_minor=$2,version=$3,updated_at=$4 WHERE id=$1 AND version=$5", s.ID, s.Balance.MinorUnits(), s.Version, s.UpdatedAt, s.Version-1)
	if err != nil {
		return databaseError(err)
	}
	if tag.RowsAffected() != 1 {
		r.metrics.Concurrency.Inc()
		return application.ErrConflict
	}
	return nil
}

const transactionColumns = `id::text,wallet_id::text,player_id::text,currency,amount_minor,kind,status,
    COALESCE(provider_id,''),COALESCE(external_transaction_id,''),COALESCE(idempotency_key,''),
    COALESCE(encode(payload_hash,'hex'),''),COALESCE(round_id,''),COALESCE(game_id,''),
    COALESCE(reference_external_transaction_id,''),COALESCE(resolved_reference_id::text,''),
    COALESCE(failure_code,''),result_balance_minor,created_at,updated_at`

type scanner interface{ Scan(...any) error }

func scanTransaction(row scanner) (*domain.WagerTransaction, error) {
	var s domain.TransactionSnapshot
	i := &s.Input
	var currency domain.Currency
	var units int64
	var result *int64
	err := row.Scan(&i.ID, &i.WalletID, &i.PlayerID, &currency, &units, &i.Kind, &s.Status,
		&i.ProviderID, &i.ExternalTransactionID, &i.IdempotencyKey, &i.PayloadHash, &i.RoundID, &i.GameID,
		&i.ReferenceExternalTransactionID, &s.ResolvedReferenceID, &s.FailureCode, &result, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, databaseError(err)
	}
	i.Money, err = domain.MoneyFromMinorUnits(units, currency)
	if err != nil {
		return nil, databaseError(err)
	}
	if result != nil {
		balance, err := domain.MoneyFromMinorUnits(*result, currency)
		if err != nil {
			return nil, databaseError(err)
		}
		s.ResultBalance = &balance
	}
	transaction, err := domain.RestoreWagerTransaction(s)
	return transaction, databaseError(err)
}

func (r *Repository) Identity(ctx context.Context, provider, key, external string) (*domain.WagerTransaction, error) {
	rows, err := r.db.Query(ctx, "SELECT "+transactionColumns+" FROM wager_transactions WHERE provider_id=$1 AND (idempotency_key=$2 OR external_transaction_id=$3)", provider, key, external)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	var transaction *domain.WagerTransaction
	for rows.Next() {
		if transaction != nil {
			return nil, application.ErrConflict
		}
		transaction, err = scanTransaction(rows)
		if err != nil {
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
	}
	if transaction == nil {
		return nil, application.ErrNotFound
	}
	return transaction, nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (r *Repository) ReserveTransaction(ctx context.Context, transaction *domain.WagerTransaction, trace application.Trace) (bool, error) {
	s := transaction.Snapshot()
	i := s.Input
	var hash []byte
	if i.PayloadHash != "" {
		hash, _ = hex.DecodeString(i.PayloadHash)
	}
	var balance any
	if s.ResultBalance != nil {
		balance = s.ResultBalance.MinorUnits()
	}
	var id string
	err := r.db.QueryRow(ctx, `INSERT INTO wager_transactions
        (id,wallet_id,player_id,currency,amount_minor,kind,status,provider_id,external_transaction_id,idempotency_key,
         payload_hash,round_id,game_id,reference_external_transaction_id,result_balance_minor,created_at,updated_at,correlation_id,causation_id)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
        ON CONFLICT DO NOTHING RETURNING id::text`,
		i.ID, i.WalletID, i.PlayerID, i.Money.Currency(), i.Money.MinorUnits(), i.Kind, s.Status,
		nullable(i.ProviderID), nullable(i.ExternalTransactionID), nullable(i.IdempotencyKey), hash, nullable(i.RoundID), nullable(i.GameID),
		nullable(i.ReferenceExternalTransactionID), balance, s.CreatedAt, s.UpdatedAt, nullable(trace.CorrelationID), nullable(trace.CausationID)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, databaseError(err)
}

func (r *Repository) SaveTransaction(ctx context.Context, transaction *domain.WagerTransaction, schedule *application.ReferenceSchedule) error {
	s := transaction.Snapshot()
	var balance, next, expires any
	var attempts any
	if s.ResultBalance != nil {
		balance = s.ResultBalance.MinorUnits()
	}
	if schedule != nil {
		next, expires, attempts = schedule.Next, schedule.Expires, schedule.Attempts
	}
	_, err := r.db.Exec(ctx, `UPDATE wager_transactions SET status=$2,resolved_reference_id=$3,failure_code=$4,
        result_balance_minor=$5,updated_at=$6,next_retry_at=$7,reference_expires_at=$8,reference_attempts=COALESCE($9,reference_attempts),
        lease_id=NULL,lease_until=NULL WHERE id=$1`, s.Input.ID, s.Status, nullable(s.ResolvedReferenceID), nullable(string(s.FailureCode)), balance, s.UpdatedAt, next, expires, attempts)
	return databaseError(err)
}

func (r *Repository) Transaction(ctx context.Context, id string) (*domain.WagerTransaction, error) {
	return scanTransaction(r.db.QueryRow(ctx, "SELECT "+transactionColumns+" FROM wager_transactions WHERE id=$1", id))
}
func (r *Repository) ExternalTransaction(ctx context.Context, provider, external string) (*domain.WagerTransaction, error) {
	return scanTransaction(r.db.QueryRow(ctx, "SELECT "+transactionColumns+" FROM wager_transactions WHERE provider_id=$1 AND external_transaction_id=$2", provider, external))
}
func (r *Repository) Reversed(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM wager_transactions WHERE resolved_reference_id=$1 AND status='PROCESSED' AND kind IN ('REFUND','ROLLBACK'))", id).Scan(&exists)
	return exists, databaseError(err)
}
func (r *Repository) AddLedger(ctx context.Context, entry domain.WalletLedgerEntry) error {
	s := entry.Snapshot()
	_, err := r.db.Exec(ctx, `INSERT INTO wallet_ledger (id,wallet_id,transaction_id,currency,direction,amount_minor,balance_before_minor,balance_after_minor,wallet_version,created_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, s.ID, s.WalletID, s.TransactionID, s.Money.Currency(), s.Direction, s.Money.MinorUnits(), s.BalanceBefore.MinorUnits(), s.BalanceAfter.MinorUnits(), s.WalletVersion, s.CreatedAt)
	return databaseError(err)
}
func (r *Repository) AddEvents(ctx context.Context, events []domain.Event) error {
	for _, event := range events {
		_, err := r.db.Exec(ctx, `INSERT INTO outbox (event_id,wallet_id,aggregate_id,event_type,payload,occurred_at) VALUES ($1,$2,$3,$4,$5::jsonb,$6)`, event.ID(), event.WalletID(), event.AggregateID(), event.Type(), string(event.Payload()), event.OccurredAt())
		if err != nil {
			return databaseError(err)
		}
	}
	return nil
}
func (r *Repository) ReserveInbox(ctx context.Context, message application.IncomingMessage) error {
	_, err := r.db.Exec(ctx, `INSERT INTO inbox (consumer_name,message_id,payload_hash,received_at) VALUES ($1,$2,$3,clock_timestamp()) ON CONFLICT DO NOTHING`, message.Consumer, message.ID, message.Hash)
	if err != nil {
		return databaseError(err)
	}
	var same bool
	err = r.db.QueryRow(ctx, "SELECT payload_hash=$3 FROM inbox WHERE consumer_name=$1 AND message_id=$2", message.Consumer, message.ID, message.Hash).Scan(&same)
	if err != nil {
		return databaseError(err)
	}
	if !same {
		return application.ErrConflict
	}
	return nil
}
func (r *Repository) CompleteInbox(ctx context.Context, message application.IncomingMessage, transactionID string) error {
	_, err := r.db.Exec(ctx, `UPDATE inbox SET transaction_id=$3,completed_at=clock_timestamp() WHERE consumer_name=$1 AND message_id=$2 AND completed_at IS NULL`, message.Consumer, message.ID, transactionID)
	return databaseError(err)
}
func (r *Repository) ReferenceSchedule(ctx context.Context, id string) (application.ReferenceSchedule, error) {
	var schedule application.ReferenceSchedule
	err := r.db.QueryRow(ctx, "SELECT reference_attempts,next_retry_at,reference_expires_at,COALESCE(lease_id::text,''),lease_until FROM wager_transactions WHERE id=$1 FOR UPDATE", id).Scan(&schedule.Attempts, &schedule.Next, &schedule.Expires, &schedule.LeaseID, &schedule.LeaseUntil)
	return schedule, databaseError(err)
}
func (r *Repository) TransactionTrace(ctx context.Context, id string) (application.Trace, error) {
	var trace application.Trace
	err := r.db.QueryRow(ctx, "SELECT COALESCE(correlation_id::text,''),COALESCE(causation_id,'') FROM wager_transactions WHERE id=$1", id).Scan(&trace.CorrelationID, &trace.CausationID)
	if err != nil {
		return trace, databaseError(err)
	}
	if trace.CorrelationID == "" {
		trace.CorrelationID, err = identifier.New()
	}
	return trace, err
}

type cursor struct {
	At time.Time `json:"at"`
	ID string    `json:"id"`
}

func (r *Repository) Ledger(ctx context.Context, walletID, encoded string, limit int) (application.LedgerPage, error) {
	page := application.LedgerPage{Entries: []domain.LedgerSnapshot{}}
	if limit < 1 || limit > 100 {
		return page, application.ErrInvalidCursor
	}
	if _, err := r.Wallet(ctx, walletID); err != nil {
		return page, err
	}
	var at, id any
	if encoded != "" {
		if len(encoded) > 512 {
			return page, application.ErrInvalidCursor
		}
		data, err := base64.RawURLEncoding.DecodeString(encoded)
		var position cursor
		if err != nil || json.Unmarshal(data, &position) != nil || position.At.IsZero() {
			return page, application.ErrInvalidCursor
		}
		var parsed pgtype.UUID
		if parsed.Scan(position.ID) != nil || !parsed.Valid {
			return page, application.ErrInvalidCursor
		}
		at, id = position.At, position.ID
	}
	rows, err := r.db.Query(ctx, `SELECT id::text,transaction_id::text,direction,amount_minor,balance_before_minor,balance_after_minor,wallet_version,created_at,currency
        FROM wallet_ledger WHERE wallet_id=$1 AND ($2::timestamptz IS NULL OR (created_at,id)>($2,$3::uuid)) ORDER BY created_at,id LIMIT $4`, walletID, at, id, limit+1)
	if err != nil {
		return page, databaseError(err)
	}
	defer rows.Close()
	for rows.Next() {
		entry := domain.LedgerSnapshot{WalletID: walletID}
		var amount, before, after int64
		var currency domain.Currency
		if err := rows.Scan(&entry.ID, &entry.TransactionID, &entry.Direction, &amount, &before, &after, &entry.WalletVersion, &entry.CreatedAt, &currency); err != nil {
			return page, databaseError(err)
		}
		entry.Money, _ = domain.MoneyFromMinorUnits(amount, currency)
		entry.BalanceBefore, _ = domain.MoneyFromMinorUnits(before, currency)
		entry.BalanceAfter, _ = domain.MoneyFromMinorUnits(after, currency)
		validated, err := domain.RestoreWalletLedgerEntry(entry)
		if err != nil {
			return page, databaseError(err)
		}
		page.Entries = append(page.Entries, validated.Snapshot())
	}
	if err := rows.Err(); err != nil {
		return page, databaseError(err)
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		last := page.Entries[limit-1]
		payload, _ := json.Marshal(cursor{last.CreatedAt, last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(payload)
	}
	return page, nil
}

func (r *Repository) Reconcile(ctx context.Context, id string) (application.Reconciliation, error) {
	result := application.Reconciliation{WalletID: id}
	var stored, calculated int64
	var currency domain.Currency
	err := r.db.QueryRow(ctx, `SELECT w.balance_minor,w.currency,
        COALESCE((SELECT SUM(CASE direction WHEN 'CREDIT' THEN amount_minor::numeric ELSE -amount_minor::numeric END) FROM wallet_ledger WHERE wallet_id=w.id),0)::bigint,
        (SELECT count(*) FROM wallet_ledger WHERE wallet_id=w.id) FROM wallets w WHERE w.id=$1`, id).Scan(&stored, &currency, &calculated, &result.CheckedEntries)
	if err != nil {
		return result, databaseError(err)
	}
	result.StoredBalance, _ = domain.MoneyFromMinorUnits(stored, currency)
	result.CalculatedBalance, _ = domain.MoneyFromMinorUnits(calculated, currency)
	result.Difference, err = result.StoredBalance.Subtract(result.CalculatedBalance)
	result.Consistent = stored == calculated
	return result, databaseError(err)
}

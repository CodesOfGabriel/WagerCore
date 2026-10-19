package domain_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"junglegaming/internal/domain"
)

const (
	walletID = "0192f291-27dd-7d3f-8071-5f8685deef37"
	playerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	txID     = "0192f298-345e-7e38-af88-e43f851a819d"
	refID    = "0192f298-345e-7e38-af88-e43f851a819e"
	entryID  = "0192f298-345e-7e38-af88-e43f851a819f"
)

var createdAt = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func TestWalletMovementsAndRestoration(t *testing.T) {
	wallet, err := domain.NewWallet(walletID, playerID, fromUnits(t, 10000, domain.BRL), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if wallet.Snapshot().Version != 1 {
		t.Fatal("initial version is not one")
	}
	if err := wallet.Debit(fromUnits(t, 2500, domain.BRL), createdAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := wallet.Credit(fromUnits(t, 4000, domain.BRL), createdAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	state := wallet.Snapshot()
	if state.Balance.MinorUnits() != 11500 || state.Version != 3 {
		t.Fatalf("unexpected wallet: %+v", state)
	}
	restored, err := domain.RestoreWallet(state)
	if err != nil || restored.Snapshot() != state {
		t.Fatalf("restore changed the wallet: %v", err)
	}
	state.Version = 100
	if wallet.Snapshot().Version != 3 {
		t.Fatal("snapshot changed the wallet")
	}
}

func TestWalletRejectsInvalidState(t *testing.T) {
	valid := domain.WalletSnapshot{
		ID: walletID, PlayerID: playerID, Balance: fromUnits(t, 100, domain.BRL),
		Version: 1, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	for _, tt := range []struct {
		name string
		edit func(*domain.WalletSnapshot)
		want error
	}{
		{"empty id", func(s *domain.WalletSnapshot) { s.ID = "" }, domain.ErrInvalidID},
		{"nil uuid", func(s *domain.WalletSnapshot) { s.ID = "00000000-0000-0000-0000-000000000000" }, domain.ErrInvalidID},
		{"invalid player", func(s *domain.WalletSnapshot) { s.PlayerID = "player-1" }, domain.ErrInvalidID},
		{"negative balance", func(s *domain.WalletSnapshot) { s.Balance = fromUnits(t, -1, domain.BRL) }, domain.ErrInvalidAmount},
		{"invalid money", func(s *domain.WalletSnapshot) { s.Balance = domain.Money{} }, domain.ErrInvalidCurrency},
		{"zero version", func(s *domain.WalletSnapshot) { s.Version = 0 }, domain.ErrInvalidVersion},
		{"zero time", func(s *domain.WalletSnapshot) { s.CreatedAt = time.Time{} }, domain.ErrInvalidTimestamp},
		{"time regression", func(s *domain.WalletSnapshot) { s.UpdatedAt = createdAt.Add(-time.Second) }, domain.ErrInvalidTimestamp},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := valid
			tt.edit(&state)
			if _, err := domain.RestoreWallet(state); !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
		})
	}
	var uninitialized domain.Wallet
	if err := uninitialized.Debit(fromUnits(t, 1, domain.BRL), createdAt); !errors.Is(err, domain.ErrInvalidID) {
		t.Fatalf("uninitialized wallet error: %v", err)
	}
	var missing *domain.Wallet
	if err := missing.Validate(); !errors.Is(err, domain.ErrInvalidWallet) {
		t.Fatalf("nil wallet error: %v", err)
	}
}

func TestWalletFailedMovementPreservesState(t *testing.T) {
	for _, tt := range []struct {
		name    string
		balance int64
		version int64
		money   domain.Money
		at      time.Time
		credit  bool
		want    error
	}{
		{"insufficient funds", 100, 1, fromUnits(t, 101, domain.BRL), createdAt, false, domain.ErrInsufficientFunds},
		{"currency mismatch", 100, 1, fromUnits(t, 1, domain.USD), createdAt, false, domain.ErrCurrencyMismatch},
		{"zero movement", 100, 1, fromUnits(t, 0, domain.BRL), createdAt, false, domain.ErrInvalidAmount},
		{"negative movement", 100, 1, fromUnits(t, -1, domain.BRL), createdAt, true, domain.ErrInvalidAmount},
		{"amount overflow", math.MaxInt64, 1, fromUnits(t, 1, domain.BRL), createdAt, true, domain.ErrMoneyOverflow},
		{"version overflow", 100, math.MaxInt64, fromUnits(t, 1, domain.BRL), createdAt, false, domain.ErrVersionOverflow},
		{"time regression", 100, 1, fromUnits(t, 1, domain.BRL), createdAt.Add(-time.Second), true, domain.ErrInvalidTimestamp},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wallet, err := domain.RestoreWallet(domain.WalletSnapshot{
				ID: walletID, PlayerID: playerID, Balance: fromUnits(t, tt.balance, domain.BRL),
				Version: tt.version, CreatedAt: createdAt, UpdatedAt: createdAt,
			})
			if err != nil {
				t.Fatal(err)
			}
			before := wallet.Snapshot()
			if tt.credit {
				err = wallet.Credit(tt.money, tt.at)
			} else {
				err = wallet.Debit(tt.money, tt.at)
			}
			if !errors.Is(err, tt.want) || wallet.Snapshot() != before {
				t.Fatalf("error=%v, state changed=%t", err, wallet.Snapshot() != before)
			}
		})
	}
}

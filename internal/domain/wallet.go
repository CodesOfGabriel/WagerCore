package domain

import (
	"math"
	"time"
)

type WalletSnapshot struct {
	ID        string
	PlayerID  string
	Balance   Money
	Version   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Wallet struct {
	state WalletSnapshot
}

func NewWallet(id, playerID string, initialBalance Money, at time.Time) (*Wallet, error) {
	return RestoreWallet(WalletSnapshot{
		ID: id, PlayerID: playerID, Balance: initialBalance,
		Version: 1, CreatedAt: at, UpdatedAt: at,
	})
}

func RestoreWallet(state WalletSnapshot) (*Wallet, error) {
	wallet := &Wallet{state: state}
	if err := wallet.Validate(); err != nil {
		return nil, err
	}
	wallet.state.CreatedAt = state.CreatedAt.UTC()
	wallet.state.UpdatedAt = state.UpdatedAt.UTC()
	return wallet, nil
}

func (w *Wallet) Validate() error {
	if w == nil {
		return ErrInvalidWallet
	}
	if err := ValidateID(w.state.ID); err != nil {
		return err
	}
	if err := ValidateID(w.state.PlayerID); err != nil {
		return err
	}
	if err := validateNonNegative(w.state.Balance); err != nil {
		return err
	}
	if w.state.Version < 1 {
		return ErrInvalidVersion
	}
	if !validTimes(w.state.CreatedAt, w.state.UpdatedAt) {
		return ErrInvalidTimestamp
	}
	return nil
}

func (w *Wallet) Snapshot() WalletSnapshot {
	return w.state
}

func (w *Wallet) Debit(money Money, at time.Time) error {
	return w.move(money, at, Debit)
}

func (w *Wallet) Credit(money Money, at time.Time) error {
	return w.move(money, at, Credit)
}

func (w *Wallet) move(money Money, at time.Time, direction Direction) error {
	if err := w.Validate(); err != nil {
		return err
	}
	if err := validatePositive(money); err != nil {
		return err
	}
	if at.IsZero() || at.Before(w.state.UpdatedAt) {
		return ErrInvalidTimestamp
	}
	if w.state.Version == math.MaxInt64 {
		return ErrVersionOverflow
	}
	var balance Money
	var err error
	if direction == Debit {
		balance, err = w.state.Balance.Subtract(money)
	} else {
		balance, err = w.state.Balance.Add(money)
	}
	if err != nil {
		return err
	}
	if balance.MinorUnits() < 0 {
		return ErrInsufficientFunds
	}
	w.state.Balance = balance
	w.state.Version++
	w.state.UpdatedAt = at.UTC()
	return nil
}

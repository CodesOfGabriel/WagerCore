package domain

import "time"

type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

type LedgerSnapshot struct {
	ID            string
	WalletID      string
	TransactionID string
	Direction     Direction
	Money         Money
	BalanceBefore Money
	BalanceAfter  Money
	WalletVersion int64
	CreatedAt     time.Time
}

type WalletLedgerEntry struct {
	state LedgerSnapshot
}

func NewWalletLedgerEntry(state LedgerSnapshot) (WalletLedgerEntry, error) {
	entry := WalletLedgerEntry{state: state}
	if err := entry.Validate(); err != nil {
		return WalletLedgerEntry{}, err
	}
	entry.state.CreatedAt = state.CreatedAt.UTC()
	return entry, nil
}

func RestoreWalletLedgerEntry(state LedgerSnapshot) (WalletLedgerEntry, error) {
	return NewWalletLedgerEntry(state)
}

func (e WalletLedgerEntry) Snapshot() LedgerSnapshot {
	return e.state
}

func (e WalletLedgerEntry) Validate() error {
	for _, id := range []string{e.state.ID, e.state.WalletID, e.state.TransactionID} {
		if err := ValidateID(id); err != nil {
			return err
		}
	}
	if err := validatePositive(e.state.Money); err != nil {
		return err
	}
	if err := validateNonNegative(e.state.BalanceBefore); err != nil {
		return err
	}
	if err := validateNonNegative(e.state.BalanceAfter); err != nil {
		return err
	}
	if e.state.WalletVersion < 1 {
		return ErrInvalidVersion
	}
	if e.state.CreatedAt.IsZero() {
		return ErrInvalidTimestamp
	}
	var expected Money
	var err error
	switch e.state.Direction {
	case Debit:
		expected, err = e.state.BalanceBefore.Subtract(e.state.Money)
	case Credit:
		expected, err = e.state.BalanceBefore.Add(e.state.Money)
	default:
		return ErrInvalidLedgerEntry
	}
	if err != nil {
		return err
	}
	comparison, err := expected.Compare(e.state.BalanceAfter)
	if err != nil {
		return err
	}
	if comparison != 0 {
		return ErrInvalidLedgerEntry
	}
	return nil
}

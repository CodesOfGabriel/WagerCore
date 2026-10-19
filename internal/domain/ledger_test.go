package domain_test

import (
	"testing"

	"junglegaming/internal/domain"
)

func ledgerState(t *testing.T) domain.LedgerSnapshot {
	return domain.LedgerSnapshot{
		ID: entryID, WalletID: walletID, TransactionID: txID, Direction: domain.Debit,
		Money: fromUnits(t, 2500, domain.BRL), BalanceBefore: fromUnits(t, 10000, domain.BRL),
		BalanceAfter: fromUnits(t, 7500, domain.BRL), WalletVersion: 2, CreatedAt: createdAt,
	}
}

func TestLedgerEntryAndRestoration(t *testing.T) {
	for _, direction := range []domain.Direction{domain.Debit, domain.Credit} {
		state := ledgerState(t)
		state.Direction = direction
		if direction == domain.Credit {
			state.BalanceAfter = fromUnits(t, 12500, domain.BRL)
		}
		entry, err := domain.NewWalletLedgerEntry(state)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := domain.RestoreWalletLedgerEntry(entry.Snapshot())
		if err != nil || restored.Snapshot() != state {
			t.Fatalf("ledger changed on restore: %v", err)
		}
		copy := entry.Snapshot()
		copy.Direction = "INVALID"
		if entry.Snapshot() != state {
			t.Fatal("snapshot changed the ledger")
		}
	}
}

func TestLedgerRejectsInvalidEntries(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*domain.LedgerSnapshot)
	}{
		{"wrong arithmetic", func(s *domain.LedgerSnapshot) { s.BalanceAfter = fromUnits(t, 7501, domain.BRL) }},
		{"unknown direction", func(s *domain.LedgerSnapshot) { s.Direction = "OTHER" }},
		{"zero movement", func(s *domain.LedgerSnapshot) { s.Money = fromUnits(t, 0, domain.BRL) }},
		{"wrong currency", func(s *domain.LedgerSnapshot) { s.Money = fromUnits(t, 2500, domain.USD) }},
		{"negative balance", func(s *domain.LedgerSnapshot) { s.BalanceAfter = fromUnits(t, -1, domain.BRL) }},
		{"invalid id", func(s *domain.LedgerSnapshot) { s.TransactionID = "" }},
		{"invalid version", func(s *domain.LedgerSnapshot) { s.WalletVersion = 0 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			state := ledgerState(t)
			tt.edit(&state)
			if _, err := domain.NewWalletLedgerEntry(state); err == nil {
				t.Fatal("invalid ledger accepted")
			}
		})
	}
	if err := (domain.WalletLedgerEntry{}).Validate(); err == nil {
		t.Fatal("uninitialized ledger accepted")
	}
}

package domain_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"junglegaming/internal/domain"
)

func transactionInput(t *testing.T, kind domain.TransactionKind) domain.TransactionInput {
	input := domain.TransactionInput{
		ID: txID, WalletID: walletID, PlayerID: playerID, ProviderID: "provider-a",
		ExternalTransactionID: "tx-1", IdempotencyKey: "provider-a:tx-1", PayloadHash: strings.Repeat("ab", 32),
		RoundID: "round-1", GameID: "game-1", Kind: kind, Money: fromUnits(t, 2500, domain.BRL),
	}
	if kind == domain.Loss {
		input.Money = fromUnits(t, 0, domain.BRL)
	}
	if kind == domain.Refund || kind == domain.Rollback {
		input.ReferenceExternalTransactionID = "original-1"
	}
	return input
}

func TestTransactionCreationAndZeroPolicy(t *testing.T) {
	for _, kind := range []domain.TransactionKind{domain.Bet, domain.Win, domain.Loss, domain.Refund, domain.Rollback} {
		t.Run(string(kind), func(t *testing.T) {
			input := transactionInput(t, kind)
			transaction, err := domain.NewWagerTransaction(input, createdAt)
			if err != nil || transaction.Snapshot().Status != domain.Pending {
				t.Fatalf("creation error=%v", err)
			}
			if kind == domain.Loss {
				input.Money = fromUnits(t, 1, domain.BRL)
			} else {
				input.Money = fromUnits(t, 0, domain.BRL)
			}
			if _, err := domain.NewWagerTransaction(input, createdAt); !errors.Is(err, domain.ErrInvalidAmount) {
				t.Fatalf("zero policy error=%v", err)
			}
		})
	}
	if _, err := domain.NewWagerTransaction(transactionInput(t, domain.Opening), createdAt); !errors.Is(err, domain.ErrInvalidTransaction) {
		t.Fatalf("external OPENING error=%v", err)
	}
}

func TestOpeningTransaction(t *testing.T) {
	wallet, err := domain.NewWallet(walletID, playerID, fromUnits(t, 10000, domain.BRL), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := domain.NewOpeningTransaction(txID, wallet)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Process(fromUnits(t, 1, domain.BRL), "", createdAt); !errors.Is(err, domain.ErrInvalidTransaction) {
		t.Fatalf("opening with another balance error=%v", err)
	}
	if err := transaction.Process(wallet.Snapshot().Balance, "", createdAt); err != nil {
		t.Fatal(err)
	}
	state := transaction.Snapshot()
	if state.Input.Kind != domain.Opening || state.Input.ProviderID != "" || state.Input.PayloadHash != "" || state.Input.ExternalTransactionID != "" || state.Input.IdempotencyKey != "" || state.Input.RoundID != "" || state.Input.GameID != "" || wallet.Snapshot().Version != 1 {
		t.Fatalf("invalid opening: %+v", state)
	}
	zero, _ := domain.NewWallet(walletID, playerID, fromUnits(t, 0, domain.BRL), createdAt)
	if _, err := domain.NewOpeningTransaction(txID, zero); !errors.Is(err, domain.ErrInvalidAmount) {
		t.Fatalf("zero opening error=%v", err)
	}
	if err := wallet.Credit(fromUnits(t, 1, domain.BRL), createdAt); err != nil {
		t.Fatal(err)
	}
	if _, err := domain.NewOpeningTransaction(txID, wallet); !errors.Is(err, domain.ErrInvalidWallet) {
		t.Fatalf("opening after movement error=%v", err)
	}
}

func TestTransactionTerminalStates(t *testing.T) {
	for _, status := range []domain.TransactionStatus{domain.Processed, domain.Rejected, domain.Failed} {
		t.Run(string(status), func(t *testing.T) {
			transaction, err := domain.NewWagerTransaction(transactionInput(t, domain.Bet), createdAt)
			if err != nil {
				t.Fatal(err)
			}
			balance := fromUnits(t, 7500, domain.BRL)
			switch status {
			case domain.Processed:
				err = transaction.Process(balance, "", createdAt)
			case domain.Rejected:
				err = transaction.Reject(domain.InsufficientFunds, balance, createdAt)
			case domain.Failed:
				err = transaction.Fail(createdAt)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := transaction.Snapshot()
			attempts := []error{
				transaction.Process(balance, "", createdAt), transaction.Reject(domain.InsufficientFunds, balance, createdAt),
				transaction.AwaitReference(createdAt), transaction.Fail(createdAt),
			}
			for _, err := range attempts {
				if !errors.Is(err, domain.ErrTerminalTransaction) {
					t.Fatalf("terminal transition error=%v", err)
				}
			}
			if !reflect.DeepEqual(transaction.Snapshot(), before) {
				t.Fatal("terminal transaction changed")
			}
			restored, err := domain.RestoreWagerTransaction(before)
			if err != nil || !reflect.DeepEqual(restored.Snapshot(), before) {
				t.Fatalf("restore changed transaction: %v", err)
			}
		})
	}
}

func TestTransactionPendingReference(t *testing.T) {
	transaction, err := domain.NewWagerTransaction(transactionInput(t, domain.Refund), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.AwaitReference(createdAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := transaction.AwaitReference(createdAt.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Process(fromUnits(t, 10000, domain.BRL), "", createdAt.Add(3*time.Second)); !errors.Is(err, domain.ErrInvalidReference) {
		t.Fatalf("missing resolved reference error=%v", err)
	}
	if transaction.Snapshot().Status != domain.PendingReference {
		t.Fatal("invalid processing changed state")
	}
	if err := transaction.Process(fromUnits(t, 10000, domain.BRL), refID, createdAt.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	copy := transaction.Snapshot()
	*copy.ResultBalance = fromUnits(t, 1, domain.BRL)
	if transaction.Snapshot().ResultBalance.MinorUnits() != 10000 {
		t.Fatal("snapshot changed original result")
	}
	restored, err := domain.RestoreWagerTransaction(copy)
	if err != nil {
		t.Fatal(err)
	}
	*copy.ResultBalance = fromUnits(t, 2, domain.BRL)
	if restored.Snapshot().ResultBalance.MinorUnits() != 1 {
		t.Fatal("restoration retained a shared pointer")
	}
}

func TestTransactionRejectsInvalidInputAndTransitions(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*domain.TransactionInput)
	}{
		{"missing provider", func(s *domain.TransactionInput) { s.ProviderID = "" }},
		{"whitespace", func(s *domain.TransactionInput) { s.RoundID = " round " }},
		{"oversized key", func(s *domain.TransactionInput) { s.IdempotencyKey = strings.Repeat("a", 256) }},
		{"missing hash", func(s *domain.TransactionInput) { s.PayloadHash = "" }},
		{"invalid hash", func(s *domain.TransactionInput) { s.PayloadHash = strings.Repeat("zz", 32) }},
		{"missing reference", func(s *domain.TransactionInput) { s.Kind = domain.Refund }},
		{"unexpected reference", func(s *domain.TransactionInput) { s.ReferenceExternalTransactionID = "original" }},
		{"unknown kind", func(s *domain.TransactionInput) { s.Kind = "OTHER" }},
		{"self reference", func(s *domain.TransactionInput) {
			s.Kind = domain.Refund
			s.ReferenceExternalTransactionID = s.ExternalTransactionID
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input := transactionInput(t, domain.Bet)
			tt.edit(&input)
			if _, err := domain.NewWagerTransaction(input, createdAt); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	transaction, _ := domain.NewWagerTransaction(transactionInput(t, domain.Bet), createdAt)
	before := transaction.Snapshot()
	for _, err := range []error{
		transaction.AwaitReference(createdAt),
		transaction.Process(fromUnits(t, 1, domain.USD), "", createdAt),
		transaction.Process(fromUnits(t, -1, domain.BRL), "", createdAt),
		transaction.Process(fromUnits(t, 1, domain.BRL), "", createdAt.Add(-time.Second)),
		transaction.Reject("UNKNOWN", fromUnits(t, 1, domain.BRL), createdAt),
	} {
		if err == nil {
			t.Fatal("invalid transition accepted")
		}
	}
	if !reflect.DeepEqual(transaction.Snapshot(), before) {
		t.Fatal("invalid transition changed state")
	}
	if err := (&domain.WagerTransaction{}).Validate(); err == nil {
		t.Fatal("uninitialized transaction accepted")
	}
}

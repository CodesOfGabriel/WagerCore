package domain_test

import (
	"encoding/json"
	"testing"
	"time"

	"junglegaming/internal/domain"
)

func TestEventKeepsSnapshotAfterTransactionChanges(t *testing.T) {
	at := time.Now().UTC()
	amount, _ := domain.NewMoney("25.00", domain.BRL)
	input := domain.TransactionInput{ID: "11111111-1111-4111-8111-111111111111", WalletID: "22222222-2222-4222-8222-222222222222", PlayerID: "33333333-3333-4333-8333-333333333333", ProviderID: "provider-a", ExternalTransactionID: "refund-1", IdempotencyKey: "refund-key", PayloadHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RoundID: "round-1", GameID: "game-1", Kind: domain.Refund, Money: amount, ReferenceExternalTransactionID: "bet-1"}
	transaction, err := domain.NewWagerTransaction(input, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.AwaitReference(at); err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewWagerTransactionPendingReference("44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555", "message-1", transaction)
	if err != nil {
		t.Fatal(err)
	}
	original := string(event.Payload())
	balance, _ := domain.NewMoney("100.00", domain.BRL)
	if err := transaction.Reject(domain.ReferenceNotFound, balance, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if string(event.Payload()) != original {
		t.Fatal("event followed mutable transaction state")
	}
	mutated := event.Payload()
	mutated[0] = 'x'
	if string(event.Payload()) != original {
		t.Fatal("caller mutated event payload")
	}
	var envelope struct {
		Version                               int
		EventType, CorrelationID, CausationID string
		Data                                  domain.TransactionEventData
	}
	if err := json.Unmarshal(event.Payload(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != 1 || envelope.EventType != "WagerTransactionPendingReference" || envelope.CausationID != "message-1" || envelope.Data.Status != domain.PendingReference || envelope.Data.Balance != nil {
		t.Fatalf("unexpected event snapshot: %+v", envelope)
	}
	if _, err := domain.NewWagerTransactionProcessed(input.ID, input.WalletID, "", transaction); err == nil {
		t.Fatal("rejected transaction produced a processed event")
	}
}

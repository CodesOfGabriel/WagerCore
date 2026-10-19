package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

type Event interface {
	ID() string
	Type() string
	AggregateID() string
	WalletID() string
	OccurredAt() time.Time
	Payload() []byte
}

type recordedEvent struct {
	id, kind, aggregateID, walletID string
	occurredAt                      time.Time
	payload                         []byte
}

func (e recordedEvent) ID() string            { return e.id }
func (e recordedEvent) Type() string          { return e.kind }
func (e recordedEvent) AggregateID() string   { return e.aggregateID }
func (e recordedEvent) WalletID() string      { return e.walletID }
func (e recordedEvent) OccurredAt() time.Time { return e.occurredAt }
func (e recordedEvent) Payload() []byte       { return bytes.Clone(e.payload) }

type TransactionEventData struct {
	TransactionID         string            `json:"transactionId"`
	WalletID              string            `json:"walletId"`
	PlayerID              string            `json:"playerId"`
	ProviderID            string            `json:"providerId,omitempty"`
	ExternalTransactionID string            `json:"externalTransactionId,omitempty"`
	RoundID               string            `json:"roundId,omitempty"`
	GameID                string            `json:"gameId,omitempty"`
	Kind                  TransactionKind   `json:"kind"`
	Status                TransactionStatus `json:"status"`
	Money                 Money             `json:"money"`
	Balance               *Money            `json:"balance,omitempty"`
	FailureCode           FailureCode       `json:"failureCode,omitempty"`
	ReferenceExternalID   string            `json:"referenceExternalTransactionId,omitempty"`
}

type BalanceChangedData struct {
	WalletID      string    `json:"walletId"`
	TransactionID string    `json:"transactionId"`
	Direction     Direction `json:"direction"`
	Money         Money     `json:"money"`
	BalanceBefore Money     `json:"balanceBefore"`
	BalanceAfter  Money     `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
}

type WagerTransactionProcessed struct{ recordedEvent }
type WagerTransactionRejected struct{ recordedEvent }
type WagerTransactionPendingReference struct{ recordedEvent }
type WalletBalanceChanged struct{ recordedEvent }

func NewWagerTransactionProcessed(id, correlation, causation string, transaction *WagerTransaction) (WagerTransactionProcessed, error) {
	event, err := transactionEvent(id, correlation, causation, transaction, Processed, "WagerTransactionProcessed")
	return WagerTransactionProcessed{event}, err
}

func NewWagerTransactionRejected(id, correlation, causation string, transaction *WagerTransaction) (WagerTransactionRejected, error) {
	event, err := transactionEvent(id, correlation, causation, transaction, Rejected, "WagerTransactionRejected")
	return WagerTransactionRejected{event}, err
}

func NewWagerTransactionPendingReference(id, correlation, causation string, transaction *WagerTransaction) (WagerTransactionPendingReference, error) {
	event, err := transactionEvent(id, correlation, causation, transaction, PendingReference, "WagerTransactionPendingReference")
	return WagerTransactionPendingReference{event}, err
}

func transactionEvent(id, correlation, causation string, transaction *WagerTransaction, status TransactionStatus, kind string) (recordedEvent, error) {
	if err := transaction.Validate(); err != nil {
		return recordedEvent{}, err
	}
	state := transaction.Snapshot()
	if state.Status != status {
		return recordedEvent{}, ErrInvalidTransition
	}
	input := state.Input
	data := TransactionEventData{
		TransactionID: input.ID, WalletID: input.WalletID, PlayerID: input.PlayerID,
		ProviderID: input.ProviderID, ExternalTransactionID: input.ExternalTransactionID,
		RoundID: input.RoundID, GameID: input.GameID, Kind: input.Kind, Status: status,
		Money: input.Money, Balance: state.ResultBalance, FailureCode: state.FailureCode,
		ReferenceExternalID: input.ReferenceExternalTransactionID,
	}
	return newEvent(id, kind, input.ID, input.WalletID, correlation, causation, state.UpdatedAt, data)
}

func NewWalletBalanceChanged(id, correlation, causation string, entry WalletLedgerEntry) (WalletBalanceChanged, error) {
	if err := entry.Validate(); err != nil {
		return WalletBalanceChanged{}, err
	}
	state := entry.Snapshot()
	event, err := newEvent(id, "WalletBalanceChanged", state.WalletID, state.WalletID, correlation, causation, state.CreatedAt, BalanceChangedData{
		WalletID: state.WalletID, TransactionID: state.TransactionID, Direction: state.Direction,
		Money: state.Money, BalanceBefore: state.BalanceBefore, BalanceAfter: state.BalanceAfter, WalletVersion: state.WalletVersion,
	})
	return WalletBalanceChanged{event}, err
}

func newEvent(id, kind, aggregate, wallet, correlation, causation string, at time.Time, data any) (recordedEvent, error) {
	for _, value := range []string{id, aggregate, wallet, correlation} {
		if err := ValidateID(value); err != nil {
			return recordedEvent{}, err
		}
	}
	if causation != "" && !validText(causation) {
		return recordedEvent{}, errors.New("invalid event causation")
	}
	if at.IsZero() {
		return recordedEvent{}, ErrInvalidTimestamp
	}
	payload, err := json.Marshal(struct {
		EventID       string    `json:"eventId"`
		EventType     string    `json:"eventType"`
		AggregateID   string    `json:"aggregateId"`
		CorrelationID string    `json:"correlationId"`
		CausationID   string    `json:"causationId,omitempty"`
		OccurredAt    time.Time `json:"occurredAt"`
		Version       int       `json:"version"`
		Data          any       `json:"data"`
	}{id, kind, aggregate, correlation, causation, at.UTC(), 1, data})
	return recordedEvent{id, kind, aggregate, wallet, at.UTC(), payload}, err
}

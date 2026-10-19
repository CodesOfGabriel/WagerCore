package domain

import (
	"encoding/hex"
	"strings"
	"time"
)

type TransactionKind string

const (
	Opening  TransactionKind = "OPENING"
	Bet      TransactionKind = "BET"
	Win      TransactionKind = "WIN"
	Loss     TransactionKind = "LOSS"
	Refund   TransactionKind = "REFUND"
	Rollback TransactionKind = "ROLLBACK"
)

type TransactionStatus string

const (
	Pending          TransactionStatus = "PENDING"
	PendingReference TransactionStatus = "PENDING_REFERENCE"
	Processed        TransactionStatus = "PROCESSED"
	Rejected         TransactionStatus = "REJECTED"
	Failed           TransactionStatus = "FAILED"
)

type FailureCode string

const (
	InsufficientFunds         FailureCode = "INSUFFICIENT_FUNDS"
	ReversalInsufficientFunds FailureCode = "REVERSAL_INSUFFICIENT_FUNDS"
	ReferenceNotFound         FailureCode = "REFERENCE_NOT_FOUND"
	ReferenceNotProcessed     FailureCode = "REFERENCE_NOT_PROCESSED"
	ReferenceMismatch         FailureCode = "REFERENCE_MISMATCH"
	AlreadyReversed           FailureCode = "ALREADY_REVERSED"
	AmountMismatch            FailureCode = "AMOUNT_MISMATCH"
	InfrastructureFailure     FailureCode = "INFRASTRUCTURE_FAILURE"
)

func (code FailureCode) business() bool {
	switch code {
	case InsufficientFunds, ReversalInsufficientFunds, ReferenceNotFound,
		ReferenceNotProcessed, ReferenceMismatch, AlreadyReversed, AmountMismatch:
		return true
	default:
		return false
	}
}

type TransactionInput struct {
	ID                             string
	WalletID                       string
	PlayerID                       string
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	RoundID                        string
	GameID                         string
	Kind                           TransactionKind
	Money                          Money
	ReferenceExternalTransactionID string
}

type TransactionSnapshot struct {
	Input               TransactionInput
	Status              TransactionStatus
	ResolvedReferenceID string
	FailureCode         FailureCode
	ResultBalance       *Money
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type WagerTransaction struct {
	state TransactionSnapshot
}

func NewWagerTransaction(input TransactionInput, at time.Time) (*WagerTransaction, error) {
	if input.Kind == Opening {
		return nil, ErrInvalidTransaction
	}
	return RestoreWagerTransaction(TransactionSnapshot{
		Input: input, Status: Pending, CreatedAt: at, UpdatedAt: at,
	})
}

func NewOpeningTransaction(id string, wallet *Wallet) (*WagerTransaction, error) {
	if err := wallet.Validate(); err != nil {
		return nil, err
	}
	state := wallet.Snapshot()
	if state.Version != 1 || !state.UpdatedAt.Equal(state.CreatedAt) {
		return nil, ErrInvalidWallet
	}
	return RestoreWagerTransaction(TransactionSnapshot{
		Input: TransactionInput{
			ID: id, WalletID: state.ID, PlayerID: state.PlayerID,
			Kind: Opening, Money: state.Balance,
		},
		Status: Pending, CreatedAt: state.CreatedAt, UpdatedAt: state.CreatedAt,
	})
}

func RestoreWagerTransaction(state TransactionSnapshot) (*WagerTransaction, error) {
	transaction := &WagerTransaction{state: copyTransactionState(state)}
	if err := transaction.Validate(); err != nil {
		return nil, err
	}
	transaction.state.CreatedAt = state.CreatedAt.UTC()
	transaction.state.UpdatedAt = state.UpdatedAt.UTC()
	return transaction, nil
}

func (t *WagerTransaction) Snapshot() TransactionSnapshot {
	return copyTransactionState(t.state)
}

func copyTransactionState(state TransactionSnapshot) TransactionSnapshot {
	if state.ResultBalance != nil {
		balance := *state.ResultBalance
		state.ResultBalance = &balance
	}
	return state
}

func (t *WagerTransaction) Validate() error {
	if t == nil {
		return ErrInvalidTransaction
	}
	if err := validateTransactionInput(t.state.Input); err != nil {
		return err
	}
	if !validTimes(t.state.CreatedAt, t.state.UpdatedAt) {
		return ErrInvalidTimestamp
	}
	if t.state.ResolvedReferenceID != "" {
		if err := validateID(t.state.ResolvedReferenceID); err != nil {
			return err
		}
		if t.state.ResolvedReferenceID == t.state.Input.ID || t.state.Input.ReferenceExternalTransactionID == "" {
			return ErrInvalidReference
		}
	}
	if t.state.ResultBalance != nil {
		if err := validateNonNegative(*t.state.ResultBalance); err != nil {
			return err
		}
		if t.state.ResultBalance.Currency() != t.state.Input.Money.Currency() {
			return ErrCurrencyMismatch
		}
	}
	switch t.state.Status {
	case Pending, PendingReference:
		if t.state.FailureCode != "" || t.state.ResultBalance != nil || t.state.ResolvedReferenceID != "" {
			return ErrInvalidTransaction
		}
		if t.state.Status == PendingReference && t.state.Input.ReferenceExternalTransactionID == "" {
			return ErrInvalidReference
		}
	case Processed:
		if t.state.FailureCode != "" || t.state.ResultBalance == nil {
			return ErrInvalidTransaction
		}
		if t.state.Input.Kind == Opening && t.state.ResultBalance.MinorUnits() != t.state.Input.Money.MinorUnits() {
			return ErrInvalidTransaction
		}
		if (t.state.Input.ReferenceExternalTransactionID != "") != (t.state.ResolvedReferenceID != "") {
			return ErrInvalidReference
		}
	case Rejected:
		if !t.state.FailureCode.business() || t.state.ResultBalance == nil {
			return ErrInvalidTransaction
		}
	case Failed:
		if t.state.FailureCode != InfrastructureFailure || t.state.ResultBalance != nil {
			return ErrInvalidTransaction
		}
	default:
		return ErrInvalidTransaction
	}
	if t.state.Input.Kind == Opening && t.state.Status != Pending && t.state.Status != Processed {
		return ErrInvalidTransition
	}
	return nil
}

func validateTransactionInput(input TransactionInput) error {
	for _, id := range []string{input.ID, input.WalletID, input.PlayerID} {
		if err := validateID(id); err != nil {
			return err
		}
	}
	if err := validateNonNegative(input.Money); err != nil {
		return err
	}
	switch input.Kind {
	case Opening, Bet, Win, Refund, Rollback:
		if input.Money.MinorUnits() == 0 {
			return ErrInvalidAmount
		}
	case Loss:
		if input.Money.MinorUnits() != 0 {
			return ErrInvalidAmount
		}
	default:
		return ErrInvalidTransaction
	}
	metadata := []string{input.ProviderID, input.ExternalTransactionID, input.IdempotencyKey, input.PayloadHash, input.RoundID, input.GameID}
	if input.Kind == Opening {
		for _, value := range append(metadata, input.ReferenceExternalTransactionID) {
			if value != "" {
				return ErrInvalidTransaction
			}
		}
		return nil
	}
	for _, value := range metadata {
		if !validText(value) {
			return ErrInvalidTransaction
		}
	}
	hash, err := hex.DecodeString(input.PayloadHash)
	if err != nil || len(hash) != 32 || input.PayloadHash != strings.ToLower(input.PayloadHash) {
		return ErrInvalidTransaction
	}
	if input.ReferenceExternalTransactionID != "" && (!validText(input.ReferenceExternalTransactionID) || input.ReferenceExternalTransactionID == input.ExternalTransactionID) {
		return ErrInvalidReference
	}
	switch input.Kind {
	case Bet, Loss:
		if input.ReferenceExternalTransactionID != "" {
			return ErrInvalidReference
		}
	case Refund, Rollback:
		if input.ReferenceExternalTransactionID == "" {
			return ErrInvalidReference
		}
	}
	return nil
}

func (t *WagerTransaction) AwaitReference(at time.Time) error {
	return t.transition(PendingReference, "", nil, "", at)
}

func (t *WagerTransaction) Process(balance Money, resolvedReferenceID string, at time.Time) error {
	return t.transition(Processed, "", &balance, resolvedReferenceID, at)
}

func (t *WagerTransaction) Reject(code FailureCode, balance Money, at time.Time) error {
	return t.transition(Rejected, code, &balance, "", at)
}

func (t *WagerTransaction) Fail(at time.Time) error {
	return t.transition(Failed, InfrastructureFailure, nil, "", at)
}

func (t *WagerTransaction) transition(status TransactionStatus, code FailureCode, balance *Money, referenceID string, at time.Time) error {
	if err := t.Validate(); err != nil {
		return err
	}
	switch t.state.Status {
	case Processed, Rejected, Failed:
		return ErrTerminalTransaction
	}
	if at.IsZero() || at.Before(t.state.UpdatedAt) {
		return ErrInvalidTimestamp
	}
	next := t.Snapshot()
	next.Status, next.FailureCode, next.ResultBalance = status, code, balance
	next.ResolvedReferenceID, next.UpdatedAt = referenceID, at.UTC()
	candidate, err := RestoreWagerTransaction(next)
	if err != nil {
		return err
	}
	t.state = candidate.state
	return nil
}

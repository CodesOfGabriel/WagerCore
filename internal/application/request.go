package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"junglegaming/internal/domain"
)

type WagerRequest struct {
	ProviderID                     string                 `json:"providerId"`
	ExternalTransactionID          string                 `json:"externalTransactionId"`
	PlayerID                       string                 `json:"playerId"`
	WalletID                       string                 `json:"walletId"`
	RoundID                        string                 `json:"roundId"`
	GameID                         string                 `json:"gameId"`
	Kind                           domain.TransactionKind `json:"kind"`
	Money                          domain.Money           `json:"money"`
	ReferenceExternalTransactionID string                 `json:"referenceExternalTransactionId,omitempty"`
}

func (r WagerRequest) Canonical() ([]byte, error) {
	amount, err := r.Money.Amount()
	if err != nil {
		return nil, err
	}
	fields := map[string]any{
		"providerId": r.ProviderID, "externalTransactionId": r.ExternalTransactionID,
		"playerId": r.PlayerID, "walletId": r.WalletID, "roundId": r.RoundID, "gameId": r.GameID,
		"kind": r.Kind, "money": map[string]string{"amount": amount, "currency": string(r.Money.Currency())},
	}
	if r.ReferenceExternalTransactionID != "" {
		fields["referenceExternalTransactionId"] = r.ReferenceExternalTransactionID
	}
	return json.Marshal(fields)
}

func (r WagerRequest) Input(id, key string) (domain.TransactionInput, error) {
	payload, err := r.Canonical()
	if err != nil {
		return domain.TransactionInput{}, err
	}
	hash := sha256.Sum256(payload)
	return domain.TransactionInput{
		ID: id, WalletID: r.WalletID, PlayerID: r.PlayerID, ProviderID: r.ProviderID,
		ExternalTransactionID: r.ExternalTransactionID, IdempotencyKey: key, PayloadHash: hex.EncodeToString(hash[:]),
		RoundID: r.RoundID, GameID: r.GameID, Kind: r.Kind, Money: r.Money, ReferenceExternalTransactionID: r.ReferenceExternalTransactionID,
	}, nil
}

type WagerResult struct {
	TransactionID    string                   `json:"transactionId"`
	Status           domain.TransactionStatus `json:"status"`
	Balance          *domain.Money            `json:"balance,omitempty"`
	FailureCode      domain.FailureCode       `json:"failureCode,omitempty"`
	IdempotentReplay bool                     `json:"idempotentReplay"`
}

func resultOf(transaction *domain.WagerTransaction, replay bool) WagerResult {
	s := transaction.Snapshot()
	return WagerResult{s.Input.ID, s.Status, s.ResultBalance, s.FailureCode, replay}
}

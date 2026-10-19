package sqsbroker

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"time"

	"junglegaming/internal/application"
)

type RequestedData struct {
	application.WagerRequest
	IdempotencyKey string `json:"idempotencyKey"`
}
type Requested struct {
	MessageID  string        `json:"messageId"`
	Type       string        `json:"type"`
	OccurredAt time.Time     `json:"occurredAt"`
	Data       RequestedData `json:"data"`
}

func Decode(body string) (Requested, []byte, error) {
	var message Requested
	if len(body) > 64<<10 {
		return message, nil, errors.New("message body exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return message, nil, errors.New("invalid message JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return message, nil, errors.New("message must contain one JSON object")
	}
	if message.MessageID == "" || len(message.MessageID) > 255 || message.Type != "WagerTransactionRequested" || message.OccurredAt.IsZero() {
		return message, nil, errors.New("invalid message envelope")
	}
	canonical, err := message.Data.WagerRequest.Canonical()
	if err != nil {
		return message, nil, err
	}
	payload, err := json.Marshal(map[string]any{"messageId": message.MessageID, "type": message.Type, "occurredAt": message.OccurredAt.UTC().Format(time.RFC3339Nano), "idempotencyKey": message.Data.IdempotencyKey, "data": json.RawMessage(canonical)})
	if err != nil {
		return message, nil, err
	}
	hash := sha256.Sum256(payload)
	return message, hash[:], nil
}

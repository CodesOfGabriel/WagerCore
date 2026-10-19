package sqsbroker

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
		return message, nil, fmt.Errorf("%w: message body exceeds limit", application.ErrInvalidInput)
	}
	decoder := json.NewDecoder(bytes.NewBufferString(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return message, nil, fmt.Errorf("%w: invalid message JSON", application.ErrInvalidInput)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return message, nil, fmt.Errorf("%w: message must contain one JSON object", application.ErrInvalidInput)
	}
	if message.MessageID == "" || len(message.MessageID) > 255 || message.Type != "WagerTransactionRequested" || message.OccurredAt.IsZero() {
		return message, nil, fmt.Errorf("%w: invalid message envelope", application.ErrInvalidInput)
	}
	canonical, err := message.Data.WagerRequest.Canonical()
	if err != nil {
		return message, nil, fmt.Errorf("%w: %w", application.ErrInvalidInput, err)
	}
	payload, err := json.Marshal(map[string]any{"messageId": message.MessageID, "type": message.Type, "occurredAt": message.OccurredAt.UTC().Format(time.RFC3339Nano), "idempotencyKey": message.Data.IdempotencyKey, "data": json.RawMessage(canonical)})
	if err != nil {
		return message, nil, fmt.Errorf("%w: %w", application.ErrInvalidInput, err)
	}
	hash := sha256.Sum256(payload)
	return message, hash[:], nil
}
